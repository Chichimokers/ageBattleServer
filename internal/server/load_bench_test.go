package server

import (
	"flag"
	"fmt"
	"testing"
	"time"

	"github.com/luskaner/ageBattleServer/internal/loadgen"
)

var (
	loadMatches   = flag.Int("load.matches", 4, "concurrent matches (sessions) in the load test")
	loadPlayers   = flag.Int("load.players", 8, "players per match, including the host")
	loadObservers = flag.Int("load.observers", 8, "observers per match")
	loadDuration  = flag.Duration("load.duration", 20*time.Second, "load test duration")
	loadPageEvery = flag.Duration("load.pageinterval", 125*time.Millisecond, "host game-data page interval")
)

// TestLoadRealistic drives a real server through capture-derived traffic at a
// configurable scale, then prints a report. It is not a pass/fail unit test; it
// is a regression harness for throughput, latency, CPU and allocation. Run:
//
//	go test -run TestLoadRealistic -v ./internal/server/ \
//	    -load.matches=4 -load.players=8 -load.observers=24 -load.duration=30s
func TestLoadRealistic(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping load test in -short mode")
	}
	silenceLogger()

	cfg := &Config{
		EphemeralListenPorts:    EphemeralWS,
		SimulationPeriod:        125,
		ObserverMax:             512,
		ObserverSocketSkipMS:    0,
		LogLevel:                "none",
	}
	srv := startServer(t, cfg)
	port := srv.wsListener.GetPort()
	if port == 0 {
		t.Fatal("webSocketPort 0 after Start: the listener is not listening on a port")
	}

	profile := loadgen.DefaultProfile()
	profile.Matches = *loadMatches
	profile.Players = *loadPlayers
	profile.Observers = *loadObservers
	profile.Duration = *loadDuration
	profile.PageInterval = *loadPageEvery

	runner, err := loadgen.New(fmt.Sprintf("ws://127.0.0.1:%d/", port), profile)
	if err != nil {
		t.Fatalf("loadgen.New: %v", err)
	}
	start := time.Now()
	runner.Debug = func(format string, args ...any) {
		t.Logf("%6.2fs "+format, append([]any{time.Since(start).Seconds()}, args...)...)
	}

	rep := runner.Run(t.Context())

	counters := srv.MetricsCollector().GetAllCounters()
	rep.ServerBytesReceived = counters["BytesReceived"]
	rep.ServerBytesSent = counters["BytesSent"]

	t.Log("\n" + loadgen.FormatReport(rep))
	// The report was advisory and the suite stayed green with every observer
	// failing to subscribe. It is now an assertion: the load model only counts
	// as exercised if it produced no client-side errors and every connection it
	// asked for reached the server.
	if rep.Stats.Errors != 0 {
		t.Errorf("%d client-side errors during the run; the load model is not clean", rep.Stats.Errors)
	}
	wantConns := float64(profile.Matches * (profile.Players + profile.Observers))
	if got := counters["WsConnections"]; got < wantConns {
		t.Errorf("WsConnections = %.0f, want at least %.0f: %d match(es) x (%d players + %d observers)",
			got, wantConns, profile.Matches, profile.Players, profile.Observers)
	}
	if rep.Stats.RecvMsgs == 0 || rep.ServerBytesReceived == 0 {
		t.Errorf("no traffic observed: client recv=%d, server bytes received=%.0f", rep.Stats.RecvMsgs, rep.ServerBytesReceived)
	}
}
