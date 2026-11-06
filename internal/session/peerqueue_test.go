package session

import (
	"io"
	"log"
	"runtime"
	"testing"

	"github.com/luskaner/ageBattleServer/internal/protocol"
)

// WHAT THIS FILE IS
//
// The other half of formula (2) in peersweep_test.go, and the half with no
// ceiling: the frame store is bounded per SESSION (one ceil of blocks for the
// whole session), while each peer's pending upload queue is bounded per PEER and
// **nothing bounds the sum**. So the worst-case footprint of a session is the
// sum of its per-peer caps, and the only way to know it is to saturate them.
//
//	entry count >= 0x12C (300)     the peer has 300 chunks
//	queued bytes >= 0x80000 (512 KiB)
//
// Both are the peer's own record: 40 bytes holding {begin, end, capacity} of the
// chunks, a u32 byte counter and the peer id, pushed
// when the peer joins and erased when it disconnects, and the erasure destroys
// whatever the peer had uploaded and not yet been flushed.
//
// ack_test.go already covers the cap's MECHANICS on a bare AckState with one or
// two slots. What is asked here, and was never asked, is what a session full of
// peers that all hit the cap costs, and whether the cap holds when a peer keeps
// uploading past it.
//
// The byte counter charges the payload alone, the client's chunk minus its u16
// length (chunkCharge), while the queued sub is [slot][len u16][payload]. A sub is
// a pooled buffer with a floor of 256 bytes (ack.go, subPool.New), so a small
// chunk costs 256 bytes of real memory while charging 62. That is why the REAL
// per-peer figure below is four times the charged one, and why the queue is
// asserted on both.
//
// A tick takes at most ackDrainPasses chunks from a peer and at most ackMaxSubs
// into one 0x0A, so a saturated queue drains over several ticks, not one.

// ---------------------------------------------------------------------------
// THE CAPS, and what each one bounds
// ---------------------------------------------------------------------------

const (
	// queueSatPeers is the peer count for the aggregate question: the worst case
	// of a session is the sum over its peers, so it has to be asked at a peer
	// count where the sum is worth asking about. 32 is the top of the sweep in
	// peersweep_test.go, so the two files agree on what a large session is.
	queueSatPeers = 32

	// queueSatSmallChunk is the chunk that makes the ENTRY cap bind rather
	// than the byte cap: 300 x (64 - 2) = 18600 charged bytes, which is 3.5% of
	// 0x80000, so all 300 entries fit and the 301st is dropped for being the
	// 301st.
	queueSatSmallChunk = 64

	// queueSatBigChunk is the chunk that makes the BYTE cap bind rather than
	// the entry cap: each charges 131070, four come to 524280 of 524288 bytes, so
	// the fifth chunk is dropped.
	queueSatBigChunk = 131072

	// queueSatBigAttempts is how many big chunks are offered past the cap. It is
	// the CAP and the reason this test cannot reach the memory the §25 test
	// reached: with the cap working, four are queued; with it broken, five would
	// be, at 147456 bytes each (measured), so 737 KiB. It is never anywhere near
	// the 20 GB that has to be killed by hand.
	queueSatBigAttempts = 5

	// queueSatEntryAttempts is how many small chunks are offered past the entry
	// cap: 400 against a cap of 300, so the number a broken cap would show is
	// distinguishable from the number a working one shows.
	queueSatEntryAttempts = maxAckQueueEntries + 100

	// queueSatPastAttempts is how many more uploads are driven AFTER the ENTRY
	// cap has been reached, to show that it is enforced rather than merely
	// reached: a queue that grows from here is the failure mode, and ten times
	// the cap is where it would be unmistakable.
	queueSatPastAttempts = 10 * maxAckQueueEntries

	// queueSatBigPastAttempts is the same for the BYTE cap, and it is twenty
	// rather than three thousand because the chunk here is 128 KiB: with the cap
	// broken, twenty of them are 2.7 MiB, which is the most this file will ever
	// hold for that assertion.
	queueSatBigPastAttempts = 20

	// queueSatRealPerSub is what one queued sub costs the allocator: the pooled
	// buffer's 256-byte floor (ack.go, subPool.New) plus its 24-byte slice header
	// and the pointer to it in the slot's queue, and then 30% for the slack the
	// queue's own growth leaves. A cap that stopped binding would put a 147456-byte
	// buffer where this says 256, which is 576x, so the margin here does not
	// have to be generous to be sharp.
	queueSatRealPerSub = 256 + 24

	// queueSatMaxRealPerPeer is the ceiling for one saturated peer's real
	// footprint: the entry cap times queueSatRealPerSub, times 1.3 for the
	// vector's slack. 300 x 280 x 1.3 = 109200, against a measured 88 KiB.
	queueSatMaxRealPerPeer = maxAckQueueEntries * queueSatRealPerSub * 13 / 10

	// queueSatHeapPasses is how many times the heap measurement runs, keeping
	// the minimum: the first measurement of this shape in a process reads tens
	// of KiB high whatever it measures (SESSION-JOURNAL.md 34).
	queueSatHeapPasses = 3
)

