package session

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
	"time"
)

// mockMsg is one message a mockConn recorded. The session layer never builds
// wire frames itself, so the mocks assert on (type, body) rather than on bytes.
type mockMsg struct {
	typ  byte
	body []byte
}

type mockConn struct {
	msgs       []mockMsg
	closed     bool
	remoteAddr string
	// peerID is the connection's u64 identifier, the identity the observer list
	// is keyed on. A plain field, not a mutex-guarded one, because every
	// session method that touches it already holds the session lock.
	peerID uint64
	// msgVersion is the connection's announced u16 message version, the version
	// the observer end-of-game marker is gated on (see ClientMessageVersion).
	msgVersion uint16
}

func (c *mockConn) SendMessage(msgType byte, body []byte) error {
	c.msgs = append(c.msgs, mockMsg{typ: msgType, body: append([]byte(nil), body...)})
	return nil
}

func (c *mockConn) Close() error {
	c.closed = true
	return nil
}

func (c *mockConn) RemoteAddr() string {
	return c.remoteAddr
}

func (c *mockConn) SetPeerIdentifier(v uint64) { c.peerID = v }

func (c *mockConn) PeerIdentifier() uint64 { return c.peerID }

// msgVersion is the connection's u16 message version, which the
// observer end-of-game marker reads. Zero is what a
// connection that never sent a LobbyConnect has.
func (c *mockConn) ClientMessageVersion() uint16      { return c.msgVersion }
func (c *mockConn) setClientMessageVersion(v uint16) { c.msgVersion = v }

// countMsgs returns how many recorded messages have the given type.
func countMsgs(msgs []mockMsg, msgType byte) int {
	n := 0
	for _, m := range msgs {
		if m.typ == msgType {
			n++
		}
	}
	return n
}

func hasMsg(msgs []mockMsg, msgType byte) bool {
	return countMsgs(msgs, msgType) > 0
}

func TestNewSession(t *testing.T) {
	s := NewSession(1)
	if s.ID64() != 1 {
		t.Errorf("expected ID 1, got %d", s.ID64())
	}
	if s.IsClosed() {
		t.Error("new session should not be closed")
	}
}

func TestNextPeerID(t *testing.T) {
	s := NewSession(1)

	id1 := s.NextPeerID()
	if id1 != 1 {
		t.Errorf("first peer ID should be 1, got %d", id1)
	}

	id2 := s.NextPeerID()
	if id2 != 2 {
		t.Errorf("second peer ID should be 2, got %d", id2)
	}
}

func TestNextPeerIDRollover(t *testing.T) {
	s := NewSession(1)
	s.nextPeerID = 255

	id := s.NextPeerID()
	if id != 255 {
		t.Errorf("expected 255, got %d", id)
	}

	id = s.NextPeerID()
	if id != 1 {
		t.Errorf("expected rollover to 1, got %d", id)
	}
}

func TestAddPeer(t *testing.T) {
	s := NewSession(1)
	conn := &mockConn{}
	peer := NewPeer(1, conn)

	s.AddPeer(peer)

	if s.PeerCount() != 1 {
		t.Errorf("expected 1 peer, got %d", s.PeerCount())
	}

	got := s.GetPeer(1)
	if got == nil {
		t.Fatal("expected to find peer 1")
	}
	if got.ID != 1 {
		t.Errorf("expected peer ID 1, got %d", got.ID)
	}
}

func TestRemovePeer(t *testing.T) {
	s := NewSession(1)
	conn := &mockConn{}
	peer := NewPeer(1, conn)

	s.AddPeer(peer)
	s.RemovePeer(1)

	if s.PeerCount() != 0 {
		t.Errorf("expected 0 peers, got %d", s.PeerCount())
	}
	if s.GetPeer(1) != nil {
		t.Error("peer should be removed")
	}
}

// 0x1D SetObservableParamatersMessage sets four things, and only
// while the session has not started recording: the lag in pages, the observable
// flag, the u64 credential, and a window whose reader is not identified.
func TestSetObservableParameters(t *testing.T) {
	s := NewSession(1)
	s.SetObserverConfig(4, 8) // MaxObservers > 0

	if !s.SetObservableParameters(1, 0xDEADBEEF, 3, 8) {
		t.Fatal("0x1D should apply before the session records")
	}
	if !s.Reconnectable {
		t.Error("a non-zero first u32 must set the observable flag")
	}
	if got := s.ObservableHash(); got != 0xDEADBEEF {
		t.Errorf("observableHash = %#x, want 0xDEADBEEF", got)
	}
	if want := 3 * 8; s.LagPages != want {
		t.Errorf("LagPages = %d, want %d (seconds * ticksPerSec)", s.LagPages, want)
	}
	if s.observableWindow != -1 {
		t.Errorf("observableWindow = %d, want -1 with observers enabled", s.observableWindow)
	}

	// A zero first u32 clears the observable flag, which is what makes a
	// later StartObserve answer 4.
	s.SetObservableParameters(0, 0, 0, 8)
	if s.Reconnectable {
		t.Error("a zero first u32 must clear the observable flag")
	}
	if status, _ := s.StartObserve(&mockConn{}, 0, 0); status != 4 {
		t.Errorf("StartObserve after 0x1D cleared the flag = %d, want 4", status)
	}

	// Observers disabled: the window becomes a page count instead of -1.
	s2 := NewSession(1)
	s2.SetObserverConfig(0, 8)
	s2.SetObservableParameters(1, 0, 0, 8)
	if want := 60000/8 + 1; s2.observableWindow != want {
		t.Errorf("observableWindow = %d, want %d with observers disabled", s2.observableWindow, want)
	}
}

// Once the session is recording the original ignores 0x1D entirely: the message
// only applies while the recording flag is clear.
func TestSetObservableParametersIgnoredWhileRecording(t *testing.T) {
	s := NewSession(1)
	s.SetObserverConfig(4, 8)
	s.RecordFrame(ackFrameType, ackFrame(0))

	if s.SetObservableParameters(0, 0xAAAA, 5, 8) {
		t.Fatal("0x1D must be ignored once the session records")
	}
	if s.ObservableHash() != 0 {
		t.Error("an ignored 0x1D must not change the observable hash")
	}
	if !s.Reconnectable {
		t.Error("an ignored 0x1D must not change the observable flag")
	}
}

// observerTestHelpers are the pieces the observer tests share: a frame body that
// says which frame it is, a session whose store is small enough that a handful
// of frames makes it forget its first one, and the ack type the store holds.

// ackFrameType is the message type the frame store carries: the session tick
// appends the 0x0A it just built, so an observer is served 0x0A.
const ackFrameType = 0x0A

// ackFrame is a 43-byte frame body (100 against a block once the store's 57
// bytes of overhead are added) whose last two bytes are the frame number, so a
// test can tell both the count and the order the frames came back in.
func ackFrame(i int) []byte {
	b := make([]byte, 43)
	b[0] = 0xfe
	b[41] = byte(i >> 8)
	b[42] = byte(i)
	return b
}

func ackFrameNumber(b []byte) int {
	return int(b[41])<<8 | int(b[42])
}

// recordAcks stores n ack frames numbered 0..n-1.
func recordAcks(s *Session, n int) {
	for i := range n {
		s.RecordFrame(ackFrameType, ackFrame(i))
	}
}

// smallStoreSession is a session whose frame store keeps a single 200-byte
// block, so three frames of ackFrame are enough to make it drop its first block
// and Base() move past 0. That is the state a long match reaches, and the one
// the status-3 arm is about.
func smallStoreSession(id int64) *Session {
	s := NewSession(id)
	s.maxPagesPerMatch = 1
	s.pageSize = 200
	return s
}

