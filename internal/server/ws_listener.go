package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"

	"github.com/lxzan/gws"

	"github.com/luskaner/ageBattleServer/internal/logger"
)

type WsSocketListener struct {
	port      uint
	certFile  string
	keyFile   string
	tlsConfig *tls.Config
	upgrader  *gws.Upgrader
	mu        sync.Mutex
	conns     []*gws.Conn
	httpSrv   *http.Server
	// ln is the listener net.Listen returned in Start. It is kept for GetPort:
	// with Config.EphemeralListenPorts the configured port is 0 and only the
	// bound one is the real address, which is what a caller that asked the OS
	// for a port has to dial.
	ln     net.Listener
	secure bool
}

type wsEventHandler struct {
	host         ClientHost
	counter      metricsCounter
	observerSkip uint32
	mu           sync.Mutex
	clients      map[*gws.Conn]*WsClient
	// listener lets OnClose remove the closed connection from the listener's
	// conn set (it is set once at construction, before any connection opens).
	listener *WsSocketListener
}

func (h *wsEventHandler) OnOpen(socket *gws.Conn) {
	h.counter.AddToCounter("WsConnections", 1)

	wc := NewWsClient(socket, h.host, h.counter, h.observerSkip)
	h.mu.Lock()
	h.clients[socket] = wc
	h.mu.Unlock()
}

func (h *wsEventHandler) OnClose(socket *gws.Conn, err error) {
	// A close frame or a peer that hangs up is a normal end, not a connection
	// error. The counter used to move on every close, which made a clean match
	// look like a failing transport. gws reports a received close frame as a
	// CloseError, so its normal codes are not errors either.
	normalClose := false
	var closeErr *gws.CloseError
	if errors.As(err, &closeErr) {
		switch closeErr.Code {
		case 1000, 1001, 1005:
			normalClose = true
		}
	}
	if err != nil && !normalClose {
		h.counter.AddToCounter("WsConnectionErrors", 1)
	}

	h.mu.Lock()
	wc := h.clients[socket]
	delete(h.clients, socket)
	h.mu.Unlock()

	if wc != nil {
		_ = wc.Close()
	}

	if h.listener != nil {
		h.listener.removeConn(socket)
	}
}

func (h *wsEventHandler) OnPing(socket *gws.Conn, payload []byte) {
	_ = socket.WritePong(payload)
}

func (h *wsEventHandler) OnPong(_ *gws.Conn, _ []byte) {}

func (h *wsEventHandler) OnMessage(socket *gws.Conn, message *gws.Message) {
	defer func() { _ = message.Close() }()

	data := message.Bytes()
	if len(data) == 0 {
		return
	}

	h.counter.AddToCounter("BytesReceived", float64(len(data)))

	h.mu.Lock()
	wc := h.clients[socket]
	h.mu.Unlock()
	if wc == nil {
		return
	}

	wc.touch()

	if logger.ShouldLog(logger.LevelDebug) {
		logger.Debug("WsTransport", "onMessage", "message of %d bytes from=%s", len(data), socket.RemoteAddr())
	}

	wc.enqueue(data)
}

func NewWsSocketListener(host ClientHost, counter metricsCounter, observerSkip uint32, port uint, certFile, keyFile string) (*WsSocketListener, error) {
	l := &WsSocketListener{
		port:     port,
		certFile: certFile,
		keyFile:  keyFile,
		secure:   certFile != "" && keyFile != "",
	}

	if l.secure {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("load cert: %w", err)
		}
		l.tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			NextProtos:   []string{"http/1.1"},
		}
	}

	handler := &wsEventHandler{
		host:         host,
		counter:      counter,
		observerSkip: observerSkip,
		clients:      make(map[*gws.Conn]*WsClient),
		listener:     l,
	}

	l.upgrader = gws.NewUpgrader(handler, &gws.ServerOption{
		ParallelEnabled: false,
		ReadBufferSize:  1024,
		// The 101 response carries a Server header and this one does
		// not without it. Measured on every capture with a webSocketPort
		// (2026-09-30_010531_a553ef8fd63c and 2026-09-30_183120_c4e7c284ff1c,
		// both directions, both connections): the game is answered with
		// Connection, Sec-WebSocket-Accept, `Server: WebSocket++/0.8.2` and
		// Upgrade, and the value is the websocketpp build it links. It is
		// reproduced because the wire is what is being reproduced, and the
		// internal/sim harness compares the header set of the two answers so the
		// difference cannot come back unnoticed.
		//
		// The ORDER still differs and cannot be changed from here: gws writes
		// `Upgrade` and `Connection` itself (upgrader.go responseWriter.Init)
		// before any header a caller adds. internal/sim reports the order and
		// says out loud why it is not must-match.
		ResponseHeader: http.Header{"Server": []string{"WebSocket++/0.8.2"}},
	})

	return l, nil
}

