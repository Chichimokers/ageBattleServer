package sim

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A trimmed capture: the same run, with most of its wall clock cut off.
//
// Every capture here is replayed in REAL TIME, on purpose, because the server emits
// 0x0A per simulation tick and not per input: feeding a capture faster leaves the
// server less wall-clock time to produce the output the comparison is about
// (Options.ReplaySpeed). The price of that decision is that a test costs what the
// recorded run cost, so the 14 minute capture is a 14 minute test and the 24 minute
// one does not fit in any budget. The only way out that does not falsify the
// comparison is to make the DATA shorter. This file is that: it keeps the first
// seconds of a capture and drops the rest of the clock, changing nothing else.
//
// What makes a crop checkable is that the truth is a recording. The harness compares
// our server against what the ORIGINAL answered to the same bytes, and those bytes
// are on disk, so a crop either leaves a prefix of them (nothing observable was
// lost) or it does not (and CheckTrim says so). Nothing here decides that: the
// verdicts are computed by replaying both captures, in CheckTrim, from the same
// rules every other capture goes through.
//
// # What is never cut
//
// The handshake and the control answers (0x19, 0x1E) are inside the kept window by
// construction: they are what a capture is usually taken for, and the crop point is
// chosen after the last one. The observer's replay window start is not something the
// crop can move, because the window starts at the first retained frame of the store
// and the frames before the cut are untouched. The order of the sub-messages and of
// the checksum entries cannot move either, for the same reason: they are sequences,
// and a suffix is removed, never a middle. Every one of those four claims is
// asserted by CheckTrim rather than repeated here.

// formatVersionTrimmed is the metadata version of a capture that has been cropped.
// Version 1 is what cmd/proxy writes and version 2 is the same document plus the
// trim record, which is what says the window is not the whole run. Load accepts
// both, and refuses a version 2 with no trim record: a capture that claims to be
// cropped without saying what was cut is a capture nobody can audit.
const formatVersionTrimmed = 2

// TrimRecord is what a trimmed capture carries in its metadata.json. It is written
// by the tool that cropped the capture and read back by the harness, which prints it
// in the report: a reader of a failing test has to be able to see that the capture
// he is looking at is three minutes of a twenty-five minute run, and what the tool
// checked before believing that was safe.
type TrimRecord struct {
	// Tool is what produced it, so a crop can be re-derived and a different tool's
	// crop is not silently mixed with this one's.
	Tool string `json:"tool"`
	// Source is the capture this one was cut from, by name.
	Source string `json:"source"`
	// KeptFrom and KeptTo are the RFC3339 stamps of the first and last row kept,
	// and the span between them is the capture's new length. Dropped is what the
	// crop took off the end, so "how much of the run is not in this directory" is
	// a number in the file rather than a subtraction the reader has to do.
	KeptFrom string `json:"kept_from"`
	KeptTo   string `json:"kept_to"`
	KeptSpan string `json:"kept_span"`
	Dropped  string `json:"dropped"`
	// Why is the reason the crop point was chosen there, in the words of whoever
	// chose it, measured against the report it came from.
	Why string `json:"why"`
	// Verified is the evidence: the invariants CheckTrim asserted, with the
	// numbers they were asserted on. A crop whose evidence is empty was never
	// checked, and the harness says so in the report.
	Verified []string `json:"verified,omitempty"`
	// Leftover is how many bytes of the truth's last message the cut left
	// incomplete on each log, if any. Non-zero means the crop point fell inside a
	// message, so that message is not compared at all.
	Leftover string `json:"leftover,omitempty"`
}

// TrimOptions is a crop request. Keep is measured from the first row of the
// capture, which is the earliest of anything either log recorded.
type TrimOptions struct {
	Keep   time.Duration
	Source string
	Why    string
}

