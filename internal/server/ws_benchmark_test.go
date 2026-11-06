package server

import (
	"fmt"
	"testing"
	"time"

	"github.com/lxzan/gws"

	"github.com/luskaner/ageBattleServer/internal/protocol"
)

// benchWSHandler receives WS frames on a channel so the benchmark waits on a
// blocking receive instead of polling.
type benchWSHandler struct {
	frames chan []byte
}

func (h *benchWSHandler) OnOpen(*gws.Conn)         {}
func (h *benchWSHandler) OnClose(*gws.Conn, error) {}
func (h *benchWSHandler) OnPing(*gws.Conn, []byte) {}
func (h *benchWSHandler) OnPong(*gws.Conn, []byte) {}
func (h *benchWSHandler) OnMessage(_ *gws.Conn, m *gws.Message) {
	h.frames <- append([]byte(nil), m.Bytes()...)
}

// startWSServerBench boots a real server with a WebSocket listener.
func startWSServerBench(b *testing.B) (*Server, int) {
	b.Helper()
	silenceLogger()
	cfg := &Config{
		EphemeralListenPorts: EphemeralWS,
		LogLevel:             "none",
	}
	srv := startServer(b, cfg)
	port := srv.wsListener.GetPort()
	if port == 0 {
		b.Fatal("webSocketPort 0 after Start: the listener is not listening on a port")
	}
	return srv, port
}

// dialWSBench opens a WS client connection to the listener.
func dialWSBench(b *testing.B, port int, h *benchWSHandler) *gws.Conn {
	b.Helper()
	addr := fmt.Sprintf("ws://127.0.0.1:%d/", port)
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, _, err := gws.NewClient(h, &gws.ClientOption{Addr: addr})
		if err == nil {
			go conn.ReadLoop()
			return conn
		}
		if time.Now().After(deadline) {
			b.Fatalf("WS dial: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// joinSessionBench puts the connection into a session, which is what makes the
// 0x14 echo unconditional. A connection with no session is answered only while
// its 100-echo budget lasts (handleLobbyPing), so a benchmark that pinged without
// joining would stop being answered partway through and time out.
//
// The LobbyConnect's own reply is drained here, before the timer starts, so the
// benchmark's channel holds only the echoes it is measuring.
func joinSessionBench(b *testing.B, conn *gws.Conn, h *benchWSHandler) {
	b.Helper()
	connect := protocol.BuildLobbyConnect(0, 1, 0x0123456789abcdef, 0, 0, 5, 0, 0)
	if err := conn.WriteMessage(gws.OpcodeBinary, wsMessage(protocol.MsgLobbyConnect, connect)); err != nil {
		b.Fatalf("lobby connect: %v", err)
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case got := <-h.frames:
			if typ, _ := frameType(got); typ == protocol.MsgLobbyConnect {
				return
			}
		case <-deadline:
			b.Fatal("timeout waiting for the LobbyConnect reply")
		}
	}
}

// BenchmarkWSPingRoundTrip measures one full WS client->server->client round trip
// on the message the dispatcher actually echoes.
//
// It used to send 0x11 and wait for an echo the dispatcher does not implement, so
// it could only ever time out: a red benchmark in the tree is a false signal. 0x14
// is the echoed message (handleLobbyPing), and the connection joins a session
// first so the echo is unconditional rather than budgeted.
func BenchmarkWSPingRoundTrip(b *testing.B) {
	_, port := startWSServerBench(b)
	h := &benchWSHandler{frames: make(chan []byte, 64)}
	conn := dialWSBench(b, port, h)
	defer func() { _ = conn.WriteClose(1000, nil) }()
	joinSessionBench(b, conn, h)

	frame := wsMessage(protocol.MsgLobbyPing, nil)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := conn.WriteMessage(gws.OpcodeBinary, frame); err != nil {
			b.Fatalf("write: %v", err)
		}
		select {
		case got := <-h.frames:
			if typ, _ := frameType(got); typ != protocol.MsgLobbyPing {
				b.Fatalf("unexpected reply type 0x%02x", typ)
			}
		case <-time.After(2 * time.Second):
			b.Fatal("timeout waiting for the WS ping reply")
		}
	}
	b.StopTimer()
}

// BenchmarkWSPingBurst measures the cost of draining a burst of N pings: it sends
// the whole batch first, then waits for all the replies. That is the shape that
// shows how the main loop batches them, which one-at-a-time cannot.
func BenchmarkWSPingBurst(b *testing.B) {
	_, port := startWSServerBench(b)
	h := &benchWSHandler{frames: make(chan []byte, 1024)}
	conn := dialWSBench(b, port, h)
	defer func() { _ = conn.WriteClose(1000, nil) }()
	joinSessionBench(b, conn, h)

	frame := wsMessage(protocol.MsgLobbyPing, nil)
	const batch = 64

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for range batch {
			if err := conn.WriteMessage(gws.OpcodeBinary, frame); err != nil {
				b.Fatalf("write: %v", err)
			}
		}
		for range batch {
			select {
			case got := <-h.frames:
				if typ, _ := frameType(got); typ != protocol.MsgLobbyPing {
					b.Fatalf("unexpected reply type 0x%02x", typ)
				}
			case <-time.After(2 * time.Second):
				b.Fatal("timeout waiting for the WS ping reply")
			}
		}
	}
	b.StopTimer()
}
