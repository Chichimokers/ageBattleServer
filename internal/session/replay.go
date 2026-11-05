package session

import "sync"

// Frame is one stored message of the frame store. The type travels with the
// body because a stored packet holds the whole serialized message, and both
// readers put it back on the wire unchanged: the observer update and the
// reconnect resend send the stored object, not a body re-wrapped in a type of
// this server's choosing.
type Frame struct {
	Type byte
	Body []byte
}

// frameOverhead is the 57 bytes the store charges to every frame it keeps, on
// top of the message size. It is what a frame costs against its block, and it is
// why a block holds fewer frames than its byte count divided by the frame size.
const frameOverhead = 57

// Replay is the per-session frame store.
//
// It is a list of blocks, and that is not a detail of the implementation: what an
// observer joining late can be given is exactly the window the store still
// holds, so the shape of the store is the shape of the answer the client gets.
//
//   - A block holds blockSize bytes: the -pageSize setting, default 65536. A
//     single frame larger than that gets a block of its own size, which is the
//     original's own rule (max(pageSize, need)).
//   - The store keeps at most maxBlocks blocks, ceil(peers * f) with peers
//     floored at 8, where f is the -maxPagesToStorePerSession scale, 1.0 by
//     default. 8 blocks in any real match.
//   - The oldest block is dropped when a new block is needed and the count is
//     already at the cap, so the retained window is the most recent
//     maxBlocks*blockSize bytes and never more.
//   - maxFrames is an optional cap in frames: 60000/ticksPerSec+1 until the game
//     announces itself observable with maxObservers > 0, which is what AoE2DE
//     does, so in practice it does not bound anything.
//
// The frames of one block are sub-slices of that block's buffer, so a block is
// one allocation for every frame it holds and the store's bytes are the block's
// bytes. A frame that is still referenced keeps its block alive after the store
// has dropped it, which is why dropping a block needs no reference count.
//
// Base() is the absolute number of the first frame still held, which both the
// StartObserveRequest handler and the observer update read.
type Replay struct {
	mu        sync.RWMutex
	frames    []Frame
	total     int   // frames ever appended, the store's frame numbering
	base      int   // the absolute number of frames[0]
	blockSize int
	maxBlocks int
	maxFrames int // <= 0 disables the frame cap
	blockAt   []int // absolute number of the first frame of each live block
	blockUsed int   // bytes charged to the last (open) block
	// blocks holds the live blocks' buffers, parallel to blockAt. The last one is
	// the open block, the one Reserve writes into; its capacity is the block's
	// size, which is blockSize unless one frame needed more.
	blocks [][]byte
	// spare is the buffer of the block that was just dropped, kept so the next
	// block reuses it instead of allocating. It is nil until the first roll.
	spare []byte
	// reserved is the slice Reserve handed out and Commit has not published yet.
	// It is nil between ticks.
	reserved []byte
}

// NewReplay creates a frame store of at most maxBlocks blocks of blockSize
// bytes, with an optional cap of maxFrames retained frames (<= 0 disables it).
func NewReplay(maxBlocks, blockSize, maxFrames int) *Replay {
	if maxBlocks < 1 {
		maxBlocks = 1
	}
	if blockSize < 1 {
		blockSize = 1
	}
	return &Replay{maxBlocks: maxBlocks, blockSize: blockSize, maxFrames: maxFrames}
}

// MaxPages reports the store's byte window, which is what the cap means now that
// the store is bounded in bytes rather than in frames.
func (r *Replay) MaxPages() int { return r.maxBlocks * r.blockSize }

// BlockSize is the -pageSize value the store was built with.
func (r *Replay) BlockSize() int { return r.blockSize }

// Total returns the number of frames ever appended. It is the observer range's
// exclusive upper bound: the observer update serves up to it and
// ActivateObserver measures the match length against it.
func (r *Replay) Total() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.total
}

// Retained returns how many frames the store still holds.
func (r *Replay) Retained() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.frames)
}

// Base returns the absolute number of the oldest retained frame. It is 0 while
// the store still holds everything and grows once the first block is dropped.
func (r *Replay) Base() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.base
}

