// Package sim replays a capture against this server and diffs what comes back
// against what the original answered to the same bytes.
//
// One capture directory is one test, and the test encodes nothing about the
// capture: it hands the directory to Run and Run decides what to compare.
// Adding a capture is adding a directory, never editing this package.
//
// # What the truth is
//
// The truth is always the original's traffic. Which of the two logs is the
// original is decided by metadata.json's writer field, never by the file name:
// main.log and secondary.log are fixed names (cmd/proxy/capture.go:12) and the
// two binaries have been swapped from run to run, which is exactly what
// produced three wrong conclusions about the StartObserve status byte
// (SESSION-JOURNAL.md, "Capture identity"). A capture whose sides are
// not identified has no truth to compare against, and it is refused rather than
// passed, because a tri-state that collapses to "fine" hides the one fact that
// makes the capture useless.
//
// # Two verdict levels
//
// must-match is what the client depends on. If it changes, the game breaks:
//
//   - the 0x19 StartObserveResponse status byte
//   - whether a 0x1E ActivateObserverResponse arrives
//   - the replay stream the observer is served: which point of the match its
//     first 0x0A frame is, and the order of the frames. The stream is the
//     session's own 0x0A acks, because that is what the frame store holds
//     (it is fed the ack the tick just built); an earlier revision read 0x09 here,
//     which no observer is ever sent, so the rule compared nothing.
//
// Everything else is informative: recorded, printed, and never fatal. The
// original has quirks of its own (CAPTURES.md row 8: 2 of 26 LOBBYPING answered
// in _26), and a test that fails on those ends up switched off entirely, which
// costs more than the divergence it reported.
//
// # When
//
// Every finding about a message carries the moment it happened, on both sides,
// counted from the first message the client sent. That is what separates a
// difference produced during the match from one produced while the harness was
// still reading after the last input, and a total cannot: "we sent eleven more"
// reads the same either way. The [message-tail] rule is where the two are
// decided, and the decision is computed from the two streams' own timestamps so
// it cannot say the opposite of what the numbers say. See when.go for the axis,
// its two different precisions, and why nothing is located by aligning streams
// that are hundreds of identical acks long.
//
// # What is not compared
//
// Only what is ours to decide. Timestamps, socket ports and clock-derived
// fields are masked before the comparison; the masked fields are listed in the
// report so the list itself stays auditable. The ports of the replay are
// necessarily different (the capture's ports belong to the run that produced
// it), and every field the harness changes on purpose is reported as a
// deviation with its reason.
package sim

import (
	"bytes"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luskaner/ageBattleServer/internal/config"
	"github.com/luskaner/ageBattleServer/internal/protocol"
	"github.com/luskaner/ageBattleServer/internal/server"
)

// The capture layout, defined once in cmd/proxy/capture.go:12. Only the two
// logs and the metadata are read. proxy.log and COMMIT.txt are not traffic.
const (
	mainLogName      = "main.log"
	secondaryLogName = "secondary.log"
	metadataName     = "metadata.json"
)

// The writer tri-state from cmd/proxy/identity.go. "unknown" is a fact of its
// own and not a synonym for "ours".
const (
	WriterOurs     = "ours"
	WriterOriginal = "original"
	WriterUnknown  = "unknown"
)

// The two framings a capture can carry, decided per connection by its bytes.
const (
	FramingWS    = "ws"
	FramingRLink = "rlink-len"
)

// Options tune a replay. The zero value is what a generated test uses.
type Options struct {
	// ReplaySpeed divides every gap between captured messages: 1 is real time.
	// Below 1 the gaps are compressed, and it is NEVER the default.
	//
	// Compression used to be justified as safe for the must-match rules, and the
	// first real capture proved that wrong: at x2.97 the original sent 500
	// EndOfGameData and we sent 176. The reason is that the server emits 0x0A per
	// TICK, not per input, so feeding the same input faster leaves the server less
	// wall-clock time to produce the output it owes. Any capture replayed faster
	// than real time therefore measures the harness, not the server.
	ReplaySpeed float64
	// MaxWallClock bounds the whole replay. ZERO, the default, means no bound: a
	// long capture is replayed in real time however long it takes, because a long
	// game is a case that has to be verified and compressing it silently trades
	// the thing being tested for test runtime.
	MaxWallClock time.Duration
	// Settle is how long the replay keeps reading after the last input message,
	// so the session ticks have delivered everything they owe. Zero picks a
	// value from the recorded simulation period.
	Settle time.Duration
}

func (o *Options) withDefaults() {
	if o.ReplaySpeed <= 0 {
		o.ReplaySpeed = 1
	}
	// No wall-clock ceiling. A capture of a long game replays in real time, and
	// the test costs what the game cost. A negative value is meaningless and is
	// treated the same way, so there is no way to ask for "unlimited" by accident.
	if o.MaxWallClock < 0 {
		o.MaxWallClock = 0
	}
}

// captureMeta mirrors cmd/proxy.CaptureMeta (cmd/proxy/capture.go:54). It is
// declared again here because that type lives in package main, which cannot be
// imported; the JSON field names and the writer tri-state are the contract.
//
// Trim is the one field the proxy never writes: it belongs to format_version 2, a
// capture whose wall clock has been cropped (trim.go). Version 1 without it is an
// ordinary capture; version 2 without it is refused.
type captureMeta struct {
	FormatVersion int         `json:"format_version"`
	StartedAt     string      `json:"started_at"`
	Commit        string      `json:"commit"`
	Dirty         string      `json:"dirty"`
	Primary       captureSide `json:"primary"`
	Secondary     captureSide `json:"secondary"`
	PrimaryArgs   []string    `json:"primary_args"`
	SecondaryArgs []string    `json:"secondary_args"`
	Warning       string      `json:"warning,omitempty"`
	Trim          *TrimRecord `json:"trim,omitempty"`
}

type captureSide struct {
	Writer  string `json:"writer"`
	ExePath string `json:"exe_path"`
	SHA256  string `json:"sha256"`
}

// Side is one of the two binaries of a run, as recorded in metadata.json.
type Side struct {
	File   string
	Writer string
	Args   []string
	SHA256 string
	Exe    string
}

// Msg is one protocol message recovered from a capture or from a replay: the
// payload is [type][body] with the framing it arrived in, plus the offset inside
// its direction's stream and the moment it arrived.
//
// At is a different clock on each side and that is deliberate. For a captured
// message it is the instant the proxy stamped the write it was in
// (cmd/proxy/packets.go:47, whole seconds). For a message our own server sent
// during a replay it is the instant the read that carried it returned, so the
// report can say when in the match an extra message was produced. The two are
// comparable because both are put on the same axis, from the capture's origin:
// see Capture.origin and Window.
type Msg struct {
	Type    byte
	Body    []byte
	Framing string // "ws" | "rlink-len"
	Offset  int
	At      time.Time
}

func (m *Msg) String() string {
	return fmt.Sprintf("0x%02X %s len=%d %s", m.Type, protocol.MessageTypeName(m.Type), len(m.Body), hexPreview(m.Body, 24))
}