func TestObserverStartObserveArms(t *testing.T) {
	s := NewSession(1)
	s.SetObserverConfig(2, 8)

	conn := &mockConn{}

	// No store yet. This is the "no replay" arm of the cascade, and it is the
	// arm that REGISTERS: it builds the status-0 response and
	// then appends the entry.
	if status, _ := s.StartObserve(conn, 0, 0); status != 0 {
		t.Errorf("expected status 0 without a store, got %d", status)
	}
	if !s.HasObserver(conn) {
		t.Error("the status-0 arm registers the observer; without it the client never activates")
	}
	s.RemoveObserver(conn)

	// A store that has not dropped its first frame. This is the capture's case:
	// 2026-09-29_012528_03cb67c7cf19 kept all 259651 bytes of the match's acks in
	// its blocks, answered 0, and served both observers from frame 0.
	s.RecordFrame(ackFrameType, ackFrame(0))
	s.RecordFrame(ackFrameType, ackFrame(1))
	if s.replay.Base() != 0 {
		t.Fatalf("Base = %d, want 0", s.replay.Base())
	}
	if status, _ := s.StartObserve(conn, 0, 0); status != 0 {
		t.Errorf("expected status 0 while the store still holds frame 0, got %d", status)
	}
	if !s.HasObserver(conn) {
		t.Error("the observer must be registered while the store still holds the start of the match")
	}

	// A store that HAS dropped its first frame answers 3 and registers nobody:
	// that arm only builds the response.
	small := smallStoreSession(1)
	small.SetObserverConfig(2, 8)
	recordAcks(small, 3)
	if small.replay.Base() == 0 {
		t.Fatalf("Base = 0: three frames of 100 bytes over one 200-byte block must have dropped the first")
	}
	if status, _ := small.StartObserve(conn, 0, 0); status != 3 {
		t.Errorf("expected status 3 once the store has dropped its first frame, got %d", status)
	}
	if small.HasObserver(conn) {
		t.Error("status 3 does not register: that arm never touches the observer list")
	}

	// Cap reached: rejected with status 2 (the observer list is at the cap).
	// The cap is one here, and conn already holds that slot.
	capped := NewSession(1)
	capped.SetObserverConfig(1, 8)
	capped.RecordFrame(ackFrameType, ackFrame(0))
	conn2 := &mockConn{}
	if status, _ := capped.StartObserve(conn2, 0, 0); status != 0 {
		t.Fatalf("expected the first observer to be admitted, got %d", status)
	}
	if status, _ := capped.StartObserve(&mockConn{}, 0, 0); status != 2 {
		t.Errorf("expected status 2 over the cap, got %d", status)
	}

	// Not reconnectable: rejected with status 4, which is a different rejection
	// from the cap. In the original these are two arms of the same cascade, and
	// each carries its own constant.
	sess2 := NewSession(1)
	sess2.SetReconnectable(false)
	sess2.RecordFrame(ackFrameType, ackFrame(0))
	if status, _ := sess2.StartObserve(conn2, 0, 0); status != 4 {
		t.Errorf("expected status 4 when not reconnectable, got %d", status)
	}

	// Observers disabled entirely (cap of zero) is the same arm.
	sess3 := NewSession(1)
	sess3.SetObserverConfig(0, 8)
	sess3.RecordFrame(ackFrameType, ackFrame(0))
	if status, _ := sess3.StartObserve(conn2, 0, 0); status != 4 {
		t.Errorf("expected status 4 with observers disabled, got %d", status)
	}

	// Removal via StopObserve.
	if !s.RemoveObserver(conn) {
		t.Error("RemoveObserver should find the observer")
	}
	if s.ObserverCount() != 0 {
		t.Error("observer list should be empty")
	}
	if sess2.RemoveObserver(conn2) {
		t.Error("RemoveObserver should report nothing removed for unknown clients")
	}
}

// TestObserverStartObserveTwoConnections covers the two observe attempts of
// capture 2026-09-29_012528_03cb67c7cf19: the game makes them on two connections,
// and each one has to become its own entry. The identity compared is the
// connection's peerIdentifier, which is what the two captures' requests carry
// distinctly, so a second attempt can never be mistaken for a repeat of the first,
// which is what would have left it with no window of its own.
func TestObserverStartObserveTwoConnections(t *testing.T) {
	s := NewSession(1)
	s.SetObserverConfig(2, 1)
	recordAcks(s, 300)

	// The two peerIdentifiers are the ones the capture's two 0x1C bodies carry
	// at offset 9, the third u64 of the request.
	first, second := &mockConn{}, &mockConn{}
	if status, _ := s.StartObserve(first, 0xC5C0428C2971CE8B, 0); status != 0 {
		t.Fatalf("first attempt: expected status 0, got %d", status)
	}
	if !s.ActivateObserver(first, 8) {
		t.Fatal("first attempt: ActivateObserver should find its entry")
	}
	// The first observer rides the live stream while the match goes on.
	for i := range 20 {
		s.RecordFrame(ackFrameType, ackFrame(300+i))
		s.Tick()
	}

	if status, _ := s.StartObserve(second, 0xA9AE099C85ED50C4, 0); status != 0 {
		t.Fatalf("second attempt: expected status 0, got %d", status)
	}
	if s.ObserverCount() != 2 {
		t.Fatalf("ObserverCount = %d, want 2: a second connection is a second observer", s.ObserverCount())
	}
	if !s.ActivateObserver(second, 8) {
		t.Fatal("second attempt: ActivateObserver should find its entry")
	}

	// The late one is served from the start of the match, not from where it
	// joined, and 200 frames per visit is the original's cap.
	s.Tick()
	if n := countMsgs(second.msgs, ackFrameType); n != 200 {
		t.Fatalf("the late observer got %d frames on the first visit, want the 200 cap", n)
	}
	s.Tick()
	late := framesOfType(second.msgs, ackFrameType)
	if len(late) != 320 {
		t.Fatalf("the late observer got %d frames, want 320 (the whole store over two visits)", len(late))
	}
	for i, b := range late {
		if got := ackFrameNumber(b); got != i {
			t.Fatalf("frame %d of the late observer is store frame %d: the window must start at 0 and be in order", i, got)
		}
	}
	if n := countMsgs(second.msgs, 0x09); n != 0 {
		t.Errorf("the observer got %d 0x09 pages: the store holds the session's 0x0A acks, not the uploads", n)
	}
}

// framesOfType returns the bodies of the recorded messages of one type.
func framesOfType(msgs []mockMsg, msgType byte) [][]byte {
	var out [][]byte
	for _, m := range msgs {
		if m.typ == msgType {
			out = append(out, m.body)
		}
	}
	return out
}

// TestObserverStartObserveDedup used to pin an idempotent repeated StartObserve
// on one connection. That is not the original's behaviour and the guard is gone:
// the original never scans the list, it appends, so a
// second request that reaches the 0 arm adds a second entry and renames the
// first. See TestStartObserveOnTheSameConnectionTwice, which is where that lives
// now, next to the identifier cases it belongs with.

// errConn is a PeerConnection whose SendMessage always fails, standing in for a
// stalled observer.
type errConn struct{ peerID uint64; msgVersion uint16 }

func (c *errConn) SendMessage(_ byte, _ []byte) error { return errors.New("stalled") }
func (c *errConn) Close() error                       { return nil }
func (c *errConn) RemoteAddr() string                 { return "test" }
func (c *errConn) SetPeerIdentifier(v uint64)         { c.peerID = v }
func (c *errConn) PeerIdentifier() uint64             { return c.peerID }
func (c *errConn) ClientMessageVersion() uint16       { return c.msgVersion }
func (c *errConn) setClientMessageVersion(v uint16)   { c.msgVersion = v }

// TestObserverDropStalled covers the stalled-observer recovery: consecutive
// failed frame deliveries reach maxObserverSendFailures and the observer is
// removed from the list so the server keeps serving the rest.
func TestObserverDropStalled(t *testing.T) {
	s := NewSession(1)
	s.SetObserverConfig(1, 1) // interval 1 -> served every other tick
	recordAcks(s, 40)
	conn := &errConn{}
	if status, _ := s.StartObserve(conn, 0, 0); status != 0 {
		t.Fatalf("expected status 0, got %d", status)
	}
	if !s.ActivateObserver(conn, 8) {
		t.Fatal("ActivateObserver should find the entry")
	}

	for range 8 {
		s.Tick()
	}
	if s.ObserverCount() != 0 {
		t.Errorf("ObserverCount = %d, want 0 (stalled observer dropped)", s.ObserverCount())
	}
}

func TestObserverActivateAndKick(t *testing.T) {
	s := NewSession(1)
	s.SetObserverConfig(2, 8)
	recordAcks(s, 2)

	conn := &mockConn{}
	if status, _ := s.StartObserve(conn, 0, 0); status != 0 {
		t.Fatalf("expected status 0, got %d", status)
	}

	// Activation answers with ActivateObserverResponse (type 0x1E) and enables
	// frame delivery. The body is [status u8][time i64 LE] and reaches the wire
	// unframed: the transport adds whatever prefix its port uses.
	if !s.ActivateObserver(conn, 8) {
		t.Fatal("ActivateObserver should find the entry")
	}
	if len(conn.msgs) != 1 || conn.msgs[0].typ != 0x1E || len(conn.msgs[0].body) != 9 {
		t.Fatalf("expected one 9-byte ActivateObserverResponse, got %+v", conn.msgs)
	}

	// A session that is neither recording nor reconnectable kicks its observers
	// with a StartObserveResponse status 4. Recording only turns
	// true once the store exists and never resets in the original, so force it
	// here to exercise the kick path.
	conn.msgs = nil
	s.SetReconnectable(false)
	s.Recording = false
	s.Tick()
	if len(conn.msgs) != 1 || conn.msgs[0].typ != 0x19 || !bytes.Equal(conn.msgs[0].body, []byte{4}) {
		t.Fatalf("expected kick (type 0x19 status 4), got %+v", conn.msgs)
	}
	if s.ObserverCount() != 0 {
		t.Error("kicked observers should be removed")
	}
}