func (l *WsSocketListener) Start(_ context.Context) error {
	addr := fmt.Sprintf(":%d", l.port)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		socket, err := l.upgrader.Upgrade(w, r)
		if err != nil {
			return
		}

		l.mu.Lock()
		l.conns = append(l.conns, socket)
		l.mu.Unlock()

		go socket.ReadLoop()
	})

	httpSrv := &http.Server{
		Addr:      addr,
		Handler:   mux,
		TLSConfig: l.tlsConfig,
	}
	l.httpSrv = httpSrv

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	l.mu.Lock()
	l.ln = ln
	l.mu.Unlock()

	go func() {
		var err error
		if l.secure {
			err = httpSrv.Serve(tls.NewListener(ln, l.tlsConfig))
		} else {
			err = httpSrv.Serve(ln)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("WsTransport", "listen", "webSocketPort error: %v", err)
		}
	}()

	protocol := "ws"
	if l.secure {
		protocol = "wss"
	}
	logger.Info("WsTransport", "listen", "webSocketPort listening on %s://%s (using gws)", protocol, l.GetLocalAddr())
	return nil
}

func (l *WsSocketListener) Stop() {
	l.mu.Lock()
	for _, conn := range l.conns {
		_ = conn.WriteClose(1000, []byte("server shutting down"))
	}
	l.conns = nil
	srv := l.httpSrv
	l.mu.Unlock()

	if srv != nil {
		_ = srv.Shutdown(context.Background())
	}

	l.mu.Lock()
	l.httpSrv = nil
	l.ln = nil
	l.mu.Unlock()
}

func (l *WsSocketListener) IsSecure() bool {
	return l.secure
}

// GetLocalAddr is the address the listener is on, ":port". After Start it is the
// port net.Listen actually bound, which is the only one that is the real address
// when the port was OS-assigned (Config.EphemeralListenPorts); before Start it is
// the configured one, which is the ":port" form a log line uses.
func (l *WsSocketListener) GetLocalAddr() string {
	if p := l.GetPort(); p != 0 {
		return fmt.Sprintf(":%d", p)
	}
	if l.httpSrv != nil && l.httpSrv.Addr != "" {
		return l.httpSrv.Addr
	}
	return fmt.Sprintf(":%d", l.port)
}

// GetPort is the TCP port the WebSocket listener is listening on, and 0 before
// Start. It is the bound port rather than the configured one, which is what the
// bs listener's GetPort already reports (internal/net/tcp.go:134) and what a
// caller that asked for an OS-assigned port has to dial.
func (l *WsSocketListener) GetPort() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ln == nil {
		return 0
	}
	if a, ok := l.ln.Addr().(*net.TCPAddr); ok {
		return a.Port
	}
	return 0
}

func (l *WsSocketListener) GetConnections() []*gws.Conn {
	l.mu.Lock()
	defer l.mu.Unlock()
	conns := make([]*gws.Conn, len(l.conns))
	copy(conns, l.conns)
	return conns
}

// removeConn drops a connection from the conn set and reports whether it was
// present. It is called both by CloseConnection and by OnClose, so a connection
// that disconnects by itself no longer accumulates in the set (previously
// l.conns only grew until Stop()).
func (l *WsSocketListener) removeConn(conn *gws.Conn) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, c := range l.conns {
		if c == conn {
			l.conns[i] = l.conns[len(l.conns)-1]
			l.conns = l.conns[:len(l.conns)-1]
			return true
		}
	}
	return false
}

func (l *WsSocketListener) CloseConnection(conn *gws.Conn) {
	if !l.removeConn(conn) {
		return // unknown or already-removed connection: no-op
	}
	_ = conn.WriteClose(1000, []byte("closing"))
}

func (l *WsSocketListener) GetConnectionCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.conns)
}

func (l *WsSocketListener) Broadcast(msgType gws.Opcode, data []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()

	for _, conn := range l.conns {
		_ = conn.WriteMessage(msgType, data)
	}
}

func (l *WsSocketListener) UnderlyingConn() net.Conn {
	return nil
}
