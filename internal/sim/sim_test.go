package sim_test

import (
	"encoding/base64"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/luskaner/ageBattleServer/internal"
	"github.com/luskaner/ageBattleServer/internal/protocol"
	"github.com/luskaner/ageBattleServer/internal/sim"
)

// The synthetic capture.
//
// No capture survives in the repository (the historical thirty were deleted,
// SESSION-JOURNAL.md "discards from the history"), and the harness still has to be
// verifiable today. So the fixture below builds one, in the exact format the
// proxy writes, and encodes the observer handshake the decompilation and the
// historical captures agree on:
//
//	player   (bsPort, length-prefixed frames):  LobbyConnect 0x15, then three
//	         replay pages 0x09 with inner type 0xFE
//	observer (webSocketPort, WebSocket frames): StartObserve 0x1C, then
//	         ActivateObserver 0x1A
//	original's answers: 0x19 status 0, 0x1E status 0, then the retained page
//	         window in order, then 0x22
//
// Three pages with -maxPagesToStorePerSession 2 is what makes the observer
// admissible: the original's frame store is always finite, of size
// ceil(peers * f) with peers floored at 8, and StartObserve refuses while the
// first retained frame is still 0. _26 answered 0 and served 7343 pages; _30
// answered 3, the client never sent ActivateObserver and reported the start of
// the match gone. The fixture is the _26 shape.
//
// What is hand-written here is only the ORIGINAL's side, and only the parts the
// three must-match rules read. The 0x0A heartbeat in it is a stand-in: the real
// server emits one ack per simulation tick and the fixture writes one, so that
// difference is reported as informative, which is the point of having two
// verdict levels.
const (
	fixtureBsPrimary    = 14001
	fixtureWsPrimary    = 14003
	fixtureBsSecondary  = 15001
	fixtureWsSecondary  = 15003
	fixturePlayerPort   = 51001
	fixtureObserverPort = 51002
)

// sideFixture is one side of a synthetic capture: who it was, how it was
// launched, and what it answered to the observer.
type sideFixture struct {
	writer   string
	args     []string
	bsPort   int
	wsPort   int
	status   byte  // the 0x19 StartObserveResponse byte
	activate bool  // whether a 0x1E came back
	frames   []int // which of the match's 0x0A acks, in which order, this side served the observer
}

func primaryFixture() sideFixture {
	return sideFixture{
		writer: sim.WriterOriginal,
		args: []string{
			"-relaybroadcastPort", "0",
			"-bsPort", strconv.Itoa(fixtureBsPrimary),
			"-outofbandPort", "14002",
			"-webSocketPort", strconv.Itoa(fixtureWsPrimary),
			"-simulationPeriod", "125",
			"-maxPagesToStorePerSession", "2",
		},
		bsPort:   fixtureBsPrimary,
		wsPort:   fixtureWsPrimary,
		status:   0,
		activate: true,
		frames:   []int{0, 1},
	}
}

func secondaryFixture() sideFixture {
	s := primaryFixture()
	s.writer = sim.WriterOurs
	s.args = []string{
		"-relaybroadcastPort", "0",
		"-bsPort", strconv.Itoa(fixtureBsSecondary),
		"-outofbandPort", "15002",
		"-webSocketPort", strconv.Itoa(fixtureWsSecondary),
		"-simulationPeriod", "125",
		"-maxPagesToStorePerSession", "2",
	}
	s.bsPort = fixtureBsSecondary
	s.wsPort = fixtureWsSecondary
	return s
}

// The client's LobbyConnect, keyed on (titleID 0x1234, sessionID 0x2a) and
// built by the protocol's own serialiser so the fixture cannot drift from it.
var fixtureSessionKey = uint64(0x1234)<<48 | 0x2a

func fixtureLobbyConnect() []byte {
	return protocol.BuildLobbyConnect(0x1234, 0x2a, 0x0123456789abcdef, 0, 0, 5, 0, 0)
}

// fixtureUploads are the three replay pages the host uploads as 0x09 GameData
// (inner type 0xFE, the replay stream).
func fixtureUploads() [][]byte {
	var out [][]byte
	for _, p := range [][]byte{{0xAA, 0x01}, {0xBB, 0x02, 0x03}, {0xCC, 0x04}} {
		out = append(out, (&protocol.GameDataMessage{InnerType: 0xFE, Payload: p}).Serialize())
	}
	return out
}

// fixtureAcks are the 0x0A acks the session built while the host uploaded those
// three pages, and they are what the observer is served.
//
// This is the shape capture 2026-09-29_012528_03cb67c7cf19 measured on the
// original: the observer receives the session's own 0x0A stream, in order, from
// the first ack of the match, and not one 0x09. The frame store is fed by the
// session's own tick, so what an observer is served is what the player was sent,
// not what the player uploaded.
//
// The bodies are written out here rather than taken from the code under test: a
// fixture built from the implementation cannot fail when the implementation is
// wrong, which is the one thing a fixture is for. The format is the one
// AckState documents and replaycontent.go decodes: [0xfe][sub count] + subs +
// [pending count][pending slots] + [u16 len] block, with a sub being
// [slot][u16 LE len][payload]. The length is the PAYLOAD's: handleGameData
// queues body[1:], which is already the upload's own [u16 LE len][payload] chunk,
// so the client's length prefix is the sub's. So the sub for the upload AA 01 is
// `01 02 00 aa 01`, and the ack that flushes it alone ends `00 00 00`: no pending
// peer, and a block of length zero.
//
// An earlier revision of these three bodies wrote the sub length big-endian and
// left out the pending-count byte, which no ack has ever been shaped like. It did
// not matter while the only rule that read them compared whole bodies against
// each other; decodeAck refuses them, and a decoder that cannot read the fixture
// cannot be exercised by it.
func fixtureAcks() [][]byte {
	return [][]byte{
		{0xfe, 0x01, 0x01, 0x02, 0x00, 0xAA, 0x01, 0x00, 0x00, 0x00},
		{0xfe, 0x01, 0x01, 0x03, 0x00, 0xBB, 0x02, 0x03, 0x00, 0x00, 0x00},
		{0xfe, 0x00, 0x01, 0x01, 0x00, 0x00},
	}
}