func TestObserverUpdateStride(t *testing.T) {
	s := NewSession(1)
	s.SetObserverConfig(8, 2)
	s.SetReconnectable(true)
	recordAcks(s, 11)

	obs1 := &mockConn{msgVersion: 4}
	obs2 := &mockConn{msgVersion: 4}
	if status, _ := s.StartObserve(obs1, 1, 0); status != 0 {
		t.Fatalf("expected status 0 for obs1, got %d", status)
	}
	if status, _ := s.StartObserve(obs2, 2, 0); status != 0 {
		t.Fatalf("expected status 0 for obs2, got %d", status)
	}
	if !s.ActivateObserver(obs1, 8) || !s.ActivateObserver(obs2, 8) {
		t.Fatal("activation failed")
	}

	// With interval 2 (period 3): tick 0 -> observer 0, tick 1 -> observer 1,
	// tick 2 -> none, then it wraps. Both catch up over the whole store, and a
	// session with no players left is the one case where the original also sends
	// the end-of-game-data marker, which is the term that says the store's end is
	// this observer's target.
	for range 6 {
		s.Tick()
	}

	sent1 := countMsgs(obs1.msgs, ackFrameType)
	sent2 := countMsgs(obs2.msgs, ackFrameType)
	if sent1 != 11 || sent2 != 11 {
		t.Fatalf("expected 11 frames each, got obs1=%d obs2=%d", sent1, sent2)
	}
	if !hasMsg(obs1.msgs, 0x22) || !hasMsg(obs2.msgs, 0x22) {
		t.Error("caught-up observers should receive EndOfGameDataMessage (type 0x22)")
	}

	// Additional frames are delivered on the next cycle, to the observer this
	// tick's stride selected.
	s.RecordFrame(ackFrameType, ackFrame(11))
	s.Tick()
	got1, got2 := countMsgs(obs1.msgs, ackFrameType), countMsgs(obs2.msgs, ackFrameType)
	if got1-got2 != 1 {
		t.Errorf("exactly one observer should be served per stride step, got obs1=%d obs2=%d", got1, got2)
	}
}

// TestObserverNotStalledByEviction: an observer that fell behind the retained
// window is jumped to the window's start instead of stalling. The original
// refuses the range and leaves the progress where it is, so this is a deliberate
// difference; it can only be reached
// after maxBlocks*pageSize bytes of history, which is fifty minutes of match at
// the default settings.
func TestObserverNotStalledByEviction(t *testing.T) {
	s := smallStoreSession(1)
	s.SetObserverConfig(1, 1)
	// One block, two frames: the store still holds frame 0, so the observer is
	// admitted.
	recordAcks(s, 2)

	conn := &mockConn{msgVersion: 4}
	if status, _ := s.StartObserve(conn, 0, 0); status != 0 {
		t.Fatalf("expected status 0, got %d", status)
	}
	if !s.ActivateObserver(conn, 8) {
		t.Fatal("ActivateObserver should find the entry")
	}
	conn.msgs = nil

	// The third frame recycles the block and the window starts at 2, which is
	// past the observer's progress of 0: without the resync it would never be
	// served again.
	recordAcks(s, 7)

	for range 8 {
		s.Tick()
	}
	if n := countMsgs(conn.msgs, ackFrameType); n == 0 {
		t.Fatal("the observer received nothing: it is stuck below the window")
	}
	if !hasMsg(conn.msgs, 0x22) {
		t.Error("caught-up observer should receive EndOfGameDataMessage (type 0x22)")
	}
	s.mu.RLock()
	progress := s.observers[0].Progress
	s.mu.RUnlock()
	if progress != s.PageCount {
		t.Errorf("observer Progress = %d, want %d (caught up)", progress, s.PageCount)
	}
}

func TestObserverResyncsToWindowBase(t *testing.T) {
	s := smallStoreSession(1)
	s.SetObserverConfig(1, 1)
	recordAcks(s, 2)

	conn := &mockConn{}
	if status, _ := s.StartObserve(conn, 0, 0); status != 0 {
		t.Fatalf("expected status 0, got %d", status)
	}
	if !s.ActivateObserver(conn, 8) {
		t.Fatal("ActivateObserver should find the entry")
	}
	conn.msgs = nil

	// Force the observer to lag behind the window, then let the recycled block
	// advance the base well past its Progress.
	s.mu.Lock()
	s.observers[0].Progress = 1
	s.mu.Unlock()
	recordAcks(s, 7)

	for range 6 {
		s.Tick()
	}

	s.mu.RLock()
	progress := s.observers[0].Progress
	s.mu.RUnlock()
	if progress < s.replay.Base() {
		t.Errorf("observer Progress = %d, want >= base %d", progress, s.replay.Base())
	}
	if n := countMsgs(conn.msgs, ackFrameType); n == 0 {
		t.Error("resynced observer should receive frames from the retained window")
	}
}

// TestObserverNoEndOfGameDataMarkerWhilePlayersRemain pins the marker gate.
// The original only sends it when the store's end is this observer's target (the
// game has started, GameStarted, or the session has no peers left) and the observer's
// client announces message version 4 or more. Nothing here ever starts the game
// over, so with a player in the session the marker is never sent, which
// is what the capture shows: the original sent zero type-34 messages to either of
// its two observers.
func TestObserverNoEndOfGameDataMarkerWhilePlayersRemain(t *testing.T) {
	s := NewSession(1)
	s.SetObserverConfig(2, 1)
	s.AddPeer(NewPeer(1, &mockConn{}))
	recordAcks(s, 4)

	conn := &mockConn{}
	if status, _ := s.StartObserve(conn, 0, 0); status != 0 {
		t.Fatalf("expected status 0, got %d", status)
	}
	if !s.ActivateObserver(conn, 8) {
		t.Fatal("ActivateObserver should find the entry")
	}
	conn.msgs = nil

	for range 4 {
		s.Tick()
	}
	if n := countMsgs(conn.msgs, ackFrameType); n != 4 {
		t.Fatalf("observer got %d frames, want 4", n)
	}
	if hasMsg(conn.msgs, 0x22) {
		t.Error("no type-34 marker while a player is in the session: the original sends none")
	}
}

// discardConn is a PeerConnection that drops everything (no accumulation, no
// allocation), used to measure Broadcast's own cost.
type discardConn struct{ peerID uint64; msgVersion uint16 }

func (c *discardConn) SendMessage(_ byte, _ []byte) error { return nil }
func (c *discardConn) Close() error                       { return nil }
func (c *discardConn) RemoteAddr() string                 { return "bench" }
func (c *discardConn) SetPeerIdentifier(v uint64)         { c.peerID = v }
func (c *discardConn) PeerIdentifier() uint64             { return c.peerID }
func (c *discardConn) ClientMessageVersion() uint16       { return c.msgVersion }
func (c *discardConn) setClientMessageVersion(v uint16)   { c.msgVersion = v }

// BenchmarkSessionBroadcast measures the per-peer-add broadcast frame. Before
// C10 it allocated a 3-byte header and then reallocated the appended frame
// (two allocations); after, one pre-sized frame.
func BenchmarkSessionBroadcast(b *testing.B) {
	s := NewSession(1)
	for i := byte(1); i <= 4; i++ {
		s.AddPeer(NewPeer(i, &discardConn{}))
	}
	body := make([]byte, 64)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Broadcast(0x09, body, 1)
	}
}

// Broadcast must reach every peer but the excluded one, and it must not build
// the frame itself: the transport owns framing.
func TestBroadcast(t *testing.T) {
	s := NewSession(1)
	conn1 := &mockConn{}
	conn2 := &mockConn{}
	conn3 := &mockConn{}

	s.AddPeer(NewPeer(1, conn1))
	s.AddPeer(NewPeer(2, conn2))
	s.AddPeer(NewPeer(3, conn3))

	s.Broadcast(0x09, []byte{0x01, 0x02}, 1)

	if len(conn1.msgs) != 0 {
		t.Error("excluded peer should not receive broadcast")
	}
	for i, conn := range []*mockConn{conn2, conn3} {
		if len(conn.msgs) != 1 {
			t.Fatalf("peer %d should receive exactly one broadcast, got %d", i+2, len(conn.msgs))
		}
		if conn.msgs[0].typ != 0x09 || !bytes.Equal(conn.msgs[0].body, []byte{0x01, 0x02}) {
			t.Errorf("peer %d got type=%x body=%x, want 09 0102", i+2, conn.msgs[0].typ, conn.msgs[0].body)
		}
	}
}

func TestClose(t *testing.T) {
	s := NewSession(1)
	conn := &mockConn{}
	peer := NewPeer(1, conn)

	s.AddPeer(peer)
	s.Close()

	if !s.IsClosed() {
		t.Error("session should be closed")
	}
}

func TestNewPeer(t *testing.T) {
	conn := &mockConn{}
	peer := NewPeer(5, conn)

	if peer.ID != 5 {
		t.Errorf("expected ID 5, got %d", peer.ID)
	}
}

func TestPeerClose(t *testing.T) {
	conn := &mockConn{}
	peer := NewPeer(1, conn)

	err := peer.Close()
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !conn.closed {
		t.Error("connection should be closed")
	}
}

func TestPeerSendMessage(t *testing.T) {
	conn := &mockConn{}
	peer := NewPeer(1, conn)

	if err := peer.SendMessage(0x13, []byte{0x01, 0x02, 0x03}); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if len(conn.msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(conn.msgs))
	}
	if conn.msgs[0].typ != 0x13 || !bytes.Equal(conn.msgs[0].body, []byte{0x01, 0x02, 0x03}) {
		t.Errorf("got type=%x body=%v, want 13 [1 2 3]", conn.msgs[0].typ, conn.msgs[0].body)
	}
}

