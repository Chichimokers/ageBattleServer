package server

import (
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/luskaner/ageBattleServer/internal/protocol"
	"github.com/luskaner/ageBattleServer/internal/session"
)

// connectClient sends one LobbyConnect for (titleID, sessionID) with the given
// secret and reconnect state.
func connectClient(t *testing.T, srv *Server, c *mockMsgClient, titleID uint16, sessionID int64, secret uint64, state byte) {
	t.Helper()
	body := protocol.BuildLobbyConnect(titleID, sessionID, secret, state, 0, 5, 0, 0)
	srv.dispatchMessage(c, protocol.MsgLobbyConnect, body)
}

// TestReconnectOnAConnectionThatHoldsAnotherPeerIsRefused covers N-02: a
// reconnect whose secret resolves to a different peer than the one the
// connection already carries must not move the peer, because that would leave
// the first peer in the session with no connection to reach it.
func TestReconnectOnAConnectionThatHoldsAnotherPeerIsRefused(t *testing.T) {
	srv := createTestServer(t)
	a := &mockMsgClient{}
	b := &mockMsgClient{}
	connectClient(t, srv, a, 1, 7, 0x1111, 0)
	connectClient(t, srv, b, 1, 7, 0x2222, 0)

	sess := srv.sessionManager.GetSession(session.SessionKey{TitleID: 1, SessionID: 7})
	if sess == nil {
		t.Fatal("the session was not created")
	}
	if got := sess.PeerCount(); got != 2 {
		t.Fatalf("PeerCount() = %d, want 2", got)
	}

	// b asks to resume a's peer.
	connectClient(t, srv, b, 1, 7, 0x1111, 1)

	if b.closed {
		t.Fatal("the refused reconnect closed the connection")
	}
	if got := sess.PeerCount(); got != 2 {
		t.Errorf("PeerCount() = %d after the refused reconnect, want 2 (no orphan peer)", got)
	}
	if id := sess.PeerIDForConn(b); id != 2 {
		t.Errorf("b holds peer %d after the refused reconnect, want its own peer 2", id)
	}
	if id := sess.PeerIDForConn(a); id != 1 {
		t.Errorf("a holds peer %d after the refused reconnect, want 1", id)
	}

	// A reconnect on a FRESH connection is the normal path and still moves the
	// peer; the replaced connection is closed.
	c := &mockMsgClient{}
	connectClient(t, srv, c, 1, 7, 0x1111, 1)
	if !a.closed {
		t.Error("the replaced connection was not closed")
	}
	if id := sess.PeerIDForConn(c); id != 1 {
		t.Errorf("the fresh connection holds peer %d, want 1", id)
	}
}

// TestReconnectWithUnknownSecretOnAJoinedConnectionIsClosed covers R-01: a
// connection that already holds a peer and reconnects with a secret that
// resolves to no peer must follow the "no peer with that secret" path and close,
// not dereference the nil peer.
func TestReconnectWithUnknownSecretOnAJoinedConnectionIsClosed(t *testing.T) {
	srv := createTestServer(t)
	a := &mockMsgClient{}
	connectClient(t, srv, a, 1, 7, 0x1111, 0)
	sess := srv.sessionManager.GetSession(session.SessionKey{TitleID: 1, SessionID: 7})
	if sess == nil {
		t.Fatal("the session was not created")
	}

	// No panic here: before the fix the refusal branch formatted peer.ID with
	// peer == nil.
	connectClient(t, srv, a, 1, 7, 0xDEAD, 1)

	if !a.closed {
		t.Error("a reconnect with an unknown secret did not close the connection")
	}
	if got := sess.PeerCount(); got != 1 {
		t.Errorf("PeerCount() = %d after the refused reconnect, want 1", got)
	}
}

