package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luskaner/ageBattleServer/internal/protocol"
	"github.com/luskaner/ageBattleServer/internal/session"
)

type mockMsgClient struct {
	lastPing   time.Time
	sent       [][]byte
	timeoutSet uint32
	observer   bool
	pinged     bool
	closed     bool
	// peerID is the connection's identity, the one the observer list is keyed on
	// (session.PeerConnection).
	peerID uint64
	// msgVersion is the connection's client message version, the one the observer
	// end-of-game marker is gated on (session.PeerConnection).
	msgVersion uint16
}

func (m *mockMsgClient) ClientMessageVersion() uint16     { return m.msgVersion }
func (m *mockMsgClient) setClientMessageVersion(v uint16) { m.msgVersion = v }

func (m *mockMsgClient) Send(data []byte) error {
	if m.closed {
		return errors.New("closed")
	}
	m.sent = append(m.sent, append([]byte(nil), data...))
	return nil
}

// mockMsgClient stands in for a bsPort client: SendMessage applies the same
// [u16 BE length][type][body] framing the real transport does, so tests can
// assert on the exact bytes that would hit the wire.
func (m *mockMsgClient) SendMessage(msgType byte, body []byte) error {
	return m.Send(frameMessage(msgType, body))
}

func (m *mockMsgClient) Close() error {
	m.closed = true
	return nil
}

func (m *mockMsgClient) RemoteAddr() string { return "mock:1" }

func (m *mockMsgClient) Process(_ int, _ time.Time) {}

func (m *mockMsgClient) SetObserver(v bool) { m.observer = v }
func (m *mockMsgClient) IsObserver() bool   { return m.observer }

func (m *mockMsgClient) SetPeerIdentifier(v uint64) { m.peerID = v }
func (m *mockMsgClient) PeerIdentifier() uint64     { return m.peerID }

func (m *mockMsgClient) pingStarted() bool { return m.pinged }
func (m *mockMsgClient) setPingStarted()   { m.pinged = true }

func (m *mockMsgClient) setTimeoutOverride(v uint32) { m.timeoutSet = v }

func (m *mockMsgClient) lobbyPingTick(now time.Time, counter uint32) {
	if now.Sub(m.lastPing) < lobbyPingInterval {
		return
	}
	m.lastPing = now
	var body [8]byte
	binary.LittleEndian.PutUint32(body[:4], counter)
	_ = m.SendMessage(protocol.MsgLobbyPing, body[:])
}

func (m *mockMsgClient) lastSent() []byte {
	if len(m.sent) == 0 {
		return nil
	}
	return m.sent[len(m.sent)-1]
}

func TestDispatchMessageObserverDrop(t *testing.T) {
	srv := createTestServer(t)
	client := &mockMsgClient{observer: true}

	srv.dispatchMessage(client, protocol.MsgGameData, []byte{0x01, 0x00, 0x00})

	if len(client.sent) != 0 {
		t.Errorf("observer message should be dropped, sent %d frames", len(client.sent))
	}
}

// The allowlist the original applies to observer peers, read from its bitmask
// 0x1D100000 (with a guard that rejects any type above 0x1C, so bits 20, 24, 26, 27
// and 28). Verified against the dispatcher's own classes, where each one's declared
// size equals its type id.
func TestObserverMessageAllowed(t *testing.T) {
	allowed := []byte{
		protocol.MsgLobbyPing,        // 0x14 keepalive
		protocol.MsgStartObserve,     // 0x18 start observing
		protocol.MsgActivateObserver, // 0x1A activate
		protocol.MsgStopObserve,      // 0x1B stop
		0x1C,                         // StartObserveRequest Vx
	}
	for _, typ := range allowed {
		if !observerMessageAllowed(typ) {
			t.Errorf("type 0x%02x should be allowed for observers", typ)
		}
	}
	// Everything else is rejected by the original's mask, including 0x0F Peer
	// and 0x10 Proxy: an observer does not send those.
	rejected := []byte{
		protocol.MsgGameData,         // 0x09
		protocol.MsgEndOfGameData,    // 0x0A
		protocol.MsgSeek,             // 0x0D
		0x0E,                         // SeekResponse
		protocol.MsgPeer,             // 0x0F
		protocol.MsgProxy,            // 0x10
		protocol.MsgPing,             // 0x11
		protocol.MsgLobbyRelay,       // 0x13
		protocol.MsgGameDataChecksum, // 0x17
		protocol.MsgLeaveSimulation,  // 0x1F
	}
	for _, typ := range rejected {
		if observerMessageAllowed(typ) {
			t.Errorf("type 0x%02x must be rejected for observers (mask 0x1D100000)", typ)
		}
	}
}

// Regression: the filter used to omit 0x1A ActivateObserver, which made
// handleActivateObserver unreachable for a real observer, so observation could
// never actually activate. 0x0F Peer was allowed instead, and an observer
// sending it could make this server act on a Dropout the original ignores.
func TestObserverActivateObserverIsNotDropped(t *testing.T) {
	if observerMessageAllowed(protocol.MsgActivateObserver) != true {
		t.Fatal("0x1A ActivateObserver must pass the observer filter")
	}
	if observerMessageAllowed(protocol.MsgPeer) != false {
		t.Error("0x0F Peer must be dropped when it comes from an observer")
	}
}

func TestDispatchMessageUnknown(t *testing.T) {
	srv := createTestServer(t)
	client := &mockMsgClient{}
	srv.dispatchMessage(client, 0xFD, []byte{0x01})
	if len(client.sent) != 0 {
		t.Error("unknown type should produce no reply")
	}
}

// The original's dispatcher has no case for these types, so it only logs them
// (an unknown-message warning) and answers nothing. In particular
// 0x11 Ping is *not* echoed: the only ping the original ever emits is the 0x14
// LOBBYPING echo, so a client that pings with 0x11 gets silence from the original
// and from this server.
func TestDispatchMessageUndispatchedTypesGetNoReply(t *testing.T) {
	for _, typ := range []byte{
		protocol.MsgGameVoice,     // 0x07
		protocol.MsgMultiGameData, // 0x08
		protocol.MsgNudge,         // 0x0B
		protocol.MsgRouteCheck,    // 0x0C
		protocol.MsgSeek,          // 0x0D
		protocol.MsgSeekResponse,  // 0x0E
		protocol.MsgPing,          // 0x11
	} {
		t.Run(protocol.MessageTypeName(typ), func(t *testing.T) {
			srv := createTestServer(t)
			client := &mockMsgClient{}
			srv.dispatchMessage(client, typ, []byte{0x0B, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x0B, 0x0C})
			if len(client.sent) != 0 {
				t.Errorf("type 0x%02x has no case in the original dispatcher, want no reply, got %x", typ, client.sent)
			}
		})
	}
}

// 0x14 LOBBYPING is the one ping the original does echo, byte for byte.
func TestDispatchMessageLobbyPingEcho(t *testing.T) {
	srv := createTestServer(t)
	client := &mockMsgClient{}
	srv.dispatchMessage(client, protocol.MsgLobbyPing, []byte{0xAA})
	last := client.lastSent()
	if len(last) != 4 || last[0] != 0x00 || last[1] != 0x02 || last[2] != protocol.MsgLobbyPing || last[3] != 0xAA {
		t.Errorf("expected 00 02 14 aa, got %x", last)
	}
}

// A client that has not joined a session is echoed at most 100 times: the budget
// starts at 100 and the dispatcher decrements it per echo, after which the ping
// goes unanswered, silently.
func TestDispatchMessageLobbyPingEchoBudget(t *testing.T) {
	srv := createTestServer(t)
	client := &mockMsgClient{}

	for i := range lobbyPingEchoBudget {
		client.sent = nil
		srv.dispatchMessage(client, protocol.MsgLobbyPing, []byte{0xAA})
		if last := client.lastSent(); len(last) != 4 {
			t.Fatalf("ping %d should be echoed, got %x", i+1, last)
		}
	}

	client.sent = nil
	srv.dispatchMessage(client, protocol.MsgLobbyPing, []byte{0xAA})
	if last := client.lastSent(); len(last) != 0 {
		t.Errorf("ping %d must go unanswered once the budget is spent, got %x",
			lobbyPingEchoBudget+1, last)
	}
}

// Once the client is in a session the budget no longer applies: the original
// takes the branch that echoes a session member's ping, and echoes unconditionally,
// with no counter involved.
func TestDispatchMessageLobbyPingInSessionIgnoresBudget(t *testing.T) {
	srv := createTestServer(t)
	client := &mockMsgClient{}
	testSessionWithPeer(t, srv, client)

	// Spend the whole budget first.
	for range lobbyPingEchoBudget + 5 {
		srv.dispatchMessage(client, protocol.MsgLobbyPing, []byte{0xAA})
	}
	client.sent = nil
	srv.dispatchMessage(client, protocol.MsgLobbyPing, []byte{0xAA})
	if last := client.lastSent(); len(last) != 4 || last[2] != protocol.MsgLobbyPing {
		t.Errorf("a client in a session must be echoed regardless of the budget, got %x", last)
	}
}

// A self-addressed relay is not special: the first relay of a connection is
// handled by what it carries. The game's first relay is its RequestJoin, which
// is consumed and re-broadcast as a 0x0F to every session peer; a later
// self-addressed relay of game traffic is forwarded unchanged to its target,
// which here is the sender itself.
func TestDispatchMessageLobbyRelayRequestJoin(t *testing.T) {
	srv, _, client, other := keyedTwoPeerServer(t)

	nested := compressedPeerMessage(t, requestJoinInner(testKey, 0xABCDEF))
	srv.dispatchMessage(client, protocol.MsgLobbyRelay, relayBody(1, 1, nested))
	want := frameMessage(protocol.MsgPeer, nested[1:])
	assertOnlyFrame(t, "the sender", client, want)
	// The other peer of the session sees the same 0x0F (capture _13).
	assertOnlyFrame(t, "the other peer", other, want)

	client.sent, other.sent = nil, nil
	relay2 := relayBody(1, 1, peerMessage(0, []byte{protocol.PeerMsgGameData, 0xFD, 0x01, 0x00, 0x02}))
	srv.dispatchMessage(client, protocol.MsgLobbyRelay, relay2)
	assertOnlyFrame(t, "the addressed sender", client, frameMessage(protocol.MsgLobbyRelay, relay2))
	assertNothingSent(t, "the other peer", other)
}

// The server never builds a PEERHEADER: every 0x0F it emits carries a header a
// client sent. The re-broadcast of a RequestJoin repeats the received peer
// message byte for byte, so the ack / retransmission fields in that header are
// never ours to fill, and they must arrive at the other peers bit for bit, with
// the inner message still compressed. This pins that: a real 15-byte header
// with non-trivial values goes in and the same bytes come out.
func TestRequestJoinRebroadcastKeepsPeerHeaderVerbatim(t *testing.T) {
	srv, _, client, other := keyedTwoPeerServer(t)

	header := make([]byte, protocol.PeerHeaderLen)
	header[0] = protocol.FlagCompressed
	binary.LittleEndian.PutUint32(header[1:5], 0xDEADBEEF)
	binary.LittleEndian.PutUint32(header[5:9], 0x01020304)
	binary.LittleEndian.PutUint16(header[9:11], 0x0506)
	binary.LittleEndian.PutUint32(header[11:15], 0x0708090A)
	compressed := zlibBytes(t, requestJoinInner(testKey, 0x77))
	nested := append([]byte{protocol.MsgPeer}, header...)
	nested = append(nested, compressed...)

	srv.dispatchMessage(client, protocol.MsgLobbyRelay, relayBody(0, 0, nested))

	for name, c := range map[string]*mockMsgClient{"sender": client, "other peer": other} {
		got := c.lastSent()
		if len(got) < 3+protocol.PeerHeaderLen {
			t.Fatalf("%s: frame too short: %x", name, got)
		}
		if got[2] != protocol.MsgPeer {
			t.Errorf("%s: expected 0x0F, got 0x%02x", name, got[2])
		}
		if !bytes.Equal(got[3:3+protocol.PeerHeaderLen], header) {
			t.Errorf("%s: PEERHEADER altered in transit:\n got %x\nwant %x", name, got[3:3+protocol.PeerHeaderLen], header)
		}
		if !bytes.Equal(got[3+protocol.PeerHeaderLen:], compressed) {
			t.Errorf("%s: inner message altered in transit: %x", name, got[3+protocol.PeerHeaderLen:])
		}
	}
}

