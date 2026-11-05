package session

import (
	"encoding/binary"
	"math"
	"runtime"
	"testing"
	"time"

	"github.com/luskaner/ageBattleServer/internal/protocol"
)

// WHAT THIS FILE IS
//
// Nothing in this repository had ever measured the cost of our own server: every
// clock that had been read belonged to the simulation harness, which is slow on
// purpose because it replays captures in real time. The longest recording is 24
// minutes, so "does a LONG match stay bounded" had never been asked of anything.
//
// It is asked here, without waiting: Session.PopAckTick and Session.Tick are
// driven directly, in the order the server's main loop runs them
// (server.go:374-376), so two hours of match cost less than a second of wall
// clock and the answer is about the code rather than about the harness.
//
// Everything synthetic here is bounded by a constant, in this file, on purpose.
// The caps are the direct lesson of the test that reached 20 GB of RAM
// (SESSION-JOURNAL.md 25): a synthetic horizon that lives in somebody's head is a
// horizon that gets doubled by whoever edits the test next without noticing.
//
// TWO THINGS THIS FILE SAYS OUT LOUD
//
//   - The clock inside the session is the wall clock (queueEntries takes a
//     time.Time and syncTimedOut subtracts it), so two hours of match in one
//     second cannot make a peer trip the 60 s sync timeout. The players here
//     upload on every tick and the timeout is not what this file probes.
//
//   - The clock of the MEASUREMENT is the wall clock too, and on this machine it
//     is quantised coarsely enough to matter: timing a 700 ns operation with
//     time.Since returns 0 for 95% of the samples, and 19 of the 55 buckets of
//     the first quarter of a 256-tick-bucketed run read exactly 0 (both
//     measured, not assumed). So nothing here is timed one tick or one visit at
//     a time. Per-tick cost is two whole clock readings around two whole
//     quarters of 14336 ticks, 60 to 120 ms each, and per-visit cost is the same
//     two readings divided by the thousands of visits inside them. What is
//     claimed exactly, with no clock in it at all, is the allocation count and
//     the heap after a forced GC.

// ---------------------------------------------------------------------------
// THE FIXTURE. Bounded, and every cap named after what it bounds.
// ---------------------------------------------------------------------------

// longMatchTickMS is the simulationPeriod every committed capture records.
const longMatchTickMS = 125

// longMatchTicksPerSecond is the simulationPeriod read the other way round (8).
const longMatchTicksPerSecond = 1000 / longMatchTickMS

// longMatchHorizonHours is how much simulated match this file drives. The
// captures stop at 24 minutes because the replay is real time; that is the whole
// point of this file.
const longMatchHorizonHours = 2

// longMatchHorizonTicks is the horizon in ticks, which is the unit of the loop.
const longMatchHorizonTicks = longMatchHorizonHours * 60 * 60 * longMatchTicksPerSecond

// longMatchPlayerPeers bounds the player count of the synthetic session. It is
// the two-player shape capture _6 measured on the original: one 0x0A a tick
// carrying both peers' sub-messages, and the pending tail naming the quiet one.
const longMatchPlayerPeers = 2

// longMatchObserverPeers bounds the observer list. Two is what
// 2026-09-29_012528_03cb67c7cf19 has: the game squeezes observing twice.
const longMatchObserverPeers = 2

// longMatchPageSize is the store's block size (the -pageSize
// setting). 65536 is the original's own default, so the
// store here is the one a real deployment builds: 8 blocks, half a megabyte of
// ack history, which at the frame size below is about 1400 ticks of match and so
// rolls over some 40 times inside the horizon.
const longMatchPageSize = 0x10000

// longMatchMaxPagesToStore is the -maxPagesToStorePerSession scale. 0 is the
// original's default (f = 1.0), whose effect is to floor the block count at 8.
const longMatchMaxPagesToStore = 0

// longMatchRecyclePageSize is the -pageSize of the recycling test: 1024 bytes,
// or two or three frames a block, which turns the store over about 28000 times
// inside the horizon instead of the 40 of the realistic one (measured: 57584 of
// 57600 frames leave through dropOldestBlockLocked). It is the cap that makes the
// recycling path the subject instead of an occasional event.
const longMatchRecyclePageSize = 1024

// longMatchRecycleJoinTick is when the observers join in the recycling fixture.
// In that one the store turns over every 16 frames, so at the 600 ticks of the
// other fixture it would already answer 3; at 2 it has recycled nothing yet.
const longMatchRecycleJoinTick = 2

// longMatchObserverIntervalTicks is the observer update interval with the default
// settings:
// 8 TICKS, so every observer is served once every ninth
// tick. Not a knob this file turns: it is the cadence the captures measured.
const longMatchObserverIntervalTicks = 8

// longMatchUploadBytes is the payload of one 0xFE GameData sub-message.
// Measured, not invented: SESSION-JOURNAL.md 24 measured the original's ack
// stream at 259651 bytes in 1570 frames, about 165 bytes a frame with one
// player, and a flush of two peers is roughly twice that.
const longMatchUploadBytes = 150

// longMatchChecksumRecordsPerUpload are the 8-byte records every player uploads
// each tick, and they are a cap of the fixture for a reason from the original and
// not for convenience: the reconciliation consumes ONE record per peer per round,
// and the session tick runs it at most reconcileMaxRounds = 10 times.
// Four is well inside what the
// original drains, so the queues grow neither here nor there.
// Uploading more than 10 a tick would fill that queue in the original as well,
// and that difference is not something this repository fixes.
const longMatchChecksumRecordsPerUpload = 4

// longMatchObserverJoinTick is when the observers arrive, in ticks. It is inside
// the store's first window (about 1400 frames at this frame size), which is the
// only window from which a real observer can still be served from the start of
// the match: once the store has dropped its first frame, StartObserve answers 3
// and does NOT register.
const longMatchObserverJoinTick = 600

