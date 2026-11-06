package session

import (
	"encoding/binary"
	"math"
	"runtime"
	"testing"

	"github.com/luskaner/ageBattleServer/internal/protocol"
)

// WHAT THIS FILE IS
//
// SESSION-JOURNAL.md 34 measured the cost of a LONG match: two hours of simulated
// time, 57600 ticks, with a flat 30 allocations a tick and 21 KiB of heap growth.
// That is the axis of TIME. This file is the other axis, the one nothing had ever
// asked: **how much does a client cost**. It sweeps the peer count and asserts the
// cost PER PEER, because the original says the cost per peer is a formula, and a
// formula can be asserted while a peak can only be reported.
//
// The formula, from the original and not from anybody's summary:
//
//  1. THE FRAME STORE IS ceil(peers * f) BLOCKS OF pageSize BYTES, with peers
//     floored at 8. The count is read from the length of the peer slot vector,
//     clamped up to eight, multiplied by f and rounded up; f comes from the
//     -maxPagesToStorePerSession scale, which is 1.0 by default and 0.125 when
//     the flag is given. So the store is 8
//     blocks of 65536 = 512 KiB: half a megabyte at any peer count up to eight,
//     and one 64 KiB block PER PEER above the floor.
//
//  2. EACH PEER'S UPLOAD QUEUE IS CAPPED AT 0x12C (300) ENTRIES OR 0x80000
//     (512 KiB) BYTES.
//
//	Both are per peer: the queue belongs to the peer's own 40-byte record, pushed
//	when it joins and erased when it disconnects. **Neither cap has a global limit
//	anywhere in the original**, so a session's worst case is the sum over its peers
//	and grows with the peer count. That is what the second half of this file
//	measures, and it is the only one of the two formulas with no ceiling at all.
//
// Observers are NOT peers here, and in the original they are not peers either:
// StartObserve appends to the observer vector and never touches the peer slot
// array, so an observer
// does not enter the ceil above, which counts the peer vector's length.
// TestObserversAreNotPeers pins that, because it is the difference between "the
// store grows with the number of CONNECTIONS" and "the store grows with the
// number of PLAYERS".
//
// EVERY CAP IN THIS FILE IS A NAMED CONSTANT, and the biggest thing it will ever
// allocate is written out here rather than discovered:
//
//	store at 32 peers:  ceil(32 * 1.0) = 32 blocks x 64 KiB = 2 MiB charged.
//	                    Every retained frame also costs a 32-byte Frame header
//	                    and Go rounds each body copy up to a size class, so the
//	                    real figure at 32 peers is about 2.1 MiB (measured below).
//	queues at 32 peers:  32 x 300 subs. With the small chunk this file uses
//	                    (65 bytes charged) that is 624 KiB charged and about
//	                    2.5 MiB of real buffers, because every sub is a pooled
//	                    buffer with a floor of 256 bytes.
//	byte cap:           exercised on ONE peer, with 128 KiB chunks, which reach
//	                    it in three subs (393219 of 524288 bytes charged) and
//	                    allocate 3 x 139264 = 409 KiB. Driving all 32 peers to
//	                    the BYTE cap instead would be 32 x 0x80000 = 16 MiB
//	                    charged and about 17 MiB real, which the fast path
//	                    deliberately does not do: the aggregate bound is
//	                    asserted instead, over a measured figure.
//	ticks:               4096 to fill a store, plus 512 to find its peak, plus
//	                    256 to measure the tick's cost. The fill is 2097 ticks at
//	                    one peer and 417 at thirty-two.
//
// So the peak of this file is ONE 32-peer point, about 5 MiB, and the points run
// one at a time. It was developed by running the 1-peer point alone, which is
// 512 KiB.

// ---------------------------------------------------------------------------
// THE SWEEP AXIS AND THE TWO SETTINGS, all of them fixed here.
// ---------------------------------------------------------------------------

// peerSweepTicksPerSecond is the other way round (8): the simulationPeriod every
// committed capture records.
const peerSweepTicksPerSecond = 8

// peerSweepPageSize is -pageSize, and 65536 is the original's own default.
const peerSweepPageSize = 0x10000

// peerSweepMaxPagesToStore is -maxPagesToStorePerSession. 0 is the original's
// default, so f = 1.0 and one block per peer above the floor of eight.
const peerSweepMaxPagesToStore = 0

// sweepPoint is one point of the sweep: how many peers, and how many blocks the
// binary's ceil gives for that count at f = 1.0.
//
// The blocks column is formula (1) evaluated BY HAND, not a call into the code
// under test: a test that recomputes the expected value with the three lines it
// is checking proves nothing, and the point of the column is that it is
// falsifiable. peerSweepStorePerPeer is that formula's per-peer term.
type sweepPoint struct {
	peers  int
	blocks int
}

var peerSweep = [...]sweepPoint{
	{1, 8}, {2, 8}, {4, 8}, {8, 8}, {16, 16}, {32, 32},
}

// peerSweepStorePerPeer is the per-peer term of formula (1) at these settings:
// one block of -pageSize for every peer above the floor of eight. A peer above
// the floor costs 65536 bytes of the
// operator's -pageSize setting, and that is the number the measured slope is
// asserted against.
const peerSweepStorePerPeer = peerSweepPageSize

// peerSweepFloorPeers is the count at which formula (1)'s floor stops being
// one. At and below it every point has to build the SAME 8-block store, which is
// asserted on maxBlocks and on the footprint rather than on a difference between
// two noisy byte counts.
const peerSweepFloorPeers = 8