// Trim copies the capture at src into dst, keeping the rows of both logs that fall
// inside the first opts.Keep of the capture's own timeline, and writes the trim
// record into the copy's metadata.json.
//
// The rows are copied field for field, base64 payload included, so the trimmed log
// is a subset of the original log and not a re-encoding of it: anything the harness
// reads out of a trimmed capture was in the capture. The timestamps are NOT moved:
// the timeline is the same axis before and after, so a moment in a trimmed report
// means the same moment of the same match that it meant in the full one.
//
// dst must not exist, or must be an empty directory. A crop that overwrote a
// capture would destroy the only copy of a run that cannot be repeated, which is
// exactly what this file must never do.
func Trim(src, dst string, opts TrimOptions) (*TrimRecord, error) {
	if opts.Keep <= 0 {
		return nil, fmt.Errorf("trim: a window of %s would keep nothing, and a capture with no rows compares nothing and looks like a pass", opts.Keep)
	}
	raw, err := os.ReadFile(filepath.Join(src, metadataName))
	if err != nil {
		return nil, fmt.Errorf("trim: read %s: %w", metadataName, err)
	}
	var meta captureMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, fmt.Errorf("trim: parse %s: %w", metadataName, err)
	}
	if meta.FormatVersion == formatVersionTrimmed {
		if meta.Trim == nil {
			return nil, fmt.Errorf("trim: %s says format_version %d but carries no trim record, so it is refused rather than cropped", filepath.Base(src), formatVersionTrimmed)
		}
		return nil, fmt.Errorf("trim: %s is already trimmed (kept %s of its %s): crop the original, not a crop",
			filepath.Base(src), meta.Trim.KeptSpan, meta.Trim.Dropped)
	}

	if err := requireEmptyDir(dst); err != nil {
		return nil, fmt.Errorf("trim: %w", err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return nil, fmt.Errorf("trim: %w", err)
	}

	// Two logs, one window: the crop point has to be the same instant in both, or
	// the truth would be answering past the moment the input stopped and every
	// count after it would be a difference between the two clocks.
	first, err := firstRowTime(src, mainLogName, secondaryLogName)
	if err != nil {
		return nil, err
	}
	if first.IsZero() {
		return nil, fmt.Errorf("trim: %s holds no rows with a timestamp in either log: there is no run to cut", src)
	}
	last := first.Add(opts.Keep)

	rec := &TrimRecord{
		Tool:     "cmd/trimsim",
		Source:   orUnknown(opts.Source, filepath.Base(src)),
		KeptFrom: first.Format(time.RFC3339),
		KeptTo:   last.Format(time.RFC3339),
		KeptSpan: opts.Keep.String(),
		Why:      opts.Why,
	}
	var kept, dropped int
	for _, name := range []string{mainLogName, secondaryLogName} {
		k, d, err := copyWindow(filepath.Join(src, name), filepath.Join(dst, name), first, last)
		if err != nil {
			return nil, fmt.Errorf("trim: %s: %w", name, err)
		}
		kept += k
		dropped += d
	}
	if kept == 0 {
		return nil, fmt.Errorf("trim: the first %s of %s hold no row at all, so the crop would leave an empty capture", opts.Keep, filepath.Base(src))
	}
	rec.Dropped = fmt.Sprintf("%d of %d rows", dropped, kept+dropped)
	// The bytes are a third pass over the logs for a number nobody can work out
	// afterwards: how much smaller this capture is than the run it came from.
	if size, err := windowBytes(src, first, last); err == nil {
		rec.Dropped = fmt.Sprintf("%s, %.1f MB of log", rec.Dropped, float64(size)/(1<<20))
	}

	meta.FormatVersion = formatVersionTrimmed
	meta.Trim = rec
	if err := writeMeta(dst, meta); err != nil {
		return nil, err
	}
	// The rest of the capture describes the RUN, not the window: the commit it was
	// made at, which binary wrote which log, and the sha256 of both. Those stay, or
	// a cropped capture could not be traced back to the run that produced it.
	if err := copySidecars(src, dst); err != nil {
		return nil, err
	}
	return rec, nil
}

// SetVerified records what CheckTrim established about a crop, and rewrites the
// capture's metadata with it. It is a separate step from Trim on purpose: the crop
// and the proof are two facts, and a crop whose proof has not been run must be
// visible as unproven rather than as fine.
func SetVerified(dir string, verified []string, leftover string) error {
	raw, err := os.ReadFile(filepath.Join(dir, metadataName))
	if err != nil {
		return err
	}
	var meta captureMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return err
	}
	if meta.Trim == nil {
		return fmt.Errorf("%s carries no trim record, so there is nothing to record a proof for", metadataName)
	}
	meta.Trim.Verified = verified
	meta.Trim.Leftover = leftover
	return writeMeta(dir, meta)
}

func writeMeta(dir string, meta captureMeta) error {
	body, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, metadataName), append(body, '\n'), 0o644); err != nil {
		return err
	}
	return nil
}

// sidecars are the files that describe the run rather than carry its traffic. The
// two *.meta.json are what a single log can be read on its own with, and refusing a
// capture whose sidecar contradicts metadata.json (checkSidecars) is one of the
// three wrong-conclusion guards, so a crop that dropped them would have weakened a
// check to save a kilobyte.
var sidecars = []string{"main.meta.json", "secondary.meta.json", "COMMIT.txt", "roles.json"}

func copySidecars(src, dst string) error {
	for _, name := range sidecars {
		body, err := os.ReadFile(filepath.Join(src, name))
		if errors.Is(err, os.ErrNotExist) {
			// A capture predating the sidecars is still readable: metadata.json is
			// the authoritative record either way.
			continue
		}
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dst, name), body, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func requireEmptyDir(dst string) error {
	entries, err := os.ReadDir(dst)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("%s exists and is not empty: a crop writes a new capture and never overwrites one", dst)
	}
	return nil
}

