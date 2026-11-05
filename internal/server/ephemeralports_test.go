package server

import (
	"context"
	"net"
	"testing"
)

// WHAT THIS FILE IS
//
// Config.EphemeralListenPorts and Server.BoundPorts exist for one caller:
// internal/sim, which starts a real server for every capture replay and cannot
// use the recorded ports. It used to pick them by binding :0, reading the port
// and letting go, and asking the server to bind that number later. That leaves
// the port owned by nobody in between, and a listener bind on Windows is
// exclusive, so anything that takes it in that window makes Start fail and the
// replay dies with "start server: ... bind". Measured on this machine: 1 in 8
// runs of `go test -short ./internal/sim` under load (SESSION-JOURNAL.md 38).
//
// The fix is to ask at bind time and read the answer back, so these three things
// have to be true and are asserted here:
//
//   - a server asked for OS-assigned ports really binds two DIFFERENT ones, and
//     BoundPorts names them;
//   - BoundPorts is the port that is listening, not the configured one: a caller
//     that dials it has to reach the transport;
//   - asking for an OS-assigned port on ONE transport does not switch the other
//     one on, because a test that exercises only the WebSocket listener must not
//     silently acquire a bs listener.
//
// It is also where the pick-a-port helper this package used to have is gone from:
// nothing in internal/server asks the OS for a port and lets it go, and the only
// test below that still names a port of its own says why in its own comment.
//
// The inventory of that shape over the whole module, so the next reader does not
// have to grep for it. Four places bind and KEEP: internal/net's own listener
// (tcp.go:63), the bs listener, the WebSocket listener (ws_listener.go:176) and
// the proxy's (cmd/proxy/tcp_proxy.go:389), and every one of them reports what it
// got. Two places cannot keep it and say so: the proxy's TestWaitForPortGivesUp,
// which needs a port nothing is listening on, and its
// TestClientPortIsTheConfiguredOneWhenThereIsOne, whose claim IS about a
// configured number. Both go through takeUnusedPort (cmd/proxy/
// process_manager_test.go), which checks the port and retries with a written cap
// instead of handing over a number it did not verify. One test binds a literal,
// port 23012, and says why: nothing that asks the OS for a port can be handed it.
// The integration tests ask for OS-assigned ports, the out-of-band one included.
// And that is the whole list; no production code picks a port by binding and
// letting go.
//
// The falsification is in the git history of this file's own comment: put
// EphemeralListenPorts back to "not read" in New and the ports read 0, and put
// it back to "one switch for both" and the WebSocket-only server grows a bs
// listener.

// TestEphemeralListenPortsBindTwoDistinctPorts is the whole claim. The ports
// come from the OS, so the two numbers must be real, must not be the configured
// ones (which are 0 here) and must not collide with each other: two transports on
// one port is a server whose second listener failed.
func TestEphemeralListenPortsBindTwoDistinctPorts(t *testing.T) {
	silenceLogger()
	srv, err := New(&Config{
		EphemeralListenPorts: EphemeralBoth,
		SimulationPeriod:     125,
		LogLevel:             "none",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if bs, ws := srv.BoundPorts(); bs != 0 || ws != 0 {
		t.Fatalf("BoundPorts before Start = %d/%d, want 0/0: a port that is not bound yet is not one to dial", bs, ws)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = srv.Stop() }()

	bs, ws := srv.BoundPorts()
	t.Logf("OS-assigned listen ports: bsPort %d, webSocketPort %d", bs, ws)
	if bs == 0 || ws == 0 {
		t.Fatalf("BoundPorts after Start = %d/%d, want two real ports: a server asked for OS-assigned ports has to "+
			"report the ones it got, or the caller has nothing to dial", bs, ws)
	}
	if bs == ws {
		t.Fatalf("both listeners bound port %d. The two transports are separate listeners and one port cannot hold both", bs)
	}
	if bsPort, wsPort := srv.bsListener.GetPort(), srv.wsListener.GetPort(); uint(bsPort) != bs || uint(wsPort) != ws {
		t.Fatalf("BoundPorts says %d/%d and the listeners say %d/%d. The caller dials what BoundPorts says, so the two "+
			"have to be the same number", bs, ws, bsPort, wsPort)
	}

	// And the port is not just reported, it is listening: the two transports
	// accept on it. A bsPort accept is enough to prove the listener is bound and
	// not merely recorded.
	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(int(bs))))
	if err != nil {
		t.Fatalf("dial the reported bsPort %d: %v", bs, err)
	}
	_ = conn.Close()
}