func (m *Msg) payload() []byte {
	return append([]byte{m.Type}, m.Body...)
}

// Conn is one client connection, rebuilt from one side's log. The client port
// is the only identifier the two logs share: the proxy dials both children from
// its own process, so the game sees one ephemeral port and the proxy's two
// connections carry that same port (cmd/proxy/tcp_proxy.go:188).
type Conn struct {
	ClientPort int
	ServerPort int
	Transport  string // "ws" | "rlink-len"
	// TransportNote is what the transport decision could not settle by itself, if
	// anything: it is carried next to Transport instead of being dropped, because a
	// framing that was a judgement call has to read as one in the report rather
	// than as a fact.
	TransportNote string
	Up            []*Msg // what the client sent
	Down          []*Msg // what the server answered: the truth
	Notes         []string

	// upHandshake and downHandshake are the HTTP upgrade exchange the proxy logged
	// as raw records, kept whole. They are bytes the original put on the wire and
	// nothing else here looks at them; ruleUpgradeResponse compares the two sides'
	// answers without the message machinery, because an HTTP header is not a
	// message and reading one as one is what produced a "0x65 ping" that never
	// existed.
	upHandshake   []byte
	downHandshake []byte

	// otherUp is the same client->server stream as the other side of the capture
	// logged it, used to cross-check the capture itself and never as truth.
	otherUp []*Msg
	// otherDown is the same thing for what the OTHER side answered: the run this
	// capture recorded of the other binary, on the same client and the same wall
	// clock as the truth beside it. It is never a truth and never compared as
	// one; it is the third series of the tick-cadence rule (cadence.go), which is
	// the only way to tell a difference in what the server emits from a difference
	// in when the harness made it answer.
	otherDown []*Msg
	// up and down are the raw writes, turned into Up and Down by finalise.
	up, down []chunk
}

func (c *Conn) note(format string, args ...any) {
	c.Notes = append(c.Notes, fmt.Sprintf(format, args...))
}

// Capture is a loaded capture: the metadata, the two sides, and the connections
// of the side that is the original.
type Capture struct {
	Dir       string
	Meta      captureMeta
	Reference Side
	Other     Side
	Ours      Side
	Conns     []*Conn
	Notes     []string
}

func (c *Capture) note(format string, args ...any) {
	c.Notes = append(c.Notes, fmt.Sprintf(format, args...))
}

// Args returns the command line our own side was launched with, because that is
// the configuration the harness has to reproduce: the question is whether the
// server, run the way it was run, answers what the original answered.
//
// The original's command line is reported next to it and never silently
// preferred. When the two differ the difference is a fact about the capture that
// has to be visible, because a divergence caused by a different -pageSize or
// -simulationPeriod is not a fidelity finding at all.
func (c *Capture) Args() []string {
	if c.Ours.Writer == WriterOurs {
		return c.Ours.Args
	}
	return c.Reference.Args
}

// origin is the instant the capture's clock starts: the first message the
// client sent, to any of its connections. Every elapsed time in the report is
// counted from here, and replay() schedules the very same instant as the start
// of the input, which is what lets a message of ours and a message of the
// original at the same elapsed time be compared as two answers to the same
// moment of the same match.
func (c *Capture) origin() time.Time {
	var first time.Time
	for _, conn := range c.Conns {
		for _, m := range conn.Up {
			if m.At.IsZero() {
				continue
			}
			if first.IsZero() || m.At.Before(first) {
				first = m.At
			}
		}
	}
	return first
}

// Origin is the capture's clock, for the tools that have to line its traffic up on
// a timeline of their own (cmd/trimsim decides where to crop a capture by it).
func (c *Capture) Origin() time.Time { return c.origin() }

// lastTruthMoment is the last instant the ORIGINAL's log of this run carries a
// message, in the capture's own clock: the last thing it was seen to say, in
// either direction, on any of its connections. The replay needs it because that
// clock has one-second resolution, so it says how far into a second the recording
// goes and nothing about what happened in the rest of it (see settleForTruth).
func (c *Capture) lastTruthMoment() time.Time {
	var last time.Time
	for _, conn := range c.Conns {
		for _, m := range append(append([]*Msg{}, conn.Up...), conn.Down...) {
			if m.At.After(last) {
				last = m.At
			}
		}
	}
	return last
}

// Trim is this capture's crop record, or nil for a capture that holds its whole
// run. It is the same record the report prints, so a tool and a reader see one
// thing.
func (c *Capture) Trim() *TrimRecord { return c.Meta.Trim }

