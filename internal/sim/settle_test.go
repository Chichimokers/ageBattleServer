package sim

import (
	"strings"
	"testing"
	"github.com/luskaner/ageBattleServer/internal/server"
	"time"
)

// The settle window decides how much of the match the replay gets to read, and
// reading less of it than the recording holds is indistinguishable, in the
// report, from the server having said less. That is what these tests are about:
// each one is a shape of capture whose settle used to be wrong in a way that
// produced a divergence the server did not have.

// settleBase is the base settleForTruth takes as its input in the tests below: three
// periods, floored at 400 ms, which is what settleFor returned before the observer
// step's period became part of it. settleForTruth is a pure function of that base, so
// the tests that exercise the WIDENING pass a base that is genuinely shorter than the
// whole second they need to reach -- which is the situation the bsPort capture is in.
const settleBase = 400 * time.Millisecond

// TestTheSettleCoversTheObserverStep is the arithmetic, and it is the whole fix. The
// observer step is a PHASE: its cursor takes interval+1 values, wrapping only
// when it is strictly greater than the interval, and the entry at index 0 is
// served on the ticks where it is 0. So an observer that activates just after a
// tick waits up to interval+1 ticks, which is the bound internal/session's
// TestTheWorstPhaseCostsTheWholePeriod measures, and the settle has to be at
// least that plus the tick the write happens on.
//
// The number is not read out of this file: it comes from the server's own
// ObserverUpdateIntervalTicks, which is the same function the session is configured
// with. A settle derived from a copy of that number would be a settle that could
// disagree with the step it is waiting for, which is the defect itself.
func TestTheSettleCoversTheObserverStep(t *testing.T) {
	for _, tc := range []struct {
		name    string
		period  float64
		flagSet bool
		flagMS  uint
	}{
		{"the original's defaults", 125, false, 0},
		{"a fast period", 10, false, 0},
		{"the flag given, 1 tick", 125, true, 125},
		{"the flag given, 8 ticks", 125, true, 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &server.Config{
				SimulationPeriod:           tc.period,
				ObserverUpdateIntervalMS:   tc.flagMS,
				ObserverUpdateIntervalSet:  tc.flagSet,
			}
			ticks := server.ObserverUpdateIntervalTicks(cfg)
			if ticks < 1 {
				ticks = 1
			}
			want := time.Duration(ticks+2) * simPeriod(cfg)
			got := settleFor(cfg, Options{}, true)
			if got < want {
				t.Fatalf("settle = %s, but an observer activated at the end of the recording "+
					"waits up to %d ticks (%s) for the step's first visit and the write happens on "+
					"that tick: the replay would close before the frame existed",
					got, ticks+1, time.Duration(ticks+1)*simPeriod(cfg))
			}
		})
	}
}

// TestTheSettleStaysInsideTheCeiling: the floor is a floor and not a licence. The
// ceiling exists so that a 10 s period does not turn a test into a 100 s wait, and a
// configuration whose step period is longer than the ceiling is a configuration the
// settle CANNOT cover. The ceiling wins, as it has to, and what is checked here is that
// the gap is visible rather than silent: observerStepPeriod reports what it would take.
func TestTheSettleStaysInsideTheCeiling(t *testing.T) {
	cfg := &server.Config{SimulationPeriod: 12500, ObserverUpdateIntervalSet: false}
	if got := settleFor(cfg, Options{}, true); got != 3*time.Second {
		t.Fatalf("settle = %s, want the 3s ceiling", got)
	}
	want := observerStepPeriod(cfg) * simPeriod(cfg)
	if want <= 3*time.Second {
		t.Fatalf("this case is meant to be one the ceiling cannot cover, and the step period is only %s", want)
	}
}

// An explicit settle is the caller's, whatever it is. opts.Settle exists so a test can
// ask for a shorter read on purpose, and this is what keeps the floor above from
// overruling it: a test that wants to see the truncation has to be able to produce it.
func TestAnExplicitSettleWinsOverTheStepFloor(t *testing.T) {
	cfg := &server.Config{SimulationPeriod: 125}
	if got := settleFor(cfg, Options{Settle: 100 * time.Millisecond}, true); got != 100*time.Millisecond {
		t.Fatalf("settle = %s, want the 100ms the caller asked for", got)
	}
}

// TestTheSettleSkipsTheObserverStepWithoutAnObserver: the observer step's period is
// waited for because an observer activated at the end of the recording has to be
// served, and a capture with no observer has no step to wait for. The settle is then
// the tick ack's own three periods, floored at 400 ms, which is what it was before the
// step's period became part of it.
//
// The two committed captures this is about are 2026-09-28_205830 (original at the
// front, no observer) and 2026-09-30_201122 (bsPort, no observer). The rule is keyed on
// whether the capture has an observer and not on its name, so a capture added later
// gets the right settle without anyone remembering to list it.
func TestTheSettleSkipsTheObserverStepWithoutAnObserver(t *testing.T) {
	cfg := &server.Config{SimulationPeriod: 125}
	withObserver := settleFor(cfg, Options{}, true)
	without := settleFor(cfg, Options{}, false)
	if without != 400*time.Millisecond {
		t.Fatalf("settle with no observer = %s, want the 400 ms floor: the tick ack needs three periods "+
			"(%s) and the floor is above that", without, 3*simPeriod(cfg))
	}
	if withObserver <= without {
		t.Fatalf("settle with an observer = %s and without = %s: the observer step's period has to be the "+
			"difference, or this rule is not the one being tested", withObserver, without)
	}
}

