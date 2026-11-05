package main

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"time"
)

// t2ChunkPool reuses the per-read copies handed to the Target 2 sender
// goroutine: the pump's read buffer is reused, so the duplicate side needs its
// own bytes.
var t2ChunkPool = sync.Pool{
	New: func() any { return make([]byte, 0, 4096) },
}

// TCPProxyServer accepts incoming connections and duplicates streams to two target sockets.
// Target 1 is the primary path: writes to it are synchronous so its TCP backpressure
// applies (like any relay). Target 2 is the drained duplicate: it gets a dedicated
// sender goroutine with a bounded queue that drops when full, so a slow or hung
// Target 2 can never stall the primary path.
type TCPProxyServer struct {
	config          Config
	logger          *CSVLogger
	secondaryLogger *CSVLogger
	listeners       []net.Listener
	// clientPort is the port the first listener bound, which is the client-facing
	// bsPort. It is compared against to tell the plaintext RLink transport from
	// the ones that terminate TLS, so it has to be the bound port and not the
	// configured one: with ProxyPort 0 the configured one names nothing.
	clientPort int
	wg         sync.WaitGroup
	quit       chan struct{}

	mu      sync.Mutex
	stopped bool
	conns   map[net.Conn]struct{}

	mitmTLS      bool
	tlsServerCfg *tls.Config
	tlsClientCfg *tls.Config
	keyLogFile   *os.File
}

// NewTCPProxyServer constructs the proxy server
func NewTCPProxyServer(cfg Config, logger, secondaryLogger *CSVLogger) (*TCPProxyServer, error) {
	s := &TCPProxyServer{
		config:          cfg,
		logger:          logger,
		secondaryLogger: secondaryLogger,
		quit:            make(chan struct{}),
		conns:           make(map[net.Conn]struct{}),
	}

	if cfg.MitmTLS {
		cert, err := tls.LoadX509KeyPair(cfg.MitmCert, cfg.MitmKey)
		if err != nil {
			return nil, fmt.Errorf("load MITM cert/key: %w", err)
		}
		var keyLog io.Writer
		if cfg.MitmKeyLog != "" {
			f, err := os.OpenFile(cfg.MitmKeyLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
			if err != nil {
				return nil, fmt.Errorf("open MITM keylog: %w", err)
			}
			s.keyLogFile = f
			keyLog = f
		}
		s.mitmTLS = true
		s.tlsServerCfg = &tls.Config{
			Certificates: []tls.Certificate{cert},
			KeyLogWriter: keyLog,
		}
		s.tlsClientCfg = &tls.Config{
			InsecureSkipVerify: true,
			KeyLogWriter:       keyLog,
		}
	}

	return s, nil
}

// wrapTLS terminates TLS on a connection using the proxy certificate, or returns
// the connection unchanged when MITM is disabled.
func (s *TCPProxyServer) wrapTLS(conn net.Conn, side string) (net.Conn, error) {
	if !s.mitmTLS {
		return conn, nil
	}
	tlsConn := tls.Server(conn, s.tlsServerCfg)
	_ = tlsConn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tlsConn.Handshake(); err != nil {
		return nil, fmt.Errorf("TLS %s handshake: %w", side, err)
	}
	_ = tlsConn.SetDeadline(time.Time{})
	return tlsConn, nil
}

// dialTarget opens a TCP connection to a target and, when useTLS is set, completes
// a TLS client handshake against it.
func (s *TCPProxyServer) dialTarget(addr string, useTLS bool) (net.Conn, error) {
	//goland:noinspection ALL
	raw, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if !useTLS {
		return raw, nil
	}
	tlsConn := tls.Client(raw, s.tlsClientCfg)
	_ = tlsConn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tlsConn.Handshake(); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("TLS target handshake: %w", err)
	}
	_ = tlsConn.SetDeadline(time.Time{})
	return tlsConn, nil
}

// peekConn keeps a buffered reader in front of the connection so the first bytes
// can be inspected without losing them from the stream.
type peekConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *peekConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// isTLSRecord reports whether the first byte of a stream looks like a TLS record.
func isTLSRecord(b byte) bool {
	switch b {
	case 0x14, 0x15, 0x16, 0x17:
		return true
	}
	return false
}