// A relay that carries only the two peer IDs has no nested message, so nothing
// asks for it to be forwarded: it reaches nobody.
func TestDispatchMessageLobbyRelayShortBody(t *testing.T) {
	srv := createTestServer(t)
	client := &mockMsgClient{}
	srv.AddClient(client)
	sess := srv.sessionManager.CreateSession()
	peer := sess.NewPeer(1, client)
	srv.sessionManager.AddPeer(sess, peer)

	srv.dispatchMessage(client, protocol.MsgLobbyRelay, []byte{0x01, 0x01})
	if len(client.sent) != 0 {
		t.Errorf("a relay with no nested message must reach nobody, got %x", client.sent)
	}
}

// TestLobbyRelayRequestJoinCrossRelay13 is the regression test for capture _13:
// player 2 could not join the lobby because its first relay, the peer message
// carrying its RequestJoin, was only echoed to the sender. The original also
// delivers it to the other session players, which is how the host learns a new
// player is joining: the original's own log of that run shows one send to
// player 2 and one to player 1, both 324 bytes. The frames have the capture's
// shape: 13 00 00 (to = from = 0, the joiner does not know
// its peer ID yet), a header 0f 04 00 00 00 00 ff ff ff ff 00 00 ff ff ff ff and
// a zlib-compressed RequestJoin.
func TestLobbyRelayRequestJoinCrossRelay13(t *testing.T) {
	srv, _, p1, p2 := keyedTwoPeerServer(t) // p1 is the host, p2 the joining player

	nested := compressedPeerMessage(t, requestJoinInner(testKey, 0x3742))
	srv.dispatchMessage(p2, protocol.MsgLobbyRelay, relayBody(0, 0, nested))
	want := frameMessage(protocol.MsgPeer, nested[1:])
	// Echo to the sender: the 0x0F with the relay's nested peer message.
	assertOnlyFrame(t, "the joining player", p2, want)
	// Cross-relay to the host: the same 0x0F. This was the regression (_13:
	// the real server sent it, we did not, so player 1 never saw player 2).
	assertOnlyFrame(t, "the host", p1, want)

	// P2's retry relays (13 00 00 0f 00 00 00 00 00 00 ff ff ff ff 00 00
	// ff ff ff ff 0d, every ~2s in the capture) carry an unhandled inner type,
	// so they are forwarded to peer 0, which does not exist: dropped, exactly
	// like the original.
	retry := []byte{0x00, 0x00, 0x0f, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0xff, 0xff, 0xff, 0xff, 0x00, 0x00, 0xff, 0xff, 0xff, 0xff, 0x0d}
	srv.dispatchMessage(p2, protocol.MsgLobbyRelay, retry)
	if len(p2.sent) != 1 || len(p1.sent) != 1 {
		t.Errorf("retry relay must be dropped (no peer 0), sent %d/%d messages", len(p2.sent), len(p1.sent))
	}
}

func TestHandleProxyDataReplayCallback(t *testing.T) {
	srv, sess, c1, _ := twoPeerServer(t)

	var mu sync.Mutex
	gotSession := uint64(0)
	var gotData []byte
	srv.config.ReplayDataHandler = func(sessionID uint64, data []byte) {
		mu.Lock()
		gotSession = sessionID
		gotData = append([]byte(nil), data...)
		mu.Unlock()
	}

	payload := []byte{0x01, 0x01, 0x00, 0x02, 0x00, 0x03, 0x00, 0x00}
	srv.dispatchMessage(c1, protocol.MsgProxy, payload)

	mu.Lock()
	defer mu.Unlock()
	if gotSession != uint64(sess.ID64()) {
		t.Errorf("sessionID = %d, want %d", gotSession, sess.ID64())
	}
	if !bytes.Equal(gotData, payload) {
		t.Errorf("data mismatch: got %x want %x", gotData, payload)
	}
}

func TestHandleProxyDataReplayCallbackNoSession(t *testing.T) {
	srv := createTestServer(t)
	c := &mockMsgClient{}
	srv.AddClient(c)

	called := false
	srv.config.ReplayDataHandler = func(sessionID uint64, _ []byte) {
		if sessionID != 0 {
			t.Errorf("sessionID = %d, want 0 for unjoined client", sessionID)
		}
		called = true
	}

	srv.dispatchMessage(c, protocol.MsgProxy, []byte{0x01, 0x00, 0x00})
	if !called {
		t.Error("ReplayDataHandler was not called")
	}
}

func TestHandleGameDataShortBody(t *testing.T) {
	srv := createTestServer(t)
	client := &mockMsgClient{}
	srv.dispatchMessage(client, protocol.MsgGameData, []byte{0x01, 0x00})
}

// TestHandleGameDataDoesNotFeedTheFrameStore pins where the store's contents come
// from, which is the one thing about the observer path that is invisible until an
// observer connects.
//
// The 0x09 GameData the players upload does NOT go into the store: the session
// tick appends the 0x0A it just built. An earlier revision stored the uploads and
// served them to observers, which is what left the second
// observer of capture 2026-09-29_012528_03cb67c7cf19 with 70 messages against the
// original's 803.
func TestHandleGameDataDoesNotFeedTheFrameStore(t *testing.T) {
	srv := createTestServer(t)
	client := &mockMsgClient{}
	testSessionWithPeer(t, srv, client)

	sess := srv.sessionManager.GetSession(session.SessionKey{SessionID: 1})
	if sess == nil {
		t.Fatal("session 1 not created")
	}

	gd := (&protocol.GameDataMessage{InnerType: 0xFE, Payload: []byte{0xAA, 0xBB}}).Serialize()
	srv.dispatchMessage(client, protocol.MsgGameData, gd)

	if sess.PageCount != 0 {
		t.Errorf("PageCount = %d, want 0: a 0x09 upload is not a stored frame", sess.PageCount)
	}
	if sess.Recording {
		t.Error("a 0x09 upload does not start recording; the store is created by the first ack")
	}

	// The ack tick is what fills it, through the same body the players get.
	sess.QueueGameData(1, []byte{0x00, 0x00})
	srv.sendEndOfGameDataTicks()
	if sess.PageCount != 1 {
		t.Errorf("PageCount = %d, want 1 after one ack tick", sess.PageCount)
	}
	if !sess.Recording {
		t.Error("session should be recording once the store exists")
	}
}

func TestHandleGameDataSetsPingStarted(t *testing.T) {
	srv := createTestServer(t)
	client := &mockMsgClient{}

	gd := (&protocol.GameDataMessage{InnerType: 0xFE, Payload: []byte{0x01}}).Serialize()
	srv.dispatchMessage(client, protocol.MsgGameData, gd)
	if !client.pingStarted() {
		t.Error("client should be ping-started after 0xfe gamedata")
	}

	srv.dispatchMessage(client, protocol.MsgGameData, (&protocol.GameDataMessage{InnerType: 0x01, Payload: []byte{0x01}}).Serialize())
	if !client.pingStarted() {
		t.Error("pingStarted should stay set")
	}
}

// testSessionWithPeer creates a session and one peer on client: the state a player
// connection leaves behind, and the setup a dozen of these tests need before they can
// dispatch anything that looks a session up.
//
// It is a FIXTURE and it says so, because the handler it replaces was not a path
// the original has. A top-level 0x01 creates nothing: the original's dispatcher has
// no case for it, the default arm only logs, and
// TestTopLevelRequestJoinIsAnUnknownMessageType is what keeps
// it that way. The path the game really takes is the LobbyConnect, which
// handleLobbyServe implements, and the tests that are about the
// wire drive that one. What is left here is the two calls a fixture needs -- the
// session and the peer slot -- and deliberately not the AddPeer broadcast the
// removed handler emitted, because the original's internal REQUESTJOIN does not
// emit one either: that path adds the id to the session's tree and returns, with no
// AddPeer anywhere.
func testSessionWithPeer(t *testing.T, srv *Server, client session.PeerConnection) *session.Session {
	t.Helper()
	sess := srv.sessionManager.CreateSession()
	if sess == nil {
		t.Fatal("CreateSession returned nil")
	}
	sess.SetObserverConfig(int(srv.config.ObserverMax), srv.observerUpdateIntervalTicks())
	peerID := sess.NextPeerID()
	if err := srv.sessionManager.AddPeer(sess, sess.NewPeer(peerID, client)); err != nil {
		t.Fatalf("AddPeer: %v", err)
	}
	return sess
}

// A top-level 0x01 does nothing, because the original's dispatcher has no case for
// it. This is the falsification of the handler that used to be there: it created a
// session, registered a peer and broadcast an AddPeer, and a client that sent an
// 0x01 got a whole match out of it where the original logs the type as unknown
// and returns: the `default:` arm's only other statement is the log itself.
//
// The dispatcher's case labels are 9, 0x0F, 0x10, 0x13, 0x14, 0x17, 0x18/0x1C, 0x1A,
// 0x1B, 0x1D and 0x1F, plus the 0x15 handled before the switch. The only path that
// does what the handler did -- the internal REQUESTJOIN -- is reached from ONE place:
// the type-2 case of the internal peer switch, which is itself reached from the
// `0x0F` case and from the recursive dispatch of the `0x13`. There is no path from a
// top-level 0x01 to any of that.
//
// What has to be checked is that the connection is left ALIVE as well: the original's
// default arm returns 0 and closes nothing, so a handler that answered by dropping the
// client would be a second difference with the same cause.
func TestTopLevelRequestJoinIsAnUnknownMessageType(t *testing.T) {
	srv := createTestServer(t)

	client := &mockMsgClient{}
	srv.dispatchMessage(client, protocol.MsgRequestJoin, []byte{0xDE, 0xAD})

	if n := srv.sessionManager.SessionCount(); n != 0 {
		t.Errorf("a top-level 0x01 created %d session(s): the original's dispatcher has no case for it and reaches nothing", n)
	}
	if len(client.sent) != 0 {
		t.Errorf("a top-level 0x01 answered %d frame(s) (%x): the original logs an unknown type and returns",
			len(client.sent), client.sent)
	}
	if client.closed {
		t.Error("the default arm returns 0 and closes nothing; the connection has to stay open")
	}
	if client.observer {
		t.Error("an unknown message type must not turn the connection into an observer")
	}
}

// The two guards that come before the switch are not the 0x01's to pass through
// either: a connection the original treats as unregistered (no session on it) is
// answered to nothing but the 0x15, and an observer's types are a bitmask. Neither
// is implemented, and both are UNINTENTIONAL-DIVERGENCES.md
// 10.2 and the observer gate; what this pins is that 0x01 does not become a third
// exception, so removing the handler cannot be worked around by another guard.
func TestTopLevelRequestJoinIsNotOneOfTheGateTypes(t *testing.T) {
	srv := createTestServer(t)

	player := &mockMsgClient{}
	before := srv.sessionManager.SessionCount()
	srv.dispatchMessage(player, protocol.MsgRequestJoin, nil)
	if got := srv.sessionManager.SessionCount(); got != before {
		t.Errorf("session count %d -> %d", before, got)
	}
	if player.closed {
		t.Error("the connection was closed")
	}

	// And as an observer: 0x01 is not in the bitmask, so it is dropped before the
	// switch, which is a different log and the same absence of effects.
	obs := &mockMsgClient{observer: true}
	srv.dispatchMessage(obs, protocol.MsgRequestJoin, nil)
	if len(obs.sent) != 0 || obs.closed {
		t.Errorf("an observer's 0x01 produced %d frame(s) and closed=%v", len(obs.sent), obs.closed)
	}
}