// fixtureObserveRequest is the 24-byte Vx StartObserveRequest: the packed
// (titleID, sessionID) key, then the two u64 the client's request carries.
func fixtureObserveRequest() []byte {
	body := make([]byte, 24)
	for i := range 8 {
		body[i] = byte(fixtureSessionKey >> uint(8*i))
		body[8+i] = byte(0xF0 >> i)
		body[16+i] = byte(0x5A + i)
	}
	return body
}

// wsLoggedFrame builds a WebSocket frame exactly the way the proxy logs one. A
// client frame keeps its masking key but its payload was unmasked in place
// before the row was written (cmd/proxy/framer.go:53 called from
// tcp_proxy.go:265); a server frame carries no mask at all.
func wsLoggedFrame(payload []byte, fromClient bool) []byte {
	var frame []byte
	if fromClient {
		frame = append(frame, 0x82, byte(len(payload))|0x80, 0x12, 0x34, 0x56, 0x78)
	} else {
		frame = append(frame, 0x82, byte(len(payload)))
	}
	return append(frame, payload...)
}

func rlinkFrame(msgType byte, body []byte) []byte {
	n := len(body) + 1
	return append([]byte{byte(n >> 8), byte(n), msgType}, body...)
}

var (
	fixtureUpgradeRequest = []byte("GET / HTTP/1.1\r\nHost: 127.0.0.1:13003\r\nUpgrade: websocket\r\n" +
		"Connection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n")
	// The fixture's "original" answers with the header set the original really
	// sends, in the order the real captures record. It used to answer with three
	// headers and the order gws writes, because the rule that reads this did not
	// exist: the WebSocket upgrade response was thrown away, so a fixture could
	// hold any answer at all and nothing would notice. ruleUpgradeResponse caught
	// it on the first run and named the missing header, which is what a fixture is
	// for. The value is RFC 6455's derivation of the key above ("the sample
	// nonce"), and TestFixtureUpgradeIsRFCDerived in upgrade_test.go keeps it so.
	fixtureUpgradeResponse = []byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\nServer: WebSocket++/0.8.2\r\nUpgrade: websocket\r\n\r\n")
)

// row is one CSV row of a capture log.
type row struct {
	at        time.Duration
	client    int
	toServer  bool
	server    int
	data      []byte
	splitInto int // when > 1, the write is chopped into this many rows
}

// buildLog writes one side's log: the player's traffic on the bs port, the
// observer's on the WebSocket port, and what the server answered to each.
func buildLog(t *testing.T, path string, s sideFixture, uploads [][]byte, observe []byte) {
	t.Helper()
	var rows []row
	acks := fixtureAcks()

	// Player: LobbyConnect, then three 0x09 uploads. All in the same second: the
	// proxy stamps whole seconds, and the observer asks a second later.
	rows = append(rows, row{client: fixturePlayerPort, toServer: true, server: s.bsPort,
		data: rlinkFrame(protocol.MsgLobbyConnect, fixtureLobbyConnect())})
	for _, p := range uploads {
		rows = append(rows, row{client: fixturePlayerPort, toServer: true, server: s.bsPort,
			data: rlinkFrame(protocol.MsgGameData, p)})
	}
	rows = append(rows,
		// The connect is echoed verbatim, which is the handshake the game needs.
		row{client: fixturePlayerPort, server: s.bsPort, at: 0,
			data: rlinkFrame(protocol.MsgLobbyConnect, fixtureLobbyConnect())},
	)
	// The acks. These are the frames the observer is served out of the store, so
	// they are written for the player first and the observer below takes a prefix
	// of them.
	for _, a := range acks {
		rows = append(rows, row{client: fixturePlayerPort, server: s.bsPort,
			data: rlinkFrame(protocol.MsgEndOfGameData, a)})
	}

	// Observer: the WebSocket upgrade, then the handshake, a second later.
	rows = append(rows,
		row{client: fixtureObserverPort, toServer: true, server: s.wsPort, at: 1 * time.Second, data: fixtureUpgradeRequest, splitInto: 2},
		row{client: fixtureObserverPort, server: s.wsPort, at: 1 * time.Second, data: fixtureUpgradeResponse, splitInto: 3},
		row{client: fixtureObserverPort, toServer: true, server: s.wsPort, at: 1 * time.Second,
			data: wsLoggedFrame(append([]byte{0x1C}, observe...), true)},
		row{client: fixtureObserverPort, toServer: true, server: s.wsPort, at: 1 * time.Second,
			data: wsLoggedFrame([]byte{protocol.MsgActivateObserver}, true)},
		row{client: fixtureObserverPort, server: s.wsPort, at: 1 * time.Second,
			data: wsLoggedFrame([]byte{protocol.MsgStartObserveResp, s.status}, false)},
	)
	if s.activate {
		// [status][estimated start time i64 LE]. The time is masked before any
		// comparison: it is the moment this process started, not the protocol.
		activate := []byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}
		rows = append(rows, row{client: fixtureObserverPort, server: s.wsPort, at: 1 * time.Second,
			data: wsLoggedFrame(append([]byte{protocol.MsgActivateObserverResp}, activate...), false)})
	}
	// The replay stream: 0x0A, the session's own acks, in the order this side
	// served them. No 0x09 and no type-34 marker, because that is what an observer
	// is sent while a player is in the session.
	for _, i := range s.frames {
		rows = append(rows, row{client: fixtureObserverPort, server: s.wsPort, at: 1 * time.Second,
			data: wsLoggedFrame(append([]byte{protocol.MsgEndOfGameData}, acks[i]...), false)})
	}

	writeLog(t, path, rows)
}