// queueFootprint returns what the session's pending sub-messages COST (which is
// exactly what the byte counters sum: the queue charges chunkCharge(sub) on push
// and refunds it on pop) and what the allocator HOLDS for them.
//
// The charged figure is the original's unit and is exact. The real figure carries
// Go's pooling and size classes, so it is only ever used against a bound that
// says which of those is in play.
func queueFootprint(a *AckState) (charged, real int) {
	for _, q := range a.queued {
		if q == nil {
			continue
		}
		for _, sub := range q.subs[q.head:] {
			charged += chunkCharge(sub.b)
			real += cap(sub.b)
		}
		real += cap(q.subs) * 8 // one pointer per queued sub, plus growth slack
	}
	return charged, real
}

// slotBytes reports one slot's accounting: how many subs it is holding and how
// many bytes it has charged for them, which is the pair the two caps
// bound. It reads the map directly because this file is in the package and the
// figures are the point.
func slotBytes(a *AckState, slot byte) (count, charged int) {
	if q := a.queued[slot]; q != nil {
		return q.len(), q.bytes
	}
	return 0, 0
}

// quietDropLog silences the drop log for the duration of a test. The original
// writes a line for every dropped packet and so does this,
// which at the saturation below is 3200 lines of a message the tests here assert
// on anyway. It is restored by t.Cleanup, and it is safe because nothing in this
// package runs its tests in parallel.
func quietDropLog(t *testing.T) {
	t.Helper()
	quiet := log.Writer()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(quiet) })
}