func TestSessionManagerCreateSession(t *testing.T) {
	mgr := NewManager(0, 0)
	s1 := mgr.CreateSession()
	s2 := mgr.CreateSession()

	if s1.ID64() == s2.ID64() {
		t.Error("sessions should have different IDs")
	}
	if mgr.SessionCount() != 2 {
		t.Errorf("expected 2 sessions, got %d", mgr.SessionCount())
	}
}

func TestSessionManagerGetSession(t *testing.T) {
	mgr := NewManager(0, 0)
	s := mgr.CreateSession()

	got := mgr.GetSession(SessionKey{SessionID: s.ID64()})
	if got == nil {
		t.Fatal("should find session")
	}
	if got.ID64() != s.ID64() {
		t.Error("session ID mismatch")
	}
}

func TestSessionManagerRemoveSession(t *testing.T) {
	mgr := NewManager(0, 0)
	s := mgr.CreateSession()
	id := s.ID64()

	mgr.RemoveSession(SessionKey{SessionID: id})

	if mgr.GetSession(SessionKey{SessionID: id}) != nil {
		t.Error("session should be removed")
	}
	if mgr.SessionCount() != 0 {
		t.Errorf("expected 0 sessions, got %d", mgr.SessionCount())
	}
}

func TestSessionManagerFindSessionByPeer(t *testing.T) {
	mgr := NewManager(0, 0)
	s := mgr.CreateSession()
	conn := &mockConn{}
	peer := NewPeer(1, conn)
	mgr.AddPeer(s, peer)

	found := mgr.FindSessionByPeer(conn)
	if found == nil {
		t.Fatal("should find session by peer connection")
	}
	if found.ID64() != s.ID64() {
		t.Error("found wrong session")
	}
}

func TestSessionManagerHandleLeaveSimulation(t *testing.T) {
	mgr := NewManager(0, 0)
	s := mgr.CreateSession()
	conn := &mockConn{}
	peer := NewPeer(1, conn)
	mgr.AddPeer(s, peer)

	mgr.HandleLeaveSimulation(conn)

	// The leave removes nothing from the session: the peer vector and the
	// session map are untouched. A session ends when its last peer DISCONNECTS,
	// which is the disconnect path and, here, RemoveClient.
	if mgr.SessionCount() != 1 {
		t.Errorf("SessionCount = %d, want 1: a LeaveSimulation does not end the session", mgr.SessionCount())
	}
	if s.PeerCount() != 1 {
		t.Errorf("PeerCount = %d, want 1", s.PeerCount())
	}
}

func TestSessionNewPeer(t *testing.T) {
	s := NewSession(1)
	conn := &mockConn{}
	peer := s.NewPeer(5, conn)

	if peer.ID != 5 {
		t.Errorf("expected ID 5, got %d", peer.ID)
	}
}

// A peer whose game-data queue stays empty past its deadline is reaped with
// syncDataTimeoutReason. The deadline defaults to
// 60 s and the peer's LobbyConnect timeout override replaces it.
// Without the reaping a peer that stops sending stalls the
// session forever.
// The original only checks a peer whose game-data queue has drained,
// so a peer still holding a backlog is spared even once its
// deadline has passed. The tick drains before the check runs, so the exemption is
// visible until the next drain.
func TestSessionSyncTimeoutSparesPeerWithQueuedData(t *testing.T) {
	s := NewSession(1)
	s.AddPeer(NewPeer(1, &mockConn{}))
	s.SetPeerTimeoutOverride(1, 1000)
	s.QueueGameData(1, []byte{0x01, 0x02, 0x03})

	// The queue still holds the sub, so the deadline is not even evaluated.
	if got := s.SyncTimedOutPeers(time.Now().Add(2 * time.Second)); len(got) != 0 {
		t.Fatalf("peer with queued data must be spared, got %v", got)
	}

	// Once the tick drains it, the same elapsed time reaps it.
	s.PopAckTick()
	if got := s.SyncTimedOutPeers(time.Now().Add(2 * time.Second)); len(got) != 1 {
		t.Fatalf("after the drain the peer must be reaped, got %v", got)
	}
}

// The sync-timeout answer comes out of the walk PopAckTick already made, and that
// walk runs BEFORE the drain. So a peer whose queue empties in this tick's drain
// is still seen holding data by this tick's check, and is reaped on the NEXT one:
// one tick (125 ms) later than the live check it replaced. This pins that
// ordering, which is the price of not walking the peers twice a tick.
func TestSessionSyncTimeoutReapsOneTickAfterTheQueueEmpties(t *testing.T) {
	s := NewSession(1)
	s.AddPeer(NewPeer(1, &mockConn{}))
	s.SetPeerTimeoutOverride(1, 1) // 1 ms, so the deadline is already past
	s.QueueGameData(1, []byte{0x01, 0x02, 0x03})
	time.Sleep(5 * time.Millisecond)

	// This tick's walk sees the queue still holding the sub, so the peer is
	// spared even though its deadline has passed.
	s.PopAckTick()
	s.Tick()
	if _, _, ok := s.GetPeer(1).CloseDeadline(); ok {
		t.Fatal("the peer must not be reaped on the tick whose walk saw its queue non-empty")
	}

	// The next tick's walk sees the drained queue and reaps it.
	s.PopAckTick()
	s.Tick()
	if _, reason, ok := s.GetPeer(1).CloseDeadline(); !ok || reason != syncDataTimeoutReason {
		t.Fatalf("the peer must be reaped on the next tick, got ok=%v reason=%q", ok, reason)
	}
}

func TestSessionSyncTimeoutReapsSilentPeer(t *testing.T) {
	s := NewSession(1)
	conn := &mockConn{}
	s.AddPeer(NewPeer(1, conn))
	s.QueueGameData(1, []byte{0x01, 0x02, 0x03})
	s.PopAckTick() // the tick drains, leaving the queue empty as the original sees it

	// Data just arrived: not timed out.
	if got := s.SyncTimedOutPeers(time.Now()); len(got) != 0 {
		t.Fatalf("peer timed out immediately after queueing data: %v", got)
	}

	// Past the 60 s default with no further data.
	got := s.SyncTimedOutPeers(time.Now().Add(61 * time.Second))
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("SyncTimedOutPeers = %v, want [1] after 61 s", got)
	}
}

// A peer's timeout override replaces the 60 s default, so the same elapsed time
// reaps a peer with a short override and spares one with a long one.
func TestSessionSyncTimeoutHonoursOverride(t *testing.T) {
	s := NewSession(1)
	shortConn := &mockConn{}
	longConn := &mockConn{}
	s.AddPeer(NewPeer(1, shortConn))
	s.AddPeer(NewPeer(2, longConn))
	s.SetPeerTimeoutOverride(1, 1000)   // 1 s
	s.SetPeerTimeoutOverride(2, 600000) // 10 min
	s.QueueGameData(1, []byte{0x01})
	s.QueueGameData(2, []byte{0x01})
	s.PopAckTick()

	got := s.SyncTimedOutPeers(time.Now().Add(2 * time.Second))
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("SyncTimedOutPeers = %v, want only [1] (the 1 s override)", got)
	}


	got = s.SyncTimedOutPeers(time.Now().Add(11 * time.Minute))
	if len(got) != 2 {
		t.Fatalf("SyncTimedOutPeers = %v, want both after 11 min", got)
	}
}

// The sync reaper uses the immediate scheduler, which refuses to override a
// deadline that already exists (it checks the scheduled flag first).
// A peer already on its way out keeps its original reason and timing.
func TestPeerScheduleCloseIfNoneKeepsExistingDeadline(t *testing.T) {
	p := NewPeer(1, &mockConn{})
	at := time.Now().Add(30 * time.Second)

	if !p.ScheduleCloseIfNone(at, syncDataTimeoutReason) {
		t.Fatal("the first schedule should be accepted")
	}
	if p.ScheduleCloseIfNone(time.Now(), "something else") {
		t.Error("a second schedule must be refused while a deadline is pending")
	}
	gotAt, reason, ok := p.CloseDeadline()
	if !ok || !gotAt.Equal(at) || reason != syncDataTimeoutReason {
		t.Errorf("deadline = %v/%q, want the first one kept", gotAt, reason)
	}
}