func writeLog(t *testing.T, path string, rows []row) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	w := csv.NewWriter(f)
	if err := w.Write([]string{
		"timestamp", "src_host", "src_port", "dest_host", "dest_port",
		"msg_type", "type_name", "body_len", "body_hex", "data_b64",
	}); err != nil {
		t.Fatalf("write header: %v", err)
	}
	base := time.Date(2026, 9, 28, 4, 2, 13, 0, time.UTC)
	for i, r := range rows {
		chunks := [][]byte{r.data}
		if r.splitInto > 1 {
			chunks = nil
			for off := 0; off < len(r.data); {
				n := (len(r.data) - off + r.splitInto - 1) / r.splitInto
				chunks = append(chunks, r.data[off:off+n])
				off += n
			}
		}
		for _, c := range chunks {
			src, dst := r.client, r.server
			if !r.toServer {
				src, dst = r.server, r.client
			}
			// The row is written exactly as the proxy writes it, msg_type
			// included: that column is the proxy's own guess at the frame's
			// content (cmd/proxy/packets.go:33), and the harness must not depend
			// on it.
			msgType, body, _ := internal.ParseMessage(c)
			rowOut := []string{
				base.Add(r.at).Format(time.RFC3339),
				"127.0.0.1", strconv.Itoa(src), "127.0.0.1", strconv.Itoa(dst),
				fmt.Sprintf("0x%02X", msgType),
				protocol.MessageTypeName(msgType),
				strconv.Itoa(len(body)),
				hex.EncodeToString(body),
				base64.StdEncoding.EncodeToString(c),
			}
			if err := w.Write(rowOut); err != nil {
				t.Fatalf("write row %d: %v", i, err)
			}
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		t.Fatalf("flush %s: %v", path, err)
	}
}

// writeCapture writes a whole capture directory: both logs and the metadata,
// with the writers exactly as given. Which of the two logs is the original is
// what the writers say, and the file names say nothing.
func writeCapture(t *testing.T, dir string, primary, secondary sideFixture) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	uploads := fixtureUploads()
	observe := fixtureObserveRequest()
	buildLog(t, filepath.Join(dir, "main.log"), primary, uploads, observe)
	buildLog(t, filepath.Join(dir, "secondary.log"), secondary, uploads, observe)
	writeMeta(t, dir, primary, secondary)
	return dir
}

type metaSide struct {
	Writer  string `json:"writer"`
	ExePath string `json:"exe_path"`
	SHA256  string `json:"sha256"`
}

func writeMeta(t *testing.T, dir string, primary, secondary sideFixture) {
	t.Helper()
	meta := struct {
		FormatVersion int      `json:"format_version"`
		StartedAt     string   `json:"started_at"`
		Commit        string   `json:"commit"`
		Dirty         string   `json:"dirty"`
		Primary       metaSide `json:"primary"`
		Secondary     metaSide `json:"secondary"`
		PrimaryArgs   []string `json:"primary_args"`
		SecondaryArgs []string `json:"secondary_args"`
	}{
		FormatVersion: 1,
		StartedAt:     "2026-09-28T04:02:13Z",
		Commit:        "0000000",
		Dirty:         "false",
		Primary:       metaSide{Writer: primary.writer, ExePath: "BattleServerOrig.exe", SHA256: "aa"},
		Secondary:     metaSide{Writer: secondary.writer, ExePath: "BattleServerReimpl.exe", SHA256: "bb"},
		PrimaryArgs:   primary.args,
		SecondaryArgs: secondary.args,
	}
	body, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), append(body, '\n'), 0o644); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}
}

func goodCapture(t *testing.T) string {
	t.Helper()
	return writeCapture(t, filepath.Join(t.TempDir(), "capture"), primaryFixture(), secondaryFixture())
}

// mustMatchRules returns the rules that fired, in order.
func mustMatchRules(rep *sim.Report) []string {
	out := make([]string, 0, len(rep.MustMatch))
	for _, f := range rep.MustMatch {
		out = append(out, f.Rule)
	}
	return out
}