// peerSweepMaxTicks bounds the fill loop. It is the CAP, not the length: each
// fixture stops as soon as every one of its store's blocks is live, and 4096 is
// twice what the slowest point needs (measured: 2097 ticks at one peer, where
// every frame is 250 charged bytes and the 512 KiB window takes 2096 of them).
const peerSweepMaxTicks = 4096

// peerSweepPeakScan is how many ticks are run AFTER the window is full, looking
// for the store's peak. The store's footprint is a sawtooth: it grows until the
// last block cannot take another frame, and then loses a whole block at once
// (Replay.AddFrame calls dropOldestBlockLocked exactly when a new block is needed
// and the count is already at the cap). The peak of one tooth is the whole
// window, and it is the figure formula (1) is about; anything else is a fraction
// of it that depends on where in the tooth the reading was taken.
//
// 512 covers at least one whole tooth at the slowest point (262 ticks at one
// peer, 13 at thirty-two), so the peak is found at every point.
const peerSweepPeakScan = 512

// peerSweepAllocTicks is how many ticks the per-tick allocation figure covers.
// It is taken in the steady state at the end, when the store is full and every
// tooth recycles a block, which is the state a real long match is in.
const peerSweepAllocTicks = 256

// peerSweepPasses is how many times the whole sweep runs, keeping the minimum of
// each point. The first measurement of this shape in a process reads tens of KiB
// high whatever it measures, because the allocator's own structures are cold
// (measured in SESSION-JOURNAL.md 34: 41 KiB on the first churn run against 3 KiB
// after it). The figure being resolved here is a 64 KiB-per-peer slope, so one
// pass would be resolving a signal against noise of the same order.
const peerSweepPasses = 2

// peerSweepUploadBytes is the payload of one 0xFE per peer per tick. 150 is what
// SESSION-JOURNAL.md 24 measured on the original's ack stream. It reaches the
// numbers in this file only through the size of one frame: the store's window is
// a BYTE window, so a bigger frame means fewer frames fit in it.
const peerSweepUploadBytes = 150

// peerSweepChecksumRecords is the number of 8-byte checksum records each peer
// uploads a tick. Four is inside what the original drains: the reconciliation
// takes ONE record per peer per round and the tick runs it at most ten times. A
// peer uploading more
// than ten a tick grows its own checksum queue in the ORIGINAL too, which is why
// this is a constant here and not something to raise.
const peerSweepChecksumRecords = 4

// peerSweepObservers is the observer axis: how many observers are seated while
// the peer count is swept. Observers are not peers and do not enter formula (1),
// which is the claim TestObserversAreNotPeers checks.
const peerSweepObservers = 64

// ---------------------------------------------------------------------------
// THE FIXTURE
// ---------------------------------------------------------------------------

// sweepConn counts and keeps NOTHING. A mock that recorded every message would
// be the fixture's own leak: an observer is served the same 0x0A the players get
// (SESSION-JOURNAL.md 24), so recording it costs a 5 KB copy per tick per
// observer and would dwarf the thing being measured.
type sweepConn struct {
	peerID     uint64
	msgVersion uint16
	sent       int
	acks       int
	bytes      int64
}

func (c *sweepConn) SendMessage(msgType byte, body []byte) error {
	c.sent++
	c.bytes += int64(len(body))
	if msgType == protocol.MsgEndOfGameData {
		c.acks++
	}
	return nil
}

func (c *sweepConn) Close() error                 { return nil }
func (c *sweepConn) RemoteAddr() string           { return "sweep" }
func (c *sweepConn) SetPeerIdentifier(v uint64)   { c.peerID = v }
func (c *sweepConn) PeerIdentifier() uint64       { return c.peerID }
func (c *sweepConn) ClientMessageVersion() uint16 { return c.msgVersion }

// sweepFixture is a session with a given number of players and a given number of
// observers, and one tick of it. Unlike longMatchDriver, whose peer count is a
// constant because its subject is TIME, this one's peer count is a parameter
// because the peer count is the subject. The bound on it is the sweep list above.
type sweepFixture struct {
	s        *Session
	players  []*sweepConn
	obs      []*sweepConn
	upload   []byte
	checksum []byte
	ticks    int
}

// newSweepFixture builds a session with peers players and observers observers,
// with the two settings the sweep holds fixed.
func newSweepFixture(peers, observers int) *sweepFixture {
	s := NewSession(1)
	s.pageSize = peerSweepPageSize
	s.maxPagesPerMatch = peerSweepMaxPagesToStore
	s.SetObserverConfig(observers+1, 8)
	// The 0x1D the game sends before the match: observable, and with
	// maxObservers > 0 the store's frame cap becomes -1,
	// so the block count is the only thing bounding it.
	s.SetObservableParameters(1, publishedHash, 0, peerSweepTicksPerSecond)

	d := &sweepFixture{
		s:        s,
		upload:   make([]byte, 2+peerSweepUploadBytes),
		checksum: make([]byte, 8*peerSweepChecksumRecords),
	}
	for i := range peers {
		c := &sweepConn{msgVersion: 5}
		s.AddPeer(NewPeer(byte(i+1), c))
		d.players = append(d.players, c)
	}
	for i := range observers {
		d.obs = append(d.obs, &sweepConn{peerID: uint64(0xA0 + i)})
	}
	return d
}