// TestObserverKeepsUnconditionalPingEcho covers R-03: an observer is no longer a
// session peer, but the original keeps its connection bound to the session and
// answers every 0x14; the 100-echo budget is only for connections with no
// session at all.
func TestObserverKeepsUnconditionalPingEcho(t *testing.T) {
	srv := createTestServer(t)
	srv.config.ObserverMax = 4
	sess := srv.sessionManager.GetOrCreateSession(session.SessionKey{TitleID: 1, SessionID: 1})
	sess.SetObserverConfig(4, 1)
	sess.SetReconnectable(true)
	sess.RecordFrame(protocol.MsgEndOfGameData, []byte{0xfe, 0x00, 0x00})
	sess.RecordFrame(protocol.MsgEndOfGameData, []byte{0xfe, 0x00, 0x00})

	observer := &mockMsgClient{}
	body := make([]byte, 24)
	binary.LittleEndian.PutUint64(body[:8], uint64(1)<<48|1)
	srv.dispatchMessage(observer, 0x1C, body)
	if sess.ObserverCount() != 1 {
		t.Fatal("the observer was not registered")
	}
	before := len(observer.sent)
	const pings = 105
	for range pings {
		srv.handleLobbyPing(observer, []byte{0x01})
	}
	if got := len(observer.sent) - before; got != pings {
		t.Errorf("observer ping echoes = %d, want %d (unconditional, not the 100 budget)", got, pings)
	}
}

// TestObserverWatchingTwoSessionsKeepsOneEntry covers N-04: a connection that
// starts observing a second session leaves the first one, and the session it
// stopped watching is erased when that was its last entry.
func TestObserverWatchingTwoSessionsKeepsOneEntry(t *testing.T) {
	srv := createTestServer(t)
	srv.config.ObserverMax = 4

	first := srv.sessionManager.GetOrCreateSession(session.SessionKey{TitleID: 1, SessionID: 1})
	second := srv.sessionManager.GetOrCreateSession(session.SessionKey{TitleID: 1, SessionID: 2})
	for _, sess := range []*session.Session{first, second} {
		sess.SetObserverConfig(4, 1)
		sess.SetReconnectable(true)
		sess.RecordFrame(protocol.MsgEndOfGameData, []byte{0xfe, 0x00, 0x00})
		sess.RecordFrame(protocol.MsgEndOfGameData, []byte{0xfe, 0x00, 0x00})
	}

	observer := &mockMsgClient{}
	startObserve := func(sid uint64) {
		body := make([]byte, 24)
		binary.LittleEndian.PutUint64(body[:8], uint64(1)<<48|sid)
		srv.dispatchMessage(observer, 0x1C, body)
	}

	startObserve(1)
	if got := first.ObserverCount(); got != 1 {
		t.Fatalf("first session observers = %d, want 1", got)
	}
	if last := observer.lastSent(); len(last) != 4 || last[3] != 0 {
		t.Fatalf("first StartObserve was not accepted: %x", last)
	}

	startObserve(2)
	if got := first.ObserverCount(); got != 0 {
		t.Errorf("first session observers = %d after re-subscribing elsewhere, want 0", got)
	}
	if got := second.ObserverCount(); got != 1 {
		t.Errorf("second session observers = %d, want 1", got)
	}
	if srv.sessionManager.GetSession(session.SessionKey{TitleID: 1, SessionID: 1}) != nil {
		t.Error("the session whose last observer moved away is still in the map")
	}

	srv.RemoveClient(observer)
	if srv.sessionManager.SessionCount() != 0 {
		t.Errorf("SessionCount() = %d after the observer disconnected, want 0", srv.sessionManager.SessionCount())
	}
}