// Load reads a capture directory and rebuilds its connections.
//
// Direction is decided by the ports of the side being read, taken from the
// command line that same side was launched with. A row is server->client when
// its source port is one of them. It cannot be decided from the destination
// port, because the proxy logs the client's frames against the proxy's own
// listen port (cmd/proxy/tcp_proxy.go:265) and only the child ever uses the
// port in the recorded command line.
//
// It also reads the other side's log, which is never the truth: what the client
// sent has to be identical in both or the capture itself is suspect, and what the
// other side answered is the only third series there is for telling a difference
// in the tick cadence from a difference in the moment this build was made to
// answer. See crossCheck and cadence.go.
func Load(dir string) (*Capture, error) {
	raw, err := os.ReadFile(filepath.Join(dir, metadataName))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", metadataName, err)
	}
	var meta captureMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, fmt.Errorf("parse %s: %w", metadataName, err)
	}
	if meta.FormatVersion != 0 && meta.FormatVersion != 1 && meta.FormatVersion != formatVersionTrimmed {
		return nil, fmt.Errorf("%s: format_version %d is not a version this harness understands", metadataName, meta.FormatVersion)
	}
	if meta.FormatVersion == formatVersionTrimmed && meta.Trim == nil {
		return nil, fmt.Errorf("%s: format_version %d says the capture is trimmed and carries no trim record, so what was cut off it is unknown: refused rather than read as a whole run",
			metadataName, meta.FormatVersion)
	}

	cap := &Capture{Dir: dir, Meta: meta}
	primary := Side{File: mainLogName, Writer: meta.Primary.Writer, Args: meta.PrimaryArgs, SHA256: meta.Primary.SHA256, Exe: meta.Primary.ExePath}
	secondary := Side{File: secondaryLogName, Writer: meta.Secondary.Writer, Args: meta.SecondaryArgs, SHA256: meta.Secondary.SHA256, Exe: meta.Secondary.ExePath}

	for _, s := range []Side{primary, secondary} {
		if s.Writer != WriterOriginal {
			continue
		}
		if cap.Reference.Writer == WriterOriginal {
			return nil, fmt.Errorf("both sides of %s say they are the original (%s and %s); a capture has one truth and this one is not identified", dir, primary.Writer, secondary.Writer)
		}
		cap.Reference = s
	}
	if cap.Reference.Writer != WriterOriginal {
		return nil, fmt.Errorf("no side of %s is identified as the original (primary.writer=%q secondary.writer=%q): there is no truth to compare against, so this capture cannot be used. It is refused rather than passed, because passing it would look like a clean run", dir, primary.Writer, secondary.Writer)
	}
	if cap.Reference.File == mainLogName {
		cap.Other = secondary
	} else {
		cap.Other = primary
	}
	// Ours is the side metadata.json calls ours, so the harness reproduces the
	// configuration our binary actually ran with. A capture where neither side
	// is identified as ours leaves it on the non-reference side, and says so.
	switch {
	case cap.Other.Writer == WriterOurs:
		cap.Ours = cap.Other
	case cap.Reference.Writer == WriterOurs:
		cap.Ours = cap.Reference
	default:
		cap.Ours = cap.Other
		cap.note("no side of %s is identified as ours (primary.writer=%q secondary.writer=%q): the replay used the %s command line, which metadata.json does not vouch for",
			dir, primary.Writer, secondary.Writer, cap.Ours.File)
	}
	if err := checkSidecars(dir, cap); err != nil {
		return nil, err
	}
	if meta.Warning != "" {
		cap.note("metadata.json carries a warning from the run: %s", meta.Warning)
	}

	ports := serverPortsFromArgs(cap.Reference.Args)
	if len(ports) == 0 {
		return nil, fmt.Errorf("%s records no listen port for the %s side (%v): without them the direction of every row is unknown, so the capture cannot be read", metadataName, cap.Reference.File, cap.Reference.Args)
	}

	conns, notes, err := loadLog(filepath.Join(dir, cap.Reference.File), ports)
	if err != nil {
		return nil, err
	}
	cap.Notes = append(cap.Notes, notes...)

	cap.Conns = make([]*Conn, 0, len(conns))
	for _, c := range conns {
		cap.Conns = append(cap.Conns, c)
	}
	sort.Slice(cap.Conns, func(i, j int) bool { return cap.Conns[i].ClientPort < cap.Conns[j].ClientPort })

	// The other side's log is only cross-checked (informative): the client sent
	// the same bytes to both binaries, and if they disagree the capture itself
	// is suspect. It is not the truth and never becomes it.
	//
	// It runs after cap.Conns is filled, because crossCheck walks the capture's
	// connections: it used to run before, and with the list still empty it stored
	// nothing at all, so the input-agreement finding and the tick-cadence rule
	// had no second series to read and neither had ever run.
	otherPorts := serverPortsFromArgs(cap.Other.Args)
	if len(otherPorts) > 0 {
		other, otherNotes, err := loadLog(filepath.Join(dir, cap.Other.File), otherPorts)
		if err != nil {
			cap.note("the other side (%s) could not be read, so its traffic is not cross-checked: %v", cap.Other.File, err)
		} else {
			cap.Notes = append(cap.Notes, otherNotes...)
			cap.crossCheck(other)
		}
	} else {
		cap.note("the other side (%s) records no listen port, so its traffic is not cross-checked", cap.Other.File)
	}
	return cap, nil
}

// checkSidecars cross-checks main.meta.json and secondary.meta.json against
// metadata.json. The sidecars exist so a single log can be read on its own, and
// nothing keeps the two in step. When they disagree, the capture is ambiguous
// about which side is which, and every conclusion drawn from it would be a coin
// toss: that is the exact mechanism behind the three wrong hypotheses about the
// StartObserve byte, so it is refused instead of resolved by preference.
func checkSidecars(dir string, cap *Capture) error {
	for _, s := range []struct {
		log    string
		writer string
	}{
		{mainLogName, cap.Meta.Primary.Writer},
		{secondaryLogName, cap.Meta.Secondary.Writer},
	} {
		raw, err := os.ReadFile(filepath.Join(dir, strings.TrimSuffix(s.log, filepath.Ext(s.log))+".meta.json"))
		if err != nil {
			// A capture predating the sidecars is still usable: metadata.json is
			// the authoritative record either way.
			continue
		}
		var side struct {
			File   string `json:"file"`
			Writer string `json:"writer"`
		}
		if err := json.Unmarshal(raw, &side); err != nil {
			return fmt.Errorf("%s.meta.json is not valid JSON: %w", strings.TrimSuffix(s.log, filepath.Ext(s.log)), err)
		}
		if side.Writer != s.writer {
			return fmt.Errorf("%s.meta.json says writer %q but metadata.json says %q for the same side: the capture is ambiguous about which log is the original, and nothing built on it could be trusted",
				strings.TrimSuffix(s.log, filepath.Ext(s.log)), side.Writer, s.writer)
		}
	}
	return nil
}

// diffArgs reports the flags whose values differ between two command lines. The
// listen ports always differ (the two children never share one), so they are
// compared like any other flag and the reader sees the whole difference.
func diffArgs(a, b []string) []string {
	fa, fb := argsToMap(a), argsToMap(b)
	seen := make(map[string]bool, len(fa)+len(fb))
	var out []string
	for name, va := range fa {
		seen[name] = true
		if vb, ok := fb[name]; !ok || vb != va {
			out = append(out, fmt.Sprintf("-%s: ours=%q original=%q", name, va, fb[name]))
		}
	}
	for name, vb := range fb {
		if !seen[name] {
			out = append(out, fmt.Sprintf("-%s: ours=<unset> original=%q", name, vb))
		}
	}
	sort.Strings(out)
	return out
}

func argsToMap(args []string) map[string]string {
	out := make(map[string]string, len(args))
	for i := 0; i < len(args); i++ {
		name, value, hasValue, next := splitArg(args, i)
		if name == "" {
			continue
		}
		if !hasValue {
			out[name] = "true"
		} else {
			out[name] = value
		}
		i = next
	}
	return out
}

// crossCheck compares the reference's client->server stream against the other
// side's for every connection both of them saw. The proxy duplicates every
// client write to both children (cmd/proxy/tcp_proxy.go:276), so any
// difference is a defect in the capture, not in either server.
//
// What the other side ANSWED is kept too, and for a different reason: it is the
// same run of a server on the same client and the same wall clock, which makes it
// the only third series available for telling a difference in the tick cadence
// from a difference in the moment the harness made this build answer. See
// cadence.go. It is never the truth and never becomes it.
func (c *Capture) crossCheck(other map[int]*Conn) {
	for _, conn := range c.Conns {
		o, ok := other[conn.ClientPort]
		if !ok {
			conn.note("the other side (%s) has no log of client port %d", c.Other.File, conn.ClientPort)
			continue
		}
		if conn.Transport != o.Transport {
			conn.note("the two sides disagree about the framing of client port %d: %s vs %s", conn.ClientPort, conn.Transport, o.Transport)
		}
		conn.otherUp = o.Up
		conn.otherDown = o.Down
	}
}