func TestMockConnError(t *testing.T) {
	conn := &mockConn{}
	err := conn.Close()
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSessionDoubleClose(t *testing.T) {
	s := NewSession(1)
	s.Close()
	s.Close()
}

func TestSessionGetPeers(t *testing.T) {
	s := NewSession(1)
	s.AddPeer(NewPeer(1, &mockConn{}))
	s.AddPeer(NewPeer(2, &mockConn{}))

	peers := s.GetPeers()
	if len(peers) != 2 {
		t.Errorf("expected 2 peers, got %d", len(peers))
	}
}

// The tick reconciles only *after* it has built the 0x0A, because the original
// sends the ack to every peer first and reconciles
// afterwards. An earlier revision reconciled first and
// shipped the merged entries in the same frame; that ordering was wrong.
func TestSessionPopAckTickReconcilesAfterTheAck(t *testing.T) {
	s := NewSession(1)
	s.AddPeer(NewPeer(1, &mockConn{}))
	s.AddPeer(NewPeer(2, &mockConn{}))
	entry := []byte{0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11}
	s.QueueChecksumEntries(1, entry)
	s.QueueChecksumEntries(2, entry)
	s.QueueGameData(1, []byte{0x00, 0x00})

	ack, notice := s.PopAckTick()
	if notice != nil {
		t.Fatalf("agreeing peers must not produce a desync notice, got %x", notice)
	}
	if bytes.Contains(ack, entry) {
		t.Fatalf("the merged entry must not ride the frame built before the reconciliation: %x", ack)
	}

	// The next tick carries it.
	ack, notice = s.PopAckTick()
	if notice != nil {
		t.Fatalf("agreeing peers must not produce a desync notice, got %x", notice)
	}
	if !bytes.Contains(ack, entry) {
		t.Fatalf("the merged entry must appear in the following tick, got %x", ack)
	}
}

// A disagreement makes the tick broadcast a desync notice after the ack, one per
// disagreeing round. The notice is a GameData (0x09) message whose body is
// [FB][len u16 LE][flag][payload], and the payload lists that round's front
// entries: [01][ref u32 LE][count u16 LE] + {peer u8, d0 u32 LE, d1 u32 LE} per
// active peer, with ref the reference entry's second dword. Two peers make 25
// payload bytes, which do not compress, so the flag is 0. A disagreement still
// consumes one entry from every peer, so two queued entries make two rounds, two
// notices, and the third round stops.
func TestSessionPopAckTickDesyncNotice(t *testing.T) {
	s := NewSession(1)
	s.AddPeer(NewPeer(1, &mockConn{}))
	s.AddPeer(NewPeer(2, &mockConn{}))
	s.QueueChecksumEntries(1, []byte{
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18})
	s.QueueChecksumEntries(2, []byte{
		0x01, 0x02, 0x03, 0x04, 0xff, 0xfe, 0xfd, 0xfc,
		0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28})
	s.QueueGameData(1, []byte{0x00, 0x00})

	ack, notices := s.PopAckTick()
	if ack == nil {
		t.Fatal("the ack must still be produced on a disagreement")
	}
	want := [][]byte{
		{0xFB, 0x1A, 0x00, 0x00, // len 26: the flag and 25 payload bytes, raw
			0x01, 0x05, 0x06, 0x07, 0x08, 0x02, 0x00,
			0x01, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
			0x02, 0x01, 0x02, 0x03, 0x04, 0xff, 0xfe, 0xfd, 0xfc},
		{0xFB, 0x1A, 0x00, 0x00,
			0x01, 0x15, 0x16, 0x17, 0x18, 0x02, 0x00,
			0x01, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
			0x02, 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28},
	}
	if len(notices) != len(want) {
		t.Fatalf("got %d desync notices, want %d: one per disagreeing round", len(notices), len(want))
	}
	for i := range want {
		if !bytes.Equal(notices[i], want[i]) {
			t.Errorf("notice %d = %x,\nwant       %x", i+1, notices[i], want[i])
		}
	}
	if s.DesyncCount != 2 {
		t.Errorf("DesyncCount = %d, want 2: two rounds disagreed and the third stopped", s.DesyncCount)
	}
}


var benchPeerSink int

// BenchmarkSessionPeerIteration measures iterating a session's peers the way
// the ack-tick, broadcast, leave-simulation and liveness paths read them.
// Before P10 the callers materialized a snapshot slice via GetPeers (one
// allocation per call); after they iterate the peers directly under the
// session lock with ForEachPeer (allocation-free).
func BenchmarkSessionPeerIteration(b *testing.B) {
	for _, n := range []int{2, 8, 32} {
		b.Run(fmt.Sprintf("peers=%d", n), func(b *testing.B) {
			s := NewSession(1)
			for i := range n {
				s.AddPeer(NewPeer(byte(i+1), &mockConn{}))
			}
			b.ReportAllocs()
			b.Run("GetPeersSnapshot", func(b *testing.B) {
				b.ReportAllocs()
				total := 0
				for i := 0; i < b.N; i++ {
					for _, p := range s.GetPeers() {
						total += int(p.ID)
					}
				}
				benchPeerSink += total
			})
			b.Run("ForEachPeer", func(b *testing.B) {
				b.ReportAllocs()
				total := 0
				for i := 0; i < b.N; i++ {
					s.ForEachPeer(func(p *Peer) {
						total += int(p.ID)
					})
				}
				benchPeerSink += total
			})
		})
	}
}

func TestPeerCloseNilConn(t *testing.T) {
	p := NewPeer(1, nil)
	if err := p.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}
}

func TestHasObserverNotFound(t *testing.T) {
	s := NewSession(1)
	if s.HasObserver(&mockConn{}) {
		t.Error("HasObserver should report false for unknown clients")
	}
}

func TestActivateObserverEdges(t *testing.T) {
	s := NewSession(1)
	s.SetReconnectable(true)
	s.SetObserverConfig(2, 8)
	recordAcks(s, 2)

	// ticksPerSec < 1 is clamped to 1 (the offset math must not divide by zero).
	conn := &mockConn{}
	if status, _ := s.StartObserve(conn, 0x51DE, 0); status != 0 {
		t.Fatalf("expected status 0, got %d", status)
	}
	if !s.ActivateObserver(conn, 0) {
		t.Fatal("ActivateObserver should find the entry")
	}
	if len(conn.msgs) != 1 || conn.msgs[0].typ != 0x1E || len(conn.msgs[0].body) != 9 {
		t.Fatalf("expected one 9-byte ActivateObserverResponse, got %+v", conn.msgs)
	}

	// A lagged observer is one that does NOT present the u64 the session
	// published with 0x1D, so it takes a different
	// subtraction for the reported start time. Both arms are below, in
	// TestTheLaggedObserverReportsATimeAheadOfNow.
	//
	// Unknown clients are rejected. "Unknown" is now the peerIdentifier, not the
	// connection: a connection that never sent a StartObserve carries 0, and a
	// registered observer here is known by 0x51DE, so a fresh connection cannot
	// match. The original's activate is the same silent no-op.
	unknown := &mockConn{}
	unknown.SetPeerIdentifier(0xDEAD)
	if s.ActivateObserver(unknown, 8) {
		t.Error("ActivateObserver should reject unknown clients")
	}
}

// publishedHash is the u64 the players announce with 0x1D.
// It is the value a StartObserveRequest's passwordHash has to
// match for the observer NOT to be lagged.
const publishedHash = 0x21616C6661AA7A42

// The lagged arm of ActivateObserver, end to end: the flag comes from the hash
// comparison, and the time it reports is AHEAD of now while the match is shorter
// than the published lag.
//
// The formula is the original's, and it is not the non-lagged one with a sign
// flipped:
//
//	lagged:     (LagPages    - PageCount) / (1000 / ticksPerSecond)
//	not lagged: -(PageCount              / (1000 / ticksPerSecond))
//
// so with a lag of 30 s at 8 ticks/s (240 pages) against a 100-frame match the
// lagged arm reports +17 seconds and the plain arm -12: 29 seconds apart and on
// opposite sides of now. A lag smaller than the match moves the lagged arm into
// the past, which is why the sign cannot be asserted on its own.
func TestTheLaggedObserverReportsATimeAheadOfNow(t *testing.T) {
	const (
		ticksPerSec = 8
		lagSeconds  = 30
		lagPages    = lagSeconds * ticksPerSec // 240
		matchFrames = 100
	)
	plainOffset := -(matchFrames / ticksPerSec)              // -12
	laggedOffset := (lagPages - matchFrames) / ticksPerSec    // +17
	if plainOffset >= 0 || laggedOffset <= 0 {
		t.Fatalf("the two arms must fall on opposite sides of now: plain=%d lagged=%d", plainOffset, laggedOffset)
	}

	newSession := func(hashInRequest uint64) (*Session, *mockConn, int64) {
		s := NewSession(1)
		s.SetObserverConfig(2, ticksPerSec)
		s.SetObservableParameters(1, publishedHash, lagSeconds, ticksPerSec)
		if want := lagPages; s.LagPages != want {
			t.Fatalf("LagPages = %d, want %d (the published lag in pages)", s.LagPages, want)
		}
		recordAcks(s, matchFrames)
		conn := &mockConn{}
		if status, _ := s.StartObserve(conn, 0, hashInRequest); status != 0 {
			t.Fatalf("StartObserve = %d, want 0", status)
		}
		if !s.ActivateObserver(conn, ticksPerSec) {
			t.Fatal("ActivateObserver should find the entry")
		}
		if len(conn.msgs) != 1 || conn.msgs[0].typ != 0x1E || len(conn.msgs[0].body) != 9 {
			t.Fatalf("expected one 9-byte ActivateObserverResponse, got %+v", conn.msgs)
		}
		got := int64(binary.LittleEndian.Uint64(conn.msgs[0].body[1:9]))
		if conn.msgs[0].body[0] != 0 {
			t.Errorf("status byte = %d, want 0: the only other arm of the activate sends nothing at all", conn.msgs[0].body[0])
		}
		return s, conn, got
	}

	// The hash the session published: not lagged, time in the past.
	_, _, plain := newSession(publishedHash)
	if d := plain - nowUnix(); d != int64(plainOffset) {
		t.Errorf("plain observer time = now%+ds, want now%+ds (-(PageCount/ticksPerSec))", d, plainOffset)
	}

	// A different hash, and no hash at all: both lagged, time ahead of now.
	for _, hash := range []uint64{publishedHash ^ 1, 0} {
		_, _, lagged := newSession(hash)
		if d := lagged - nowUnix(); d != int64(laggedOffset) {
			t.Errorf("lagged observer (hash %#x) time = now%+ds, want now%+ds ((LagPages-PageCount)/ticksPerSec)", hash, d, laggedOffset)
		}
	}
}