// handleConnection processes client streams, duplicates payloads, and returns Target 1 output
func (s *TCPProxyServer) handleConnection(clientConn net.Conn, t1Port, t2Port, listenPort int) {
	defer s.wg.Done()
	defer func() {
		s.mu.Lock()
		delete(s.conns, clientConn)
		s.mu.Unlock()
		_ = clientConn.Close()
	}()

	// In MITM mode the proxy terminates TLS and bridges plaintext to both targets
	// over separate TLS connections. The client's first byte decides: a TLS record
	// means wss/oob (TLS bridge); plaintext means ws:// without certs (raw relay,
	// still captured).
	useTLS := s.mitmTLS && listenPort != s.clientPort
	if s.mitmTLS && listenPort != s.clientPort {
		br := bufio.NewReader(clientConn)
		_ = clientConn.SetReadDeadline(time.Now().Add(5 * time.Second))
		first, err := br.Peek(1)
		_ = clientConn.SetReadDeadline(time.Time{})
		if err != nil {
			log.Printf("[PROXY] Client closed before sending data: %v", err)
			return
		}
		clientConn = &peekConn{Conn: clientConn, r: br}
		if !isTLSRecord(first[0]) {
			useTLS = false
			log.Printf("[PROXY] Plaintext client (no TLS) on port %d, relaying raw", listenPort)
		} else {
			tlsConn, err := s.wrapTLS(clientConn, "client")
			if err != nil {
				log.Printf("[PROXY MITM] %v", err)
				return
			}
			clientConn = tlsConn
			log.Printf("[PROXY MITM] TLS established with client (TLS %x)", tlsConn.(*tls.Conn).ConnectionState().Version)
		}
	}

	clientHost, clientPort := "unknown", 0
	if ta, ok := clientConn.RemoteAddr().(*net.TCPAddr); ok {
		clientHost, clientPort = ta.IP.String(), ta.Port
	} else {
		clientHost = clientConn.RemoteAddr().String()
	}

	log.Printf("[PROXY] Incoming connection from %s:%d", clientHost, clientPort)

	// Connect to Target 1
	t1Addr := net.JoinHostPort(s.config.Target1Host, fmt.Sprintf("%d", t1Port))
	target1Conn, err := s.dialTarget(t1Addr, useTLS)
	if err != nil {
		log.Printf("[PROXY ERROR] Failed to connect to Target 1 (%s): %v", t1Addr, err)
		return
	}
	defer func(target1Conn net.Conn) {
		_ = target1Conn.Close()
	}(target1Conn)
	log.Printf("[PROXY] Connected to Target 1 (%s)", t1Addr)

	// Connect to Target 2 (Optional failure, won't break primary pipeline)
	t2Addr := net.JoinHostPort(s.config.Target2Host, fmt.Sprintf("%d", t2Port))
	target2Conn, err := s.dialTarget(t2Addr, useTLS)
	if err != nil {
		log.Printf("[PROXY WARNING] Failed to connect to Target 2 (%s): %v", t2Addr, err)
	} else {
		defer func(target2Conn net.Conn) {
			_ = target2Conn.Close()
		}(target2Conn)
		log.Printf("[PROXY] Connected to Target 2 (%s)", t2Addr)
	}

	var connWg sync.WaitGroup
	connWg.Add(4)

	// The proxy relays the same framing the battleserver speaks on the wire:
	// [len u16 BE][type][body] on the bs/oob ports, WebSocket frames on the ws
	// port. Frames can be split or coalesced across TCP reads, so each
	// direction reassembles complete frames before they are logged.
	mode := frameRLink
	if listenPort == s.config.ProxyWSPort {
		mode = frameWebSocket
	}

	// Target 2 duplicate sender: consumes pooled copies of every client read on
	// a bounded queue. A full queue drops the chunk (Target 2 is best-effort);
	// a write error keeps draining and dropping, never touching the primary.
	t2Send := make(chan []byte, 256)

	// 0. Target 2 duplicate writer (decoupled from the primary path).
	go func() {
		defer connWg.Done()
		if target2Conn == nil {
			for range t2Send {
			}
			return
		}
		for chunk := range t2Send {
			_, _ = target2Conn.Write(chunk)
			t2ChunkPool.Put(chunk)
		}
	}()

	// 1. Client -> Target 1 & Target 2
	go func() {
		defer connWg.Done()
		defer close(t2Send)
		asm := newFrameAssembler(mode)
		buf := make([]byte, 4096)
		for {
			n, err := clientConn.Read(buf)
			if err != nil {
				break
			}
			for _, frame := range asm.feed(buf[:n]) {
				// Log complete incoming client frame
				s.logger.LogMain(clientHost, clientPort, s.config.ProxyHost, listenPort, frame)
				// The same bytes are duplicated verbatim to Target 2 below, so
				// the frame boundaries are identical and can be recorded with the
				// T2 endpoints. Without this the secondary CSV only holds
				// server->client traffic for Target 2, which hides everything our
				// own server received: no way to tell a message we dropped from
				// one we never saw. Same logger as the T2->client rows below, so
				// both directions of Target 2 land in secondary_log_NN.csv.
				// Only recorded when Target 2 is actually connected, so the row
				// still means "this reached T2".
				if target2Conn != nil {
					s.secondaryLogger.LogSecondary(clientHost, clientPort, s.config.Target2Host, t2Port, frame)
				}
			}

			// Send to Target 1: synchronous so its TCP backpressure applies;
			// any write failure tears this connection group down.
			if _, err := target1Conn.Write(buf[:n]); err != nil {
				break
			}
			// Duplicate to Target 2 asynchronously: hand off a pooled copy,
			// dropping it when the duplicate side falls behind.
			if target2Conn != nil {
				chunk := t2ChunkPool.Get().([]byte)
				chunk = append(chunk[:0], buf[:n]...)
				select {
				case t2Send <- chunk:
				default:
					t2ChunkPool.Put(chunk)
				}
			}
		}
		_ = target1Conn.Close()
		if target2Conn != nil {
			_ = target2Conn.Close()
		}
	}()

	// 2. Target 1 -> Client
	go func() {
		defer connWg.Done()
		asm := newFrameAssembler(mode)
		buf := make([]byte, 4096)
		for {
			n, err := target1Conn.Read(buf)
			if err != nil {
				break
			}
			for _, frame := range asm.feed(buf[:n]) {
				// Log complete Target 1 response frame
				s.logger.LogMain(s.config.Target1Host, t1Port, clientHost, clientPort, frame)
			}

			// Forward back to the client; a write failure ends this group.
			if _, err := clientConn.Write(buf[:n]); err != nil {
				break
			}
		}
		_ = clientConn.Close()
	}()

	// 3. Target 2 -> Drain and Log
	go func() {
		defer connWg.Done()
		if target2Conn == nil {
			return
		}
		asm := newFrameAssembler(mode)
		buf := make([]byte, 4096)
		for {
			n, err := target2Conn.Read(buf)
			if err != nil {
				if err != io.EOF {
					// Expected connection close
				}
				break
			}
			for _, frame := range asm.feed(buf[:n]) {
				// Log complete secondary response frame
				s.secondaryLogger.LogSecondary(s.config.Target2Host, t2Port, clientHost, clientPort, frame)
			}
		}
	}()

	connWg.Wait()
}

