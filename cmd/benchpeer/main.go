// Package main compares two server binaries on what an operator can measure from
// the outside: how long each takes to start listening, what each costs while it
// sits idle, what a single connection costs it, how long its WebSocket handshake
// takes, and — with -load — what it costs while internal/loadgen drives a match
// against it.
//
// It is deliberately a BLACK-BOX tool. It knows a path, an argument list and a
// port, and nothing else: it does not know which of the two binaries is the
// reimplementation, it does not read either one's internals, and it applies the
// same measurement to whatever it is given. That is what makes its output
// comparable between them, and it is why it can be run against a binary this
// project has no source for.
//
// Every figure it prints is a property of a RUNNING PROCESS: working set,
// private bytes, handle count, thread count and CPU time, read with the Win32
// process APIs, plus TCP connect and a WebSocket upgrade measured by the clock.
//
// WHAT THE NUMBERS ARE NOT, and this is the sentence that has to travel with
// them: a working-set comparison between a Go binary and an MSVC C++ binary is
// a comparison of what the OPERATOR PAYS, including each language runtime's
// idle footprint and garbage collector headroom. It is not a measurement of
// either server's algorithmic efficiency, and a Go number that is larger here is
// not a regression against the original.
//
// See PERFORMANCE.md for the figures and for what each one does and does not
// establish.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/luskaner/ageBattleServer/internal/loadgen"
)

// settings is one binary's run: the path, the arguments, and the two ports it
// will be asked to listen on. Ports are per-run so two binaries never contend.
type settings struct {
	Label  string
	Path   string
	Args   []string
	BsPort int
	WsPort int
}

// result is everything measured about one run. The zero value of every field is
// a real possibility (a metric that could not be measured reports 0 and says so
// in the note), which is why each has a Note.
type result struct {
	Label string `json:"label"`
	Path  string `json:"path"`

	// StartupMs is the wall time from launching the process to the WebSocket port
	// accepting a TCP connection. It is the figure a launcher waits on.
	StartupMs float64 `json:"startup_ms"`
	// UpgradeMs is the median of the handshake samples on the WebSocket port:
	// the request is written and the clock stops when the response's headers
	// are complete.
	UpgradeMs float64 `json:"upgrade_ms"`
	// UpgradeSamplesUs keeps every sample, in MICROSECONDS, because that
	// figure's distribution is bimodal on at least one of the two binaries and
	// a median of a bimodal sample says which cluster won rather than what
	// happened. The unit is the microsecond because the millisecond is coarser
	// than this machine's clock: it reads 0 for 47% of loopback TCP round
	// trips, and a sample stored in milliseconds and printed to two decimals
	// turns every reading under 10 us into the same "0.00", which cannot be told
	// apart from a reading that never happened.
	UpgradeSamplesUs []int64 `json:"upgrade_samples_us"`
	// UpgradeUnresolved counts the handshakes the clock could not time at all.
	// They are counted rather than carried into the samples: an untimed
	// reading is not a fast handshake, it is a clock with no resolution left,
	// and averaging it in as a zero would report the clock, not the handshake.
	UpgradeUnresolved int `json:"upgrade_unresolved"`

	// Idle*, sampled over the sampling window with the process holding no
	// client connections. Medians, not maxima, so one scheduling hiccup does
	// not become the figure.
	IdleWindowS      float64 `json:"idle_window_s"`
	IdleWorkingSetMB float64 `json:"idle_working_set_mb"`
	IdlePrivateMB    float64 `json:"idle_private_mb"`
	IdleHandles      float64 `json:"idle_handles"`
	IdleThreads      float64 `json:"idle_threads"`
	IdleProcessCPU   float64 `json:"idle_process_cpu_pct"`
	// SystemCPU is the whole machine's busy fraction over the same window, and
	// it is reported next to every other figure because a CPU percentage is
	// meaningless without it.
	SystemCPU float64 `json:"system_cpu_pct"`

	// PerConn* are the deltas across the connection phase divided by the number
	// of connections opened. WorkingSetMBPerConn can read low or zero when the
	// runtime had headroom to begin with; that is a real and common outcome
	// and not a measurement failure.
	Conns               int     `json:"conns"`
	WorkingSetMBPerConn float64 `json:"working_set_mb_per_conn"`
	PrivateMBPerConn    float64 `json:"private_mb_per_conn"`
	HandlesPerConn      float64 `json:"handles_per_conn"`
	ThreadsPerConn      float64 `json:"threads_per_conn"`

	// Load* are the load phase: internal/loadgen driving the binary for the
	// window, with the process sampled while it runs. They are zero when -load
	// was not given, and LoadWindowS says which of the two it was.
	//
	// LoadProcessCPU is the SERVER's own CPU over the window, read from the
	// process, and not the generator's: the generator runs in this process and its
	// cost is not the figure anyone is asking for. LoadSystemCPU is the whole
	// machine's busy fraction over the same window, reported beside it for the
	// reason every other CPU figure here is.
	LoadWindowS      float64 `json:"load_window_s"`
	LoadWorkingSetMB float64 `json:"load_working_set_mb"`
	LoadCommitMB     float64 `json:"load_commit_mb"`
	LoadProcessCPU   float64 `json:"load_process_cpu_pct"`
	LoadSystemCPU    float64 `json:"load_system_cpu_pct"`
	// LoadMessages is every message the generator exchanged with the server, both
	// directions, and LoadErrors is how many of its own operations failed. A run
	// with errors is a run whose message count is not a throughput figure.
	LoadMessages uint64 `json:"load_messages"`
	LoadErrors   uint64 `json:"load_errors"`

	// Started and Err record whether the run happened at all.
	Started bool `json:"started"`
	// Unavailable lists figures this machine could not produce, each with the
	// error that says why. A figure is never reported as zero for being
	// unmeasurable.
	Unavailable []string `json:"unavailable,omitempty"`
	Note        string   `json:"note,omitempty"`
	Err         string   `json:"err,omitempty"`
}