// TestSettleForTruthReachesTheLastWholeSecond is the measured case: on
// 2026-09-30_201122_c4e7c284ff1c the last captured input is stamped t=47s and the
// original's last eight messages are stamped t=47s too, so the recording covers
// part of the second 47..48s. A 400 ms settle reads to t=47.4s and would report
// "the original sent 8 acks, we sent 3" against a server that sent all eight.
func TestSettleForTruthReachesTheLastWholeSecond(t *testing.T) {
	origin := time.Date(2026, 9, 30, 20, 27, 20, 0, time.UTC)
	last := origin.Add(47 * time.Second) // the truth's last stamp is a whole second
	span := 47 * time.Second              // the input span is the same instant

	got, need, capped := settleForTruth(settleBase, span, last, origin)
	if capped {
		t.Fatalf("a capture whose recording ends in the same second as its input must not be capped, got %s", got)
	}
	if want := time.Second; got != want {
		t.Fatalf("settle = %s, want %s: the replay has to reach the end of the second the capture stops in, or it reads less of the match than the recording holds", got, want)
	}
	if end := span + got; end != 48*time.Second {
		t.Fatalf("the replay would close at t=%s, which is not the end of the last second of the recording (t=48s)", end)
	}
	if need != got {
		t.Fatalf("an uncapped widening must report what it would have taken to cover the second whole as what it took, got %s and %s", got, need)
	}
}

// TestSettleForTruthIsTheStampResolution is the same thing stated as the general
// rule: whatever second the last stamp falls in, the replay closes at that
// second's end.
func TestSettleForTruthIsTheStampResolution(t *testing.T) {
	origin := time.Date(2026, 9, 30, 20, 27, 20, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		span    time.Duration
		lastIn  time.Duration
		wantEnd time.Duration
	}{
		{"input and last stamp in the same second", 47 * time.Second, 47 * time.Second, 48 * time.Second},
		{"the stamp is just past a second boundary", 9 * time.Second, 9*time.Second + 10*time.Millisecond, 10 * time.Second},
		{"the recording runs on for seconds after the input", 10 * time.Second, 10*time.Second + 400*time.Millisecond, 11 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, capped := settleForTruth(settleBase, tc.span, origin.Add(tc.lastIn), origin)
			if capped {
				t.Fatalf("unexpected cap at %s", got)
			}
			if end := tc.span + got; end != tc.wantEnd {
				t.Fatalf("the replay closes at t=%s, want t=%s (settle %s)", end, tc.wantEnd, got)
			}
		})
	}
}

// TestSettleForTruthLeavesASettleThatAlreadyCoversIt is the other direction: a
// capture whose base settle already reaches past the last second must not be
// shortened, and must not be reported as widened.
func TestSettleForTruthLeavesASettleThatAlreadyCoversIt(t *testing.T) {
	origin := time.Date(2026, 9, 30, 20, 27, 20, 0, time.UTC)
	base := 3 * time.Second
	got, _, capped := settleForTruth(base, 3*time.Second, origin.Add(3*time.Second), origin)
	if got != base || capped {
		t.Fatalf("settle = %s capped=%v, want the base %s untouched", got, capped, base)
	}
}

// TestSettleForTruthCapsAndSays so: a recording that goes on for minutes after
// the last input would need a settle as long as that gap, which is the harness's
// own reading and not the match. The cap is there for the budget, and the caller
// reports it, so the read is not silently shorter than the recording.
func TestSettleForTruthCapsAndSays(t *testing.T) {
	origin := time.Date(2026, 9, 30, 20, 27, 20, 0, time.UTC)
	got, need, capped := settleForTruth(settleBase, 10*time.Second, origin.Add(601*time.Second), origin)
	if !capped {
		t.Fatal("a settle of ten minutes must be capped: it is the harness reading, not the match")
	}
	if want := settleBase + maxExtraSettleForTruth; got != want {
		t.Fatalf("settle = %s, want %s", got, want)
	}
	// The settle the caller is told about is what was NOT taken. Without it the
	// reason it writes into the report has nothing to quote, and the first version
	// of this code quoted the difference between a value and itself: 0s.
	if want := 602*time.Second - 10*time.Second; need != want {
		t.Fatalf("what it would have taken = %s, want %s: the last stamp is at t=601s, so the second it falls in ends at t=602s", need, want)
	}
	if need <= got {
		t.Fatalf("what it would have taken (%s) must be longer than what it took (%s) for the cap to be the reason the report gives", need, got)
	}
}