// observerTestServer builds a server with room for four observers. The frame store
// is fed by the session tick with the 0x0A it builds, and the observer is
// registered by the arm that answers 0, so these
// tests only need the store to hold something at all: a couple of RecordFrame
// calls is enough.
func observerTestServer(t *testing.T) *Server {
	srv, err := New(&Config{
		BsPort:           0,
		LogLevel:         "none",
		ObserverMax:      4,
		MaxPagesPerMatch: 1,
	})
	if err != nil {
		t.Fatalf("New error: %v", err)
	}
	srv.config.ObserverMax = 4
	return srv
}

// The legacy 0x18 layout carries no titleID, and the original's parser fills the
// field with (WORD)-1 = 0xFFFF, which as a signed 16-bit value is -1. Sessions are
// keyed on the pair (titleID, sessionID), so whether a 0x18 request is served
// depends on the title the session was created under: it misses a session created
// under a real title, and it *matches* a session created under -1, which is what a
// client that never set RL_TITLEID sends in its LobbyConnect.
func TestHandleStartObserveOldFormatMissesRealTitle(t *testing.T) {
	srv := observerTestServer(t)
	client := &mockMsgClient{}
	testSessionWithPeer(t, srv, client)

	sess := srv.sessionManager.GetSession(session.SessionKey{SessionID: 1})
	sess.RecordFrame(protocol.MsgEndOfGameData, []byte{0xFE, 0x00, 0x00})

	body := make([]byte, 8)
	binary.LittleEndian.PutUint64(body, 1)
	srv.dispatchMessage(client, protocol.MsgStartObserve, body)

	if last := client.lastSent(); len(last) != 0 {
		t.Errorf("0x18 forces titleID -1, which does not match a session created under title 0, so no reply is due; got %x", last)
	}
	if client.observer {
		t.Error("a 0x18 StartObserve must not mark the client as observer")
	}
}

// A client that does not set RL_TITLEID connects with titleID -1, so its session is
// keyed (-1, sessionID). A legacy 0x18 observe for that session must be served:
// both the connect and the request leave -1 in the same field.
func TestHandleStartObserveOldFormatMatchesDefaultTitle(t *testing.T) {
	srv := observerTestServer(t)
	client := &mockMsgClient{}

	// A first connect: reconnect state 0.
	connect := make([]byte, 17)
	binary.LittleEndian.PutUint64(connect[:8], 1) // sessionID 48b
	binary.LittleEndian.PutUint16(connect[6:8], 0xFFFF)
	srv.dispatchMessage(client, protocol.MsgLobbyConnect, connect)

	sess := srv.sessionManager.GetSession(session.SessionKey{
		TitleID:   -1,
		SessionID: 1,
	})
	if sess == nil {
		t.Fatal("session keyed (-1, 1) was not created by a LobbyConnect carrying titleID -1")
	}
	sess.RecordFrame(protocol.MsgEndOfGameData, []byte{0xFE, 0x00, 0x00})
	sess.RecordFrame(protocol.MsgEndOfGameData, []byte{0xFE, 0x00, 0x00})

	body := make([]byte, 8)
	binary.LittleEndian.PutUint64(body, 1)
	srv.dispatchMessage(client, protocol.MsgStartObserve, body)

	var last []byte
	for _, f := range client.sent {
		if len(f) >= 3 && f[2] == protocol.MsgStartObserveResp {
			last = f
		}
	}
	// Status 0 on the wire: the original puts the same byte there for "accepted"
	// and for "no replay" (capture _26: 0x1C -> 0x19=00 -> ActivateObserver ->
	// 7343 pages). Acceptance is proven by client.observer below, not by the byte.
	if len(last) != 4 || last[3] != 0 {
		t.Fatalf("expected StartObserveResp status 0 for the -1 titled session, got %x", last)
	}
	if !client.observer {
		t.Error("a served 0x18 StartObserve must mark the client as observer")
	}
}

func TestHandleStartObserveVxFormat(t *testing.T) {
	srv := observerTestServer(t)
	client := &mockMsgClient{}
	testSessionWithPeer(t, srv, client)

	sess := srv.sessionManager.GetSession(session.SessionKey{SessionID: 1})
	sess.RecordFrame(protocol.MsgEndOfGameData, []byte{0xFE, 0x00, 0x00})
	sess.RecordFrame(protocol.MsgEndOfGameData, []byte{0xFE, 0x00, 0x00})

	body := make([]byte, 24)
	binary.LittleEndian.PutUint64(body[:8], 1) // titleID + sessionID 48 bits
	binary.LittleEndian.PutUint64(body[8:16], 7)
	binary.LittleEndian.PutUint64(body[16:24], 0x1122334455667788) // passwordHash -> lagged
	srv.dispatchMessage(client, 0x1C, body)

	last := client.lastSent()
	if len(last) != 4 || last[2] != protocol.MsgStartObserveResp || last[3] != 0 {
		t.Errorf("expected StartObserveResp status 3, got %x", last)
	}
}

func TestHandleStartObserveSessionNotFound(t *testing.T) {
	srv := createTestServer(t)
	client := &mockMsgClient{}
	body := make([]byte, 8)
	binary.LittleEndian.PutUint64(body, 99)
	srv.dispatchMessage(client, protocol.MsgStartObserve, body)
	if len(client.sent) != 0 {
		t.Errorf("expected no reply for missing session, got %x", client.lastSent())
	}
}

func TestHandleStartObserveNoReplay(t *testing.T) {
	srv := observerTestServer(t)
	client := &mockMsgClient{}
	testSessionWithPeer(t, srv, client)

	body := make([]byte, 24)
	binary.LittleEndian.PutUint64(body[:8], 1) // titleID 0, sessionID 1
	srv.dispatchMessage(client, 0x1C, body)

	last := client.lastSent()
	if len(last) != 4 || last[3] != 0 {
		t.Errorf("expected status 0 (no replay), got %x", last)
	}
}

func TestHandleStartObserveRejectedStatus2(t *testing.T) {
	srv := createTestServer(t)
	client := &mockMsgClient{}
	testSessionWithPeer(t, srv, client)

	sess := srv.sessionManager.GetSession(session.SessionKey{SessionID: 1})
	sess.MaxObservers = 0

	body := make([]byte, 24)
	binary.LittleEndian.PutUint64(body[:8], 1) // titleID 0, sessionID 1
	srv.dispatchMessage(client, 0x1C, body)

	if len(client.sent) != 1 {
		t.Fatalf("expected one response, got %d", len(client.sent))
	}
	last := client.lastSent()
	if len(last) != 4 || last[3] != 4 {
		t.Errorf("expected status 4 (observers disabled), got %x", last)
	}
}

// TestHandleStartObserveRolledStoreAnswersThree is the byte that broke _30, and
// it is a different byte from the one it used to send.
//
// The store has to have really dropped its first block for this arm to be the one
// taken, so the session is built with a one-block store of the original's own
// block size and then filled past it. Answering 4 instead would say "observers are
// disabled for this session", which sends whoever goes looking in the wrong
// direction; the byte the original writes is 3.
func TestHandleStartObserveRolledStoreAnswersThree(t *testing.T) {
	// observerTestServer's settings with a frame store of ONE block of 200 bytes,
	// which is what puts the session in the state _30 was in: the store has really
	// dropped its first frame by the time the observer asks.
	srv, err := New(&Config{
		BsPort:           0,
		LogLevel:         "none",
		ObserverMax:      4,
		MaxPagesPerMatch: 1,
		PageSize:         200,
	})
	if err != nil {
		t.Fatalf("New error: %v", err)
	}
	client := &mockMsgClient{}
	testSessionWithPeer(t, srv, client)

	sess := srv.sessionManager.GetSession(session.SessionKey{SessionID: 1})
	// A frame costs its size plus the 57 the original charges it, so two 100-byte
	// frames do not fit in a 200-byte block and the third one recycles it.
	for range 3 {
		sess.RecordFrame(protocol.MsgEndOfGameData, make([]byte, 100))
	}
	if got := sess.ReplayPageCount(); got != 1 {
		t.Fatalf("the store holds %d frames after three into a one-block window, want 1: the rollover is the "+
			"precondition of this test and without it the arm taken is a different one", got)
	}

	body := make([]byte, 24)
	binary.LittleEndian.PutUint64(body[:8], 1)
	srv.dispatchMessage(client, 0x1C, body)

	if len(client.sent) != 1 {
		t.Fatalf("expected one response, got %d: %x", len(client.sent), client.sent)
	}
	last := client.lastSent()
	if len(last) != 4 || last[3] != 3 {
		t.Errorf("a store that has dropped the start of the match must be answered with status 3, got %x", last)
	}
	if sess.HasObserver(client) {
		t.Error("the 3 arm does not register the observer (it returns without touching the list), " +
			"and registering it here is what served the second observer of 2026-09-29_012528 from the middle of the match")
	}
}

func TestHandleActivateObserver(t *testing.T) {
	srv := observerTestServer(t)
	srv.config.SimulationPeriod = 125

	client := &mockMsgClient{}
	testSessionWithPeer(t, srv, client)
	sess := srv.sessionManager.GetSession(session.SessionKey{SessionID: 1})
	sess.RecordFrame(protocol.MsgEndOfGameData, []byte{0xFE, 0x00, 0x00})
	sess.RecordFrame(protocol.MsgEndOfGameData, []byte{0xFE, 0x00, 0x00})

	body := make([]byte, 24)
	binary.LittleEndian.PutUint64(body[:8], 1) // titleID 0, sessionID 1
	srv.dispatchMessage(client, 0x1C, body)

	srv.dispatchMessage(client, protocol.MsgActivateObserver, nil)
	last := client.lastSent()
	if len(last) != 12 || last[2] != protocol.MsgActivateObserverResp || last[3] != 0 {
		t.Errorf("expected ActivateObserverResp, got %x", last)
	}

	// Unknown observer: no reply.
	other := &mockMsgClient{}
	srv.dispatchMessage(other, protocol.MsgActivateObserver, nil)
	if len(other.sent) != 0 {
		t.Error("unknown observer should get no reply")
	}
}

func TestHandleStopObserve(t *testing.T) {
	srv := observerTestServer(t)
	client := &mockMsgClient{}
	testSessionWithPeer(t, srv, client)
	sess := srv.sessionManager.GetSession(session.SessionKey{SessionID: 1})
	sess.RecordFrame(protocol.MsgEndOfGameData, []byte{0xFE, 0x00, 0x00})

	body := make([]byte, 8)
	binary.LittleEndian.PutUint64(body, 1)
	srv.dispatchMessage(client, protocol.MsgStartObserve, body)
	client.observer = true

	srv.dispatchMessage(client, protocol.MsgStopObserve, nil)

	if client.observer {
		t.Error("observer flag should be cleared")
	}
	srv.mu.Lock()
	_, ok := srv.observerSessions[client]
	srv.mu.Unlock()
	if ok {
		t.Error("client should be removed from observerSessions")
	}
	if srv.metricsCollector.GetCounter("RemovePeerRequests") != 1 {
		t.Error("RemovePeerRequests counter should be incremented")
	}
}

// TestHandleLeaveSimulation pins that a LeaveSimulation takes nothing out of the
// session. The handler empties the leaver's two queues, clears its participating
// mark and can end the recording, and never touches the peer vector; the peer goes
// when its connection closes. It used to assert that the session was removed, from
// the message's name.
func TestHandleLeaveSimulation(t *testing.T) {
	srv := createTestServer(t)
	client := &mockMsgClient{}
	testSessionWithPeer(t, srv, client)

	srv.dispatchMessage(client, protocol.MsgLeaveSimulation, nil)

	if got := srv.sessionManager.SessionCount(); got != 1 {
		t.Errorf("session count = %d, want 1: a LeaveSimulation does not end the session", got)
	}
	sess := srv.sessionManager.GetSessions()[0]
	if sess.PeerCount() != 1 {
		t.Errorf("peer count = %d, want 1: the LeaveSimulation handler does not remove the peer", sess.PeerCount())
	}
	if p := sess.GetPeer(1); p == nil || p.Participating {
		t.Error("the peer should still be a member and no longer marked as participating")
	}
}