// tick is one simulation tick in the order the server's main loop runs them
// (server.go:374-376): the players' uploads, the 0x0A to every peer, and the
// session tick. Nothing here sleeps, which is what makes 4096 ticks of a
// 32-peer match cost milliseconds.
func (d *sweepFixture) tick(tick int) {
	binary.LittleEndian.PutUint16(d.upload, uint16(len(d.upload)-2))
	for off := 2; off < len(d.upload); off++ {
		d.upload[off] = byte(tick)
	}
	for off := 0; off < len(d.checksum); off += 8 {
		binary.LittleEndian.PutUint32(d.checksum[off:], uint32(tick*16))
		binary.LittleEndian.PutUint32(d.checksum[off+4:], uint32(tick))
	}
	for i := range d.players {
		d.s.QueueGameData(byte(i+1), d.upload)
		d.s.QueueChecksumEntries(byte(i+1), d.checksum)
	}
	body, notices := d.s.PopAckTick()
	if body != nil {
		d.s.SendToAll(protocol.MsgEndOfGameData, body)
	}
	for _, notice := range notices {
		d.s.SendToAll(protocol.MsgGameData, notice)
	}
	d.s.Tick()
	d.ticks++
}

// seatObservers runs the pair a client sends and the pair the captures show:
// StartObserve and then ActivateObserver. It reports rather than fails so the
// measurement loops carry no *testing.T: a refusal is a fixture fact, and each
// caller asserts on it in its own words.
func (d *sweepFixture) seatObservers() (seated int, refused bool) {
	for _, c := range d.obs {
		status, registered := d.s.StartObserve(c, c.peerID, 0)
		if status != 0 || !registered {
			return seated, true
		}
		if !d.s.ActivateObserver(c, peerSweepTicksPerSecond) {
			return seated, true
		}
		seated++
	}
	return seated, false
}

// runUntilAllBlocksLive drives the match until every one of the store's blocks
// is live at the same time, which is the moment the window is full: the next
// frame that does not fit will drop the oldest block (Replay.AddFrame calls
// dropOldestBlockLocked exactly then). Before it the store could be holding a
// fraction of its window and every figure below would be an underestimate.
//
// It returns how many ticks it took, and false if the cap stopped it first.
func (d *sweepFixture) runUntilAllBlocksLive() (int, bool) {
	for d.ticks < peerSweepMaxTicks {
		d.tick(d.ticks)
		if d.s.replay != nil && len(d.s.replay.blockAt) == d.s.replay.maxBlocks {
			return d.ticks, true
		}
	}
	return d.ticks, false
}

// ---------------------------------------------------------------------------
// WHAT IS MEASURED AT A POINT
// ---------------------------------------------------------------------------

// frameHeaderBytes is what a Frame costs in the store's own array: a 24-byte
// slice header plus the one-byte type, padded to the 8-byte alignment both need.
// It is written down rather than read with unsafe.Sizeof because this package
// does not import unsafe, and because it cannot matter to any bound here: at this
// fixture's frame size it is 32 bytes against about 5000 charged, under 1% of the
// store, and every ratio below has a 15% margin.
const frameHeaderBytes = 32

// sweepReading is one point of the sweep after its match has run.
//
// storeBytes is the store's OWN peak footprint in the bytes the allocator
// actually handed out: every retained frame's body copy at its capacity
// (Replay.AddFrame copies into append([]byte(nil), body...), whose capacity is
// the size class Go chose), plus the frame array at its capacity, plus the block
// index. It is exact and it needs no collection, which is why the slope is fitted
// on it. liveHeap is the whole process after a forced GC with this fixture still
// reachable, which is the figure that catches a per-client cost the store is not
// responsible for.
type sweepReading struct {
	point         sweepPoint
	fillTicks     int
	ticks         int
	blocks        int
	window        int
	full          bool
	peakRetained  int
	peakCharged   int
	peakStore     int
	liveHeap      uint64
	allocsPerTick float64
	minPlayerAcks int
	desyncs       int
}

// storeFootprint walks the store and returns the bytes the store CHARGES to
// itself (frameOverhead included) and the bytes
// the allocator HOLDS. Two different questions: the charged figure is the
// formula's own unit, the real one is what the process pays for it.
//
// The real figure is the live blocks' capacity, because the frames of a block are
// sub-slices of that block's buffer: a frame's own cap is the rest of its block
// and summing it would count the same bytes once per frame.
func storeFootprint(r *Replay) (charged, real int) {
	for i := range r.frames {
		charged += len(r.frames[i].Body) + frameOverhead
	}
	for _, b := range r.blocks {
		real += cap(b)
	}
	real += cap(r.frames) * frameHeaderBytes
	real += cap(r.blockAt) * 8 // one int per live block
	return charged, real
}

