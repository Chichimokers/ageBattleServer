package sim

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/luskaner/ageBattleServer/internal/logger"
	"github.com/luskaner/ageBattleServer/internal/server"
)

// Deviation is one field the harness changed on purpose when it rebuilt the
// server, with the reason. The list is the audit trail: whatever is not on it
// has to be identical to what the capture recorded, and the report prints it in
// full so a reader can see what was not reproduced.
type Deviation struct {
	Field    string
	Recorded string
	Running  string
	Why      string
}

// maxCollected bounds the bytes one connection may deliver, so a capture that
// was cut mid-game cannot make a test allocate without limit. A real observer
// stream is a few megabytes; the ceiling is two orders of magnitude above that.
const maxCollected = 64 << 20

// replayRun is what one replay produced: the answer per connection, the
// configuration it ran with, the deviations it needed, and the three facts the
// report needs to place our messages on the same time axis as the original's.
// Started is the instant the input was scheduled from (the capture's own origin),
// so a message's arrival minus Started is the elapsed time the report speaks in.
type replayRun struct {
	Conns      []*Conn
	Config     *server.Config
	Deviations []Deviation
	Started    time.Time
	// Span is how long the capture's own client traffic lasted, Settle how much
	// longer the replay kept reading after the last of it, and InputEnd the two
	// together as the replay really ran them: the socket closes at
	// InputEnd+Settle, measured from Started.
	//
	// InputEnd is not Span when the input was compressed, and then it is on the
	// compressed clock ours are stamped on and the original's are not, which
	// Window.Speed says out loud instead of letting two series be compared.
	Span     time.Duration
	Settle   time.Duration
	InputEnd time.Duration
	Speed    float64
	// lastTruth is the last moment the capture's own side of the run recorded, in
	// its own clock. It is what settleForTruth measures the last second against.
	lastTruth time.Time
}

// Window is the tail of the replay: the moment the last input was fed and the
// moment the connection was closed. A message of ours inside it was produced
// with no input left to cause it, which is the difference the report has to make
// visible. Its two ends are the ones the replay really used, not the ones it
// meant to use: a report about a window the replay did not read in is a report
// about nothing.
func (r replayRun) Window() Window {
	return Window{Start: r.InputEnd, End: r.InputEnd + r.Settle, Speed: r.Speed}
}