// A closed connection takes its peer out of the session at once; the session goes
// with its last peer when nobody observes it. This used to pin the opposite (the
// peer stayed a member until its sync timeout), read from old captures in which
// the close reached the server late.
func TestRemoveClientRemovesTheSessionPeer(t *testing.T) {
	srv, sess, c1, c2 := twoPeerServer(t)

	srv.RemoveClient(c1)

	if sess.PeerCount() != 1 {
		t.Errorf("peer count after disconnect = %d, want 1 (the closed connection's peer leaves)", sess.PeerCount())
	}
	if sess.GetPeer(1) != nil {
		t.Error("the disconnected peer must leave the session")
	}
	if srv.sessionManager.SessionCount() != 1 {
		t.Error("session should survive a single player disconnect")
	}

	// Relays to the disconnected peer are dropped: it is no longer a member.
	srv.dispatchMessage(c2, protocol.MsgLobbyRelay, []byte{0x01, 0x02, 0x0f, 0xaa})
	if len(c1.sent) != 0 {
		t.Errorf("relay to a disconnected peer should be dropped, got %d messages", len(c1.sent))
	}

	srv.RemoveClient(c2)

	if srv.sessionManager.SessionCount() != 0 {
		t.Errorf("session should end with its last peer when nobody observes it, count = %d", srv.sessionManager.SessionCount())
	}
}

func TestRouteLobbyRelay(t *testing.T) {
	srv, _, c1, c2 := twoPeerServer(t)

	// The nested peer message carries an unhandled inner type (0x0D), which the
	// server does not take, so the relay is forwarded.
	relay := relayBody(0x02, 0x01, peerMessage(0, []byte{0x0D}))
	srv.dispatchMessage(c1, protocol.MsgLobbyRelay, relay)

	if len(c2.sent) != 1 {
		t.Fatalf("target peer should receive the relay, sent = %d messages", len(c2.sent))
	}
	want := frameMessage(protocol.MsgLobbyRelay, relay)
	if !bytes.Equal(c2.sent[0], want) {
		t.Errorf("relay to target = %x, want %x", c2.sent[0], want)
	}
	if len(c1.sent) != 0 {
		t.Errorf("relay must not be echoed to the sender, got %d messages", len(c1.sent))
	}

	srv.RemoveClient(c2)
	c2.sent = nil
	srv.dispatchMessage(c1, protocol.MsgLobbyRelay, relay)

	if len(c2.sent) != 0 {
		t.Errorf("relay to a disconnected peer should be dropped, got %d messages", len(c2.sent))
	}
}

func TestHandleProxyData(t *testing.T) {
	srv := createTestServer(t)
	client := &mockMsgClient{}
	srv.AddClient(client)
	sess := srv.sessionManager.CreateSession()
	p1 := sess.NewPeer(1, client)
	srv.sessionManager.AddPeer(sess, p1)

	// body = [srcPeerID=1][dstCount=1 u16 LE][dstPeerIDs=[1]][dataLen u16 LE]
	//        [head (0 bytes)][record (15 bytes)]
	body := make([]byte, 0, 7+proxyRecordSize)
	body = append(body, 0x01, 0x01, 0x00, 0x01, proxyRecordSize, 0x00)
	for range proxyRecordSize {
		body = append(body, 0xde)
	}
	srv.dispatchMessage(client, protocol.MsgProxy, body)

	if len(client.sent) != 1 {
		t.Fatalf("expected 1 relayed message, got %d", len(client.sent))
	}
	got := client.sent[0]
	if len(got) < 3 || got[2] != protocol.MsgProxy || !bytes.Equal(got[3:], body) {
		t.Errorf("expected Proxy relay of the body, got %x", got)
	}
}

func TestHandleProxyDataRelaysToDestPeers(t *testing.T) {
	srv, _, c1, c2 := twoPeerServer(t)

	// Source peer 1 sends to destination peers [1, 2]. The payload carries a
	// shared head plus one 15-byte record per destination (capture _25): each peer
	// receives [head + its own record] with the
	// destination list rewritten to just itself.
	head := []byte{0xaa, 0xbb}
	rec1 := bytes.Repeat([]byte{0x11}, proxyRecordSize)
	rec2 := bytes.Repeat([]byte{0x22}, proxyRecordSize)

	body := []byte{0x01, 0x02, 0x00, 0x01, 0x02}
	body = binary.LittleEndian.AppendUint16(body, uint16(len(head)+2*proxyRecordSize))
	body = append(body, head...)
	body = append(body, rec1...)
	body = append(body, rec2...)
	srv.dispatchMessage(c1, protocol.MsgProxy, body)

	frame := func(dst byte, rec []byte) []byte {
		f := []byte{0x01, 0x01, 0x00, dst}
		f = binary.LittleEndian.AppendUint16(f, uint16(len(head)+proxyRecordSize))
		f = append(f, head...)
		return append(f, rec...)
	}

	if len(c1.sent) != 1 {
		t.Errorf("expected 1 relay to c1, got %d", len(c1.sent))
	}
	if len(c2.sent) != 1 {
		t.Errorf("expected 1 relay to c2, got %d", len(c2.sent))
	}
	if len(c1.sent) == 1 && !bytes.Equal(c1.sent[0][3:], frame(1, rec1)) {
		t.Errorf("c1 relay mismatch: %x", c1.sent[0][3:])
	}
	if len(c2.sent) == 1 && !bytes.Equal(c2.sent[0][3:], frame(2, rec2)) {
		t.Errorf("c2 relay mismatch: %x", c2.sent[0][3:])
	}
}

func TestHandleProxyDataDropsDisconnectedPeer(t *testing.T) {
	srv, _, c1, c2 := twoPeerServer(t)

	srv.RemoveClient(c2)

	// Destination peer 2 is no longer live: its relay is dropped while peer 1
	// still receives its own section.
	body := []byte{0x01, 0x02, 0x00, 0x01, 0x02}
	body = binary.LittleEndian.AppendUint16(body, uint16(2*proxyRecordSize))
	body = append(body, bytes.Repeat([]byte{0x33}, 2*proxyRecordSize)...)
	srv.dispatchMessage(c1, protocol.MsgProxy, body)
	if len(c2.sent) != 0 {
		t.Errorf("relay to a disconnected peer should be dropped, got %d messages", len(c2.sent))
	}
	if len(c1.sent) != 1 {
		t.Errorf("expected 1 relay to c1, got %d", len(c1.sent))
	}
}

func TestHandleProxyDataShortBody(t *testing.T) {
	srv := createTestServer(t)
	client := &mockMsgClient{}
	srv.AddClient(client)
	sess := srv.sessionManager.CreateSession()
	p1 := sess.NewPeer(1, client)
	srv.sessionManager.AddPeer(sess, p1)

	srv.dispatchMessage(client, protocol.MsgProxy, []byte{0x01, 0x02})
	srv.dispatchMessage(client, protocol.MsgProxy, []byte{0x01, 0x00, 0x00})
	// Payload shorter than one record per destination is malformed and the
	// whole message is dropped (original "dropping PROXY message").
	short := []byte{0x01, 0x01, 0x00, 0x01, 0x02, 0x00, 0xaa}
	srv.dispatchMessage(client, protocol.MsgProxy, short)
	if len(client.sent) != 0 {
		t.Error("short/malformed proxy body should be ignored")
	}
}

func TestHandleSetObservableParameters(t *testing.T) {
	srv := createTestServer(t)
	client := &mockMsgClient{}
	srv.dispatchMessage(client, protocol.MsgSetObservableParameters, []byte{0x01})
}

func TestHandleGameDataEdges(t *testing.T) {
	srv := createTestServer(t)
	client := &mockMsgClient{}

	srv.handleGameData(client, []byte{0x01, 0x02})
	srv.handleGameData(client, []byte{0x01, 0x02, 0x03})
	if len(client.sent) != 0 {
		t.Error("short/non-0xfe bodies should be ignored")
	}

	// No session yet: the ack must not crash or queue anywhere.
	srv.handleGameData(client, []byte{0xFE, 0x05, 0x00, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE})

	// With a session, the ack is queued on the session and flushed at the
	// next tick. Only the host has joined (1 player, slot 1), so the flush's
	// pending-peer list is empty (capture _24): tail `00 00 00`.
	testSessionWithPeer(t, srv, client)
	sess := srv.sessionManager.GetSession(session.SessionKey{SessionID: 1})
	srv.handleGameData(client, []byte{0xFE, 0x05, 0x00, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE})
	body, _ := sess.PopAckTick()
	want := append([]byte{0xFE, 0x01, 0x01}, []byte{0x05, 0x00, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE}...)
	want = append(want, 0x00, 0x00, 0x00)
	if !bytes.Equal(body, want) {
		t.Errorf("ack flush mismatch: got %x, want %x", body, want)
	}
}

func TestHandleLobbyChecksumAckEdges(t *testing.T) {
	srv := createTestServer(t)
	client := &mockMsgClient{}

	srv.handleLobbyChecksumAck(client, nil, 0)
	srv.handleLobbyChecksumAck(client, []byte{0x02, 0x01}, 0)
	if len(client.sent) != 0 {
		t.Error("malformed checksum bodies should be ignored")
	}

	// Without a session the ack must be dropped silently.
	srv.handleLobbyChecksumAck(client, bytes.Repeat([]byte{0x11}, 17), 0)

	testSessionWithPeer(t, srv, client)
	sess := srv.sessionManager.GetSession(session.SessionKey{SessionID: 1})

	// Entries are [crc u32 LE][page u32 LE]. The session has a single peer, so
	// "every active peer has an entry" is satisfied by this one message.
	entries := make([]byte, 16)
	binary.LittleEndian.PutUint32(entries[4:8], 1)
	binary.LittleEndian.PutUint32(entries[12:16], 2)
	chunk := append([]byte{0x05, 0x00}, bytes.Repeat([]byte{0x22}, 5)...)
	body := make([]byte, 0, 1+16+2+len(chunk))
	body = append(body, 2)
	body = append(body, entries...)
	body = append(body, 0x09, 0xFE)
	body = append(body, chunk...)

	srv.handleLobbyChecksumAck(client, body, 0)

	// Tick 1: the inner chunk flushes as a single sub. With only slot 1 in the
	// session the pending list is empty (capture _24): tail `00`, then an empty
	// entries block `00 00`. The reconciled entries do NOT ride this frame: the
	// original sends the 0x0A and only then reconciles.
	wantFlush := []byte{0xFE, 0x01, 0x01, 0x05, 0x00, 0x22, 0x22, 0x22, 0x22, 0x22, 0x00, 0x00, 0x00}
	if got, _ := sess.PopAckTick(); !bytes.Equal(got, wantFlush) {
		t.Errorf("flush mismatch: got %x, want %x", got, wantFlush)
	}

	// Tick 2: the entries the previous tick's reconciliation merged go out on
	// their own, with nothing left to flush.
	wantEntries := []byte{0xFE, 0x00, 0x01, 0x01, 0x13, 0x00, 0x02, 0x02, 0x00}
	wantEntries = append(wantEntries, entries...)
	if got, _ := sess.PopAckTick(); !bytes.Equal(got, wantEntries) {
		t.Errorf("entries mismatch: got %x, want %x", got, wantEntries)
	}

	// Tick 3: heartbeat, nothing left to merge.
	wantHb := []byte{0xFE, 0x00, 0x01, 0x01, 0x00, 0x00}
	if got, _ := sess.PopAckTick(); !bytes.Equal(got, wantHb) {
		t.Errorf("heartbeat mismatch: got %x, want %x", got, wantHb)
	}

	// A second upload is reconciled after its own tick, so its block is deferred
	// the same way.
	body2 := make([]byte, 9)
	body2[0] = 1
	copy(body2[1:], bytes.Repeat([]byte{0x33}, 8))
	srv.handleLobbyChecksumAck(client, body2, 0)

	if got, _ := sess.PopAckTick(); !bytes.Equal(got, wantHb) {
		t.Errorf("upload tick mismatch: got %x, want %x", got, wantHb)
	}

	wantStandalone2 := []byte{0xFE, 0x00, 0x01, 0x01, 0x0B, 0x00, 0x02, 0x01, 0x00}
	wantStandalone2 = append(wantStandalone2, bytes.Repeat([]byte{0x33}, 8)...)
	if got, _ := sess.PopAckTick(); !bytes.Equal(got, wantStandalone2) {
		t.Errorf("standalone2 mismatch: got %x, want %x", got, wantStandalone2)
	}
}