// TestReportSaysWhenEveryDivergenceHappened is the end-to-end property of the
// timing report: a capture has to come out saying WHEN, on both sides, and the
// numbers it prints have to add up. The arithmetic is checked by parsing the
// report, because a report whose own lines disagree is worse than no report: it
// reads as two findings and it is one.
//
// What is checked per connection, and all of it is a property of the report and not
// of the wall clock:
//
//   - there IS a tail finding, and it reaches one of the two conclusions;
//   - the split it prints is exact, and it agrees with the totals it prints in the
//     same finding. That is the part that catches a judge that lies: the verdict
//     word, the two counts in the sentence and the "the split is exact" line have
//     to be three views of the same numbers.
//   - the OBSERVER connection is the settle one. That half is structural and not
//     luck: the fixture feeds its whole handshake at t=1s, which is where the
//     window opens, so no answer to it can arrive before the window.
//
// What is NOT checked here is which conclusion the PLAYER connection reaches. It used
// to be required to be "interleaved", and that assertion was a flake measured at 1 in
// 8 to 10 runs of this package: "interleaved" needs more of our messages before the
// window than the original had, our server emits one ack per simulation tick and the
// original's recording of that second has three, so the assertion needs at least five
// of our eight 125 ms ticks to land inside the first wall-clock second. That is how
// many ticks the host's scheduler managed, which is not a property of the protocol and
// not something a test may assert. Widening a tolerance would not fix it either: the
// number is not near a boundary, it is a count of ticks.
//
// The two conclusions stay covered at this level, deterministically and without a
// replay, by TestCompareConnReportsTheTimes (interleaved) and
// TestMessageTailIsNotAMustMatch (settle) in when_test.go, and at the unit level by
// TestJudgeTailSettlesOnly and TestJudgeTailInterleaved.
func TestReportSaysWhenEveryDivergenceHappened(t *testing.T) {
	dir := goodCapture(t)
	rep, err := sim.Check(dir, sim.Options{})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	out := rep.Render()

	for _, want := range []string{
		"time axis  : t is elapsed since the first message the client sent",
		"window     : the last input was fed at t=",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report must state its axis and its window, missing %q\n---\n%s", want, out)
		}
	}
	for _, c := range rep.Conns {
		if !c.OursLast.Valid {
			t.Errorf("connection %d: our stream has no recorded end, so the report cannot say when it stopped", c.ClientPort)
		}
		if !c.TheirsLast.Valid {
			t.Errorf("connection %d: the original's stream has no recorded end", c.ClientPort)
		}
	}

	exact := regexp.MustCompile(`the split is exact: (-?\d+) before the window plus (-?\d+) at or after it is the (-?\d+) extra`)
	totals := regexp.MustCompile(`our stream holds (-?\d+) message\(s\) before that moment against the original's (-?\d+)`)
	inWindow := regexp.MustCompile(`(\d+) of our (\d+) fall at or after the window opened, against (\d+) of its (\d+)`)
	interleaved := regexp.MustCompile(`(-?\d+) of the (-?\d+) message\(s\) we sent and it did not were emitted BEFORE the settle window opened`)
	settled := regexp.MustCompile(`all (-?\d+) message\(s\) we sent and it did not fall at or after the moment`)

	verdict := map[int]string{}
	found := 0
	for _, f := range rep.Informative {
		if f.Rule != sim.RuleMessageTail {
			continue
		}
		found++
		joined := strings.Join(f.Lines, "\n")
		m := exact.FindStringSubmatch(joined)
		if m == nil {
			t.Fatalf("conn %d: a tail finding must print the exact split, got: %v", f.ClientPort, f.Lines)
		}
		before, after, extra := atoi(t, m[1]), atoi(t, m[2]), atoi(t, m[3])
		if before+after != extra {
			t.Errorf("conn %d: %d before + %d at or after = %d, not the %d extra", f.ClientPort, before, after, before+after, extra)
		}

		// The same numbers, read off the other two lines the finding prints. This
		// is what "the report does not lie" means here: the count in the verdict
		// sentence, the counts in the totals line and the exact split are three
		// views of one verdict and they have to be the same view.
		tot := totals.FindStringSubmatch(joined)
		if tot == nil {
			if settled := settled.FindStringSubmatch(joined); settled == nil {
				t.Fatalf("conn %d: a tail finding must print either the interleaved sentence with its "+
					"two counts, or the settled one, got: %v", f.ClientPort, f.Lines)
			} else if got := atoi(t, settled[1]); got != extra {
				t.Errorf("conn %d: the settled sentence says %d extra and the split line says %d", f.ClientPort, got, extra)
			}
			verdict[f.ClientPort] = "settle"
			continue
		}
		oursBefore, theirsBefore := atoi(t, tot[1]), atoi(t, tot[2])
		if oursBefore-theirsBefore != before {
			t.Errorf("conn %d: the totals line says %d of ours before the window against the original's %d, "+
				"which is %d extras before it, and the split line says %d",
				f.ClientPort, oursBefore, theirsBefore, oursBefore-theirsBefore, before)
		}
		iw := inWindow.FindStringSubmatch(joined)
		if iw == nil {
			t.Fatalf("conn %d: a tail finding must print how many of each side's messages fall at or "+
				"after the window, got: %v", f.ClientPort, f.Lines)
		}
		inOurs, oursAll, inTheirs, theirsAll := atoi(t, iw[1]), atoi(t, iw[2]), atoi(t, iw[3]), atoi(t, iw[4])
		if oursAll-inOurs != oursBefore {
			t.Errorf("conn %d: the totals line says %d of ours fall before the window and the count line "+
				"says %d of %d fall after it, which is %d", f.ClientPort, oursBefore, inOurs, oursAll, oursAll-inOurs)
		}
		if theirsAll-inTheirs != theirsBefore {
			t.Errorf("conn %d: the totals line says %d of the original's fall before the window and the "+
				"count line says %d of %d fall after it", f.ClientPort, theirsBefore, inTheirs, theirsAll)
		}
		if inOurs-inTheirs != after {
			t.Errorf("conn %d: %d of ours at or after the window against %d of its is %d of ours that "+
				"fell there, and the split line says %d", f.ClientPort, inOurs, inTheirs, inOurs-inTheirs, after)
		}
		il := interleaved.FindStringSubmatch(joined)
		if il == nil {
			// No "N of the M ... were emitted BEFORE" sentence: with more of ours
			// than of the original's before the window this is the settle arm.
			if after != extra {
				t.Errorf("conn %d: the finding reached the settle conclusion but the split says %d of "+
					"%d extras are before the window", f.ClientPort, after, extra)
			}
			verdict[f.ClientPort] = "settle"
			continue
		}
		if got := atoi(t, il[1]); got != before {
			t.Errorf("conn %d: the sentence says %d extras were emitted before the window and the split "+
				"line says %d", f.ClientPort, got, before)
		}
		verdict[f.ClientPort] = "interleaved"
	}
	if found != 2 {
		t.Fatalf("both connections of this capture sent a different number of messages, so there must be 2 tail findings, got %d", found)
	}
	// The observer's whole handshake is stamped at t=1s, which is where the window
	// opens, so no answer to it can arrive before: that connection is settle-only
	// by construction and this is the half of the old assertion that did not
	// depend on how many ticks the host managed.
	if got := verdict[fixtureObserverPort]; got != "settle" {
		t.Errorf("the observer connection (port %d) reached %q, want %q: its whole handshake is fed at "+
			"t=1s and the window opens at t=1s", fixtureObserverPort, got, "settle")
	}

	// A message that is not at a moment gets a message that is at a moment, and
	// the type counts carry the window split.
	counts := 0
	for _, f := range rep.Informative {
		if f.Rule != sim.RuleMessageCount {
			continue
		}
		counts++
		joined := strings.Join(f.Lines, "\n")
		if !strings.Contains(joined, "window [t=") {
			t.Errorf("a count finding must say how the type's messages fall around the window, got: %v", f.Lines)
		}
	}
	if counts == 0 {
		t.Error("this capture's 0x0A counts differ, so there must be a count finding")
	}
	t.Log("\n" + out)
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("the report printed a number that is not one: %q", s)
	}
	return n
}

func hasRule(fs []sim.Finding, rule string) bool {
	for _, f := range fs {
		if f.Rule == rule {
			return true
		}
	}
	return false
}

func informativeRules(rep *sim.Report) []string {
	out := make([]string, 0, len(rep.Informative))
	for _, f := range rep.Informative {
		out = append(out, f.Rule)
	}
	return out
}

