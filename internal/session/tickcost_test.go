package session

import (
	"encoding/binary"
	"fmt"
	"math"
	"runtime"
	"runtime/debug"
	"testing"
	"time"

	"github.com/luskaner/ageBattleServer/internal/protocol"
)

// WHAT THIS FILE IS, AND WHY IT IS A TEST AND NOT A DOCUMENT
//
// The 34 measured the cost of a LONG match (two hours, 57600 ticks, 21 KiB of
// heap growth, 30 allocations a tick, of which 9,3 a tick were per peer). The 37
// measured the cost PER CLIENT (the store's blocks per peer, the two upload-queue
// caps, and those same 9,30 objects a peer a tick). Neither of them said what a
// tick COSTS, only how much it allocates and how much it holds: both of those
// figures are exact and neither needs a clock, which is why they are the ones that
// got asserted.
//
// This file is the missing third axis, and it is a test because a cost that is
// only written down is a cost nobody re-measures. It answers:
//
//   - how long one tick takes at each peer count, at the recorded simulation
//     period (125 ms, the -simulationPeriod every committed capture carries);
//   - what that adds up to over a real match, with the arithmetic printed;
//   - what part of the tick the time goes in, by measuring the same fixture with
//     pieces of the tick taken away;
//   - what the tick leaves behind: the steady-state heap at each peer count (read
//     off the 37's own sweep, in the same state it measures) and the transient
//     garbage of ONE tick.
//
// The clock discipline is the 34's and it is not optional: this machine's
// monotonic clock is quantised, so timing one tick reads 0 for most of them
// (measured there: 19 zeros out of 55 single buckets). Every figure here is a
// whole window of costBucketTicks ticks timed with ONE pair of readings, repeated
// costRepeats times, keeping the fastest window: load only ever adds time, so the
// minimum is the load-free figure. What that cannot hide is a tick that really
// got slower, because that is in every repetition.
//
// Every cap here is a named constant, and the biggest thing this file allocates is
// the 37's: one 32-peer fixture with a 2 MiB store, run a point at a time.

// ---------------------------------------------------------------------------
// THE AXES, all of them fixed here
// ---------------------------------------------------------------------------

// costBucketTicks is the window one pair of clock readings covers. 4096 ticks at
// the slowest point measured here (32 peers) is tens of milliseconds, which is the
// shortest window this machine's clock resolves without the figure being a
// lottery; the 34 used 14336 for the same reason.
const costBucketTicks = 4096

// costRepeats is how many windows each point is timed for, keeping the fastest.
// One window is a measurement and three is a habit: the 34 measured that going
// from one repetition to two moved the worst observed late/early ratio from 1,26
// to 1,02.
const costRepeats = 3

// costStageBucketTicks and costStageRepeats are the same two things for the
// decomposition, one size down: a stage is measured as the DIFFERENCE between two
// whole ticks, so the window has to be long enough that the difference clears the
// clock's resolution and short enough that six stages and two peer counts fit in
// the fast path's budget.
const (
	costStageBucketTicks = 2048
	costStageRepeats     = 3
)

// costWarmTicks is how many ticks of the mode being measured run before the
// window opens, so the store, the pools and the maps are in the state the figure
// describes and not the state the fixture was built in.
const costWarmTicks = 256

// costTransientBytes is the ceiling on what ONE transient measurement may
// allocate, and it is also what keeps the collector out of it: with the collector
// off the heap goal is Go's 4 MiB floor, so a window that allocates more than
// this starts collecting and the figure stops being "what a tick leaves behind".
// costTransientTicks is the window's cap and this is its budget, and the window
// is shortened to fit it (measureTransient).
const costTransientBytes = 1 << 20

// costTransientTicks is the window the transient figure is measured over, with the
// collector switched OFF (see measureTransient). It is the cap on the ticks in
// that window, so raising it is a decision and not an accident.
const costTransientTicks = 256

// costTransientMinTicks is the shortest window that still divides: at the largest
// peer count one tick allocates tens of KiB, so the byte budget above is reached
// in a few dozen ticks and a window of one tick would be a reading of the
// collector's own timing.
const costTransientMinTicks = 32

// costMatchHours is the horizon the extrapolation is drawn on. Two hours is the
// 34's horizon and also a long AoE2DE match.
const costMatchHours = 2

// costTicksPerSecond is the other way round (1000 / the
// -simulationPeriod), and 125 ms is what every committed capture records.
const costTicksPerSecond = 8

// costTicksPerMatch is costMatchHours at costTicksPerSecond, written as the
// arithmetic rather than a literal so the report and the code cannot disagree.
var costTicksPerMatch = costMatchHours * 3600 * costTicksPerSecond

// ---------------------------------------------------------------------------
// THE STAGES: the same fixture with pieces of the tick taken away
// ---------------------------------------------------------------------------

// costMode is one shape of tick. Every stage of the ack path is either present or
// not in one of these, and the cost of a stage is the difference between two modes
// that differ only in it. That is what makes the split attributable: the fixture,
// the peer count, the store and the clock are the same in both.
type costMode struct {
	name string
	// uploads is how many 0xFE game-data sub-messages each peer queues a tick
	// (AckState.QueueDataSub), and therefore how many subs the drain flushes.
	uploads int
	// checksums is how many 8-byte checksum records each peer queues a tick
	// (AckState.QueueEntries), and therefore how many reconcile rounds the tick
	// runs: the original takes one record per peer per round, ten rounds
	// at most, and four is what the 37's fixture uses.
	checksums int
	// observers is how many observers are seated and being served.
	observers int
}