// LobbyConnect creates/joins the session, keyed by the connect message's
// sessionID; a second LobbyConnect from the same client must keep a single peer
// entry.
//
// It must NOT drive the observable flag. The only two writers in the original are
// the session constructor and the 0x1D handler; the connect handler does not
// mention the field. The reconnect state in the message selects
// the connect handler's reconnect branch instead, which is a different thing.
// Capture _26: with one human player against the AI and one observer, the
// original's 0x0A heartbeat body is fe 00 01 01 00 00 for BOTH the player and the
// observer: one pending peer. Ours was fe 00 02 01 02 00 00, because
// handleLobbyConnect adds a session peer for every connection that connects and
// the observer was never taken back out. The observer then received 0x09 pages
// and 0x22 markers the original never sends, and missed most of the 0x0A stream.
//
// This pins all three: the observer is not a peer, the player's pending list
// stays [1], and the observer gets the same 0x0A body as the player.
func TestObserverIsNotASessionPeer(t *testing.T) {
	srv := observerTestServer(t)
	player := &mockMsgClient{}
	observer := &mockMsgClient{}

	connect := func(c *mockMsgClient, sid uint64) {
		body := make([]byte, 17) // a first connect: reconnect state 0
		binary.LittleEndian.PutUint64(body[:8], sid)
		srv.dispatchMessage(c, protocol.MsgLobbyConnect, body)
	}
	connect(player, 1)
	sess := srv.sessionManager.GetSession(session.SessionKey{SessionID: 1})
	if sess == nil {
		t.Fatal("LobbyConnect should create the session")
	}
	if got := sess.PeerCount(); got != 1 {
		t.Fatalf("PeerCount() = %d, want 1 for the player alone", got)
	}

	// The observer connects, which adds it as a peer like any other client.
	connect(observer, 1)
	if got := sess.PeerCount(); got != 2 {
		t.Fatalf("PeerCount() = %d, want 2 once the observer has connected", got)
	}

	// It then starts observing. The first request is refused (the store has not
	// rolled over yet), the second is accepted, which is the point at which the
	// peer must go away.
	obsReq := make([]byte, 24)
	binary.LittleEndian.PutUint64(obsReq[:8], 1)
	srv.dispatchMessage(observer, 0x1C, obsReq)
	sess.RecordFrame(protocol.MsgEndOfGameData, []byte{0xfe, 0x00, 0x00})
	sess.RecordFrame(protocol.MsgEndOfGameData, []byte{0xfe, 0x00, 0x00})
	srv.dispatchMessage(observer, 0x1C, obsReq)

	last := observer.lastSent()
	if len(last) != 4 || last[2] != protocol.MsgStartObserveResp || last[3] != 0 {
		t.Fatalf("the observer should be accepted, got %x", last)
	}
	if got := sess.PeerCount(); got != 1 {
		t.Errorf("PeerCount() = %d after the client became an observer, want 1: an observer is not a session peer", got)
	}

	// The player's ack must name one pending peer, byte for byte as the capture.
	// The ack state only starts once game data has been queued, and the first
	// tick is a flush, so drain it and take the heartbeat after.
	sess.QueueGameData(1, []byte{0x00, 0x00})
	if first, _ := sess.PopAckTick(); first == nil {
		t.Fatal("the first tick should flush the queued sub")
	}
	body, _ := sess.PopAckTick()
	want := []byte{0xFE, 0x00, 0x01, 0x01, 0x00, 0x00}
	if !bytes.Equal(body, want) {
		t.Errorf("player ack = %x, want %x (one pending peer, as in capture _26)", body, want)
	}
}

// The observer is served the session's own 0x0A stream out of the frame store,
// byte for byte, which is what the original does and what the capture shows: its
// two observer connections received 1346 and 800 messages, every one of them
// identical to a message of the player's 0x0A stream and in the same order.
//
// The observer is not a peer, so the peer walk never reaches it: the frames come
// from the session tick's observer update, not from the ack broadcast. Serving it a
// second copy of the live ack instead, or serving it the 0x09 uploads, are the two
// shapes this test exists to keep out.
func TestObserverReceivesTheSessionAck(t *testing.T) {
	srv := observerTestServer(t)
	player := &mockMsgClient{}
	observer := &mockMsgClient{}

	connect := func(c *mockMsgClient, sid uint64) {
		b := make([]byte, 17) // a first connect: reconnect state 0
		binary.LittleEndian.PutUint64(b[:8], sid)
		srv.dispatchMessage(c, protocol.MsgLobbyConnect, b)
	}
	connect(player, 1)
	sess := srv.sessionManager.GetSession(session.SessionKey{SessionID: 1})
	connect(observer, 1)
	obsReq := make([]byte, 24)
	binary.LittleEndian.PutUint64(obsReq[:8], 1)
	srv.dispatchMessage(observer, 0x1C, obsReq)
	srv.dispatchMessage(observer, protocol.MsgActivateObserver, nil)

	// The ack state starts on the first queued sub and that tick is the flush, so
	// drain it first and let the next tick produce the heartbeat.
	sess.QueueGameData(1, []byte{0x00, 0x00})
	srv.sendEndOfGameDataTicks()
	player.sent = nil
	observer.sent = nil
	srv.sendEndOfGameDataTicks()
	srv.tickSessions()

	var playerAck, observerAck []byte
	var observerPages int
	for _, f := range player.sent {
		if len(f) >= 4 && f[2] == protocol.MsgEndOfGameData {
			playerAck = f[3:] // the mock frames as [u16 len][type][body]
		}
	}
	for _, f := range observer.sent {
		if len(f) >= 4 && f[2] == protocol.MsgEndOfGameData {
			observerAck = f[3:]
		}
		if len(f) >= 3 && f[2] == protocol.MsgGameData {
			observerPages++
		}
	}
	if playerAck == nil {
		t.Fatal("the player received no 0x0A")
	}
	if observerAck == nil {
		t.Fatalf("the observer received no 0x0A at all: %x", observer.sent)
	}
	if !bytes.Equal(observerAck, playerAck) {
		t.Errorf("observer ack = %x, want the player's %x", observerAck, playerAck)
	}
	if !bytes.Equal(playerAck, []byte{0xFE, 0x00, 0x01, 0x01, 0x00, 0x00}) {
		t.Errorf("ack = %x, want the capture _26 heartbeat fe0001010000", playerAck)
	}
	if observerPages != 0 {
		t.Errorf("the observer received %d 0x09 pages: the store holds the 0x0A acks, not the uploads", observerPages)
	}
}

// The reconnect leaves the replaced connection dead, and the original says so
// twice: the socket swap hands the NEW socket to the peer and leaves the connection
// object holding the old one, and then the connection object is scheduled to close
// at once, with a zero-delay deadline that the socket loop evaluates on its next
// pass. Before this the dead connection kept reading and kept counting as a live
// client, and only the peer pointer had moved.
func TestTheReconnectClosesTheReplacedConnection(t *testing.T) {
	srv := createTestServer(t)
	first := &mockMsgClient{}

	const secret = 0x5A5A_0F0F_1234_5678
	connect := func(c *mockMsgClient, state byte, dataReceived uint32) {
		n := 23
		if state == 2 {
			n = 27
		}
		b := make([]byte, n)
		binary.LittleEndian.PutUint64(b[:8], 21)
		binary.LittleEndian.PutUint64(b[8:16], secret)
		b[16] = state
		if state == 2 {
			binary.LittleEndian.PutUint32(b[17:21], dataReceived)
		}
		srv.dispatchMessage(c, protocol.MsgLobbyConnect, b)
	}

	connect(first, 0, 0)
	second := &mockMsgClient{}
	connect(second, 2, 0)

	if !first.closed {
		t.Error("the replaced connection must be closed by the reconnect")
	}
	if second.closed {
		t.Error("the connection that carries the live socket must stay open")
	}
	// The slot is unchanged and the peer's connection is the live one, so the
	// session still has a player rather than a ghost.
	sess := srv.sessionManager.GetSession(session.SessionKey{SessionID: 21})
	if got := sess.PeerCount(); got != 1 {
		t.Fatalf("PeerCount() = %d, want 1", got)
	}
	if got := sess.PeerIDForConn(second); got != 1 {
		t.Errorf("the live connection resolves to slot %d, want 1", got)
	}
}

// Only reconnect state 2 resends frames, and that is the arm the original has:
// the connect's own state field decides between delivering the connect and going
// on to rewrite it and resend the frames from the declared point.
//
// The states above 2 reach the second arm with a dataReceivedCount of -1,
// because the parser only reads that field when the state IS 2, and a range
// starting below zero is refused by the range check as an invalid one: no reply,
// and the connection is closed. Before this a state 1 connect resent the whole
// retained window from frame 0, which is neither of the two arms.
//
// State 2 answers with the connect FIRST and then resends. This used to pin the
// frames ahead of the reply and an echo for every state.
func TestOnlyReconnectStateTwoResendsFrames(t *testing.T) {
	for _, tc := range []struct {
		name          string
		state         byte
		wantFrames    int
		wantLastFrame byte
		wantClosed    bool
	}{
		{name: "state 1 resends nothing", state: 1, wantFrames: 0},
		{name: "state 3 carries no count, so it is closed", state: 3, wantFrames: 0, wantClosed: true},
		{name: "state 2 resends from the declared point", state: 2, wantFrames: 1, wantLastFrame: 0x03},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := createTestServer(t)
			first := &mockMsgClient{}

			const secret = 0x1234_5678_9ABC_DEF0
			connect := func(c *mockMsgClient, state byte, dataReceived uint32) {
				n := 23
				if state == 2 {
					n = 27
				}
				b := make([]byte, n)
				binary.LittleEndian.PutUint64(b[:8], 31)
				binary.LittleEndian.PutUint64(b[8:16], secret)
				b[16] = state
				if state == 2 {
					binary.LittleEndian.PutUint32(b[17:21], dataReceived)
				}
				srv.dispatchMessage(c, protocol.MsgLobbyConnect, b)
			}

			connect(first, 0, 0)
			sess := srv.sessionManager.GetSession(session.SessionKey{SessionID: 31})
			for i := 1; i <= 3; i++ {
				sess.RecordFrame(protocol.MsgEndOfGameData, []byte{0xFE, byte(i)})
			}

			second := &mockMsgClient{}
			connect(second, tc.state, 2)

			var frames [][]byte
			for _, f := range second.sent {
				if len(f) >= 3 && f[2] == protocol.MsgEndOfGameData {
					frames = append(frames, f)
				}
			}
			if len(frames) != tc.wantFrames {
				t.Fatalf("state %d resent %d frames, want %d", tc.state, len(frames), tc.wantFrames)
			}
			if tc.wantFrames > 0 && !bytes.Equal(frames[0][3:], []byte{0xFE, tc.wantLastFrame}) {
				t.Errorf("resent frame = %x, want the last one (%02x)", frames[0][3:], tc.wantLastFrame)
			}
			if second.closed != tc.wantClosed {
				t.Errorf("state %d left the connection closed=%v, want %v", tc.state, second.closed, tc.wantClosed)
			}
			// A served reconnect answers with the connect before anything else; a
			// refused one gets nothing at all.
			if tc.wantClosed {
				if len(second.sent) != 0 {
					t.Errorf("state %d was answered with %d messages, want none", tc.state, len(second.sent))
				}
			} else if len(second.sent) == 0 || second.sent[0][2] != protocol.MsgLobbyConnect {
				t.Error("the connect must be answered first")
			}
		})
	}
}