// runSweepPoint builds one point and measures it: the store's block count and
// peak footprint, the per-tick allocation cost in the steady state, and the live
// heap with the fixture still reachable.
func runSweepPoint(p sweepPoint, observers int) sweepReading {
	d := newSweepFixture(p.peers, observers)
	fillTicks, full := d.runUntilAllBlocksLive()

	// The peak scan: one whole sawtooth of the store's footprint, so the figure
	// below is the window formula (1) describes and not a fraction of it.
	var peakRetained, peakCharged, peakStore int
	for range peerSweepPeakScan {
		d.tick(d.ticks)
		charged, real := storeFootprint(d.s.replay)
		if real > peakStore {
			peakRetained, peakCharged, peakStore = d.s.replay.Retained(), charged, real
		}
	}

	// The per-tick allocation figure, in the steady state.
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range peerSweepAllocTicks {
		d.tick(d.ticks)
	}
	runtime.ReadMemStats(&after)

	minAcks := math.MaxInt
	for _, c := range d.players {
		minAcks = min(minAcks, c.acks)
	}
	r := sweepReading{
		point:         p,
		fillTicks:     fillTicks,
		ticks:         d.ticks,
		blocks:        d.s.replay.maxBlocks,
		window:        d.s.replay.MaxPages(),
		full:          full,
		peakRetained:  peakRetained,
		peakCharged:   peakCharged,
		peakStore:     peakStore,
		allocsPerTick: float64(after.Mallocs-before.Mallocs) / peerSweepAllocTicks,
		minPlayerAcks: minAcks,
		desyncs:       d.s.DesyncCount,
	}

	// The live heap, with the fixture ALIVE: this is the only place it can be
	// read, because a heap sampled after the fixture goes out of scope is the
	// test binary's own and says nothing about a session. KeepAlive after the
	// reading is what stops the collector from treating the fixture as garbage
	// while the number is being taken.
	r.liveHeap = liveHeapAfterGC()
	runtime.KeepAlive(d)
	return r
}

// liveHeapAfterGC is HeapAlloc after two forced collections. Two, not one:
// sync.Pool moves its primary cache into the victim cache on the first GC and
// drops the victim on the second, so a single GC leaves ack.go's subPool holding
// buffers that are not live and that would be charged to whatever fixture is
// being measured.
func liveHeapAfterGC() uint64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

// sweepAll runs the whole sweep peerSweepPasses times and keeps the minimum of
// each point. The minimum is the cold-allocator-free figure; a real per-peer
// cost is in every pass.
func sweepAll(observers int) []sweepReading {
	best := make([]sweepReading, len(peerSweep))
	for pass := range peerSweepPasses {
		for i, p := range peerSweep {
			r := runSweepPoint(p, observers)
			if pass == 0 || r.liveHeap < best[i].liveHeap {
				best[i] = r
			}
		}
	}
	return best
}

// fitSlope is the least-squares slope of y against x, in bytes (or objects) per
// peer, over the points at or above the floor. A max-against-baseline comparison
// says whether the peak is too big; the slope is what says whether ONE CLIENT
// costs what it should, which is the question this file is about.
func fitSlope(readings []sweepReading, pick func(sweepReading) float64) float64 {
	var sx, sy, sxx, sxy float64
	n := 0.0
	for _, r := range readings {
		if r.point.peers < peerSweepFloorPeers {
			continue
		}
		x := float64(r.point.peers)
		y := pick(r)
		sx, sy, sxx, sxy = sx+x, sy+y, sxx+x*x, sxy+x*y
		n++
	}
	den := n*sxx - sx*sx
	if n < 2 || den == 0 {
		return 0
	}
	return (n*sxy - sx*sy) / den
}