// nowUnix is the clock ActivateObserver reads, called separately from the one the
// assertion compares against so a second boundary between them cannot turn a
// correct implementation into a failure.
func nowUnix() int64 { return time.Now().Unix() }

// The lagged flag is decided by whether the request's passwordHash matches the
// u64 the session published with 0x1D, and by nothing else.
// The three cases are the three branches of that one expression.
func TestTheLaggedFlagIsTheHashComparison(t *testing.T) {
	cases := []struct {
		name           string
		published      uint64
		inRequest      uint64
		wantLagged     bool
		wantNotLagged  bool
	}{
		{name: "nothing published, nothing presented", published: 0, inRequest: 0, wantLagged: true},
		{name: "nothing published, something presented", published: 0, inRequest: 0x1234, wantLagged: true},
		{name: "published, wrong hash", published: publishedHash, inRequest: publishedHash ^ 1, wantLagged: true},
		{name: "published, no hash", published: publishedHash, inRequest: 0, wantLagged: true},
		{name: "published, matching hash", published: publishedHash, inRequest: publishedHash, wantNotLagged: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewSession(1)
			s.SetObserverConfig(2, 8)
			if tc.published != 0 {
				s.SetObservableParameters(1, tc.published, 0, 8)
			}
			conn := &mockConn{}
			if status, _ := s.StartObserve(conn, 0, tc.inRequest); status != 0 {
				t.Fatalf("StartObserve = %d, want 0", status)
			}
			if got := s.observers[0].Lagged; got != tc.wantLagged {
				t.Errorf("entry.Lagged = %v, want %v", got, tc.wantLagged)
			}
		})
	}
}

// The lagged arm of the observer update, end to end: the observer is served a feed
// that stops LagPages short of the end of the store, and the frames inside the
// window are the same ones, in the same order, that a plain observer gets.
//
// The original's rule is the whole of it:
//
//	the store's end is the target when GameStarted or the session has no peers
//	the target is PageCount - LagPages for a lagged entry, else PageCount
//
// The exception is real and is exercised too: once the game has started the
// lagged observer is served the live end like anybody else.
func TestTheLaggedObserverStopsShortOfLive(t *testing.T) {
	const (
		ticksPerSec = 8
		lagSeconds  = 5
		lagPages    = lagSeconds * ticksPerSec // 40
		matchFrames = 100
	)
	build := func() (*Session, *mockConn, *mockConn) {
		s := NewSession(1)
		// Update interval 1, so one pass reaches both entries: the stride counter
		// only visits every interval-th observer per tick.
		s.SetObserverConfig(2, 1)
		s.SetObservableParameters(1, publishedHash, lagSeconds, ticksPerSec)
		s.AddPeer(NewPeer(1, &mockConn{})) // a live peer: the store's end is not the target
		recordAcks(s, matchFrames)
		plain := &mockConn{}
		lagged := &mockConn{}
		// Same session, two requests that differ only in the hash they present:
		// one matching, one not, and that is the whole difference between the two
		// entries. The identifiers have to differ too: they are what the list is
		// keyed on, so two zeros would be one observer.
		if status, _ := s.StartObserve(plain, 1, publishedHash); status != 0 {
			t.Fatalf("StartObserve(plain) = %d, want 0", status)
		}
		if status, _ := s.StartObserve(lagged, 2, publishedHash^1); status != 0 {
			t.Fatalf("StartObserve(lagged) = %d, want 0", status)
		}
		if s.observers[0].Lagged || !s.observers[1].Lagged {
			t.Fatalf("entry flags = %v/%v, want the matching hash first and the wrong one lagged",
				s.observers[0].Lagged, s.observers[1].Lagged)
		}
		if !s.ActivateObserver(plain, ticksPerSec) || !s.ActivateObserver(lagged, ticksPerSec) {
			t.Fatal("both entries should activate")
		}
		plain.msgs, lagged.msgs = nil, nil // drop the two 0x1E answers
		return s, plain, lagged
	}

	s, plain, lagged := build()
	s.updateObserversLocked()
	if got, want := countMsgs(plain.msgs, ackFrameType), matchFrames; got != want {
		t.Fatalf("plain observer got %d frames, want %d", got, want)
	}
	if got, want := countMsgs(lagged.msgs, ackFrameType), matchFrames-lagPages; got != want {
		t.Fatalf("lagged observer got %d frames, want %d (PageCount-LagPages)", got, want)
	}
	// The frames it did get are the FIRST ones, and it must not be handed the
	// most recent LagPages of them, which is the whole point of the flag.
	if n := ackFrameNumber(lastAckBody(t, lagged.msgs)); n != matchFrames-lagPages-1 {
		t.Errorf("lagged observer's last frame is #%d, want #%d: it must stop short of live",
			n, matchFrames-lagPages-1)
	}
	if !bytes.Equal(firstAckBody(t, lagged.msgs), firstAckBody(t, plain.msgs)) {
		t.Error("both observers must be served the same store, from the same first frame")
	}

	// More ticks with no new frames: the lagged observer is already at its target
	// and gets nothing more.
	before := len(lagged.msgs)
	s.updateObserversLocked()
	if len(lagged.msgs) != before {
		t.Errorf("a caught-up lagged observer got %d more messages", len(lagged.msgs)-before)
	}

	// GameStarted (the game has started) lifts the lag: the store's end becomes the
	// target again.
	s.SetStartObservationGameStartedForTest()
	s.updateObserversLocked()
	if n := ackFrameNumber(lastAckBody(t, lagged.msgs)); n != matchFrames-1 {
		t.Errorf("with the game started the lagged observer's last frame is #%d, want #%d", n, matchFrames-1)
	}

	// And with no peers left the same happens (the other half of that condition).
	s2, plain2, lagged2 := build()
	for _, p := range s2.GetPeers() {
		p.Close()
	}
	s2.RemovePeer(1)
	s2.updateObserversLocked()
	if got, want := countMsgs(lagged2.msgs, ackFrameType), matchFrames; got != want {
		t.Errorf("a lagged observer of an empty session got %d frames, want %d", got, want)
	}
	if got, want := countMsgs(plain2.msgs, ackFrameType), matchFrames; got != want {
		t.Errorf("plain observer of an empty session got %d frames, want %d", got, want)
	}
}

// SetStartObservationGameStartedForTest sets GameStarted, the flag the original
// never sets in this server, so the one condition that lifts the lag can be
// exercised. It is a test-only setter because no production path may write it.
func (s *Session) SetStartObservationGameStartedForTest() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.GameStarted = true
}

func firstAckBody(t *testing.T, msgs []mockMsg) []byte {
	t.Helper()
	for _, m := range msgs {
		if m.typ == ackFrameType {
			return m.body
		}
	}
	t.Fatalf("no 0x0A frame in %d messages", len(msgs))
	return nil
}

func lastAckBody(t *testing.T, msgs []mockMsg) []byte {
	t.Helper()
	var last []byte
	for _, m := range msgs {
		if m.typ == ackFrameType {
			last = m.body
		}
	}
	if last == nil {
		t.Fatalf("no 0x0A frame in %d messages", len(msgs))
	}
	return last
}

// The two facts of the observer step, and the second one is the answer to a question
// that was open for two rounds: **does the original serve a freshly activated observer
// in the same tick it activates, or does it wait for its turn at the step?** It waits,
// and there is no arm anywhere in the original that could say otherwise.
//
// The activate handler (reached from the 0x1A case) walks
// the observer list, marks the entry active and builds ONE message, the
// ActivateObserverResponse whose wire type is 30. It never fetches
// frames from the store, and the only two places that do write them to an
// observer are the tick's observer step
// and the reconnect resend. Inside the step there is nothing that
// distinguishes a new arrival: the body opens with the active flag, the
// cursor picks the entries, and the cursor advances on every
// tick whether it visited anybody or not.
//
// So the question is answered and what is left is its arithmetic, which is what this
// test measures: **a freshly activated observer is served within interval+1 ticks and
// never earlier**, for every phase the cursor can be in. Before this test nothing in the
// repository pinned that number, and it is the number the harness's settle has to cover
// (internal/sim/replay.go, settleFor) and the number a fixture's length has to exceed.
func TestAFreshlyActivatedObserverIsServedWithinOneStepPeriod(t *testing.T) {
	const interval = 8 // the original's default, without -observerUpdateIntervalMS

	for phase := 0; phase <= interval; phase++ {
		s := NewSession(1)
		s.SetReconnectable(true)
		s.SetObserverConfig(2, interval)
		recordAcks(s, 5)

		conn := &mockConn{}
		if status, registered := s.StartObserve(conn, 0, 0); status != 0 || !registered {
			t.Fatalf("phase %d: StartObserve = %d, %v", phase, status, registered)
		}
		// Put the cursor at this phase. It only ever takes the values 0..interval
		// (it wraps only when it is STRICTLY greater than the
		// interval), so those are all the phases there are and this loop covers them
		// all. The observer is inactive, so no tick serves it.
		for range phase {
			s.updateObserversLocked()
		}
		if got := s.observerTick; got != phase {
			t.Fatalf("phase %d: the cursor is at %d after %d ticks", phase, got, phase)
		}

		if !s.ActivateObserver(conn, 8) {
			t.Fatalf("phase %d: ActivateObserver did not find the entry", phase)
		}
		// The activation itself carries the 0x1E and nothing else: no frame, because
		// there is no call to the store on that path.
		if n := countMsgs(conn.msgs, ackFrameType); n != 0 {
			t.Fatalf("phase %d: ActivateObserver served %d frame(s); the original's "+
				"handler builds one message, the 0x1E", phase, n)
		}
		conn.msgs = nil

		served, ticks := 0, 0
		for ticks < interval+1 {
			ticks++
			s.updateObserversLocked()
			if n := countMsgs(conn.msgs, ackFrameType); n > served {
				served = n
				break
			}
		}
		if served == 0 {
			t.Fatalf("phase %d: no frame in %d ticks after the activation, and the "+
				"cursor visits index 0 on every counter==0 tick, so %d is the bound",
				phase, ticks, interval+1)
		}
		if ticks == 1 && phase != 0 {
			// Only the phase that is already 0 may be served on the first tick.
			t.Errorf("phase %d: served on the first tick, but the cursor was at %d", phase, phase)
		}
	}
}