// Reserve returns a slice of n bytes inside the open block for the caller to
// write a frame body into, opening a block when the current one cannot hold it.
// The bytes are NOT published: Commit does that, so a reader can never see a
// half-written frame.
//
// The returned slice is the store's own memory. The caller must write it and
// then call Commit before anything else reserves, and must not keep it after
// that: the next block roll recycles nothing, but a later Commit publishes it as
// a frame the store owns.
func (r *Replay) Reserve(n int) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	need := n + frameOverhead
	switch {
	case len(r.blockAt) == 0:
		r.openBlockLocked(need)
	case need+r.blockUsed > cap(r.openBlock()):
		// The open block is full. A new one is opened, and the oldest goes when
		// the count is already at the cap, which is the only place the store
		// ever forgets anything.
		if len(r.blockAt) >= r.maxBlocks {
			r.dropOldestBlockLocked()
		}
		r.openBlockLocked(need)
	}
	b := r.openBlock()
	start := len(b)
	b = b[:start+n]
	r.blocks[len(r.blocks)-1] = b
	r.reserved = b[start:]
	r.blockUsed += need
	return r.reserved
}

// Commit publishes the frame Reserve handed out as a frame of msgType. It is a
// no-op when nothing is reserved.
func (r *Replay) Commit(msgType byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	body := r.reserved
	r.reserved = nil
	if body == nil {
		return
	}
	if r.maxFrames > 0 && r.total-r.base+1 > r.maxFrames {
		r.trimToMaxFramesLocked()
	}
	r.frames = append(r.frames, Frame{Type: msgType, Body: body})
	r.total++
}

// AddFrame stores one message. The caller keeps ownership of nothing: the body
// is copied into the store's open block, because both callers hand over a buffer
// they are about to send or return to a pool.
func (r *Replay) AddFrame(msgType byte, body []byte) {
	copy(r.Reserve(len(body)), body)
	r.Commit(msgType)
}

// openBlock is the block Reserve writes into. The store must have one.
func (r *Replay) openBlock() []byte { return r.blocks[len(r.blocks)-1] }

// openBlockLocked opens a block able to hold a frame that needs `need` bytes
// charged against it, and makes it the open one. Called with the lock held.
//
// The buffer of the block that was just dropped is reused when it is large
// enough, which is what the original does and what makes a steady-state tick
// allocate nothing. It is safe only because no frame outlives the tick: the
// frames of the dropped block are gone from the store, and the one caller that
// hands frames out (ResendFrames) copies them.
func (r *Replay) openBlockLocked(need int) {
	size := max(r.blockSize, need)
	buf := r.spare
	r.spare = nil
	if cap(buf) < size {
		buf = make([]byte, 0, size)
	}
	r.blockAt = append(r.blockAt, r.total)
	r.blocks = append(r.blocks, buf[:0])
	r.blockUsed = 0
}

// dropOldestBlockLocked forgets the oldest live block. Called with the lock held
// and only when a new block is about to be opened, which is the only place the
// store ever forgets anything.
//
// The block's buffer is kept as the spare the next block reuses, and the frames
// that pointed into it are dropped. Both arrays are compacted with copy and their
// emptied tails cleared, because re-slicing them off the front would leave the
// dropped block reachable through the backing array and the collector would keep
// it alive.
func (r *Replay) dropOldestBlockLocked() {
	if len(r.blockAt) == 0 {
		return
	}
	r.spare = r.blocks[0]
	n := copy(r.blockAt, r.blockAt[1:])
	r.blockAt = r.blockAt[:n]
	n = copy(r.blocks, r.blocks[1:])
	clear(r.blocks[n:])
	r.blocks = r.blocks[:n]
	if len(r.blockAt) == 0 {
		r.base = r.total
		clear(r.frames)
		r.frames = r.frames[:0]
		return
	}
	drop := r.blockAt[0] - r.base
	r.base = r.blockAt[0]
	n = copy(r.frames, r.frames[drop:])
	clear(r.frames[n:])
	r.frames = r.frames[:n]
}

// trimToMaxFramesLocked drops whole blocks from the front until the retained
// frame count is back inside the cap. The cap is applied only when a block is
// opened, and whole blocks are dropped, so the window it leaves is a
// block-quantum shorter than the cap.
func (r *Replay) trimToMaxFramesLocked() {
	for len(r.blockAt) > 1 && r.total-r.base+1 > r.maxFrames {
		r.dropOldestBlockLocked()
	}
}

// GetFrames returns the retained frames whose absolute numbers fall in
// [first, last] inclusive, inclusive on both ends.
//
// It returns nil when the range is not wholly inside what the store holds, and
// it does NOT clamp: a last at or past the frame count and a first below the
// oldest retained frame are both refused, and both callers treat the refusal as
// an observer that cannot be updated or as a failed resend, leaving their own
// counter where it was. Clamping here would invent frames that are never sent.
//
// The returned slice shares the store's frames; callers must not modify them and
// must not retain them across an AddFrame call.
func (r *Replay) GetFrames(first, last int) []Frame {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.frames) == 0 || first < 0 || last < first {
		return nil
	}
	if last >= r.total || first < r.base {
		return nil
	}
	return r.frames[first-r.base : last-r.base+1]
}
