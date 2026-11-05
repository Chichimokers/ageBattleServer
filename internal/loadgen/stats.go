package loadgen

import (
	"sync/atomic"
	"time"
)

// Stats aggregates client-side traffic counters and ping latency across all
// roles. It is safe for concurrent use.
type Stats struct {
	sentMsgs  atomic.Uint64
	sentBytes atomic.Uint64
	recvMsgs  atomic.Uint64
	recvBytes atomic.Uint64

	pingCount atomic.Uint64
	pingSumNS atomic.Uint64
	pingMinNS atomic.Uint64
	pingMaxNS atomic.Uint64

	errors atomic.Uint64
}

func (s *Stats) recordSent(n int) {
	s.sentMsgs.Add(1)
	s.sentBytes.Add(uint64(n))
}

func (s *Stats) recordRecv(n int) {
	s.recvMsgs.Add(1)
	s.recvBytes.Add(uint64(n))
}

// recordRecvBytes counts received bytes without counting a message. A WebSocket
// frame is one message made of a header and a payload; the header bytes are
// traffic but not a message, and counting each read as one inflated RecvMsgs.
func (s *Stats) recordRecvBytes(n int) {
	s.recvBytes.Add(uint64(n))
}

// recordRecvMsg counts one complete received message.
func (s *Stats) recordRecvMsg() {
	s.recvMsgs.Add(1)
}

func (s *Stats) recordPing(d time.Duration) {
	ns := uint64(d)
	s.pingCount.Add(1)
	s.pingSumNS.Add(ns)
	for {
		cur := s.pingMinNS.Load()
		if cur != 0 && ns >= cur {
			break
		}
		if s.pingMinNS.CompareAndSwap(cur, ns) {
			break
		}
	}
	for {
		cur := s.pingMaxNS.Load()
		if ns <= cur {
			break
		}
		if s.pingMaxNS.CompareAndSwap(cur, ns) {
			break
		}
	}
}

func (s *Stats) recordError() {
	s.errors.Add(1)
}

// Snapshot is a point-in-time copy of the counters.
type Snapshot struct {
	SentMsgs, SentBytes, RecvMsgs, RecvBytes uint64
	PingCount, PingSumNS, PingMinNS, PingMax uint64
	Errors                                   uint64
}

func (s *Stats) snapshot() Snapshot {
	return Snapshot{
		SentMsgs:  s.sentMsgs.Load(),
		SentBytes: s.sentBytes.Load(),
		RecvMsgs:  s.recvMsgs.Load(),
		RecvBytes: s.recvBytes.Load(),
		PingCount: s.pingCount.Load(),
		PingSumNS: s.pingSumNS.Load(),
		PingMinNS: s.pingMinNS.Load(),
		PingMax:   s.pingMaxNS.Load(),
		Errors:    s.errors.Load(),
	}
}

// Report holds the aggregate result of one run plus the resource stats delta
// sampled around it.
type Report struct {
	Profile Profile
	Stats   Snapshot
	Elapsed time.Duration
	// Resource usage deltas when readResourceStats was available (in-process
	// runs); zero otherwise.
	CPUUsedSeconds float64
	HeapBytes      float64
	AllocsBytes    float64
	AllocsObjs     float64
	// Server counters (in-process runs); zero otherwise.
	ServerBytesReceived, ServerBytesSent float64
}

// AvgLatency returns the mean of the recorded ping round trips.
func (r Report) AvgLatency() time.Duration {
	if r.Stats.PingCount == 0 {
		return 0
	}
	return time.Duration(r.Stats.PingSumNS / r.Stats.PingCount)
}
