package server

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime/metrics"
	"sync"
	"testing"
	"time"

	internalmetrics "github.com/luskaner/ageBattleServer/internal/metrics"
	"github.com/luskaner/ageBattleServer/internal/protocol"
	"github.com/luskaner/ageBattleServer/internal/session"
)

// resourceStats snapshots process-wide CPU and heap metrics via runtime/metrics.
// The in-process server under test is measured together with the client harness,
// which is the same number a production process would report for this traffic.
type resourceStats struct {
	cpuTotal    float64
	cpuIdle     float64
	totalBytes  float64
	heapBytes   float64
	allocsBytes float64
	allocsObjs  float64
}

func readResourceStats() resourceStats {
	var s resourceStats
	descs := metrics.All()
	samples := make([]metrics.Sample, 0, len(descs))
	for _, d := range descs {
		samples = append(samples, metrics.Sample{Name: d.Name})
	}
	metrics.Read(samples)
	for _, smp := range samples {
		var v float64
		switch smp.Value.Kind() {
		case metrics.KindUint64:
			v = float64(smp.Value.Uint64())
		case metrics.KindFloat64:
			v = smp.Value.Float64()
		default:
			continue
		}
		switch smp.Name {
		case "/cpu/classes/total:cpu-seconds":
			s.cpuTotal = v
		case "/cpu/classes/idle:cpu-seconds":
			s.cpuIdle = v
		case "/memory/classes/total:bytes":
			s.totalBytes = v
		case "/memory/classes/heap/objects:bytes":
			s.heapBytes = v
		case "/gc/heap/allocs:bytes":
			s.allocsBytes = v
		case "/gc/heap/allocs:objects":
			s.allocsObjs = v
		}
	}
	return s
}

func reportResourceMetrics(b *testing.B, before, after resourceStats) {
	wall := b.Elapsed().Seconds()
	used := after.cpuTotal - after.cpuIdle - (before.cpuTotal - before.cpuIdle)
	if wall > 0 && used > 0 {
		// cpu-% is cumulative across all cores (may exceed 100). The runtime
		// cpu-class counters are sampled estimates, coarse on short runs.
		b.ReportMetric(used/wall*100, "cpu-%")
	}
	b.ReportMetric(after.heapBytes, "liveheap-B")
	b.ReportMetric(after.totalBytes-before.totalBytes, "mem-delta-B")
	b.ReportMetric((after.allocsBytes-before.allocsBytes)/float64(b.N), "allocs-B/op")
	b.ReportMetric((after.allocsObjs-before.allocsObjs)/float64(b.N), "allocs-obj/op")
}

// There is no bsPort round-trip benchmark, and this is why rather than an
// omission.
//
// BenchmarkBsPortPingRoundTrip used to live here: it sent 0x11 Ping and waited
// for the echo. The dispatcher has no case for 0x11 (handlers.go's default arm
// logs it as unhandled), so the benchmark failed on its first iteration and had
// been a red signal in the tree. The only type the server echoes without a
// session is 0x14 LobbyPing, and that echo is budgeted at lobbyPingEchoBudget
// (100) per connection: a benchmark loop of b.N would stop being answered after
// the hundredth iteration and fail the same way. Driving it from inside a session
// would measure the session's dispatch and the ack path, not the transport round
// trip the benchmark claimed to measure, and the transport already has
// BenchmarkClientSendBatch below for the write side.
//
// A benchmark that cannot run is worse than none, so it was removed rather than
// left failing. What would bring it back is a message type the server answers
// unconditionally, which is a protocol fact and not a test one.

// benchHost is a no-op ClientHost so the write-path benchmarks can exercise the
// Client transport without a full Server.
type benchHost struct{}

func (benchHost) AddClient(_ ClientConn)                            {}
func (benchHost) RemoveClient(_ ClientConn)                         {}
func (benchHost) NotifyWork(_ ClientConn)                           {}
func (benchHost) dispatchMessage(_ messageClient, _ byte, _ []byte) {}