// TestSyntheticCaptureMatchesOriginal is the harness proving it works end to end:
// the client's own traffic goes in, and everything the client depends on comes
// back the way the original answered it.
func TestSyntheticCaptureMatchesOriginal(t *testing.T) {
	dir := goodCapture(t)
	rep, err := sim.Check(dir, sim.Options{})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	t.Log("\n" + rep.Render())
	if !rep.OK() {
		t.Fatalf("expected no must-match divergence against the original, got %d:\n%s",
			len(rep.MustMatch), rep.FirstDivergence())
	}
	if len(rep.Conns) != 2 {
		t.Fatalf("expected 2 connections in the capture, got %d", len(rep.Conns))
	}
	for _, c := range rep.Conns {
		if !c.Compared {
			t.Errorf("connection %d was not compared at all: %v", c.ClientPort, c.Notes)
		}
	}
	// The masked field has to actually mask: the fixture's ActivateObserverResponse
	// carries 0x1122334455667788 as the start time and ours carries the real
	// clock, so a body mismatch on 0x1E would mean the mask is not working.
	for _, f := range rep.Informative {
		if f.Rule == sim.RuleBodyMismatch && strings.HasPrefix(f.Ours, "0x1E") {
			t.Errorf("the masked start time was compared as a body: %s", f.Detail)
		}
	}
	// The LobbyConnect echo is byte-identical, so nothing may be reported for it.
	for _, f := range rep.Informative {
		if f.Rule == sim.RuleBodyMismatch && strings.HasPrefix(f.Ours, "0x15") {
			t.Errorf("the LobbyConnect echo should be byte-identical, but a body differed: %s", f.Detail)
		}
	}
}

// TestSyntheticCaptureDetectsInvertedStatusByte is the falsifiability of the
// first must-match rule: the same capture with the original's 0x19 byte turned
// into the 3 that broke _30 has to be reported, and reported as a must-match.
func TestSyntheticCaptureDetectsInvertedStatusByte(t *testing.T) {
	bad := primaryFixture()
	bad.status = 3
	dir := writeCapture(t, filepath.Join(t.TempDir(), "capture"), bad, secondaryFixture())

	rep, err := sim.Check(dir, sim.Options{})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !hasRule(rep.MustMatch, sim.RuleStartObserveStatus) {
		t.Fatalf("a 0x19 of 3 against the original's %d must be a must-match divergence, got %v",
			bad.status, mustMatchRules(rep))
	}
	f := rep.MustMatch[0]
	if !strings.Contains(f.Ours, "0x19") || !strings.Contains(f.Theirs, "0x19") {
		t.Errorf("the finding must carry both frames, got ours=%q theirs=%q", f.Ours, f.Theirs)
	}
	t.Log("\n" + rep.FirstDivergence())
}

func TestSyntheticCaptureDetectsUnservedObserver(t *testing.T) {
	// The client's StartObserveRequest names a session nobody created, so the
	// server has nothing to answer: neither 0x19 nor 0x1E comes back, while the
	// capture says the original served both.
	dir := filepath.Join(t.TempDir(), "capture")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	stray := make([]byte, 24)
	for i := range 8 {
		stray[i] = byte((fixtureSessionKey + 0x99) >> uint(8*i))
	}
	uploads := fixtureUploads()
	// main.log is the truth, and its client sent the request for a session that
	// does not exist. The original still answered: that is the _30 shape, with
	// the direction reversed.
	buildLog(t, filepath.Join(dir, "main.log"), primaryFixture(), uploads, stray)
	buildLog(t, filepath.Join(dir, "secondary.log"), secondaryFixture(), uploads, fixtureObserveRequest())
	writeMeta(t, dir, primaryFixture(), secondaryFixture())

	rep, err := sim.Check(dir, sim.Options{})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !hasRule(rep.MustMatch, sim.RuleStartObserveStatus) {
		t.Errorf("the original answered 0x19 and we did not: expected %s, got %v",
			sim.RuleStartObserveStatus, mustMatchRules(rep))
	}
	if !hasRule(rep.MustMatch, sim.RuleActivateObserverResponse) {
		t.Errorf("the original answered 0x1E and we did not: expected %s, got %v",
			sim.RuleActivateObserverResponse, mustMatchRules(rep))
	}
	t.Log("\n" + rep.FirstDivergence())
}

// TestSyntheticCaptureDetectsReorderedPages covers the third rule: the order of
// the pages the observer is served. A reversed pair crashed the client in
// capture _5, which is why the order is a must-match and not the count.
func TestSyntheticCaptureDetectsReorderedPages(t *testing.T) {
	bad := primaryFixture()
	bad.frames = []int{1, 0}
	dir := writeCapture(t, filepath.Join(t.TempDir(), "capture"), bad, secondaryFixture())

	rep, err := sim.Check(dir, sim.Options{})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !hasRule(rep.MustMatch, sim.RuleReplayPageOrder) {
		t.Fatalf("serving the pages in the reverse order must be a must-match divergence, got %v",
			mustMatchRules(rep))
	}
	t.Log("\n" + rep.FirstDivergence())
}

// TestTruthComesFromMetadataNotFromTheFileName is the property that three wrong
// conclusions about the StartObserve byte were made of: the file names carry no
// meaning and metadata.json's writer decides. Here the truth is in
// secondary.log, and the harness has to read it from there even though
// main.log is the file the layout puts first.
func TestTruthComesFromMetadataNotFromTheFileName(t *testing.T) {
	truth := secondaryFixture()
	truth.writer = sim.WriterOriginal
	truth.status = 3
	truth.activate = false
	truth.frames = []int{1, 0}
	decoy := primaryFixture()
	decoy.writer = sim.WriterOurs

	dir := writeCapture(t, filepath.Join(t.TempDir(), "capture"), decoy, truth)
	rep, err := sim.Check(dir, sim.Options{})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	for _, rule := range []string{sim.RuleStartObserveStatus, sim.RuleActivateObserverResponse, sim.RuleReplayPageOrder} {
		if !hasRule(rep.MustMatch, rule) {
			t.Errorf("the truth is in secondary.log, so %s had to fire; got %v", rule, mustMatchRules(rep))
		}
	}
	if rep.Reference != "secondary.log" {
		t.Errorf("the reference must be the side metadata.json calls the original, got %q", rep.Reference)
	}
	if rep.ReferenceWriter != sim.WriterOriginal {
		t.Errorf("reference writer = %q, want %q", rep.ReferenceWriter, sim.WriterOriginal)
	}
	t.Log("\n" + rep.Render())
}