// longMatchBucketTicks is how many ticks one measurement bucket covers, and so
// the grain of the memory curve. 256 ticks are 32 s of match.
const longMatchBucketTicks = 256

// longMatchBuckets is how many buckets the horizon has.
const longMatchBuckets = longMatchHorizonTicks / longMatchBucketTicks

// longMatchGCSampleEvery is how often a heap sample is taken, in buckets. The
// memory curve needs one forced GC per sample, and a GC every 256 ticks costs
// more than the bucket's own work does (measured: 785 us of GC against about
// 900 us for all 57600 ticks), besides which emptying ack.go's subPool on every
// window makes the tick itself dearer. Every 4 buckets gives 56 samples, which
// is a curve, and keeps the warmup at 4096 ticks.
const longMatchGCSampleEvery = 4

// longMatchWarmupSamples are the leading samples that are the baseline rather
// than the plateau. 4 samples are 4096 ticks, more than the ~1400 the store
// takes to fill and turn over twice, so the baseline is measured with a full
// store and the comparison is plateau against a steady state and not against the
// empty one.
const longMatchWarmupSamples = 4

// longMatchCostRepeats is how many times the cost pass runs the whole horizon,
// keeping the fastest figure for each quarter. Machine load only ever adds time,
// so the minimum is the load-free one, and a real degradation is in every
// repetition. Two is what the spread needed (measured: the late/early ratio went
// from 1.26 with one repetition to about 1.1 with two) without making the fast
// path pay for a third.
const longMatchCostRepeats = 2

// ---------------------------------------------------------------------------
// THE MEASUREMENT
// ---------------------------------------------------------------------------

// longMatchConn is a PeerConnection that counts what it is handed and keeps
// nothing. A mock that recorded every message would be the test's own leak, and
// two hours of match at one 0x0A a tick is 57600 of them per peer.
type longMatchConn struct {
	peerID     uint64
	msgVersion uint16
	sent       int
	sentBytes  int64
	// acks counts protocol.MsgEndOfGameData, which for an observer is the only
	// thing the store ever serves it.
	acks int
}

func (c *longMatchConn) SendMessage(msgType byte, body []byte) error {
	c.sent++
	c.sentBytes += int64(len(body))
	if msgType == protocol.MsgEndOfGameData {
		c.acks++
	}
	return nil
}

func (c *longMatchConn) Close() error                 { return nil }
func (c *longMatchConn) RemoteAddr() string           { return "longmatch" }
func (c *longMatchConn) SetPeerIdentifier(v uint64)   { c.peerID = v }
func (c *longMatchConn) PeerIdentifier() uint64       { return c.peerID }
func (c *longMatchConn) ClientMessageVersion() uint16 { return c.msgVersion }

// longMatchDriver is the synthetic match: the session, its players, its
// observers, and one tick of it.
type longMatchDriver struct {
	s        *Session
	players  []*longMatchConn
	obs      []*longMatchConn
	upload   []byte
	checksum []byte
	// joinTick is when the observers arrive, or -1 for never (which the
	// recycling fixture does not need: in it a late observer cannot register at
	// all, so its observers have to be in from the start).
	joinTick        int
	observerJoined  bool
	joinRefused     bool
	maxRetainedSeen int
}

// newLongMatch builds the fixture with a session bounded by the constants above:
// neither the peer count nor the observer count is a parameter, because a fixture
// whose size can be widened from the test that uses it is a fixture without a
// cap. pageSize and maxPages are parameters because they are the frame store's
// two settings, and the point is to be able to build either the store a real
// deployment builds or a tiny one that turns over every few frames.
func newLongMatch(pageSize, maxPages int) *longMatchDriver {
	s := NewSession(1)
	s.pageSize = pageSize
	s.maxPagesPerMatch = maxPages
	s.SetObserverConfig(longMatchObserverPeers+2, longMatchObserverIntervalTicks)
	// The 0x1D the game sends before the match starts: the observable flag set
	// and a window of -1 because maxObservers > 0, which is what leaves the
	// store's frame cap disabled for the whole run.
	s.SetObservableParameters(1, 0x21616c6661aa7a42, 0, longMatchTicksPerSecond)

	d := &longMatchDriver{
		s:        s,
		upload:   make([]byte, 2+longMatchUploadBytes),
		checksum: make([]byte, 8*longMatchChecksumRecordsPerUpload),
		joinTick: longMatchObserverJoinTick,
	}
	for i := byte(1); i <= longMatchPlayerPeers; i++ {
		c := &longMatchConn{msgVersion: 5}
		s.AddPeer(NewPeer(i, c))
		d.players = append(d.players, c)
	}
	for i := range longMatchObserverPeers {
		d.obs = append(d.obs, &longMatchConn{peerID: uint64(i + 1)})
	}
	return d
}

// tick is ONE simulation tick, in the order the server's main loop runs them
// (server.go:374-376): the players' 0xFE uploads, the session's 0x0A to every
// peer, and the session tick that serves the observers out of the store.
func (d *longMatchDriver) tick(tick int) {
	d.uploadAll(tick)
	body, notices := d.s.PopAckTick()
	if body != nil {
		d.s.ForEachPeer(func(p *Peer) {
			_ = p.SendMessage(protocol.MsgEndOfGameData, body)
		})
	}
	for _, notice := range notices {
		d.s.ForEachPeer(func(p *Peer) {
			_ = p.SendMessage(protocol.MsgGameData, notice)
		})
	}
	d.s.Tick()
}

