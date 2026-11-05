// Command gensim turns proxy captures into simulation tests.
//
// One capture is one test, and the test encodes nothing about the capture: it
// hands the directory to sim.Run and sim decides what to compare. That is what
// makes adding a capture a matter of dropping a directory in, never of editing
// the harness, so this generator only has to do the mechanical part: put the
// capture where `go test` can read it, and write the one function that runs it.
//
//	gensim captures                    # every capture under ./captures
//	gensim -out src_go/internal/sim src_go/internal/sim/testdata/synthetic
//	gensim -check captures             # fail if anything is out of date
//
// -check is the mode worth running in CI: it writes nothing and fails when the
// committed test files do not match what the captures would produce, so a
// capture cannot be added without its test and a test cannot be edited by hand.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// copiedFiles is what a capture needs to be replayable. proxy.log is the proxy's
// own diagnostic output: not traffic, and the largest file of the three, so it
// stays behind with the capture.
var copiedFiles = []string{
	"main.log",
	"secondary.log",
	"metadata.json",
	"main.meta.json",
	"secondary.meta.json",
	"COMMIT.txt",
	"roles.json",
}

// capturesRoot is where the proxy writes, relative to the working directory
// (cmd/proxy/capture.go:CaptureDir). It is a default, not a rule: every root
// can be named on the command line.
const defaultCapturesRoot = "captures"

// testDataDirName is the subdirectory of internal/sim that the generated tests
// read from. The path in a generated test is relative to the sim package, which
// is also where testdata has to live for `go test` to be able to read it.
const testDataDirName = "testdata"

func main() {
	out := flag.String("out", "internal/sim", "directory that holds the sim package and its testdata")
	check := flag.Bool("check", false, "write nothing and fail when the generated files are out of date")
	flag.Parse()

	if err := run(flag.Args(), *out, *check); err != nil {
		fmt.Fprintln(os.Stderr, "gensim:", err)
		os.Exit(1)
	}
}

func run(roots []string, out string, check bool) error {
	if len(roots) == 0 {
		roots = []string{defaultCapturesRoot}
	}
	abs, err := filepath.Abs(out)
	if err != nil {
		return err
	}
	testdata := filepath.Join(abs, testDataDirName)

	captures, order, err := discover(roots)
	if err != nil {
		return err
	}
	if len(order) == 0 {
		return fmt.Errorf("no capture found in %s: a capture is a directory with a metadata.json in it", strings.Join(roots, ", "))
	}

	// The generated test's file name is ident(name), which maps every run of
	// punctuation to one underscore. Two different captures can therefore want
	// the same file, and the second would overwrite the first's test in silence.
	idOwner := make(map[string]string, len(order))
	for _, name := range order {
		id := ident(name)
		if prev, ok := idOwner[id]; ok {
			return fmt.Errorf("captures %q and %q both map to the generated identifier %q: their tests would overwrite each other, so one has to be renamed", prev, name, id)
		}
		idOwner[id] = name
	}
	// A capture already installed in testdata collides the same way, and the
	// generated file would overwrite one of the two without saying so.
	installed, err := installedCaptures(testdata)
	if err != nil {
		return err
	}
	for _, name := range installed {
		id := ident(name)
		if prev, ok := idOwner[id]; ok && prev != name {
			return fmt.Errorf("capture %q maps to the generated identifier %q, which the installed capture %q already uses: one has to be renamed", name, id, prev)
		}
		idOwner[id] = name
	}

	var stale []string
	for _, name := range order {
		src := captures[name]
		dst := filepath.Join(testdata, name)
		if err := install(src, dst, check); err != nil {
			return fmt.Errorf("capture %s: %w", name, err)
		}
		path := filepath.Join(abs, testFileName(name))
		changed, err := write(path, testBody(name), check)
		if err != nil {
			return err
		}
		if changed {
			fmt.Printf("%s %s\n", verb(check), rel(abs, path))
		}
	}
	// A generated test whose capture is gone would keep running against a
	// directory that no longer exists, and fail for a reason that has nothing to
	// do with the server. They are removed, and only those: the marker comment
	// is what makes a file ours to delete. The comparison goes through ident(),
	// because the file name carries the reduced name and the captures carry the
	// real one.
	// A capture counts as live if it is already installed in testdata, not only
	// if it was found in the roots that were passed in. The roots are a search
	// path, not the whole world: running the generator against a single new
	// capture would otherwise declare every other generated test stale and
	// DELETE it, taking the harness's own regression tests with it, in silence.
	live := make(map[string]bool, len(order))
	for _, name := range order {
		live[ident(name)] = true
	}
	for _, name := range installed {
		live[ident(name)] = true
	}
	for _, f := range existingGenerated(abs) {
		if live[ident(captureOf(f))] {
			continue
		}
		if !check {
			if err := os.Remove(filepath.Join(abs, f)); err != nil {
				return err
			}
		}
		stale = append(stale, f)
		fmt.Printf("%s %s (its capture is gone)\n", verb(check), rel(abs, filepath.Join(abs, f)))
	}
	if check && len(stale) > 0 {
		return fmt.Errorf("%d generated test(s) have no capture: %s", len(stale), strings.Join(stale, ", "))
	}
	return nil
}