// replay starts a server built from the capture's own command line and drives
// every captured connection into it over a real socket, then returns what the
// server answered. The socket is the faithful path and not an internal
// dispatchMessage call: the transports differ (raw length-prefixed on bsPort,
// WebSocket frames on webSocketPort, and the observer flag that changes how a
// client's socket is processed), and a comparison that skipped them would not be
// comparing the thing that breaks.
func replay(cap *Capture, cfg *server.Config, opts Options) (replayRun, error) {
	run := replayRun{}
	runCfg, dev := prepareConfig(cfg)
	settle := settleFor(runCfg, opts)
	run.Config, run.Deviations, run.Settle = runCfg, dev, settle
	run.lastTruth = cap.lastTruthMoment()

	// The settle has to cover the observer step's own period, and when it is that
	// floor and not the three ticks it used to be, the report says so with the
	// arithmetic. A reader who sees a harness reading on for 1,25 s past the last
	// captured input has to be able to find out whether that is a fudge or a derived
	// number, and a deviation that says "it is the step's period, here is the
	// period" is the difference between the two.
	if step := observerStepPeriod(runCfg) * simPeriod(runCfg); step > 3*simPeriod(runCfg) && step <= settle {
		run.Deviations = append(run.Deviations, Deviation{
			Field:    "settle window",
			Recorded: "the capture says nothing about how long the recording was read past the last input",
			Running:  settle.String(),
			Why: fmt.Sprintf("the observer step is a phase of interval+1 ticks, not a rate: the stride cursor "+
				"takes that many values and index 0 is served on the ticks where it is 0 "+
				"(and internal/session's TestTheWorstPhaseCostsTheWholePeriod measures "+
				"the bound). At %s of a simulation period and an interval of %d ticks that is %s, which is more "+
				"than the three ticks the tick ack alone needs, so the settle is that instead. Reading on for "+
				"less than the step's own period is what made an observer that activated at the end of a "+
				"recording arrive with no frame at all, and the report called it a difference of the server",
				simPeriod(runCfg), server.ObserverUpdateIntervalTicks(runCfg), step),
		})
	}

	prevLevel := logger.GetGlobalLevel()
	if lvl, err := logger.ParseLevel("warning"); err == nil {
		logger.SetGlobalLevel(lvl)
	}
	defer logger.SetGlobalLevel(prevLevel)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv, err := server.New(runCfg)
	if err != nil {
		return run, fmt.Errorf("start server: %w", err)
	}
	if err := srv.Start(ctx); err != nil {
		return run, fmt.Errorf("start server: %w", err)
	}
	defer func() { _ = srv.Stop() }()

	// The ports to dial are the ones the listeners bound, not the ones the
	// configuration asked for: prepareConfig asked for OS-assigned ports, and this
	// is where they are known. Reading them back is the whole point of that
	// change, and it is also what makes the reported deviation true: the report
	// names the port the replay really listened on.
	bsPort, wsPort := srv.BoundPorts()
	run.Deviations = append(run.Deviations, Deviation{
		Field:    "listen ports",
		Recorded: fmt.Sprintf("bsPort %d, webSocketPort %d", cfg.BsPort, cfg.WebSocketPort),
		Running:  fmt.Sprintf("bsPort %d, webSocketPort %d", bsPort, wsPort),
		Why: "the recorded ports belong to the run that made the capture and are not this process's to bind, " +
			"so the OS assigns both at bind time and the harness reads back what the listeners got. Asking at " +
			"bind time is also the only way to get a port without a window in which it belongs to nobody",
	})

	sched, span, speed := timeline(cap.origin(), cap.Conns, opts)
	run.Span, run.Speed = span, speed
	if speed == 1 {
		// The truth's clock has one-second resolution, so the settle has to reach
		// the end of the second the capture stops in, or the two sides are compared
		// over spans of different length and the shorter one is reported as a
		// divergence. See settleForTruth.
		if wider, need, capped := settleForTruth(settle, span, run.lastTruth, cap.origin()); wider != settle {
			// The extra has to be read before settle is overwritten, or the reason for
			// the cap would quote a difference of 0s.
			extra := wider - settle
			settle = wider
			run.Settle = wider
			why := "the original's stamps are whole seconds, so the last message of the recording happened somewhere " +
				"inside its last second. The settle now reaches the end of that second, so both sides are compared over " +
				"the same span of the match instead of ours over a shorter one"
			if capped {
				why += fmt.Sprintf(". CAPPED: the settle grew by %s instead of the %s it would have taken to cover that second whole, "+
					"because reading past the last input for that long does not fit the test budget, so the last second is "+
					"only partly read and the tail counts of it are not comparable", extra, need)
			}
			run.Deviations = append(run.Deviations, Deviation{
				Field:    "settle window",
				Recorded: fmt.Sprintf("the capture's last message is stamped %s, which is somewhere inside that whole second", run.lastTruth.Format(time.RFC3339)),
				Running:  wider.String(),
				Why:      why,
			})
		}
	}
	if speed > opts.ReplaySpeed {
		run.Deviations = append(run.Deviations, Deviation{
			Field:    "replay speed",
			Recorded: fmt.Sprintf("the capture spans %s", span),
			Running:  fmt.Sprintf("x%.2f", speed),
			Why: "a wall-clock ceiling was asked for, so the gaps were compressed. " +
				"THE COUNTS BELOW DO NOT MEAN ANYTHING: the server emits 0x0A per tick, not per " +
				"input, so running the input faster leaves it less wall-clock time to produce what " +
				"it owes. Re-run without the ceiling to compare counts",
		})
	}
	end := time.Duration(float64(span)/speed) + settle
	run.InputEnd = end - settle

	start := time.Now()
	run.Started = start
	results := make([]*Conn, len(cap.Conns))
	var wg sync.WaitGroup
	var failedMux sync.Mutex
	var failed []error
	// A capture taken with -sslCert/-sslKey made the ports TLS listeners, so the
	// replay has to speak TLS or the upgrade is refused with a bare 400 and the
	// server answers nothing at all. Found on the first real capture: the original
	// sent 547 messages and we sent 0, because the harness dialled plaintext at a
	// wss port. runCfg already has the pair blanked when the files are not on this
	// machine, so a non-empty value here means the server really is listening TLS.
	//
	// It is a property of the PORT and not of the run: the certificate pair goes to
	// the WebSocket listener (internal/server/server.go:199) and the bs listener is
	// built without it (:195), so the bsPort is plaintext even with the pair in
	// force. Deciding it once per replay dialled TLS into that plaintext listener,
	// and a bsPort capture came back with ours=0: measured on
	// 2026-09-30_201122_c4e7c284ff1c, 24 and 2 messages from the original against
	// none from us, on both of its connections, with nothing anywhere saying why.
	// That is the shape of failure this harness exists to refuse: a clean report
	// over an empty stream of ours, with the counts informative instead of fatal.
	//
	// The certificate is a self-signed pair belonging to whichever machine made the
	// capture, so the name is not checked. The battle frames inside TLS are the
	// same ones either way, and that is what is being compared.
	useTLS, tlsDev := replayTLS(runCfg, cap.Conns)
	run.Deviations = append(run.Deviations, tlsDev...)

	for i, conn := range cap.Conns {
		var port uint
		if conn.Transport == FramingWS {
			port = wsPort
		} else {
			port = bsPort
		}
		if port == 0 {
			conn.note("not replayed: the recorded command line has no %s, so there is no port to connect to",
				portName(conn.Transport))
			results[i] = conn
			continue
		}
		wg.Add(1)
		go func(i int, conn *Conn, port uint, overTLS bool) {
			defer wg.Done()
			got, err := replayConn(conn, port, sched[conn.ClientPort], start, end, overTLS)
			if err != nil {
				// The error leaves the replay instead of staying a note on the
				// connection: see unfinishedReplay for why, and for the measurement.
				conn.note("replay failed: %v", err)
				failedMux.Lock()
				failed = append(failed, fmt.Errorf("client port %d (%s): %w", conn.ClientPort, portName(conn.Transport), err))
				failedMux.Unlock()
			}
			results[i] = got
		}(i, conn, port, useTLS && conn.Transport == FramingWS)
	}
	wg.Wait()
	run.Conns = results
	if err := unfinishedReplay(failed); err != nil {
		return run, err
	}
	return run, nil
}

