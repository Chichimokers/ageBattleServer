package session

import (
	"errors"
	"fmt"
	"log"
	"sync"
)

// ErrSessionErased is AddPeer's refusal of a session that is no longer in the
// session map: its last connection closed after the caller was handed it.
var ErrSessionErased = errors.New("session was erased")

// SessionKey identifies a session by the pair (titleID, sessionID).
//
// The session map is an ordered tree keyed on that pair, and its comparator puts
// titleID first, compared as a **signed** 16-bit value. Keying by sessionID alone
// merges sessions the protocol keeps apart, which matters now that one server
// process can host sessions from several titles.
type SessionKey struct {
	TitleID   int16
	SessionID int64
}

type Manager struct {
	sessions map[SessionKey]*Session
	// peerSession is the O(1) relay route: session of each peer connection,
	// kept in sync by AddPeer/RemovePeer/ReconnectPeer/HandleDisconnect/RemoveSession.
	// Peer lifecycle must go through the manager (Session.AddPeer/RemovePeer/
	// ReconnectPeer alone bypass the index).
	peerSession map[PeerConnection]*Session
	mu          sync.RWMutex
	nextID      int64

	// maxPagesPerMatch is the per-session frame-store block count
	// (ceil(peers * value/8), so 8 with the default of 0) and
	// pageSize is the block size in bytes (the -pageSize setting, 65536 by
	// default). maxSessions is the maximum number of concurrent sessions
	// (matches): new sessions are denied once the limit is reached. 0 is
	// unlimited for maxSessions.
	maxPagesPerMatch uint
	pageSize         uint
	maxSessions      uint
}

func NewManager(maxPagesPerMatch, maxSessions uint) *Manager {
	return NewManagerWithPageSize(maxPagesPerMatch, maxSessions, 0)
}

// NewManagerWithPageSize is NewManager with the -pageSize block size the frame
// store is built from. 0 means the default, 65536.
func NewManagerWithPageSize(maxPagesPerMatch, maxSessions, pageSize uint) *Manager {
	return &Manager{
		sessions:         make(map[SessionKey]*Session),
		peerSession:      make(map[PeerConnection]*Session),
		nextID:           1,
		maxPagesPerMatch: maxPagesPerMatch,
		pageSize:         pageSize,
		maxSessions:      maxSessions,
	}
}

// createSessionInternal creates a Session wired to this Manager's frame-store
// sizing. Callers must hold m.mu.
func (m *Manager) createSessionInternal(k SessionKey) *Session {
	sess := NewSession(k.SessionID)
	sess.titleID = k.TitleID
	sess.key = k
	sess.maxPagesPerMatch = int(m.maxPagesPerMatch)
	sess.pageSize = int(m.pageSize)
	return sess
}

// sessionsAtLimit reports whether the concurrent session cap is reached.
// Callers must hold m.mu.
func (m *Manager) sessionsAtLimit() bool {
	return m.maxSessions > 0 && len(m.sessions) >= int(m.maxSessions)
}

func (m *Manager) CreateSession() *Session {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.sessionsAtLimit() {
		log.Printf("Max session limit reached (%d), denying new session", m.maxSessions)
		return nil
	}

	k := SessionKey{SessionID: m.nextID}
	m.nextID++

	sess := m.createSessionInternal(k)
	m.sessions[k] = sess

	return sess
}

func (m *Manager) GetSession(k SessionKey) *Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[k]
}

// GetOrCreateSession returns the session with the given key, creating it when it
// does not exist yet. The key is used exactly as given: session ID 0 is an
// ordinary key, not a request for a generated one. A caller that needs a
// generated ID asks CreateSession, so the manager never rewrites the key a
// client sent.
// Returns nil when a new session would exceed the concurrent session limit.
func (m *Manager) GetOrCreateSession(k SessionKey) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()

	if sess, ok := m.sessions[k]; ok {
		return sess
	}

	if m.sessionsAtLimit() {
		log.Printf("Max session limit reached (%d), denying new session", m.maxSessions)
		return nil
	}

	sess := m.createSessionInternal(k)
	m.sessions[k] = sess

	return sess
}