// BenchmarkClientSendBatch measures how fast the TCP writeLoop drains a burst
// of SendMessage calls over a pipe. Before P5 each queued message was flushed
// to the socket individually (one write+flush per message); after, everything
// queued at drain time is coalesced into a single flush per batch. The reader
// drains the frames as the writer produces them, so ns/op is the wall time for
// a whole batch of client->socket round trips.
func BenchmarkClientSendBatch(b *testing.B) {
	for _, batch := range []int{1, 16, 64} {
		b.Run(fmt.Sprintf("batch=%d", batch), func(b *testing.B) {
			serverEnd, clientEnd := net.Pipe()
			client := NewClient(serverEnd, benchHost{})
			go client.writeLoop()
			defer func(client *Client) {
				_ = client.Close()
			}(client)
			rd := bufio.NewReader(clientEnd)
			payload := make([]byte, 64)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for range batch {
					if err := client.SendMessage(protocol.MsgLobbyPing, payload); err != nil {
						b.Fatal(err)
					}
				}
				// Every frame is [u16 BE len][type][body]; drain them all.
				for range batch {
					var lenBuf [2]byte
					if _, err := io.ReadFull(rd, lenBuf[:]); err != nil {
						b.Fatal(err)
					}
					length := int(binary.BigEndian.Uint16(lenBuf[:]))
					if _, err := io.CopyN(io.Discard, rd, int64(length)); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}

// BenchmarkServerClientCountGauge measures the client-count read behind the
// Clients gauge while connections churn concurrently. Before P8 collectMetrics
// took the server lock (a writer that also stalls the relay's isLiveClient
// readers and the client add/remove path); after it is a wait-free atomic load.
func BenchmarkServerClientCountGauge(b *testing.B) {
	for _, churn := range []bool{false, true} {
		b.Run(fmt.Sprintf("churn=%v", churn), func(b *testing.B) {
			s := &Server{
				mu:               sync.RWMutex{},
				clients:          make([]ClientConn, 0, 64),
				clientSet:        make(map[any]struct{}),
				metricsCollector: internalmetrics.NewCollector(),
				sessionManager:   session.NewManager(0, 0),
			}
			stop := make(chan struct{})
			if churn {
				go func() {
					for {
						select {
						case <-stop:
							return
						default:
						}
						c := &mockMsgClient{}
						s.AddClient(c)
						s.RemoveClient(c)
					}
				}()
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				s.collectMetrics()
			}
			b.StopTimer()
			close(stop)
		})
	}
}

// BenchmarkServerIsLiveClient measures the live-client delivery gate in the
// relay path (handleLobbyRelay). Before P3 it scanned the client registry
// (O(clients)); after it is an O(1) membership check in the client set.
func BenchmarkServerIsLiveClient(b *testing.B) {
	for _, n := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("clients=%d", n), func(b *testing.B) {
			clients := make([]ClientConn, 0, n)
			clientSet := make(map[any]struct{}, n)
			var last session.PeerConnection
			for range n {
				c := &mockMsgClient{}
				clients = append(clients, c)
				clientSet[c] = struct{}{}
				last = c
			}
			s := &Server{clients: clients, clientSet: clientSet}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if !s.isLiveClient(last) {
					b.Fatal("not live")
				}
			}
		})
	}
}

// BenchmarkBsPortGameDataThroughput floods the server with replay GameData pages
// (the host uploading a live game, the busiest hot path of the protocol) and
// drains the session-wide EndOfGameData acks. Per-op work is a fixed batch of
// pages; SetBytes turns it into MB/s of client->server wire traffic. The server
// retains the pages in the session replay, so the liveheap figure includes the
// page store under load.
func BenchmarkBsPortGameDataThroughput(b *testing.B) {
	_, port := startBlackBoxServer(b)
	c := newTrafficClient(b, port)

	connect := lobbyConnectBody(1)
	c.send(protocol.MsgLobbyConnect, connect)
	c.expectFrame(2*time.Second, protocol.MsgLobbyConnect, connect)

	payload := make([]byte, 256)
	for i := range payload {
		payload[i] = byte(i)
	}
	frame := wireFrame(protocol.MsgGameData, gameDataPage(payload))
	const batch = 128

	before := readResourceStats()
	b.SetBytes(int64(len(frame) * batch))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for range batch {
			if _, err := c.conn.Write(frame); err != nil {
				b.Fatalf("write: %v", err)
			}
			c.sent += int64(len(frame))
		}
		// Drain until the acks ([0xFE][count]...) cover the whole batch. A tick
		// drains a bounded number of queued sub-messages (session.PopTick), so
		// the batch is acked over several ticks; heartbeat acks (count 0) are
		// skipped.
		deadline := time.Now().Add(2 * time.Second)
		acked := 0
		for acked < batch {
			if time.Now().After(deadline) {
				b.Fatalf("timeout: got %d/%d acks", acked, batch)
			}
			_ = c.conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
			var lenBuf [2]byte
			if _, err := io.ReadFull(c.rd, lenBuf[:]); err != nil {
				var netErr net.Error
				if errors.As(err, &netErr) && netErr.Timeout() {
					continue
				}
				b.Fatalf("read ack length: %v", err)
			}
			length := int(binary.BigEndian.Uint16(lenBuf[:]))
			ack := make([]byte, length)
			if _, err := io.ReadFull(c.rd, ack); err != nil {
				b.Fatalf("read ack payload: %v", err)
			}
			c.recv += int64(2 + length)
			if len(ack) >= 3 && ack[0] == protocol.MsgEndOfGameData && ack[1] == 0xfe {
				acked += int(ack[2])
			}
		}
	}
	b.StopTimer()
	reportResourceMetrics(b, before, readResourceStats())
}

