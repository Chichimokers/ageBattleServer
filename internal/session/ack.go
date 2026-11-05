package session

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"log"
	"sync"
	"time"
)

// AckState holds the per-session EndOfGameData (0x0A) tick state.
//
// One ack body is built per simulation tick (~125ms) from the session-wide state
// and the same body is sent to every player in the session (verified against
// capture _6: both players receive identical bodies). Once game data has started
// (a 0xFE GameData ack or a checksum ack), each tick emits one 0x0A message: the
// chunks the drain took off the peers' queues this tick (see PopTick), with the
// checksum entries merged on earlier ticks appended when they fit. A tick with
// nothing to flush still emits one, as a standalone checksum ack or a heartbeat.
//
// Format:
//
//	body       = [FE][subCount] + subs + [K] + pending + [blockLen] + block
//	sub        = [slot u8][len u16 LE][payload]
//	block      = [len u16 LE][02][N u16 LE] + 8N
//	           = [00 00]                               (no merged entries)
//	block      = absent when the peer version <= 1
//
// The [FE] byte is the message's own first body byte; the sub count is the number
// of subs, [K] is the pending-peer list's length and the block is serialized by
// the drain itself. Byte-verified against captures _6 (AoE2, 2 players) and _24
// (AoE1, 1 player).
//
// [K][pending...] enumerates the peers the tick did NOT take a chunk from on the
// drain's first pass: a participating peer whose queue was empty, or whose oldest
// chunk did not fit, in the queue's own order. A flush lists the peers absent
// from its subs (_6: slot-1 flush -> `01 02`, mixed (1,2) flush -> `00`), a
// standalone ack or heartbeat carries no subs so every participating peer is
// listed (_6: `02 01 02`; solo _24: `01 01`). When the only player's data fills
// the whole flush the list is empty and the tail degenerates to `00 00 00`
// (_24). The earlier reading — a fixed complement-slot pair — only matched
// two-player sessions by coincidence.
type AckState struct {
	mu      sync.Mutex
	started bool
	// queued holds each player slot's game-data chunks, oldest first, and what
	// they are charged against the per-peer caps, so a peer that uploads faster
	// than the tick drains hits the protocol's cap instead of growing without
	// bound.
	//
	// queued, perPeer and lastSync are ARRAYS indexed by the slot byte and not
	// maps. The tick's CPU profile is what decided it (SESSION-JOURNAL.md 39): at
	// 32 peers the map machinery is 282 ms of a 953-sample window, 29.6% of the
	// whole tick, and ReconcileChecksums alone spends 172 of its 230 ms inside
	// mapaccess1 because it looks a slot up twice a round.
	// The domain is a byte, so a map is a hash of a value that is already a
	// perfect index: 256 entries is 2 KiB of pointers against a hash table that
	// has to hash, probe and compare to reach the same entry. Nothing here is
	// ranged over, so the order a map would have given is never observed, and a
	// miss is nil exactly as an absent key was.
	//
	// The cost is 10 KiB of fixed size per AckState (2 KiB + 2 KiB of pointers,
	// 6 KiB of time.Time and 256 bytes of presence). An AckState is one per
	// SESSION and not one per peer, so this does not touch the per-client slope
	// that 37.7 and 38.1 bound; the measured live-heap delta is in 39.
	queued [256]*slotQueue
	// perPeer holds each peer's pending checksum entries, in arrival order. There is
	// one queue per peer and they are reconciled positionally on every tick (see
	// ReconcileChecksums), rather than funnelling every peer's entries into one
	// buffer.
	perPeer [256]*checksumQueue
	// merged is the reconciled list that goes into the 0x0A ack: one entry per
	// agreeing reconcile round. It stays here until a 0x0A has room for it.
	merged []byte
	// subs and pending are the 0x0A being built: the flushed sub-messages back to
	// back, and the pending-peer list. They are fields and not locals so neither
	// allocates on every tick: both are copied into the body before PopTick
	// returns, and nothing outside this file ever sees them.
	subs    []byte
	pending []byte
	// notice is the desync notice the last reconcile round built, nil unless that
	// round disagreed (see DesyncNotice).
	notice []byte
	// lastSync is when each player slot last uploaded game data or checksums, the
	// value the sync-timeout check compares against the peer's deadline.
	//
	// lastSyncSet is the presence half that a map's comma-ok used to give for
	// free, and it is a real array and not "is the time zero". A slot's entry is
	// written once and NEVER removed: DropPeer drops queued and perPeer and
	// leaves this one, which is the behaviour of the map it replaces, so presence
	// has to be remembered separately from the value rather than inferred from a
	// sentinel nobody promised never to store.
	lastSync    [256]time.Time
	lastSyncSet [256]bool
	// now is the clock the last-sync stamps read; nil means the wall clock. Tests
	// set it to step time without sleeping.
	now func() time.Time
}