// unfinishedReplay is what a connection the harness could not drive does to the
// run. It is not a finding and it is not a note: a replay that never finished has
// no verdict to give, because "the server answered nothing" and "the harness got
// nothing" look identical in the connection table and mean opposite things.
//
// It is the other half of the shape of failure this harness exists to refuse. A
// run with an unfinished connection used to go on and report the empty side as a
// must-match divergence, which is the one conclusion a reader must not draw from
// a stream the harness never managed to collect: measured on this machine with 24
// CPU burners, 3 of the 5 failures TestCroppedCaptureStillAnswersTheSame had in
// 120 runs were the WebSocket upgrade never completing, and every one of them was
// reported as "the original answered StartObserve once and we answered none"
// followed by two more findings and a verdict about the crop. A goroutine dump
// taken at the moment of the failure has the server inside gws.Upgrader.Upgrade ->
// http.(*conn).hijackLocked -> connReader.abortPendingRead, waiting for net/http's
// background read to come back from the socket: the 101 was never written, so
// there was nothing for the harness to read in its 5 s.
func unfinishedReplay(failed []error) error {
	if len(failed) == 0 {
		return nil
	}
	return fmt.Errorf("the replay did not complete, so nothing it did or did not answer is a verdict about the server: %w",
		errors.Join(failed...))
}

// replayTLS decides whether the replay dials with TLS, and declares the deviation
// when the capture holds a connection that is not a WebSocket one.
//
// It is a property of the PORT and not of the run, and it is the decision that
// cost a whole capture before it was written down: the certificate pair goes to
// the WebSocket listener (internal/server/server.go:199) and the bs listener is
// built without it (:195), so a capture taken with -sslCert/-sslKey is plaintext
// on its bsPort. Deciding it once per replay dialled TLS into that plaintext
// listener, the server read the ClientHello as an RLink length and answered
// nothing, and 2026-09-30_201122_c4e7c284ff1c came back with ours=0 on both of its
// connections against 24 and 2 messages from the original, with a clean report and
// nothing anywhere saying why. No capture before it had gone over bsPort, so the
// supposition had been right by accident and nobody had put it to the test.
//
// The declaration is the other half of it: a server that answered nothing on a
// port looks exactly the same in the connection table as one that was sent
// something it did not understand.
func replayTLS(cfg *server.Config, conns []*Conn) (bool, []Deviation) {
	useTLS := cfg.SSLCert != "" && cfg.SSLKey != ""
	if !useTLS || !hasPlaintextTransport(conns) {
		return useTLS, nil
	}
	return useTLS, []Deviation{{
		Field:    "bsPort over TLS",
		Recorded: "the recorded command line names -sslCert/-sslKey, and the capture holds the frames the proxy had already decrypted, so it cannot say whether the game spoke TLS on the bsPort",
		Running:  "plaintext",
		Why: "this server's bs listener never takes the certificate pair (internal/server/server.go:195 builds it without one, " +
			":199 is the listener that gets it), so the replay dials its bsPort in plaintext. A server that answered nothing " +
			"there would look exactly the same in the table below",
	}}
}