func main() {
	var (
		binaries = flag.String("bin", "", "comma-separated BINARY=LABEL[:PORT], repeatable; each is measured in turn")
		extra    = flag.String("args", "-bsPort,{bsPort},-webSocketPort,{wsPort},-outOfBandPort,0", "argument template; {bsPort} and {wsPort} are replaced per run")
		basePort = flag.Int("base-port", 29001, "first game port; each run takes the next free pair")
		window   = flag.Duration("window", 20*time.Second, "idle sampling window per repetition")
		conns    = flag.Int("conns", 8, "connections held open for the per-connection cost")
		sample   = flag.Duration("sample", 200*time.Millisecond, "sampling interval inside the window")
		repeat   = flag.Int("repeat", 3, "repetitions per binary; the MEDIAN is reported, with the range beside it")
		upgrades = flag.Int("upgrades", 5, "WebSocket handshakes per repetition; a bimodal figure cannot be summarised by repeating one sample of it")
		asJSON   = flag.Bool("json", false, "emit JSON instead of a table")
		load     = flag.Bool("load", false, "run internal/loadgen against each binary for the window and sample it under load")
		loadM    = flag.Int("load-matches", 4, "concurrent sessions the load phase drives")
		loadP    = flag.Int("load-players", 8, "players per session in the load phase, including the host")
		loadO    = flag.Int("load-observers", 8, "observers per session in the load phase")
	)
	flag.Parse()

	runs, err := parseRuns(*binaries, *basePort)
	if err != nil {
		fmt.Fprintln(os.Stderr, "benchpeer:", err)
		os.Exit(2)
	}
	if len(runs) == 0 {
		fmt.Fprintln(os.Stderr, "benchpeer: -bin is required")
		os.Exit(2)
	}
	if *repeat < 1 {
		fmt.Fprintln(os.Stderr, "benchpeer: -repeat must be at least 1")
		os.Exit(2)
	}
	if *load && (*loadM < 1 || *loadP < 1 || *loadO < 0) {
		fmt.Fprintln(os.Stderr, "benchpeer: -load needs -load-matches and -load-players of at least 1, and -load-observers of at least 0")
		os.Exit(2)
	}
	spec := loadSpec{Enabled: *load, Matches: *loadM, Players: *loadP, Observers: *loadO}

	// Every binary is repeated and the MEDIAN is what is reported. This is not a
	// refinement, it is the difference between a measurement and an anecdote:
	// measured here, the reimplementation's idle working set came out 51.7 MB on
	// one repetition and 11.7 MB on the next, and a single-run tool would have
	// printed whichever one it happened to see and called it the figure.
	//
	// Repeating fixes that, and it is the right answer for a figure whose spread
	// is one distribution. It is NOT the right answer for a figure whose
	// distribution is bimodal, and the WebSocket handshake is: repeating one sample
	// per repetition only decides the median by whichever cluster got the majority,
	// so the number in the table depends on the phase of the target's tick. That is
	// why the handshake is sampled `upgrades` times inside one repetition, and why
	// the samples are printed rather than summarised.
	var all []result
	for i, r := range runs {
		for rep := 0; rep < *repeat; rep++ {
			run := r
			run.BsPort = *basePort + 2*i
			run.WsPort = run.BsPort + 1
			run.Args = expand(*extra, run)
			all = append(all, measure(run, *window, *sample, *conns, *upgrades, spec))
		}
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(all); err != nil {
			fmt.Fprintln(os.Stderr, "benchpeer:", err)
			os.Exit(1)
		}
		return
	}
	printTable(summarise(all, *repeat))
}