// TestPeerUploadQueueStopsAtTheOriginalsCap drives ONE peer to each of the two
// caps and then keeps pushing, which is the difference between a cap and a
// comment: a queue that has reached its limit and one that is enforcing it are
// the same observation until the peer uploads more.
func TestPeerUploadQueueStopsAtTheOriginalsCap(t *testing.T) {
	quietDropLog(t)

	// --- the entry cap, 0x12C chunks ----------------------------------------
	s := NewSession(1)
	s.AddPeer(NewPeer(1, &sweepConn{msgVersion: 5}))
	small := make([]byte, queueSatSmallChunk)
	for range queueSatEntryAttempts {
		s.QueueGameData(1, small)
	}
	count, charged := slotBytes(&s.Ack, 1)
	qCharged, qReal := queueFootprint(&s.Ack)
	t.Logf("entry cap: offered %d chunks of %d bytes to one peer; the slot holds %d entries and %d charged bytes, "+
		"the queue %d charged and %d real bytes over %d subs",
		queueSatEntryAttempts, queueSatSmallChunk, count, charged, qCharged, qReal, queuedSubs(&s.Ack))
	if count != maxAckQueueEntries {
		t.Errorf("one peer offered %d chunks holds %d of them; the cap is %d (0x12C). A queue "+
			"that keeps taking them is not bounded by anything", queueSatEntryAttempts, count, maxAckQueueEntries)
	}
	if want := maxAckQueueEntries * (queueSatSmallChunk - 2); charged != want {
		t.Errorf("the slot charged %d bytes for %d chunks of %d, want %d (the payload alone, without the "+
			"u16 length, chunkCharge)", charged, count, queueSatSmallChunk, want)
	}
	if qCharged != charged {
		t.Errorf("the queued subs are %d bytes long and the slot's counter says %d: the two have to be the same "+
			"number, because the drain takes one back off for every sub it flushes (slotQueue.pop)",
			qCharged, charged)
	}
	if qReal > queueSatMaxRealPerPeer {
		t.Errorf("one peer at the entry cap holds %d real bytes, over the %d the bound allows (%d subs of a "+
			"256-byte pooled buffer and a 24-byte header each, plus 30%% for the vector's slack)",
			qReal, queueSatMaxRealPerPeer, maxAckQueueEntries)
	}
	// The drop must not disable the tick: the original marks the peer as having
	// sent data on the drop path too, and a session whose tick went quiet because
	// a peer overran would be a much worse defect than an unbounded queue.
	// A tick takes at most ackDrainPasses chunks from the peer, so the 300 drain
	// over ceil(300 / 17) = 18 ticks and not one.
	drainTicks := (maxAckQueueEntries + ackDrainPasses - 1) / ackDrainPasses
	for tick := range drainTicks {
		body, _ := s.PopAckTick()
		if body == nil {
			t.Fatal("PopAckTick returned nothing after 400 uploads of which 300 were taken: the dropped packets " +
				"must not stop the tick")
		}
		if want := min(ackDrainPasses, maxAckQueueEntries-tick*ackDrainPasses); int(body[1]) != want {
			t.Fatalf("tick %d flushed %d chunks, want %d: one peer gives at most %d a tick", tick+1, body[1], want,
				ackDrainPasses)
		}
	}
	if !s.Ack.QueueEmpty(1) {
		count, _ = slotBytes(&s.Ack, 1)
		t.Errorf("after %d draining ticks the slot still holds %d subs", drainTicks, count)
	}

	// --- and the ENTRY cap HOLDS while the peer keeps pushing ---------------
	// A cap that has been reached and a cap that is enforced are the same
	// observation until the peer uploads more, so the peer uploads ten times the
	// cap past it. With the cap broken this would queue 3000 subs instead of 300,
	// which at 256 bytes a pooled buffer is 768 KB rather than 77 KB: the
	// assertion is exact, so the memory does not have to be.
	beforeCount, beforeCharged, beforeReal := count, charged, qReal
	for range queueSatPastAttempts {
		s.QueueGameData(1, small)
	}
	count, charged = slotBytes(&s.Ack, 1)
	_, qReal = queueFootprint(&s.Ack)
	t.Logf("the peer then uploaded %d more small chunks: %d entries, %d charged bytes and %d real bytes, against "+
		"%d, %d and %d before", queueSatPastAttempts, count, charged, qReal, beforeCount, beforeCharged, beforeReal)
	if count != beforeCount || charged != beforeCharged || qReal != beforeReal {
		t.Errorf("after %d more uploads past the entry cap the slot holds %d entries, %d charged and %d real "+
			"bytes, against %d, %d and %d before. The cap stopped holding", queueSatPastAttempts, count, charged,
			qReal, beforeCount, beforeCharged, beforeReal)
	}

	// --- the byte cap, 0x80000 bytes ----------------------------------------
	// One chunk of 131072 bytes, so the byte cap binds in three and the entry cap
	// at 300 never comes near it. The attempts are capped at five, so even a
	// broken cap stays at 700 KiB.
	s = NewSession(1)
	s.AddPeer(NewPeer(1, &sweepConn{msgVersion: 5}))
	big := make([]byte, queueSatBigChunk)
	for range queueSatBigAttempts {
		s.QueueGameData(1, big)
	}
	count, charged = slotBytes(&s.Ack, 1)
	_, qReal = queueFootprint(&s.Ack)
	// A chunk is taken while the charge stays below the cap, so the cap binds at
	// the most chunks whose charges add up to less than 0x80000.
	wantEntries := (maxAckQueueBytes - 1) / (queueSatBigChunk - 2)
	t.Logf("byte cap: offered %d chunks of %d bytes to one peer; the slot holds %d entries and %d charged bytes "+
		"(%d real), and %d is where (0x80000 - 1) / %d lands",
		queueSatBigAttempts, queueSatBigChunk, count, charged, qReal, wantEntries, queueSatBigChunk-2)
	if count != wantEntries {
		t.Errorf("one peer offered %d chunks of %d bytes holds %d of them, want %d: (0x80000 - 1) / %d is %d, so "+
			"the cap binds at that many",
			queueSatBigAttempts, queueSatBigChunk, count, wantEntries, queueSatBigChunk-2, wantEntries)
	}
	if charged >= maxAckQueueBytes {
		t.Errorf("the slot charged %d bytes, at or over the %d the cap is", charged, maxAckQueueBytes)
	}
	// Each sub of 131073 bytes lands in a 139264-byte object (measured), which
	// is the real cost of the byte cap whatever the accounting says. This is the
	// figure that says the cap is keeping real memory down and not just a
	// counter: a peer that queued one more chunk of this size would be at 680 KiB
	// and the accounting would say it is 120 KiB over the limit.
	if want := wantEntries * 139264; qReal > want+want/16 {
		t.Errorf("one peer at the byte cap holds %d real bytes, over the %d that %d subs of %d bytes cost as "+
			"Go objects (measured: each lands in a 139264-byte size class). The accounting says %d bytes are "+
			"queued and this is what they cost, so a figure well above the first is memory the cap is not "+
			"holding down", qReal, want+want/16, wantEntries, queueSatBigChunk, charged)
	}

	// --- and the BYTE cap HOLDS while the peer keeps pushing ----------------
	// Same chunk this time, because that is the one that is being dropped: a
	// different chunk would be a different queue entry to fill. The attempts are
	// capped at twenty, so even a broken cap stops at 20 x 139264 = 2.7 MiB.
	beforeCount, beforeCharged, beforeReal = count, charged, qReal
	for range queueSatBigPastAttempts {
		s.QueueGameData(1, big)
	}
	count, charged = slotBytes(&s.Ack, 1)
	_, qReal = queueFootprint(&s.Ack)
	t.Logf("the peer then offered %d more chunks of %d bytes: %d entries, %d charged bytes and %d real bytes, "+
		"against %d, %d and %d before", queueSatBigPastAttempts, queueSatBigChunk, count, charged, qReal,
		beforeCount, beforeCharged, beforeReal)
	if count != beforeCount || charged != beforeCharged || qReal != beforeReal {
		t.Errorf("after %d more uploads past the byte cap the slot holds %d entries, %d charged and %d real "+
			"bytes, against %d, %d and %d before. The cap stopped holding, and this is the cap with no "+
			"global limit above it: at %d peers it would be %d",
			queueSatBigPastAttempts, count, charged, qReal, beforeCount, beforeCharged, beforeReal, queueSatPeers,
			queueSatPeers*maxAckQueueBytes)
	}
}