// Start blocks until Stop is called. The listeners must already be open: Bind
// owns them, because the client-facing port has to be known and advertised
// before the children start. Calling Bind here as well was the double bind: the
// second bind either failed (the port was already owned by the first listener)
// or, worse, left a listener nobody tracked.
func (s *TCPProxyServer) Start() error {
	if len(s.listeners) == 0 {
		return fmt.Errorf("proxy not bound: call Bind before Start")
	}
	<-s.quit
	return nil
}

// Bind opens every listener and starts accepting on it, without blocking.
//
// It is separate from Start because the client-facing port has to be a real,
// owned number BEFORE anything advertises it: the relay broadcast that tells the
// game where to connect carries it, and the children are started with it. The
// proxy used to ask the OS for a port by binding ":0", read the number and close
// it, which leaves the port owned by nobody until the proxy binds it again, and
// on Windows a listener bind is exclusive, so whatever takes it in between keeps
// it and the whole capture dies on "could not bind to TCP address". Asking at bind
// time removes the window, and Bind is what makes the number available early.
//
// ProxyPort 0 means "the OS assigns it", which is what main sets when no -bsPort
// was given.
func (s *TCPProxyServer) Bind() error {
	if err := s.listenAndServe(s.config.ProxyPort, s.config.Target1Port, s.config.Target2Port); err != nil {
		s.closeListenersOnBindFailure()
		return err
	}
	if s.config.ProxyWSPort != 0 {
		if err := s.listenAndServe(s.config.ProxyWSPort, s.config.Target1WSPort, s.config.Target2WSPort); err != nil {
			s.closeListenersOnBindFailure()
			return err
		}
	}
	if err := s.listenAndServe(s.config.ProxyOOBPort, s.config.Target1OOBPort, s.config.Target2OOBPort); err != nil {
		s.closeListenersOnBindFailure()
		return err
	}
	return nil
}

