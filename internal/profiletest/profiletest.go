// Package profiletest reads the sample count out of a pprof profile.
//
// It exists because two packages in this repo take CPU profiles of themselves and
// both have to answer the same question before they may report anything: does the
// file carry samples, or is it an empty window with a beautiful top-10 waiting to be
// read out of it? A count is a counter and can be asserted; "have a look at the
// profile" cannot.
//
// The alternative to this package is shelling out to `go tool pprof` from a test,
// which depends on that tool's text output staying the same across Go releases, or
// counting Sample records, which is not the same number (see SampleCount).
package profiletest

import (
	"bytes"
	"compress/gzip"
	"io"
)

// SampleCount is the number of CPU samples in a pprof CPU profile.
//
// Four things about the format make this necessary rather than convenient, and all
// four were got wrong first, so they are written down here once instead of twice:
//
//   - Go 1.27 GZIPS what pprof.StartCPUProfile and Lookup("heap").WriteTo produce.
//     The file starts with 1f 8b, so a parser that reads the bytes as protobuf
//     counts zero samples in a file `go tool pprof` reports as full. Measured on
//     this machine: 0 against 958.
//   - Sample is field 2 of Profile, not field 1 (field 1 is sample_type). Counting
//     field 1 counts the two value types.
//   - The number of Sample RECORDS is not the number of samples. Go coalesces
//     identical stacks inside one profiling cycle, so a window can carry fewer
//     records than ticks: measured 678 records carrying 958 samples.
//   - So the count has to come out of the values, and it has to be the FIRST of the
//     two: Go writes value = [samples, cpu-nanoseconds], and the second of those is
//     958 million on the same window, not 958.
//
// It is checked against `go tool pprof -sample_index=samples` on six real profiles
// in SESSION-JOURNAL.md 39, where the two agree exactly. That cross-check is what
// makes this worth having: an earlier version of this walker skipped an unknown
// field by moving on one byte instead of consuming its payload, which
// desynchronised it and counted MORE rather than failing loudly (1192 against
// pprof's 1076), and nothing but the comparison would have caught it.
func SampleCount(raw []byte) int {
	body := raw
	if len(body) > 1 && body[0] == 0x1f && body[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return 0
		}
		defer func() { _ = zr.Close() }()
		plain, err := io.ReadAll(zr)
		if err != nil {
			return 0
		}
		body = plain
	}
	total, i := 0, 0
	for i < len(body) {
		field, wire, start, next, ok := NextField(body, i)
		if !ok {
			return total
		}
		if field == 2 && wire == 2 { // Profile.sample
			total += firstSampleValue(body[start:next])
		}
		i = next
	}
	return total
}

// Contains reports whether any function name in the profile mentions needle.
//
// It is the assertion that a positive SampleCount cannot make on its own. A window
// can carry hundreds of samples and every one of them can be the TEST HARNESS: the
// fixtures run in the same process as the code they drive, so a profile of a window
// in which the code under test never ran is a perfectly well-formed profile of the
// goroutine that was supposed to be running it. SampleCount says the profiler was
// on; this says what it was looking at.
//
// needle is matched against the profile's string table, where pprof keeps whole
// function names such as
// "github.com/luskaner/ageBattleServer/internal/session.(*AckState).PopTick",
// so a package path or a method name both work.
func Contains(raw []byte, needle string) bool {
	body := raw
	if len(body) > 1 && body[0] == 0x1f && body[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return false
		}
		defer func() { _ = zr.Close() }()
		plain, err := io.ReadAll(zr)
		if err != nil {
			return false
		}
		body = plain
	}
	return bytes.Contains(body, []byte(needle))
}

// firstSampleValue is Sample.value[0] out of one Sample record. In that record field
// 1 is location_id (a repeated uint64) and field 2 is value, so the first field-2
// varint is the sample count and not a location index.
func firstSampleValue(sample []byte) int {
	i := 0
	for i < len(sample) {
		field, wire, start, _, ok := NextField(sample, i)
		if !ok {
			return 0
		}
		if field == 2 && wire == 0 {
			v, after := Varint(sample, start)
			if after == 0 {
				return 0
			}
			return int(v)
		}
		if wire == 0 {
			if _, after := Varint(sample, start); after == 0 {
				return 0
			} else {
				i = after
			}
		} else {
			i = start
			_, _, _, n, ok := NextField(sample, i)
			if !ok {
				return 0
			}
			i = n
		}
	}
	return 0
}

// NextField reads one protobuf field header at i and returns its field number, its
// wire type, the index where its PAYLOAD begins, and the index just past the whole
// field.
//
// Consuming the payload is the point, and it is the whole difference between this
// and a walker that skips an uninteresting field by moving on one byte: a varint
// payload is exactly the shape of a small tag, so the walk desynchronises and starts
// counting fields that are not in the file.
func NextField(raw []byte, i int) (field, wire uint64, start, next int, ok bool) {
	tag, after := Varint(raw, i)
	if after == 0 {
		return 0, 0, 0, 0, false
	}
	field, wire = tag>>3, tag&7
	switch wire {
	case 0: // varint
		if _, end := Varint(raw, after); end == 0 {
			return 0, 0, 0, 0, false
		} else {
			return field, wire, after, end, true
		}
	case 1: // 64-bit
		if after+8 > len(raw) {
			return 0, 0, 0, 0, false
		}
		return field, wire, after, after + 8, true
	case 2: // length-delimited
		size, end := Varint(raw, after)
		// Compare before converting: int(size) on a 32-bit build can wrap a
		// huge declared length into a small positive number and accept a field
		// that runs past the buffer.
		if end == 0 || end > len(raw) || size > uint64(len(raw)-end) {
			return 0, 0, 0, 0, false
		}
		return field, wire, end, end + int(size), true
	case 5: // 32-bit
		if after+4 > len(raw) {
			return 0, 0, 0, 0, false
		}
		return field, wire, after, after + 4, true
	}
	return 0, 0, 0, 0, false
}

// Varint decodes the varint at i and returns its value and the index just past it,
// or (0, 0) when there is not a varint there.
func Varint(raw []byte, i int) (uint64, int) {
	var v uint64
	var shift uint
	for ; i < len(raw); i++ {
		b := raw[i]
		v |= uint64(b&0x7f) << shift
		if b < 0x80 {
			return v, i + 1
		}
		shift += 7
		if shift > 63 {
			return 0, 0
		}
	}
	return 0, 0
}