// TestCaptureWithoutAnOriginalIsRefused: a capture whose sides are unknown has
// no truth, and passing it would look like a clean run.
func TestCaptureWithoutAnOriginalIsRefused(t *testing.T) {
	primary, secondary := primaryFixture(), secondaryFixture()
	primary.writer = sim.WriterUnknown
	secondary.writer = sim.WriterUnknown
	dir := writeCapture(t, filepath.Join(t.TempDir(), "capture"), primary, secondary)

	_, err := sim.Check(dir, sim.Options{})
	if err == nil {
		t.Fatal("a capture with no identified original must be refused, not passed")
	}
	for _, want := range []string{"primary.writer", "secondary.writer", "unknown"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must name %q so the run is diagnosable, got: %v", want, err)
		}
	}
}

// TestLoadReadsTheCapture: the loader has to recover the messages the client
// sent and the original answered, from logs whose rows are chopped mid-frame.
func TestLoadReadsTheCapture(t *testing.T) {
	dir := goodCapture(t)
	cap, err := sim.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cap.Reference.File != "main.log" {
		t.Fatalf("reference = %q, want main.log", cap.Reference.File)
	}
	if len(cap.Conns) != 2 {
		t.Fatalf("expected 2 connections, got %d", len(cap.Conns))
	}

	byPort := map[int]*sim.Conn{}
	for _, c := range cap.Conns {
		byPort[c.ClientPort] = c
	}
	player := byPort[fixturePlayerPort]
	if player == nil {
		t.Fatal("the player's connection was not read")
	}
	if player.Transport != sim.FramingRLink {
		t.Errorf("the bs port is length-prefixed, got transport %q", player.Transport)
	}
	if len(player.Up) != 4 {
		t.Fatalf("the client sent LobbyConnect and three pages, got %d messages: %v", len(player.Up), player.Up)
	}
	if player.Up[0].Type != protocol.MsgLobbyConnect {
		t.Errorf("first client message = 0x%02x, want 0x%02x", player.Up[0].Type, protocol.MsgLobbyConnect)
	}
	for i, m := range player.Up[1:] {
		if m.Type != protocol.MsgGameData {
			t.Errorf("client message %d = 0x%02x, want GameData", i+1, m.Type)
		}
	}
	// The connect echo and the three acks. The acks are the frames the observer is
	// served out of the store, so they are what the player got too.
	if len(player.Down) != 4 {
		t.Fatalf("the server answered the connect and three acks on the bs port, got %d", len(player.Down))
	}
	if player.Down[0].Type != protocol.MsgLobbyConnect {
		t.Errorf("first answer = %v, want the connect echo", player.Down[0])
	}
	for i, m := range player.Down[1:] {
		if m.Type != protocol.MsgEndOfGameData {
			t.Errorf("answer %d = %v, want an EndOfGameData ack", i+1, m)
		}
	}

	observer := byPort[fixtureObserverPort]
	if observer == nil {
		t.Fatal("the observer's connection was not read")
	}
	if observer.Transport != sim.FramingWS {
		t.Errorf("the websocket port is WebSocket-framed, got transport %q", observer.Transport)
	}
	// The upgrade request and response are HTTP, not battle messages: they must
	// be skipped, and the first message has to be the StartObserveRequest.
	if len(observer.Up) != 2 {
		t.Fatalf("the observer sent StartObserve and ActivateObserver, got %v", observer.Up)
	}
	if observer.Up[0].Type != 0x1C {
		t.Errorf("first observer message = 0x%02x, want 0x1C", observer.Up[0].Type)
	}
	if observer.Up[1].Type != protocol.MsgActivateObserver {
		t.Errorf("second observer message = 0x%02x, want 0x%02x", observer.Up[1].Type, protocol.MsgActivateObserver)
	}
	if len(observer.Down) != 4 {
		t.Fatalf("the original answered 0x19, 0x1E and two replay frames, got %d: %v", len(observer.Down), observer.Down)
	}
	if observer.Down[0].Type != protocol.MsgStartObserveResp || observer.Down[0].Body[0] != 0 {
		t.Errorf("the original's 0x19 must carry status 0, got %v", observer.Down[0])
	}
}

// TestConfigFromRecordedArgs: the harness rebuilds the configuration with the
// server's own flag parser, so the test can never be testing a configuration
// nobody ran. It also has to survive a command line carrying flags this parser
// does not know, which is what an original-only flag looks like.
func TestConfigFromRecordedArgs(t *testing.T) {
	args := []string{
		"-bsPort", "14001",
		"-webSocketPort", "14003",
		"-simulationPeriod", "125",
		"-maxPagesToStorePerSession", "5",
		"-name", "Sim Server",
		"-enableStats",
		"-somethingOnlyTheOriginalKnows", "42",
	}
	cfg, dropped, err := sim.ConfigFromArgs(args)
	if err != nil {
		t.Fatalf("ConfigFromArgs: %v", err)
	}
	if cfg.BsPort != 14001 || cfg.WebSocketPort != 14003 {
		t.Errorf("ports = %d/%d, want 14001/14003", cfg.BsPort, cfg.WebSocketPort)
	}
	if cfg.MaxPagesPerMatch != 5 {
		t.Errorf("maxPagesPerMatch = %d, want 5 (the original's own flag name must set the same field)", cfg.MaxPagesPerMatch)
	}
	if cfg.SimulationPeriod != 125 {
		t.Errorf("simulationPeriod = %v, want 125", cfg.SimulationPeriod)
	}
	if cfg.Name != "Sim Server" {
		t.Errorf("name = %q, want %q", cfg.Name, "Sim Server")
	}
	if !cfg.EnableStats {
		t.Error("a boolean flag with no value must not swallow the next argument")
	}
	if len(dropped) != 1 || !strings.Contains(dropped[0], "somethingOnlyTheOriginalKnows") {
		t.Errorf("the unknown flag and its value must be reported as dropped, got %v", dropped)
	}
}

// TestConfigFromRecordedArgsRefusesACommandLineThatNeverRan: -help and
// -simulationPeriod 0 with an observer interval both make the real binary exit,
// so a capture carrying them describes nothing.
func TestConfigFromRecordedArgsRefusesACommandLineThatNeverRan(t *testing.T) {
	for name, args := range map[string][]string{
		"help":           {"-help", "-bsPort", "14001"},
		"simulationZero": {"-simulationPeriod", "0", "-observerUpdateIntervalMS", "8"},
		"missingValue":   {"-bsPort", "not-a-number"},
	} {
		if _, _, err := sim.ConfigFromArgs(args); err == nil {
			t.Errorf("%s: ConfigFromArgs(%v) must fail: that command line makes the server exit, so it never ran", name, args)
		}
	}
}