// closeListenersOnBindFailure releases the listeners a partial Bind already
// opened. main exits on the error, but a caller or a test that reuses the proxy
// object would otherwise keep those ports and accept loops alive.
func (s *TCPProxyServer) closeListenersOnBindFailure() {
	for _, ln := range s.listeners {
		_ = ln.Close()
	}
	s.listeners = nil
	s.clientPort = 0
}

// ClientPort is the TCP port the game connects to, the one the relay broadcast
// advertises. It is the port the first listener actually bound, which is the only
// one there is when the OS assigned it, and has to be read after Bind.
func (s *TCPProxyServer) ClientPort() int { return s.clientPort }

func (s *TCPProxyServer) listenAndServe(listenPort, t1Port, t2Port int) error {
	addr := fmt.Sprintf("%s:%d", s.config.ProxyHost, listenPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("could not bind to TCP address %s: %w", addr, err)
	}
	// The first listener is the client-facing bsPort; the number it bound is what
	// the rest of the proxy has to call it, including the check that tells the
	// client-facing transport (plaintext RLink) from the ones that terminate TLS.
	bound := ln.Addr().(*net.TCPAddr).Port
	if s.clientPort == 0 {
		s.clientPort = bound
	}
	s.listeners = append(s.listeners, ln)
	log.Printf("[PROXY SERVER] Listening on TCP %s", addr)

	go s.acceptLoop(ln, t1Port, t2Port, bound)
	return nil
}

func (s *TCPProxyServer) acceptLoop(ln net.Listener, t1Port, t2Port, listenPort int) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				// The listener was closed by Stop or by a partial Bind rollback:
				// there is nothing to accept on it ever again.
				return
			}
			select {
			case <-s.quit:
				return
			default:
				log.Printf("[PROXY ACCEPT ERROR] %v", err)
				// Brief backoff so a persistent error (e.g. fd exhaustion)
				// degrades to a slow retry instead of a busy spin.
				time.Sleep(10 * time.Millisecond)
				continue
			}
		}

		// Register under the same lock that observes `stopped` so Stop can
		// never Wait while an Add is still in flight.
		s.mu.Lock()
		if s.stopped {
			s.mu.Unlock()
			_ = conn.Close()
			return
		}
		s.wg.Add(1)
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		go s.handleConnection(conn, t1Port, t2Port, listenPort)
	}
}

// Stop closes the proxy listeners and every live connection so the pump
// goroutines exit, then waits for them. Idempotent: the signal handler and the
// deferred shutdown path may both call it.
func (s *TCPProxyServer) Stop() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()

	close(s.quit)
	for _, ln := range s.listeners {
		_ = ln.Close()
	}
	// Closing the client connections unblocks their pumps; each pump closes
	// its target side in turn.
	for _, c := range conns {
		_ = c.Close()
	}
	if s.keyLogFile != nil {
		_ = s.keyLogFile.Close()
	}
	s.wg.Wait()
}
