package session

import (
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/luskaner/ageBattleServer/internal/protocol"
)

// PeerConnection is the transport surface a session peer needs. Framing is the
// transport's job, never the caller's: the bsPort client prefixes a big-endian
// u16 length, the WebSocket client sends the bare [type][body] payload, and both
// are correct for their own port. Building frames here (as observerFrame and
// Session.Broadcast used to) is what put a length prefix on the WebSocket
// observer stream.
//
// SetPeerIdentifier and PeerIdentifier carry the u64 the observer list is keyed
// on. That identifier belongs to the connection and not to the observer entry,
// which is observable in three ways; see Session.StartObserve for the argument.
//
// ClientMessageVersion carries the version the client announced in its
// LobbyConnect, and it is on the connection for the same reason. It is written
// once, by the LobbyConnect handler, and read live from the observer's own
// connection when deciding whether to send it the end-of-game marker. So a client
// that never sent a LobbyConnect has version 0 and is never sent the marker,
// while one that did keeps its announced version for as long as the connection
// lives -- including after it becomes an observer.
type PeerConnection interface {
	SendMessage(msgType byte, body []byte) error
	Close() error
	RemoteAddr() string
	SetPeerIdentifier(v uint64)
	PeerIdentifier() uint64
	ClientMessageVersion() uint16
}

type Peer struct {
	ID       byte
	Conn     PeerConnection
	Session  *Session
	LastSeen time.Time
	// Key is the peer's u64 identity in the session's peer table. The table is
	// keyed by a u64 and AddPeer carries it as globalID, RequestJoin as a trailing
	// u64, and RequestJoinDenied as the peer it wants removed.
	Key uint64

	// Secret is the per-peer credential the client publishes in its
	// LobbyConnect, and it is how a peer is found again on a reconnect: the
	// reconnect path looks the peer up by this value and, on a hit, moves it onto
	// the new connection and replays its stored frames. It is never logged: it is
	// a credential.
	Secret uint64

	// ClientMessageVersion is the version the peer announced in its
	// LobbyConnect handshake. It is kept here as well as on the connection
	// because both version gates read it: the host-only Dropout forward and the
	// end-of-game marker. This server relays the Dropout as an unhandled type,
	// so the end-of-game gate is the only live one; it is read from the
	// CONNECTION (PeerConnection.ClientMessageVersion) because the observer
	// entries carry no version of their own, and this copy is kept because it is
	// the same value the connect handler read.
	ClientMessageVersion uint16

	// TimeoutOverride is the peer's LobbyConnect timeout override in ms. It
	// replaces the 60 s sync-timeout default for that peer. It is only filled
	// from the -allowTimeoutOverride option, which is why it is 0 for most peers.
	TimeoutOverride uint32

	// Participating records whether this peer is in the started game. The session
	// tick sets it on every peer at the moment it creates the frame store, the
	// reconciliation and the sync-timeout check both skip the peers without it,
	// and LeaveSimulation clears it for the leaver. The last clear is what ends
	// the recording: see Session.LeaveSimulation.
	Participating bool
	mu            sync.Mutex

	// closeAt is the scheduled disconnect time. A removal request sets it to
	// now+30s together with the reason, and keeps the earliest deadline when one
	// is already set; the tick closes the peer when it passes. A RemovePeer from
	// another peer only schedules this, it does not send a notice and does not
	// close anything itself.
	closeAt     time.Time
	closeReason string

	// syncMessagesAccepted counts the sync messages taken off this peer,
	// including the ones dropped because the peer's queue was full. It does not
	// count the two ways a message arrives and does not count: a peer that is not
	// a member, and a peer the recording gate refuses (SESSION-JOURNAL.md 32.4
	// records the second as a divergence).
	//
	// The only reader is the reconnect: the LobbyConnect handler clones the
	// client's own message and overwrites its dataReceivedCount with this counter
	// before echoing it back, so the client learns how many of its uploads the
	// server took. Atomic because it is written from the message-dispatch path
	// and read from the connect path.
	syncMessagesAccepted atomic.Uint32
}

func NewPeer(id byte, conn PeerConnection) *Peer {
	return &Peer{
		ID:       id,
		Conn:     conn,
		LastSeen: time.Now(),
	}
}

func (p *Peer) SendMessage(msgType byte, body []byte) error {
	return p.Conn.SendMessage(msgType, body)
}

// ScheduleClose sets the peer's disconnect deadline, keeping the earlier of the
// new and existing deadlines.
func (p *Peer) ScheduleClose(at time.Time, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closeAt.IsZero() && p.closeAt.Before(at) {
		return
	}
	p.closeAt = at
	p.closeReason = reason
}

// ScheduleCloseIfNone schedules a disconnect only when none is already pending,
// and reports whether it did.
//
// There are two schedulers and they differ in exactly this way. The one used by
// RemovePeer and RequestJoinDenied keeps the earliest of the new and existing
// deadlines. The one used by the sync-timeout check does nothing at all if a
// deadline already exists.
func (p *Peer) ScheduleCloseIfNone(at time.Time, reason string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closeAt.IsZero() {
		return false
	}
	p.closeAt = at
	p.closeReason = reason
	return true
}

// CloseDeadline reports the scheduled disconnect and its reason.
func (p *Peer) CloseDeadline() (time.Time, string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closeAt, p.closeReason, !p.closeAt.IsZero()
}

func (p *Peer) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Conn != nil {
		return p.Conn.Close()
	}
	return nil
}

// SyncMessagesAccepted reads the peer's accepted-sync counter. See the field's
// comment for what it counts and who reads it.
func (p *Peer) SyncMessagesAccepted() uint32 {
	return p.syncMessagesAccepted.Load()
}

// NoteSyncMessageFrom counts one sync message taken from peerID and reports
// whether there was a peer to count it against. A peer that is not a member is
// not counted.
//
// **The game-data path does not use this.** It is the counting half on its own,
// kept for a caller that has already decided the upload counts, and
// Session.QueueGameData is the one that decides. Counting there and asking here
// would be two places holding the same rule.
func (s *Session) NoteSyncMessageFrom(peerID byte) bool {
	p := s.GetPeer(peerID)
	if p == nil {
		return false
	}
	p.syncMessagesAccepted.Add(1)
	return true
}

// SyncMessagesAccepted is Peer.SyncMessagesAccepted by slot, for the reconnect
// echo, which has the peer and not the connection.
func (s *Session) SyncMessagesAccepted(peerID byte) uint32 {
	p := s.GetPeer(peerID)
	if p == nil {
		return 0
	}
	return p.SyncMessagesAccepted()
}

// ObserverEntry is an entry of the session's observer list: the connection, the
// observer's progress through the frame store, an active flag (set by
// ActivateObserver) and a lagged flag (set at StartObserve).
//
// The entry carries NO copy of the observer's identity, and that is the point.
// Both lookups dereference the connection to read the identifier from it, so the
// identity follows the connection, and a connection that is sent a second
// StartObserve renames every entry pointing at it. See Session.StartObserve.
type ObserverEntry struct {
	Client   PeerConnection
	Progress int
	Active   bool
	Lagged   bool
	// SendFailures counts consecutive failed page deliveries; when it reaches
	// maxObserverSendFailures the observer is dropped from the list (a stalled
	// observer would otherwise block the server's main loop on every write).
	SendFailures int
}

// maxObserverSendFailures is the consecutive failed deliveries after which an
// observer is considered unreachable and removed from the session's observer
// list.
const maxObserverSendFailures = 3