// TestEphemeralOutOfBandPortBindsAnOSAssignedPort extends the claim to the
// out-of-band listener. A test of that listener has no configured number it may
// own either, and a literal low port is not bindable everywhere: on Linux any port
// under 1024 needs privileges the test does not have.
func TestEphemeralOutOfBandPortBindsAnOSAssignedPort(t *testing.T) {
	silenceLogger()
	srv, err := New(&Config{
		EphemeralListenPorts: EphemeralOOB,
		SimulationPeriod:     125,
		LogLevel:             "none",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = srv.Stop() }()

	if srv.oobListener == nil {
		t.Fatal("a server asked for an OS-assigned out-of-band port built no out-of-band listener")
	}
	oob := srv.oobListener.GetPort()
	if oob == 0 {
		t.Fatal("the out-of-band listener reports port 0 after Start: the caller has nothing to dial")
	}
	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(oob)))
	if err != nil {
		t.Fatalf("dial the reported out-of-band port %d: %v", oob, err)
	}
	_ = conn.Close()

	if bs, ws := srv.BoundPorts(); bs != 0 || ws != 0 {
		t.Errorf("BoundPorts = %d/%d with only the out-of-band port asked for: naming one transport must not switch "+
			"the game transports on", bs, ws)
	}
}

// TestBoundPortsIsZeroForATransportThatIsOff keeps the flag contract intact:
// "-bsPort 0" means that transport is off, and a server that is off has no port
// to report. EphemeralListenPorts is not a way to switch a transport on when the
// operator did not ask for it, and it is a set rather than a switch precisely so
// that asking for the WebSocket port says nothing about the bs one.
func TestBoundPortsIsZeroForATransportThatIsOff(t *testing.T) {
	silenceLogger()
	srv, err := New(&Config{
		EphemeralListenPorts: EphemeralBS,
		SimulationPeriod:     125,
		LogLevel:             "none",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = srv.Stop() }()

	bs, ws := srv.BoundPorts()
	t.Logf("bsPort OS-assigned, webSocketPort not configured: BoundPorts = %d/%d", bs, ws)
	if bs == 0 {
		t.Error("the bsPort was asked for and is listening, so BoundPorts has to report it")
	}
	if ws != 0 {
		t.Errorf("BoundPorts reports webSocketPort %d with no webSocketPort configured. -webSocketPort 0 means the "+
			"transport is off and there is no port, and asking for an OS-assigned bs port does not switch it on", ws)
	}
	if srv.wsListener != nil {
		t.Error("a server that was not asked for a webSocketPort built the webSocket listener anyway")
	}
}

// TestBoundPortsReportsAConfiguredPort is the other direction: a port the operator
// did configure is reported as itself. This is the one claim in this file that
// cannot be made with an OS-assigned port, because the claim IS about the
// configured number, and asking the OS would compare the OS's answer with itself.
//
// The port is a literal and not one this test obtained by binding ":0" and
// letting go: the ephemeral range starts well above it, so nothing that asks the
// OS for a port can ever be handed this one, and the window in which it belongs
// to nobody does not exist. It is above 1023 because Linux refuses an unprivileged
// bind below 1024, and below 32768 because that is where the lowest default
// ephemeral range starts (Linux 32768, Windows and macOS 49152).
func TestBoundPortsReportsAConfiguredPort(t *testing.T) {
	const configured = 23012
	silenceLogger()
	srv, err := New(&Config{
		BsPort:           configured,
		SimulationPeriod: 125,
		LogLevel:         "none",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Skipf("Start could not bind the configured bsPort %d: %v. Something else on this machine owns it, which is "+
			"not what this test is about", configured, err)
	}
	defer func() { _ = srv.Stop() }()

	bs, ws := srv.BoundPorts()
	t.Logf("bsPort %d configured: BoundPorts = %d/%d", configured, bs, ws)
	if bs != configured {
		t.Errorf("BoundPorts reports bsPort %d for a bsPort configured as %d. A configured port is the port the "+
			"operator asked for and has to be reported as itself", bs, configured)
	}
	if ws != 0 {
		t.Errorf("BoundPorts reports webSocketPort %d with no webSocketPort configured", ws)
	}
}

// TestBoundPortsIsZeroAfterStop covers N-13: a port the server stopped
// listening on is not a port a caller can dial.
func TestBoundPortsIsZeroAfterStop(t *testing.T) {
	silenceLogger()
	srv, err := New(&Config{
		EphemeralListenPorts: EphemeralBoth,
		SimulationPeriod:     125,
		LogLevel:             "none",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if bs, ws := srv.BoundPorts(); bs == 0 || ws == 0 {
		t.Fatalf("BoundPorts after Start = %d/%d, want two real ports", bs, ws)
	}
	if err := srv.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if bs, ws := srv.BoundPorts(); bs != 0 || ws != 0 {
		t.Errorf("BoundPorts after Stop = %d/%d, want 0/0", bs, ws)
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