// TestSaturatedPeerQueuesScaleWithThePeerCount is the question the second half of
// formula (2) is really about. Neither cap has a global limit, so a session's
// worst case is the SUM over its peers: at 32 peers the entry cap alone is
// 32 x 300 chunks, and the byte cap would be 32 x 512 KiB = 16 MiB of accounted
// payload. Nothing in the original stops that, so what has to hold is that it is
// exactly what the per-peer caps say it is, and not more.
//
// The byte cap is deliberately NOT driven on all 32 peers: 32 x 512 KiB is 16 MiB
// charged and about 17 MiB real for a figure the per-peer case already measures,
// and the aggregate assertion below is on the entry cap, which costs 2.8 MiB.
func TestSaturatedPeerQueuesScaleWithThePeerCount(t *testing.T) {
	quietDropLog(t)
	sat := queueSatHeapMin(t, true)
	control := queueSatHeapMin(t, false)
	perPeer := float64(sat.heap-control.heap) / queueSatPeers

	t.Logf("queue saturation: %d peers, each offered %d chunks of %d bytes, %d passes of each measurement; "+
		"live heap %d KiB saturated against %d KiB with empty queues, so %.1f KiB and %.0f bytes a peer "+
		"(a saturated peer is charged %d of the %d its cap allows, and holds %d subs in %d real bytes)",
		queueSatPeers, queueSatEntryAttempts, queueSatSmallChunk, queueSatHeapPasses, sat.heap>>10, control.heap>>10,
		float64(sat.heap-control.heap)/1024, perPeer, sat.chargedPerPeer, maxAckQueueBytes, sat.entriesPerPeer,
		sat.realPerPeer)

	// The aggregate is the per-peer cap times the peer count, and that is the
	// whole claim: nothing here is bounded by anything but the sum of the peers.
	if want := queueSatPeers * maxAckQueueEntries; sat.entries != want {
		t.Errorf("%d saturated peers hold %d subs in all, want %d = %d peers x the %d cap. There is no global "+
			"limit on the entry cap, so this number has to be the product and nothing else",
			queueSatPeers, sat.entries, want, queueSatPeers, maxAckQueueEntries)
	}
	if want := queueSatPeers * maxAckQueueEntries * (queueSatSmallChunk - 2); sat.charged != want {
		t.Errorf("%d saturated peers charged %d bytes in all, want %d", queueSatPeers, sat.charged, want)
	}
	// The aggregate byte ceiling the formula implies, which is the number a
	// deployment has to plan for: peers x 512 KiB, with no ceiling of its own.
	if ceiling := queueSatPeers * maxAckQueueBytes; sat.charged > ceiling {
		t.Errorf("%d saturated peers charged %d bytes against the %d the per-peer caps imply", queueSatPeers,
			sat.charged, ceiling)
	}
	if got := float64(sat.real) / queueSatPeers; got > queueSatMaxRealPerPeer {
		t.Errorf("a saturated peer holds %.0f real bytes, over the %d the bound allows (%d subs x %d, plus slack)",
			got, queueSatMaxRealPerPeer, maxAckQueueEntries, queueSatRealPerSub)
	}
	if perPeer > queueSatMaxRealPerPeer {
		t.Errorf("a saturated peer costs %.0f bytes of live heap against the %d the bound allows. 300 chunks of "+
			"%d bytes is %d charged and %d real in one pooled buffer each, so anything above that is a queue "+
			"holding something the accounting does not know about",
			perPeer, queueSatMaxRealPerPeer, queueSatSmallChunk, maxAckQueueEntries*(1+queueSatSmallChunk),
			maxAckQueueEntries*queueSatRealPerSub)
	}

	// --- and the session comes back when the queues drain -------------------
	// "Bounded" has to include the way out: a queue that stays full after the
	// ticks that should have flushed it is a different defect. One 0x0A carries
	// at most ackMaxSubs sub-messages and at most ackDrainPasses of one peer, so
	// 9600 of them drain over 40 ticks: 255 = 7 x 32 + 31, so each of the first
	// 37 takes 8 chunks from peers 1..31 and 7 from peer 32, the 38th empties
	// peers 1..31 (4 each) and takes 17 of peer 32's remaining 41, and the last
	// two take its 24 at 17 a tick.
	s := NewSession(1)
	s.pageSize = peerSweepPageSize
	s.maxPagesPerMatch = peerSweepMaxPagesToStore
	conns := make([]*sweepConn, queueSatPeers)
	for i := range conns {
		conns[i] = &sweepConn{msgVersion: 5}
		s.AddPeer(NewPeer(byte(i+1), conns[i]))
	}
	small := make([]byte, queueSatSmallChunk)
	for i := range conns {
		for range queueSatEntryAttempts {
			s.QueueGameData(byte(i+1), small)
		}
	}
	queuedTotal := queueSatPeers * maxAckQueueEntries
	if got := queuedSubs(&s.Ack); got != queuedTotal {
		t.Fatalf("the session queued %d subs for %d saturated peers, want %d", got, queueSatPeers, queuedTotal)
	}
	const drainTicks = 40
	wantPerTick := func(tick int) int {
		switch {
		case tick < 37:
			return ackMaxSubs
		case tick == 37:
			return 31*4 + ackDrainPasses
		case tick == 38:
			return ackDrainPasses
		}
		return 24 - ackDrainPasses
	}
	flushed, flushedBytes := 0, 0
	for tick := range drainTicks {
		body, _ := s.PopAckTick()
		if body == nil {
			t.Fatalf("PopAckTick returned nothing at tick %d with subs still queued", tick+1)
		}
		// One 0x0A a tick to every peer, carrying the flushed subs: the queue going
		// out on the wire is what makes "bounded" mean anything.
		s.SendToAll(protocol.MsgEndOfGameData, body)
		if want := wantPerTick(tick); int(body[1]) != want {
			t.Fatalf("tick %d flushed %d subs, want %d", tick+1, body[1], want)
		}
		if len(body) >= ackMaxSize {
			t.Fatalf("tick %d built a %d-byte body, at or over the %d limit", tick+1, len(body), ackMaxSize)
		}
		flushed += int(body[1])
		flushedBytes += len(body)
	}
	if got := queuedSubs(&s.Ack); got != 0 || flushed != queuedTotal {
		t.Errorf("%d subs are still queued after %d draining ticks, which flushed %d of %d", got, drainTicks,
			flushed, queuedTotal)
	}
	if got := s.ReplayPageCount(); got != drainTicks {
		t.Errorf("the store holds %d frames after %d ticks, want one a tick", got, drainTicks)
	}
	for i, c := range conns {
		if c.acks != drainTicks {
			t.Errorf("peer %d received %d acks for %d ticks", i+1, c.acks, drainTicks)
		}
	}
	if want := queuedTotal * (1 + queueSatSmallChunk); flushedBytes < want {
		t.Errorf("the flushed ack bodies add up to %d bytes, under the %d the %d queued subs should add up to: "+
			"the drain is not carrying everything the queue was holding", flushedBytes, want, queuedTotal)
	}
	// And the memory goes back down, which is the other half of "bounded".
	after := liveHeapAfterGC()
	runtime.KeepAlive(s)
	t.Logf("after %d draining ticks the live heap is %d KiB, against %d KiB saturated and %d KiB with empty "+
		"queues, and the acks that carried the flush add up to %d bytes", drainTicks, after>>10, sat.heap>>10,
		control.heap>>10, flushedBytes)
	if after > sat.heap {
		t.Errorf("the live heap is %d KiB after the queue drained, above the %d KiB it held while the queue was "+
			"full: a drain that gives the memory back is the whole point of the cap", after>>10, sat.heap>>10)
	}
}