// ---------------------------------------------------------------------------
// THE BOUNDS, each with the number it was chosen from. Every figure below is
// read off runs of these tests in both normal and -race mode and is written down
// in SESSION-JOURNAL.md 37, so the next person can tighten rather than re-derive.
// ---------------------------------------------------------------------------
//
//	store blocks          8, 8, 8, 8, 16, 32 for 1, 2, 4, 8, 16, 32 peers
//	store window          512 KiB at and below the floor, then 64 KiB per peer
//	store charged/window  0.92 to 1.00, and not by choice: a block is closed when
//	                      the next frame does not fit, so a full block is short
//	                      by up to one frame and the window is short by up to one
//	                      frame per block (0.4% at one peer, 7.6% at 32)
//	store real/window     0.95 to 1.05: Go's size classes round each body copy
//	                      up (208 for a 193-byte ack, 5376 for a 4936-byte one)
//	                      and each frame adds a 32-byte header on top of the
//	                      57 the store charges it
//	allocs/tick           4 at one peer, 6.05 at 32: a FLAT count that does not
//	                      grow with the peers at all (0.00 objects a peer a
//	                      tick), after the pooling of SESSION-JOURNAL.md 38 took
//	                      it from 17 and 310. Every one of them is in the ack
//	                      path or in the fixture's own send
const (
	// peerSweepStoreWindowMinRatio and peerSweepStoreWindowMaxRatio bound the
	// store's PEAK real bytes against the window the formula declares. Measured
	// 0.95 to 1.05 across the six points, and the band is 0.85..1.40 because the
	// peak is a sawtooth peak: what it cannot be is half a window or two
	// windows, and those are 2x and 2x outside the band.
	peerSweepStoreWindowMinRatio = 0.85
	peerSweepStoreWindowMaxRatio = 1.40

	// peerSweepChargedSlopeMinRatio and peerSweepChargedSlopeMaxRatio are the
	// tightest bounds in this file, and they are on the store's CHARGED bytes,
	// which are the formula's own unit: what the store's own block accounting
	// subtracts per frame and what the ceil of the formula pays for. Measured
	// 0.99x. A store
	// that kept one block more or one block fewer than ceil(peers * f) is
	// outside 0.90..1.10, and it is the ratio that says so rather than the
	// allocator's rounding.
	peerSweepChargedSlopeMinRatio = 0.90
	peerSweepChargedSlopeMaxRatio = 1.10

	// peerSweepStoreSlopeMinRatio and peerSweepStoreSlopeMaxRatio are the same
	// statement about the bytes the ALLOCATOR hands out, which is what the
	// process actually pays. It is looser because Go rounds every body copy up
	// to a size class, and the rounding grows with the frame: a 193-byte ack
	// becomes a 208-byte object (7.8%) and a 4936-byte one a 5376-byte object
	// (8.9%), so the slope comes out at 1.10x the charged one. Measured 1.10x.
	// The band is 0.85..1.40, so a peer costing three times the formula fails,
	// and so does one costing half of it: a store that stopped keeping the
	// history it pays for answers a late observer with status 3. The ten-times
	// regression this
	// file was written for is 10.0 against a ceiling of 1.40.
	peerSweepStoreSlopeMinRatio = 0.85
	peerSweepStoreSlopeMaxRatio = 1.40

	// peerSweepHeapSlopeMinRatio and peerSweepHeapSlopeMaxRatio bound the LIVE
	// HEAP's slope against the same formula. It is looser than the store's own
	// because the heap is read at whatever point of the sawtooth the run ended
	// on (up to one block of jitter, which is 1/24 of the fitted interval) and
	// because it also carries the peer struct, two map entries and the
	// peer-order byte. Measured 0.96x to 1.03x across passes.
	peerSweepHeapSlopeMinRatio = 0.75
	peerSweepHeapSlopeMaxRatio = 1.75

	// peerSweepHeapCeiling is absolute, so that a per-client cost already inside
	// the baseline cannot slip through the slope. The biggest point of this
	// sweep measured 2.6 MiB of live heap with a full 32-block store; 32 MiB is
	// more than twelve times that, and the suite's own budget is 500 MB.
	peerSweepHeapCeiling = 32 << 20

	// peerSweepMaxAllocsPerPeerPerTick is the slope of the per-tick allocation
	// count against the peer count, over the points at or above the floor of eight.
	// It is a slope and not a per-point ratio: there is a fixed per-tick cost that
	// does not scale with the peers (the ack body, the store's copy of it, the
	// queue-record snapshot and the fixture's send closure), and a ratio reads that
	// fixed part as a per-peer cost.
	//
	// MEASURED 0,00 without the race detector and 0,50 with it (the detector's
	// shadow bookkeeping is itself per peer per tick). It used to be measured at
	// 9,30 and 9,81, and the bound was 12,0 because those two were too close
	// together for anything tighter to be about a defect and not about the
	// toolchain. That reason is gone: the per-peer allocations this path had are
	// pooled or reused away (SESSION-JOURNAL.md 38), so the same sensitivity the 37
	// declared -- three or more extra allocations a peer and a tick fail -- sits at
	// 3,0 with six times the margin over the race figure. The accounting it used to
	// name (a subPool.Put interface box, four 8-byte checksum records and the
	// backing array the reconciliation drained) is in 38 with what each of them is
	// now.
	peerSweepMaxAllocsPerPeerPerTick = 3.0

	// peerSweepMaxAllocsPerTick is the absolute ceiling at the largest point, so
	// that a cost which does not fit the per-peer shape (a leak of one object a
	// tick, say) still fails. Measured 6,05 at 32 peers (4 without the sort, which
	// only runs on a flush with mixed slots); 24 is four times that and catches
	// eighteen extra objects a tick.
	peerSweepMaxAllocsPerTick = 24
)