// The same bound from the worst phase, and the worst phase is not the one the name
// suggests. The cursor visits index 0 when it is 0, so the longest wait is not from
// `interval` (which wraps to 0 on the very next tick, `:206-209`) but from **1**: from
// there the cursor has to climb back round, which is **interval+1 ticks**, 1,125 ms at
// the default 125 ms period. That is the number the 40.4 flake was about: a recording
// that ends before it never shows a replay frame, and whether it ends before it depends
// on where the session's cursor happened to be.
func TestTheWorstPhaseCostsTheWholePeriod(t *testing.T) {
	const interval = 8

	s := NewSession(1)
	s.SetReconnectable(true)
	s.SetObserverConfig(2, interval)
	recordAcks(s, 5)

	c := &mockConn{}
	if status, _ := s.StartObserve(c, 0, 0); status != 0 {
		t.Fatalf("StartObserve = %d", status)
	}
	// One tick puts the cursor at 1, just past the value that visits index 0. The
	// observer has not activated, so nothing is served and nothing is recorded.
	s.updateObserversLocked()
	if got := s.observerTick; got != 1 {
		t.Fatalf("cursor = %d, want 1", got)
	}
	if !s.ActivateObserver(c, 8) {
		t.Fatal("ActivateObserver did not find the entry")
	}
	c.msgs = nil

	ticks := 0
	for ticks < 3*interval {
		ticks++
		s.updateObserversLocked()
		if countMsgs(c.msgs, ackFrameType) > 0 {
			break
		}
	}
	if ticks != interval+1 {
		t.Fatalf("served after %d ticks, want interval+1 = %d: the cursor has to come "+
			"back round to 0", ticks, interval+1)
	}

	// And from the best phase it is one tick, not zero: the activation serves no
	// frame, so there is no "same tick" to be had.
	s2 := NewSession(1)
	s2.SetReconnectable(true)
	s2.SetObserverConfig(2, interval)
	recordAcks(s2, 5)
	c2 := &mockConn{}
	if status, _ := s2.StartObserve(c2, 0, 0); status != 0 {
		t.Fatalf("StartObserve = %d", status)
	}
	if !s2.ActivateObserver(c2, 8) {
		t.Fatal("ActivateObserver did not find the entry")
	}
	c2.msgs = nil
	s2.updateObserversLocked()
	if n := countMsgs(c2.msgs, ackFrameType); n == 0 {
		t.Error("from cursor 0 the observer is served on the very next tick")
	}
}

func TestUpdateObserversEdges(t *testing.T) {
	// Interval 0 is guarded to 1 (the original would loop forever), and
	// inactive observer entries are skipped.
	s := NewSession(1)
	s.SetReconnectable(true)
	s.SetObserverConfig(2, 0)
	recordAcks(s, 9)

	conn := &mockConn{}
	if status, _ := s.StartObserve(conn, 0, 0); status != 0 {
		t.Fatalf("expected status 0, got %d", status)
	}
	s.updateObserversLocked()
	if len(conn.msgs) != 0 {
		t.Fatal("inactive observer must not receive frames")
	}

	// The 200-frame cap per update and the caught-up marker.
	// With interval 8 the observer is served when the stride counter wraps to 0
	// (every 9 calls).
	s2 := NewSession(1)
	s2.SetReconnectable(true)
	s2.SetObserverConfig(2, 8)
	recordAcks(s2, 251)

	conn2 := &mockConn{msgVersion: 4}
	if status, _ := s2.StartObserve(conn2, 0, 0); status != 0 {
		t.Fatalf("expected status 0, got %d", status)
	}
	if !s2.ActivateObserver(conn2, 8) {
		t.Fatal("ActivateObserver should find the entry")
	}
	conn2.msgs = nil

	for range 9 {
		s2.updateObserversLocked()
	}
	if n := countMsgs(conn2.msgs, ackFrameType); n != 200 {
		t.Fatalf("expected 200 frames after the capped update, got %d", n)
	}

	s2.updateObserversLocked()
	if n := countMsgs(conn2.msgs, ackFrameType); n != 251 {
		t.Fatalf("expected 251 frames total, got %d", n)
	}
	if !hasMsg(conn2.msgs, 0x22) {
		t.Error("caught-up observer should receive EndOfGameDataMessage (type 0x22)")
	}
}

// BenchmarkSessionPeerIDForConn measures the relay's player-slot lookup
// (PeerIDForConn, called on every game-data/checksum ack). Before P4 it scanned
// all session peers (O(peers)); after it is an O(1) inverse-map lookup.
func BenchmarkSessionPeerIDForConn(b *testing.B) {
	for _, peers := range []int{2, 8, 32} {
		b.Run(fmt.Sprintf("peers=%d", peers), func(b *testing.B) {
			s := NewSession(1)
			var last PeerConnection
			for i := 1; i <= peers; i++ {
				conn := &mockConn{}
				s.AddPeer(NewPeer(byte(i), conn))
				last = conn
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if s.PeerIDForConn(last) == 0 {
					b.Fatal("not found")
				}
			}
		})
	}
}

// The RemovePeer path schedules the target peer to disconnect
// 30 s out, recording the reason, and keeps the earliest deadline when one is
// already set. The tick closes it when the deadline passes.

func TestRemovePeerGracefulSchedulesClose(t *testing.T) {
	s := NewSession(1)
	conn := &mockConn{}
	s.AddPeer(NewPeer(1, conn))

	now := time.Now()
	if !s.RemovePeerGracefully(1, 2, 0xCAFEBABE, "kicked", now) {
		t.Fatal("RemovePeerGracefully should find the peer")
	}
	at, reason, ok := s.Peers[1].CloseDeadline()
	if !ok {
		t.Fatal("peer should have a close deadline")
	}
	if d := at.Sub(now); d != 30*time.Second {
		t.Errorf("deadline in %v, want 30s", d)
	}
	if !bytes.Contains([]byte(reason), []byte("reasonCode=3405691582")) {
		t.Errorf("reason = %q, want it to carry the reason code", reason)
	}
	if conn.closed {
		t.Error("RemovePeer must not close the connection immediately")
	}

	// The tick closes it once the deadline passes, not before.
	s.Tick()
	if conn.closed {
		t.Error("peer closed before its deadline")
	}
	s.Peers[1].ScheduleClose(now.Add(-time.Second), "past")
	s.Tick()
	if !conn.closed {
		t.Error("peer should be closed after its deadline passes")
	}
}

func TestRemovePeerGracefulKeepsEarliestDeadline(t *testing.T) {
	s := NewSession(1)
	s.AddPeer(NewPeer(1, &mockConn{}))

	now := time.Now()
	s.RemovePeerGracefully(1, 2, 1, "first", now)
	first, _, _ := s.Peers[1].CloseDeadline()
	// A later proposal does not push the deadline out.
	s.RemovePeerGracefully(1, 2, 2, "second", now.Add(20*time.Second))
	at, reason, _ := s.Peers[1].CloseDeadline()
	if !at.Equal(first) {
		t.Errorf("deadline moved to %v, want it kept at %v", at, first)
	}
	if !bytes.Contains([]byte(reason), []byte("first")) {
		t.Errorf("reason = %q, want the first one kept", reason)
	}
	// An earlier proposal does pull it in: a base 20 s back means a deadline at
	// now+10s, which beats the now+30s already stored.
	s.RemovePeerGracefully(1, 2, 3, "third", now.Add(-20*time.Second))
	at, reason, _ = s.Peers[1].CloseDeadline()
	if d := at.Sub(now); d != 10*time.Second {
		t.Errorf("deadline = %v from now, want +10s (base-20s plus the 30s grace)", d)
	}
	if !bytes.Contains([]byte(reason), []byte("third")) {
		t.Errorf("reason = %q, want the earlier one", reason)
	}
}