// TestStopObserveClosesTheConnection covers N-06: the original closes the
// observing connection after removing the observer.
func TestStopObserveClosesTheConnection(t *testing.T) {
	srv := createTestServer(t)
	srv.config.ObserverMax = 4
	sess := srv.sessionManager.GetOrCreateSession(session.SessionKey{TitleID: 1, SessionID: 1})
	sess.SetObserverConfig(4, 1)
	sess.SetReconnectable(true)
	sess.RecordFrame(protocol.MsgEndOfGameData, []byte{0xfe, 0x00, 0x00})
	sess.RecordFrame(protocol.MsgEndOfGameData, []byte{0xfe, 0x00, 0x00})

	observer := &mockMsgClient{}
	body := make([]byte, 24)
	binary.LittleEndian.PutUint64(body[:8], uint64(1)<<48|1)
	srv.dispatchMessage(observer, 0x1C, body)
	if sess.ObserverCount() != 1 {
		t.Fatal("the observer was not registered")
	}

	srv.dispatchMessage(observer, protocol.MsgStopObserve, nil)
	if !observer.closed {
		t.Error("StopObserve did not close the connection")
	}
	if got := sess.ObserverCount(); got != 0 {
		t.Errorf("observers after StopObserve = %d, want 0", got)
	}
}

// TestChecksumFromASlotlessConnectionIsDropped covers N-10. The mapping used to
// survive a peer removal done outside the manager, which leaves
// FindSessionByPeer != nil while PeerIDForConn is 0; the 0x17 must stop there
// instead of queueing under slot 0.
func TestChecksumFromASlotlessConnectionIsDropped(t *testing.T) {
	srv := createTestServer(t)
	sess := srv.sessionManager.CreateSession()
	conn := &mockMsgClient{}
	if err := srv.sessionManager.AddPeer(sess, sess.NewPeer(1, conn)); err != nil {
		t.Fatal(err)
	}
	// Remove the peer from the session only, leaving the manager's relay index
	// stale: this is the state the old handleStartObserve produced.
	sess.RemovePeer(1)
	if srv.sessionManager.FindSessionByPeer(conn) == nil {
		t.Fatal("the test needs the stale relay index to reach the slot check")
	}

	checksum := []byte{1, 0, 0, 0, 0, 0, 0, 0, 0}
	srv.handleLobbyChecksumAck(conn, checksum, 0)

	if ack, _ := sess.PopAckTick(); ack != nil {
		t.Errorf("a slotless checksum produced an ack: %x", ack)
	}

	// The same bytes queued under slot 0 do produce an ack, which is what the
	// guard prevents.
	sess.QueueChecksumEntries(0, checksum[1:])
	if ack, _ := sess.PopAckTick(); ack == nil {
		t.Fatal("the control case did not queue anything: the assertion above proves nothing")
	}
}

// quietHost records dispatches but does not process a queued frame on NotifyWork,
// so a test can queue one and drive Process itself.
type quietHost struct{ recordingHost }

func (h *quietHost) NotifyWork(ClientConn) {}

// TestWsClientDropsAQueuedFrameOnClose covers the rest of N-03 / R-08: a frame
// queued before the close is discarded by the close path, and a Process that runs
// afterwards dispatches nothing.
func TestWsClientDropsAQueuedFrameOnClose(t *testing.T) {
	host := &quietHost{}
	wc := &WsClient{host: host}
	wc.enqueue([]byte{protocol.MsgLobbyConnect, 0x00})
	if len(wc.incoming) != 1 {
		t.Fatalf("queued frames = %d, want 1", len(wc.incoming))
	}
	wc.mu.Lock()
	wc.closed = true
	wc.mu.Unlock()
	wc.dropIncoming()
	if len(wc.incoming) != 0 {
		t.Errorf("queued frames after the close path = %d, want 0", len(wc.incoming))
	}
	wc.Process(100, time.Now())
	if got := len(host.got); got != 0 {
		t.Errorf("a frame queued before the close was dispatched (%d message(s))", got)
	}
}

// TestWsClientDropsFramesAfterClose covers N-03: a frame queued after the close
// is not buffered, and a Process on a closed client dispatches nothing.
func TestWsClientDropsFramesAfterClose(t *testing.T) {
	host := &recordingHost{}
	wc := &WsClient{host: host, closed: true}
	wc.enqueue([]byte{protocol.MsgLobbyConnect, 0x00})
	if len(wc.incoming) != 0 {
		t.Errorf("a closed client queued %d frame(s)", len(wc.incoming))
	}
	wc.Process(100, time.Now())
	if got := len(host.got); got != 0 {
		t.Errorf("a closed client dispatched %d message(s)", got)
	}
}