// peerSlots is the session's peer table, and it is an ARRAY indexed by the
// player slot. A slot table has two consequences that a map cannot state:
//
//   - A lookup is an array load, not a hash. Session.Peers was 17% of a tick's CPU
//     (SESSION-JOURNAL.md 39.7: 13.4% of lookups and 4.2% of the walk), and the
//     walk is now peerOrder, which the walk below explains.
//   - The membership has no order of its own, so every walk has to be told which
//     order it wants. There is exactly one answer to give, and it is peerOrder.
//
// 256 slots is 2 KiB of pointers per session, and a session's peers are capped
// well below that (maxSessions x players). peerOrder is what says how many of the
// slots are occupied: AddPeer appends and RemovePeer erases, and nothing else
// writes here.
type peerSlots [256]*Peer

// peerSlotCount is the size of the peer table, which is the domain of a peer id:
// a byte, and 0 is reserved.
const peerSlotCount = 256

type Session struct {
	ID         int64
	// Peers is the peer table, indexed by player slot. Indexing it answers
	// "is there a peer in this slot"; WALKING it is not how anything reads the
	// peers, because a slot table has no order: use ForEachPeer or GetPeers, which
	// walk peerOrder (join order, the order the game-data queue is in).
	Peers      peerSlots
	nextPeerID byte
	mu         sync.RWMutex
	closed     bool
	createdAt  time.Time

	// key and titleID are the session's identity in the manager's map: the pair
	// (titleID, sessionID), with titleID compared as a signed 16-bit value.
	key     SessionKey
	titleID int16

	// peerByConn is the inverse peer lookup (conn -> peer). It makes the relay
	// slot lookup PeerIDForConn O(1) instead of scanning Peers on every
	// game-data/checksum ack. Kept in sync by AddPeer/RemovePeer.
	peerByConn map[PeerConnection]*Peer

	// peerOrder is the order AddPeer was called in, which is the order the
	// game-data queue is in: one record per peer is appended when it joins and
	// erased when it disconnects. The 0x0A's pending list and its flushed
	// sub-messages come out of that queue in this order, so with ids that were
	// not handed out in join order it is not the slot order. It is also the only
	// walk of the peers there is: see peerSlots.
	peerOrder []byte

	// Observer system fields. Their values come from the session constructor
	// arguments, from SetObservableParamatersMessage (0x1D) and from the tick.
	Reconnectable          bool // from the 0x1D first u32, read as "is reconnectable"
	Recording              bool // set when the frame store is created
	GameStarted            bool // set by the step that also clears Recording
	PageCount              int  // the frame store's frame counter
	LagPages               int  // client lag in pages, from 0x1D; not parsed by the Go lobby connect
	MaxObservers           int  // the operator's observer limit
	ObserverUpdateInterval int  // in simulation ticks
	observers              []*ObserverEntry
	observerTick           int    // stride counter
	observableHash         uint64 // the u64 players publish with 0x1D
	observableWindow       int    // the frame store's frame cap; -1 disables it
	DesyncCount            int    // incremented per broadcast desync notice
	replay                 *Replay
	Ack                    AckState // session-wide EndOfGameData (0x0A) tick state

	// hostID is the peer the clients treat as the simulation host. It is set when
	// an IntegrityCheck proposes one with migrateState 0, and the proxy relay
	// uses it to decide whether a relayed message goes only to the host. 0 means
	// unknown.
	hostID byte

	// maxPagesPerMatch is the operator's -maxPagesToStorePerSession as the
	// frame-store block count: the store keeps ceil(peers * value/8) blocks of
	// pageSize bytes. 0 is the default, f = 1.0, so 8 blocks.
	// Set by the Manager at session creation, together with pageSize.
	maxPagesPerMatch int
	pageSize         int
}

func NewSession(id int64) *Session {
	return &Session{
		ID:            id,
		peerByConn:    make(map[PeerConnection]*Peer),
		nextPeerID:    1, // player slots are 1-based (peer ID 0 is reserved)
		createdAt:     time.Now(),
		Reconnectable: true,
	}
}

func (s *Session) NewPeer(id byte, conn PeerConnection) *Peer {
	return NewPeer(id, conn)
}

func (s *Session) ID64() int64 {
	return s.ID
}

func (s *Session) NextPeerID() byte {
	s.mu.Lock()
	defer s.mu.Unlock()

	id := s.nextPeerID
	s.nextPeerID++
	if s.nextPeerID == 0 {
		s.nextPeerID = 1
	}
	return id
}

func (s *Session) AddPeer(p *Peer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p.Session = s
	s.Peers[p.ID] = p
	s.peerByConn[p.Conn] = p
	// A record per peer in the queue, in join order. A peer id that is already a
	// member is not appended twice: the queue only ever grows through the path
	// that checks the peer table slot is free first.
	if _, ok := s.peerOrderIndex(p.ID); !ok {
		s.peerOrder = append(s.peerOrder, p.ID)
	}
}

// peerOrderIndex reports whether id is already in the join order. The caller
// holds the lock.
func (s *Session) peerOrderIndex(id byte) (int, bool) {
	for i, v := range s.peerOrder {
		if v == id {
			return i, true
		}
	}
	return 0, false
}

// RemovePeer takes a member out of the session, which is what the disconnect
// does: the slot is cleared, the peer leaves the join order, and its two queue
// entries go with it. If the session is recording and no member left is in the
// started game, the match is over, exactly as after the last LeaveSimulation.
func (s *Session) RemovePeer(id byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.Peers[id]; p != nil {
		delete(s.peerByConn, p.Conn)
	}
	s.Peers[id] = nil
	if i, ok := s.peerOrderIndex(id); ok {
		s.peerOrder = append(s.peerOrder[:i], s.peerOrder[i+1:]...)
		s.endMatchIfNoneActiveLocked()
	}
	// The disconnect erases the peer's two queue entries (the game-data one and the
	// checksum one) and destroys whatever the peer had uploaded and not yet been
	// flushed. Nothing that peer queued ever reaches the wire.
	s.Ack.DropPeer(id)
}

// endMatchIfNoneActiveLocked marks the match as over when the session is
// recording and no member is still in the started game: Recording goes down and
// GameStarted goes up in the same step. The two ways a player leaves the game
// share it, LeaveSimulation and the disconnect. The caller holds the lock.
func (s *Session) endMatchIfNoneActiveLocked() {
	if !s.Recording {
		return
	}
	// The walk is peerOrder and not the peer table: a slot table has no order,
	// and this answer is the same either way, because it is an existential over
	// the peers and not a walk whose order anything can see.
	for _, id := range s.peerOrder {
		if q := s.Peers[id]; q != nil && q.Participating {
			return
		}
	}
	s.Recording = false
	s.GameStarted = true
}

// Vacant reports whether the session has no member and no observer left, the
// one state in which the disconnect path erases it. A session whose players
// are gone lives on while anybody still watches it.
func (s *Session) Vacant() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.peerOrder) == 0 && len(s.observers) == 0
}

func (s *Session) GetPeer(id byte) *Peer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Peers[id]
}

// PeerIDForConn returns the session peer ID (1-based player slot) of the
// connection, or 0 when it is not a member of the session.
func (s *Session) PeerIDForConn(conn PeerConnection) byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if p := s.peerByConn[conn]; p != nil {
		return p.ID
	}
	return 0
}