func (m *Manager) RemoveSession(k SessionKey) {
	m.mu.Lock()
	sess, ok := m.sessions[k]
	if ok {
		sess.ForEachPeer(func(p *Peer) {
			delete(m.peerSession, p.Conn)
		})
		delete(m.sessions, k)
	}
	m.mu.Unlock()
	if !ok {
		return
	}
	// The connections close after the manager lock is released: a WebSocket
	// connection's Close reaches HandleDisconnect, which takes that lock.
	sess.Close()
	log.Printf("Session %d (titleID %d) removed", k.SessionID, k.TitleID)
}

func (m *Manager) SessionCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

func (m *Manager) GetSessions() []*Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	return sessions
}

// SnapshotSessions fills dst with the current sessions, reusing dst's backing
// array when large enough, and returns the slice to iterate. It is intended for
// the server main loop, which reuses the same buffer each tick.
func (m *Manager) SnapshotSessions(dst []*Session) []*Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if cap(dst) < len(m.sessions) {
		dst = make([]*Session, len(m.sessions))
	}
	dst = dst[:len(m.sessions)]
	i := 0
	for _, sess := range m.sessions {
		dst[i] = sess
		i++
	}
	return dst
}

// AddPeer registers a session peer and indexes it for O(1) relay routing.
// Sessions must be mutated through the manager (Session.AddPeer alone does not
// touch the index).
//
// A connection can belong to one session at a time: a conn still mapped to
// another session is first unregistered from it, so no stale index entry
// survives. Adding an ID that is already taken by a different live connection
// is rejected instead of silently replacing that peer.
func (m *Manager) AddPeer(sess *Session, peer *Peer) error {
	m.mu.Lock()

	if m.sessions[sess.key] != sess {
		m.mu.Unlock()
		return ErrSessionErased
	}
	var erased *Session
	if prev := m.peerSession[peer.Conn]; prev != nil && prev != sess {
		prev.RemovePeer(prev.PeerIDForConn(peer.Conn))
		erased = m.eraseIfVacantLocked(prev)
	}
	if existing := sess.GetPeer(peer.ID); existing != nil {
		if existing.Conn == peer.Conn {
			// Idempotent re-add of the same connection: refresh the mapping.
			m.peerSession[peer.Conn] = sess
			m.mu.Unlock()
			m.closeErased(erased)
			return nil
		}
		m.mu.Unlock()
		m.closeErased(erased)
		return fmt.Errorf("session %d: peer ID %d already taken by another connection", sess.ID, peer.ID)
	}
	m.peerSession[peer.Conn] = sess
	sess.AddPeer(peer)
	m.mu.Unlock()
	// The previous session was left with no member and no observer: close it
	// after the manager lock is gone, because closing a connection reaches
	// HandleDisconnect, which takes that lock again.
	m.closeErased(erased)
	return nil
}

// RemovePeer unregisters a session peer and its relay route.
func (m *Manager) RemovePeer(sess *Session, id byte, conn PeerConnection) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.peerSession, conn)
	sess.RemovePeer(id)
}

// FindSessionByPeer returns the session the peer connection belongs to, or nil.
// The peerSession index makes this O(1) regardless of how many sessions or
// peers are live; it is the relay route for every relayed message.
func (m *Manager) FindSessionByPeer(conn PeerConnection) *Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.peerSession[conn]
}