// TestPeerCountSweepMatchesTheStoreFormula is the sweep, and it says five
// separate things:
//
//   - the store's block count is the original's ceil at every point, evaluated
//     by hand in the table above and not by the code under test;
//   - the store's peak footprint is that block count's window at every point;
//   - the store's cost PER PEER is one -pageSize block above the floor of eight,
//     in both directions: this is the "each extra peer costs about what the
//     formula says" claim, and it is a slope, not a peak;
//   - the per-client bookkeeping outside the store is small enough not to be
//     hiding a per-client allocation, read off the live heap and off the
//     allocation count per tick;
//   - and every point's match actually ran, because a memory test over a store
//     that never filled measures an empty store eight times.
func TestPeerCountSweepMatchesTheStoreFormula(t *testing.T) {
	readings := sweepAll(0)

	t.Logf("peer sweep: %d passes (min of each), -pageSize %d, f = 1.0, fill cap %d ticks, peak scan %d ticks, "+
		"allocs/tick over the last %d", peerSweepPasses, peerSweepPageSize, peerSweepMaxTicks, peerSweepPeakScan,
		peerSweepAllocTicks)
	t.Logf("%4s %6s %8s %10s %10s %10s %9s %8s %7s %7s", "peers", "blocks", "window", "store KiB", "charged KiB",
		"frames", "heap KiB", "allocs/t", "fill", "ticks")
	for _, r := range readings {
		t.Logf("%4d %6d %7dK %10.1f %10.1f %10d %9d %8.2f %7d %7d", r.point.peers, r.blocks, r.window>>10,
			float64(r.peakStore)/1024, float64(r.peakCharged)/1024, r.peakRetained, r.liveHeap>>10,
			r.allocsPerTick, r.fillTicks, r.ticks)
	}

	// --- the match really ran, at every point -------------------------------
	for _, r := range readings {
		p := r.point.peers
		if !r.full {
			t.Errorf("%d peers: the store never had all %d of its blocks live in %d ticks, so every figure for "+
				"this point is the cost of a store with room to spare rather than the cost of a full one",
				p, r.point.blocks, peerSweepMaxTicks)
		}
		if want := r.ticks; r.minPlayerAcks != want {
			t.Errorf("%d peers: the quietest player received %d acks for %d ticks. One 0x0A a tick to every peer "+
				"is the original's behaviour, and an ack that goes missing would make the memory figures "+
				"irrelevant", p, r.minPlayerAcks, want)
		}
		if r.desyncs != 0 {
			t.Errorf("%d peers: %d desync notices, with %d identical checksum records a tick from every player. "+
				"They are supposed to agree, so the reconciliation ran and never once disagreed, which is what "+
				"makes the per-peer allocation figure mean anything", p, r.desyncs, peerSweepChecksumRecords)
		}
		// The formula itself, checked against a column written by hand.
		if r.blocks != r.point.blocks {
			t.Errorf("%d peers: the store was built with %d blocks, and with f = 1.0 the formula says "+
				"ceil(max(%d,8) * 1.0) = %d. Every other figure in this row is about a store that is not the one "+
				"the formula describes", p, r.blocks, p, r.point.blocks)
		}
		if r.window != r.blocks*peerSweepPageSize {
			t.Errorf("%d peers: the store's window is %d bytes for %d blocks of %d", p, r.window, r.blocks,
				peerSweepPageSize)
		}
		if r.liveHeap > peerSweepHeapCeiling {
			t.Errorf("%d peers: the live heap reached %d KiB, over the %d KiB absolute ceiling", p, r.liveHeap>>10,
				peerSweepHeapCeiling>>10)
		}
	}

	// --- the store's peak footprint against the formula's window -------------
	for _, r := range readings {
		ratio := float64(r.peakStore) / float64(r.window)
		if ratio < peerSweepStoreWindowMinRatio || ratio > peerSweepStoreWindowMaxRatio {
			t.Errorf("%d peers: the store's peak is %d real bytes against the %d byte window its %d blocks of %d "+
				"bytes declare, a ratio of %.2f over a budget of %.2f..%.2f. A store that does not fill its window "+
				"is throwing away history, which is what answers a late observer with status 3; one that exceeds it "+
				"is keeping something the formula does not account for",
				r.point.peers, r.peakStore, r.window, r.blocks, peerSweepPageSize, ratio,
				peerSweepStoreWindowMinRatio, peerSweepStoreWindowMaxRatio)
		}
	}

	// --- the floor: at and below eight peers the store is the same store -----
	// ceil(max(n,8) * 1.0) is 8 for 1, 2 and 4 peers,
	// and the maxBlocks assertion above already says the four floor points built
	// the same store. What is left is that they also HOLD the same amount, which
	// is the per-point ratio above plus this: the four windows are identical
	// numbers, and a spread across them would be the store growing with the peer
	// count where the original says it cannot.
	floorLo, floorHi := math.MaxInt64, 0
	for _, r := range readings {
		if r.point.peers > peerSweepFloorPeers {
			continue
		}
		if r.window != peerSweep[0].blocks*peerSweepPageSize {
			t.Errorf("%d peers: the window is %d bytes, and every point at or below the floor of %d has to have "+
				"the same %d", r.point.peers, r.window, peerSweepFloorPeers, peerSweep[0].blocks*peerSweepPageSize)
		}
		floorLo = min(floorLo, r.peakStore)
		floorHi = max(floorHi, r.peakStore)
	}
	t.Logf("at and below the floor of %d peers: %d blocks and a %d byte window at every point, peak store "+
		"footprint %d..%d bytes", peerSweepFloorPeers, peerSweep[0].blocks, peerSweep[0].blocks*peerSweepPageSize,
		floorLo, floorHi)

	// --- the slope, which is the actual claim -------------------------------
	// Three slopes, because "the store" is three numbers: the bytes it CHARGES
	// to itself (the formula's own unit, which is what the block count and the
	// per-block size are about), the bytes the allocator HANDS OUT for it
	// (which carry Go's size-class rounding, so its slope is a little above the
	// charged one: 1.10x measured against 0.99x), and the live heap of the
	// whole process (which carries both plus the per-client bookkeeping).
	chargedSlope := fitSlope(readings, func(r sweepReading) float64 { return float64(r.peakCharged) })
	storeSlope := fitSlope(readings, func(r sweepReading) float64 { return float64(r.peakStore) })
	heapSlope := fitSlope(readings, func(r sweepReading) float64 { return float64(r.liveHeap) })
	allocSlope := fitSlope(readings, func(r sweepReading) float64 { return r.allocsPerTick })
	t.Logf("slope over the points at or above the floor of %d peers: store CHARGED %.0f B/peer (%.2fx of the "+
		"formula's %d), store REAL %.0f B/peer (%.2fx, the difference being Go's size classes), live heap "+
		"%.0f B/peer (%.2fx); allocations %.2f objects/tick/peer over a budget of %.1f",
		peerSweepFloorPeers, chargedSlope, chargedSlope/peerSweepStorePerPeer, peerSweepStorePerPeer, storeSlope,
		storeSlope/peerSweepStorePerPeer, heapSlope, heapSlope/peerSweepStorePerPeer, allocSlope,
		peerSweepMaxAllocsPerPeerPerTick)

	// The charged slope is the formula's own arithmetic, so its band is the
	// tightest one here: measured 0.99x, and a store that kept one block more or
	// one block fewer than the formula says at each peer count is outside 0.90.
	if lo := peerSweepStorePerPeer * peerSweepChargedSlopeMinRatio; chargedSlope < lo {
		t.Errorf("each peer above the floor of %d costs %.0f bytes of frame store as the store CHARGES them, where "+
			"the formula says %d (%.3fx), under the floor of %.0f. The charged bytes are what the store's own "+
			"block accounting subtracts per frame and what the ceil of the formula pays for, so this is the "+
			"formula's own number and not a measurement of it",
			peerSweepFloorPeers, chargedSlope, peerSweepStorePerPeer, chargedSlope/peerSweepStorePerPeer, lo)
	}
	if hi := peerSweepStorePerPeer * peerSweepChargedSlopeMaxRatio; chargedSlope > hi {
		t.Errorf("each peer above the floor of %d costs %.0f bytes of frame store as the store CHARGES them, "+
			"%.3fx the %d the formula says (ceil(peers * f) blocks of -pageSize bytes); the ceiling is %.0f",
			peerSweepFloorPeers, chargedSlope, chargedSlope/peerSweepStorePerPeer, peerSweepStorePerPeer, hi)
	}
	if lo := peerSweepStorePerPeer * peerSweepStoreSlopeMinRatio; storeSlope < lo {
		t.Errorf("each peer above the floor of %d costs %.0f bytes of frame store where the formula says %d "+
			"(%.2fx), under the floor of %.0f. A peer that costs less than a block is the store not keeping the "+
			"history the formula pays for, and the first thing that shows is a late observer answered with "+
			"status 3", peerSweepFloorPeers, storeSlope, peerSweepStorePerPeer, storeSlope/peerSweepStorePerPeer, lo)
	}
	if hi := peerSweepStorePerPeer * peerSweepStoreSlopeMaxRatio; storeSlope > hi {
		t.Errorf("each peer above the floor of %d costs %.0f bytes of frame store, %.2fx the %d the formula says "+
			"(the formula multiplies the peer count by f and keeps that many blocks of "+
			"-pageSize bytes); the ceiling is %.0f. One client costing more than a block is the defect this "+
			"file exists to fail on", peerSweepFloorPeers, storeSlope, storeSlope/peerSweepStorePerPeer,
			peerSweepStorePerPeer, hi)
	}
	if lo := peerSweepStorePerPeer * peerSweepHeapSlopeMinRatio; heapSlope < lo {
		t.Errorf("the live heap grows %.0f bytes per peer above the floor where the formula accounts for %d, "+
			"under the floor of %.0f", heapSlope, peerSweepStorePerPeer, lo)
	}
	if hi := peerSweepStorePerPeer * peerSweepHeapSlopeMaxRatio; heapSlope > hi {
		t.Errorf("the live heap grows %.0f bytes per peer above the floor of %d peers, %.2fx the %d the store "+
			"formula accounts for and over the ceiling of %.0f. The store itself accounts for %.0f of those "+
			"bytes a peer, and the two figures differing by %.0f is the per-client cost outside the formula",
			heapSlope, peerSweepFloorPeers, heapSlope/peerSweepStorePerPeer, peerSweepStorePerPeer, hi,
			storeSlope, heapSlope-storeSlope)
	}
	if allocSlope > peerSweepMaxAllocsPerPeerPerTick {
		t.Errorf("the tick allocates %.2f more objects for each extra peer, over a budget of %.1f (measured 0.00, and "+
			"0.50 under -race, whose shadow bookkeeping is per peer per tick: this path used to spend one interface box "+
			"per subPool.Put, four 8-byte checksum records and about three backing arrays a peer, and SESSION-JOURNAL.md "+
			"38 says where each of them went). Anything above that is a per-client allocation on a path that has none",
			allocSlope, peerSweepMaxAllocsPerPeerPerTick)
	}
	if last := readings[len(readings)-1]; last.allocsPerTick > peerSweepMaxAllocsPerTick {
		t.Errorf("at %d peers the tick allocates %.1f objects, over the absolute ceiling of %d", last.point.peers,
			last.allocsPerTick, peerSweepMaxAllocsPerTick)
	}
}