// The modes, in the order the decomposition reads best in.
var (
	// The whole tick: every peer uploads one page and four checksum records a
	// tick, the ack flushes them, the store records it and the tick serves the
	// observers. Exactly what the 37's sweep drives, so its figures are comparable
	// with the ones already in SESSION-JOURNAL.md 37.
	costModeFull = costMode{name: "full tick: 1 upload + 4 checksums a peer", uploads: 1, checksums: 4}

	// One checksum record instead of four. The reconcile runs one round and stops,
	// so the difference is three rounds of reconciliation plus three quarters of
	// the checksum bookkeeping. One record is still queued because a session with
	// nothing queued at all has not started and builds no ack at all
	// (AckState.PopTick returns nil until started), which would be a different
	// state and not a smaller tick.
	costModeOneChecksum = costMode{name: "1 upload + 1 checksum record a peer", uploads: 1, checksums: 1}

	// No uploads: the ack carries no subs, so no flushed map is built, no sort
	// runs, the body is the short heartbeat and no sub goes back to the pool.
	costModeNoUploads = costMode{name: "no uploads: the heartbeat ack", uploads: 0, checksums: 1}

	// Four uploads a peer a tick instead of one. The difference against the
	// reference is three more subs per peer through the drain, and divided by
	// three it is the MARGINAL cost of one more sub: what a peer that uploads
	// more often costs on top of the fixed cost it already pays.
	costModeFourUploads = costMode{name: "4 uploads + 4 checksums a peer", uploads: 4, checksums: 4}

	// The reference plus a full house of seated observers, all streaming.
	// the observer update interval at its default of 8 ticks means the stride visits
	// one observer
	// in nine per tick, so the difference against the reference is
	// the observer update and nothing else. 256 and not the 37's 64 because this
	// stage is measured as a difference of two whole ticks and 64 observers come to
	// about 0,3 us a tick, which is the order of the spread between two windows of
	// the same mode: at 64 the difference came out with the sign of the clock. 256
	// observers visit about 28 times a tick, which is a difference this file can
	// read, and the per-observer cost is what costObserversPerTick divides back
	// out.
	costModeObservers = costMode{name: "the full tick plus 256 observers", uploads: 1, checksums: 4, observers: 256}
)

var costModes = []costMode{costModeFull, costModeOneChecksum, costModeNoUploads, costModeFourUploads, costModeObservers}

// costStagePeers are the peer counts the decomposition runs at: the floor of the
// store's formula (8) and a point well above it (32). The step between them is
// what says whether a stage is per peer, which is the question the 37 left open
// for the checksum records.
var costStagePeers = []int{8, 32}

// costObserverStride is how many ticks separate two visits to the same observer:
// the observer update interval is 8 TICKS and the original walks the list with
// the interval
// plus one, so every observer is visited once every ninth tick, which is also the
// 200 frames a second capture 2026-09-29_012528 measured on the original.
const costObserverStride = 9

// costObserverVisits is how many of n seated observers one tick reaches with that
// stride.
func costObserverVisits(n int) int {
	if n < 1 {
		return 0
	}
	return (n + costObserverStride - 1) / costObserverStride
}

// ---------------------------------------------------------------------------
// THE FIXTURE
// ---------------------------------------------------------------------------