// serverPortsFromArgs returns the listen ports the recorded command line gives
// the binary: -bsPort, -webSocketPort and -outOfBandPort. The names are matched
// case-insensitively, as internal/config.normalizeArgs does, and a value may be
// attached with "=" or follow as the next argument.
func serverPortsFromArgs(args []string) map[int]bool {
	ports := make(map[int]bool, 3)
	for i := 0; i < len(args); i++ {
		name, value, hasValue, next := splitArg(args, i)
		switch name {
		case "bsport", "websocketport", "outofbandport":
			if !hasValue {
				continue
			}
			if n, err := strconv.Atoi(value); err == nil && n > 0 && n <= 65535 {
				ports[n] = true
			}
		}
		i = next
	}
	return ports
}

// splitArg reads the flag at args[i] and returns its lowercased name, its value
// (either after "=" or args[i+1]), whether a value was present, and the index
// of the last argument it consumed.
//
// A boolean flag takes no value, so the token after it is not consumed. Without
// that rule "-enableStats -bsPort 14001" read "-bsPort" as the value of
// -enableStats and then skipped it, and the replay never found the port it had
// to dial.
func splitArg(args []string, i int) (name, value string, hasValue bool, next int) {
	arg := args[i]
	if arg == "-" || !strings.HasPrefix(arg, "-") {
		return "", "", false, i
	}
	name = strings.ToLower(strings.TrimLeft(arg, "-"))
	if n, v, found := strings.Cut(name, "="); found {
		return n, v, true, i
	}
	if !isBoolFlagName(name) && i+1 < len(args) {
		return name, args[i+1], true, i + 1
	}
	return name, "", false, i
}

// isBoolFlagName reports whether the flag takes no value. It mirrors the boolean
// flags registered by internal/config.ParseFlags; splitArg works on recorded
// command lines, where those are the original's flags and nothing else.
func isBoolFlagName(name string) bool {
	switch name {
	case "allowtimeoutoverride", "enablestats", "help", "h", "?":
		return true
	}
	return false
}

// chunk is one raw TCP write as the proxy logged it: a slice of the stream and
// the time it was seen. A logical message can span several chunks, which is why
// the stream is concatenated before anything is framed.
type chunk struct {
	data []byte
	at   time.Time
}

// loadLog reads one capture log and rebuilds its connections. msg_type is
// deliberately ignored: it is the proxy's own guess at what a frame holds
// (cmd/proxy/packets.go:33 via internal.ParseMessage), so it is 0x82 for a
// WebSocket frame but the message's own type for a bsPort frame, and 0x47 for
// the HTTP upgrade. A reader that keyed on that column would throw away every row
// of every bsPort capture, and a capture whose rows are all discarded is a
// capture that compares nothing and says nothing: see the guard at the bottom of
// this function, which is what makes an unreadable log loud instead of empty.
//
// data_b64 is the raw bytes, and those are what gets framed.
func loadLog(path string, serverPorts map[int]bool) (map[int]*Conn, []string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	defer func() { _ = f.Close() }()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err != nil {
		return nil, nil, fmt.Errorf("%s: read header: %w", filepath.Base(path), err)
	}
	cols, err := csvColumns(header)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}

	conns := make(map[int]*Conn)
	var notes []string
	rows, bad, framed := 0, 0, 0
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s: stopped at row %d: %v", filepath.Base(path), rows+1, err))
			break
		}
		rows++
		srcPort, err1 := strconv.Atoi(strings.TrimSpace(rec[cols.srcPort]))
		dstPort, err2 := strconv.Atoi(strings.TrimSpace(rec[cols.dstPort]))
		if err1 != nil || err2 != nil {
			bad++
			continue
		}
		data, err := base64.StdEncoding.DecodeString(rec[cols.data])
		if err != nil || len(data) == 0 {
			bad++
			continue
		}
		at, err := time.Parse(time.RFC3339, strings.TrimSpace(rec[cols.ts]))
		if err != nil {
			// A row with no readable clock cannot be placed on the replay
			// timeline. Scheduling it at zero silently moves it to the start of
			// the match and makes the report claim a speed the capture does not
			// support, so the capture is refused instead.
			return nil, nil, fmt.Errorf("%s: row %d has an unparseable timestamp %q: %w", filepath.Base(path), rows, rec[cols.ts], err)
		}

		var conn *Conn
		toServer := true
		clientPort := srcPort
		if serverPorts[srcPort] {
			// Server -> client: the source is the child, so the destination is
			// the game's ephemeral port.
			clientPort, toServer = dstPort, false
		}
		conn = conns[clientPort]
		if conn == nil {
			conn = &Conn{ClientPort: clientPort}
			conns[clientPort] = conn
		}
		if toServer {
			conn.ServerPort = dstPort
			conn.up = append(conn.up, chunk{data: data, at: at})
		} else {
			conn.ServerPort = srcPort
			conn.down = append(conn.down, chunk{data: data, at: at})
		}
	}
	if bad > 0 {
		notes = append(notes, fmt.Sprintf("%s: %d of %d rows had no usable ports or payload and were skipped", filepath.Base(path), bad, rows))
	}

	for _, conn := range conns {
		finalise(conn)
		framed += len(conn.Up) + len(conn.Down)
	}
	// A log with rows in it, and not one row that decoded, is a format mismatch,
	// not an empty capture. Returning a clean empty result there would be the
	// worst possible failure: the test would pass and the report would be blank,
	// and the first sign of it would be a real capture coming out empty with
	// nobody able to say why. An actually empty log (no rows at all) is fine.
	if rows > 0 && bad >= rows {
		return nil, notes, fmt.Errorf(
			"%s has %d rows and not one of them could be read: the CSV format has changed, or data_b64 is not where this build expects it. Refusing to return an empty capture that would look like a pass",
			filepath.Base(path), rows)
	}
	// Rows that decode are not enough either. A log whose every row decodes and
	// whose bytes frame into no message at all is a reader that no longer
	// understands the log, and the two shapes this harness has to read differ in
	// exactly that: a WebSocket capture carries a frame per row and a bsPort one
	// carries the RLink message, so a reader that only frames the first shape
	// returns every other capture as an empty one. Nothing downstream can see the
	// difference: the connections come back, with a note, and the report says the
	// capture had nothing to compare. So it is refused here, by name.
	if rows > 0 && framed == 0 {
		return nil, notes, fmt.Errorf(
			"%s has %d rows and every one of them decoded, but not one message could be framed out of them in either direction of any connection: this build cannot read this capture's framing (a WebSocket capture frames per WebSocket frame, a bsPort one per [u16 BE length][type][body] message, and the transport is decided from the bytes). Refusing to return an empty capture that would look like a pass",
			filepath.Base(path), rows)
	}
	return conns, notes, nil
}

type csvCol struct{ ts, srcPort, dstPort, data int }

// csvColumns maps the header the proxy writes (cmd/proxy/packets.go:76) to
// column indexes, by name, so a column reordering does not silently turn into
// nonsense. Only three columns are actually needed.
func csvColumns(header []string) (csvCol, error) {
	idx := make(map[string]int, len(header))
	for i, h := range header {
		idx[strings.TrimSpace(h)] = i
	}
	col := csvCol{ts: -1, srcPort: -1, dstPort: -1, data: -1}
	for name, dst := range map[string]*int{
		"timestamp": &col.ts,
		"src_port":  &col.srcPort,
		"dest_port": &col.dstPort,
		"data_b64":  &col.data,
	} {
		i, ok := idx[name]
		if !ok {
			return col, fmt.Errorf("the header has no %q column, so this is not a capture log this harness can read", name)
		}
		*dst = i
	}
	return col, nil
}