// clock is the time a last-sync stamp records.
func (a *AckState) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

// LastSync reports when the given player slot last uploaded game data or
// checksums.
func (a *AckState) LastSync(slot byte) (time.Time, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastSync[slot], a.lastSyncSet[slot]
}

// QueueEmpty reports whether the slot has no pending sub-message. The
// sync-timeout check only runs against a peer whose game-data queue has drained;
// a peer still holding a backlog is never timed out on that pass.
func (a *AckState) QueueEmpty(slot byte) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	q := a.queued[slot]
	return q == nil || q.len() == 0
}

// SyncState answers the two questions the sync-timeout check asks about a slot,
// under ONE lock: whether its game-data queue has drained, and when it last
// uploaded. The check used to take this lock twice per peer per tick (QueueEmpty
// and LastSync), which at 32 peers was 64 acquisitions a tick for an answer the
// drain's own walk had already computed.
func (a *AckState) SyncState(slot byte) (empty bool, last time.Time, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	q := a.queued[slot]
	return q == nil || q.len() == 0, a.lastSync[slot], a.lastSyncSet[slot]
}

// DropPeer empties both of a slot's queues. It is what a peer's game data and
// checksum entries get on the way out, and there are two places that do it, the
// difference being only what happens to the peer:
//
//   - the LeaveSimulation (0x1F): the entry is emptied and KEEPS its slot, its
//     peer id and its place in the list. The peer stays.
//   - the disconnect: the entry is ERASED from both queues, and its queued
//     chunks are destroyed with it, so whatever the peer had uploaded and not yet
//     been flushed is never sent to anybody.
//
// The started flag is left alone, because it is the session's own "this match is
// carrying data" latch and the tick keeps reading it after the leave.
func (a *AckState) DropPeer(slot byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if q := a.queued[slot]; q != nil {
		for q.len() > 0 {
			subPool.Put(q.pop())
		}
	}
	// Dropping the entry is what a delete did to the map, and what it bought is
	// the same thing: the slotQueue or checksumQueue for a slot nobody is using
	// stops being reachable from here, so the collector can have it. Setting the
	// slot to nil is not a leak and not a reuse hazard, because every reader
	// already treats nil as "empty" (QueueEmpty) or as "no entries"
	// (pendingEntries).
	a.queued[slot] = nil
	a.perPeer[slot] = nil
}

// Started reports whether the session has begun carrying game data. The timeout
// is gated on the peer's "participating in the started game" mark, which is
// skipped when clear, so a peer of a session that never started is not reaped.
func (a *AckState) Started() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.started
}

// slotQueue is one player's queued game-data chunks, oldest first, and their
// charge against the per-peer byte cap.
//
// The chunks are read from a head index rather than re-sliced off the front, for
// the reason checksumQueue gives: popping from the front of a Go slice consumes
// its capacity. A tick takes at most ackDrainPasses chunks from a peer, so a
// queue can stay non-empty for many ticks; pop moves the live chunks back to the
// start once the consumed prefix is half the slice, so a queue that never fully
// drains does not grow without bound.
type slotQueue struct {
	subs []*subBuf
	head int
	// bytes is the payload bytes of the queued chunks (chunkCharge), which the
	// byte cap bounds.
	bytes int
}

// slotQueueCompactAt is the consumed prefix from which pop compacts the queue.
const slotQueueCompactAt = 32

// len is how many chunks are queued.
func (q *slotQueue) len() int { return len(q.subs) - q.head }

// front is the oldest queued chunk. The queue must not be empty.
func (q *slotQueue) front() *subBuf { return q.subs[q.head] }

// push queues a chunk and charges it.
func (q *slotQueue) push(sub *subBuf) {
	q.subs = append(q.subs, sub)
	q.bytes += chunkCharge(sub.b)
}