// hasPlaintextTransport reports whether the capture has a connection that is not a
// WebSocket one, which is what makes the bsPort's lack of TLS worth declaring.
func hasPlaintextTransport(conns []*Conn) bool {
	for _, c := range conns {
		if c.Transport != "" && c.Transport != FramingWS {
			return true
		}
	}
	return false
}

func portName(transport string) string {
	if transport == FramingWS {
		return "webSocketPort"
	}
	return "bsPort"
}

// prepareConfig copies the recorded configuration and changes only what cannot
// be reproduced from inside a test. The listen ports belong to the run that
// produced the capture and may well be in use now; the rest is the environment of
// that run (its lobby, its certificates), which the harness has no way to
// provide, and every one of those is reported rather than done quietly.
//
// The ports are the one thing that is NOT replaced by a number the harness picks.
// This used to ask the OS for a free port, close it and let the server bind it
// later, and that lost the race: a listener bind on Windows is exclusive, so
// whatever took the port in between owns it and Start fails, and the whole replay
// dies with "start server: ... bind". Measured on this machine: 1 in 8 runs of
// `go test -short ./internal/sim` under load, on TestSim_synthetic and on anything
// else that replays. Asking for OS-assigned ports instead (Config.
// EphemeralListenPorts) means the port is never free, so there is nothing to lose;
// the replay reads the two ports back from the server with Server.BoundPorts and
// the deviation below names them. Picking a free port and holding it is not
// available either: the server binds its own.
func prepareConfig(cfg *server.Config) (*server.Config, []Deviation) {
	run := *cfg
	var dev []Deviation
	change := func(field string, old, new any, why string) {
		dev = append(dev, Deviation{
			Field:    field,
			Recorded: fmt.Sprint(old),
			Running:  fmt.Sprint(new),
			Why:      why,
		})
	}

	run.EphemeralListenPorts = server.EphemeralBoth
	run.BsPort = 0
	run.WebSocketPort = 0

	if cfg.OutOfBandPort != 0 {
		run.OutOfBandPort = 0
		change("outOfBandPort", cfg.OutOfBandPort, 0,
			"a separate TLS service that no battle message travels through; the harness replays the battle protocol only")
	}
	if (cfg.SSLCert != "" || cfg.SSLKey != "") && !(fileExists(cfg.SSLCert) && fileExists(cfg.SSLKey)) {
		run.SSLCert, run.SSLKey = "", ""
		change("sslCert/sslKey", cfg.SSLCert, "(none)",
			"the paths belong to the machine that made the capture and the files are not here. The harness speaks plaintext ws to its own port; the battle frames inside TLS are the same ones")
	}
	if cfg.LobbyPort != 0 {
		run.LobbyPort = 0
		change("lobbyPort", cfg.LobbyPort, 0,
			"there is no lobby server in a test, and a connection attempt to one would only add noise")
	}
	if cfg.MuninLog != "" {
		run.MuninLog = ""
		change("muninLog", cfg.MuninLog, "(none)", "a test does not write Munin statistics")
	}
	if run.LogLevel != "none" {
		change("logLevel", run.LogLevel, "warning",
			"the captured debug log is not part of the comparison and would bury the report. The level cannot change a single byte on the wire")
		run.LogLevel = "warning"
	}
	return &run, dev
}

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// settleFor is how long the replay keeps reading after the last input message,
// so the session ticks have delivered everything they owe an observer.
//
// There are two things owed and they have different periods. The **tick ack** is one per
// simulation tick, so three periods is enough to see it. The **observer step** is a
// phase, not a rate: the stride cursor takes `interval+1` values and the
// entry at index 0 is served on the ticks where it is 0
// (`internal/server/ticks.go`, `internal/session/session.go`
// `updateObserversLocked`), so an observer that activated after a tick waits up to
// `interval+1` ticks for its first visit. With the defaults that is 9 ticks = 1,125 ms,
// and it is the number `TestTheWorstPhaseCostsTheWholePeriod` measures.
//
// The settle was three periods, and that is exactly **one tick short** of what an
// observer activated at the end of a recording needs. It is a measured defect and not a
// hypothetical: with a 400 ms settle and the phase that lands the visit 9 ticks out, the
// replay closed the connection before the frame was written, and the harness reported a
// `replay-page-order` must-match with no frame in it -- the 1-in-60 flake of 40.4 and of
// `SESSION-JOURNAL.md` §11.1. The settle is not a tolerance on an assertion: it is how much of the
// run gets recorded, and the thing it was not covering is the thing it exists to cover.
//
// So the floor is `interval + 2` ticks: `interval + 1` for the step's period, and one
// more so that the write the visit makes happens strictly inside the settle instead of
// on its last instant, where the reader may not be scheduled yet. The floor and the
// ceiling keep a 1 ms or a 10 s period from making it useless or endless.
func settleFor(cfg *server.Config, opts Options) time.Duration {
	if opts.Settle > 0 {
		return opts.Settle
	}
	period := simPeriod(cfg)
	settle := 3 * period
	settle = max(settle, observerStepPeriod(cfg)*period)
	settle = max(settle, 400*time.Millisecond)
	return min(settle, 3*time.Second)
}

