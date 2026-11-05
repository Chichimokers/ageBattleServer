package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunRejectsIdentifierCollisions covers N-31: "a-b" and "a_b" both reduce to
// the same generated test file name, so one capture's test would overwrite the
// other's in silence.
func TestRunRejectsIdentifierCollisions(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a-b", "a_b"} {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := t.TempDir()
	err := run([]string{root}, out, true)
	if err == nil {
		t.Fatal("run accepted two captures with the same generated identifier")
	}
	if !strings.Contains(err.Error(), "a-b") || !strings.Contains(err.Error(), "a_b") {
		t.Errorf("the error does not name both captures: %v", err)
	}
}