// QueueGameData queues one game-data sub-message for the given player slot, and
// counts it against the peer's accepted-sync total. It reports whether the upload
// was taken.
//
// A `0xFE` upload is taken only when the peer exists AND **the session is not
// recording or that peer is participating**. Both halves matter and neither is
// ours:
//
//   - The peer has to be in the session at all. An upload from a slot that is
//     not a peer is not counted either.
//   - Once the session has a frame store, a peer that is **not** participating is
//     skipped. That is a peer that joined after the store was created — the
//     participating marks are set once, when the store is — or one that sent
//     LeaveSimulation.
//
// When the upload is refused, **four** things do not happen and not just the
// first: the sub-message is not queued, the accepted-sync counter does not move,
// the peer is not marked as having sent data, and its last-sync time is not
// stamped. That last one is why the refusal matters beyond the ack body: a peer
// whose stamps stop moving eventually looks like one that stopped sending.
//
// The counter is here and not in the caller's hands for that reason: it is one of
// the four, and a caller that increments it before asking is what this used to do.
func (s *Session) QueueGameData(slot byte, lenPayload []byte) bool {
	p := s.GetPeer(slot)
	if p == nil {
		return false
	}
	s.mu.RLock()
	recording := s.Recording
	s.mu.RUnlock()
	if recording && !p.Participating {
		return false
	}
	p.syncMessagesAccepted.Add(1)
	s.Ack.QueueDataSub(slot, lenPayload)
	return true
}

// QueueChecksumEntries queues the checksum entries for the next tick.
func (s *Session) QueueChecksumEntries(slot byte, entries []byte) {
	s.Ack.QueueEntries(slot, entries)
}

// PopAckTick builds the next tick's 0x0A body (nil until game data has started),
// handing it the session's game-data queue records so the pending-peer tail lists
// the peers that went quiet, and then runs the cross-peer checksum reconciliation.
//
// The order matters and is the opposite of what an earlier revision of this code
// did. The 0x0A goes to every peer first and only then does the reconciliation
// run, in a loop of at most ten rounds. The merged entries therefore reach the
// peers on the *following* tick.
//
// notices holds one desync notice body per disagreeing round, in round order
// (AckState.DesyncNotice); the caller sends each as a GameData (0x09) message to
// every peer, after the ack. The loop breaks when reconciliation reports that
// there is nothing left to do, and each disagreeing round is counted.
//
// Once the session has ended (see LeaveSimulation) there is no ack at all. That
// test runs before anything is built, so with the flag set the tick stops at the
// top of the frame-building block on every tick. Observers keep being served,
// because the observer update runs before that and reads the store, which is not
// destroyed.
func (s *Session) PopAckTick() (ack []byte, notices [][]byte) {
	s.mu.RLock()
	ended := s.GameStarted
	s.mu.RUnlock()
	if ended {
		return nil, nil
	}
	// The store and the participating marks go up BEFORE the body is built, and the
	// pending list filters on the marks, so the very first ack of a match is
	// built with the marks already set. Building it first and marking afterwards
	// left that one ack with an empty pending list.
	if s.Ack.Started() {
		s.ensureRecording()
	}
	entries := s.queueEntries(time.Now())

	ack = s.Ack.PopTick(entries)
	if ack != nil {
		// The store is written before the message goes out. An observer that is
		// already on the list is served from this store by the session tick, not by
		// a second copy of the ack.
		s.RecordFrame(protocol.MsgEndOfGameData, ack)
	}

	for range reconcileMaxRounds {
		switch s.Ack.ReconcileChecksums(entries) {
		case reconcileStop:
			return ack, notices
		case reconcileDesynced:
			notices = append(notices, s.Ack.DesyncNotice())
			s.DesyncCount++
		}
	}
	return ack, notices
}

// queueEntries snapshots the session's game-data queue records in queue
// order, which is peer-add order, and resolves the two per-record tests the drain
// makes on each record's peer.
//
// The session lock is not held across the ack state: the drain takes the ack
// state's own lock and nothing takes them the other way round, but a caller that
// wanted to be sure would have to hold both here.
func (s *Session) queueEntries(now time.Time) []QueueEntry {
	s.mu.RLock()
	entries := make([]QueueEntry, 0, len(s.peerOrder))
	for _, id := range s.peerOrder {
		p := s.Peers[id]
		if p == nil {
			continue
		}
		entries = append(entries, QueueEntry{Slot: p.ID, Participating: p.Participating})
	}
	s.mu.RUnlock()
	for i := range entries {
		// A record whose peer is not participating is skipped, and the timeout
		// test inside that branch only runs for those.
		if entries[i].Participating {
			entries[i].TimedOut = s.syncTimedOut(entries[i].Slot, now)
		}
	}
	return entries
}

// GetPeers returns the session peers in join order, which is peerOrder. A slot
// table has no order of its own, so this is the walk that names one, and the one
// the protocol's own walks use: the game-data queue and the fallback scan by
// secret, which returns the FIRST peer whose secret matches.
func (s *Session) GetPeers() []*Peer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	peers := make([]*Peer, 0, len(s.peerOrder))
	for _, id := range s.peerOrder {
		if p := s.Peers[id]; p != nil {
			peers = append(peers, p)
		}
	}
	return peers
}

// ForEachPeer iterates the session peers directly under the session lock,
// invoking fn for each, in join order (see GetPeers). It is the
// allocation-free replacement for GetPeers() when the caller only needs to read
// the peers (P10): materializing a snapshot slice per call churned the allocator
// on the ack-tick, broadcast and liveness paths. fn must not mutate the session
// or call back into it, as the lock is held for the whole iteration.
func (s *Session) ForEachPeer(fn func(*Peer)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, id := range s.peerOrder {
		if p := s.Peers[id]; p != nil {
			fn(p)
		}
	}
}

func (s *Session) PeerCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.peerOrder)
}

// SetObserverConfig applies the per-session observer limits, which arrive as
// session constructor arguments. updateInterval is in simulation ticks.
func (s *Session) SetObserverConfig(maxObservers, updateInterval int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.MaxObservers = maxObservers
	s.ObserverUpdateInterval = updateInterval
}

// SetObservableParameters applies SetObservableParamatersMessage (0x1D). It is
// only applied when the sending peer has a session and that session has not
// started recording yet, and then it sets four things:
//
//	lag pages  = thirdU32 * ticksPerSec
//	reconnectable = (firstU32 != 0)
//	observable hash = the u64, the credential a StartObserveRequest carries as
//	                  its password hash
//	frame cap  = MaxObservers <= 0 ? 60000/ticksPerSec + 1 : -1
//
// ticksPerSec is the session's simulation period expressed as ticks per second;
// the caller derives it from the configured period.
//
// The frame cap is a cap in FRAMES, not a window length: the tick hands it to the
// store and the store drops whole blocks when the retained count passes it. -1
// disables the cap, which is what the game asks for as soon as maxObservers > 0,
// so in a real match nothing bounds the store but its block count.
func (s *Session) SetObservableParameters(firstU32 uint32, hash uint64, thirdU32 uint32, ticksPerSec int) bool {
	if ticksPerSec < 1 {
		ticksPerSec = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Recording {
		return false
	}
	s.LagPages = int(thirdU32) * ticksPerSec
	s.Reconnectable = firstU32 != 0
	s.observableHash = hash
	if s.MaxObservers <= 0 {
		s.observableWindow = 60000/ticksPerSec + 1
	} else {
		s.observableWindow = -1
	}
	return true
}

// ObservableHash returns the u64 the players published with 0x1D, zero when they
// never sent it.
func (s *Session) ObservableHash() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.observableHash
}