// An unknown peer is the case the original tolerates: a RemovePeer that overtakes
// the join is logged, not treated as an error.
func TestRemovePeerGracefulUnknownPeer(t *testing.T) {
	s := NewSession(1)
	if s.RemovePeerGracefully(7, 1, 0, "too early", time.Now()) {
		t.Error("RemovePeerGracefully should report false for an unknown peer")
	}
}

func TestSessionHostFromAddPeerAndIntegrityCheck(t *testing.T) {
	s := NewSession(1)
	conn := &mockConn{}
	s.AddPeer(NewPeer(1, conn))
	s.AddPeer(NewPeer(2, &mockConn{}))

	if s.HostID() != 0 {
		t.Errorf("HostID = %d, want 0 before anything says otherwise", s.HostID())
	}
	if !s.NotePeerAdded(2, 0xAABB, true) {
		t.Fatal("NotePeerAdded should find peer 2")
	}
	if s.HostID() != 2 {
		t.Errorf("HostID = %d, want 2 (the peer flagged isHost)", s.HostID())
	}
	// A peer that is not the host must not clear it.
	s.NotePeerAdded(1, 0xCCDD, false)
	if s.HostID() != 2 {
		t.Errorf("HostID = %d, want it unchanged at 2", s.HostID())
	}
	// An AddPeer for a peer this session does not have is rejected.
	if s.NotePeerAdded(9, 0xEEFF, true) {
		t.Error("NotePeerAdded should reject a peer that is not in the session")
	}
	if s.HostID() != 2 {
		t.Errorf("HostID = %d, want it unchanged at 2", s.HostID())
	}
}

// The session's peer table is keyed by a u64 in the original: AddPeer
// supplies it as globalID, RequestJoin as a trailing u64, and RequestJoinDenied
// names the peer to drop by it.

func TestSessionPeerKeyFromAddPeer(t *testing.T) {
	s := NewSession(1)
	s.AddPeer(NewPeer(1, &mockConn{}))
	s.AddPeer(NewPeer(2, &mockConn{}))

	if s.NotePeerAdded(2, 0x1122334455667788, false) != true {
		t.Fatal("NotePeerAdded should find peer 2")
	}
	if s.Peers[2].Key != 0x1122334455667788 {
		t.Errorf("peer 2 key = %#x, want the globalID from AddPeer", s.Peers[2].Key)
	}
	if p := s.PeerByKey(0x1122334455667788); p == nil || p.ID != 2 {
		t.Errorf("PeerByKey returned %+v, want peer 2", p)
	}
	// A zero globalID must not wipe a key we already have.
	s.NotePeerAdded(2, 0, false)
	if s.Peers[2].Key != 0x1122334455667788 {
		t.Errorf("peer 2 key = %#x, want it kept when AddPeer carries no globalID", s.Peers[2].Key)
	}
	// Peer 1 never got an AddPeer, so it has no key and must not be found.
	if p := s.PeerByKey(0); p != nil {
		t.Errorf("PeerByKey(0) = %+v, want nil (0 is not a key)", p)
	}
}

func TestSessionSetPeerKeyFromRequestJoin(t *testing.T) {
	s := NewSession(1)
	s.AddPeer(NewPeer(1, &mockConn{}))

	if !s.SetPeerKey(1, 0xDEADBEEF) {
		t.Fatal("SetPeerKey should find peer 1")
	}
	if p := s.PeerByKey(0xDEADBEEF); p == nil || p.ID != 1 {
		t.Errorf("PeerByKey = %+v, want peer 1", p)
	}
	if s.SetPeerKey(9, 1) {
		t.Error("SetPeerKey should reject a peer that is not in the session")
	}
}

// RequestJoinDenied schedules the peer named by key to disconnect 30 s out,
// the same mechanism RemovePeer uses.
func TestDenyPeerJoinSchedulesClose(t *testing.T) {
	s := NewSession(1)
	denied := &mockConn{}
	other := &mockConn{}
	s.AddPeer(NewPeer(1, denied))
	s.AddPeer(NewPeer(2, other))
	s.SetPeerKey(2, 0xABCD)

	now := time.Now()
	if !s.DenyPeerJoin(0xABCD, 3, "lobby full", now) {
		t.Fatal("DenyPeerJoin should find the peer with that key")
	}
	if _, _, ok := s.Peers[1].CloseDeadline(); ok {
		t.Error("the wrong peer was scheduled")
	}
	at, reason, ok := s.Peers[2].CloseDeadline()
	if !ok {
		t.Fatal("the denied peer should have a close deadline")
	}
	if d := at.Sub(now); d != 30*time.Second {
		t.Errorf("deadline in %v, want 30s", d)
	}
	if !bytes.Contains([]byte(reason), []byte("lobby full")) {
		t.Errorf("reason = %q, want it to carry the denial reason", reason)
	}
	if other.closed {
		t.Error("DenyPeerJoin must not close the connection immediately")
	}

	if s.DenyPeerJoin(0x1234, 0, "unknown key", now) {
		t.Error("DenyPeerJoin should report false for an unknown key")
	}
}

// THE UPLOAD GATE, and the four things it stops at once.
//
// A 0xFE upload is taken only when the peer exists and, once the session is
// recording, that peer is participating. This is the rule the binary applies
// around the whole block, and it was half-modelled: the sub-message was always
// queued while the participating test was missing, so a peer that joined after
// the frame store was created had its data put into the ack that the server
// never sends.
//
// It is pinned here on all four effects and not only the queue, because the
// refusal has to stop the accepted-sync counter and the last-sync stamp too: a
// peer whose stamps stop moving eventually looks like one that stopped sending,
// and that is a connection closing for the wrong reason.
func TestAnUploadIsTakenOnlyFromAPeerThatExistsAndParticipates(t *testing.T) {
	t.Run("a slot that is not a peer is refused", func(t *testing.T) {
		s := NewSession(1)
		if s.QueueGameData(1, []byte{0x02, 0x00, 0xAA}) {
			t.Error("an upload from a slot with no peer must be refused")
		}
		if s.Ack.Started() {
			t.Error("a refused upload must not start the ack stream")
		}
		if n := s.SyncMessagesAccepted(1); n != 0 {
			t.Errorf("the counter moved to %d on a refused upload, want 0", n)
		}
	})

	t.Run("not participating once recording, refused with every effect stopped", func(t *testing.T) {
		s := NewSession(1)
		s.AddPeer(NewPeer(1, &mockConn{}))
		// The first upload starts the session, and the tick after it marks the
		// peers and creates the frame store.
		if !s.QueueGameData(1, []byte{0x02, 0x00, 0xAA}) {
			t.Fatal("the first upload from a peer of a non-recording session must be taken")
		}
		s.PopAckTick()
		if !s.Recording {
			t.Fatal("the session should be recording after its first tick with data")
		}
		if !s.Peers[1].Participating {
			t.Fatal("peer 1 should be participating: the marks are set when the store is")
		}

		// A peer that arrives after the store exists is never marked.
		late := NewPeer(2, &mockConn{})
		s.AddPeer(late)
		if late.Participating {
			t.Fatal("a peer that joined after the store was created must not be participating")
		}
		stampBefore, stamped := s.Ack.LastSync(2)
		if s.QueueGameData(2, []byte{0x02, 0x00, 0xBB}) {
			t.Error("an upload from a non-participating peer of a recording session must be refused")
		}
		if n := s.SyncMessagesAccepted(2); n != 0 {
			t.Errorf("the counter moved to %d on a refused upload, want 0", n)
		}
		if got, stillSet := s.Ack.LastSync(2); stillSet != stamped || (stamped && !got.Equal(stampBefore)) {
			t.Error("a refused upload must not stamp the peer's last-sync time")
		}
		if !s.Ack.QueueEmpty(2) {
			t.Error("a refused upload must leave the peer's queue empty")
		}

		// The participating peer of the same recording session is unaffected: the
		// guard is per peer and not a session-wide switch.
		if !s.QueueGameData(1, []byte{0x02, 0x00, 0xCC}) {
			t.Error("an upload from a participating peer must still be taken")
		}
		if n := s.SyncMessagesAccepted(1); n != 2 {
			t.Errorf("peer 1's counter = %d, want 2: both of its uploads count", n)
		}
	})

	t.Run("a peer that left is refused the same way", func(t *testing.T) {
		s := NewSession(1)
		s.AddPeer(NewPeer(1, &mockConn{}))
		s.AddPeer(NewPeer(2, &mockConn{}))
		if !s.QueueGameData(1, []byte{0x02, 0x00, 0xAA}) {
			t.Fatal("the first upload must be taken")
		}
		s.PopAckTick()
		if !s.LeaveSimulation(2) {
			t.Fatal("LeaveSimulation should have found peer 2")
		}
		if s.QueueGameData(2, []byte{0x02, 0x00, 0xBB}) {
			t.Error("an upload from a peer that left the simulation must be refused")
		}
		if n := s.SyncMessagesAccepted(2); n != 0 {
			t.Errorf("the counter moved to %d after a leave, want 0", n)
		}
	})
}
