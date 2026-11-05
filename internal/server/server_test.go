package server

import (
	"context"
	"testing"
	"time"

	"github.com/luskaner/ageBattleServer/internal/protocol"
)

func TestNewServer(t *testing.T) {
	cfg := &Config{
		Name:     "Test Server",
		BsPort:   0,
		LogLevel: "none",
	}

	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New error: %v", err)
	}
	if srv == nil {
		t.Fatal("New returned nil")
	}
	if srv.Config().Name != "Test Server" {
		t.Errorf("expected name 'Test Server', got '%s'", srv.Config().Name)
	}
}

func TestNewServerWithAllPorts(t *testing.T) {
	cfg := &Config{
		Name:                 "Full Server",
		BsPort:               27012,
		WebSocketPort:        9090,
		OutOfBandPort:        27013,
		RelayBroadcastPorts:  []uint{9999},
		LobbyHost:            "localhost",
		LobbyPort:            8443,
		PublicIP:             "192.168.1.1",
		PublicPort:           27012,
		SSLCert:              "",
		SSLKey:               "",
		Region:               "eu-west",
		ObserverMax:          256,
		SimulationPeriod:     16.0,
		PageSize:             65536,
		HeartbeatSecurityKey: "secret",
		ClusterName:          "test-cluster",
		LogLevel:             "debug",
	}

	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New error: %v", err)
	}

	if srv.bsListener == nil {
		t.Error("bsListener should be created")
	}
	if srv.wsListener == nil {
		t.Error("wsListener should be created")
	}
	if srv.oobListener == nil {
		t.Error("oobListener should be created")
	}
	if srv.relayBroadcaster == nil {
		t.Error("relayBroadcaster should be created")
	}
	if srv.lobbyClient == nil {
		t.Error("lobbyClient should be created")
	}
}

func TestServerMetrics(t *testing.T) {
	cfg := &Config{
		BsPort: 0,
	}

	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New error: %v", err)
	}

	srv.MetricsCollector().AddToCounter("TcpConnections", 5)
	srv.MetricsCollector().SetGauge("Clients", 10)

	if srv.MetricsCollector().GetCounter("TcpConnections") != 5 {
		t.Error("counter mismatch")
	}
	if srv.MetricsCollector().GetGauge("Clients") != 10 {
		t.Error("gauge mismatch")
	}
}

func TestServerObserverManagement(t *testing.T) {
	cfg := &Config{
		BsPort:                   0,
		ObserverMax:              5,
		ObserverUpdateIntervalMS: 8,
		SimulationPeriod:         125,
	}

	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New error: %v", err)
	}

	if got := msToTicks(cfg.ObserverUpdateIntervalMS, cfg.SimulationPeriod); got != 0 {
		t.Errorf("8ms at 125ms/ticks should convert to 0 ticks (as the original's conversion does), got %d", got)
	}

	// ...and that is why the flag alone cannot be the session's stride. With the
	// flag absent the original keeps the 8 TICKS it was initialised with and only
	// converts the flag inside the branch that runs when it was given. The
	// observer update strides the observer list by it, so 0 is the original's own
	// infinite loop and 1 would serve every observer on every tick.
	if got := srv.observerUpdateIntervalTicks(); got != defaultObserverUpdateIntervalTicks {
		t.Errorf("observerUpdateIntervalTicks with the flag absent = %d, want the %d-tick default",
			got, defaultObserverUpdateIntervalTicks)
	}
	cfg.ObserverUpdateIntervalSet = true
	if got := srv.observerUpdateIntervalTicks(); got != 0 {
		t.Errorf("observerUpdateIntervalTicks with the flag set to 8ms = %d, want the converted 0", got)
	}
	cfg.ObserverUpdateIntervalSet = false
	cfg.ObserverUpdateIntervalMS = 1000
	if got := srv.observerUpdateIntervalTicks(); got != defaultObserverUpdateIntervalTicks {
		t.Errorf("the flag's value must not be read when it was not given, got %d", got)
	}

	if got := srv.observerSocketSkipTicks(); got != 0 {
		t.Errorf("8ms skip should convert to 0 ticks, got %d", got)
	}

	sess := srv.sessionManager.CreateSession()
	sess.SetObserverConfig(2, 8)

	if //goland:noinspection GoMaybeNil
	sess.MaxObservers != 2 {
		t.Errorf("expected 2 max observers, got %d", sess.MaxObservers)
	}
}

func TestServerPeerManagement(t *testing.T) {
	cfg := &Config{
		BsPort: 0,
	}

	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New error: %v", err)
	}

	sess := srv.SessionManager().CreateSession()
	peer := sess.NewPeer(1, nil)
	sess.AddPeer(peer)

	if sess.GetPeer(1) == nil {
		t.Error("peer 1 should exist")
	}

	sess.RemovePeer(1)
	if sess.GetPeer(1) != nil {
		t.Error("peer 1 should be removed")
	}
}