// costTick is sweepFixture.tick with the two upload rates as parameters. It drives
// the same session in the order the server's main loop does
// (internal/server/handlers.go:341 then :376): the uploads, the ack to every
// peer, and the session tick. The payload and the checksum records are the
// sweep's own buffers, sliced to the rate this mode asks for.
func costTick(d *sweepFixture, tick, uploads, checksums int) {
	binary.LittleEndian.PutUint16(d.upload, uint16(len(d.upload)-2))
	for off := 2; off < len(d.upload); off++ {
		d.upload[off] = byte(tick)
	}
	recs := d.checksum[:8*checksums]
	for off := 0; off < len(recs); off += 8 {
		binary.LittleEndian.PutUint32(recs[off:], uint32(tick*16))
		binary.LittleEndian.PutUint32(recs[off+4:], uint32(tick))
	}
	for i := range d.players {
		slot := byte(i + 1)
		for range uploads {
			d.s.QueueGameData(slot, d.upload)
		}
		d.s.QueueChecksumEntries(slot, recs)
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

// costReady builds a fixture for one mode and brings it to the steady state that
// mode describes: the store filled (in the reference mode, so every mode is
// measured against a full store and not one with room to spare), the observers
// seated and served, and then costWarmTicks of the mode itself.
//
// It reports false when the fixture did not get there, which the caller reports
// rather than measures: a store that never filled would make every figure below
// the cost of a store with room to spare.
func costReady(peers int, m costMode) (*sweepFixture, bool) {
	d := newSweepFixture(peers, m.observers)
	if _, full := d.runUntilAllBlocksLive(); !full {
		return d, false
	}
	if m.observers > 0 {
		if seated, refused := d.seatObservers(); refused || seated != m.observers {
			return d, false
		}
	}
	for range costWarmTicks {
		costTick(d, d.ticks, m.uploads, m.checksums)
	}
	if m.observers > 0 {
		served := 0
		for _, c := range d.obs {
			served += c.acks
		}
		if served == 0 {
			return d, false
		}
	}
	return d, true
}

// ---------------------------------------------------------------------------
// WHAT IS MEASURED AT A POINT
// ---------------------------------------------------------------------------

// costReading is one (peer count, mode) measurement. NsPerTick is a wall-clock
// figure with the discipline above it; AllocsPerTick and BytesPerTick come from
// MemStats and are exact.
type costReading struct {
	peers          int
	mode           costMode
	blocks         int
	nsPerTick      float64
	allocsPerTick  float64
	bytesPerTick   float64
	liveHeapKiB    int
	ticksPerWindow int
	repeats        int
	// slowestNs is the slowest of this mode's windows, so nsPerTick (the fastest)
	// and slowestNs are the two ends of the same mode's own spread. That spread is
	// the noise of the measurement, and it is what the stage decomposition's
	// tolerance has to absorb. See costStageNoiseShare for why it cannot be a fixed
	// fraction of nsPerTick.
	slowestNs float64
}

// spreadNs is how far apart this mode's own fastest and slowest windows came, per
// tick. It is the clock's and the scheduler's noise on THIS machine, measured here,
// rather than a number this file carries.
func (r costReading) spreadNs() float64 { return r.slowestNs - r.nsPerTick }

// measureCost times the mode at the peer count and reads its allocation counts
// over the same windows, so the three numbers describe the same work. Every one of
// them is the fastest of `repeats` windows.
func measureCost(peers int, m costMode, bucketTicks, repeats int) (costReading, bool) {
	d, ok := costReady(peers, m)
	if !ok {
		return costReading{peers: peers, mode: m}, false
	}
	r := costReading{
		peers: peers, mode: m, blocks: d.s.replay.maxBlocks, ticksPerWindow: bucketTicks, repeats: repeats,
		nsPerTick: math.Inf(1), allocsPerTick: math.Inf(1), bytesPerTick: math.Inf(1),
	}
	var ms runtime.MemStats
	for range repeats {
		runtime.ReadMemStats(&ms)
		m0, b0 := ms.Mallocs, ms.TotalAlloc
		start := time.Now()
		for range bucketTicks {
			costTick(d, d.ticks, m.uploads, m.checksums)
		}
		elapsed := time.Since(start)
		runtime.ReadMemStats(&ms)
		r.nsPerTick = min(r.nsPerTick, float64(elapsed)/float64(bucketTicks))
		r.allocsPerTick = min(r.allocsPerTick, float64(ms.Mallocs-m0)/float64(bucketTicks))
		r.bytesPerTick = min(r.bytesPerTick, float64(ms.TotalAlloc-b0)/float64(bucketTicks))
	}
	r.liveHeapKiB = int(liveHeapAfterGC() >> 10)
	runtime.KeepAlive(d)
	return r, true
}

// measureTransient is the garbage ONE tick leaves behind, measured with the
// collector switched off so the answer is not "whatever the collector happened to
// take". Two explicit collections set the baseline (they run even with the
// collector off), the window runs, and the heap growth is divided by the ticks.
//
// Two details are what make the number mean anything:
//
//   - the window is sized to costTransientBytes, because with the collector off
//     Go's heap goal is its 4 MiB floor and a window that crosses it starts
//     collecting. bytesPerTick is what sizes it, taken from the measured
//     allocation rate so the window is not guessed;
//   - the number of collections over the window is read and reported, so a
//     figure that was cut short by a collection says so instead of reading low
//     without saying why.
//
// Nothing in the window survives it: the store is at its cap and recycles whole
// blocks, so every byte that grows here is garbage the collector would have taken.
func measureTransient(peers int, m costMode, bytesPerTick float64) (float64, int, bool) {
	d, ok := costReady(peers, m)
	if !ok {
		return 0, 0, false
	}
	defer runtime.KeepAlive(d)
	ticks := costTransientTicks
	if bytesPerTick > 0 {
		if fit := int(costTransientBytes / bytesPerTick); fit < ticks {
			ticks = max(fit, costTransientMinTicks)
		}
	}
	old := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(old)
	runtime.GC()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range ticks {
		costTick(d, d.ticks, m.uploads, m.checksums)
	}
	runtime.ReadMemStats(&after)
	collections := int(after.NumGC - before.NumGC)
	if collections > 0 || after.HeapAlloc < before.HeapAlloc {
		return 0, collections, true
	}
	return float64(after.HeapAlloc-before.HeapAlloc) / float64(ticks), collections, true
}

// ---------------------------------------------------------------------------
// THE BOUNDS, each with the number it was chosen from. The figures are the ones
// in SESSION-JOURNAL.md 38, measured on this machine, and they are written here so
// the next person can tighten rather than re-derive.
// ---------------------------------------------------------------------------

const (
	// costMaxNsPerTick is the ceiling on one tick at any peer count, in
	// nanoseconds. It is deliberately loose and says so: it exists to catch a tick
	// an order of magnitude more expensive, not a 10% regression, which is more
	// than this machine's clock resolves honestly (the 34 measured 19 zeroed
	// buckets out of 55 for a single tick). Measured 45 us at 32 peers, and 585 us
	// under -race, whose shadow bookkeeping is a large multiple of the work; the
	// ceiling is 3,4 times the larger of the two, and the exact per-tick statement
	// in this file is the ALLOCATION count, which is a counter and not a clock.
	costMaxNsPerTick = 2_000_000

// costMaxNsPerTickPerPeer is the same figure as a slope over the peer count,
	// which is what says the tick is not secretly quadratic in the peers. The tick
	// walks the peers' queue records twice (queueEntries and the drain) and
	// reconciles them once, so it is linear in the peers. Measured 1,4 us a peer
	// and 11 us under -race, whose shadow bookkeeping is itself per peer per tick,
	// so the bound is 2,7 times the larger of the two: enough to hold in both
	// modes and still several times below what a pairwise reconciliation costs
	// (the falsification is in SESSION-JOURNAL.md 38).
	costMaxNsPerTickPerPeer = 30_000

	// costMaxAllocsPerPeerPerTick is the SLOPE of the allocation count against the
	// peer count, over the points at or above the store's floor of eight. It is a
	// slope and not a ratio because there is a fixed per-tick cost that does not
	// scale with the peers, and a ratio would read that fixed part as a per-peer
	// cost.
	//
	// The 37 set this at 12,0 against a measured 9,30, and said in §37.9 that it
	// caught "three or more extra allocations a peer" and deliberately let one or
	// two through, because 9,30 and 9,81 were too close together for a tighter band
	// to be about anything but the toolchain. That reason is gone: the per-peer
	// allocations this path had are pooled or reused away (SESSION-JOURNAL.md 38),
	// the ack body is built inside the frame store's block and the queue-record
	// snapshot is the session's own scratch, so the measured slope is 0,00 without
	// the detector and 0,51 with it. The cap is 2,0: four times the race figure,
	// and it fails on two extra objects a peer a tick.
	costMaxAllocsPerPeerPerTick = 2.0

	// costMaxBytesPerTick is the garbage one tick produces, at the largest peer
	// count: the exact allocation rate (TotalAlloc, which counts every allocation
	// whether or not the collector has taken it). Measured 5 KiB at 32 peers,
	// which is 40 KiB a second of collector work at the recorded tick rate; the
	// ceiling is 12 KiB, a little over twice the measurement. It used to be 32 KiB
	// against 11 KiB, when the ack body and the store's copy of it were both
	// allocated every tick.
	costMaxBytesPerTick = 12 << 10

	// costMaxNsPerObserverVisit bounds ONE visit to one observer. It is a price per
	// visit and not a share of the tick, because a share grows as the tick gets
	// cheaper and that is the opposite of a regression.
//
// It is also the LOOSE half of the observer story and it says so: measured 50 to
	// 250 ns a visit with 256 observers seated, and 3 to 5,5 us under -race, whose
	// shadow bookkeeping is per access. What a visit that copied the store's frame
	// list instead of reading it through GetFrames really costs was measured by
	// putting that copy in (SESSION-JOURNAL.md 38): 292 ns a visit instead of about
	// 100, which is a copy of the slice headers and not of the bodies, so NO bound
	// on nanoseconds here can be the thing that catches it. The sharp assertion for
	// the observer update is the ALLOCATION count (one object a tick for the whole
	// update, whatever the observer count), and that one sees the same defect at
	// 31,9 objects a tick.
	costMaxNsPerObserverVisit = 30_000

	// costStageNoiseShare is how far a stage's measured difference may fall the
	// WRONG WAY before the table is declared noise, as a fraction of the reference.
	//
	// It is a floor and not the whole tolerance, and the reason is written at the
	// use site (TestTickCostSplitsIntoStages): a fraction of the reference shrinks
	// when the tick gets cheaper, while the noise it has to absorb does not. The
	// tolerance is max(this fraction, costStageNoiseFloor).
	costStageNoiseShare = 0.15
)

// TestTickCostAtEveryPeerCount is the per-peer axis: one tick at every point of the
// 37's sweep, at the recorded simulation period, with the two figures the report
// needs side by side (nanoseconds and allocations) and the extrapolation to a real
// match printed with the arithmetic that produced it.
//
// The extrapolation is arithmetic on the measured numbers and is marked as such in
// what it prints, together with what it assumes: an extrapolation whose
// assumptions are not on the report is a guess with a number on it.
func TestTickCostAtEveryPeerCount(t *testing.T) {
	var readings []costReading
	for _, p := range peerSweep {
		r, ok := measureCost(p.peers, costModeFull, costBucketTicks, costRepeats)
		if !ok {
			t.Errorf("%d peers: the fixture did not reach its steady state, so every figure for this point is the cost "+
				"of a store with room to spare rather than the cost of a full one", p.peers)
			continue
		}
		readings = append(readings, r)
	}
	if len(readings) != len(peerSweep) {
		t.Fatalf("only %d of %d points ran; the table below would be a table of holes", len(readings), len(peerSweep))
	}

	t.Logf("one tick at %d ms per simulationPeriod (-simulationPeriod %d), reference mode: %s",
		1000/costTicksPerSecond, costTicksPerSecond, costModeFull.name)
	t.Logf("%4s %6s %10s %10s %10s %10s %10s", "peers", "blocks", "ns/tick", "us/tick", "allocs/tick",
		"allocs/peer", "B/tick")
	for _, r := range readings {
		t.Logf("%4d %6d %10.0f %10.2f %10.2f %10.2f %10.0f", r.peers, r.blocks, r.nsPerTick, r.nsPerTick/1000,
			r.allocsPerTick, r.allocsPerTick/float64(r.peers), r.bytesPerTick)
	}

	for _, r := range readings {
		if r.nsPerTick > costMaxNsPerTick {
			t.Errorf("%d peers: a tick costs %.0f ns, over the %d ns ceiling. These figures are a FLOOR for a real "+
				"match and not a measurement of one, so the defect this bound is for is a tick an order of magnitude "+
				"more expensive, not a 10%% regression", r.peers, r.nsPerTick, costMaxNsPerTick)
		}
		if r.bytesPerTick > costMaxBytesPerTick {
			t.Errorf("%d peers: a tick allocates %.0f bytes, over the %d the bound allows. A tick runs %d times a "+
				"second, so this is the collector's work and not the heap's size", r.peers, r.bytesPerTick,
				costMaxBytesPerTick, costTicksPerSecond)
		}
	}
	if s := costNsSlope(readings); s > costMaxNsPerTickPerPeer {
		t.Errorf("a tick grows %.0f ns for every extra peer, over the %d ns the bound allows. The tick walks the "+
			"peers' queue records twice and reconciles them once, so it is linear in the peers", s, costMaxNsPerTickPerPeer)
	} else {
		t.Logf("tick cost slope over the points at or above the floor of %d peers: %.0f ns/peer (%.2f us) against the "+
			"%d ns the bound allows", peerSweepFloorPeers, s, s/1000, costMaxNsPerTickPerPeer)
	}
	if s := costAllocSlope(readings); s > costMaxAllocsPerPeerPerTick {
		t.Errorf("a tick allocates %.2f more objects for every extra peer, over the %.1f the bound allows. This path "+
			"has no per-peer allocation at all (0.00 measured; SESSION-JOURNAL.md 38 pools the sub buffers, reuses the "+
			"checksum queue and the pending list, and keeps the flushed set on the stack), so anything above the "+
			"detector's own per-peer shadow is a per-client allocation that came back", s, costMaxAllocsPerPeerPerTick)
	} else {
		t.Logf("allocation slope over the points at or above the floor of %d peers: %.2f objects/peer/tick against the "+
			"%.1f the bound allows; the fixed part of a tick, at one peer, is %.0f objects", peerSweepFloorPeers, s,
			costMaxAllocsPerPeerPerTick, readings[0].allocsPerTick-float64(readings[0].peers)*s)
	}

	// The extrapolation. The arithmetic is printed with the result so it can be
	// checked by hand, and the assumptions are printed with it because they are
	// what would make it wrong.
	for _, players := range []int{4, 8} {
		r := costNearest(readings, players)
		cpu := r.nsPerTick * float64(costTicksPerMatch) / 1e9
		t.Logf("EXTRAPOLATED to a %d-player match from the measured %d-player point: %d ticks (%d h x 3600 x %d "+
			"ticks/s) x %.0f ns/tick = %.3f s of CPU for the whole match. Heap with the session alive: %d KiB, of "+
			"which the store is %d KiB (%s). Assumes: every player uploads a page and four checksum records EVERY tick "+
			"(a real game uploads in bursts, so this is the ceiling and not the average), one session per core (the "+
			"main loop is one goroutine, so N concurrent matches divide the same tick), and ack bodies of this size "+
			"(the store is byte-bounded, so a bigger body does not cost more heap, it costs more CPU per tick)",
			players, r.peers, costTicksPerMatch, costMatchHours, costTicksPerSecond, r.nsPerTick, cpu, r.liveHeapKiB,
			costStoreBlocks(r.peers)*peerSweepPageSize>>10, costStoreWindow(r.peers))
	}
}

// TestTickCostHeapAndTransient is the memory axis: the steady-state heap at each
// peer count (the 37's own sweep, in the state it measures: the store full and the
// fixture alive) and the transient garbage of ONE tick, which is the figure that
// says whether the steady state is also what the process holds between two
// collections.
func TestTickCostHeapAndTransient(t *testing.T) {
	steady := sweepAll(0)
	t.Logf("steady state: the 37's sweep, %d passes, the store full and the fixture alive. transient: the collector "+
		"off over a window sized to stay under %d KiB so no collection happens inside it (columns: allocated bytes a "+
		"tick exactly, and the heap growth over that window with the collector off)", peerSweepPasses, costTransientBytes>>10)
	t.Logf("%4s %7s %10s %9s %12s %12s %6s", "peers", "blocks", "store KiB", "heap KiB", "alloc B/tick", "transient B/tick", "GCs")
	for _, r := range steady {
		live, ok := measureCost(r.point.peers, costModeFull, costBucketTicks, 1)
		if !ok {
			t.Fatalf("%d peers: the fixture did not reach its steady state for the live reading", r.point.peers)
		}
		tr, collections, ok := measureTransient(r.point.peers, costModeFull, live.bytesPerTick)
		if !ok {
			t.Fatalf("%d peers: the fixture did not reach its steady state for the transient reading", r.point.peers)
		}
		t.Logf("%4d %7d %10.1f %9d %12.0f %12.0f %6d", r.point.peers, r.blocks, float64(r.peakStore)/1024,
			r.liveHeap>>10, live.bytesPerTick, tr, collections)
		if collections > 0 {
			t.Errorf("%d peers: %d collection(s) ran inside the transient window, so the heap growth over it is a "+
				"lower bound and not what a tick leaves behind. The window is sized to costTransientBytes for this and "+
				"the tick's own allocation is what grew", r.point.peers, collections)
		}
		if live.bytesPerTick > costMaxBytesPerTick {
			t.Errorf("%d peers: a tick allocates %.0f bytes, over the %d the bound allows. A tick runs %d times a "+
				"second, so this is the collector's work and not the heap's size", r.point.peers, live.bytesPerTick,
				costMaxBytesPerTick, costTicksPerSecond)
		}
		if int(r.liveHeap>>10) > peerSweepHeapCeiling>>10 {
			t.Errorf("%d peers: the live heap is %d KiB, over the %d KiB ceiling the 37 sets", r.point.peers,
				r.liveHeap>>10, peerSweepHeapCeiling>>10)
		}
	}
}

// TestTickCostSplitsIntoStages is the attribution: the same fixture at two peer
// counts with one stage at a time taken away or added, so every stage has a number
// and the biggest one is named.
//
// The numbers are DIFFERENCES of two whole ticks, which is what makes them
// attributable and also what limits them: a stage cheaper than the clock's
// resolution over a window reads as zero, and the printout says so rather than
// pretending otherwise.
func TestTickCostSplitsIntoStages(t *testing.T) {
	for _, peers := range costStagePeers {
		// Every mode gets its own fixture and they are all built FIRST, and then the
		// windows are taken ROUND ROBIN, one per mode per repetition. Measuring one
		// mode to the end and then the next let a drift in the machine (or in the
		// heap) land on one mode and not the others, and a stage that comes to about
		// a microsecond cannot tell a drift of two from a cost of two.
		fixtures := make([]*sweepFixture, len(costModes))
		readings := make([]costReading, len(costModes))
		for i, m := range costModes {
			d, ok := costReady(peers, m)
			if !ok {
				t.Fatalf("%d peers, %s: the fixture did not reach its steady state", peers, m.name)
			}
			fixtures[i] = d
			readings[i] = costReading{peers: peers, mode: m, blocks: d.s.replay.maxBlocks,
				ticksPerWindow: costStageBucketTicks, repeats: costStageRepeats,
				nsPerTick: math.Inf(1), allocsPerTick: math.Inf(1), bytesPerTick: math.Inf(1),
				slowestNs: 0}
		}
		var ms runtime.MemStats
		for range costStageRepeats {
			for i, m := range costModes {
				d := fixtures[i]
				runtime.ReadMemStats(&ms)
				m0 := ms.Mallocs
				start := time.Now()
				for range costStageBucketTicks {
					costTick(d, d.ticks, m.uploads, m.checksums)
				}
				elapsed := time.Since(start)
				runtime.ReadMemStats(&ms)
				readings[i].nsPerTick = min(readings[i].nsPerTick, float64(elapsed)/float64(costStageBucketTicks))
				readings[i].slowestNs = max(readings[i].slowestNs, float64(elapsed)/float64(costStageBucketTicks))
				readings[i].allocsPerTick = min(readings[i].allocsPerTick,
					float64(ms.Mallocs-m0)/float64(costStageBucketTicks))
			}
		}
		byMode := map[string]costReading{}
		for _, r := range readings {
			byMode[r.mode.name] = r
		}
		for _, d := range fixtures {
			runtime.KeepAlive(d)
		}

		ref := byMode[costModeFull.name]
		t.Logf("%d peers, windows of %d ticks, fastest of %d, ns/tick is the whole tick in that mode:",
			peers, costStageBucketTicks, costStageRepeats)
		t.Logf("  %-40s %10s %10s %10s", "mode", "ns/tick", "allocs/t", "delta ns")
		t.Logf("  %-40s %10.0f %10.2f %10s", costModeFull.name, ref.nsPerTick, ref.allocsPerTick, "-")
		d := func(m costMode) (float64, float64) {
			r := byMode[m.name]
			t.Logf("  %-40s %10.0f %10.2f %10.0f", m.name, r.nsPerTick, r.allocsPerTick, r.nsPerTick-ref.nsPerTick)
			return r.nsPerTick - ref.nsPerTick, r.allocsPerTick - ref.allocsPerTick
		}
		dOne, aOne := d(costModeOneChecksum)
		dNone, aNone := d(costModeNoUploads)
		dFour, aFour := d(costModeFourUploads)
		dObs, aObs := d(costModeObservers)

		// One more sub per peer, which is what costModeFourUploads adds over the
		// reference, has a per-peer price; the three checksum records it does not
		// add do not (the peer count is the same in both modes). That is the
		// difference between a stage that scales with the clients and one that
		// scales with the work, and it is what says the tick has no per-pair work
		// in it.
		marginalSub := (dFour - dNone) / 3 / float64(peers)

		// What the 32 left to check: the reconciliation compares the front record of
		// each peer against the FIRST peer's and against nothing else
		// (AckState.ReconcileChecksums), so it is one 8-byte comparison a peer a
		// round and not a pairwise walk. What a pairwise walk would show is a
		// per-peer term that grows with the peer count, and the two points are here
		// for exactly that: 8 peers and 32 peers is a factor of four, and a pairwise
		// walk would make the per-peer term grow by about four too.
		t.Logf("  per peer at %d peers: the three extra checksum rounds %.0f ns and %.2f allocs; the whole flush "+
			"%.0f ns and %.2f allocs; one more sub %.0f ns and %.2f allocs; %d observers %.0f ns and %.2f allocs, "+
			"which is %.0f ns a visit for the %d of them the stride reaches each tick", peers, -dOne/float64(peers),
			-aOne/float64(peers), -dNone/float64(peers), -aNone/float64(peers), marginalSub, aFour/3/float64(peers),
			costModeObservers.observers, dObs, aObs, dObs/float64(costObserverVisits(costModeObservers.observers)),
			costObserverVisits(costModeObservers.observers))

		// The observer update is the only thing in the session that runs per tick PER
		// OBSERVER, and it must not
		// scale with the number of observers beyond what a visit costs: it reads the
		// store through GetFrames, which returns a subslice of frames the store
		// already holds, and sends them.
		visits := costObserverVisits(costModeObservers.observers)
		perVisit := dObs / float64(visits)
		if perVisit > costMaxNsPerObserverVisit {
			t.Errorf("%d peers: a visit to an observer costs %.0f ns, over the %d the bound allows. With %d observers "+
				"seated the stride reaches %d of them a tick, and the update is reading the store through GetFrames, "+
				"which returns a subslice of frames the store already holds: a visit that copies the retained store "+
				"instead of reading it is what this catches", peers, perVisit, costMaxNsPerObserverVisit,
				costModeObservers.observers, visits)
		}
		if aObs > 1.0 {
			t.Errorf("%d peers: %d observers being served cost %.2f extra allocations a tick, over the 1.0 the 37 "+
				"allows: one object a tick for the whole update, whatever the observer count", peers,
				costModeObservers.observers, aObs)
		}
		// Every stage has a SIGN, and a sign that comes out the other way is the
		// whole table being noise: a mode that ADDS work cannot be cheaper than the
		// reference, and a mode that REMOVES work cannot be dearer. That is what this
		// checks.
		//
		// The tolerance is the LARGER of a fraction of the reference and the spread
		// this run actually measured between windows of one mode, and the second term
		// is not a refinement: without it this test got strictly more sensitive every
		// time the tick got cheaper, which is backwards.
		//
		// How that happened here, measured: 39.2 turned AckState's three maps into
		// 256-entry arrays and the tick at 32 peers went from 36.070 to 20.932 ns, a
		// fall of 42%. costStageNoiseShare is a fraction of the reference, so the
		// tolerance fell with it, from about 2.800 ns to about 1.700. The noise it has
		// to absorb did NOT fall, because the noise is the spread between two windows
		// of the same mode and that is the clock and the scheduler, not the work.
		// Result, on a machine with an unrelated game and an IDE on it: 2 failures in
		// 6 runs of this test, with "REMOVES work and still came out 3.111 ns a tick
		// dearer than the reference (14.433 against 11.322, tolerance 1.698)". The
		// same 6 runs on the maps version: 0 failures.
		//
		// So the noise is measured here instead of assumed: each mode's three windows
		// are spread round robin with the others, so a mode's own fastest-to-slowest
		// gap is the machine's drift across the whole test, and that gap is what a
		// difference of two whole ticks has to clear. It is zero on a quiet machine
		// and it is large on a loaded one, which is the behaviour a tolerance wants
		// and the opposite of what a fixed fraction of a cheaper reference does.
		tolerance := costStageTolerance(ref.nsPerTick, readings)
		// A tolerance that swallows the reference is a table that resolved nothing,
		// and it has to SAY so rather than pass looking like a sharp run. The smallest
		// stage is the three extra checksum rounds (costModeOneChecksum against the
		// reference), so that is what the resolvability line is measured against: if
		// the noise is of its size then this run cannot tell that stage from the clock
		// and the sign checks below are reading nothing.
		smallestStage := -dOne
		t.Logf("  tolerance %.0f ns = max(%.0f ns from %.0f ns x %.2f, %.0f ns measured as the widest spread "+
			"between windows of one mode)", tolerance, ref.nsPerTick*costStageNoiseShare, ref.nsPerTick,
			costStageNoiseShare, costStageNoiseFloor(readings))
		if noise := costStageNoiseFloor(readings); noise > 0 && smallestStage < noise {
			t.Logf("  NOT RESOLVED: the machine's own spread between windows of one mode is %.0f ns and the "+
				"smallest stage is %.0f ns, so this run cannot separate that stage from the clock. The sign checks "+
				"below pass because the tolerance covers the noise and NOT because the stages were measured. Run "+
				"this test on a machine that is not doing anything else to get a table that resolves", noise,
				smallestStage)
		}
		for _, s := range []struct {
			name    string
			delta   float64
			removes bool
		}{
			{costModeOneChecksum.name, dOne, true},
			{costModeNoUploads.name, dNone, true},
			{costModeFourUploads.name, dFour, false},
			{costModeObservers.name, dObs, false},
		} {
			if why := costStageSignVerdict(ref.nsPerTick, tolerance, s.delta, s.removes); why != "" {
				t.Errorf("%d peers: %s: %s", peers, s.name, why)
			}
		}
	}
}

// costStageSignVerdict is the sign rule the stage table is read with, and it returns
// the complaint when the sign is wrong and "" when it is right.
//
// It is a function and not inline code because the tolerance it is given is now the
// larger of a fraction and a MEASURED noise, and a tolerance that can grow has to be
// shown not to grow into meaninglessness: TestStageSignVerdictStillBites drives it
// with numbers instead of a clock, so the claim "a real difference of three times the
// noise still fails" is checked every run and not argued from a reading of the code.
//
// delta is the stage's measured ns/tick against the reference, and removes says
// whether the mode took work OUT. A mode that removes work coming out dearer, or a
// mode that adds work coming out cheaper, means the two modes are not the same tick
// and every other number in the table is then noise.
func costStageSignVerdict(ref, tolerance, delta float64, removes bool) string {
	if removes && delta > tolerance {
		return fmt.Sprintf("REMOVES work and still came out %.0f ns a tick dearer than the reference (%.0f against "+
			"%.0f, tolerance %.0f). The two modes are then not the same tick and the whole table is noise",
			delta, delta+ref, ref, tolerance)
	}
	if !removes && delta < -tolerance {
		return fmt.Sprintf("ADDS work and came out %.0f ns a tick CHEAPER than the reference (%.0f against %.0f, "+
			"tolerance %.0f). The two modes are then not the same tick and the whole table is noise",
			-delta, delta+ref, ref, tolerance)
	}
	return ""
}

// costStageTolerance is how far a stage's measured difference may fall the wrong way
// before the table is declared noise: the LARGER of a fraction of the reference and
// the spread this run measured between windows of one mode.
//
// It is a function so that the unit tests call THIS and not a copy of the formula.
// That is not a style point: with the max() written out in the test as well, putting
// the defect back (the proportional term alone) left every test green, because the
// test was asserting its own arithmetic. See stagetolerance_test.go.
func costStageTolerance(ref float64, readings []costReading) float64 {
	return max(ref*costStageNoiseShare, costStageNoiseFloor(readings))
}

// costStageNoiseFloor is the widest spread between the fastest and the slowest
// window of any one mode in this run, per tick. It is the measurement's own noise
// and it is what a difference of two whole ticks has to clear before the sign of
// that difference means anything.
//
// The widest and not the mean, because the check that uses it is a sign check on
// the SMALLEST stage, and the stage that fails is the one whose difference happens
// to land on the worst window of the run. The mean would be smaller and would let
// that through.
//
// It is zero when every window of every mode came out the same, which is what a
// quiet machine looks like, and it grows with the load. A fixed fraction of the
// reference cannot do that job: see the tolerance comment in
// TestTickCostSplitsIntoStages, where the tick getting 42% cheaper took the
// tolerance down with it and the noise stayed where it was.
func costStageNoiseFloor(readings []costReading) float64 {
	floor := 0.0
	for _, r := range readings {
		floor = max(floor, r.spreadNs())
	}
	return floor
}

// costNsSlope and costAllocSlope are the least-squares slope of a per-tick figure
// against the peer count, over the points at or above the store's floor of eight.
// Below the floor the store does not move with the peer count, so a per-peer term
// read off those points would be measuring the floor and not the peers.
func costNsSlope(readings []costReading) float64 {
	return costSlope(readings, func(r costReading) float64 { return r.nsPerTick })
}

func costAllocSlope(readings []costReading) float64 {
	return costSlope(readings, func(r costReading) float64 { return r.allocsPerTick })
}

func costSlope(readings []costReading, pick func(costReading) float64) float64 {
	var sx, sy, sxx, sxy, n float64
	for _, r := range readings {
		if r.peers < peerSweepFloorPeers {
			continue
		}
		x, y := float64(r.peers), pick(r)
		sx, sy, sxx, sxy, n = sx+x, sy+y, sxx+x*x, sxy+x*y, n+1
	}
	den := n*sxx - sx*sx
	if n < 2 || den == 0 {
		return 0
	}
	return (n*sxy - sx*sy) / den
}

// costNearest is the measured point at or below the requested player count. A
// measured point and not an interpolation between two of them, because the points
// ARE the measurements and a regression between them would be invented here.
func costNearest(readings []costReading, players int) costReading {
	best := readings[0]
	for _, r := range readings {
		if r.peers <= players && r.peers > best.peers {
			best = r
		}
	}
	return best
}

// costStoreBlocks is the store's block count at that peer count, from the 37's own
// table: ceil(max(peers, 8) * f) with f = 1.0, not from
// the code under test.
func costStoreBlocks(players int) int {
	return max(players, peerSweepFloorPeers)
}

func costStoreWindow(players int) string {
	return fmt.Sprintf("%d blocks of %d bytes", costStoreBlocks(players), peerSweepPageSize)
}