// TestRecordedArgsReachTheServer: the ports of the capture are the only thing
// the harness changes on purpose, and the report has to say so. Everything else,
// including the observer settings, is what the recorded command line said.
//
// The two ports are ONE deviation and not two. The harness no longer picks them:
// it asks for OS-assigned ports and reads back what the listeners bound, so the
// two numbers arrive together and are reported together (see the port race in
// internal/sim/replay.go and SESSION-JOURNAL.md 38).
func TestRecordedArgsReachTheServer(t *testing.T) {
	dir := goodCapture(t)
	rep, err := sim.Check(dir, sim.Options{})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	changed := map[string]bool{}
	for _, d := range rep.Running {
		changed[d.Field] = true
		if d.Why == "" {
			t.Errorf("deviation %s has no reason: every change on purpose must be justified in the report", d.Field)
		}
	}
	for _, want := range []string{"listen ports", "logLevel"} {
		if !changed[want] {
			t.Errorf("%s should be listed as a deviation, got %v", want, changed)
		}
	}
	// The page cap decides whether the observer is admitted at all, so it must
	// never be one of the things the harness changes.
	if changed["maxPagesPerMatch"] {
		t.Error("the page cap came from the capture and must be reproduced, not overridden")
	}
	// The configuration the server ran with is the one our own side was
	// launched with, and the original's is reported next to it.
	if len(rep.ConfigDiff) == 0 {
		t.Error("the two sides of this capture have the same flags except their ports, so a diff has to be reported")
	}
	for _, d := range rep.ConfigDiff {
		if !strings.Contains(d, "bsport") && !strings.Contains(d, "websocketport") && !strings.Contains(d, "outofbandport") {
			t.Errorf("the two children may only differ in their listen ports, got %q", d)
		}
	}
}

var writeFixtureDir = flag.String("writefixture", "",
	"write the synthetic capture to this directory instead of running the comparison, so testdata can be regenerated")

// The second synthetic fixture: an observer that arrives at a store that has
// really rolled over.
//
// This is the _30 symptom, and it is the one shape of the observer handshake that
// none of the five committed captures covers: every one of them answered 0,
// because none of their stores ever dropped a block. So the arm that answers 3,
// the one that does NOT register the observer and leaves the client with no 0x1E
// and no replay frames, had no end-to-end coverage at all: it existed as a unit
// test of a state machine and as a number measured in a capture that was
// deleted.
//
// What makes the store roll is the recorded command line and nothing else, which
// is the point: the original's store is ceil(peers * f) blocks of -pageSize
// bytes, with f = -maxPagesToStorePerSession/8 and peers floored at 8, so one
// block of 128 bytes is a store that forgets its first frame after two acks, and
// the same capture with the default 64 KiB block is a store that forgets
// nothing. Both directions come out of one fixture body.
//
// rolloverPageSize is the block size that makes the store roll within the second
// the fixture records. It is an explicit number and not "whatever is small": the
// fixture's first ack is 38 bytes and the heartbeats after it are 6, and the
// original charges a frame its own size plus 57 bytes of overhead, so 128 puts
// the second ack over the line and the store recycles its only block.
const rolloverPageSize = 128

// rolloverFixture is the pair of sides of the second fixture, differing from the
// first one only in the recorded command line and in what the original answered.
func rolloverFixture(pageSize int) (sideFixture, sideFixture) {
	primary := primaryFixture()
	primary.status = 3
	// The original answered 3 and the client never sent an ActivateObserver it was
	// answered to: no 0x1E and no replay frame. That is the whole of the arm: the
	// response is built and the server returns without registering the observer.
	primary.activate = false
	primary.frames = nil
	primary.args = rolloverArgs(fixtureBsPrimary, fixtureWsPrimary, pageSize)
	secondary := primary
	secondary.writer = sim.WriterOurs
	secondary.bsPort = fixtureBsSecondary
	secondary.wsPort = fixtureWsSecondary
	secondary.args = rolloverArgs(fixtureBsSecondary, fixtureWsSecondary, pageSize)
	return primary, secondary
}

// rolloverCapture writes the fixture at the given block size and returns its
// directory.
func rolloverCapture(t *testing.T, dir string, pageSize int) string {
	t.Helper()
	primary, secondary := rolloverFixture(pageSize)
	return writeCapture(t, dir, primary, secondary)
}

func rolloverArgs(bsPort, wsPort, pageSize int) []string {
	return []string{
		"-relaybroadcastPort", "0",
		"-bsPort", strconv.Itoa(bsPort),
		"-outofbandPort", "14002",
		"-webSocketPort", strconv.Itoa(wsPort),
		"-simulationPeriod", "125",
		"-maxPagesToStorePerSession", "1",
		"-pageSize", strconv.Itoa(pageSize),
	}
}