// uploadAll has every player upload one 0xFE and its 0x14 of checksums. Both
// recorded sides of every capture hold 8 acks a second with real data behind them,
// and a flush naming two slots is what capture _6 measured; the two peers send
// THE SAME checksums, which is what makes the reconciliation agree instead of
// broadcasting a desync notice on every tick, and which is what lets this file
// assert that the reconcile path ran and never disagreed.
func (d *longMatchDriver) uploadAll(tick int) {
	binary.LittleEndian.PutUint16(d.upload, uint16(len(d.upload)-2))
	for off := 2; off < len(d.upload); off++ {
		// A byte that moves every tick, so no two frames are ever the same bytes
		// and nothing downstream can be sharing one buffer.
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
}

// joinObservers is the observers arriving partway in: StartObserve and then the
// 0x1A, which is the pair the client sends and the pair the captures show.
//
// It reports rather than fails so that the measurement loops can carry no
// *testing.T: a refused join is a fixture fact, and each test asserts on it in
// its own words.
func (d *longMatchDriver) joinObservers() {
	for _, c := range d.obs {
		status, registered := d.s.StartObserve(c, c.peerID, 0)
		if status != 0 || !registered {
			d.joinRefused = true
			return
		}
		if !d.s.ActivateObserver(c, longMatchTicksPerSecond) {
			d.joinRefused = true
			return
		}
	}
	d.observerJoined = true
}

// noteStoreShape records the store's high-water marks, which is how a recycling
// that copies, or a backing array that grows, gets caught even when the final
// value looks innocent.
func (d *longMatchDriver) noteStoreShape() {
	if d.s.replay == nil {
		return
	}
	d.maxRetainedSeen = max(d.maxRetainedSeen, len(d.s.replay.frames))
}

// heapSample is one point of the curve. live is HeapAlloc AFTER a forced GC,
// which is the only number that says whether anything is STILL REACHABLE; without
// the GC, HeapAlloc is mostly garbage, and a leak test that measures garbage
// passes against a server that leaks everything. Mallocs is cumulative and its
// deltas are the allocation profile.
type heapSample struct {
	tick    int
	live    uint64
	mallocs uint64
}

// runBucket runs one whole bucket. With sample false it takes no measurement at
// all: the caller that is timing the tick loops wraps the whole region itself,
// and a clock reading inside a bucket is not worth having on this machine.
func (d *longMatchDriver) runBucket(bucket int, sample bool) heapSample {
	for i := range longMatchBucketTicks {
		n := bucket*longMatchBucketTicks + i
		if !d.observerJoined && n == d.joinTick {
			d.joinObservers()
		}
		d.tick(n)
		d.noteStoreShape()
	}
	if !sample {
		return heapSample{}
	}
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return heapSample{
		tick:    (bucket + 1) * longMatchBucketTicks,
		live:    ms.HeapAlloc,
		mallocs: ms.Mallocs,
	}
}

// heapSlope is the least-squares slope of the live curve in bytes per tick. A
// max-against-baseline comparison says whether there is a gross leak; the slope
// also catches one that is small enough for GC noise to hide inside the max.
func heapSlope(samples []heapSample) float64 {
	n := float64(len(samples))
	if n < 2 {
		return 0
	}
	var sx, sy float64
	for _, s := range samples {
		sx += float64(s.tick)
		sy += float64(s.live)
	}
	mx, my := sx/n, sy/n
	var num, den float64
	for _, s := range samples {
		dx := float64(s.tick) - mx
		num += dx * (float64(s.live) - my)
		den += dx * dx
	}
	if den == 0 {
		return 0
	}
	return num / den
}

// liveFrom is the live heap of every sample but the warmup window, which is the
// "plateau" half of the curve as opposed to the "baseline" half.
func liveFrom(samples []heapSample, from int) []uint64 {
	out := make([]uint64, 0, len(samples)-from)
	for _, s := range samples[from:] {
		out = append(out, s.live)
	}
	return out
}

func maxU64(v []uint64) uint64 {
	m := uint64(0)
	for _, x := range v {
		if x > m {
			m = x
		}
	}
	return m
}

func minU64(v []uint64) uint64 {
	m := ^uint64(0)
	for _, x := range v {
		if x < m {
			m = x
		}
	}
	return m
}

// ---------------------------------------------------------------------------
// THE BOUNDS, each one with the number it was chosen from. Every figure below
// was read off a run of these tests, in both normal and -race mode, and is also
// written down in SESSION-JOURNAL.md 34 so that the next person does not have to
// re-derive them nor loosen them.
// ---------------------------------------------------------------------------
//
//	live heap (after a forced GC)  baseline ~800 KiB, plateau ~820 KiB over
//	                               57600 ticks, i.e. 22 KiB of growth and a
//	                               drift of 0,10 to 0,30 B/tick
//	                               -> 4 MiB of growth, 8 B/tick of slope,
//	                               32 MiB absolute
//	allocations per tick          30,0 (31,0 under -race) when this was
//	                               written, 6,01 (6,00) after the pooling
//	                               of SESSION-JOURNAL.md 38, first and
//	                               last quarter within 0,01 of each other
//	                               -> ceiling 24, growth budget 8
//	tick time                     first quarter 4,5 to 5,3 us, last quarter
//	                               4,4 to 4,6 us: the worst ratio seen over
//	                               four unloaded runs was 1,02
//	                               -> 3x plus a 5000 ns floor
//	observer visits               22 to 25 us a visit in the first half and
//	                               20,6 to 20,9 us in the second (9,2 and 9,0
//	                               frames a visit); under -race, 184,5 and
//	                               185,6 us, worst ratio 1,01
//	                               -> 3x plus a 5000 ns floor
//
// Why the clock bounds are 3x and not 1,3x, when the unloaded spread is 2%:
// under the full `go test -short ./...` the packages run in parallel and the
// same build measured 10083 ns a tick in the first quarter against 15681 ns in
// the last, a ratio of 1,56 that is the machine's load and not the server. The
// allocation count in that same run was 30,01 against 30,01 (6,01 against 6,00
// after the pooling of SESSION-JOURNAL.md 38), which is the point: the allocation
// assertions are exact and are the sharp per-tick statement, and the clock bounds
// are there to catch a cost that degrades by an order of magnitude. A growth probe
// that scans the store per visit measured a ratio of 5,09 and fails at 3x.
const (
	// longMatchHeapGrowthBudget is how far the live heap may grow above its
	// baseline. Measured: 22 KiB over two hours. 4 MiB is 190 times that, and it
	// is still a leak budget rather than a ceiling: 4 MiB over 57600 ticks is
	// 72 bytes a tick, so any leak that reserves anything per tick fails it.
	longMatchHeapGrowthBudget = 4 << 20

	// longMatchHeapSlopeBudget is the permitted drift in bytes per tick.
	// Measured: 0,30 B/tick at worst, about 17 KiB over the whole horizon. 8
	// B/tick is 26 times that and 440 KiB in total: a leak of a single slice
	// header per tick (48 bytes) already fails it, and one of ten bytes a tick
	// brushes it.
	longMatchHeapSlopeBudget = 8

	// longMatchHeapCeiling is an absolute bound on live heap, so that a leak
	// already inside the baseline cannot slip through the max-against-baseline
	// comparison. Measured: 820 KiB. 32 MiB is 40 times that.
	longMatchHeapCeiling = 32 << 20

	// longMatchMaxAllocsPerTick is the ceiling on Mallocs per tick in steady
	// state. Measured: 0,01 in the first quarter and 0,01 in the last. The tick
	// allocates nothing per tick any more: the ack body is built inside the frame
	// store's open block, the queue-record snapshot is the session's own scratch,
	// and the send is a loop over the peers instead of a closure. What is left is
	// the store's amortised block allocation, one 64 KiB block every few hundred
	// ticks.
	//
	// It was 30,0 (31,0 under -race) with a ceiling of 48 when this was written,
	// then 24 against a measured 6,00. The ceiling is now 2, which is the
	// sensitivity the old one claimed: a new per-tick path that reserves anything
	// fails it, and two objects a tick over two hours of match is 115200 objects
	// the tick did not use to make.
	longMatchMaxAllocsPerTick = 2

	// longMatchAllocGrowthBudget is how much the per-tick allocation cost may
	// grow between the first and the last quarter, in Mallocs per tick. Measured:
	// -0,03 before the pooling of SESSION-JOURNAL.md 38 and -0,01 after it, so it
	// fell by a hundredth of an object either way. Constant growth is the shape a
	// structure that degrades has.
	longMatchAllocGrowthBudget = 8

	// longMatchTimeGrowthRatio and longMatchTimeFloorBudget are the per-tick
	// cost assertion: the last quarter may not pass this multiple of the first,
	// with a floor in nanoseconds so that clock noise cannot fail the test by
	// itself. Both quarters are timed with one clock reading each and the fastest
	// of two repetitions, and the multiple is 3 for the reason the block comment
	// above gives: it separates a degrading structure (5,09 measured with a probe)
	// from a loaded machine (1,56 measured with the whole short suite running),
	// and the sharp per-tick statement is the allocation count above.
	longMatchTimeGrowthRatio = 3
	longMatchTimeFloorBudget = 5_000

	// longMatchVisitGrowthRatio and longMatchVisitFloorBudget are the same for
	// the cost of ONE visit to an observer, measured in its own test over the
	// thousands of visits in each half of the horizon.
	longMatchVisitGrowthRatio = 3
	longMatchVisitFloorBudget = 5_000
)

// TestLongMatchMemoryAndCostAreBounded drives two hours of match at the recorded
// cadence and asserts that the memory plateaus and that the per-tick cost neither
// grows nor exceeds a ceiling.
//
// It is the whole point of this file, and the reason it can be in the fast path
// is that it never sleeps: 57600 ticks of session work take less than a second,
// against the 24 minutes the same match costs in the harness.
func TestLongMatchMemoryAndCostAreBounded(t *testing.T) {
	d := newLongMatch(longMatchPageSize, longMatchMaxPagesToStore)

	start := time.Now()
	// PASS 1, memory. One forced GC every longMatchGCSampleEvery buckets, so
	// every sample is "what is still reachable" rather than "what the collector
	// has not got to yet".
	var heap []heapSample
	for b := range longMatchBuckets {
		if s := d.runBucket(b, b%longMatchGCSampleEvery == 0); s.live != 0 {
			heap = append(heap, s)
		}
	}
	memWall := time.Since(start)
	if d.joinRefused {
		t.Fatalf("the observers were refused at tick %d (status must be 0 and registered true): an observer that "+
			"joins inside the store's first window is the arm that registers", d.joinTick)
	}

	// PASS 2, cost. Same fixture shape, same horizon, no forced GC: the
	// allocation profile has to be the real one, and a GC per window would empty
	// ack.go's subPool in every one of them.
	//
	// Each quarter is timed with ONE pair of clock readings around the whole
	// quarter, not per bucket. This machine's monotonic clock is quantised: a
	// single 256-tick bucket is about 1 ms of work and the clock reads 0 for a
	// good share of them (measured: 19 zeros out of 55 in the first quarter, and
	// none in the last, from the same build). A quarter is 60 to 120 ms and holds
	// 56 of those buckets, so one reading of it is a measurement and 56 of them
	// are a lottery.
	//
	// And the whole thing is repeated, keeping the FASTEST of each quarter,
	// because background load on the machine only ever adds time: the minimum is
	// the load-free figure, and two repetitions moved the worst observed
	// late/early ratio from 1,26 to 1,02 (measured). What that cannot hide is a
	// tick that genuinely costs more at the end of the match, because that is in
	// every repetition.
	q := longMatchBuckets / 4
	earlyPer, latePer := math.Inf(1), math.Inf(1)
	earlyNs, lateNs := math.Inf(1), math.Inf(1)
	var earlyWall, lateWall time.Duration
	for range longMatchCostRepeats {
		d2 := newLongMatch(longMatchPageSize, longMatchMaxPagesToStore)
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		runMallocs := ms.Mallocs
		start = time.Now()
		for b := range q {
			d2.runBucket(b, false)
		}
		repEarly := time.Since(start)
		runtime.ReadMemStats(&ms)
		repEarlyAllocs := float64(ms.Mallocs-runMallocs) / float64(q*longMatchBucketTicks)
		for b := q; b < 3*q; b++ {
			d2.runBucket(b, false)
		}
		runtime.ReadMemStats(&ms)
		midMallocs := ms.Mallocs
		start = time.Now()
		for b := 3 * q; b < 4*q; b++ {
			d2.runBucket(b, false)
		}
		repLate := time.Since(start)
		runtime.ReadMemStats(&ms)
		repLateAllocs := float64(ms.Mallocs-midMallocs) / float64(q*longMatchBucketTicks)
		if d2.joinRefused {
			t.Fatalf("the observers were refused at tick %d in the cost pass", d2.joinTick)
		}
		if v := float64(repEarly) / float64(q*longMatchBucketTicks); v < earlyNs {
			earlyNs, earlyWall = v, repEarly
		}
		if v := float64(repLate) / float64(q*longMatchBucketTicks); v < lateNs {
			lateNs, lateWall = v, repLate
		}
		if repEarlyAllocs < earlyPer {
			earlyPer = repEarlyAllocs
		}
		if repLateAllocs < latePer {
			latePer = repLateAllocs
		}
	}

	t.Logf("long match: %d ticks = %d s of match at %d ms/tick, %d buckets of %d ticks, %d memory samples, "+
		"%d cost repetitions", longMatchHorizonTicks, longMatchHorizonTicks/longMatchTicksPerSecond,
		longMatchTickMS, longMatchBuckets, longMatchBucketTicks, len(heap), longMatchCostRepeats)

	// --- memory --------------------------------------------------------------
	if len(heap) <= longMatchWarmupSamples {
		t.Fatalf("only %d memory samples for a warmup of %d: the constants do not leave a plateau to measure",
			len(heap), longMatchWarmupSamples)
	}
	baseline := maxU64(liveFrom(heap, 0)[:longMatchWarmupSamples])
	post := liveFrom(heap, longMatchWarmupSamples)
	plateau := maxU64(post)
	slope := heapSlope(heap[longMatchWarmupSamples:])

	t.Logf("live heap after a forced GC: baseline %d KiB, plateau %d KiB (min %d KiB), drift %.2f B/tick, "+
		"measured over ticks %d..%d", baseline>>10, plateau>>10, minU64(post)>>10, slope,
		heap[longMatchWarmupSamples].tick, heap[len(heap)-1].tick)
	t.Logf("pass 1 (memory, forced GC every %d buckets): %v", longMatchGCSampleEvery, memWall)

	if baseline == 0 {
		t.Fatalf("the baseline live heap is 0 bytes, so nothing below means anything")
	}
	if plateau > baseline+longMatchHeapGrowthBudget {
		t.Errorf("the live heap grew from %d KiB to %d KiB over %d ticks, a growth of %d KiB; the budget is %d KiB. "+
			"A heap that only goes up has the shape of a leak, and it took %d bytes a tick to do it",
			baseline>>10, plateau>>10, longMatchHorizonTicks, (plateau-baseline)>>10,
			longMatchHeapGrowthBudget>>10, (plateau-baseline)/longMatchHorizonTicks)
	}
	if slope > longMatchHeapSlopeBudget {
		t.Errorf("the live heap drifts at %.2f bytes per tick over the last %d ticks (from %d KiB to %d KiB); the "+
			"budget is %d B/tick. A drift that slow hides inside the max-against-baseline comparison and is still a leak",
			slope, longMatchHorizonTicks, minU64(post)>>10, plateau>>10, longMatchHeapSlopeBudget)
	}
	if plateau > longMatchHeapCeiling {
		t.Errorf("the live heap reached %d KiB, over the %d KiB absolute ceiling", plateau>>10, longMatchHeapCeiling>>10)
	}

	// --- per-tick cost -------------------------------------------------------
	t.Logf("allocs/tick: first quarter %.2f, last quarter %.2f (ceiling %d)", earlyPer, latePer,
		longMatchMaxAllocsPerTick)
	t.Logf("tick time: first quarter %.0f ns (%v), last quarter %.0f ns (%v), fastest of %d repetitions",
		earlyNs, earlyWall, lateNs, lateWall, longMatchCostRepeats)

	if latePer > longMatchMaxAllocsPerTick {
		t.Errorf("the tick allocates %.2f objects per tick in the last quarter; the ceiling is %d. The ack path "+
			"rebuilds its body every tick by design, but a per-tick cost that grows with the "+
			"length of the match is a defect and not a cost",
			latePer, longMatchMaxAllocsPerTick)
	}
	if latePer > earlyPer+longMatchAllocGrowthBudget {
		t.Errorf("the tick allocates %.2f objects in the last quarter against %.2f in the first: it costs %.2f more "+
			"per tick two hours in than at the start, over a budget of %d",
			latePer, earlyPer, latePer-earlyPer, longMatchAllocGrowthBudget)
	}
	if want := earlyNs*longMatchTimeGrowthRatio + longMatchTimeFloorBudget; lateNs > want {
		t.Errorf("the average tick costs %.0f ns in the last quarter against %.0f ns in the first; the budget is "+
			"%.0f ns (%.1fx plus a %.0f ns floor). A data structure that gets slower the longer the game runs is a "+
			"defect, not a curiosity",
			lateNs, earlyNs, want, float64(longMatchTimeGrowthRatio), float64(longMatchTimeFloorBudget))
	}

	// --- what actually has to have happened ----------------------------------
	// A memory test that passes on top of a session that did nothing is a green
	// test over two empties, which is the most expensive bug the harness has
	// produced (SESSION-JOURNAL.md 24).
	if d.s.replay.Total() != longMatchHorizonTicks {
		t.Errorf("the store holds %d frames for %d acks: the tick appends exactly one frame",
			d.s.replay.Total(), longMatchHorizonTicks)
	}
	if d.s.replay.Base() == 0 {
		t.Errorf("Base() = 0 after %d ticks: at 8 a second with two players the store never recycled, so the "+
			"recycling path this file exists to probe did not run", longMatchHorizonTicks)
	}
	assertStoreIsBounded(t, d, longMatchPageSize, d.maxRetainedSeen)
	for i, c := range d.players {
		if c.acks != longMatchHorizonTicks {
			t.Errorf("player %d received %d acks for %d ticks", i+1, c.acks, longMatchHorizonTicks)
		}
	}
	for i, c := range d.obs {
		if c.acks == 0 {
			t.Errorf("observer %d was served nothing at all: the memory above would say nothing about the "+
				"observer path", i+1)
			continue
		}
		if gap := d.s.PageCount - c.acks; gap > 3*longMatchObserverIntervalTicks {
			t.Errorf("observer %d is %d frames behind the store with an interval of %d ticks: it should ride the "+
				"live stream, and a lag that grows is a leak of another kind", i+1, gap, longMatchObserverIntervalTicks)
		}
	}
	if d.s.DesyncCount != 0 {
		t.Errorf("%d desync notices over %d ticks with %d identical checksum records a tick from every player: they "+
			"are supposed to agree, so the reconcile path ran and never once disagreed, which is what it takes for "+
			"DesyncCount == 0 to mean anything", d.s.DesyncCount, longMatchHorizonTicks,
			longMatchChecksumRecordsPerUpload)
	}
}

// assertStoreIsBounded checks the store's own shape. These are structural
// invariants of Replay rather than measurements, and they hold at any horizon: a
// store that keeps more blocks than its cap, or whose backing array grows past
// twice the frames it ever held at once, is recycling by copying or is letting
// the array grow.
//
// highWaterRetained is the most frames the store ever held at the same time, and
// it is the only fair comparison for cap(): at the end of a run the store can be
// holding very few frames while its array keeps the capacity of a full window.
func assertStoreIsBounded(t *testing.T, d *longMatchDriver, blockSize, highWaterRetained int) {
	t.Helper()
	r := d.s.replay
	if len(r.blockAt) > r.maxBlocks {
		t.Errorf("the store holds %d live blocks, over its cap of %d: a block is only ever forgotten by "+
			"dropOldestBlockLocked", len(r.blockAt), r.maxBlocks)
	}
	// Every retained frame was charged at least frameOverhead bytes against a
	// block, so the retained count cannot exceed the store's whole byte window
	// divided by that. It is the sharpest statement available without knowing
	// the frame sizes in advance.
	if ceiling := r.maxBlocks * blockSize / frameOverhead; len(r.frames) > ceiling {
		t.Errorf("the store retains %d frames, over the %d its %d blocks of %d bytes can hold at %d bytes a frame",
			len(r.frames), ceiling, r.maxBlocks, blockSize, frameOverhead)
	}
	if cap(r.frames) > 2*highWaterRetained {
		t.Errorf("the store's frame array has capacity %d for a high-water mark of %d retained frames; the "+
			"recycling memmoves inside the array and never grows it, so a capacity above twice that is the array growing",
			cap(r.frames), highWaterRetained)
	}
	if got := len(r.frames) * frameOverhead; got > r.maxBlocks*blockSize {
		t.Errorf("the retained frames charge %d bytes against a %d byte window", got, r.maxBlocks*blockSize)
	}
}

// TestFrameStoreRecyclesWithoutGrowing is the same fixture with a store small
// enough to turn over every few frames: 1024 bytes of block for a ~370 byte
// frame is two or three frames a block and about 20000 recycles inside the
// horizon, where the realistic store does 40. What has to hold is that recycling
// does not copy, does not keep the buffers behind the frames, and does not leave
// the observer list growing.
func TestFrameStoreRecyclesWithoutGrowing(t *testing.T) {
	d := newLongMatch(longMatchRecyclePageSize, longMatchMaxPagesToStore)
	d.joinTick = longMatchRecycleJoinTick

	var heap []heapSample
	start := time.Now()
	for b := range longMatchBuckets {
		if s := d.runBucket(b, b%longMatchGCSampleEvery == 0); s.live != 0 {
			heap = append(heap, s)
		}
	}
	wall := time.Since(start)
	if d.joinRefused {
		t.Fatalf("the observers were refused at tick %d: in this fixture the store is full from the second frame, "+
			"so the join has to happen before the first recycle", d.joinTick)
	}

	r := d.s.replay
	baseline := maxU64(liveFrom(heap, 0)[:longMatchWarmupSamples])
	post := liveFrom(heap, longMatchWarmupSamples)
	plateau := maxU64(post)
	slope := heapSlope(heap[longMatchWarmupSamples:])
	dropped := r.base

	t.Logf("recycling store: the window holds %d frames, so %d of %d frames went out through dropOldestBlockLocked "+
		"over %d ticks in %v; array capacity at most %d",
		d.maxRetainedSeen, dropped, r.total, longMatchHorizonTicks, wall, cap(r.frames))
	t.Logf("live heap: baseline %d KiB, plateau %d KiB, drift %.2f B/tick", baseline>>10, plateau>>10, slope)

	// The same two bounds as the main test, on the same two numbers, so a store
	// that recycles twenty thousand times and grows is failed by bounds already
	// chosen from the other fixture's measurement.
	if plateau > baseline+longMatchHeapGrowthBudget {
		t.Errorf("the live heap grew from %d KiB to %d KiB over %d recycled frames; the budget is %d KiB. Recycling "+
			"that copies, or that lets the buffers behind the frames grow, looks exactly like this",
			baseline>>10, plateau>>10, dropped, longMatchHeapGrowthBudget>>10)
	}
	if slope > longMatchHeapSlopeBudget {
		t.Errorf("the live heap drifts at %.2f bytes per tick over %d recycled frames; the budget is %d B/tick",
			slope, dropped, longMatchHeapSlopeBudget)
	}
	assertStoreIsBounded(t, d, longMatchRecyclePageSize, d.maxRetainedSeen)
	if r.maxBlocks != 8 {
		t.Errorf("the store was built with %d blocks, want 8: peers floored at 8 times f = 1.0",
			r.maxBlocks)
	}

	// The observer arm on a store that has already forgotten the start of the
	// match: status 3 and NOT registered. Here it is a
	// fact about a store that has recycled thousands of times rather than a
	// synthetic flag.
	late := &longMatchConn{peerID: 99}
	status, registered := d.s.StartObserve(late, late.peerID, 0)
	if status != 3 || registered {
		t.Errorf("StartObserve on a store that has dropped %d of its %d frames: status %d registered %t; want 3 and false",
			dropped, r.total, status, registered)
	}
	if got := d.s.ObserverCount(); got != longMatchObserverPeers {
		t.Errorf("the observer list holds %d entries after a rejected StartObserve; a rejected request must not "+
			"touch it", got)
	}
}

// TestObserverServeCostDoesNotGrowWithTheMatchLength is the observer path on its
// own: the captures put 200 frames per visit and a visit every ninth tick in the
// picture, and the question is not whether a visit is cheap but whether it costs
// the same in minute 120 as in minute one.
//
// A visit is identified by an observer's own frame counter moving, rather than by
// second-guessing the stride counter: a tick that served an
// observer is a tick whose work includes the serve, and which ticks those are is
// exactly what this test must not assume. The two windows are the two halves of
// the horizon, 28800 ticks apart, each holding thousands of visits, which is
// what makes an aggregate of a millisecond-scale quantity trustworthy on a clock
// whose granularity is hundreds of microseconds.
func TestObserverServeCostDoesNotGrowWithTheMatchLength(t *testing.T) {
	d := newLongMatch(longMatchPageSize, longMatchMaxPagesToStore)
	type window struct {
		ticks, visits, frames int
		wall                  time.Duration
	}
	windows := [2]window{}
	half := longMatchHorizonTicks / 2
	prev := make([]int, len(d.obs))

	// Each half is timed with ONE pair of clock readings, for the reason
	// TestLongMatchMemoryAndCostAreBounded states: this clock is quantised and a
	// per-tick reading is mostly noise. A half is 28800 ticks and about 130 ms,
	// which is two orders of magnitude above the quantum.
	windowStart := time.Now()
	for tick := range longMatchHorizonTicks {
		if !d.observerJoined && tick == d.joinTick {
			d.joinObservers()
			if d.joinRefused {
				t.Fatalf("the observers were refused at tick %d, so the observer path never ran", d.joinTick)
			}
		}
		w := &windows[0]
		if tick >= half {
			if tick == half {
				windows[0].wall = time.Since(windowStart)
				windowStart = time.Now()
			}
			w = &windows[1]
		}
		w.ticks++
		d.tick(tick)
		served := 0
		for i, c := range d.obs {
			if c.acks > prev[i] {
				served += c.acks - prev[i]
				prev[i] = c.acks
			}
		}
		if served > 0 {
			w.visits++
			w.frames += served
		}
	}
	windows[1].wall = time.Since(windowStart)
	early, late := windows[0], windows[1]

	if early.visits == 0 || late.visits == 0 {
		t.Fatalf("the two windows hold %d and %d visits: the observer path never ran", early.visits, late.visits)
	}
	earlyNs := float64(early.wall) / float64(early.visits)
	lateNs := float64(late.wall) / float64(late.visits)
	earlyFrames := float64(early.frames) / float64(early.visits)
	lateFrames := float64(late.frames) / float64(late.visits)

	t.Logf("first half: %d ticks in %v, %d visits, %d frames served (%.0f ns and %.1f frames a visit); "+
		"second half: %d ticks in %v, %d visits, %d frames served (%.0f ns and %.1f frames a visit)",
		early.ticks, early.wall, early.visits, early.frames, earlyNs, earlyFrames,
		late.ticks, late.wall, late.visits, late.frames, lateNs, lateFrames)
	for i, c := range d.obs {
		t.Logf("observer %d was served %d frames in %d bytes", i+1, c.acks, c.sentBytes)
	}

	if want := earlyFrames * 2; lateFrames > want {
		t.Errorf("a visit carries %.1f frames in the second half of the match against %.1f in the first, over a "+
			"budget of %.1f. The store hands out a fixed window of at most 200 frames "+
			"whatever the age of the match", lateFrames, earlyFrames, want)
	}
	if want := earlyNs*longMatchVisitGrowthRatio + longMatchVisitFloorBudget; lateNs > want {
		t.Errorf("a visit to an observer costs %.0f ns in the second half of the match against %.0f ns in the "+
			"first; the budget is %.0f ns (%.1fx plus a %.0f ns floor). A visit that gets dearer as the hours go by "+
			"is a structure walking something that grew",
			lateNs, earlyNs, want, float64(longMatchVisitGrowthRatio), float64(longMatchVisitFloorBudget))
	}
	for i, c := range d.obs {
		if c.acks == 0 {
			t.Errorf("observer %d was served nothing: the cost measured above would be of a path that never ran", i+1)
		}
		if gap := d.s.PageCount - c.acks; gap > 3*longMatchObserverIntervalTicks {
			t.Errorf("observer %d is %d frames behind the store: it should ride the live stream", i+1, gap)
		}
	}
}

// longMatchChurnObservers and longMatchChurnSeated size the churn test.
//
// 1000 observers joining and leaving is far more than any match has, and it is a
// constant in the file so that raising it is a visible diff rather than a test
// that quietly grew. The number is not arbitrary in the other direction either:
// it has to stay inside the store's first window (about 1400 frames at this
// frame size), because StartObserve only registers while the store still holds
// the first frame of the match, so a longer churn would be answering 3 all the
// way and would not touch the list at all.
const (
	longMatchChurnObservers = 1000
	longMatchChurnSeated    = 3 // how many stay registered, under MaxObservers

	// longMatchChurnResidueBudget is how much heap a thousand departed observers
	// may leave behind over a control run that saw none of them. Measured: 3 KiB,
	// and it does not scale with the observer count, so it is the allocator's own
	// residue and not anything per connection. 64 KiB is twenty times that and
	// still fails on a leak of one ObserverEntry (48 bytes) per departed
	// observer, which is 48 KiB here.
	longMatchChurnResidueBudget = 64 << 10

	// longMatchChurnRuns is how many fresh fixtures each of the two churn
	// measurements runs; see churnHeapMin for why it is not one.
	longMatchChurnRuns = 3
)

// TestObserverChurnDoesNotGrowTheList is the other half of the observer
// question: the long-match tests have the same two observers for two hours,
// which says nothing about the list getting shorter. In a real lobby observers
// come and go (a spectator closes the game, the game retries), and the original's
// observer list is a vector with an erase on disconnect
// and an append per StartObserve.
//
// The list's length cannot leak, because MaxObservers caps it, so what is probed
// is what else the churn leaves behind, and that needs a DIFFERENTIAL: the same
// run without a single observer, subtracted. Comparing the heap before and after
// a churn that also fills the frame store would measure the store.
func TestObserverChurnDoesNotGrowTheList(t *testing.T) {
	withChurn := churnHeapMin(t, true)
	withoutChurn := churnHeapMin(t, false)
	excess := withChurn - withoutChurn
	if withChurn < withoutChurn {
		excess = 0
	}

	t.Logf("churn: %d observers joined and left over %d ticks, %d runs each; live heap %d KiB with the churn "+
		"against %d KiB without it, so %d KiB left behind by %d connections (%.1f bytes each)",
		longMatchChurnObservers, longMatchChurnObservers, longMatchChurnRuns, withChurn>>10, withoutChurn>>10,
		excess>>10, longMatchChurnObservers, float64(excess)/float64(longMatchChurnObservers))
	if excess > longMatchChurnResidueBudget {
		t.Errorf("%d observers joined and left and the live heap is %d KiB above a session that saw none of them, "+
			"over a budget of %d KiB. %d KiB for %d connections is %.0f bytes each, and an observer that is gone "+
			"should not be in the heap at all",
			longMatchChurnObservers, excess>>10, longMatchChurnResidueBudget>>10, excess>>10, longMatchChurnObservers,
			float64(excess)/float64(longMatchChurnObservers))
	}
}

// churnHeapMin is the smallest live heap churnHeap reports over longMatchChurnRuns
// fresh fixtures.
//
// The repetition is not paranoia: the first measurement of this shape in a
// process reads about 40 KiB high whatever it measures (the allocator's own
// structures are still cold), and that is 40 KiB of noise against a residue this
// file wants to resolve down to a few KiB. A minimum over three runs is immune
// to it, and the residue itself does not scale with the number of observers
// (measured: 41, 3, 3, 3 KiB for 125, 250, 500 and 1000 observers), which is
// the fact that says there is nothing per-connection here.
func churnHeapMin(t *testing.T, withObservers bool) uint64 {
	t.Helper()
	best := ^uint64(0)
	for range longMatchChurnRuns {
		if v := churnHeap(t, withObservers); v < best {
			best = v
		}
	}
	return best
}

// churnHeap runs the churn fixture once and reports the live heap at the end.
// With observers false it is the control: identical in every respect except that
// no observer ever joins.
func churnHeap(t *testing.T, withObservers bool) uint64 {
	t.Helper()
	d := newLongMatch(longMatchPageSize, longMatchMaxPagesToStore)
	d.obs = nil // the control has no observer connections to churn
	d.s.AddPeer(NewPeer(1, &longMatchConn{msgVersion: 5}))
	d.s.QueueGameData(1, []byte{0x02, 0x00, 0xAA})
	if _, _ = d.s.PopAckTick(); !d.s.Recording {
		t.Fatal("no frame store after the first ack: the fixture cannot register an observer at all")
	}

	var seated []*longMatchConn
	for i := range longMatchChurnObservers {
		if withObservers {
			c := &longMatchConn{peerID: uint64(1000 + i)}
			status, registered := d.s.StartObserve(c, c.peerID, 0)
			if status != 0 || !registered {
				t.Fatalf("round %d: StartObserve status %d registered %t; want 0 and true while the store still "+
					"holds the first frame of the match", i, status, registered)
			}
			if !d.s.ActivateObserver(c, longMatchTicksPerSecond) {
				t.Fatalf("round %d: ActivateObserver did not find the entry it had just registered", i)
			}
			seated = append(seated, c)
			// Retire the oldest one whenever the fixture holds its full set, so
			// the list is always emptying as well as filling.
			if len(seated) > longMatchChurnSeated {
				if !d.s.RemoveObserver(seated[0]) {
					t.Fatalf("round %d: RemoveObserver did not find the entry that was registered", i)
				}
				seated = seated[1:]
			}
		}
		d.tick(i)
	}
	if withObservers {
		if got := d.s.ObserverCount(); got != longMatchChurnSeated {
			t.Errorf("the observer list holds %d entries after %d joins and leaves; want the %d the fixture kept "+
				"seated", got, longMatchChurnObservers, longMatchChurnSeated)
		}
	}
	var ms runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}