// LeaveSimulation is the whole of the LeaveSimulation message (0x1F): take the
// sender's session and its peer id, and none of it removes the peer from the
// session. It does, in order:
//
//	- look up the peer's slot; if there is none, do nothing
//	- clear the participating mark: the leaver is no longer "in the game"
//	- empty its game-data queue entry, keeping the entry
//	- empty its checksum entry, keeping the entry
//	- if the session was recording and NO peer is still participating, mark the
//	  match as over
//
// That last step is one 16-bit store over two adjacent bytes, and it is the only
// writer of the "match is over" flag anywhere in the server: 256 is 0x0100, so
// little endian it clears the recording flag and sets the game-started flag. The
// same store appears in the peer-removal path, which is the other way a match
// ends. So the flag means "this match is over", and it is what the session tick
// reads to stop building acks (see PopAckTick) and what the observer update reads
// to serve the live end of the store plus the end-of-game marker.
//
// What it is NOT is a peer removal. Only the disconnect path takes a peer out of
// the session, and it erases the session itself once it is empty. An earlier
// revision of this code removed the peer here and ended the session when the last
// one left; both come from reading the message name instead of the handler.
//
// It reports whether the peer was found.
func (s *Session) LeaveSimulation(peerID byte) bool {
	s.mu.Lock()
	p := s.Peers[peerID]
	if p == nil {
		s.mu.Unlock()
		return false
	}
	p.Participating = false
	s.endMatchIfNoneActiveLocked()
	s.mu.Unlock()
	// Outside the lock: the ack state has its own.
	s.Ack.DropPeer(peerID)
	return true
}

// SetReconnectable mirrors the LobbyConnect reconnect state being non-zero.
func (s *Session) SetReconnectable(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Reconnectable = v
}

// SetPeerSecret records the credential the peer published in its LobbyConnect,
// which is how a later reconnect finds it. A zero secret is not stored, so it can
// never be matched.
func (s *Session) SetPeerSecret(id byte, secret uint64) bool {
	if secret == 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.Peers[id]
	if p == nil {
		return false
	}
	p.Secret = secret
	return true
}

// PeerBySecret finds the peer carrying the given LobbyConnect secret, the lookup
// a reconnect does. The secret must be non-zero: zero means the client published
// no secret and every peer with an unset secret would otherwise match.
//
// The walk is peerOrder, which is what decides the answer when two peers carry
// the same secret: the lookup returns the FIRST peer whose secret matches, so
// "the one that joined first" is the answer and not a coin toss. Which is the
// only thing this changes: a map answered this question differently on every run.
func (s *Session) PeerBySecret(secret uint64) *Peer {
	if secret == 0 {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, id := range s.peerOrder {
		if p := s.Peers[id]; p != nil && p.Secret == secret {
			return p
		}
	}
	return nil
}

// ReconnectPeer moves an existing peer onto a new connection, which is what the
// reconnect branch does when the peer is found by secret.
//
// It keeps the slot, the peer key and every per-peer field, so the player does
// not get a second slot and the old connection's entry does not linger as a
// phantom until the sync timeout reaps it. It reports the connection it
// replaced, because that one is closed: the two sockets are swapped, which leaves
// the replaced connection holding the old socket, and the caller schedules it for
// immediate close. The peer keeps the NEW socket, which is why every send in that
// branch goes to the peer and not to the connection.
//
// This moves the session's own index only. Manager.ReconnectPeer is the call
// that also moves the relay route; without it the new connection resolves to no
// session.
func (s *Session) ReconnectPeer(id byte, conn PeerConnection, secret uint64) (PeerConnection, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.Peers[id]
	if p == nil {
		return nil, false
	}
	old := p.Conn
	if old != nil {
		// Drop the reverse index entry for the connection being replaced, or
		// PeerIDForConn keeps resolving the dead one.
		if s.peerByConn[old] == p {
			delete(s.peerByConn, old)
		}
	}
	p.Conn = conn
	s.peerByConn[conn] = p
	if secret != 0 {
		p.Secret = secret
	}
	p.LastSeen = time.Now()
	return old, true
}

// ResendFrames answers a state-2 reconnect: the stored frames from `from` up to
// the newest one, and whether the request can be served at all. A refused
// request gets no reply and its connection is closed, so the answer has to tell
// "nothing to send" apart from "cannot send".
//
// The range is [from, frame count - 1] and it is refused when it is invalid
// (from below zero, or past the newest frame, which includes a client that
// already holds every frame), when the newest frame does not exist, and when the
// first frame asked for is no longer stored. The store refuses rather than
// clamping, so the client is never sent a window it did not ask for. The one
// exception is a count of 0 against an empty store: that request is served,
// with nothing to send.
//
// The frames are copied out under the lock: the store shifts its frames in place
// when it drops a block, so a window into it is not safe to send from.
func (s *Session) ResendFrames(from int) ([]Frame, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	last := s.PageCount - 1
	if from == 0 && last == -1 {
		return nil, true
	}
	if s.replay == nil {
		return nil, false
	}
	frames := s.replay.GetFrames(from, last)
	if frames == nil {
		return nil, false
	}
	return append([]Frame(nil), frames...), true
}

// HostID returns the peer the clients treat as the simulation host, or 0 when
// no host is known.
func (s *Session) HostID() byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hostID
}

// SetHostID records the session host. The host is taken from an IntegrityCheck
// that proposes a new host with migrateState 0; AddPeer also carries an isHost
// flag, which is how a session learns its first host.
func (s *Session) SetHostID(id byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hostID = id
}

// NotePeerAdded records the peer facts an AddPeer message carries and returns
// false when the message names a peer that is not in this session. AddPeer's
// globalID is the peer's key in the session's peer table, so it is recorded here.
// The handler does more bookkeeping that has not been traced; what is reproduced
// here is the part the rest of the server depends on: the peer key and which peer
// is the host.
func (s *Session) NotePeerAdded(id byte, globalID uint64, isHost bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.Peers[id]
	if p == nil {
		return false
	}
	if globalID != 0 {
		p.Key = globalID
	}
	if isHost {
		s.hostID = id
	}
	return true
}

// SetPeerKey records a peer's u64 identity, as a RequestJoin does with its
// trailing u64.
func (s *Session) SetPeerKey(id byte, key uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.Peers[id]
	if p == nil {
		return false
	}
	p.Key = key
	return true
}

// SetPeerClientMessageVersion records the version a peer announced in its
// LobbyConnect handshake, the field both version gates read.
func (s *Session) SetPeerClientMessageVersion(id byte, version uint16) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.Peers[id]
	if p == nil {
		return false
	}
	p.ClientMessageVersion = version
	return true
}

// Key returns the session's identity: the pair (titleID, sessionID) the
// manager keys it by.
func (s *Session) Key() SessionKey {
	return s.key
}

// TitleID returns the title this session was created under.
func (s *Session) TitleID() int16 {
	return s.titleID
}

// ClientMessageVersionOf returns the version the peer announced at connect.
func (s *Session) ClientMessageVersionOf(id byte) (uint16, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.Peers[id]
	if p == nil {
		return 0, false
	}
	return p.ClientMessageVersion, true
}

// SetPeerTimeoutOverride records the peer's LobbyConnect timeout override, the
// value that replaces the 60 s sync-timeout default for that peer.
func (s *Session) SetPeerTimeoutOverride(id byte, ms uint32) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.Peers[id]
	if p == nil {
		return false
	}
	p.TimeoutOverride = ms
	return true
}

// TimeoutOverrideOf returns the timeout override the peer was given at its first
// LobbyConnect, 0 when it has none.
func (s *Session) TimeoutOverrideOf(id byte) uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.Peers[id]; p != nil {
		return p.TimeoutOverride
	}
	return 0
}

// defaultSyncTimeoutMS is the sync-timeout deadline used when a peer sends no
// timeout override.
const defaultSyncTimeoutMS = 60000