// pop takes the oldest chunk off the queue and refunds its charge. The queue must
// not be empty. The caller owns the returned buffer.
func (q *slotQueue) pop() *subBuf {
	sub := q.subs[q.head]
	q.subs[q.head] = nil
	q.head++
	if q.head == len(q.subs) {
		q.subs = q.subs[:0]
		q.head = 0
	} else if q.head >= slotQueueCompactAt && 2*q.head >= len(q.subs) {
		n := copy(q.subs, q.subs[q.head:])
		clear(q.subs[n:])
		q.subs = q.subs[:n]
		q.head = 0
	}
	q.bytes -= chunkCharge(sub.b)
	return sub
}

// chunkCharge is what a queued sub-message is charged against the byte cap: its
// payload, without the slot byte and the u16 length in front of it. A sub too
// short to hold those two is charged nothing.
func chunkCharge(sub []byte) int {
	return max(len(sub)-3, 0)
}

// QueueEntry is one record of the session's game-data queue, which is what the
// 0x0A's pending list is built from. Each record holds:
//
//	the peer's queue: one chunk per uploaded 0xFE, each holding that message's
//	own [len u16][body]
//	a byte counter, added by each upload and taken down by each drain
//	the peer id, the byte that reaches the wire
//
// One record is pushed per peer when the peer joins and erased when it
// disconnects, so the queue is in PEER-ADD ORDER and that is the order the
// pending list and the flushed sub-messages come out in. It is not the peer
// table's order and not the slot order: with the two apart, the peer that joined
// first is the one listed.
//
// The two flags are the per-record tests the drain makes on the peer the record
// names:
//
//   - Participating: a record whose peer is not in the started game is skipped
//     entirely, so it is neither flushed nor listed.
//   - TimedOut: an empty queue whose owner has passed its sync deadline gets
//     scheduled to be closed and is not listed either. Declared here so the drain
//     and the closer agree.
type QueueEntry struct {
	Slot          byte
	Participating bool
	TimedOut      bool
	// peer is the record's peer, carried so the timeout test does not have to
	// look it up again under the session lock. It is nil for a record a caller
	// built by hand, which is every test that constructs one.
	peer *Peer
}

// Per-player queue caps: a packet is dropped when the peer's queue already
// holds 0x12C entries or its payload bytes would reach 0x80000 (300 entries /
// 512 KiB).
const (
	maxAckQueueEntries = 0x12C
	maxAckQueueBytes   = 0x80000
)

// The per-tick drain limits (see PopTick).
const (
	// ackDrainPasses is how many passes one tick makes over the queue records at
	// most: the first, which also builds the pending list, and up to sixteen more
	// while a pass leaves chunks behind in a queue it took from.
	ackDrainPasses = 17
	// ackMaxSubs is the most sub-messages one 0x0A carries: the count is a u8,
	// and a chunk goes in only while the body holds fewer than this many.
	ackMaxSubs = 0xFF
	// ackMaxSize bounds the running size of the 0x0A, counted as the message type
	// byte plus the body: a chunk, and the checksum block, go in only if the size
	// stays strictly below it. The largest body is therefore 0xF7FD bytes plus the
	// pending list.
	ackMaxSize = 0xF7FF
	// ackBaseSize is the running size before any sub-message, pending entry or
	// checksum entry: the type byte, the [FE] marker, the sub count, the pending
	// count and the checksum block's u16 length.
	ackBaseSize = 6
)

// subBuf is one queued sub-message: the [slot][len u16 LE][payload] triple that
// QueueDataSub builds and PopTick copies into the ack body.
//
// It is a struct behind a pointer, and not a bare []byte, because that is what
// makes the pool free: sync.Pool stores an `any`, and putting a []byte in one
// RESERVES 24 bytes for the slice header every single time (measured: one object
// per peer per tick, SESSION-JOURNAL.md 37.7). A pointer is one word, fits in the
// interface word as it is, and the header it points at is reused in place.
//
// The ownership is the whole point and it is one line: a *subBuf is either in a
// slotQueue or in the pool, never in both and never in two places. PopTick
// copies every byte into the ack body BEFORE it puts one back, so nothing that
// leaves this file ever holds one.
type subBuf struct {
	b []byte
}

// subPoolFloor is the capacity every pooled sub starts with, which is what the
// 37's queue measurement is written against: a 65-byte sub in a 256-byte buffer
// plus a 24-byte header is 280 bytes of real memory per queued sub.
const subPoolFloor = 256