func TestBuildBroadcastMessage(t *testing.T) {
	cfg := &Config{
		Name:                "Test",
		PublicIP:            "192.168.1.1",
		WebSocketPort:       9090,
		PublicPort:          27012,
		OutOfBandPort:       27013,
		RelayBroadcastPorts: []uint{9999},
		SimulationPeriod:    16.0,
	}

	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New error: %v", err)
	}

	srv.relayBroadcaster = NewRelayBroadcaster(srv.buildBroadcastMessages, []uint{9999})
	msg := srv.buildBroadcastMessage()

	if len(msg) < 30 {
		t.Errorf("broadcast message too short: %d bytes", len(msg))
	}

	if msg[0] != 0x21 {
		t.Errorf("expected broadcast type 0x21, got 0x%02x", msg[0])
	}

	if msg[1] != 0x24 || msg[2] != 0x00 {
		t.Errorf("expected little-endian GUID length 0x24 0x00, got 0x%02x 0x%02x", msg[1], msg[2])
	}

	guidLen := int(msg[1]) | int(msg[2])<<8
	guid := string(msg[3 : 3+guidLen])
	if len(guid) != 36 {
		t.Errorf("expected 36-char GUID, got %d: %q", len(guid), guid)
	}

	off := 3 + guidLen
	nameLen := int(msg[off]) | int(msg[off+1])<<8
	if string(msg[off+2:off+2+nameLen]) != "Test" {
		t.Errorf("expected name 'Test', got %q", string(msg[off+2:off+2+nameLen]))
	}
	off += 2 + nameLen

	portOffsets := []struct {
		name string
		want uint16
	}{
		{"publicPort", 27012},
		{"webSocketPort", 0},
		{"outOfBandPort", 27013},
	}
	for _, p := range portOffsets {
		got := uint16(msg[off]) | uint16(msg[off+1])<<8
		if got != p.want {
			t.Errorf("expected %s little-endian %d, got %d", p.name, p.want, got)
		}
		off += 2
	}

	if off != len(msg) {
		t.Errorf("expected message length %d, got %d", off, len(msg))
	}
}

func TestBuildBroadcastMessages(t *testing.T) {
	cfg := &Config{
		Name:                "Test",
		PublicIP:            "192.168.1.1",
		WebSocketPort:       9090,
		PublicPort:          27012,
		OutOfBandPort:       27013,
		RelayBroadcastPorts: []uint{9999},
		SimulationPeriod:    16.0,
	}

	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New error: %v", err)
	}

	srv.relayBroadcaster = NewRelayBroadcaster(srv.buildBroadcastMessages, []uint{9999})

	parseLegacy := func(t *testing.T, msg []byte) {
		t.Helper()
		if len(msg) < 8 {
			t.Fatalf("legacy message too short: %d bytes", len(msg))
		}
		if msg[0] != protocol.MsgRelayBroadcastLegacy {
			t.Fatalf("expected legacy type 0x%02x, got 0x%02x", protocol.MsgRelayBroadcastLegacy, msg[0])
		}
		guidLen := int(msg[1]) | int(msg[2])<<8
		off := 3 + guidLen
		nameLen := int(msg[off]) | int(msg[off+1])<<8
		off += 2 + nameLen
		port := uint16(msg[off]) | uint16(msg[off+1])<<8
		if port != uint16(cfg.PublicPort) {
			t.Errorf("expected legacy public port %d, got %d", cfg.PublicPort, port)
		}
		off += 2
		ws := uint16(msg[off]) | uint16(msg[off+1])<<8
		if ws != 0 {
			t.Errorf("expected legacy websocket port 0 (not advertised), got %d", ws)
		}
		if off+2 != len(msg) {
			t.Errorf("expected message length %d, got %d", off+2, len(msg))
		}
	}

	t.Run("port 8888 sends legacy only", func(t *testing.T) {
		msgs := srv.buildBroadcastMessages(8888)
		if len(msgs) != 1 {
			t.Fatalf("expected 1 message, got %d", len(msgs))
		}
		parseLegacy(t, msgs[0])
	})

	t.Run("port 9999 sends current and legacy", func(t *testing.T) {
		msgs := srv.buildBroadcastMessages(9999)
		if len(msgs) != 2 {
			t.Fatalf("expected 2 messages, got %d", len(msgs))
		}
		if msgs[0][0] != protocol.MsgRelayBroadcast {
			t.Errorf("expected current type 0x%02x, got 0x%02x", protocol.MsgRelayBroadcast, msgs[0][0])
		}
		parseLegacy(t, msgs[1])
	})

	t.Run("other port sends current only", func(t *testing.T) {
		msgs := srv.buildBroadcastMessages(1234)
		if len(msgs) != 1 {
			t.Fatalf("expected 1 message, got %d", len(msgs))
		}
		if msgs[0][0] != protocol.MsgRelayBroadcast {
			t.Errorf("expected current type 0x%02x, got 0x%02x", protocol.MsgRelayBroadcast, msgs[0][0])
		}
	})
}