// syncTimedOut is one peer's half of the drain's timeout branch: is this peer's
// game data late? The deadline is the peer's override or the 60 s default and the
// clock starts at the peer's last upload.
//
// Only a drained queue can be late, so a peer still holding a backlog is spared
// whatever the elapsed time is.
func (s *Session) syncTimedOut(slot byte, now time.Time) bool {
	p := s.GetPeer(slot)
	if p == nil || !s.Ack.QueueEmpty(slot) {
		return false
	}
	timeout := time.Duration(p.TimeoutOverride) * time.Millisecond
	if p.TimeoutOverride == 0 {
		timeout = defaultSyncTimeoutMS * time.Millisecond
	}
	last, ok := s.Ack.LastSync(slot)
	if !ok {
		// Nothing queued yet: measure from when the peer joined, so a peer
		// that never sends anything is eventually reaped.
		last = p.LastSeen
	}
	return now.Sub(last) > timeout
}

// SyncTimedOutPeers returns the player slots whose game data is late, which the
// server answers by scheduling them to close with syncDataTimeoutReason.
// The check is `now - lastSync > override`, with the deadline defaulting to 60 s.
//
// Three conditions gate it, and all three are part of the protocol:
//
//   - The drain only checks a peer whose game-data queue has drained. A peer
//     still holding a backlog is skipped for that pass, so a peer that paused
//     with data queued is spared. Timing out purely on elapsed time would reap
//     peers that are spared.
//   - It skips peers whose "participating in the started game" mark is clear, so
//     a peer of a session that never started is not reaped. That is the same
//     record the pending list is built from, and the same line that keeps such a
//     peer off the pending list.
//   - It walks the queue records, so the answer is in peer-add order.
//
// Without this a peer that stops sending stalls the session forever, because the
// tick only ever drains what is queued.
func (s *Session) SyncTimedOutPeers(now time.Time) []byte {
	if !s.Ack.Started() {
		return nil
	}
	var timedOut []byte
	for _, e := range s.queueEntries(now) {
		if e.TimedOut {
			timedOut = append(timedOut, e.Slot)
		}
	}
	return timedOut
}

// PeerByKey finds the peer with the given u64 identity, the lookup over the
// session's keyed peer table. That table is keyed by the identity itself, so two
// peers cannot carry the same key. Here two slots can, because the key is a field
// of the peer and SetPeerKey writes it per slot; when they do, the walk answers
// with the one that joined first rather than with a per-run coin toss.
func (s *Session) PeerByKey(key uint64) *Peer {
	if key == 0 {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, id := range s.peerOrder {
		if p := s.Peers[id]; p != nil && p.Key == key {
			return p
		}
	}
	return nil
}

// DenyPeerJoin is the RequestJoinDenied path: the peer named by the message's
// key is scheduled to disconnect 30 s out with the reason recorded. It returns
// false when the sending peer has no session or the key is unknown; the former is
// the ordering case that is logged as a warning rather than an error.
func (s *Session) DenyPeerJoin(key uint64, reasonCode uint32, reason string, now time.Time) bool {
	p := s.PeerByKey(key)
	if p == nil {
		return false
	}
	p.ScheduleClose(now.Add(peerRemovalGrace), fmt.Sprintf(
		"join denied, reasonCode=%d, debugReason=%s", reasonCode, reason))
	return true
}

// RemovePeerGracefully is the RemovePeer path: the target peer is scheduled to
// disconnect 30 seconds from now, with the reason recorded, and the earliest
// deadline wins. It returns false when the peer is unknown, which is a case the
// protocol tolerates: a RemovePeer that overtakes the join is logged as "someone
// is send message in the wrong order", not treated as an error.
func (s *Session) RemovePeerGracefully(peerID byte, byPeerID byte, reasonCode uint32, reason string, now time.Time) bool {
	s.mu.RLock()
	p := s.Peers[peerID]
	s.mu.RUnlock()
	if p == nil {
		return false
	}
	p.ScheduleClose(now.Add(peerRemovalGrace), fmt.Sprintf(
		"peer=%d removed by peer=%d, reasonCode=%d, debugReason=%s",
		peerID, byPeerID, reasonCode, reason))
	return true
}

// peerRemovalGrace is the 30 s a removed peer is given.
const peerRemovalGrace = 30 * time.Second

// RecordFrame stores one frame of the session's ack stream in the frame store,
// creating the store on the first one.
//
// WHAT is stored is the decisive part, and an earlier revision got it wrong. The
// store is fed by the session tick, not by the 0x09 GameData the players upload:
// the tick builds the EndOfGameData message, hands it to the store's append and
// only then sends it to the peers. An observer is therefore served a copy of the
// very 0x0A stream the players get, one message per tick, and the stored frame
// numbers are tick numbers.
//
// The capture of 2026-09-29_012528_03cb67c7cf19 says exactly that, with no room
// for interpretation: the two observer connections received 1346 and
// 800 messages, every one of them byte-identical to a message of the player's
// 0x0A stream and in strictly increasing order of it, from ack #0 (the first ack
// of the match, at t=22s) up to ack #1345 and ack #799. Not one 0x09 and not one
// 0x22. Serving the uploaded pages instead is what left the second observer with
// 70 messages where 803 were sent.
//
// The store's size is the number of BLOCKS it keeps, not the number of frames:
// ceil(peers * f) with peers floored at 8, where f is the
// -maxPagesToStorePerSession scale. The flag is divided by 8 on its way in, so the
// DEFAULT is f = 1.0 and 8 blocks. Each block is the -pageSize setting, 65536 by
// default: half a megabyte of ack history, which at the ~165 bytes per tick this
// capture measured is fifty minutes of match, so a store this size does not roll
// over during a normal game and an observer is served from frame 0.
//
// The Recording flag is set here because it means "the store exists", not
// "somebody uploaded something".
//
// body is the message body and is copied: the caller hands over the buffer it is
// about to send, which its transport reuses.
func (s *Session) RecordFrame(msgType byte, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureRecordingLocked()
	s.replay.AddFrame(msgType, body)
	s.PageCount++
}

// ensureRecording creates the frame store if it does not exist yet, in this
// order: the recording flag goes up, then every peer is marked as being in the
// started game, and only then the store itself. The marks are what the pending
// list of the 0x0A filters on and what the reconciliation and the sync-timeout
// check read, so this has to happen before that message is built and not when it
// is appended.
func (s *Session) ensureRecording() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureRecordingLocked()
}

// ensureRecordingLocked is ensureRecording for a caller that holds the lock.
// The marks are set ONCE, so a peer that joins after the store exists never gets
// one.
func (s *Session) ensureRecordingLocked() {
	if s.replay != nil {
		return
	}
	s.Recording = true
	// The marks go on every peer of the session, and the block count below is
	// ceil(peers * f) with peers FLOORED AT 8, so the count is the membership and
	// not the number of occupied slots in a 256-entry table. peerOrder is the
	// membership: AddPeer appends and RemovePeer erases, and nothing else writes
	// either.
	for _, id := range s.peerOrder {
		if p := s.Peers[id]; p != nil {
			p.Participating = true
		}
	}
	n := max(len(s.peerOrder), 8)
	scale := 1.0
	if s.maxPagesPerMatch > 0 {
		scale = float64(s.maxPagesPerMatch) * 0.125
	}
	blocks := int(math.Ceil(float64(n) * scale))
	if blocks < 1 {
		blocks = 1
	}
	blockSize := s.pageSize
	if blockSize < 1 {
		blockSize = defaultPageSize
	}
	s.replay = NewReplay(blocks, blockSize, s.observableWindow)
}

// defaultPageSize is the -pageSize default and the block size of the frame store
// when the flag is unset.
const defaultPageSize = 0x10000