// simPeriod is the simulation period as a duration: -simulationperiod, or the original's
// 125 ms when the flag was not given (the flag's own default is 0, and 0 is not a period).
func simPeriod(cfg *server.Config) time.Duration {
	if cfg != nil && cfg.SimulationPeriod > 0 {
		return time.Duration(cfg.SimulationPeriod * float64(time.Millisecond))
	}
	return 125 * time.Millisecond
}

// observerStepPeriod is how long the observer step takes to come round once, in ticks:
// the stride plus one, because the counter wraps only when it is STRICTLY greater than
// the stride. One tick on top is the write's own tick; see
// settleFor.
func observerStepPeriod(cfg *server.Config) time.Duration {
	return time.Duration(server.ObserverUpdateIntervalTicks(cfg)+2)
}

// maxExtraSettleForTruth bounds how much longer settleForTruth will read past the
// last captured input. The point of the extra settle is to cover the rest of ONE
// whole second, which is at most a second; anything longer would mean the
// recording runs on long after the client stopped talking, and that span is the
// harness's settle, not the match. The cap keeps the suite inside its budget and
// is reported when it binds, because a truncated read is not a clean one.
const maxExtraSettleForTruth = 1500 * time.Millisecond

// settleForTruth widens the settle so the replay covers the whole second the
// capture's own clock ends in.
//
// This is not a nicety: it is the difference between comparing two spans of the
// match and comparing two spans of different length. The proxy stamps whole
// seconds (cmd/proxy/packets.go:47), so the truth's last recorded message is
// known to have happened somewhere inside its last second and nowhere later,
// while the replay's last instant is the input span plus the settle. On
// 2026-09-30_201122_c4e7c284ff1c the last input is stamped t=47s and the original
// answered with eight 0x0A all stamped t=47s, while a 400 ms settle stopped the
// replay at t=47.4s and collected three of them: the report then read "the
// original sent 8, we sent 3" and "the original sent 58 messages and we sent 53",
// which are the harness's own truncation and not a divergence of the server. The
// capture's own recorded run of OUR binary (main.log, the same commit) sent the
// same eight, which is what says the server was right all along.
//
// It is a pure function of the two clocks so a test can drive it both ways, and
// it is only applied at real-time speed: with the input compressed the two series
// are not on one axis and no amount of settling repairs that.
//
// It returns the settle to use, the settle that would have covered that second
// whole, and whether the cap bound. The middle value is reported when the cap
// binds, because "we stopped reading here" is only meaningful with what it was
// instead of, and a reason that quoted a 0s difference would say nothing.
func settleForTruth(base time.Duration, span time.Duration, truthLast, origin time.Time) (settle, need time.Duration, capped bool) {
	if truthLast.IsZero() || origin.IsZero() {
		return base, base, false
	}
	// The capture's clock is on second boundaries, so the last second it can hold
	// is the one truthLast falls in, and it ends a whole second later.
	end := truthLast.Truncate(time.Second).Add(time.Second).Sub(origin)
	need = end - span
	if need <= base {
		return base, base, false
	}
	if extra := need - base; extra > maxExtraSettleForTruth {
		return base + maxExtraSettleForTruth, need, true
	}
	return need, need, false
}