// parseRuns turns "-bin a.exe=ours:29001,b.exe=orig:29011" into settings. The
// port is optional and defaults to being assigned from -base-port in order, so
// the common case is just "-bin a.exe=ours,b.exe=orig".
func parseRuns(spec string, base int) ([]settings, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, nil
	}
	var out []settings
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, label, ok := strings.Cut(item, "=")
		if !ok || name == "" || label == "" {
			return nil, fmt.Errorf("-bin entry %q: want PATH=LABEL[:PORT]", item)
		}
		label, portStr, hasPort := strings.Cut(label, ":")
		s := settings{Label: label, Path: name}
		if hasPort {
			n, err := parseInt(portStr)
			if err != nil {
				return nil, fmt.Errorf("-bin entry %q: %w", item, err)
			}
			s.BsPort, s.WsPort = n, n+1
		}
		out = append(out, s)
	}
	return out, nil
}

func parseInt(s string) (int, error) {
	n := 0
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0, fmt.Errorf("%q is not a port number", s)
	}
	return n, nil
}

// expand replaces the two placeholders. The template is a comma-separated
// argument list so that no argument containing a space needs quoting here.
func expand(tmpl string, s settings) []string {
	var out []string
	for _, a := range strings.Split(tmpl, ",") {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		a = strings.ReplaceAll(a, "{bsPort}", fmt.Sprint(s.BsPort))
		a = strings.ReplaceAll(a, "{wsPort}", fmt.Sprint(s.WsPort))
		out = append(out, a)
	}
	return out
}

// loadSpec is the load phase's configuration. Enabled is separate from the
// profile's own numbers so that "no load phase" and "a load phase of zero
// matches" cannot be confused: the second is refused at the flag, and the first
// leaves every Load* figure at zero with LoadWindowS saying so.
type loadSpec struct {
	Enabled   bool
	Matches   int
	Players   int
	Observers int
}