// finalise turns a connection's raw chunks into messages: strip the WebSocket
// handshake if there is one, decide the framing, then frame the concatenated
// stream. Concatenating first is what makes a message split across two writes
// come out whole, which the original does for every message it sends
// (OBSERVER_CAPTURE.md, "Notas").
//
// The handshake is stripped from the message stream but KEPT on the connection,
// because it is bytes the original put on the wire and nothing else in the
// harness looks at them: without this the difference between the original's 101
// response and ours is invisible. Measured on 2026-09-30_010531_a553ef8fd63c, the
// original answers with Connection, Sec-WebSocket-Accept, Server and Upgrade,
// and ours with Upgrade, Connection and Sec-WebSocket-Accept, so one header is
// missing on our side and the order of the rest differs.
func finalise(c *Conn) {
	up, upUpgraded, upNotes, upHS := stripHandshake(c.up, false)
	c.Notes = append(c.Notes, upNotes...)
	down, downUpgraded, downNotes, downHS := stripHandshake(c.down, true)
	c.Notes = append(c.Notes, downNotes...)
	c.upHandshake, c.downHandshake = upHS, downHS

	c.Transport, c.TransportNote = detectTransport(streamBytes(up), streamBytes(down), upUpgraded || downUpgraded)
	if c.Transport == "" {
		c.note("neither direction holds a recognisable frame, so this connection is not the battle protocol (it is most likely the out-of-band port)")
		return
	}
	if c.TransportNote != "" {
		c.note("%s", c.TransportNote)
	}

	msgs, left, err := scan(up, c.Transport)
	if err != nil {
		c.note("client->server: %v", err)
	}
	c.Up = msgs
	if left > 0 {
		c.note("client->server: %d trailing bytes are an incomplete frame (the capture was cut mid-message), not compared", left)
	}
	msgs, left, err = scan(down, c.Transport)
	if err != nil {
		c.note("server->client: %v", err)
	}
	c.Down = msgs
	if left > 0 {
		c.note("server->client: %d trailing bytes are an incomplete frame (the capture was cut mid-message), not compared", left)
	}
}

// stripHandshake removes an HTTP upgrade request or response from the front of a
// stream. The proxy logs the handshake as a raw record: it cannot start a
// frame, so the framer flushes it (cmd/proxy/framer.go:60). The bytes it removed
// are returned as well, because they are the server's own answer and the
// comparison needs them.
//
// It also says whether it stripped one, because a connection that carries an HTTP
// upgrade IS a WebSocket connection: that is stronger evidence than anything the
// bytes after it can be read as.
//
// A handshake longer than one record is still one handshake: the original's 101
// response arrives split in two TCP writes (measured on
// 2026-09-30_010531_a553ef8fd63c, 86 bytes and then 70), and the harness used to
// read the tail as a separate record whose first byte it then reported as a
// message type. That is where the long-standing "our WebSocket sends a ping 0x65
// where the original sends a pong 0x6B" came from: 0x65 is the 'e' of
// "Sec-WebSocket-Accept" and 0x6B is the 'k' of the accept key, both inside the
// original's response. Neither side ever sent a control frame.
func stripHandshake(chunks []chunk, fromServer bool) ([]chunk, bool, []string, []byte) {
	joined := streamBytes(chunks)
	var prefix string
	if fromServer {
		prefix = "HTTP/1."
	} else {
		prefix = "GET "
	}
	if !bytes.HasPrefix(joined, []byte(prefix)) {
		return chunks, false, nil, nil
	}
	end := bytes.Index(joined, []byte("\r\n\r\n"))
	if end < 0 {
		return nil, true, []string{fmt.Sprintf("%s handshake with no end of headers, %d bytes dropped", sideName(fromServer), len(joined))}, nil
	}
	cut := end + 4
	// A response can arrive in several records and the last one carries no header
	// line of its own, so it has no "\r\n\r\n" of its own. Everything up to and
	// including the blank line is the handshake; anything after it in the same
	// record is message traffic and stays in the stream.
	rest := dropPrefix(chunks, cut)
	return rest, true, []string{fmt.Sprintf("%s WebSocket handshake: %d bytes of HTTP skipped", sideName(fromServer), cut)}, joined[:cut]
}

func sideName(fromServer bool) string {
	if fromServer {
		return "server->client"
	}
	return "client->server"
}

func streamBytes(chunks []chunk) []byte {
	n := 0
	for _, c := range chunks {
		n += len(c.data)
	}
	out := make([]byte, 0, n)
	for _, c := range chunks {
		out = append(out, c.data...)
	}
	return out
}

func dropPrefix(chunks []chunk, n int) []chunk {
	out := make([]chunk, 0, len(chunks))
	for _, c := range chunks {
		if n >= len(c.data) {
			n -= len(c.data)
			continue
		}
		if n > 0 {
			c.data = c.data[n:]
			n = 0
		}
		out = append(out, c)
	}
	return out
}

// atOffset returns the time of the chunk that holds the given offset, so a
// message gets the timestamp of the write it started in. The proxy stamps whole
// seconds (cmd/proxy/packets.go:47), so the order is exact and the gaps are only
// accurate to the second.
func atOffset(chunks []chunk, off int) time.Time {
	pos := 0
	for _, c := range chunks {
		if off < pos+len(c.data) {
			return c.at
		}
		pos += len(c.data)
	}
	if len(chunks) > 0 {
		return chunks[len(chunks)-1].at
	}
	return time.Time{}
}

// detectTransport decides the framing of a connection, and it decides it by the
// CONTENT of the bytes and never by the msg_type column: that column is the
// proxy's guess at what a frame holds, so it reads 0x82 on a WebSocket capture
// and the inner protocol type (0x0A, 0x13, 0x09, ...) on a bsPort one, which has
// not a single 0x82 row in the whole log. Both shapes are read here, and neither
// is asked to look like the other.
//
// Two sources of evidence, in this order:
//
//  1. An HTTP upgrade on the connection. stripHandshake removes it before this is
//     called, so its presence is a fact about the capture rather than an
//     inference: a connection that was upgraded is a WebSocket connection.
//  2. The bytes themselves, and the whole of them. Two bytes are not enough: an
//     RLink message is [u16 BE length][type][body], so a message of 33024..33279
//     or 33536..34047 bytes begins 0x81 or 0x82, which is byte for byte what a
//     WebSocket FIN+binary head looks like. Believing the first byte framed the
//     rest of a bsPort stream as WebSocket frames and turned a real capture into
//     nonsense, so each candidate framing is walked over both directions in full
//     and the one that reads without meeting a byte that cannot start a frame is
//     the one taken. It is the same walk scan() does, without building messages.
//
// A framing that walks cleanly is not enough on its own: a stream has to frame at
// least one message for the transport to mean anything. When neither candidate
// does, the connection is not the battle protocol and says so, which is what an
// out-of-band TLS connection looks like. When BOTH do, that is a fact about the
// capture rather than something to resolve by preference: the framing that reads
// more of the stream is taken and the ambiguity is reported.
func detectTransport(up, down []byte, upgraded bool) (transport, note string) {
	if upgraded {
		return FramingWS, ""
	}
	rlink := framingOf(up, down, FramingRLink)
	ws := framingOf(up, down, FramingWS)
	switch {
	case ws.readable() && !rlink.readable():
		return FramingWS, ""
	case rlink.readable() && !ws.readable():
		return FramingRLink, ""
	case !ws.readable() && !rlink.readable():
		return "", ""
	}
	chosen := ws
	if rlink.frames > ws.frames {
		chosen = rlink
	}
	note = fmt.Sprintf("both framings read this connection whole (%s %d frame(s) against %s %d, and %s is the one taken because it reads more of it): the bytes alone do not settle it, so the message types this connection produced are the evidence",
		FramingRLink, rlink.frames, FramingWS, ws.frames, chosen.name)
	return chosen.name, note
}