// StartObserve handles StartObserveRequest.
//
// The decision is a cascade, and the status byte is a constant per arm:
//
//	if maxObservers > 0 and reconnectable {              // else -> 4
//	    if observerCount < maxObservers {               // else -> 2
//	        if recording and store has a frame
//	            and store's first retained frame > 0 {    // else -> 0 + REGISTER
//	            -> 3
//
// The two constants that matter are not what their names suggest:
//
//   - 3 is the arm that does NOT register the observer. It builds the response
//     and returns, leaving the observer list alone, so the client is on its own.
//   - 0 is the arm that DOES. Its tail marks the connection observer, appends an
//     entry with progress 0, active 0 and the lagged flag, and only then returns.
//
// So the byte on the wire and the registration are two different things, and the
// registration is the one the client depends on: it sends ActivateObserver, the
// handler finds the entry and only an entry that exists gets served. This is what
// the captures show, and it inverts what this code used to do:
// 2026-09-29_012528_03cb67c7cf19 has an original that answered 0, whose store
// never dropped its first frame (the whole 259651 bytes of the match's acks fit
// in its four blocks), and which served both observers from frame 0.
//
// Status codes, then:
//
//	0 = the observer is registered, with progress 0
//	3 = not registered: the session is recording and its store has already
//	    dropped the first frame of the match
//	2 = not registered, the observer list is already at the cap
//	4 = not registered, observers are disabled for this session: maxObservers <= 0
//	    or not reconnectable
//
// Note that 2, 3 and 4 are different non-registrations, not one: an earlier
// revision returned 2 for both rejections, which the protocol never does.
//
// Reconnectable is set by LobbyConnect from the ReconnectState field. If the
// client sends ReconnectState=0 (e.g. on observer reconnect), Reconnectable
// becomes false and subsequent StartObserve requests get status 4. Observators
// cannot reconnect if ReconnectState=0 was sent.
//
// passwordHash is the request's own field, and it decides the LAGGED flag of the
// entry, which is what the whole lagged observer path hangs off. The rule is that
// an observer is lagged unless it presents EXACTLY the u64 the session published
// with 0x1D: no published hash means lagged, a wrong hash means lagged, and only
// the matching one clears it. This is also what closes SESSION-JOURNAL.md 20.3
// ("the u64 of 0x1D against the passwordHash of StartObserveRequest are not
// compared because there is no proof the protocol compares them"): it does, here,
// and the u64 it is compared against is the one SetObservableParameters writes
// from the same message.
//
// The rule is the opposite of "the client sent a hash", which is what this used to
// assume, and the captures are on the other side of it: the game publishes
// 0x21616c6661aa7a42 and every observer in the five committed captures sends
// passwordHash 0 (measured over internal/sim/testdata, pinned by
// TestTheCommittedObserversAreLaggedUnderTheOriginalsRule). Those observers are
// therefore lagged and were not lagged here.
//
// # peerIdentifier: the observer list is keyed on it, not on the connection
//
// peerIdentifier is the request's third u64, body offset 9. The legacy 24-byte
// form of the request has no such field, so it is forced to 0 there.
//
// The connection carries it, and the whole server agrees on that being the only
// writer and the only reader: the connection constructor starts it at 0, this arm
// writes it and only this arm, and the activate and stop lookups both compare
// against it.
//
// The two lookups are the same comparator: equality on a u64, first match, in the
// caller's OWN session only. There is no peerID, no address and no second key
// anywhere in either. So:
//
//   - Two connections presenting the SAME identifier are the same observer as
//     far as the list is concerned. A 0x1A from the second one arms the FIRST
//     entry and the 0x1E goes to the second connection; a 0x1B from it removes
//     the first entry and closes itself. The observer update still serves both
//     entries, each to its own connection.
//   - An identifier of 0 is an identity, not the absence of one. Every connection
//     starts at 0, so a zero-identified entry is matched by every player of the
//     session, and 0x1A/0x1B from a player are not blocked: the observer type
//     mask is one-way.
//   - The value is NOT copied into the entry, so a connection sent a second
//     StartObserve renames the entries that point at it. That is why this
//     function used to have a "a repeated StartObserve is idempotent" guard
//     keyed on the connection: it is gone because the append is unconditional and
//     a guard would swallow a new identifier and never register it.
//   - It is written only in this arm. The arms that answer 3, 2 and 4 never touch
//     it, so a rejected request leaves whatever was there.
//
// The second return value is the registration, and it is not derivable from the
// status: 0 is both "registered" and "rejected" (SESSION-JOURNAL.md 24). The
// caller used to re-derive it with a list search, which is wrong as soon as one
// connection holds two entries.
func (s *Session) StartObserve(client PeerConnection, peerIdentifier, passwordHash uint64) (status int, registered bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.MaxObservers <= 0 || !s.Reconnectable {
		return 4, false
	}
	if len(s.observers) >= s.MaxObservers {
		return 2, false
	}
	if s.Recording && s.replay != nil && s.replay.Total() > 0 && s.replay.Base() > 0 {
		return 3, false
	}
	// The one write of the connection's observer identifier.
	client.SetPeerIdentifier(peerIdentifier)
	// See the lagged rule in the comment above.
	lagged := s.observableHash == 0 || passwordHash != s.observableHash
	// progress 0, active 0, lagged. The entry waits inactive until the client
	// sends ActivateObserver.
	s.observers = append(s.observers, &ObserverEntry{Client: client, Lagged: lagged})
	return 0, true
}

// matchesObserver is the observer-list comparator, shared by the activate, the
// stop and the has query so the three cannot drift apart: first entry whose
// connection carries the caller's peerIdentifier.
func (s *Session) matchesObserver(client PeerConnection) *ObserverEntry {
	id := client.PeerIdentifier()
	for _, o := range s.observers {
		if o.Client.PeerIdentifier() == id {
			return o
		}
	}
	return nil
}

// ActivateObserver answers ActivateObserver (0x1A) with 0x1E. The whole exchange
// is short and every field of its answer comes from the session:
//
//	- walk the observer list and stop at the entry whose connection carries the
//	  same peerIdentifier; set that entry active. If no entry matches, nothing is
//	  sent at all.
//	- ticksPerSecond = 1000 / the session's simulation period
//	- lagged observer:  offset = (lagPages - pageCount) / ticksPerSecond
//	  not lagged:       offset = -(pageCount / ticksPerSecond)
//	- time = now + offset seconds
//
// So the body is 9 bytes: [status u8][time i64 LE], the status is always 0
// because the only other arm sends nothing, and the time is the wall clock plus
// the offset in seconds.
//
// "Whose connection carries the same peerIdentifier" is the whole comparator: the
// field lives on the connection, the entry only points at it, and the first match
// wins. The entry that gets armed and the connection that gets the answer are
// therefore two different objects whenever two connections share an identifier.
//
// The DIRECTION of that offset is the two-armed question, and the arms are not
// opposite signs by construction, they are two different subtractions: lagPages is
// the lag the players published with 0x1D in seconds (SetObservableParameters) and
// pageCount is how many frames the match has produced. A lagged observer is told
// the match started LagPages minus PageCount seconds ago, which is in the PAST
// once the match is longer than the lag and in the FUTURE while it is not: with
// the lag the game actually publishes (0, measured over the committed captures)
// both arms collapse to -(PageCount / ticksPerSec) and the two bytes on the wire
// are the same, which is why the field is masked in the harness and why a wrong
// sign there has never shown up in a capture.
func (s *Session) ActivateObserver(client PeerConnection, ticksPerSec int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.matchesObserver(client)
	if o == nil {
		return false
	}
	o.Active = true
	if ticksPerSec < 1 {
		ticksPerSec = 1
	}
	offset := -(s.PageCount / ticksPerSec)
	if o.Lagged {
		offset = (s.LagPages - s.PageCount) / ticksPerSec
	}
	// ActivateObserverResponse body: [status u8][time i64 LE] = 9 bytes.
	body := make([]byte, 9)
	body[0] = 0 // status (always 0, accepted)
	binary.LittleEndian.PutUint64(body[1:9], uint64(time.Now().Unix()+int64(offset)))
	_ = client.SendMessage(protocol.MsgActivateObserverResp, body)
	return true
}