// HandleLeaveSimulation runs the LeaveSimulation message (0x1F) of the peer behind
// conn, which is Session.LeaveSimulation and nothing else: the peer stays in the
// session and the session stays in the map.
//
// It used to unregister the peer and end the session when it was left empty, which
// is what the message's name suggests and what the protocol does not do. The
// handler empties the peer's two queues, clears its participating mark and
// possibly ends the recording; it does not touch the session's peer list. Only
// the disconnect path removes a peer, and it erases the session when it is
// empty. That is HandleDisconnect, which Server.RemoveClient calls.
func (m *Manager) HandleLeaveSimulation(conn PeerConnection) {
	m.mu.RLock()
	sess := m.peerSession[conn]
	m.mu.RUnlock()
	if sess == nil {
		return
	}
	sess.LeaveSimulation(sess.PeerIDForConn(conn))
}

// ReconnectPeer moves a session peer onto a new connection and moves its relay
// route with it: a reconnect swaps only the socket, so the peer keeps its slot
// and its session, and everything the new connection sends is that peer's
// traffic. The replaced connection loses its route, so closing it later takes
// nothing out of the session. It reports the replaced connection, which the
// caller closes.
func (m *Manager) ReconnectPeer(sess *Session, id byte, conn PeerConnection, secret uint64) (PeerConnection, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := sess.ReconnectPeer(id, conn, secret)
	if !ok {
		return nil, false
	}
	if old != nil && m.peerSession[old] == sess {
		delete(m.peerSession, old)
	}
	m.peerSession[conn] = sess
	return old, true
}

// HandleDisconnect is the close path of a connection, and the only path that
// takes a peer out of its session. observed is the session the connection
// observes, or nil.
//
// The connection's peer leaves its session at once: the slot, the join order and
// both queue entries go (Session.RemovePeer, which also ends a recording match
// when no member left is in the game). Its observer entry leaves the observed
// session. A session is erased only when it is vacant, with no member and no
// observer left; until then the observer step serves its observers the rest of
// the store and the end-of-game marker. A connect that was handed the session
// before it was erased gets ErrSessionErased from AddPeer and joins the live one.
func (m *Manager) HandleDisconnect(conn PeerConnection, observed *Session) {
	m.mu.Lock()
	var erased []*Session
	member := m.peerSession[conn]
	delete(m.peerSession, conn)
	if member != nil {
		if id := member.PeerIDForConn(conn); id != 0 {
			member.RemovePeer(id)
		}
		if s := m.eraseIfVacantLocked(member); s != nil {
			erased = append(erased, s)
		}
	}
	if observed != nil {
		observed.RemoveObserverConn(conn)
		if s := m.eraseIfVacantLocked(observed); s != nil && (len(erased) == 0 || erased[0] != s) {
			erased = append(erased, s)
		}
	}
	m.mu.Unlock()
	// Closing the connections reaches HandleDisconnect again, so it runs after
	// the manager lock is released.
	for _, s := range erased {
		s.Close()
	}
}

// RemoveSessionIfVacant erases sess when it has no member and no observer left.
// It is the stop-watching path's cleanup: a session that lost its last observer
// is vacant even though no connection is closing, and nothing else would take it
// out of the map.
func (m *Manager) RemoveSessionIfVacant(sess *Session) {
	if sess == nil {
		return
	}
	m.mu.Lock()
	erased := m.eraseIfVacantLocked(sess)
	m.mu.Unlock()
	m.closeErased(erased)
}

// closeErased closes a session that eraseIfVacantLocked took out of the map.
// Callers must not hold m.mu: a connection's Close reaches HandleDisconnect,
// which takes it. nil is a no-op.
func (m *Manager) closeErased(sess *Session) {
	if sess != nil {
		sess.Close()
	}
}

// eraseIfVacantLocked erases sess from the session map when it has no member and
// no observer left, and returns it so the caller can close it once the manager
// lock is gone. A session that is no longer the one in the map under its key is
// left alone. Callers must hold m.mu.
func (m *Manager) eraseIfVacantLocked(sess *Session) *Session {
	if m.sessions[sess.key] != sess || !sess.Vacant() {
		return nil
	}
	delete(m.sessions, sess.key)
	log.Printf("Session %d (titleID %d) removed: no member and no observer left", sess.key.SessionID, sess.key.TitleID)
	return sess
}
