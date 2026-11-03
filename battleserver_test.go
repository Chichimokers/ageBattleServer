package battleserver

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/luskaner/ageBattleServer/internal/protocol"
	"github.com/luskaner/ageBattleServer/internal/server"
)

func wireFrame(msgType byte, body []byte) []byte {
	frame := make([]byte, 0, 3+len(body))
	frame = append(frame, byte((1+len(body))>>8), byte(1+len(body)), msgType)
	frame = append(frame, body...)
	return frame
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Name != "My Server" {
		t.Errorf("Name = %q, want %q", cfg.Name, "My Server")
	}
	if cfg.BsPort != 27012 {
		t.Errorf("BsPort = %d, want 27012", cfg.BsPort)
	}
	if cfg.OutOfBandPort != 27212 {
		t.Errorf("OutOfBandPort = %d, want 27212", cfg.OutOfBandPort)
	}
	if len(cfg.RelayBroadcastPorts) != 0 {
		t.Errorf("RelayBroadcastPorts = %v, want empty (disabled)", cfg.RelayBroadcastPorts)
	}
	if cfg.LobbyHost != "localhost" {
		t.Errorf("LobbyHost = %q, want localhost", cfg.LobbyHost)
	}
	if cfg.Region != "global" {
		t.Errorf("Region = %q, want global", cfg.Region)
	}
	if cfg.ObserverMax != 512 {
		t.Errorf("ObserverMax = %d, want 512", cfg.ObserverMax)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
	}
}

func TestNewInvalidLogLevel(t *testing.T) {
	cfg := DefaultConfig()
	cfg.LogLevel = "verbose"
	if _, err := New(cfg); err == nil {
		t.Fatal("New with invalid LogLevel should fail")
	}
}

func TestNewAndRunCancelled(t *testing.T) {
	cfg := Config{
		BsPort:           0,
		OutOfBandPort:    0,
		LogLevel:         "error",
		SimulationPeriod: 0,
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := srv.Run(ctx); err != nil {
		t.Errorf("Run error: %v", err)
	}
}

func TestReplayDataHandlerEndToEnd(t *testing.T) {
	received := make(chan ReplayData, 1)
	cfg := DefaultConfig()
	cfg.BsPort = 0
	cfg.EphemeralListenPorts = server.EphemeralBS
	cfg.OutOfBandPort = 0
	cfg.WebSocketPort = 0
	cfg.RelayBroadcastPorts = nil
	cfg.LogLevel = "error"
	cfg.ReplayDataHandler = func(rd ReplayData) {
		received <- rd
	}

	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start error: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	// The listener bound ":0" and this is the port it got, so there is no window
	// in which this number belonged to nobody. Picking one by binding ":0" and
	// letting go is what this test used to do, and on Windows that loses the race:
	// a listener bind is exclusive, so whatever takes the port in between keeps it.
	port, ws := srv.BoundPorts()
	if port == 0 {
		t.Fatal("bsPort 0 after Start: the listener is not listening on a port")
	}
	if ws != 0 {
		t.Fatalf("webSocketPort %d with no webSocketPort configured", ws)
	}

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("Dial error: %v", err)
	}
	defer func() { _ = conn.Close() }()

	body := []byte{0x01, 0x00, 0x00, 0x03, 0x00, 0x05, 0x00}
	if _, err := conn.Write(wireFrame(protocol.MsgProxy, body)); err != nil {
		t.Fatalf("Write error: %v", err)
	}

	select {
	case rd := <-received:
		if !bytes.Equal(rd.Data, body) {
			t.Errorf("data mismatch: got %x want %x", rd.Data, body)
		}
		if rd.SessionID != 0 {
			t.Errorf("SessionID = %d, want 0 for unjoined client", rd.SessionID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for replay data")
	}
}