// framing is what a byte stream looked like under one candidate framing.
type framing struct {
	name string
	// frames is how many complete frames both directions hold.
	frames int
	// broken is that some byte in the stream cannot start a frame of this
	// framing, which is what rules a candidate out however many frames it read
	// before reaching it.
	broken bool
}

// readable is whether this framing accounts for the connection: at least one
// message, and not one byte that cannot start a frame.
func (f framing) readable() bool { return f.frames > 0 && !f.broken }

func framingOf(up, down []byte, name string) framing {
	frames, broken := walkFraming(up, name)
	more, moreBroken := walkFraming(down, name)
	return framing{name: name, frames: frames + more, broken: broken || moreBroken}
}

// walkFraming counts the complete frames of one direction under one framing, and
// reports whether some byte of it cannot start a frame. It mirrors scanBytes step
// for step, including the control frames of WebSocket and the truncated tail the
// capture was cut in the middle of, because a decision taken here has to agree
// with the scan that follows it or the transport would be a second opinion about
// bytes already read once.
func walkFraming(stream []byte, transport string) (frames int, broken bool) {
	for off := 0; off < len(stream); {
		if transport == FramingWS && stream[off]&0x80 != 0 {
			switch stream[off] & 0x0F {
			case 8, 9, 10:
				// Control frames carry no battle message, and a close frame is the
				// normal way a connection ends.
				n, ok := frameLen(stream[off:], true)
				if !ok {
					return frames, false
				}
				off += n
				continue
			}
		}
		n, ok := frameLen(stream[off:], transport == FramingWS)
		if !ok {
			return frames, false // truncated tail: not a defect, and not a message either
		}
		if n <= 0 {
			return frames, true
		}
		frames++
		off += n
	}
	return frames, false
}

// scan frames a stream of timestamped writes: the chunks are concatenated first
// and only then framed, which is what makes a message the original split across
// two writes come out whole.
func scan(chunks []chunk, transport string) ([]*Msg, int, error) {
	msgs, left, err := scanBytes(streamBytes(chunks), transport)
	for _, m := range msgs {
		m.At = atOffset(chunks, m.Offset)
	}
	return msgs, left, err
}

// scanBytes frames a plain byte stream. It never resynchronises: a byte that
// cannot start a frame is a real finding, and resyncing is all a resync does. A
// truncated tail (the capture, or the replay window, ended mid-message) is
// reported as a leftover count instead, because those bytes are not a defect.
func scanBytes(stream []byte, transport string) ([]*Msg, int, error) {
	var msgs []*Msg
	off := 0
	for off < len(stream) {
		if transport == "ws" && stream[off]&0x80 != 0 {
			// Control frames carry no battle message, and a close frame is the
			// normal way a connection ends.
			switch stream[off] & 0x0F {
			case 8, 9, 10:
				n, ok := frameLen(stream[off:], true)
				if !ok {
					return msgs, len(stream) - off, nil
				}
				off += n
				continue
			}
		}
		n, ok := frameLen(stream[off:], transport == "ws")
		if !ok {
			return msgs, len(stream) - off, nil
		}
		if n <= 0 {
			return msgs, 0, fmt.Errorf("byte %d: %x cannot start a %s frame", off, stream[off:min(off+4, len(stream))], transport)
		}
		payload := stream[off : off+n]
		if transport == "ws" {
			payload = wsPayload(payload)
		} else {
			payload = payload[2:]
		}
		if len(payload) > 0 {
			msgs = append(msgs, &Msg{
				Type:    payload[0],
				Body:    append([]byte(nil), payload[1:]...),
				Framing: transport,
				Offset:  off,
			})
		}
		off += n
	}
	return msgs, 0, nil
}

// frameLen returns the total length of the frame at the head of b, and whether
// the buffer already holds all of it. It mirrors the two framings the proxy
// itself uses (cmd/proxy/framer.go:130 and :149), including the 1 MiB ceiling on
// a WebSocket payload, which is what keeps a desynchronised stream from
// swallowing the rest of the capture.
func frameLen(b []byte, ws bool) (int, bool) {
	if ws {
		return wsFrameLen(b)
	}
	if len(b) < 2 {
		return 0, false
	}
	declared := int(b[0])<<8 | int(b[1])
	if declared < 1 {
		return 0, true // impossible on this protocol: treat as a zero-length frame
	}
	total := 2 + declared
	return total, total <= len(b)
}

// maxWSPayload bounds an accepted WebSocket payload, as in cmd/proxy/framer.go:15.
const maxWSPayload = 1 << 20

func wsFrameLen(b []byte) (int, bool) {
	if len(b) < 2 {
		return 0, false
	}
	if b[0]&0x80 == 0 {
		return 0, true // not FIN: not a frame this stream produces
	}
	switch b[0] & 0x0F {
	case 0, 1, 2, 8, 9, 10:
	default:
		return 0, true
	}
	length := uint64(b[1] & 0x7F)
	idx := 2
	switch length {
	case 126:
		if len(b) < 4 {
			return 0, false
		}
		length = uint64(b[2])<<8 | uint64(b[3])
		idx = 4
	case 127:
		if len(b) < 10 {
			return 0, false
		}
		if b[2] != 0 || b[3] != 0 || b[4] != 0 || b[5] != 0 {
			return 0, true
		}
		length = uint64(b[6])<<24 | uint64(b[7])<<16 | uint64(b[8])<<8 | uint64(b[9])
		idx = 10
	}
	if length > maxWSPayload {
		return 0, true
	}
	if b[1]&0x80 != 0 { // masked
		idx += 4
	}
	total := idx + int(length)
	return total, total <= len(b)
}