// TestLateObserverAgainstARolledStoreIsRefusedWithThree is the end-to-end shape of
// _30: the observer asks, the store has already forgotten the start of the match,
// and the answer is a 3 with nothing behind it.
//
// Both directions come out of the same fixture, which is what makes it falsifiable
// rather than a golden test: with the block size that rolls the store the two
// sides agree and there is no must-match divergence, and with the block size the
// original's own default would use on a real match they do not agree, and the
// difference is the start-observe byte, which is the one byte that decided the
// whole observer investigation.
func TestLateObserverAgainstARolledStoreIsRefusedWithThree(t *testing.T) {
	rolled, err := sim.Check(rolloverCapture(t, filepath.Join(t.TempDir(), "rolled"), rolloverPageSize), sim.Options{})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	t.Log("\n" + rolled.Render())
	if !rolled.OK() {
		t.Fatalf("with a store that has dropped its first block the original answered 3 and served nothing, and so "+
			"must we, but there is a must-match divergence:\n%s", rolled.FirstDivergence())
	}
	// The finding that must be there: an observer that got nothing on either side
	// is said out loud, so a green report cannot be two empty streams.
	found := false
	for _, f := range rolled.Informative {
		if f.Rule == sim.RuleReplayPageOrder && strings.Contains(f.Detail, "neither side served") {
			found = true
		}
	}
	if !found {
		t.Error("neither side served this observer a frame, and the report has to say so: a green run over two empty " +
			"streams is the failure mode of section 24")
	}

	// The falsification: a store big enough to hold the whole match is a store that
	// forgets nothing, so the original's 0 for a 3 becomes a divergence and it has
	// to be must-match.
	intact, err := sim.Check(rolloverCapture(t, filepath.Join(t.TempDir(), "intact"), 65536), sim.Options{})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !hasRule(intact.MustMatch, sim.RuleStartObserveStatus) {
		t.Fatalf("a store that has dropped nothing must be registered, so our 0 has to be reported against the "+
			"original's 3, and it has to be must-match. Got %v", mustMatchRules(intact))
	}
	f := intact.MustMatch[0]
	if !strings.Contains(f.Ours, "0x19") || !strings.Contains(f.Theirs, "0x19") {
		t.Errorf("the finding must carry both 0x19 frames, got ours=%q theirs=%q", f.Ours, f.Theirs)
	}
	if !strings.Contains(f.Detail, "0") || !strings.Contains(f.Detail, "3") {
		t.Errorf("the finding must name both bytes, got %q", f.Detail)
	}
	t.Log("\n" + intact.FirstDivergence())
}

// TestRolloverFixtureNeedsNoCaptureOnDisk guards the property the whole fixture
// family rests on: it is written by this test, in a temporary directory, from the
// code above. Nothing about it may come from a file that is not regenerated here,
// or it stops being coverage a machine without captures has.
func TestRolloverFixtureNeedsNoCaptureOnDisk(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rolled")
	rolloverCapture(t, dir, rolloverPageSize)
	for _, name := range []string{"main.log", "secondary.log", "metadata.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was not written: %v", name, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the fixture wrote %v, want only the two logs and the metadata: anything else would be state the "+
			"comparison silently depends on", names)
	}
}

// TestWriteSyntheticFixture regenerates internal/sim/testdata/synthetic, the
// capture the generated sim_synthetic_test.go runs against, and with -writefixture
// set to a directory ending in "rollover" it regenerates the second one. It only
// does anything when -writefixture is given, so the ordinary suite never rewrites
// a recorded fixture.
func TestWriteSyntheticFixture(t *testing.T) {
	if *writeFixtureDir == "" {
		t.Skip("pass -writefixture=<dir> to regenerate the committed fixture")
	}
	if err := os.MkdirAll(*writeFixtureDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	rolling := strings.HasSuffix(strings.TrimRight(filepath.ToSlash(*writeFixtureDir), "/"), "-rollover")
	if rolling {
		rolloverCapture(t, *writeFixtureDir, rolloverPageSize)
		if err := os.WriteFile(filepath.Join(*writeFixtureDir, "ORIGEN.txt"), []byte(rolloverOrigen), 0o644); err != nil {
			t.Fatalf("write ORIGEN.txt: %v", err)
		}
		t.Logf("rollover fixture written to %s", *writeFixtureDir)
		return
	}
	writeCapture(t, *writeFixtureDir, primaryFixture(), secondaryFixture())
	if err := os.WriteFile(filepath.Join(*writeFixtureDir, "ORIGEN.txt"), []byte(syntheticOrigen), 0o644); err != nil {
		t.Fatalf("write ORIGEN.txt: %v", err)
	}
	t.Logf("fixture written to %s", *writeFixtureDir)
}

// The two ORIGEN.txt, in the language of the other committed records: what is
// measured and what is written by hand, and where in the decompilation each hand
// written byte comes from.
const syntheticOrigen = "SYNTHETIC capture, hand-written by internal/sim/sim_test.go, not recorded from a match.\n" +
	"The original's side is encoded with its own acceptance rule (0x19=0 and the observer's\n" +
	"registration), its 0x1E answer, and the shape of the window the observer receives: the\n" +
	"session's 0x0As, in order, from the first one, and nothing else. That shape is the one\n" +
	"2026-09-29_012528_03cb67c7cf19 measured on the real original (1346 and 800 messages, all\n" +
	"identical byte for byte to messages of the player's 0x0A). The 0x0A differences are\n" +
	"informative on purpose: the real server emits one ack per simulation tick and here there is one.\n" +
	"Regenerate with:\n" +
	"  go test ./internal/sim -run TestWriteSyntheticFixture -writefixture=<dir>\n"

// rolloverOrigen is the second fixture: the same traffic with a frame store small
// enough to have forgotten the start of the match by the time the observer asks.
const rolloverOrigen = "SYNTHETIC capture, hand-written by internal/sim/sim_test.go, not recorded from a match.\n" +
	"It is the SAME capture as testdata/synthetic with two different things, and both are in the\n" +
	"recorded command line, which is the only thing that changes:\n" +
	"  -maxPagesToStorePerSession 1   a single block in the frame store\n" +
	"  -pageSize 128                  of 128 bytes in that block\n" +
	"The original's store is ceil(peers * f) blocks of -pageSize bytes, with peers with a minimum of 8\n" +
	"and f = -maxPagesToStorePerSession/8, and a frame costs its size plus 57 bytes of overhead.\n" +
	"With 128 bytes the second ack does not fit in the block and the store recycles its own, so by\n" +
	"the time the observer asks at t=1s the store no longer has the match's first frame.\n" +
	"The original's side is written with the arm that returns 3: it builds the response and returns\n" +
	"WITHOUT registering the observer, that is 0x19=3, no 0x1E and no replay window. It is the\n" +
	"symptom of capture _30, the only one of the five committed that does not cover this shape: in\n" +
	"all of them the Original store had lost nothing and answered 0.\n" +
	"The falsifier is the same fixture with -pageSize 65536, the original's own default: there the\n" +
	"store loses nothing, the arm is the 0 one, and the difference comes out must-match from start-observe-status.\n" +
	"Regenerate with:\n" +
	"  go test ./internal/sim -run TestWriteSyntheticFixture -writefixture=internal/sim/testdata/synthetic-rollover\n"