// discover returns the captures under the given roots: the full path of each and
// the names in a stable order, keyed by directory name. The name is what the
// generated test and its testdata directory are called, so two captures sharing
// one are refused here rather than silently merged.
func discover(roots []string) (map[string]string, []string, error) {
	captures := map[string]string{}
	var order []string
	for _, root := range roots {
		if !isDir(root) {
			return nil, nil, fmt.Errorf("%s is not a directory", root)
		}
		// A root is either one capture or a directory of them, because both are
		// things a person actually has to hand over.
		if exists(filepath.Join(root, "metadata.json")) {
			name := filepath.Base(filepath.Clean(root))
			captures[name] = root
			order = append(order, name)
			continue
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			return nil, nil, err
		}
		for _, e := range entries {
			if !e.IsDir() || !exists(filepath.Join(root, e.Name(), "metadata.json")) {
				continue
			}
			if _, dup := captures[e.Name()]; dup {
				return nil, nil, fmt.Errorf("two captures are called %s: their generated tests would collide, so one has to be renamed", e.Name())
			}
			captures[e.Name()] = filepath.Join(root, e.Name())
			order = append(order, e.Name())
		}
	}
	slices.Sort(order)
	return captures, order, nil
}

// install copies a capture into testdata, unless the copy there is already
// identical. A capture is the record of a run that cannot be repeated, so
// overwriting a committed one is refused rather than done quietly: pass a
// different -out if the copy is meant to change.
func install(src, dst string, check bool) error {
	for _, name := range copiedFiles {
		from := filepath.Join(src, name)
		body, err := os.ReadFile(from)
		if errorsIsNotExist(err) {
			// The sidecars and COMMIT.txt are not in every capture; the two logs
			// and the metadata are, and Load refuses a capture without them.
			if name == "main.log" || name == "secondary.log" || name == "metadata.json" {
				return fmt.Errorf("%s is missing: it is not a capture this harness can read", name)
			}
			continue
		}
		if err != nil {
			return err
		}
		to := filepath.Join(dst, name)
		if existing, err := os.ReadFile(to); err == nil && bytes.Equal(existing, body) {
			continue
		} else if err == nil {
			return fmt.Errorf("%s already exists in testdata with different content: a capture is the record of a run that cannot be repeated, so it is not overwritten", rel(filepath.Dir(dst), to))
		}
		if check {
			return fmt.Errorf("%s is missing or out of date in testdata", name)
		}
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(to, body, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// write puts body at path and reports whether it had to change anything. Saying
// nothing when the file is already right is the point: a generator that
// rewrites on every run makes every commit that touches it noisy, and -check has
// to be able to say "all in date" by printing nothing at all.
func write(path, body string, check bool) (bool, error) {
	existing, err := os.ReadFile(path)
	if err == nil && bytes.Equal(existing, []byte(body)) {
		return false, nil
	}
	if check {
		return true, fmt.Errorf("%s is missing or out of date", rel(filepath.Dir(path), path))
	}
	return true, os.WriteFile(path, []byte(body), 0o644)
}

// testBody is the whole generated test. It carries no knowledge of the capture:
// which side is the original, which flags the server ran with and what is
// compared all come out of the capture directory at run time.
func testBody(name string) string {
	return fmt.Sprintf(`// Code generated by cmd/gensim. DO NOT EDIT.
//
// Capture %s. The comparison, the two verdict levels and the rules are all in
// internal/sim; this function only says which directory to replay.

package sim_test

import (
	"testing"

	"github.com/luskaner/ageBattleServer/internal/sim"
)

func TestSim_%s(t *testing.T) {
	sim.Run(t, %q)
}
`, name, ident(name), filepath.ToSlash(filepath.Join(testDataDirName, name)))
}

// testFileName is sim_<capture>_test.go, with the capture name reduced to
// something a Go file may be called.
func testFileName(capture string) string { return "sim_" + ident(capture) + "_test.go" }

// ident makes a capture directory name usable as a Go identifier. The proxy
// names captures <date>_<time>_<commit> (cmd/proxy/CaptureDir), and '-' is
// not legal in an identifier, so every character that is not a letter or a digit
// becomes an underscore.
// installedCaptures lists the capture directories already sitting in testdata,
// which is what makes a generated test non-orphan even when it was not part of
// this run's search path.
func installedCaptures(testdata string) ([]string, error) {
	entries, err := os.ReadDir(testdata)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(testdata, e.Name(), "metadata.json")); err != nil {
			continue // not a capture, just a directory that looks like one
		}
		names = append(names, e.Name())
	}
	return names, nil
}

func ident(capture string) string {
	var b strings.Builder
	for _, r := range capture {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		return "captura"
	}
	// An identifier may not start with a digit.
	if out[0] >= '0' && out[0] <= '9' {
		return "c" + out
	}
	return out
}

// existingGenerated lists the generated test files in the sim package, and only
// those: the marker in the first line is what makes a file safe to delete.
func existingGenerated(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "sim_") || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !strings.Contains(string(body), "Code generated by cmd/gensim") {
			continue
		}
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// captureOf is the inverse of testFileName, for finding which capture a
// generated test belongs to.
func captureOf(file string) string {
	return strings.TrimSuffix(strings.TrimPrefix(file, "sim_"), "_test.go")
}

// exists reports whether p is present, of any kind. A capture is recognised by its
// metadata.json, which is a file, so this is deliberately not isDir.
func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func errorsIsNotExist(err error) bool { return os.IsNotExist(err) }

func rel(base, path string) string {
	if r, err := filepath.Rel(base, path); err == nil {
		return r
	}
	return path
}

func verb(check bool) string {
	if check {
		return "out of date:"
	}
	return "wrote:"
}