// wsPayload returns the payload of a WebSocket frame. The proxy unmasks the
// client's frames in place before logging them (cmd/proxy/framer.go:53 called
// from tcp_proxy.go:265), so the masking key is still in the frame but the bytes
// behind it are already plain; unmasking them again would be wrong.
func wsPayload(frame []byte) []byte {
	masked := frame[1]&0x80 != 0
	length := int(frame[1] & 0x7F)
	idx := 2
	switch length {
	case 126:
		length = int(frame[2])<<8 | int(frame[3])
		idx = 4
	case 127:
		length = int(frame[6])<<24 | int(frame[7])<<16 | int(frame[8])<<8 | int(frame[9])
		idx = 10
	}
	if masked {
		idx += 4
	}
	if idx+length > len(frame) {
		return frame[min(idx, len(frame)):]
	}
	return frame[idx : idx+length]
}

// ConfigFromArgs rebuilds the server Config from a recorded command line with
// the real flag parser (internal/config.ParseFlags), so a test can never
// re-implement or guess a flag: -simulationPeriod, -pageSize,
// -maxPagesToStorePerSession and the rest change what the server does, and a
// test that invented them would be testing a configuration nobody ran.
//
// ParseFlags reads the process-wide flag.CommandLine and os.Args and calls
// os.Exit on a bad command line, which inside a test would take the whole test
// binary down without saying why. Three things prevent that:
//
//  1. It is called once with an empty command line first, on a throwaway
//     FlagSet. That call cannot fail (no flag set, help false)
//     and leaves the complete set of registered names behind, so the flags the
//     original knows and this parser does not are found before the real call.
//     An unknown flag is dropped and reported: our default for that knob is the
//     closest reproduction available, and saying so beats refusing the capture.
//  2. The real arguments are then parsed on a FlagSet with ContinueOnError, so a
//     malformed value is an error this function returns instead of an exit.
//  3. Only then is ParseFlags called for the result, on a FlagSet with
//     ExitOnError and arguments that are known to parse.
//
// The two os.Exit paths ParseFlags can still take are the help flags and the
// usage check on -simulationPeriod 0 together with an observer interval flag
// (config.go:120-142). Both are checked here, and both mean the recorded command
// line never produced a running server, so they are refused rather than
// reproduced.
func ConfigFromArgs(args []string) (*server.Config, []string, error) {
	known, boolFlags, err := knownFlagNames()
	if err != nil {
		return nil, nil, err
	}
	clean, dropped := dropUnknownFlags(args, known, boolFlags)
	// The help flags have to be caught in the args that are about to be parsed,
	// not in the dropped ones: -help IS a known flag, so it survives the filter
	// and ParseFlags would print the usage and exit(0), taking the whole test
	// binary with it.
	if err := checkHelpFlags(clean); err != nil {
		return nil, dropped, err
	}
	for _, d := range dropped {
		if err := checkRejectedFlag(d); err != nil {
			return nil, dropped, err
		}
	}

	var simPeriod string
	if err := withFlagSet(flag.ContinueOnError, clean, func() error {
		// Registration pass: ParseFlags with nothing to parse. It only registers
		// its flags on flag.CommandLine, and it cannot exit this way.
		saved := os.Args
		os.Args = []string{"battleserver"}
		config.ParseFlags()
		os.Args = saved
		// Validation pass: the real arguments on that same FlagSet, whose error
		// handling returns the error. ParseFlags' own parse cannot be used for
		// this: whatever the FlagSet says, it calls os.Exit(2) on a bad value,
		// which inside a test would kill the run with no diagnosis.
		if err := flag.CommandLine.Parse(clean); err != nil {
			return err
		}
		simPeriod = flagValue("simulationperiod")
		return nil
	}); err != nil {
		return nil, dropped, fmt.Errorf("the recorded command line %q does not parse: %w", strings.Join(args, " "), err)
	}
	if err := checkUsageExits(simPeriod, clean); err != nil {
		return nil, dropped, err
	}

	var cfg *server.Config
	err = withFlagSet(flag.ExitOnError, clean, func() (err error) {
		cfg = config.ParseFlags()
		return err
	})

	if err != nil {
		return nil, dropped, fmt.Errorf("parsing the recorded command line %q: %w", strings.Join(args, " "), err)
	}
	return cfg, dropped, nil
}

// flagSetMu serializes the process-global swap withFlagSet makes. The generated
// tests run in parallel (they call t.Parallel), and flag.CommandLine and os.Args
// are process-wide: two captures parsing their command lines at the same time
// would read each other's flags. Parsing is microseconds, so serializing it costs
// nothing next to the replay it precedes.
var flagSetMu sync.Mutex

// withFlagSet swaps in a fresh FlagSet and a synthetic os.Args, calls fn, and
// puts both back. ParseFlags reads them as globals, so nothing else can be done
// about it, and leaving them changed would leak into the rest of the test binary.
func withFlagSet(kind flag.ErrorHandling, args []string, fn func() error) error {
	flagSetMu.Lock()
	defer flagSetMu.Unlock()
	oldFlags := flag.CommandLine
	oldArgs := os.Args
	fs := flag.NewFlagSet("battleserver", kind)
	flag.CommandLine = fs
	os.Args = append([]string{"battleserver"}, args...)
	defer func() {
		flag.CommandLine = oldFlags
		os.Args = oldArgs
	}()
	return fn()
}

// knownFlagNames runs ParseFlags once with no arguments to discover every flag
// it registers: their names, and which of them are boolean and so take no
// value.
func knownFlagNames() (names, boolFlags map[string]bool, err error) {
	names = make(map[string]bool)
	boolFlags = make(map[string]bool)
	err = withFlagSet(flag.ContinueOnError, nil, func() error {
		config.ParseFlags()
		flag.CommandLine.VisitAll(func(f *flag.Flag) {

			names[f.Name] = true
			if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
				boolFlags[f.Name] = true
			}
		})
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("discovering the flag set: %w", err)
	}
	return names, boolFlags, nil
}

// dropUnknownFlags returns args with every flag the parser does not know (and
// its value) removed, plus one entry per dropped flag. What is kept is also
// lowercased in the flag name, exactly as internal/config.normalizeArgs does
// before handing anything to the flag package: the recorded command lines carry
// the original's own spelling (-bsPort), and the flag package is case-sensitive.
func dropUnknownFlags(args []string, known, boolFlags map[string]bool) (clean, dropped []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-" || !strings.HasPrefix(arg, "-") {
			clean = append(clean, arg)
			continue
		}
		name := strings.ToLower(strings.TrimLeft(arg, "-"))
		if n, _, found := strings.Cut(name, "="); found {
			name = n
		}
		if known[name] {
			clean = append(clean, normaliseFlagArg(arg))
			if !hasInlineValue(arg) && !boolFlags[name] && i+1 < len(args) {
				// The value keeps its case: it can be a path or a secret.
				clean = append(clean, args[i+1])
				i++
			}
			continue
		}
		// An unknown flag is dropped together with its value, which is the next
		// argument unless it is itself a flag or the value was attached with "=".
		pair := []string{arg}
		if !hasInlineValue(arg) && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			pair = append(pair, args[i+1])
			i++
		}
		dropped = append(dropped, strings.Join(pair, " "))
	}
	return clean, dropped
}