// subPool reuses the per-sub-message buffers queued by QueueDataSub and consumed
// by PopTick. PopTick copies every sub into the tick body before returning the
// buffer, so returning them here is safe.
var subPool = sync.Pool{
	New: func() any { return &subBuf{b: make([]byte, 0, subPoolFloor)} },
}

// QueueDataSub queues one game-data sub-message for the given player slot:
// [slot][len u16 LE][payload], where lenPayload is the client's
// [len u16 LE][payload] chunk.
//
// Each peer's queue is bounded (300 entries / 512 KiB of payload) and the
// incoming packet is dropped with a log when it is full; so here, per slot. The
// started flag and the last-sync stamp are set either way: the drop path still
// marks the peer as having sent data.
func (a *AckState) QueueDataSub(slot byte, lenPayload []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.started = true

	// The peer's last-sync time is stamped as the data arrives, and the drain
	// compares it against the deadline to reap a peer that stopped sending.
	a.lastSync[slot] = a.clock()
	a.lastSyncSet[slot] = true
	q := a.queued[slot]
	if q == nil {
		q = &slotQueue{}
		a.queued[slot] = q
	}

	// The charge is the payload alone, the client's chunk minus its u16 length,
	// which is what chunkCharge gives for the sub this would build.
	charge := max(len(lenPayload)-2, 0)
	if q.len() >= maxAckQueueEntries || q.bytes+charge >= maxAckQueueBytes {
		log.Printf("peer %d upload dropped: its game-data queue is full", slot)
		return
	}

	sub := subPool.Get().(*subBuf)
	sub.b = append(sub.b[:0], slot)
	sub.b = append(sub.b, lenPayload...)
	q.push(sub)
}

// checksumQueue is one peer's pending checksum entries: a flat run of 8-byte
// records with a read cursor at the front.
//
// It is one buffer and not a vector of 8-byte slices because of what the tick does
// to it. The reconciliation takes ONE record per peer per round and the round runs
// at most ten times a tick, so with the four records a peer uploads a tick the
// queue is emptied and refilled four times a second per peer. A [][]byte
// allocated a record and its backing array every time (measured: about seven
// objects a peer a tick, SESSION-JOURNAL.md 37.7), because popping from the front
// of a Go slice consumes its capacity and the next append has to grow a new array.
// Here the cursor advances and the buffer is only ever emptied back to zero
// LENGTH with its capacity intact, which is what a growable queue should do: only
// a queue refilled past its high-water mark ever reallocates.
type checksumQueue struct {
	buf []byte
	off int
}

// pending is how many whole records are still unread.
func (q *checksumQueue) pending() int { return (len(q.buf) - q.off) / 8 }

// front is the record the next reconcile round consumes, or nil when there is
// none. It is a window on buf, never a copy: the merged list is what gets copied.
func (q *checksumQueue) front() []byte {
	if q.pending() == 0 {
		return nil
	}
	return q.buf[q.off : q.off+8]
}

// push appends one 8-byte record.
func (q *checksumQueue) push(rec []byte) {
	q.buf = append(q.buf, rec...)
}

// pop consumes the front record, and empties the buffer the moment it is empty
// again so the next tick writes over the same bytes.
func (q *checksumQueue) pop() {
	q.off += 8
	if q.off >= len(q.buf) {
		q.buf = q.buf[:0]
		q.off = 0
	}
}

// QueueEntries stores a peer's checksum entries in that peer's own vector.
//
// entries is a run of 8-byte records, [crc u32 LE][page u32 LE]. They are kept
// per peer and in arrival order, because the tick reconciles them across peers
// (see ReconcileChecksums). An earlier revision of this code deduplicated by page
// index on the theory that "the first arrival wins"; the protocol does not do
// that, it reconciles the peers against each other.
//
// The upload also stamps the peer's last-sync time, as a game-data upload does,
// even when it carries no entry: a peer that keeps sending checksums is not
// reaped for a sync timeout. The caller only passes the slot of a peer of the
// session.
func (a *AckState) QueueEntries(slot byte, entries []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastSync[slot] = a.clock()
	a.lastSyncSet[slot] = true
	q := a.perPeer[slot]
	if q == nil {
		q = &checksumQueue{}
		a.perPeer[slot] = q
	}
	for off := 0; off+8 <= len(entries); off += 8 {
		q.push(entries[off : off+8])
	}
	if q.pending() > 0 {
		a.started = true
	}
}