// scheduled is one captured message with the moment it is to be sent, relative
// to the start of the replay.
type scheduled struct {
	at  time.Duration
	msg *Msg
}

// timeline turns the captured timestamps into per-connection offsets from the
// capture's origin, the first message the client sent. The proxy stamps whole
// seconds (cmd/proxy/packets.go:47), so the order is exact and the gaps are only
// accurate to the second; the order is what the must-match rules read.
func timeline(origin time.Time, conns []*Conn, opts Options) (map[int][]scheduled, time.Duration, float64) {
	first, last := origin, time.Time{}
	for _, c := range conns {
		for _, m := range c.Up {
			if m.At.IsZero() {
				continue
			}
			if m.At.After(last) {
				last = m.At
			}
		}
	}
	total := last.Sub(first)
	if total < 0 {
		total = 0
	}

	speed := opts.ReplaySpeed
	// Only compress when a wall-clock ceiling was asked for. With none, a capture
	// replays in real time whatever its length: the server emits 0x0A per tick,
	// so running the input faster changes the output the comparison is about.
	if opts.MaxWallClock > 0 && total > 0 &&
		time.Duration(float64(total)/speed) > opts.MaxWallClock {
		speed = float64(total) / float64(opts.MaxWallClock)
	}

	sched := make(map[int][]scheduled, len(conns))
	for _, c := range conns {
		var lastAt time.Duration
		for _, m := range c.Up {
			at := lastAt
			if !m.At.IsZero() && !first.IsZero() {
				at = time.Duration(float64(max(m.At.Sub(first), 0)) / speed)
			}
			lastAt = at
			sched[c.ClientPort] = append(sched[c.ClientPort], scheduled{at: at, msg: m})
		}
	}
	return sched, total, speed
}

// dialReplayPort opens the connection the capture's transport needs. ws on a port
// that was recorded with certificates is wss, and dialling it in plaintext is how
// the first real capture produced 547 messages from the original and none from
// us.
func dialReplayPort(port uint, useTLS bool) (net.Conn, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	if !useTLS {
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			return nil, fmt.Errorf("dial port %d: %w", port, err)
		}
		return conn, nil
	}
	d := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := tls.DialWithDialer(d, "tcp", addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		return nil, fmt.Errorf("dial TLS port %d: %w", port, err)
	}
	return conn, nil
}