// RemoveObserver removes the client from the observer list (StopObserve, 0x1B).
//
// The lookup is the ActivateObserver one verbatim, so removal resolves exactly the
// way activation does: first entry whose connection carries the caller's
// peerIdentifier. Two things follow and both are reproduced here:
//
//   - with two connections sharing an identifier, a StopObserve from the second
//     removes the FIRST entry;
//   - the connection then closed is the one it was HANDED, not the entry's
//     connection. Closing the caller's socket is the transport's business, so
//     this returns and lets the handler do it.
func (s *Session) RemoveObserver(client PeerConnection) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := client.PeerIdentifier()
	for i, o := range s.observers {
		if o.Client.PeerIdentifier() == id {
			s.observers = append(s.observers[:i], s.observers[i+1:]...)
			return true
		}
	}
	return false
}

// removeObserverConn takes a closing connection's observer entry off the list:
// the first entry whose connection IS conn. The disconnect matches the entry by
// its connection. StopObserve matches by the identifier the connection carries,
// and with two connections sharing an identifier that would take the wrong one.
func (s *Session) removeObserverConn(conn PeerConnection) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, o := range s.observers {
		if o.Client == conn {
			s.observers = append(s.observers[:i], s.observers[i+1:]...)
			return true
		}
	}
	return false
}

// RemoveObserverConn removes every observer entry that belongs to conn. It is the
// close path's cleanup and the StopObserve fallback: one connection is one
// observer, so a list that carries it more than once (a repeated StartObserve)
// must not leave the duplicates behind when the connection stops watching.
func (s *Session) RemoveObserverConn(conn PeerConnection) {
	for s.removeObserverConn(conn) {
	}
}

// HasObserver reports whether an entry with this connection's peerIdentifier is
// in the list, with the same comparator as the other two lookups.
func (s *Session) HasObserver(client PeerConnection) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.matchesObserver(client) != nil
}

// ObserverCount returns the number of observers.
func (s *Session) ObserverCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.observers)
}

// Tick runs the per-simulation-tick session update:
//   - peers whose scheduled disconnect has passed are closed
//   - while reconnectable with observers, the observer update delivers pages to
//     the observers selected by the stride counter.
//   - otherwise, when not recording and not reconnectable, every observer is
//     kicked with a StartObserveResponse status 4 and its connection is closed.
//
// The observer connections the kick closes are closed by Tick, not by the body
// that collects them, and that is the whole point of tickLocked returning them:
// Close() is not a leaf. WsClient.Close() calls RemoveClient synchronously,
// which reaches removeObserverConn through
// Manager.HandleDisconnect, which takes s.mu again -- and Client.Close()
// on the bsPort transport does the same from its own close path. A
// sync.RWMutex is not reentrant, so closing from inside the section that holds it
// wedges the goroutine that runs Tick, which is the server's main loop. It is
// not a risk of one observer's send
// stalling: Tick() dispatches every session's pages, so one dead tick stops the
// whole server. The same reason closeExpiredPeers collects under the lock and
// closes after releasing it.
func (s *Session) Tick() {
	s.closeExpiredPeers()
	s.closeSyncTimedOutPeers()

	for _, c := range s.tickLocked() {
		_ = c.Close()
	}
}

// tickLocked is the part of the tick that runs with s.mu held, and it returns the
// observer connections the kick closed so the caller can close them after the
// lock is gone. See Tick for why the closing cannot happen here.
//
// The kick schedules the connection with a zero delay, which the close-due
// predicate reports as due on the very next pass of the socket loop, so the
// socket goes down in the same tick. The other close on the observer path is not
// here: the observer update's own close is a schedule with a reason string
// (an observer that cannot be updated), and LeaveSimulation (0x1F) closes
// nothing.
//
// The list is still emptied here and that stays a deliberate difference, recorded
// in SESSION-JOURNAL.md 32: the entry would come off anyway when the connection
// closes, so the end state of the list agrees; what is not done upstream is empty
// it while the connection is still up.
func (s *Session) tickLocked() []PeerConnection {
	s.mu.Lock()
	defer s.mu.Unlock()
	var kicked []PeerConnection
	if s.Reconnectable && len(s.observers) > 0 {
		s.updateObserversLocked()
	}
	if !s.Recording && !s.Reconnectable && len(s.observers) > 0 {
		for _, o := range s.observers {
			_ = o.Client.SendMessage(protocol.MsgStartObserveResp, []byte{4})
			kicked = append(kicked, o.Client)
		}
		log.Printf("observer support is off for session %d, dropping %d observer(s)", s.ID, len(s.observers))
		s.observers = nil
	}
	return kicked
}

// closeExpiredPeers disconnects the peers whose deadline set by
// RemovePeerGracefully has passed. These deadlines are evaluated in the session
// tick. Closing the connection is what runs the server's RemoveClient; closing an
// already-closed connection is a no-op in both transports.
func (s *Session) closeExpiredPeers() {
	now := time.Now()
	var expired []*Peer
	s.ForEachPeer(func(p *Peer) {
		if at, reason, ok := p.CloseDeadline(); ok && !now.Before(at) {
			log.Printf("Session %d: closing peer %d, %s", s.ID, p.ID, reason)
			expired = append(expired, p)
		}
	})
	for _, p := range expired {
		_ = p.Conn.Close()
	}
}

// closeSyncTimedOutPeers disconnects the peers whose game-data queue has been
// empty past their deadline, with the reason syncDataTimeoutReason.
//
// It uses the immediate scheduler that refuses to override an existing deadline,
// not the 30 s one RemovePeer uses, so a peer already on its way out keeps its
// original reason and timing.
func (s *Session) closeSyncTimedOutPeers() {
	now := time.Now()
	for _, id := range s.SyncTimedOutPeers(now) {
		p := s.GetPeer(id)
		if p == nil {
			continue
		}
		if p.ScheduleCloseIfNone(now, syncDataTimeoutReason) {
			log.Printf("Session %d: peer %d %s", s.ID, id, syncDataTimeoutReason)
		}
	}
}

// syncDataTimeoutReason is the reason a peer is closed for letting its game-data
// queue run past its deadline. It travels nowhere on the wire: it is recorded
// against the peer and printed by the tick that closes it, so it is this
// project's own wording rather than anything a client reads.
const syncDataTimeoutReason = "sync data timeout expired"