// A zero secret must never match a peer. The original compares the message's
// secret against the peer's own field, so a zero secret WOULD match a peer that
// published zero; this server refuses to look it up, so two
// clients that both connect without a secret cannot be mistaken for one another.
// A zero-secret reconnect is therefore one that finds no peer, and it is closed
// with no reply like any other.
// No capture contains a zero secret: every connect in the five committed ones
// carries a non-zero one, so there is no evidence either way about the original's
// behaviour here and this stays a deliberate difference.
func TestAZeroSecretNeverMatchesAPeer(t *testing.T) {
	srv := createTestServer(t)

	first := &mockMsgClient{}
	b1 := make([]byte, 23)
	binary.LittleEndian.PutUint64(b1[:8], 41)
	srv.dispatchMessage(first, protocol.MsgLobbyConnect, b1)

	second := &mockMsgClient{}
	b2 := make([]byte, 27)
	binary.LittleEndian.PutUint64(b2[:8], 41)
	b2[16] = 2
	srv.dispatchMessage(second, protocol.MsgLobbyConnect, b2)

	sess := srv.sessionManager.GetSession(session.SessionKey{SessionID: 41})
	if got := sess.PeerIDForConn(first); got != 1 || first.closed {
		t.Errorf("the first peer lost its slot (now %d) or its connection: a zero secret must not match it", got)
	}
	if got := sess.PeerCount(); got != 1 {
		t.Errorf("PeerCount() = %d, want 1: a reconnect that matches nobody takes no slot", got)
	}
	if len(second.sent) != 0 || !second.closed {
		t.Errorf("the zero-secret reconnect got %d replies and closed=%v, want none and closed", len(second.sent), second.closed)
	}
}

// The lagged observer path end to end, over the wire: the game publishes a
// credential and a lag with 0x1D, the observer asks to watch without presenting
// that credential, and its stream stops LagPages short of the live end.
//
// The flag is the hash the request presents against the one the game published,
// and the target is where the observer update stops serving. The 0x1E answer is
// checked too, because the two are the same fact seen twice: the lag decides both
// how far behind the stream stops and which of the two start-time formulas
// applies.
func TestTheLaggedObserverPathOverTheWire(t *testing.T) {
	srv := observerTestServer(t)
	const (
		hash        = 0x21616C6661AA7A42 // what the players publish
		lagSeconds  = 5
		ticksPerSec = 8
		lagPages    = lagSeconds * ticksPerSec
		matchFrames = lagPages + 20
	)

	player := &mockMsgClient{}
	pc := make([]byte, 17) // a first connect: reconnect state 0
	binary.LittleEndian.PutUint64(pc[:8], 1)
	srv.dispatchMessage(player, protocol.MsgLobbyConnect, pc)
	sess := srv.sessionManager.GetSession(session.SessionKey{SessionID: 1})

	// 0x1D: observable on, credential published, lag of 5 s.
	sd := make([]byte, 16)
	binary.LittleEndian.PutUint32(sd[0:4], 1)
	binary.LittleEndian.PutUint64(sd[4:12], hash)
	binary.LittleEndian.PutUint32(sd[12:16], lagSeconds)
	srv.dispatchMessage(player, protocol.MsgSetObservableParameters, sd)

	// observe builds the 24-byte Vx request the game sends: the packed key, the
	// peerIdentifier and the passwordHash. The identifier is body offset 8 (wire
	// offset 9) and it is what the observer list is keyed on, so the two observers
	// here have to carry different ones.
	observe := func(c *mockMsgClient, peerIdentifier, passwordHash uint64) {
		req := make([]byte, 24)
		binary.LittleEndian.PutUint64(req[:8], 1)
		binary.LittleEndian.PutUint64(req[8:16], peerIdentifier)
		binary.LittleEndian.PutUint64(req[16:24], passwordHash)
		srv.dispatchMessage(c, 0x1C, req)
		srv.dispatchMessage(c, protocol.MsgActivateObserver, nil)
	}

	// The observer presents nothing, which is what every observer in the five
	// committed captures does.
	lagged := &mockMsgClient{}
	observe(lagged, 0x1122334455667788, 0)
	// A second observer presents the credential and is therefore not lagged.
	plain := &mockMsgClient{}
	observe(plain, 0x99AABBCCDDEEFF00, hash)

	for range matchFrames {
		sess.RecordFrame(protocol.MsgEndOfGameData, []byte{0xFE, 0x01})
	}
	lagged.sent, plain.sent = nil, nil
	// The observer update walks the list with a stride of one whole entry, so two
	// passes are what it takes to reach the second entry.
	srv.tickSessions()
	srv.tickSessions()

	count := func(c *mockMsgClient) int {
		n := 0
		for _, f := range c.sent {
			if len(f) >= 3 && f[2] == protocol.MsgEndOfGameData {
				n++
			}
		}
		return n
	}
	if got, want := count(plain), matchFrames; got != want {
		t.Errorf("the observer with the credential got %d frames, want %d", got, want)
	}
	if got, want := count(lagged), matchFrames-lagPages; got != want {
		t.Errorf("the observer without the credential got %d frames, want %d (it must stop LagPages short of live)", got, want)
	}
}

// A reconnecting client must land on its existing slot, not take a second one.
// The original finds the peer by the connect's secret and swaps the socket; before
// this a reconnect got a brand new
// slot, the old slot lingered on a dead connection until the sync timeout reaped
// it, and the 0x0A pending list named two slots for one player.
func TestHandleLobbyConnectReconnectKeepsTheSlot(t *testing.T) {
	srv := createTestServer(t)
	first := &mockMsgClient{}

	const secret = 0xA5A5_1234_5678_9ABC
	connect := func(state byte, dataReceived uint32) []byte {
		n := 23 // ParseLobbyConnect wants the exact length: 17 + 6 for state 0
		if state == 2 {
			n = 27 // 21 + 6 once the dataReceivedCount is present
		}
		b := make([]byte, n)
		binary.LittleEndian.PutUint64(b[:8], 7) // sessionID 48b, titleID 0
		binary.LittleEndian.PutUint64(b[8:16], secret)
		b[16] = state
		if state == 2 {
			// State 2 puts the dataReceivedCount before the version fields.
			binary.LittleEndian.PutUint32(b[17:21], dataReceived)
		}
		return b
	}

	srv.dispatchMessage(first, protocol.MsgLobbyConnect, connect(0, 0))
	sess := srv.sessionManager.GetSession(session.SessionKey{SessionID: 7})
	if sess == nil {
		t.Fatal("LobbyConnect should create the session")
	}
	if got := sess.PeerCount(); got != 1 {
		t.Fatalf("PeerCount() = %d, want 1 after the first connect", got)
	}
	slot := sess.PeerIDForConn(first)
	if slot == 0 {
		t.Fatal("the first connection should hold slot 1")
	}

	// The client's connection drops and it comes back on a new socket with the
	// same secret and a non-zero reconnect state.
	second := &mockMsgClient{}
	srv.dispatchMessage(second, protocol.MsgLobbyConnect, connect(2, 0))

	if got := sess.PeerCount(); got != 1 {
		t.Errorf("PeerCount() = %d after a reconnect, want 1: the peer must not take a second slot", got)
	}
	if got := sess.PeerIDForConn(second); got != slot {
		t.Errorf("the new connection resolved to slot %d, want the original slot %d", got, slot)
	}
	if got := sess.PeerIDForConn(first); got != 0 {
		t.Errorf("the replaced connection still resolves to slot %d, want 0", got)
	}
	// The relay route moves with the peer too, not only the session's own index:
	// it is what finds the session for everything the new connection sends.
	if got := srv.sessionManager.FindSessionByPeer(second); got != sess {
		t.Errorf("the new connection's route leads to session %p, want %p", got, sess)
	}
	if got := srv.sessionManager.FindSessionByPeer(first); got != nil {
		t.Errorf("the replaced connection still has a route, to session %p", got)
	}
	// The connect is still echoed on the reconnect branch.
	if len(second.sent) == 0 || second.sent[0][2] != protocol.MsgLobbyConnect {
		t.Error("a reconnect must still be echoed")
	}
}

// The reconnect resend replays the stored frames from the point the client
// declares it already has. Mode 2 carries that point in the connect's
// dataReceivedCount field.
func TestHandleLobbyConnectReconnectResendsFrames(t *testing.T) {
	srv := createTestServer(t)
	first := &mockMsgClient{}

	const secret = 0x0102_0304_0506_0708
	connect := func(state byte, dataReceived uint32) []byte {
		n := 23 // ParseLobbyConnect wants the exact length: 17 + 6 for state 0
		if state == 2 {
			n = 27 // 21 + 6 once the dataReceivedCount is present
		}
		b := make([]byte, n)
		binary.LittleEndian.PutUint64(b[:8], 9)
		binary.LittleEndian.PutUint64(b[8:16], secret)
		b[16] = state
		if state == 2 {
			binary.LittleEndian.PutUint32(b[17:21], dataReceived)
		}
		return b
	}

	srv.dispatchMessage(first, protocol.MsgLobbyConnect, connect(0, 0))
	sess := srv.sessionManager.GetSession(session.SessionKey{SessionID: 9})
	for i := 1; i <= 3; i++ {
		sess.RecordFrame(protocol.MsgEndOfGameData, []byte{0xFE, byte(i)})
	}

	second := &mockMsgClient{}
	srv.dispatchMessage(second, protocol.MsgLobbyConnect, connect(2, 2))

	var frames [][]byte
	for _, f := range second.sent {
		if len(f) >= 3 && f[2] == protocol.MsgEndOfGameData {
			frames = append(frames, f)
		}
	}
	// PageCount is 3, so last = 2; the client says it already has up to index 1,
	// and the original's range is [dataReceivedCount, lastFrame-1]. The frames
	// come back with the type they were stored under, and the store holds the
	// session's 0x0A acks. The mock frames as [u16 len][type][body], so the frame
	// body starts at index 3.
	if len(frames) != 1 || !bytes.Equal(frames[0][3:], []byte{0xFE, 0x03}) {
		t.Fatalf("reconnect resent %d frames, want just frame 2: %x", len(frames), frames)
	}
}

// A reconnect naming a secret no peer holds is refused: it neither matches an
// arbitrary peer nor falls through to a fresh slot. The connection is closed
// with no reply.
func TestHandleLobbyConnectReconnectUnknownSecretIsClosed(t *testing.T) {
	srv := createTestServer(t)
	first := &mockMsgClient{}
	connect := func(secret uint64, state byte) []byte {
		n := 23 // ParseLobbyConnect wants the exact length: 17 + 6 for state 0
		if state == 2 {
			n = 27 // 21 + 6 once the dataReceivedCount is present
		}
		b := make([]byte, n)
		binary.LittleEndian.PutUint64(b[:8], 11)
		binary.LittleEndian.PutUint64(b[8:16], secret)
		b[16] = state
		return b
	}

	srv.dispatchMessage(first, protocol.MsgLobbyConnect, connect(0xDEAD, 0))
	sess := srv.sessionManager.GetSession(session.SessionKey{SessionID: 11})
	if got := sess.PeerCount(); got != 1 {
		t.Fatalf("PeerCount() = %d, want 1", got)
	}

	other := &mockMsgClient{}
	srv.dispatchMessage(other, protocol.MsgLobbyConnect, connect(0xBEEF, 2))
	if got := sess.PeerCount(); got != 1 {
		t.Errorf("PeerCount() = %d, want 1: an unknown secret takes no slot", got)
	}
	if len(other.sent) != 0 || !other.closed {
		t.Errorf("the unknown-secret reconnect got %d replies and closed=%v, want none and closed", len(other.sent), other.closed)
	}
}

