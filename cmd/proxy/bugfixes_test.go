package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestEnsureCertFilesCreatesTheParentDirectory covers N-19: the default pair
// lives under the executable's directory, whose parent may not exist on a first
// run.
func TestEnsureCertFilesCreatesTheParentDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "certs")
	cert := filepath.Join(dir, "selfsigned_cert.pem")
	key := filepath.Join(dir, "selfsigned_key.pem")
	if err := ensureCertFiles(cert, key); err != nil {
		t.Fatalf("ensureCertFiles: %v", err)
	}
	for _, p := range []string{cert, key} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was not written: %v", p, err)
		}
	}
}

// TestBindRollsBackOnFailure covers R-10: a partial Bind must release the
// listeners it already opened, so the object can be reused and the ports are
// not left owned by a failed start.
func TestBindRollsBackOnFailure(t *testing.T) {
	dir := t.TempDir()
	bsPort := releasedTCPPort(t)
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	busyPort := held.Addr().(*net.TCPAddr).Port

	cfg := Config{
		ProxyHost:        "127.0.0.1",
		ProxyPort:        bsPort,
		ProxyWSPort:      busyPort,
		MainLogPath:      filepath.Join(dir, MainLogName),
		SecondaryLogPath: filepath.Join(dir, SecondaryLogName),
	}
	main := NewCSVLogger(cfg.MainLogPath)
	defer func() { _ = main.Close() }()
	secondary := NewCSVLogger(cfg.SecondaryLogPath)
	defer func() { _ = secondary.Close() }()
	s, err := NewTCPProxyServer(cfg, main, secondary)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Bind(); err == nil {
		s.Stop()
		t.Fatal("Bind succeeded with the WebSocket port already held")
	}
	if got := s.ClientPort(); got != 0 {
		t.Errorf("ClientPort = %d after a failed Bind, want 0", got)
	}
	if got := len(s.listeners); got != 0 {
		t.Errorf("listeners = %d after a failed Bind, want 0", got)
	}
	// The bs listener opened first was released.
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", bsPort))
	if err != nil {
		t.Fatalf("the first listener was not released: %v", err)
	}
	_ = ln.Close()
}

// TestStartRequiresBind covers N-18: Start must not bind a second time. The
// listener set is opened once by Bind, and Start only blocks.
func TestStartRequiresBind(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		ProxyHost:        "127.0.0.1",
		ProxyPort:        0,
		MainLogPath:      filepath.Join(dir, MainLogName),
		SecondaryLogPath: filepath.Join(dir, SecondaryLogName),
	}
	main := NewCSVLogger(cfg.MainLogPath)
	defer func() { _ = main.Close() }()
	secondary := NewCSVLogger(cfg.SecondaryLogPath)
	defer func() { _ = secondary.Close() }()

	s, err := NewTCPProxyServer(cfg, main, secondary)
	if err != nil {
		t.Fatalf("NewTCPProxyServer: %v", err)
	}
	if err := s.Start(); err == nil {
		t.Fatal("Start succeeded without Bind")
	}
	if err := s.Bind(); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Start() }()
	// Start blocks: the listener is already serving, and Stop releases it.
	if port := s.ClientPort(); port == 0 {
		t.Fatal("ClientPort is 0 after Bind")
	}
	s.Stop()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Start returned %v after Stop, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("Start did not return after Stop")
	}
}