// TestSettleForTruthWithoutAClockChangesNothing: a capture with no timestamped
// message has no clock to measure against, and the base settle is all there is.
func TestSettleForTruthWithoutAClockChangesNothing(t *testing.T) {
	if got, _, capped := settleForTruth(settleBase, 47*time.Second, time.Time{}, time.Now()); got != settleBase || capped {
		t.Fatalf("settle = %s capped=%v, want the base %s", got, capped, settleBase)
	}
}

// TestLastTruthMomentSeesBothDirections is the measurement the widen rests on: the
// capture's last instant is the last thing EITHER side of the recording carries,
// because the recording stops when the last byte it saw was written.
func TestLastTruthMomentSeesBothDirections(t *testing.T) {
	base := time.Date(2026, 9, 30, 20, 27, 20, 0, time.UTC)
	cap := &Capture{Conns: []*Conn{
		{
			Up:   []*Msg{{Type: 0x15, At: base}},
			Down: []*Msg{{Type: 0x0A, At: base.Add(47 * time.Second)}},
		},
		{
			Up:   []*Msg{{Type: 0x09, At: base.Add(19 * time.Second)}},
			Down: []*Msg{{Type: 0x14, At: base.Add(12 * time.Second)}},
		},
	}}
	if got := cap.lastTruthMoment(); !got.Equal(base.Add(47 * time.Second)) {
		t.Fatalf("lastTruthMoment = %s, want %s", got, base.Add(47*time.Second))
	}
}

// TestTheSettleIsExplainedInTheReport reads the report's own words. A reader who
// sees the replay reading on past the last captured input has to be able to find
// out why, and the deviation that says so is the only place that number is
// justified outside a test: a report without it is a harness change nobody can
// audit.
//
// It replays the whole 48 s of a real capture, so it is gated by the same rule every
// generated capture test is (short.go). What it adds is that Check wires the
// deviation up, and that needs the capture. This capture has no observer, so the
// settle it reports is the one settleForTruth widened to reach the end of the
// recording's last whole second; the observer step's own deviation is covered by
// TestTheObserverStepDeviationExplainsTheSettleItSets, which needs no replay.
func TestTheSettleIsExplainedInTheReport(t *testing.T) {
	if skipLongCapture(t, "testdata/2026-09-30_201122_c4e7c284ff1c") {
		return
	}
	rep, err := Check("testdata/2026-09-30_201122_c4e7c284ff1c", Options{})
	if err != nil {
		t.Fatal(err)
	}
	var whys []string
	for _, d := range rep.Running {
		if d.Field == "settle window" {
			whys = append(whys, d.Why)
		}
	}
	if len(whys) == 0 {
		t.Fatal("the replay reads on past the last input and the report does not say why: " +
			"the settle has to be accounted for in the list of what the harness did not reproduce")
	}
	joined := strings.Join(whys, "\n")
	if !strings.Contains(joined, "whole seconds") || !strings.Contains(joined, "same span") {
		t.Fatalf("no settle deviation explains why the replay reads past the last input: %q", joined)
	}
	// The settled window must be the one the tail rule reads, or the split by the
	// window is still the old one.
	if rep.window.End-rep.window.Start < time.Second {
		t.Fatalf("the window the tail rule splits on is %s wide, so the tail split reads a shorter span than the recording holds",
			rep.window.End-rep.window.Start)
	}
}

// TestTheObserverStepDeviationExplainsTheSettleItSets is the other half: when the
// settle IS the observer step's period, the report has to say so with the
// arithmetic, because 1,25 s of reading past the last input is otherwise
// indistinguishable from a fudge. No capture is replayed: the deviation is a
// function of the configuration and the settle.
func TestTheObserverStepDeviationExplainsTheSettleItSets(t *testing.T) {
	cfg := &server.Config{SimulationPeriod: 125}
	settle := settleFor(cfg, Options{}, true)
	d, ok := settleStepDeviation(cfg, settle)
	if !ok {
		t.Fatalf("a settle of %s set by the observer step's period (%s) has no deviation explaining it",
			settle, observerStepPeriod(cfg)*simPeriod(cfg))
	}
	if d.Field != "settle window" || d.Running != settle.String() {
		t.Fatalf("deviation = %+v, want the settle window at %s", d, settle)
	}
	if !strings.Contains(d.Why, "interval+1 ticks") || !strings.Contains(d.Why, "phase") {
		t.Fatalf("the deviation does not explain the observer step's period: %q", d.Why)
	}
	// And it must not claim a settle the step did not set: a settle shorter than the
	// step's period is the tick ack's, and explaining it as the step's would be a
	// report that justifies a number it did not use.
	if _, ok := settleStepDeviation(cfg, 400*time.Millisecond); ok {
		t.Fatal("a settle shorter than the step's period is not the step's, and must not be explained as it")
	}
}