func TestHandleLobbyConnectDoesNotSetObservable(t *testing.T) {
	srv := createTestServer(t)
	client := &mockMsgClient{}

	body := make([]byte, 17)                                // a first connect: reconnect state 0
	binary.LittleEndian.PutUint64(body[:8], 0x112233445566) // sessionID
	srv.dispatchMessage(client, protocol.MsgLobbyConnect, body)

	sess := srv.sessionManager.GetSession(session.SessionKey{SessionID: 0x112233445566})
	if sess == nil {
		t.Fatal("LobbyConnect should create the session")
	}
	if sess.PeerIDForConn(client) != 1 {
		t.Errorf("client peer ID = %d, want 1", sess.PeerIDForConn(client))
	}
	if len(client.sent) == 0 || client.sent[0][2] != protocol.MsgLobbyConnect {
		t.Error("LobbyConnect should be echoed")
	}

	// A reconnectState of 0 leaves the flag alone; only 0x1D moves it.
	srv.dispatchMessage(client, protocol.MsgLobbyConnect, func() []byte {
		b := make([]byte, 21)
		binary.LittleEndian.PutUint64(b[:8], 0x112233445566)
		b[16] = 0
		return b
	}())
	if !sess.Reconnectable {
		t.Error("LobbyConnect must not clear the observable flag")
	}

	srv.dispatchMessage(client, protocol.MsgLobbyConnect, body)
	if got := sess.PeerCount(); got != 1 {
		t.Errorf("PeerCount() = %d, want 1 after a repeated connect", got)
	}
	if !sess.Reconnectable {
		t.Error("LobbyConnect must not set the observable flag")
	}
}

// The clientMessageVersion the peer announces at connect is the field both of the
// original's version gates read, so it has to reach them. It must be recorded on
// the peer, not parsed and dropped.
func TestHandleLobbyConnectRecordsClientMessageVersion(t *testing.T) {
	for _, version := range []uint16{0, 3, 4, 5} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			srv := createTestServer(t)
			client := &mockMsgClient{}

			// [sessionID|titleID 48b][secret][reconnectState], then the version
			// fields. The parse requires exact consumption: below version 5 the
			// body ends after the timeout override (17+6), and from 5 it carries
			// the profile ID (17+14).
			body := make([]byte, 23)
			binary.LittleEndian.PutUint64(body[:8], 0x112233445566)
			body[16] = 0 // reconnectState
			binary.LittleEndian.PutUint16(body[17:19], version)
			if version >= 5 {
				body = append(body, make([]byte, 8)...) // profileID
			}
			if !okConnectBody(body) {
				t.Fatalf("test body of %d bytes does not parse for version %d", len(body), version)
			}
			srv.dispatchMessage(client, protocol.MsgLobbyConnect, body)

			sess := srv.sessionManager.GetSession(session.SessionKey{SessionID: 0x112233445566})
			if sess == nil {
				t.Fatalf("LobbyConnect should create the session (body %d bytes, version %d)", len(body), version)
			}
			peerID := sess.PeerIDForConn(client)
			if peerID == 0 {
				t.Fatal("LobbyConnect should register the peer")
			}
			got, ok := sess.ClientMessageVersionOf(peerID)
			if !ok {
				t.Fatalf("peer %d not found", peerID)
			}
			if got != version {
				t.Errorf("peer.ClientMessageVersion = %d, want %d", got, version)
			}
		})
	}
}

func okConnectBody(body []byte) bool {
	_, ok := protocol.ParseLobbyConnect(body)
	return ok
}

// TestHandleLobbyConnectMalformedNoEcho covers the parse guard: the original
// returns without echoing when LobbyConnect parsing fails, so a body that is not
// exactly a LobbyConnect must produce no session, no peer and no reply.
func TestHandleLobbyConnectMalformedNoEcho(t *testing.T) {
	srv := createTestServer(t)

	// Too short for the fixed part (needs 17 bytes).
	short := &mockMsgClient{}
	srv.dispatchMessage(short, protocol.MsgLobbyConnect, make([]byte, 16))
	if len(short.sent) != 0 {
		t.Errorf("malformed LobbyConnect was echoed: %x", short.sent)
	}

	// Version 5 body with one extra byte: ParseLobbyConnect requires exact
	// consumption, so this is not a connect message.
	extra := &mockMsgClient{}
	body := make([]byte, 32) // 31-byte version-5 body plus a trailing byte
	binary.LittleEndian.PutUint64(body[:8], 0x112233445566)
	binary.LittleEndian.PutUint16(body[17:19], 5)
	srv.dispatchMessage(extra, protocol.MsgLobbyConnect, body)
	if len(extra.sent) != 0 {
		t.Errorf("oversized LobbyConnect was echoed: %x", extra.sent)
	}

	if n := srv.sessionManager.SessionCount(); n != 0 {
		t.Errorf("malformed LobbyConnect created %d session(s), want 0", n)
	}
}

// TestHandleLobbyConnectSetsObserverConfig guards the observer-limits fix: a
// session created by LobbyConnect (the real game flow) must accept observers,
// so StartObserve must see MaxObservers > 0 instead of the zero default that
// would reject every subscribe with status 2.
func TestHandleLobbyConnectSetsObserverConfig(t *testing.T) {
	srv := createTestServer(t)
	srv.config.ObserverMax = 4
	client := &mockMsgClient{}

	body := make([]byte, 23)
	binary.LittleEndian.PutUint64(body[:8], 0x776655443322) // sessionID
	body[16] = 0                                            // reconnectState: a first connect
	binary.LittleEndian.PutUint16(body[17:19], 0)           // clientMessageVersion < 5
	binary.LittleEndian.PutUint32(body[19:23], 0)           // timeoutOverride
	srv.dispatchMessage(client, protocol.MsgLobbyConnect, body)

	sess := srv.sessionManager.GetSession(session.SessionKey{SessionID: 0x776655443322})
	if sess == nil {
		t.Fatal("LobbyConnect should create the session")
	}
	if sess.MaxObservers != int(srv.config.ObserverMax) {
		t.Errorf("MaxObservers = %d, want %d", sess.MaxObservers, srv.config.ObserverMax)
	}
	if sess.MaxObservers <= 0 {
		t.Fatal("session created by LobbyConnect must accept observers")
	}
}

func TestMsToTicks(t *testing.T) {
	if got := msToTicks(250, 125); got != 2 {
		t.Errorf("msToTicks(250, 125) = %d, want 2", got)
	}
	if got := msToTicks(1000, 0); got != 8 {
		t.Errorf("msToTicks(1000, 0) = %d, want 8 (default 125ms)", got)
	}
	if got := msToTicks(8, 125); got != 0 {
		t.Errorf("msToTicks(8, 125) = %d, want 0", got)
	}
}

func TestObserverSocketSkipTicks(t *testing.T) {
	srv := createTestServer(t)
	srv.config = &Config{ObserverSocketSkipMS: 250, SimulationPeriod: 125}
	if got := srv.observerSocketSkipTicks(); got != 2 {
		t.Errorf("observerSocketSkipTicks = %d, want 2", got)
	}

	srv.config = &Config{ObserverSocketSkipMS: 8, SimulationPeriod: 125}
	if got := srv.observerSocketSkipTicks(); got != 0 {
		t.Errorf("observerSocketSkipTicks = %d, want 0", got)
	}

	srv.config = nil
	if got := srv.observerSocketSkipTicks(); got != 0 {
		t.Errorf("observerSocketSkipTicks with nil config = %d, want 0", got)
	}
}

func TestInitMuninAndWrite(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "munin.log")

	cfg := &Config{
		BsPort:           0,
		EnableStats:      true,
		MuninLog:         logPath,
		MuninReportDelay: 60000,
		LogLevel:         "none",
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New error: %v", err)
	}
	defer func() { _ = srv.Stop() }()

	if srv.muninFile == nil || srv.muninWriter == nil || srv.muninTicker == nil {
		t.Fatal("initMunin should set file, writer and ticker")
	}

	srv.metricsCollector.AddToCounter("TcpConnections", 3)
	srv.metricsCollector.AddToCounter("BytesReceived", 100)
	srv.metricsCollector.SetGauge("Clients", 7)
	srv.writeMuninMetrics()

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read munin log: %v", err)
	}
	out := string(data)
	for _, want := range []string{
		"multigraph bsmunin_connections",
		"tcp_connections.value 3",
		"ws_connections.value 0",
		"ping_connections.value 0",
		"multigraph bsmunin_errors",
		"multigraph bsmunin_traffic",
		"bytes_received.value 100",
		"multigraph bsmunin_performance",
		"clients.value 7",
		"server_cpu_usage.value 0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("munin output missing %q", want)
		}
	}
}

func TestMuninInactiveWithoutEnableStats(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "munin.log")

	cfg := &Config{
		BsPort:           0,
		MuninLog:         logPath,
		MuninReportDelay: 60000,
		LogLevel:         "none",
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New error: %v", err)
	}
	defer func() { _ = srv.Stop() }()

	if srv.muninFile != nil || srv.muninWriter != nil || srv.muninTicker != nil {
		t.Fatal("munin must not activate with only -muninLog; -enableStats is required")
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Errorf("munin log file must not be created without -enableStats (err=%v)", err)
	}
}

func TestInitMuninClusterPrefix(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "munin.log")

	cfg := &Config{
		BsPort:           0,
		EnableStats:      true,
		MuninLog:         logPath,
		MuninReportDelay: 60000,
		ClusterName:      "eu",
		LogLevel:         "none",
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New error: %v", err)
	}
	defer func() { _ = srv.Stop() }()

	srv.writeMuninMetrics()
	data, _ := os.ReadFile(logPath)
	if !bytes.Contains(data, []byte("multigraph bsmunin_eu_connections")) {
		t.Error("expected cluster-prefixed munin output")
	}
}

func TestInitMuninError(t *testing.T) {
	cfg := &Config{
		BsPort:      0,
		EnableStats: true,
		MuninLog:    filepath.Join(os.TempDir(), "nonexistent-dir", "m.log"),
	}
	srv, err := New(cfg)
	if err == nil {
		t.Fatal("expected error for invalid munin log path")
	}
	if srv != nil {
		t.Error("server should be nil on munin init error")
	}
}

func TestMuninLoop(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		BsPort:              0,
		WebSocketPort:       0,
		OutOfBandPort:       0,
		RelayBroadcastPorts: nil,
		EnableStats:         true,
		MuninLog:            dir + "\\munin.log",
		MuninReportDelay:    20,
		LogLevel:            "none",
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start error: %v", err)
	}

	// Let the 20ms ticker fire at least once, then cancel to stop the loop.
	time.Sleep(80 * time.Millisecond)
	cancel()

	deadline := time.Now().Add(2 * time.Second)
	for {
		srv.mu.Lock()
		running := srv.running.Load()
		srv.mu.Unlock()
		if !running || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	_ = srv.Stop()
	data, err := os.ReadFile(cfg.MuninLog)
	if err != nil {
		t.Fatalf("read munin log: %v", err)
	}
	if !strings.Contains(string(data), "bsmunin_connections") {
		t.Errorf("munin log missing connections graph: %q", data)
	}
}