// pendingEntries is how many checksum records a slot has waiting. It is the
// question DropPeer's emptiness and ReconcileChecksums' early return both ask, in
// one place so the cursor cannot be read two ways.
func (a *AckState) pendingEntries(slot byte) int {
	q := a.perPeer[slot]
	if q == nil {
		return 0
	}
	return q.pending()
}

// Reconciliation results.
//
// The caller runs the reconcile in a loop of at most ten rounds, breaking when it
// returns reconcileStop and counting the desync when it returns
// reconcileDesynced. Note that the stop condition is "nothing left to do", not
// "disagreement": a disagreement is counted and the loop keeps going.
const (
	reconcileAgreed    = 0 // every peer agreed; one entry consumed from each
	reconcileStop      = 1 // nothing to reconcile, or a peer still has no entry
	reconcileDesynced  = 2 // a desync notice is due (see DesyncNotice)
	reconcileMaxRounds = 10
)

// ReconcileChecksums is the per-tick cross-peer reconciliation, called up to
// reconcileMaxRounds times per tick from the session tick.
//
// It returns early unless every active peer has at least one stored entry. Then
// it compares the front entry of every active peer, both dwords, against the
// REFERENCE: the front entry of the first active peer whose entry is not (0,0).
// A (0,0) in front of the reference is a wildcard and is compared with nothing;
// once the reference is set, every later active peer must match it, (0,0)
// included.
//
//   - all agree: the reference goes onto the merged list, or (0,0) when every
//     front is (0,0), and reconcileAgreed is returned
//   - they disagree: the round builds its desync notice (DesyncNotice) and
//     reconcileDesynced is returned
//
// Either way one entry is popped from EVERY peer's queue, active or not.
// Consuming on disagreement too is what stops a stuck peer from wedging the
// queue, and it is why the caller can bound the loop at ten rounds; consuming
// from the peers outside the game is what keeps their queues from growing for as
// long as the game runs.
//
// "Active" is the participating peers: a record whose peer is not participating
// is skipped by both comparisons, and the walk carries on to the next record.
// entries is in queue order, so "first" is the peer that joined first, not the
// lowest slot. With no active peer at all there is nothing to reconcile.
//
// The comparison is against ONE reference and nothing else, which is what makes
// this linear in the peers and not quadratic: for every later peer there is
// exactly one bytes.Equal of two 8-byte records. A pairwise walk would compare
// every peer with every other, and the measured per-peer cost of this stage is flat
// between 8 and 32 peers (SESSION-JOURNAL.md 38).
func (a *AckState) ReconcileChecksums(entries []QueueEntry) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.notice = nil
	active := false
	for _, e := range entries {
		if !e.Participating {
			continue
		}
		// Every active peer must have something, or there is nothing to reconcile.
		if a.pendingEntries(e.Slot) == 0 {
			return reconcileStop
		}
		active = true
	}
	if !active {
		return reconcileStop
	}

	var ref []byte // nil until an active peer's front is not (0,0)
	agree := true
	for _, e := range entries {
		if !e.Participating {
			continue
		}
		front := a.perPeer[e.Slot].front()
		if ref == nil {
			if !bytes.Equal(front, zeroChecksum[:]) {
				ref = front
			}
			continue
		}
		if !bytes.Equal(front, ref) {
			agree = false
			break
		}
	}

	// Both outcomes read the fronts, so they run before any queue is popped.
	result := reconcileAgreed
	switch {
	case !agree:
		a.notice = a.desyncNoticeFor(ref, entries)
		result = reconcileDesynced
	case ref == nil:
		a.merged = append(a.merged, zeroChecksum[:]...)
	default:
		a.merged = append(a.merged, ref...)
	}
	for _, e := range entries {
		if q := a.perPeer[e.Slot]; q != nil && q.pending() > 0 {
			q.pop()
		}
	}
	return result
}

// zeroChecksum is the (0,0) entry: the wildcard in front of the reference, and
// what an all-(0,0) round merges.
var zeroChecksum [8]byte