// firstRowTime is the earliest timestamp either log holds. It is the start of the
// capture's clock, and it is read from the raw rows rather than from the framed
// messages so that a server message written before the client's first byte is not
// left behind by the crop.
func firstRowTime(src string, logs ...string) (time.Time, error) {
	var first time.Time
	for _, name := range logs {
		rows, err := newRowReader(filepath.Join(src, name))
		if err != nil {
			return time.Time{}, err
		}
		first, err = earliestIn(rows, first)
		// The reader holds the file open, and a crop that leaves it open would
		// make the source directory undeletable on Windows.
		if cerr := rows.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return time.Time{}, fmt.Errorf("trim: %s: %w", name, err)
		}
	}
	return first, nil
}

func earliestIn(rows *rowReader, first time.Time) (time.Time, error) {
	for {
		rec, err := rows.Next()
		if errors.Is(err, io.EOF) {
			return first, nil
		}
		if err != nil {
			return first, err
		}
		at, err := rowTime(rec)
		if err != nil {
			continue // a row the harness itself would skip
		}
		if first.IsZero() || at.Before(first) {
			first = at
		}
	}
}

// windowBytes is how much log the kept window holds, for the record.
func windowBytes(src string, from, to time.Time) (int64, error) {
	var n int64
	for _, name := range []string{mainLogName, secondaryLogName} {
		rows, err := newRowReader(filepath.Join(src, name))
		if err != nil {
			return 0, err
		}
		var size int64
		for {
			rec, err := rows.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				_ = rows.Close()
				return 0, err
			}
			at, err := rowTime(rec)
			if err != nil || at.Before(from) || !at.Before(to) {
				continue
			}
			size += int64(len(rec.fields[rows.dataCol()]))
		}
		if err := rows.Close(); err != nil {
			return 0, err
		}
		n += size
	}
	return n, nil
}

// copyWindow writes the rows of one log that fall inside [start, end) to a new
// file, and says how many it kept and how many it left behind.
//
// The window is half-open on purpose: opts.Keep seconds of a capture are the rows
// from the first one up to but not including the row exactly Keep later, so the
// span the record prints and the rows on disk are the same number. A closed window
// would keep one second more than it says, which is the kind of discrepancy that
// makes a record untrustworthy six months later.
//
// The rows are streamed, never collected: a log is tens of megabytes of base64 and
// a crop that held it all in memory would be the first thing to go wrong on the
// largest capture. The reader reads the timestamp column and the writer emits the
// record it was given, so the payload is never decoded and never re-encoded.
func copyWindow(from, to string, start, end time.Time) (kept, dropped int, err error) {
	in, err := newRowReader(from)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(to)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = out.Close() }()
	w := csv.NewWriter(out)

	header, err := in.Header()
	if err != nil {
		return 0, 0, fmt.Errorf("read header: %w", err)
	}
	if err := w.Write(header); err != nil {
		return 0, 0, err
	}
	for {
		rec, err := in.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return kept, dropped, err
		}
		at, err := rowTime(rec)
		// A row with no readable timestamp is kept: the harness skips it too, so
		// dropping it here would change nothing, and keeping it keeps this file
		// from being the place where a row quietly disappears.
		if err == nil && (at.Before(start) || !at.Before(end)) {
			dropped++
			continue
		}
		if err := w.Write(rec.fields); err != nil {
			return kept, dropped, err
		}
		kept++
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return kept, dropped, err
	}
	return kept, dropped, out.Close()
}

// rowReader streams a capture log one record at a time, holding the columns this
// package reads and nothing else.
type rowReader struct {
	f    *os.File
	r    *csv.Reader
	head []string
	ts   int
	data int
}

// captureRow is one CSV row: the raw fields as they were written, plus the moment
// the proxy stamped it. The payload stays as the base64 the proxy wrote, because a
// crop copies rows and never decodes them.
type captureRow struct {
	fields []string
	ts     time.Time
}

func newRowReader(path string) (*rowReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	rr := &rowReader{f: f, r: csv.NewReader(f)}
	rr.r.FieldsPerRecord = -1
	head, err := rr.r.Read()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%s: read header: %w", filepath.Base(path), err)
	}
	cols, err := csvColumns(head)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	rr.head, rr.ts, rr.data = head, cols.ts, cols.data
	return rr, nil
}

func (r *rowReader) Header() ([]string, error) { return r.head, nil }

// dataCol is where the base64 payload is, which is also how much log a row is
// worth: the log is a CSV and the payload is most of it.
func (r *rowReader) dataCol() int { return r.data }

func (r *rowReader) Next() (*captureRow, error) {
	rec, err := r.r.Read()
	if errors.Is(err, io.EOF) {
		return nil, io.EOF
	}
	if err != nil {
		return nil, err
	}
	row := &captureRow{fields: rec}
	row.ts, _ = time.Parse(time.RFC3339, strings.TrimSpace(rec[r.ts]))
	return row, nil
}

func (r *rowReader) Close() error { return r.f.Close() }

func rowTime(rec *captureRow) (time.Time, error) {
	if rec.ts.IsZero() {
		return time.Time{}, fmt.Errorf("row has no RFC3339 timestamp")
	}
	return rec.ts, nil
}

func orUnknown(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}