func TestRunCancels(t *testing.T) {
	cfg := &Config{
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

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- srv.Run(ctx)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestCollectMetrics(t *testing.T) {
	srv := createTestServer(t)
	srv.collectMetrics()
	if srv.metricsCollector.GetGauge("Clients") != 0 {
		t.Error("Clients gauge should be 0")
	}
	if srv.metricsCollector.GetGauge("ServerCpuUsage") != 0 {
		t.Error("ServerCpuUsage gauge should be 0")
	}
}

func TestProcessClients(t *testing.T) {
	srv := createTestServer(t)
	NewClient(&mockNetConn{}, srv)
	srv.processClients()
	if len(srv.clients) != 1 {
		t.Errorf("clients = %d, want 1", len(srv.clients))
	}
}

func TestTickSessions(t *testing.T) {
	srv := createTestServer(t)
	testSessionWithPeer(t, srv, &mockMsgClient{})
	srv.tickSessions()
}

func TestSendEndOfGameDataTicks(t *testing.T) {
	srv := createTestServer(t)
	client := &mockMsgClient{}
	testSessionWithPeer(t, srv, client)
	srv.AddClient(client)

	client.setPingStarted()
	client.lastPing = time.Time{} // force ping

	// Heartbeats only start once game data has begun flowing (capture _4:
	// the original emits no 0x0A at all without game data).
	srv.handleGameData(client, []byte{0xFE, 0x00, 0x00})
	srv.sendEndOfGameDataTicks()

	if len(client.sent) == 0 || client.sent[0][2] != protocol.MsgEndOfGameData {
		t.Errorf("expected heartbeat ack, got %x", client.sent)
	}
	// The original does not send periodic self-pings; it only echoes
	// incoming pings (see handlePing). The lobbyPingTick was removed.
}

func TestStopWithoutStart(t *testing.T) {
	srv := createTestServer(t)
	if err := srv.Stop(); err != nil {
		t.Errorf("Stop error: %v", err)
	}
	if err := srv.Stop(); err != nil {
		t.Errorf("double Stop error: %v", err)
	}
}

// peerEnvelope builds a 0x0F body (the part after the type byte): the 15-byte
// PEERHEADER followed by an inner message.
func peerEnvelope(inner ...byte) []byte {
	body := make([]byte, 0, protocol.PeerHeaderLen+len(inner))
	body = append(body, protocol.PeerHeader{PacketNumber: 42}.Serialize()...)
	return append(body, inner...)
}

func innerBlob(s string) []byte {
	b := make([]byte, 2+len(s))
	binary.LittleEndian.PutUint16(b, uint16(len(s)))
	copy(b[2:], s)
	return b
}

func TestHandlePeerAddPeer(t *testing.T) {
	srv, sess, c1, _ := twoPeerServer(t)

	inner := []byte{protocol.PeerMsgAddPeer}
	inner = append(inner, innerBlob("192.168.1.50:27012")...)
	inner = append(inner, innerBlob("")...)
	inner = append(inner, 0x02, 0x01, 0x00) // peerID 2, isHost, !isLocal
	inner = binary.LittleEndian.AppendUint64(inner, 0xAABB)

	srv.handlePeer(c1, peerEnvelope(inner...))
	if sess.HostID() != 2 {
		t.Errorf("HostID = %d, want 2 (AddPeer flagged peer 2 as host)", sess.HostID())
	}
}

// Inner types 4 and 8 have no label in the original's inner switch, so both are
// unhandled there and neither has a handler here. The "Forward DROPOUT message
// to host only" block is unreachable dead code (it sits inside the switch, whose
// labels exclude 8, and its own guard requires the inner type to be 8), so the
// original never does a host-only forward. The delivery effect is covered by
// TestProxyRelayUnhandledInnerTypeReachesEveryone.
func TestHandlePeerUnhandledInnerTypesHaveNoEffect(t *testing.T) {
	for _, typ := range []byte{0x04, protocol.PeerMsgDropoutPeer} {
		t.Run(protocol.MessageTypeName(typ), func(t *testing.T) {
			srv, sess, c1, c2 := twoPeerServer(t)
			sess.SetHostID(2)

			inner := []byte{typ}
			inner = binary.LittleEndian.AppendUint32(inner, 7)
			inner = append(inner, innerBlob("timeout")...)
			inner = append(inner, 0x02)

			c1.sent = nil
			c2.sent = nil
			srv.handlePeer(c1, peerEnvelope(inner...))

			// An unhandled inner type changes no session state and sends
			// nothing on its own; the relay is the caller's job.
			if len(c1.sent) != 0 || len(c2.sent) != 0 {
				t.Errorf("inner type 0x%02x is unhandled and must not send anything, got %d/%d frames", typ, len(c1.sent), len(c2.sent))
			}
			if sess.HostID() != 2 {
				t.Errorf("HostID = %d, want 2 (unchanged)", sess.HostID())
			}
			if _, _, ok := sess.Peers[2].CloseDeadline(); ok {
				t.Error("an unhandled inner type must not schedule a close")
			}
		})
	}
}

func TestHandlePeerRemovePeerSchedulesClose(t *testing.T) {
	srv, sess, c1, _ := twoPeerServer(t)

	inner := []byte{protocol.PeerMsgRemovePeer}
	inner = binary.LittleEndian.AppendUint32(inner, 0x1234)
	inner = append(inner, innerBlob("left")...)
	inner = append(inner, 0x02) // peer 2 is the one being removed

	srv.handlePeer(c1, peerEnvelope(inner...))
	if _, _, ok := sess.Peers[2].CloseDeadline(); !ok {
		t.Error("RemovePeer should have scheduled the target peer to close")
	}
	if _, _, ok := sess.Peers[1].CloseDeadline(); ok {
		t.Error("RemovePeer must not schedule the peer that sent it")
	}
}

// The IntegrityCheck with migrateState 0 is the host migration: the server takes
// the proposed host.
func TestHandlePeerIntegrityCheckMigratesHost(t *testing.T) {
	srv, sess, c1, _ := twoPeerServer(t)

	inner := []byte{protocol.PeerMsgIntegrityCheck}
	inner = append(inner, innerBlob("peers")...)
	inner = append(inner, 0x02, 0x00) // proposedHostID 2, migrateState 0

	srv.handlePeer(c1, peerEnvelope(inner...))
	if sess.HostID() != 2 {
		t.Errorf("HostID = %d, want 2 after the migration", sess.HostID())
	}

	// migrateState 1 is not a migration: nothing moves.
	inner[len(inner)-1] = 1
	srv.handlePeer(c1, peerEnvelope(inner...))
	if sess.HostID() != 2 {
		t.Errorf("HostID = %d, want it unchanged at 2", sess.HostID())
	}
}

// The peer-relayed GameData feeds the same per-player queue as the top-level
// 0x09.
func TestHandlePeerGameDataQueuesAck(t *testing.T) {
	srv, sess, c1, _ := twoPeerServer(t)

	// Inner type 9, then the GameData sub-type 0xFE with an empty chunk.
	inner := []byte{protocol.PeerMsgGameData, 0xFE, 0x00, 0x00}
	srv.handlePeer(c1, peerEnvelope(inner...))

	body, _ := sess.PopAckTick()
	if body == nil {
		t.Fatal("peer-relayed game data should produce an ack tick")
	}
	if body[0] != 0xFE || body[1] != 0x01 {
		t.Errorf("ack body = %x, want a flush with one sub", body)
	}
}

func TestHandlePeerMalformedHeader(t *testing.T) {
	srv, sess, c1, c2 := twoPeerServer(t)
	// Too short for the header: the original returns before dispatching, and the
	// payload is still relayed untouched. Nothing is consumed either way.
	before := sess.HostID()
	srv.handlePeer(c1, make([]byte, 4))
	if sess.HostID() != before {
		t.Errorf("HostID = %d, want it unchanged at %d", sess.HostID(), before)
	}
	if len(c1.sent) != 0 || len(c2.sent) != 0 {
		t.Errorf("a malformed header must not send anything, got %d/%d frames", len(c1.sent), len(c2.sent))
	}
}

// A relayed peer message with an unhandled inner type reaches every
// destination, not just the host. Inner types 4 and 8 have no label in the
// original's inner switch, and its unhandled path returns "relayed", which the
// consumer maps to "relay to all". The host-only forward this server used to
// synthesise came from an unreachable block: it sits inside the switch, whose
// labels exclude 8, behind a guard that requires the inner type to be 8.
func TestProxyRelayUnhandledInnerTypeReachesEveryone(t *testing.T) {
	for _, typ := range []byte{0x04, protocol.PeerMsgDropoutPeer} {
		t.Run(protocol.MessageTypeName(typ), func(t *testing.T) {
			srv, sess, c1, c2 := twoPeerServer(t)
			sess.SetHostID(1)

			// Proxy body: src 1, two destinations (1 and 2), a payload whose head
			// is the peer message followed by the two 15-byte records.
			inner := []byte{typ}
			inner = binary.LittleEndian.AppendUint32(inner, 7)
			inner = append(inner, innerBlob("gone")...)
			inner = append(inner, 0x02)
			head := append([]byte{protocol.MsgPeer}, peerEnvelope(inner...)...)

			record := make([]byte, proxyRecordSize)
			body := []byte{0x01, 0x02, 0x00, 0x01, 0x02} // src 1, dstCount 2, dsts 1 and 2
			body = binary.LittleEndian.AppendUint16(body, uint16(len(head)+2*proxyRecordSize))
			body = append(body, head...)
			body = append(body, record...)
			body = append(body, record...)

			c1.sent = nil
			c2.sent = nil
			srv.dispatchMessage(c1, protocol.MsgProxy, body)

			if len(c1.sent) != 1 {
				t.Errorf("host destination received %d frames, want 1", len(c1.sent))
			}
			if len(c2.sent) != 1 {
				t.Errorf("non-host destination received %d frames, want 1 (an unhandled inner type is not filtered by host)", len(c2.sent))
			}
		})
	}
}

// A 0x0F with a bare header and no inner part is valid and relays to all
// destinations, whatever the session's host is. This is the counterpart to the
// test above: the delivery is decided by the destination list alone.
func TestProxyRelayBarePeerMessageReachesEveryone(t *testing.T) {
	for _, hostID := range []byte{0, 1, 2} {
		srv, sess, c1, c2 := twoPeerServer(t)
		sess.SetHostID(hostID)

		record := make([]byte, proxyRecordSize)
		head := []byte{protocol.MsgPeer}
		body := []byte{0x01, 0x02, 0x00, 0x01, 0x02}
		body = binary.LittleEndian.AppendUint16(body, uint16(len(head)+2*proxyRecordSize))
		body = append(body, head...)
		body = append(body, record...)
		body = append(body, record...)

		c1.sent = nil
		c2.sent = nil
		srv.dispatchMessage(c1, protocol.MsgProxy, body)

		if len(c1.sent) != 1 || len(c2.sent) != 1 {
			t.Errorf("hostID %d: a bare peer message reaches %d/%d peers, want 1/1", hostID, len(c1.sent), len(c2.sent))
		}
	}
}

// The inner RequestJoin supplies the peer's u64 key; the session-setup work
// itself is not repeated (the game drives sessions from LobbyConnect).
func TestHandlePeerRequestJoinRecordsKey(t *testing.T) {
	srv, sess, c1, _ := twoPeerServer(t)

	inner := []byte{protocol.PeerMsgRequestJoin}
	inner = append(inner, innerBlob("player")...)
	inner = append(inner, innerBlob("")...)
	inner = binary.LittleEndian.AppendUint64(inner, 0x18_112233445566)  // titleID 24 + sessionID
	inner = binary.LittleEndian.AppendUint64(inner, 0xFEEDFACECAFEBEEF) // peer key

	srv.handlePeer(c1, peerEnvelope(inner...))
	if sess.Peers[1].Key != 0xFEEDFACECAFEBEEF {
		t.Errorf("peer 1 key = %#x, want the one from the RequestJoin", sess.Peers[1].Key)
	}
}

// RequestJoinDenied schedules the peer named by key to close, not the sender.
func TestHandlePeerJoinDeniedSchedulesNamedPeer(t *testing.T) {
	srv, sess, c1, _ := twoPeerServer(t)
	sess.SetPeerKey(2, 0xABCD)

	inner := []byte{protocol.PeerMsgRequestJoinDenied}
	inner = append(inner, innerBlob("")...)
	inner = binary.LittleEndian.AppendUint32(inner, 9)
	inner = append(inner, innerBlob("session full")...)
	inner = binary.LittleEndian.AppendUint64(inner, 0xABCD)

	srv.handlePeer(c1, peerEnvelope(inner...))

	if _, _, ok := sess.Peers[1].CloseDeadline(); ok {
		t.Error("the sender must not be scheduled")
	}
	if _, _, ok := sess.Peers[2].CloseDeadline(); !ok {
		t.Error("the denied peer should be scheduled to close")
	}
}

// A RequestJoinDenied arriving with no session is the ordering case the original
// logs as a warning rather than an error.
func TestHandlePeerJoinDeniedWithoutSession(t *testing.T) {
	srv := createTestServer(t)
	client := &mockMsgClient{}

	inner := []byte{protocol.PeerMsgRequestJoinDenied}
	inner = append(inner, innerBlob("")...)
	inner = binary.LittleEndian.AppendUint32(inner, 1)
	inner = append(inner, innerBlob("too early")...)
	inner = binary.LittleEndian.AppendUint64(inner, 1)

	srv.handlePeer(client, peerEnvelope(inner...))
	if len(client.sent) != 0 {
		t.Errorf("unexpected traffic: %d messages", len(client.sent))
	}
}