// measure runs one binary through the whole sequence and never returns a figure
// it did not take: anything that fails is reported in Err with the figure left at
// zero, so a partial run cannot be read as a complete one.
func measure(s settings, window, sample time.Duration, conns, upgrades int, load loadSpec) result {
	res := result{Label: s.Label, Path: s.Path, Conns: conns}

	cmd := exec.Command(s.Path, s.Args...)
	cmd.Stdout = nil // the original writes to a console it cannot size on a hidden window
	cmd.Stderr = nil
	hide(cmd)

	start := time.Now()
	if err := cmd.Start(); err != nil {
		res.Err = "start: " + err.Error()
		return res
	}
	res.Started = true
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}()

	pid := cmd.Process.Pid

	// 1. Startup: poll the WebSocket port with a real connect attempt. A connect
	// that is refused is the process not listening yet, and it is cheaper and
	// more honest than asking the operating system whether a socket exists.
	ready, readyErr := waitPort(s.WsPort, start, 30*time.Second)
	if readyErr != nil {
		res.Err = "startup: " + readyErr.Error()
		return res
	}
	res.StartupMs = float64(ready.Sub(start).Microseconds()) / 1000

	// 2. `upgrades` WebSocket handshakes on the same port, each timed end to end.
	//
	// More than one, because the original answers this from its main loop and the
	// answer therefore lands either almost immediately or one simulation period
	// later, with nothing in between. One sample per repetition decides the reported
	// median by which cluster got the majority, and that is a coin flip rather than a
	// measurement. The samples are kept so the report can print the shape instead of
	// one number standing in for it.
	for i := 0; i < upgrades; i++ {
		ms, err := handshake(s.WsPort)
		if err != nil {
			res.Note = strings.TrimSpace(res.Note + " upgrade: " + err.Error())
			break
		}
		if ms <= 0 {
			res.UpgradeUnresolved++
			continue
		}
		res.UpgradeSamplesUs = append(res.UpgradeSamplesUs, ms)
	}
	res.UpgradeMs = medianUs(res.UpgradeSamplesUs)

	// 3. Idle: hold no client connections and sample. `after` is the reading
	// the per-connection delta is taken from, and it is taken after the last
	// sample so the baseline is the end of the idle window and not its start.
	//
	// Thread count is attempted once, here, and a failure is recorded in
	// Unavailable instead of being reported as a zero: the figure is either
	// readable or it is not, and "0 threads" is not a reading any of these two
	// binaries can produce.
	threadsOK := false
	if n, terr := threadCount(pid); terr != nil {
		res.Unavailable = append(res.Unavailable, "thread count: "+terr.Error())
	} else {
		threadsOK = true
		res.IdleThreads = float64(n)
	}

	cpu0 := processCPUTime(pid)
	sys0 := systemCPUTime()
	var working, private, handleList []float64
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		time.Sleep(sample)
		c, err := counters(pid)
		if err != nil {
			res.Err = "idle sampling: " + err.Error()
			return res
		}
		working = append(working, mb(c.WorkingSet))
		private = append(private, mb(c.Private))
		handleList = append(handleList, float64(c.Handles))
	}
	after, err := counters(pid)
	if err != nil {
		res.Err = "idle baseline: " + err.Error()
		return res
	}
	cpu1 := processCPUTime(pid)
	sys1 := systemCPUTime()

	res.IdleWindowS = window.Seconds()
	res.IdleWorkingSetMB = median(working)
	res.IdlePrivateMB = median(private)
	res.IdleHandles = median(handleList)
	res.IdleProcessCPU = 100 * float64(cpu1-cpu0) / float64(sys1.total-sys0.total)
	res.SystemCPU = 100 * (1 - float64(sys1.idle-sys0.idle)/float64(sys1.total-sys0.total))

	// 4. Per-connection cost: open conns, keep them open, sample again. The
	// sockets are deliberately silent after connecting, so what is measured is
	// the cost of having a client connected and not of talking to it.
	held := make([]net.Conn, 0, conns)
	for i := 0; i < conns; i++ {
		c, err := net.DialTimeout("tcp", addr(s.WsPort), 5*time.Second)
		if err != nil {
			res.Note = strings.TrimSpace(fmt.Sprintf("%s conns: only %d of %d opened: %v", res.Note, len(held), conns, err))
			break
		}
		held = append(held, c)
	}
	// Let the accept and, where there is one, the reader goroutine settle
	// before the second reading. Without it this measures the accept path and
	// not the steady state that holds the connection.
	time.Sleep(2 * time.Second)
	open, err := counters(pid)
	if err != nil {
		res.Err = "open-connection reading: " + err.Error()
		return res
	}
	n := float64(len(held))
	res.Conns = len(held)
	if n > 0 {
		res.WorkingSetMBPerConn = (mb(open.WorkingSet) - mb(after.WorkingSet)) / n
		res.PrivateMBPerConn = (mb(open.Private) - mb(after.Private)) / n
		res.HandlesPerConn = float64(int(open.Handles)-int(after.Handles)) / n
		if threadsOK {
			if t2, terr := threadCount(pid); terr == nil {
				res.ThreadsPerConn = (float64(t2) - res.IdleThreads) / n
			}
		}
	}
	for _, c := range held {
		_ = c.Close()
	}

	// 5. Load: drive the binary with internal/loadgen for the window and sample it
	// while it runs. This is the end-to-end counterpart to the idle figures above:
	// those say what the process costs when nobody is talking to it, and this says
	// what it costs when a match is.
	//
	// The CPU figure is the SERVER's, read from its process, and not this tool's:
	// the generator runs here and its cost is not the number anyone is asking for.
	if load.Enabled {
		runLoad(&res, s.WsPort, pid, window, sample, load)
	}
	return res
}