// DesyncNotice is the body of the desync notice the last reconcile round built,
// for a GameData (0x09) message to every peer of the session, sent after the
// tick's 0x0A and in round order. It is nil unless that round returned
// reconcileDesynced. Every round builds a fresh slice, so the caller may hand it
// to the transport.
func (a *AckState) DesyncNotice() []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.notice
}

// desyncNoticeFor builds the notice of a round that disagreed with the given
// reference entry. The body is
//
//	[FB][len u16 LE][flag][payload]
//	payload = [01][ref u32 LE][count u16 LE] + count x {peer u8, d0 u32 LE, d1 u32 LE}
//
// with one element per active peer, in queue order, holding the front entry the
// round compared, and ref the reference entry's second dword. len counts the flag
// and the payload. The caller holds the lock and has not popped the fronts yet.
func (a *AckState) desyncNoticeFor(ref []byte, entries []QueueEntry) []byte {
	count := 0
	for _, e := range entries {
		if e.Participating {
			count++
		}
	}
	raw := make([]byte, 0, 7+9*count)
	raw = append(raw, 0x01)
	raw = append(raw, ref[4:8]...) // the second dword, already u32 LE
	raw = binary.LittleEndian.AppendUint16(raw, uint16(count))
	for _, e := range entries {
		if e.Participating {
			raw = append(raw, e.Slot)
			raw = append(raw, a.perPeer[e.Slot].front()...)
		}
	}
	blob := noticeBlob(raw)
	body := make([]byte, 0, 3+len(blob))
	body = append(body, 0xFB)
	body = binary.LittleEndian.AppendUint16(body, uint16(len(blob)))
	return append(body, blob...)
}

// noticeBlob is the notice payload behind its flag byte: flag 1 and the payload
// as a zlib stream when that is strictly smaller, otherwise flag 0 and the
// payload as it is. A compression error takes the raw form too.
//
// The protocol compresses with zlib at level 1. Go's levels 1 to 6 do not look
// for repeats in an input under 128 bytes, which is every notice of up to 13
// peers, so at level 1 a notice the protocol sends compressed would go raw.
// noticeLevel searches for repeats at every size, and on notice-sized payloads
// it lands within two bytes of zlib's level 1, so the two pick the same flag in
// all but borderline cases (measured: 44 of 45 payloads of 2 to 16 peers). The
// compressed bytes themselves are Go's; every zlib reader inflates them to the
// same payload.
func noticeBlob(raw []byte) []byte {
	var packed bytes.Buffer
	w := noticeDeflate.Get().(*zlib.Writer)
	w.Reset(&packed)
	_, werr := w.Write(raw)
	cerr := w.Close()
	w.Reset(nil)
	noticeDeflate.Put(w)
	if werr == nil && cerr == nil && packed.Len() < len(raw) {
		return append([]byte{0x01}, packed.Bytes()...)
	}
	return append([]byte{0x00}, raw...)
}

// noticeLevel is the zlib level the desync notice compresses with (see
// noticeBlob).
const noticeLevel = 7

// noticeDeflate reuses the zlib writers the desync notice compresses with. A
// writer allocates its tables on first use, and a session in a lasting desync
// builds up to ten notices a tick.
var noticeDeflate = sync.Pool{
	New: func() any {
		w, err := zlib.NewWriterLevel(nil, noticeLevel)
		if err != nil {
			panic(err) // a constant, valid level: unreachable
		}
		return w
	},
}

// PopTick builds the next tick's 0x0A body into a fresh allocation and advances
// the state. It is the form the tests use; the session calls PopTickInto with its
// frame store so the body is built where the store keeps it.
func (a *AckState) PopTick(entries []QueueEntry) []byte {
	return a.popTick(entries, nil)
}

// PopTickInto builds the next tick's 0x0A body inside the frame store's open
// block and advances the state. The store therefore keeps the exact bytes the
// players got without a second copy, which is what the original does: its tick
// allocates the 0x0A from the session's frame store and hands the same packet to
// every peer.
//
// The returned slice is the store's memory and is not published as a frame until
// the caller commits it (Replay.Commit). The caller must not retain it: every
// transport builds its own frame buffer out of it.
func (a *AckState) PopTickInto(entries []QueueEntry, store *Replay) []byte {
	return a.popTick(entries, store)
}