// updateObserversLocked serves the observer list. It must be called with s.mu
// held. A persistent counter advances by one per tick and wraps when it exceeds
// the interval (period = interval+1); each tick the observers at indices congruent
// with the counter modulo the interval receive up to 200 frames of the store.
//
// WHAT THE STRIDE COSTS, MEASURED, because it is a phase and not a rate: with the
// default interval of 8 TICKS (internal/server/ticks.go, and the reason it is 8 and
// not 8 ms is written there) the observer at index 0 is visited on the ticks whose
// counter is 0, 9, 18..., because the walk starts at the counter and steps by the
// interval, and the counter only wraps once it is strictly greater than the
// interval. An observer that becomes ACTIVE on any other tick is therefore not
// served for up to interval ticks: 1.125 s at the default period, and a fixture
// whose recording ends sooner than that after the activation never sees a single
// replay frame.
//
// The counter advances even on a tick that visited nobody, so the tick on which an
// observer is registered but not yet activated is one of those: it pushes the next
// visit a whole stride out. That is the whole mechanism behind a must-match flake
// measured on this machine at 1 in 60 replays of testdata/synthetic, and it is NOT
// new: it reproduces identically on the commit before the peer table became an
// array. Instrumented, the failing run is:
//
//	observers 1 pageCount  8  0:active=false   <- registered, Activate not in yet
//	observers 1 pageCount  9  0:active=true    <- and from here the counter is 1,
//	observers 1 pageCount 16  0:active=true    <- so index 0 is never reached
//
// The stride is not touched. Whether a freshly activated observer is served on the
// same tick or waits out the stride is not established here, and changing it to
// make a fixture deterministic would be guessing at the protocol, which is the one
// thing this file may not do (AGENTS.md rule 1). What is written down is the
// behaviour, so the next reader does not have to rediscover it with a profiler.
//
// The frames are the session's own 0x0A stream, in order, from the observer's
// own progress, which starts at 0 and is advanced by exactly what was sent. That
// is what a late observer's catch-up is: everything the store still holds, 200
// frames per visit. An observer that joined 171 s into the capture's match was
// served all 800 frames of the retained history in four visits and then rode the
// live stream.
func (s *Session) updateObserversLocked() {
	interval := s.ObserverUpdateInterval
	// The walk never terminates when the interval is 0 (with the default
	// settings 8ms converts to 0 ticks); guard against it.
	interval = max(interval, 1)
	counter := s.observerTick
	var dead []int
	for i := counter; i < len(s.observers); i += interval {
		obs := s.observers[i]
		if !obs.Active {
			continue
		}
		// The target an observer is served up to, and it is not the frame count for
		// everybody. The rule, in order:
		//
		//	liveEnd = the match has started, or the session has no peers
		//	if not lagged, or liveEnd:  target = pageCount
		//	else:                      target = pageCount - lagPages
		//
		// so a lagged observer of a LIVE match stops lagPages frames short of the
		// end of the store: its replay is a feed delayed by the lag the players
		// published, and it never sees the most recent lagPages frames. The
		// exception is liveEnd: once the game has started or the session has lost
		// its last peer, a lagged observer is served the live end like anybody else.
		//
		// This used to read liveEnd as "the session message queue is non-empty" and
		// leave the target at the frame count for every observer, which is the one
		// thing the lagged arm is not: with a lag published it hands a lagged
		// observer frames it never gets.
		liveEnd := s.GameStarted || len(s.peerOrder) == 0
		target := s.PageCount
		if obs.Lagged && !liveEnd {
			target = s.PageCount - s.LagPages
		}
		if target <= 0 {
			continue
		}
		if s.replay == nil {
			log.Printf("session %d has no frame store, observer %p gets no frames", s.ID, obs.Client)
			continue
		}
		// The store forgets whole blocks once it is full (see Replay). An observer
		// that fell behind the retained window can no longer be served the gap: the
		// store refuses a first frame below the oldest retained one, leaving the
		// progress where it was, so the observer is stuck there forever. This code
		// jumps it to the window instead. It is a deliberate difference and it can
		// only trigger after maxBlocks*pageSize bytes of history, which is fifty
		// minutes of match at the default settings.
		if base := s.replay.Base(); obs.Progress < base {
			obs.Progress = base
		}
		// Max frames per observer update.
		delta := min(target-obs.Progress, 200)
		if delta != 0 {
			if delta < 0 {
				log.Printf("session %d observer %p is past its target %d, no frames sent", s.ID, obs.Client, target)
				continue
			}
			frames := s.replay.GetFrames(obs.Progress, obs.Progress+delta-1)
			if len(frames) == 0 {
				log.Printf("session %d observer %p asked for frames [%d,%d] the store cannot serve",
					s.ID, obs.Client, obs.Progress, obs.Progress+delta-1)
				continue
			}
			for _, f := range frames {
				if err := obs.Client.SendMessage(f.Type, f.Body); err != nil {
					obs.SendFailures++
				} else {
					obs.SendFailures = 0
				}
			}
			obs.Progress += delta
			// The end-of-game-data marker is only for an observer whose
			// target is the store's own (liveEnd) and whose client announces message
			// version 4 or more. liveEnd is "the game has started or the session
			// has no peers left". The capture agrees with the version half of it: the
			// original sent zero type-34 messages to either of its two observers, and
			// neither of those connections ever sent a LobbyConnect, so neither had
			// a version.
			if liveEnd {
			// The marker is gated on the observer's own connection announcing
			// message version 4 or more, on top of the liveEnd arm this is inside.
			// The two halves are not the same gate: liveEnd says the store's end is
			// the right target, the version says the client understands the marker
			// at all. An observer that connected without a LobbyConnect has no
			// version (0) and is not sent one, which is why zero type-34 messages
			// were sent to either of the two observers in
			// 2026-09-29_012528_03cb67c7cf19.
			if obs.Progress == target && obs.Client.ClientMessageVersion() >= 4 {
				if err := obs.Client.SendMessage(protocol.MsgEndOfGameDataObserver, nil); err != nil {
					obs.SendFailures++
				} else {
					obs.SendFailures = 0
				}
			}
		}
			if obs.SendFailures >= maxObserverSendFailures {
				log.Printf("Dropping stalled observer %p from session %d", obs.Client, s.ID)
				dead = append(dead, i)
			}
		}
	}
	counter++
	if counter > interval {
		counter = 0
	}
	s.observerTick = counter
	// Remove the dropped observers in descending index order so the earlier
	// indices stay valid during the removals.
	for _, idx := range slices.Backward(dead) {
		s.observers = append(s.observers[:idx], s.observers[idx+1:]...)
	}
}

func (s *Session) IsClosed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.closed
}

// ReplayPageCount returns the number of frames the session's frame store holds,
// or 0 when no store has been created.
func (s *Session) ReplayPageCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.replay == nil {
		return 0
	}
	return s.replay.Retained()
}

func (s *Session) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	// Join order, which is the order the peers are torn down in when
	// the peers go one by one. Every connection is closed either way and nothing
	// is written to the wire by this, so the order here is a fact about the walk
	// and not about what a client sees.
	//
	// The list is collected under the lock and closed after releasing it: a
	// connection's Close reaches HandleDisconnect, which takes s.mu again, and a
	// sync.RWMutex is not reentrant.
	peers := make([]*Peer, 0, len(s.peerOrder))
	for _, id := range s.peerOrder {
		if p := s.Peers[id]; p != nil {
			peers = append(peers, p)
		}
	}
	s.mu.Unlock()
	for _, p := range peers {
		_ = p.Close()
	}
}

// Broadcast sends one message to every peer except excludePeerID. Framing is
// left to each peer's transport, so the same call is correct on bsPort and on
// the WebSocket port.
//
// It has no production caller since the top-level 0x01 handler was removed (2026-10-05,
// SESSION-JOURNAL.md 44.2): that handler was the only one that broadcast an AddPeer, and the
// dispatcher has no case for a top-level 0x01 at all. It stays because it is a primitive of
// the session, and because the peer-order tests are the ones that pin "every peer except
// this one, in join order". That it has no caller is written here so that nobody reads its
// presence as a live path.
func (s *Session) Broadcast(msgType byte, body []byte, excludePeerID byte) {
	s.ForEachPeer(func(p *Peer) {
		if p.ID != excludePeerID {
			_ = p.SendMessage(msgType, body)
		}
	})
}
