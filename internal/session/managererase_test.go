package session

import (
	"errors"
	"testing"
	"time"
)

// TestAddPeerRefusesAnErasedSession covers the window between a connect's
// GetOrCreateSession and its AddPeer: the session's last member closes in
// between and the session is erased. AddPeer must refuse the erased session, so
// the connect can fetch the live one, instead of routing the joiner into a
// session no later connect will find.
func TestAddPeerRefusesAnErasedSession(t *testing.T) {
	m := NewManager(0, 0)
	k := SessionKey{SessionID: 9}
	s := m.GetOrCreateSession(k)
	last := &mockConn{}
	if err := m.AddPeer(s, NewPeer(1, last)); err != nil {
		t.Fatal(err)
	}

	handed := m.GetOrCreateSession(k)
	m.HandleDisconnect(last, nil)
	joiner := &mockConn{}
	err := m.AddPeer(handed, NewPeer(handed.NextPeerID(), joiner))
	if !errors.Is(err, ErrSessionErased) {
		t.Fatalf("AddPeer on an erased session = %v, want ErrSessionErased", err)
	}
	if m.FindSessionByPeer(joiner) != nil {
		t.Fatal("the joiner was routed to the erased session")
	}
}

// reentrantConn is a connection whose Close reaches back into the manager, as a
// WebSocket connection's does (Close -> RemoveClient -> HandleDisconnect).
type reentrantConn struct {
	mockConn
	onClose func()
}

func (c *reentrantConn) Close() error {
	c.closed = true
	if c.onClose != nil {
		c.onClose()
	}
	return nil
}

// TestAddPeerErasesThePreviousSessionOutsideTheLock covers N-01: a connection
// moving to another session leaves its previous session with no member and no
// observer, and that session has to be erased. Closing it under the manager lock
// would deadlock, because the connection's Close re-enters HandleDisconnect.
func TestAddPeerErasesThePreviousSessionOutsideTheLock(t *testing.T) {
	m := NewManager(0, 0)
	oldKey := SessionKey{SessionID: 1}
	newKey := SessionKey{SessionID: 2}
	old := m.GetOrCreateSession(oldKey)
	live := m.GetOrCreateSession(newKey)
	conn := &reentrantConn{}
	conn.onClose = func() { m.HandleDisconnect(conn, nil) }
	if err := m.AddPeer(old, NewPeer(1, conn)); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- m.AddPeer(live, NewPeer(1, conn))
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("AddPeer: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("AddPeer did not return within 3 s: it closed the empty previous session under the manager lock")
	}
	if m.GetSession(oldKey) != nil {
		t.Error("the previous session is still in the map after its last peer moved")
	}
	if m.FindSessionByPeer(conn) != live {
		t.Error("the connection is not routed to its new session")
	}
}

// TestAddPeerKeepsThePreviousSessionWithAnObserver is the other half of N-01: a
// session with no member but still an observer is not empty and has to survive.
func TestAddPeerKeepsThePreviousSessionWithAnObserver(t *testing.T) {
	m := NewManager(0, 0)
	old := m.GetOrCreateSession(SessionKey{SessionID: 1})
	live := m.GetOrCreateSession(SessionKey{SessionID: 2})
	conn := &mockConn{}
	observer := &mockConn{}
	if err := m.AddPeer(old, NewPeer(1, conn)); err != nil {
		t.Fatal(err)
	}
	old.SetObserverConfig(4, 1)
	old.SetReconnectable(true)
	recordAcks(old, 600)
	if status, registered := old.StartObserve(observer, 1, 0); status != 0 || !registered {
		t.Fatalf("StartObserve = %d/%v, want 0/true", status, registered)
	}
	if err := m.AddPeer(live, NewPeer(1, conn)); err != nil {
		t.Fatal(err)
	}
	if m.GetSession(SessionKey{SessionID: 1}) == nil {
		t.Error("a session with an observer was erased when its last player moved")
	}
}

// TestGetOrCreateSessionUsesZeroAsAKey covers N-05: 0 is an ordinary session ID
// and not a request for a generated one.
func TestGetOrCreateSessionUsesZeroAsAKey(t *testing.T) {
	m := NewManager(0, 0)
	zero := m.GetOrCreateSession(SessionKey{SessionID: 0})
	if zero == nil {
		t.Fatal("GetOrCreateSession(0) returned nil")
	}
	if zero.ID != 0 {
		t.Fatalf("GetOrCreateSession(0) created session %d, want 0", zero.ID)
	}
	if again := m.GetOrCreateSession(SessionKey{SessionID: 0}); again != zero {
		t.Error("a second lookup of session 0 did not return the same session")
	}
	if n := m.SessionCount(); n != 1 {
		t.Errorf("SessionCount() = %d, want 1", n)
	}
}

// TestRemoveSessionClosesOutsideTheManagerLock checks that RemoveSession does not
// close the session's connections while it holds the manager lock: a close that
// calls HandleDisconnect would wait on that lock for good.
func TestRemoveSessionClosesOutsideTheManagerLock(t *testing.T) {
	m := NewManager(0, 0)
	s := m.CreateSession()
	conn := &reentrantConn{}
	conn.onClose = func() { m.HandleDisconnect(conn, nil) }
	if err := m.AddPeer(s, NewPeer(1, conn)); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		m.RemoveSession(s.Key())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("RemoveSession did not return within 3 s: a close re-entered the manager lock")
	}
	if !conn.closed {
		t.Error("the peer's connection was not closed")
	}
	if m.GetSession(s.Key()) != nil {
		t.Error("the session is still in the map")
	}
}
