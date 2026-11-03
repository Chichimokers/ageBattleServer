package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/luskaner/ageBattleServer/internal/config"
	"github.com/luskaner/ageBattleServer/internal/server"
)

func resetFlags() {
	flag.CommandLine = flag.NewFlagSet("test", flag.ContinueOnError)
}

func runHelper(t *testing.T, testCase string) (int, string) {
	t.Helper()
	return runHelperEnv(t, testCase, nil)
}

func runHelperEnv(t *testing.T, testCase string, extraEnv []string) (int, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable error: %v", err)
	}
	cmd := exec.Command(exe, "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1", "GO_HELPER_CASE="+testCase)
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("unexpected error type: %v", err)
	}
	return ee.ExitCode(), string(out)
}
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	defer os.Exit(0)
	os.Args = []string{"battleserver"}
	resetFlags()
	switch os.Getenv("GO_HELPER_CASE") {
	case "help":
		os.Args = append(os.Args, "-h")
	case "help-long":
		os.Args = append(os.Args, "--help")
	case "replay-flags-inert":
		// The full set of the original's replay arguments, plus every listener
		// off. ParseFlags must accept them and New/Start must not dial the
		// replay host: the uploader is gone.
		os.Args = append(os.Args,
			"-uploadVia", "lobby",
			"-replayHost", "127.0.0.1",
			"-replayPort", os.Getenv("GO_REPLAY_PORT"),
			"-login", "u",
			"-password", "p",
			"-replayDBName", "db",
			"-odbcDriver", "d",
			"-bsPort", "0",
			"-outOfBandPort", "0",
			"-webSocketPort", "0",
			"-relayBroadcastPort", "0",
			"-logLevel", "none",
		)
		cfg := config.ParseFlags()
		srv, err := server.New(cfg)
		if err != nil {
			_, _ = os.Stdout.WriteString("server.New: " + err.Error() + "\n")
			os.Exit(98)
		}
		if err := srv.Start(context.Background()); err != nil {
			_, _ = os.Stdout.WriteString("server.Start: " + err.Error() + "\n")
			os.Exit(97)
		}
		time.Sleep(300 * time.Millisecond)
		if err := srv.Stop(); err != nil {
			_, _ = os.Stdout.WriteString("server.Stop: " + err.Error() + "\n")
			os.Exit(96)
		}
		return
	case "simulation-period-zero":
		os.Args = append(os.Args, "-observersocketskipms", "8", "-simulationperiod", "0")
	case "main-bad-loglevel":
		os.Args = append(os.Args, "-loglevel", "bogus")
		main()
		_, _ = os.Stdout.WriteString("main did not exit\n")
		os.Exit(99)
	case "main-bad-modlevel":
		os.Args = append(os.Args, "-logmod", "Server", "-logmodlevel", "bogus")
		main()
		_, _ = os.Stdout.WriteString("main did not exit\n")
		os.Exit(99)
	case "main-new-fails":
		os.Args = append(os.Args, "-logmod", "Server", "-logmodlevel", "debug", "-websocketport", "27013", "-sslcert", "/nonexistent/cert.pem", "-sslkey", "/nonexistent/key.pem")
		main()
		_, _ = os.Stdout.WriteString("main did not exit\n")
		os.Exit(99)
	default:
		_, _ = os.Stdout.WriteString("unknown helper case\n")
		os.Exit(99)
	}
	config.ParseFlags()
	_, _ = os.Stdout.WriteString("did-not-exit\n")
	os.Exit(99)
}

func TestParseFlagsExitHelp(t *testing.T) {
	code, out := runHelper(t, "help")
	if code != 0 {
		t.Errorf("exit code for -h = %d, want 0", code)
	}
	if !strings.Contains(out, "Usage: battleserver") {
		t.Error("expected usage output on -h")
	}
}

func TestParseFlagsExitHelpLong(t *testing.T) {
	code, out := runHelper(t, "help-long")
	if code != 0 {
		t.Errorf("exit code for --help = %d, want 0", code)
	}
	if !strings.Contains(out, "Usage: battleserver") {
		t.Error("expected usage output on --help")
	}
}

// TestReplayFlagsAreAcceptedAndInert: the original's replay arguments are still
// accepted, and the server no longer connects to the replay host at all. The
// child runs a real server.New/Start/Stop with those flags against a listener
// bound here; the listener must never see a connection.
func TestReplayFlagsAreAcceptedAndInert(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port

	accepted := make(chan struct{}, 1)
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			accepted <- struct{}{}
			_ = conn.Close()
		}
	}()

	code, out := runHelperEnv(t, "replay-flags-inert",
		[]string{fmt.Sprintf("GO_REPLAY_PORT=%d", port)})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; output:\n%s", code, out)
	}

	select {
	case <-accepted:
		t.Error("the server connected to the replay host; the replay upload was supposed to be removed")
	case <-time.After(500 * time.Millisecond):
	}
}

func TestParseFlagsExitSimulationPeriodZero(t *testing.T) {
	code, out := runHelper(t, "simulation-period-zero")
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(out, "simulationPeriod shouldn't be 0") {
		t.Errorf("expected conflict message, got %q", out)
	}
}

func TestMainExitBadLogLevel(t *testing.T) {
	if code, _ := runHelper(t, "main-bad-loglevel"); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

func TestMainExitBadModLevel(t *testing.T) {
	if code, _ := runHelper(t, "main-bad-modlevel"); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

func TestMainExitNewFails(t *testing.T) {
	if code, _ := runHelper(t, "main-new-fails"); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}
