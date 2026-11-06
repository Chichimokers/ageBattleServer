package server

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/lxzan/gws"

	"github.com/luskaner/ageBattleServer/internal/protocol"
)

// The outbound queue exists for one reason: the main loop is a single goroutine
// and every session shares it, so a peer that stops reading must not be able to
// block it. Before the queue, the tick's 0x0A broadcast called gws's WriteMessage
// directly, and a peer whose receive window was full blocked the tick for every
// other session on the server.
//
// These tests are the property, not the implementation: a send to a peer that is
// not reading returns, another connection is served while it is in flight, and a
// peer that never reads is cut off instead of growing the queue without bound.

// dialWSNoRead dials the WebSocket port and never starts the read loop, so the
// server's writes fill the socket and block. It is the paused peer the queue
// exists for. Its receive buffer is shrunk to the smallest the OS accepts, which
// is half of the stall: see shrinkSendBuffer for the other half.
func dialWSNoRead(t *testing.T, port int) *gws.Conn {
	t.Helper()
	h := &wsTestHandler{}
	addr := fmt.Sprintf("ws://127.0.0.1:%d/", port)
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, _, err := gws.NewClient(h, &gws.ClientOption{Addr: addr})
		if err == nil {
			return conn
		}
		if time.Now().After(deadline) {
			t.Fatalf("WS dial: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// shrinkSendBuffer makes the server's own send buffer as small as the OS accepts.
// The stall has to happen inside the test's payload, and the kernel's send buffer
// is large enough to swallow it whole: Linux autotunes it up to tcp_wmem's 4 MiB,
// so 800 KiB fits in the server's buffer, every write returns, and nothing is ever
// queued. Shrinking the peer's receive buffer alone does not force the stall.
func shrinkSendBuffer(t *testing.T, wc *WsClient) {
	t.Helper()
	tcp, ok := wc.conn.NetConn().(*net.TCPConn)
	if !ok {
		t.Skipf("the server connection is not TCP (%T): the stall cannot be forced", wc.conn.NetConn())
	}
	if err := tcp.SetWriteBuffer(4096); err != nil {
		t.Fatalf("SetWriteBuffer: %v", err)
	}
}

// serverWsClientFor returns the server-side WsClient whose remote address is
// addr, which is the dialing connection's own local address.
func serverWsClientFor(t *testing.T, srv *Server, addr string) *WsClient {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		srv.mu.Lock()
		for _, c := range srv.clients {
			if wc, ok := c.(*WsClient); ok && wc.RemoteAddr() == addr {
				srv.mu.Unlock()
				return wc
			}
		}
		srv.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no server-side WsClient for %s", addr)
	return nil
}

// TestWsPausedReaderDoesNotStallTheSender is the property W5 buys. The paused
// peer's receive buffer is 4 KiB, the server's send buffer is 4 KiB and its reader
// never runs, so the server's writes fill the window and block; 800 KiB is far
// more than those buffers take. The send loop stands in for the main loop, and
// what is asserted is that it returns and that a second connection is served while
// it is in flight.
func TestWsPausedReaderDoesNotStallTheSender(t *testing.T) {
	srv, port := wsTestServer(t)

	paused := dialWSNoRead(t, port)
	// Closed before the server's own cleanup runs, so the writer goroutine is
	// unblocked by the socket going away rather than by a write deadline.
	defer func() { _ = paused.NetConn().Close() }()
	if tcp, ok := paused.NetConn().(*net.TCPConn); ok {
		_ = tcp.SetReadBuffer(4096)
	}
	wcPaused := serverWsClientFor(t, srv, paused.LocalAddr().String())
	shrinkSendBuffer(t, wcPaused)

	reader, readerH := dialWS(t, port)
	defer func() { _ = reader.WriteClose(1000, nil) }()
	wcReader := serverWsClientFor(t, srv, reader.LocalAddr().String())

	const frames = 200
	body := make([]byte, 4096)
	done := make(chan error, 1)
	go func() {
		for i := range frames {
			if err := wcPaused.SendMessage(protocol.MsgEndOfGameData, body); err != nil {
				done <- fmt.Errorf("frame %d: %w", i, err)
				return
			}
		}
		done <- nil
	}()

	// The other connection is served while the paused one is stalled. This is the
	// half that says the stall is not merely deferred: the writer goroutines are
	// per connection, so one peer's window cannot hold another's frames.
	if err := wcReader.SendMessage(protocol.MsgLobbyPing, []byte{1, 2, 3, 4}); err != nil {
		t.Fatalf("the second connection refused a frame: %v", err)
	}
	waitForFrame(t, readerH, protocol.MsgLobbyPing, 2*time.Second)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SendMessage refused a frame on an open connection: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SendMessage blocked on a peer that is not reading: the main loop would be stalled with it")
	}

	// And the peer really was stalled: 800 KiB cannot fit in a 4 KiB receive
	// window, so frames are still in the queue. Without this the test would pass
	// on a socket that simply took everything, which proves nothing.
	wcPaused.mu.Lock()
	queued := wcPaused.queuedBytes
	wcPaused.mu.Unlock()
	if queued == 0 {
		t.Fatal("nothing is queued for the paused peer: the socket took all 800 KiB, so this test did not exercise a stall")
	}
}

// TestWsSendQueueCapClosesTheConnection: a peer that never reads must not be able
// to grow the queue without bound. The send that would cross the cap closes the
// connection instead, with the queue-full error, and the frames already queued are
// dropped with it.
func TestWsSendQueueCapClosesTheConnection(t *testing.T) {
	srv, port := wsTestServer(t)

	paused := dialWSNoRead(t, port)
	defer func() { _ = paused.NetConn().Close() }()
	if tcp, ok := paused.NetConn().(*net.TCPConn); ok {
		_ = tcp.SetReadBuffer(4096)
	}
	wc := serverWsClientFor(t, srv, paused.LocalAddr().String())
	shrinkSendBuffer(t, wc)

	wc.mu.Lock()
	wc.maxQueuedBytes = 8 << 10
	wc.mu.Unlock()

	body := make([]byte, 4096)
	var refused error
	for i := 0; i < 2000; i++ {
		if err := wc.SendMessage(protocol.MsgEndOfGameData, body); err != nil {
			refused = err
			break
		}
	}
	if refused == nil {
		t.Fatal("8 MiB into an 8 KiB cap was accepted: the queue is unbounded")
	}
	if !strings.Contains(refused.Error(), "queue full") {
		t.Fatalf("refused with %q, want the queue-full error", refused)
	}
	wc.mu.Lock()
	closed := wc.closed
	queued := len(wc.sendQueue)
	wc.mu.Unlock()
	if !closed {
		t.Error("the connection was not closed when the queue overflowed")
	}
	if queued != 0 {
		t.Errorf("%d frame(s) still queued after the overflow: they belong to no match any more", queued)
	}
}

// TestWsCloseDeliversTheQueueBeforeClosing: a server-side close is not a drop. The
// frames queued before it still reach the peer, and the close frame comes after
// them, which is what a client that is being told the match is over needs.
func TestWsCloseDeliversTheQueueBeforeClosing(t *testing.T) {
	srv, port := wsTestServer(t)

	peer, peerH := dialWS(t, port)
	defer func() { _ = peer.WriteClose(1000, nil) }()
	wc := serverWsClientFor(t, srv, peer.LocalAddr().String())

	const frames = 8
	for i := range frames {
		if err := wc.SendMessage(protocol.MsgEndOfGameData, []byte{byte(i)}); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
	}
	if err := wc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	got := 0
	for got < frames && time.Now().Before(deadline) {
		for _, f := range peerH.takeFrames() {
			if typ, body := frameType(f); typ == protocol.MsgEndOfGameData && len(body) == 1 && int(body[0]) == got {
				got++
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got != frames {
		t.Fatalf("%d of %d queued frames reached the peer before the close", got, frames)
	}
}