// normaliseFlagArg lowercases the name of a flag and leaves its value alone.
func normaliseFlagArg(arg string) string {
	name, value, found := strings.Cut(arg, "=")
	name = "-" + strings.ToLower(strings.TrimLeft(name, "-"))
	if found {
		return name + "=" + value
	}
	return name
}

func hasInlineValue(arg string) bool {
	return strings.Contains(arg, "=")
}

// checkRejectedFlag refuses a recorded command line that carries a flag making
// the server print its usage and exit instead of listening: the run it describes
// never happened.
func checkRejectedFlag(dropped string) error {
	name := strings.ToLower(strings.TrimLeft(dropped, "-"))
	if n, _, found := strings.Cut(name, " "); found {
		name = n
	}
	if n, _, found := strings.Cut(name, "="); found {
		name = n
	}
	switch name {
	case "help", "h", "?":
		return fmt.Errorf("the recorded command line contains -%s, which makes the server print its usage and exit instead of listening", name)
	}
	return nil
}

// checkHelpFlags refuses a recorded command line carrying -help, -h or -?:
// ParseFlags prints the usage and calls os.Exit(0) for those, which inside a test
// would end the run with a wall of help text and no diagnosis. A capture that
// recorded it describes a process that never listened.
func checkHelpFlags(args []string) error {
	for i := 0; i < len(args); i++ {
		name, _, _, next := splitArg(args, i)
		switch name {
		case "help", "h", "?":
			return fmt.Errorf("the recorded command line carries -%s, which makes the server print its usage and exit instead of listening, so that run never happened", name)
		}
		i = next
	}
	return nil
}

// flagValue is the value a flag holds after a parse, formatted for comparison.
// It reads the FlagSet instead of a Config because the value checks below have to
// run before the final ParseFlags call, and that call is the one that exits(1)
// when a value is refused.
func flagValue(name string) string {
	f := flag.CommandLine.Lookup(name)
	if f == nil {
		return ""
	}
	g, ok := f.Value.(flag.Getter)
	if !ok {
		return f.Value.String()
	}
	return fmt.Sprint(g.Get())
}

// checkUsageExits refuses the usage check inside ParseFlags, which exits(1)
// instead of starting a server. A capture whose command line trips it describes
// a process that never ran, so there is nothing to compare against.
func checkUsageExits(simPeriod string, args []string) error {
	observerFlag := false
	for i := 0; i < len(args); i++ {
		name, _, _, next := splitArg(args, i)
		if name == "observerupdateintervalms" || name == "observersocketskipms" {
			observerFlag = true
		}
		i = next
	}
	if observerFlag && simPeriod == "0" {
		return fmt.Errorf("the recorded command line sets an observer interval flag together with -simulationPeriod 0, which is refused with a usage error: the server would have exited, so this capture is not from a real run")
	}
	return nil
}

// Run is what a generated test calls. It replays the capture, prints the report
// and fails the test on any must-match divergence.
//
// In -short mode a capture whose recorded clock is over ShortModeBudget is
// skipped rather than replayed, which is the whole of the fast loop; see short.go
// for why the split is made on the recording's own clock.
func Run(t testing.TB, dir string) {
	t.Helper()
	if skipLongCapture(t, dir) {
		return
	}
	rep, err := Check(dir, Options{})
	if err != nil {
		t.Fatalf("sim: %v", err)
	}
	t.Log("\n" + rep.Render())
	if !rep.OK() {
		t.Errorf("sim: %d must-match divergence(s) against the original in %s:\n%s",
			len(rep.MustMatch), dir, rep.FirstDivergence())
	}
}

// Check is the library entry point: it loads the capture, replays it against
// this server and returns the report. It never fails a test by itself, so a
// caller can inspect the verdict.
func Check(dir string, opts Options) (*Report, error) {
	opts.withDefaults()
	cap, err := Load(dir)
	if err != nil {
		return nil, err
	}
	rep := &Report{
		Capture:         filepath.Base(cap.Dir),
		Commit:          cap.Meta.Commit,
		Dirty:           cap.Meta.Dirty,
		Reference:       cap.Reference.File,
		ReferenceWriter: cap.Reference.Writer,
		OtherSide:       fmt.Sprintf("%s writer=%s", cap.Other.File, cap.Other.Writer),
		OtherFile:       cap.Other.File,
		OtherWriter:     cap.Other.Writer,
		CommandLine:     cap.Args(),
		TruthCommand:    cap.Reference.Args,
		Masked:          maskedFields,
		Notes:           cap.Notes,
		Trim:            cap.Meta.Trim,
	}
	if rep.Commit == "" {
		rep.Commit = "unknown"
	}
	if rep.Dirty == "" {
		rep.Dirty = "unknown"
	}
	// The two sides of a capture are launched with their own command lines, and
	// only the listen ports are meant to differ. Compare the line the replay
	// actually used (cap.Args()) with the original's, not the "ours" side's line:
	// when no side is identified as ours, the replay uses the other side's line
	// and comparing a different one would report a difference nobody ran.
	rep.ConfigDiff = diffArgs(cap.Args(), cap.Reference.Args)

	cfg, dropped, err := ConfigFromArgs(cap.Args())
	if err != nil {
		return nil, err
	}
	rep.DroppedFlags = dropped

	ours, err := replay(cap, cfg, opts)
	if err != nil {
		return nil, err
	}
	rep.Running = ours.Deviations
	// The two sides of the report are put on one time axis: the original's
	// moments come from the capture's origin, ours from the instant the replay
	// scheduled that same origin to, so "the same elapsed time" means the same
	// moment of the same match on both sides.
	rep.origin = cap.origin()
	rep.window = ours.Window()
	rep.started = ours.Started
	if len(dropped) == 0 {
		rep.Running = append(rep.Running, Deviation{
			Field:    "command line",
			Recorded: strings.Join(cap.Args(), " "),
			Running:  strings.Join(cap.Args(), " "),
			Why:      "reproduced exactly: every flag the recorded command line sets is the flag the server ran with",
		})
	}

	compare(rep, cap, ours.Conns)
	return rep, nil
}

// hexPreview renders the first n bytes of b in hex, with a length marker so a
// truncated dump never looks like the whole frame.
func hexPreview(b []byte, n int) string {
	if len(b) == 0 {
		return "(empty)"
	}
	shown := b
	suffix := ""
	if len(shown) > n {
		shown = shown[:n]
		suffix = fmt.Sprintf(" ... (%d bytes)", len(b))
	}
	parts := make([]string, len(shown))
	for i, c := range shown {
		parts[i] = fmt.Sprintf("%02x", c)
	}
	return strings.Join(parts, " ") + suffix
}

// filtered returns the messages of the given type, in order.
func filtered(msgs []*Msg, typ byte) []*Msg {
	var out []*Msg
	for _, m := range msgs {
		if m.Type == typ {
			out = append(out, m)
		}
	}
	return out
}