// queueSatReading is one saturation measurement: the live heap with the session
// still reachable, and the three queue figures of the same run. The per-peer
// fields are the same three divided by queueSatPeers, which is the form the
// assertions read.
type queueSatReading struct {
	heap           uint64
	entries        int
	charged        int
	real           int
	entriesPerPeer int
	chargedPerPeer int
	realPerPeer    int
}

// queueSatHeapMin runs the saturation fixture queueSatHeapPasses times and keeps
// the smallest live heap, with the queue figures of that same run. The
// repetition is not paranoia: the first measurement of this shape in a process
// reads tens of KiB high whatever it measures (SESSION-JOURNAL.md 34), against a
// signal of 85 KB a peer here.
func queueSatHeapMin(t *testing.T, saturate bool) queueSatReading {
	t.Helper()
	best := queueSatReading{heap: ^uint64(0)}
	for range queueSatHeapPasses {
		got := queueSatHeap(t, saturate)
		if got.heap < best.heap {
			best = got
		}
	}
	return best
}

// queueSatHeap builds one saturation fixture and reports its live heap with the
// session still reachable. With saturate false it is the control: the same
// session, the same peers, and not one upload, so the difference between the two
// is the queues and nothing else.
func queueSatHeap(t testing.TB, saturate bool) queueSatReading {
	s := NewSession(1)
	s.pageSize = peerSweepPageSize
	s.maxPagesPerMatch = peerSweepMaxPagesToStore
	for i := range queueSatPeers {
		s.AddPeer(NewPeer(byte(i+1), &sweepConn{msgVersion: 5}))
	}
	if saturate {
		small := make([]byte, queueSatSmallChunk)
		for i := range queueSatPeers {
			for range queueSatEntryAttempts {
				s.QueueGameData(byte(i+1), small)
			}
		}
	}
	charged, real := queueFootprint(&s.Ack)
	r := queueSatReading{
		entries:        queuedSubs(&s.Ack),
		charged:        charged,
		real:           real,
		entriesPerPeer: queuedSubs(&s.Ack) / queueSatPeers,
		chargedPerPeer: charged / queueSatPeers,
		realPerPeer:    real / queueSatPeers,
	}
	r.heap = liveHeapAfterGC()
	runtime.KeepAlive(s)
	return r
}