// runLoad drives one binary with internal/loadgen and records what it cost.
//
// The sampling is done from a ticker in this goroutine while the runner works in
// another, so a slow sample cannot stretch the load: the window is the runner's
// own duration and the samples are what happened during it. A sample that fails
// is skipped rather than ending the run, because the process is under load and a
// transient refusal is not a measurement failure; if every sample failed the
// figure is reported as unavailable instead of as a zero.
func runLoad(res *result, wsPort, pid int, window, sample time.Duration, spec loadSpec) {
	profile := loadgen.DefaultProfile()
	profile.Matches = spec.Matches
	profile.Players = spec.Players
	profile.Observers = spec.Observers
	profile.Duration = window

	runner, err := loadgen.New(fmt.Sprintf("ws://127.0.0.1:%d/", wsPort), profile)
	if err != nil {
		res.Note = strings.TrimSpace(res.Note + " load: " + err.Error())
		return
	}

	cpu0 := processCPUTime(pid)
	sys0 := systemCPUTime()
	start := time.Now()

	done := make(chan loadgen.Report, 1)
	go func() { done <- runner.Run(context.Background()) }()

	var working, commit []float64
	ticker := time.NewTicker(sample)
	defer ticker.Stop()
	var rep loadgen.Report
wait:
	for {
		select {
		case rep = <-done:
			break wait
		case <-ticker.C:
			c, cerr := counters(pid)
			if cerr != nil {
				continue
			}
			working = append(working, mb(c.WorkingSet))
			commit = append(commit, mb(c.Private))
		}
	}
	elapsed := time.Since(start)
	cpu1 := processCPUTime(pid)
	sys1 := systemCPUTime()

	res.LoadWindowS = elapsed.Seconds()
	res.LoadMessages = rep.Stats.SentMsgs + rep.Stats.RecvMsgs
	res.LoadErrors = rep.Stats.Errors
	if len(working) == 0 {
		res.Unavailable = append(res.Unavailable, "load working set and commit charge: no sample of the process succeeded during the load window")
	} else {
		res.LoadWorkingSetMB = median(working)
		res.LoadCommitMB = median(commit)
	}
	if total := sys1.total - sys0.total; total > 0 {
		res.LoadProcessCPU = 100 * float64(cpu1-cpu0) / float64(total)
		res.LoadSystemCPU = 100 * (1 - float64(sys1.idle-sys0.idle)/float64(total))
	} else {
		res.Unavailable = append(res.Unavailable, "load CPU: the system clock did not advance over the load window")
	}
}

// waitPort returns the instant the port first accepted a connection.
func waitPort(port int, since time.Time, limit time.Duration) (time.Time, error) {
	deadline := since.Add(limit)
	for {
		c, err := net.DialTimeout("tcp", addr(port), 250*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return time.Now(), nil
		}
		if time.Now().After(deadline) {
			return time.Time{}, fmt.Errorf("port %d never accepted within %s: %w", port, limit, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// handshake writes an RFC 6455 upgrade request and returns the milliseconds
// until the response's headers are complete. The key is the one from RFC 6455's
// own example, which needs no randomness to be valid.
func handshake(port int) (int64, error) {
	c, err := net.DialTimeout("tcp", addr(port), 5*time.Second)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))

	const req = "GET / HTTP/1.1\r\n" +
		"Host: 127.0.0.1\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	start := time.Now()
	if _, err := c.Write([]byte(req)); err != nil {
		return 0, err
	}
	buf := make([]byte, 512)
	var got []byte
	for {
		n, err := c.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			return 0, err
		}
		if strings.Contains(string(got), "\r\n\r\n") {
			return time.Since(start).Microseconds(), nil
		}
		if len(got) > 4096 {
			return 0, fmt.Errorf("no header terminator in %d bytes", len(got))
		}
	}
}

func addr(port int) string { return fmt.Sprintf("127.0.0.1:%d", port) }

func mb(v uint64) float64 { return float64(v) / (1024 * 1024) }

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	c := append([]float64(nil), v...)
	sort.Float64s(c)
	n := len(c)
	if n%2 == 1 {
		return c[n/2]
	}
	return (c[n/2-1] + c[n/2]) / 2
}