// popTick is the one implementation behind PopTick and PopTickInto. A nil store
// builds the body in a fresh allocation.
//
// entries is the session's game-data queue records in the queue's own order,
// which is peer-add order (see QueueEntry). It returns nil until the first game
// data or checksum ack has been queued.
//
// The body is filled by passes over the records, ackDrainPasses at most. Every
// pass walks all the records in order, and for each participating peer:
//
//   - its queue is empty: on the first pass the peer goes on the pending list,
//     unless it is being reaped for its sync timeout;
//   - its oldest chunk fits: the chunk goes into the body as a sub-message;
//   - its oldest chunk does not fit: the chunk stays queued, and on the first
//     pass the peer goes on the pending list.
//
// A chunk fits while the body holds fewer than ackMaxSubs sub-messages and the
// running size stays below ackMaxSize. A pass takes ONE chunk a peer, so the
// sub-messages interleave the peers in join order (A1 B1 A2 for two chunks of A
// and one of B). The next pass runs only if this one left a chunk behind in a
// queue it took from. Whatever is still queued after the last pass waits for the
// next tick, in place.
//
// The merged checksum entries go in only when the drain ended with nothing more
// to take, and only if they fit under the same two limits; otherwise they wait in
// the merged list. They are the previous ticks' entries: this tick's reconcile
// runs after the body is built.
func (a *AckState) popTick(entries []QueueEntry, store *Replay) []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.started {
		return nil
	}

	d := ackDrain{subs: a.subs[:0], pending: a.pending[:0], size: ackBaseSize}
	more := a.drainPass(entries, &d, true)
	for pass := 1; more && pass < ackDrainPasses; pass++ {
		more = a.drainPass(entries, &d, false)
	}

	// The block, [02][N u16 LE] + 8N, behind its own u16 length.
	n := len(a.merged) / 8
	blockLen := 3 + 8*n
	attach := n > 0 && !more && d.count < ackMaxSubs && d.size+blockLen < ackMaxSize
	if !attach {
		blockLen = 0
	}

	// The body is written straight into the store's open block when there is
	// one, so the store's copy of the ack is the ack itself. The size is exact
	// and known only after the drain, which is why the reservation is taken here
	// and not before it.
	size := 2 + len(d.subs) + 1 + len(d.pending) + 2 + blockLen
	var body []byte
	if store != nil {
		body = store.Reserve(size)
	} else {
		body = make([]byte, size)
	}
	off := 0
	body[off] = 0xfe
	body[off+1] = byte(d.count)
	off += 2
	off += copy(body[off:], d.subs)
	body[off] = byte(len(d.pending))
	off++
	off += copy(body[off:], d.pending)
	if attach {
		binary.LittleEndian.PutUint16(body[off:], uint16(blockLen))
		body[off+2] = 0x02
		binary.LittleEndian.PutUint16(body[off+3:], uint16(n))
		off += 5
		off += copy(body[off:], a.merged)
		a.merged = a.merged[:0]
	} else {
		body[off] = 0x00
		body[off+1] = 0x00
	}
	a.subs = d.subs[:0]
	a.pending = d.pending[:0]
	return body
}

// ackDrain is the 0x0A one tick's drain is building.
type ackDrain struct {
	subs    []byte // the flushed sub-messages, back to back
	count   int    // how many sub-messages subs holds
	pending []byte
	size    int // the running size: the type byte plus the body so far
}

// drainPass is one pass of the tick's drain over the queue records (see
// PopTick). first is the pass that builds the pending list. It reports whether
// a queue it took a chunk from still holds one, which is what earns another
// pass. The caller holds the lock.
func (a *AckState) drainPass(entries []QueueEntry, d *ackDrain, first bool) (more bool) {
	for _, e := range entries {
		if !e.Participating {
			continue
		}
		q := a.queued[e.Slot]
		if q == nil || q.len() == 0 {
			if first && !e.TimedOut {
				d.pending = append(d.pending, e.Slot)
				d.size++
			}
			continue
		}
		sub := q.front()
		if d.count >= ackMaxSubs || d.size+len(sub.b) >= ackMaxSize {
			if first {
				d.pending = append(d.pending, e.Slot)
				d.size++
			}
			continue
		}
		d.subs = append(d.subs, sub.b...)
		d.count++
		d.size += len(sub.b)
		// The bytes are in d.subs now, so the buffer goes back to the pool.
		subPool.Put(q.pop())
		if q.len() > 0 {
			more = true
		}
	}
	return more
}