// ---------------------------------------------------------------------------
// THE OBSERVER AXIS
// ---------------------------------------------------------------------------

// observerPlayers is the peer count of the observer fixture: two, the shape of
// capture _6, and below the floor of eight so the store's block count IS the
// floor and any movement of it is visible.
const observerPlayers = 2

// observerSteadyTicks is how long the observer fixture runs once seated. It has
// to cover at least one serve per observer: with the observer update interval at
// its default 8
// ticks a visit lands every ninth tick, and the fixture wants every observer
// visited at least twice, which 256 ticks is six times over.
const observerSteadyTicks = 256

// observerReading is one run of the observer axis. ok reports that the fixture
// did what it says it does (the store filled, every observer was seated), so the
// measurement loop can carry no *testing.T and the test can say what went wrong
// in its own words, which is the idiom longmatch_test.go uses for its refusal.
type observerReading struct {
	ok            bool
	observers     int
	peers         int
	blocks        int
	window        int
	seated        int
	served        int
	heap          uint64
	allocsPerTick float64
}

// observerPoint runs one observer fixture: fill the store, seat the observers,
// then run the match long enough for the per-tick figures to be about a session
// that HAS observers rather than one that refused them.
func observerPoint(observers int) observerReading {
	d := newSweepFixture(observerPlayers, observers)
	_, full := d.runUntilAllBlocksLive()
	seated, refused := d.seatObservers()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range observerSteadyTicks {
		d.tick(d.ticks)
	}
	runtime.ReadMemStats(&after)

	served := 0
	for _, c := range d.obs {
		served += c.acks
	}
	r := observerReading{
		ok:            full && !refused,
		observers:     observers,
		peers:         d.s.PeerCount(),
		blocks:        d.s.replay.maxBlocks,
		window:        d.s.replay.MaxPages(),
		seated:        seated,
		served:        served,
		allocsPerTick: float64(after.Mallocs-before.Mallocs) / observerSteadyTicks,
	}
	r.heap = liveHeapAfterGC()
	runtime.KeepAlive(d)
	return r
}