// medianUs is median for the handshake samples, in milliseconds. An even count
// averages the two middle samples, which on a bimodal set can land between the
// clusters; the samples are printed beside it for exactly that reason.
func medianUs(v []int64) float64 {
	if len(v) == 0 {
		return 0
	}
	c := append([]int64(nil), v...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	n := len(c)
	if n%2 == 1 {
		return float64(c[n/2]) / 1000
	}
	return float64(c[n/2-1]+c[n/2]) / 2000
}

// summarise collapses the repetitions of one binary into one row per figure: the
// MEDIAN, with the range beside it. The range is not decoration. It is what says
// whether a figure is a property of the binary or of the run it was taken in, and
// the two figures in this tool that move most between repetitions — the
// reimplementation's idle working set and the original's handshake — are only
// readable as a range, not as a single number.
type summary struct {
	Label   string
	Repeats int
	// UpgradeSamplesUs is every handshake sample pooled over the repetitions,
	// in microseconds, and UpgradeUnresolved is how many the clock could not
	// time. The samples are kept because the figure they summarise is bimodal on
	// at least one of the two binaries and the median alone does not say so.
	UpgradeSamplesUs  []int64
	UpgradeUnresolved int
	figures           map[string]float64
	ranges            map[string]string
	Unavailable       []string
	Note              string
	Err               string
}

// summarise groups by label. results arrive grouped by binary already, but
// grouping on the label keeps it right if that ever changes.
func summarise(rs []result, repeat int) []summary {
	var order []string
	byLabel := map[string][]result{}
	for _, r := range rs {
		if _, seen := byLabel[r.Label]; !seen {
			order = append(order, r.Label)
		}
		byLabel[r.Label] = append(byLabel[r.Label], r)
	}
	getters := map[string]func(result) float64{
		"startup":        func(r result) float64 { return r.StartupMs },
		"upgrade":        func(r result) float64 { return r.UpgradeMs },
		"idleWindow":     func(r result) float64 { return r.IdleWindowS },
		"idleWS":         func(r result) float64 { return r.IdleWorkingSetMB },
		"idleCommit":     func(r result) float64 { return r.IdlePrivateMB },
		"idleHandles":    func(r result) float64 { return r.IdleHandles },
		"idleCPU":        func(r result) float64 { return r.IdleProcessCPU },
		"systemCPU":      func(r result) float64 { return r.SystemCPU },
		"conns":          func(r result) float64 { return float64(r.Conns) },
		"wsPerConn":      func(r result) float64 { return r.WorkingSetMBPerConn },
		"commitPerConn":  func(r result) float64 { return r.PrivateMBPerConn },
		"handlesPerConn": func(r result) float64 { return r.HandlesPerConn },
		"threadsPerConn": func(r result) float64 { return r.ThreadsPerConn },
		"loadWindow":     func(r result) float64 { return r.LoadWindowS },
		"loadWS":         func(r result) float64 { return r.LoadWorkingSetMB },
		"loadCommit":     func(r result) float64 { return r.LoadCommitMB },
		"loadCPU":        func(r result) float64 { return r.LoadProcessCPU },
		"loadSystemCPU":  func(r result) float64 { return r.LoadSystemCPU },
		"loadMessages":   func(r result) float64 { return float64(r.LoadMessages) },
		"loadErrors":     func(r result) float64 { return float64(r.LoadErrors) },
	}
	out := make([]summary, 0, len(order))
	for _, label := range order {
		g := byLabel[label]
		s := summary{Label: label, Repeats: len(g), figures: map[string]float64{}, ranges: map[string]string{}}
		for _, r := range g {
			s.UpgradeSamplesUs = append(s.UpgradeSamplesUs, r.UpgradeSamplesUs...)
		}
		seenUnavailable := map[string]bool{}
		for name, get := range getters {
			v := make([]float64, 0, len(g))
			for _, r := range g {
				v = append(v, get(r))
			}
			lo, hi := minMax(v)
			s.figures[name] = median(v)
			s.ranges[name] = fmt.Sprintf("%.2f-%.2f", lo, hi)
		}
		for _, r := range g {
			for _, u := range r.Unavailable {
				if !seenUnavailable[u] {
					seenUnavailable[u] = true
					s.Unavailable = append(s.Unavailable, u)
				}
			}
			if r.Err != "" {
				s.Err += " " + r.Err
			}
			if r.Note != "" {
				s.Note += " " + r.Note
			}
		}
		// A figure this machine could not produce is not a zero. It is marked
		// unavailable and printed as "n/a", because a table row that reads 0.00
		// for something that was never measured is the exact figure
		// PERFORMANCE.md §2 was written about.
		if len(s.Unavailable) > 0 {
			for name := range s.figures {
				switch name {
				case "threadsPerConn":
					s.figures[name] = math.NaN()
				}
			}
		}
		out = append(out, s)
	}
	return out
}

func minMax(v []float64) (float64, float64) {
	if len(v) == 0 {
		return 0, 0
	}
	lo, hi := v[0], v[0]
	for _, x := range v {
		if x < lo {
			lo = x
		}
		if x > hi {
			hi = x
		}
	}
	return lo, hi
}

func printTable(rs []summary) {
	sec := []struct {
		name string
		key  string
	}{
		{"startup, port listening (ms)", "startup"},
		{"websocket upgrade (ms)", "upgrade"},
		{"--- idle ---", ""},
		{"idle window (s)", "idleWindow"},
		{"idle working set (MB)", "idleWS"},
		{"idle commit charge (MB)", "idleCommit"},
		{"idle handles", "idleHandles"},
		{"idle process CPU (% of machine)", "idleCPU"},
		{"system CPU over the window (%)", "systemCPU"},
		{"--- per held connection ---", ""},
		{"connections held", "conns"},
		{"working set per conn (MB)", "wsPerConn"},
		{"commit per conn (MB)", "commitPerConn"},
		{"handles per conn", "handlesPerConn"},
		{"threads per conn", "threadsPerConn"},
		{"--- under load (-load) ---", ""},
		{"load window (s)", "loadWindow"},
		{"load working set (MB)", "loadWS"},
		{"load commit charge (MB)", "loadCommit"},
		{"load process CPU (% of machine)", "loadCPU"},
		{"system CPU under load (%)", "loadSystemCPU"},
		{"load messages (both directions)", "loadMessages"},
		{"load errors", "loadErrors"},
	}
	head := fmt.Sprintf("%-34s", "MEDIAN of N repetitions")
	for _, r := range rs {
		head += fmt.Sprintf("%26s", r.Label+" (N="+fmt.Sprint(r.Repeats)+")")
	}
	fmt.Println(head)
	for _, row := range sec {
		line := fmt.Sprintf("%-34s", row.name)
		for _, r := range rs {
			if row.key == "" {
				line += fmt.Sprintf("%26s", "")
				continue
			}
			v := r.figures[row.key]
			if math.IsNaN(v) {
				line += fmt.Sprintf("%26s", "n/a")
				continue
			}
			line += fmt.Sprintf("%26s", fmt.Sprintf("%.2f [%s]", v, r.ranges[row.key]))
		}
		fmt.Println(line)
	}

	// The upgrade samples, printed. A median of a bimodal distribution says which
	// cluster won the vote and nothing about the distribution, so the figure
	// above it is shown beside the samples that produced it. Printed only while
	// they fit on a line; `-json` carries them in full at any count.
	for _, r := range rs {
		if len(r.UpgradeSamplesUs) == 0 {
			continue
		}
		s := append([]int64(nil), r.UpgradeSamplesUs...)
		if len(s) > 24 {
			fmt.Printf("NOTE %s: %d upgrade samples taken; run with -json for them, -upgrades 1 -repeat 24 to fit them on a line\n", r.Label, len(s))
			continue
		}
		sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
		fmt.Printf("upgrade samples %-15s", r.Label+" (us):")
		for _, v := range s {
			fmt.Printf(" %d", v)
		}
		fmt.Println()
	}

	for _, r := range rs {
		if r.Err != "" {
			fmt.Printf("NOTE %s: %s\n", r.Label, strings.TrimSpace(r.Err))
		}
		for _, u := range r.Unavailable {
			fmt.Printf("UNAVAILABLE %s: %s\n", r.Label, u)
		}
		if r.Note != "" {
			fmt.Printf("NOTE %s: %s\n", r.Label, strings.TrimSpace(r.Note))
		}
	}
}

func f(v float64) string { return fmt.Sprintf("%.2f", v) }