func TestServerConfig(t *testing.T) {
	cfg := &Config{
		BsPort:               27012,
		SimulationPeriod:     32.0,
		PageSize:             32768,
		AllowTimeoutOverride: true,
	}

	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New error: %v", err)
	}

	if srv.Config().SimulationPeriod != 32.0 {
		t.Errorf("expected simulation period 32.0, got %f", srv.Config().SimulationPeriod)
	}
}

func TestHandleLobbyConnectTimeoutOverride(t *testing.T) {
	body := []byte{
		0x66, 0x55, 0x44, 0x33, 0x22, 0x11, // sessionID
		0xBB, 0xAA, // titleID
		0xFF, 0xEE, 0xDD, 0xCC, 0xBB, 0xAA, 0x99, 0x88, // secret
		0x00,       // reconnectState
		0x05, 0x00, // clientMessageVersion
		0xBE, 0xBA, 0xFE, 0xCA, // timeoutOverride 0xCAFEBABE
		0x88, 0x77, 0x66, 0x55, 0x44, 0x33, 0x22, 0x11, // profileID
	}

	t.Run("flag off", func(t *testing.T) {
		srv := createTestServer(t)
		client := NewClient(&mockNetConn{}, srv)
		client.handleMessage(protocol.MsgLobbyConnect, body)
		if client.timeoutOverride != 0 {
			t.Errorf("expected timeoutOverride 0 without -allowTimeoutOverride, got %d", client.timeoutOverride)
		}
	})

	t.Run("flag on", func(t *testing.T) {
		srv := createTestServer(t)
		srv.config.AllowTimeoutOverride = true
		client := NewClient(&mockNetConn{}, srv)
		client.handleMessage(protocol.MsgLobbyConnect, body)
		if client.timeoutOverride != 0xCAFEBABE {
			t.Errorf("expected timeoutOverride 0xCAFEBABE with -allowTimeoutOverride, got %#x", client.timeoutOverride)
		}
	})

	t.Run("malformed body", func(t *testing.T) {
		srv := createTestServer(t)
		srv.config.AllowTimeoutOverride = true
		client := NewClient(&mockNetConn{}, srv)
		client.handleMessage(protocol.MsgLobbyConnect, []byte{0x01, 0x02, 0x03})
		if client.timeoutOverride != 0 {
			t.Errorf("expected timeoutOverride 0 for malformed body, got %d", client.timeoutOverride)
		}
	})
}

func TestServerStartStop(t *testing.T) {
	cfg := &Config{
		Name:                "Test Server",
		BsPort:              0,
		WebSocketPort:       0,
		OutOfBandPort:       0,
		RelayBroadcastPorts: nil,
		LobbyPort:           0,
		LogLevel:            "none",
	}

	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err = srv.Start(ctx)
	if err != nil {
		t.Fatalf("Start error: %v", err)
	}

	_ = srv.Stop()
}

// deadClient is a ClientConn with nothing behind it. AddClient only stores the
// value in two maps and a counter, so this is enough to be one, and it keeps the
// test below from needing a socket. Close is a method of its own because after
// Stop the server now closes the late arrival instead of registering it.
//
// # WHAT THIS IS FOR
//
// A connection the listener accepted a moment before it was closed is handed to
// AddClient (bs_listener.go:46 -> NewClient -> server.go:480) by its own
// goroutine, and that can happen after Stop has already run: Stop closes the
// listener first and clears the client registry at the end. Stop used to set
// clientSet to nil, so that arrival wrote to a nil map and took the whole process
// down, not just the connection: measured on this machine while stressing
// internal/sim with 24 CPU burners, 1 panic in 40 runs of the package, in the
// middle of a capture replay, with the stack ending at AddClient server.go:453.
// Clearing the map instead of dropping it keeps the same meaning (nothing is a
// live client after Stop) and makes the late arrival harmless.
//
// The falsification is one line: put clientSet = nil back in Stop and this test
// panics with "assignment to entry in nil map".
type deadClient struct {
	ClientConn
	closed bool
}

func (c *deadClient) Close() error {
	c.closed = true
	return nil
}

func TestAddClientAfterStopDoesNotPanic(t *testing.T) {
	silenceLogger()
	srv, err := New(&Config{
		EphemeralListenPorts: EphemeralBS,
		SimulationPeriod:     125,
		LogLevel:             "none",
	})
	if err != nil {
		t.Fatalf("New error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start error: %v", err)
	}
	if err := srv.Stop(); err != nil {
		t.Fatalf("Stop error: %v", err)
	}

	// The arrival the listener can still hand over after Stop. It must not take
	// the process with it, it must not be registered, and it must be closed.
	late := &deadClient{}
	srv.AddClient(late)
	if !late.closed {
		t.Error("a connection handed to a stopped server was not closed")
	}
	if n := srv.clientCount.Load(); n != 0 {
		t.Errorf("clientCount = %d after a late AddClient, want 0", n)
	}
	if len(srv.clients) != 0 {
		t.Errorf("clients = %d after a late AddClient, want 0", len(srv.clients))
	}
}