// TestStartRollsBackListenersOnFailure covers N-07: a failed Start leaves no
// listener behind and a later Start can bind the freed port.
func TestStartRollsBackListenersOnFailure(t *testing.T) {
	silenceLogger()
	// Hold one port on every interface for the bs listener so its Start fails.
	held, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint(held.Addr().(*net.TCPAddr).Port)

	srv, err := New(&Config{
		BsPort:               port,
		EphemeralListenPorts: EphemeralWS,
		SimulationPeriod:     125,
		LogLevel:             "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := srv.Start(ctx); err == nil {
		_ = srv.Stop()
		_ = held.Close()
		t.Fatal("Start succeeded with the bsPort already held")
	}
	if bs, ws := srv.BoundPorts(); bs != 0 || ws != 0 {
		t.Errorf("BoundPorts after a failed Start = %d/%d, want 0/0", bs, ws)
	}

	// Nothing from the failed attempt is still listening: the port can be
	// rebound by the same server.
	_ = held.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := srv.Start(ctx); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the second Start still cannot bind the released port")
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = srv.Stop()
}

// closingHost asks for the connection's close from inside a dispatch, which is
// what StopObserve does. It is the case the dispatch gate has to survive.
type closingHost struct {
	quietHost
	wc    *WsClient
	calls int
}

func (h *closingHost) dispatchMessage(_ messageClient, _ byte, _ []byte) {
	h.calls++
	_ = h.wc.Close()
}

// TestWsClientCloseDuringDispatchStopsTheBatch covers the rest of R-05: a close
// requested by a handler must not deadlock and no later frame of the same batch
// may be dispatched.
func TestWsClientCloseDuringDispatchStopsTheBatch(t *testing.T) {
	host := &closingHost{}
	wc := &WsClient{host: host}
	host.wc = wc
	wc.enqueue([]byte{protocol.MsgLobbyConnect, 0x00})
	wc.enqueue([]byte{protocol.MsgLobbyConnect, 0x01})

	wc.Process(100, time.Now())

	if host.calls != 1 {
		t.Errorf("dispatched %d messages, want 1: the close must stop the batch", host.calls)
	}
	if !wc.closed {
		t.Error("the connection was not closed after the handler asked for it")
	}
	if len(wc.incoming) != 0 {
		t.Errorf("incoming after the close = %d, want 0", len(wc.incoming))
	}
	if host.removedCount() != 1 {
		t.Errorf("RemoveClient calls = %d, want 1", host.removedCount())
	}
}

// TestChecksumFromANonMemberStillDispatchesTheNestedMessage covers R-15: a
// checksum entry from a connection without a slot is not queued, but the nested
// message still runs through the dispatcher as it does in the original.
func TestChecksumFromANonMemberStillDispatchesTheNestedMessage(t *testing.T) {
	srv := createTestServer(t)
	sess := srv.sessionManager.CreateSession()
	conn := &mockMsgClient{}
	if err := srv.sessionManager.AddPeer(sess, sess.NewPeer(1, conn)); err != nil {
		t.Fatal(err)
	}
	// Leave the manager's relay index stale, as the old handleStartObserve did.
	sess.RemovePeer(1)
	if srv.sessionManager.FindSessionByPeer(conn) == nil {
		t.Fatal("the test needs the stale relay index")
	}

	body := []byte{
		1,                      // one checksum entry
		0, 0, 0, 0, 0, 0, 0, 0, // the entry
		protocol.MsgLobbyPing, 0x01, // nested message
	}
	srv.handleLobbyChecksumAck(conn, body, 0)

	last := conn.lastSent()
	if len(last) < 3 || last[2] != protocol.MsgLobbyPing {
		t.Errorf("the nested 0x14 was not dispatched: %x", last)
	}
}