// observerHeapMin is the smallest of observerHeapPasses runs, for the same
// reason the sweep takes two passes: the first measurement of a heap in a process
// reads high whatever it measures (SESSION-JOURNAL.md 34).
const observerHeapPasses = 2

// TestObserversAreNotPeers is the third axis, and it is a yes/no question with a
// formula behind it: does an observer cost the frame store anything?
//
// It must not. StartObserve appends to the observer vector and never writes a
// peer slot, and the store's block
// count is a ceil over the PEER slot vector's length. So 64 observers on a
// two-player
// session is still an 8-block store, and if it were not then a lobby full of
// spectators would silently multiply the memory of every match.
//
// The second half of the question is whether an observer costs anything PER
// TICK, because the observer update is the one thing in the session that runs
// per tick per observer. It must not:
// the update reads the store through GetFrames, which returns a subslice of the
// frames the store already holds, and sends them. Both are the reason the
// allocation count with 64 observers has to be the count with none.
func TestObserversAreNotPeers(t *testing.T) {
	none := observerReading{}
	with := observerReading{}
	for range observerHeapPasses {
		if a := observerPoint(0); none.heap == 0 || a.heap < none.heap {
			none = a
		}
		if a := observerPoint(peerSweepObservers); with.heap == 0 || a.heap < with.heap {
			with = a
		}
	}
	perObserver := float64(with.heap-none.heap) / peerSweepObservers

	t.Logf("observers: %d seated on a %d-player session; store %d blocks (%d bytes) with none and %d blocks "+
		"(%d bytes) with %d; served %d frames over %d ticks; live heap %d KiB against %d KiB, so %.0f bytes an "+
		"observer; allocs/tick %.2f against %.2f",
		with.seated, observerPlayers, none.blocks, none.window, with.blocks, with.window, peerSweepObservers,
		with.served, observerSteadyTicks, with.heap>>10, none.heap>>10, perObserver, with.allocsPerTick,
		none.allocsPerTick)

	// --- they were seated, and they were served ---------------------------
	if !none.ok || !with.ok {
		t.Fatalf("the observer fixture did not run: with no observers the store filled %t and %d were seated, "+
			"with %d the store filled %t and %d were seated. An observer that cannot be seated answers "+
			"StartObserve with 3 once the store has dropped its first frame, and every "+
			"figure below would then be about a session with no observers at all",
			!none.ok, none.seated, peerSweepObservers, !with.ok, with.seated)
	}
	if with.seated != peerSweepObservers || with.served == 0 {
		t.Fatalf("%d of %d observers were seated and they were served %d frames: a memory figure about "+
			"observers is only about observers if they were there and streaming",
			with.seated, peerSweepObservers, with.served)
	}

	// --- they are not peers, and they cost the store nothing ---------------
	if with.peers != observerPlayers {
		t.Errorf("the session holds %d peers after %d observers joined, want %d. StartObserve appends to the "+
			"observer vector and never writes a peer slot, which is the array the store's block count is a "+
			"ceil over", with.peers, peerSweepObservers, observerPlayers)
	}
	if with.blocks != none.blocks || with.window != none.window {
		t.Errorf("the store went from %d blocks (%d bytes) to %d blocks (%d bytes) when %d observers joined. "+
			"The block count is ceil(max(peers, 8) * f) over the PEER vector, so "+
			"observers must not move it: a spectator would otherwise multiply every match's memory",
			none.blocks, none.window, with.blocks, with.window, peerSweepObservers)
	}
	if with.blocks != peerSweep[0].blocks {
		t.Errorf("a %d-player session's store has %d blocks, want %d: ceil(max(%d,8) * 1.0)",
			observerPlayers, with.blocks, peerSweep[0].blocks, observerPlayers)
	}

	// --- what an observer costs, and what it must not cost -----------------
	// An observer is a 16-byte entry in the original (a connection pointer, a u32
	// progress and two flag bytes) and a counting
	// connection here, so its live cost is tens of bytes. 1 KiB is over twenty
	// times that and still fails on a per-observer leak of 1024 bytes, which is
	// the shape of the defect this bound is for.
	const perObserverBudget = 1 << 10
	if perObserver > perObserverBudget {
		t.Errorf("%d observers cost %.0f bytes of live heap each, over the %d allowed. An observer entry is 16 "+
			"bytes in the original; anything near 1 KiB is something per observer "+
			"that should not be there", peerSweepObservers, perObserver, perObserverBudget)
	}

	// The sharp one, and the one with the most leverage: the observer update is the
	// only thing in the session that runs per tick PER OBSERVER, and it must not
	// allocate at all. It reads the store through GetFrames, which returns a
	// subslice of frames the store already holds, and hands them to SendMessage.
	// With 64 observers and the update interval at 8 ticks the stride visits 8 of
	// them a
	// tick, so the budget is one
	// object a tick for the whole update whatever the observer count: a defect that
	// copies the replay per observer reads 7.68 here and a defect that copies it
	// per VISITED observer reads 8.
	const observerAllocBudget = 1.0
	if got := with.allocsPerTick - none.allocsPerTick; got > observerAllocBudget {
		t.Errorf("%d observers being served cost %.2f extra allocations per tick over a budget of %.1f, and the "+
			"budget does not scale with them. The update reads the store through GetFrames, which returns a "+
			"subslice of frames the store already holds (Replay.GetFrames), and sends them: an allocation here "+
			"is a copy of the replay per observer per tick",
			peerSweepObservers, got, observerAllocBudget)
	}
}
