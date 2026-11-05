package net

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"
	"uuid"
)

type Conn interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Close() error
	RemoteAddr() net.Addr
	SetDeadline(t time.Time) error
	SetReadDeadline(t time.Time) error
	SetWriteDeadline(t time.Time) error
}

type TCPListener struct {
	addr     string
	listener net.Listener
	handler  func(Conn)
	mu       sync.Mutex
	// conns tracks the accepted connections. It is a set keyed by the wrapped
	// conn so a closed connection removes itself (trackedConn.Close), instead
	// of a slice that kept every closed connection alive until Stop().
	conns  map[net.Conn]struct{}
	closed bool
}

func NewTCPListener(addr string, handler func(Conn)) *TCPListener {
	return &TCPListener{
		addr:    addr,
		handler: handler,
		conns:   make(map[net.Conn]struct{}),
	}
}

// trackedConn removes itself from the listener's conn set when closed. Wrapping
// the raw net.Conn keeps the tracking transparent: the handler (and the server
// transport built on it) still sees a plain net.Conn.
type trackedConn struct {
	net.Conn
	l *TCPListener
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.l.remove(c)
	return err
}

func (l *TCPListener) remove(c net.Conn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.conns, c)
}

func (l *TCPListener) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", l.addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", l.addr, err)
	}

	l.mu.Lock()
	l.listener = ln
	l.mu.Unlock()

	go l.watchCtx(ctx)
	go l.acceptLoop(ctx)
	return nil
}

// watchCtx closes the listener when ctx is cancelled so the accept loop's
// blocked Accept() unblocks and the goroutine exits without requiring an
// explicit Stop(). Stop() is idempotent, so a later explicit Stop() is safe.
func (l *TCPListener) watchCtx(ctx context.Context) {
	<-ctx.Done()
	l.Stop()
}

func (l *TCPListener) acceptLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		conn, err := l.listener.Accept()
		if err != nil {
			l.mu.Lock()
			closed := l.closed
			l.mu.Unlock()
			if closed {
				return
			}
			// Transient error (e.g. fd exhaustion): back off instead of a
			// tight spin that would pin a core.
			time.Sleep(10 * time.Millisecond)
			continue
		}

		tc := &trackedConn{Conn: conn, l: l}
		l.mu.Lock()
		l.conns[tc] = struct{}{}
		l.mu.Unlock()

		go l.handler(tc)
	}
}

func (l *TCPListener) Stop() {
	l.mu.Lock()
	l.closed = true
	if l.listener != nil {
		_ = l.listener.Close()
	}
	conns := make([]net.Conn, 0, len(l.conns))
	for c := range l.conns {
		conns = append(conns, c)
	}
	l.conns = make(map[net.Conn]struct{})
	l.mu.Unlock()
	// Close outside the lock: trackedConn.Close re-enters remove().
	for _, c := range conns {
		_ = c.Close()
	}
}

func (l *TCPListener) GetPort() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.listener == nil {
		// A stopped listener has no port to dial even though its address is
		// still readable from the closed socket.
		return 0
	}
	return l.listener.Addr().(*net.TCPAddr).Port
}

func GenerateGUID() string {
	return uuid.New().String()
}