func replayConn(cap *Conn, port uint, sched []scheduled, start time.Time, end time.Duration, useTLS bool) (*Conn, error) {
	out := &Conn{ClientPort: cap.ClientPort, ServerPort: cap.ServerPort, Transport: cap.Transport, Notes: cap.Notes}
	if len(sched) == 0 {
		out.note("not replayed: the client sent nothing this harness can frame")
		return out, nil
	}

	// The connection opens at its first message's offset: the proxy logs a client's
	// upgrade in the same second as its first message, so a real client connects
	// when it is about to talk. Dialled at the replay's start, a late connection
	// would sit silent from its accept until the server's silent timeout closed it.
	if wait := start.Add(sched[0].at).Sub(time.Now()); wait > 0 {
		time.Sleep(wait)
	}
	conn, err := dialReplayPort(port, useTLS)
	if err != nil {
		return out, err
	}
	defer func() { _ = conn.Close() }()

	var (
		mu      sync.Mutex
		got     []chunk
		readErr error
		capped  bool
	)
	// Bytes the server wrote behind the upgrade response belong to the message
	// stream, not to the handshake, and they arrived when the handshake did.
	if cap.Transport == FramingWS {
		leftover, req, resp, err := wsHandshake(conn, port)
		if err != nil {
			return out, err
		}
		// The upgrade exchange is what the two sides are compared on (upgrade.go),
		// and it is not in the message stream because an HTTP header cannot start
		// a frame. It has to be kept from here, out of the one function that sees
		// it, or the comparison has nothing to read.
		out.upHandshake, out.downHandshake = req, resp
		if len(leftover) > 0 {
			got = append(got, chunk{data: leftover, at: time.Now()})
		}
	}
	done := make(chan struct{})
	// Everything the server wrote is already in the socket buffer when the
	// connection closes, and the reader may still be scheduled hundreds of
	// milliseconds later, so a read after the close is stamped with the close
	// itself. Clamping is the tightest sound bound: the server cannot have
	// written past it, and without the clamp the last messages of every replay
	// would be stamped outside the window they were produced in, which is
	// exactly the boundary the tail analysis reads.
	closedAt := start.Add(end)
	go func() {
		defer close(done)
		buf := make([]byte, 32*1024)
		for {
			n, err := conn.Read(buf)
			// The moment is taken before the copy so it is the arrival and not
			// the time the copy finished. The report compares our messages
			// against the original's on one time axis, and a message with no
			// time is exactly what hides an extra message in the settle window.
			at := time.Now()
			if at.After(closedAt) {
				at = closedAt
			}
			if n > 0 {
				mu.Lock()
				if readBytes(got)+n <= maxCollected {
					got = append(got, chunk{data: append([]byte(nil), buf[:n]...), at: at})
				} else {
					capped = true
				}
				mu.Unlock()
			}
			if err != nil {
				if !isTimeout(err) && !errors.Is(err, net.ErrClosed) {
					readErr = err
				}
				return
			}
		}
	}()

	for i, s := range sched {
		if wait := start.Add(s.at).Sub(time.Now()); wait > 0 {
			<-time.After(wait)
		}
		if err := writeMessage(conn, s.msg, cap.Transport, i); err != nil {
			out.note("client->server: stopped after %d of %d messages: %v", i, len(sched), err)
			break
		}
	}
	// Let the session ticks deliver what they owe, then close so the reader
	// unblocks. Everything the server wrote is already in the socket buffer.
	if wait := start.Add(end).Sub(time.Now()); wait > 0 {
		time.Sleep(wait)
	}
	_ = conn.Close()
	drained := true
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		drained = false
	}
	if readErr != nil {
		out.note("server->client: the connection ended with %v", readErr)
	}
	if capped {
		// Silent truncation would look like a short stream and be read as a
		// server that stopped talking, which is the one conclusion this report
		// must never reach by accident.
		out.note("server->client: the stream was cut at the %d byte ceiling, so what is compared is only the first part of it", maxCollected)
	}

	// scan, not scanBytes: the chunks carry the moment each write arrived and the
	// report needs it, which is the whole reason the reader timestamps what it
	// reads instead of appending to a buffer.
	msgs, left, err := scan(got, cap.Transport)
	if err != nil {
		out.note("server->client: %v", err)
	}
	out.Down = msgs
	if left > 0 {
		out.note("server->client: %d trailing bytes are an incomplete frame at the end of the replay, not compared", left)
	}

	// A stream the harness did not collect WHOLE is not a stream it may compare.
	// These three used to be notes on a connection that was then compared as if it
	// were complete, which is the failure unfinishedReplay was written to refuse
	// and the same one it still leaves open on this side: "the server answered
	// nothing" and "the harness stopped reading" look identical in the connection
	// table and mean opposite things. Here the difference is not even a matter of
	// degree, because the bytes are KNOWN to be missing: the reader was still
	// working when the wait ran out, or the last frame arrived in halves.
	//
	// Nothing in this repository's captures reaches any of the three (measured:
	// zero occurrences of either note across the seven capture replays of the full
	// suite), so this does not change any verdict that exists. What it changes is
	// that the day one does, the run says which of the two facts it has.
	var lost []string
	if !drained {
		lost = append(lost, "the reader did not finish within 2s of the connection closing")
	}
	if err != nil {
		lost = append(lost, fmt.Sprintf("the stream could not be framed: %v", err))
	}
	if left > 0 {
		lost = append(lost, fmt.Sprintf("%d trailing bytes are an incomplete frame at the end of the replay", left))
	}
	if len(lost) > 0 {
		return out, fmt.Errorf("the replay did not collect the whole server->client stream, so what this connection "+
			"did or did not answer is not a verdict about the server: %s", strings.Join(lost, "; "))
	}
	return out, nil
}

// readBytes is the total length of what has been read so far, so maxCollected is
// a bound on bytes and not on the number of chunks.
func readBytes(chunks []chunk) int {
	n := 0
	for _, c := range chunks {
		n += len(c.data)
	}
	return n
}

func isTimeout(err error) bool {
	var nerr net.Error
	return errors.As(err, &nerr) && nerr.Timeout()
}

// writeMessage sends one captured message on a live socket, with the framing the
// capture recorded: a length-prefixed [u16 BE][type][body] on bsPort, a WebSocket
// binary frame on webSocketPort. A WebSocket client must mask, and the payload
// recovered from the capture is already unmasked (cmd/proxy/framer.go:53), so the
// mask is applied here, not read back out of the log.
func writeMessage(w io.Writer, m *Msg, transport string, seq int) error {
	payload := m.payload()
	if transport == FramingWS {
		_, err := w.Write(wsClientFrame(payload, seq))
		return err
	}
	frame := make([]byte, 0, 2+len(payload))
	frame = append(frame, byte(len(payload)>>8), byte(len(payload)))
	frame = append(frame, payload...)
	_, err := w.Write(frame)
	return err
}

// wsClientFrame builds a masked binary WebSocket frame. The key is derived from
// the sequence number: it only has to differ between frames, and a derived one
// keeps a failing replay reproducible.
func wsClientFrame(payload []byte, seq int) []byte {
	var key [4]byte
	for i := range key {
		key[i] = byte(seq>>uint(8*i)) ^ byte(len(payload)+i*31) ^ 0x9d
	}
	frame := make([]byte, 0, 14+len(payload))
	frame = append(frame, 0x82)
	switch {
	case len(payload) < 126:
		frame = append(frame, byte(len(payload))|0x80)
	case len(payload) < 65536:
		frame = append(frame, 126|0x80, byte(len(payload)>>8), byte(len(payload)))
	default:
		frame = append(frame, 127|0x80)
		for i := 7; i >= 0; i-- {
			frame = append(frame, byte(len(payload)>>uint(8*i)))
		}
	}
	frame = append(frame, key[:]...)
	for i, b := range payload {
		frame = append(frame, b^key[i%4])
	}
	return frame
}

// wsHandshake performs the upgrade the game performs on webSocketPort, with a
// fixed key so a failing replay is byte-for-byte reproducible. It returns any
// bytes that arrived behind the response headers, which belong to the message
// stream rather than to the handshake, plus the request and the response as the
// server wrote them, which is what ruleUpgradeResponse compares.
//
// The request is returned too even though only the response is compared, because
// a reader that has the exchange can say which of the two halves is missing, and
// "the server sent no 101" and "the capture recorded no 101" are different facts.
func wsHandshake(conn net.Conn, port uint) (leftover, request, response []byte, err error) {
	key := base64.StdEncoding.EncodeToString([]byte("battleserver-sim0"))
	req := fmt.Sprintf("GET / HTTP/1.1\r\nHost: 127.0.0.1:%d\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", port, key)
	if _, err := conn.Write([]byte(req)); err != nil {
		return nil, nil, nil, fmt.Errorf("websocket upgrade request: %w", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	// The original's 101 arrives in two TCP records (86 bytes and then 70,
	// measured on 2026-09-30_010531_a553ef8fd63c) and the second one carries no
	// blank line of its own, so a single Read is not enough to see the whole
	// answer. Reading until the terminating blank line is what makes the header
	// set comparable at all.
	buf := make([]byte, 4096)
	var head []byte
	for {
		n, err := conn.Read(buf)
		if err != nil {
			if len(head) == 0 {
				return nil, []byte(req), nil, fmt.Errorf("websocket upgrade response: %w", err)
			}
			break
		}
		head = append(head, buf[:n]...)
		if bytes.Contains(head, []byte("\r\n\r\n")) {
			break
		}
	}
	end := bytes.Index(head, []byte("\r\n\r\n"))
	if end < 0 {
		return nil, []byte(req), nil, fmt.Errorf("websocket upgrade response has no end of headers: %q", firstLine(head))
	}
	if !bytes.Contains(head[:end], []byte("101")) {
		return nil, []byte(req), nil, fmt.Errorf("websocket upgrade refused: %s", firstLine(head))
	}
	cut := end + 4
	_ = conn.SetReadDeadline(time.Time{})
	return append([]byte(nil), head[cut:]...), []byte(req), append([]byte(nil), head[:cut]...), nil
}

func firstLine(b []byte) string {
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}

// freePort is gone with the port picking it served: see prepareConfig. What is
// left of the idea is in the server, which binds ":0" when
// Config.EphemeralListenPorts is set and reports what it got
// (Server.BoundPorts). The measurement that killed the pick-a-port version is in
// SESSION-JOURNAL.md 38.
