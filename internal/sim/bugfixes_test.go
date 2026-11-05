package sim

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSplitArgKnowsBooleanFlags covers N-34 and N-35: a boolean flag takes no
// value, so the token after it is still a flag, not its value.
func TestSplitArgKnowsBooleanFlags(t *testing.T) {
	args := []string{"-enableStats", "-bsPort", "14001"}
	name, value, hasValue, next := splitArg(args, 0)
	if name != "enablestats" || hasValue || value != "" || next != 0 {
		t.Fatalf("splitArg(-enableStats) = %q %q %v next=%d, want enablestats/empty/false/0", name, value, hasValue, next)
	}
	ports := serverPortsFromArgs(args)
	if !ports[14001] {
		t.Errorf("serverPortsFromArgs(%v) = %v, want 14001", args, ports)
	}
	if err := checkHelpFlags([]string{"-enableStats", "-help"}); err == nil {
		t.Error("checkHelpFlags did not see -help behind a boolean flag")
	}
	if err := checkUsageExits("0", []string{"-enableStats", "-simulationPeriod", "0", "-observerSocketSkipMS", "8"}); err == nil {
		t.Error("checkUsageExits did not see the observer flag behind a boolean flag")
	}
	if _, _, err := ConfigFromArgs([]string{"-enableStats", "-help"}); err == nil {
		t.Error("ConfigFromArgs accepted -help behind a boolean flag")
	}
}

// TestBooleanFlagListCoversTheConfigFlags keeps the harness's hand-written list
// honest: a new boolean flag in internal/config whose name is missing from
// isBoolFlagName would make splitArg consume the next token again, which is the
// N-34 bug coming back in silence.
func TestBooleanFlagListCoversTheConfigFlags(t *testing.T) {
	_, boolFlags, err := knownFlagNames()
	if err != nil {
		t.Fatalf("knownFlagNames: %v", err)
	}
	for name := range boolFlags {
		if !isBoolFlagName(name) {
			t.Errorf("boolean flag %q is registered in internal/config but isBoolFlagName does not know it", name)
		}
	}
}

// TestTrimRejectsFormatV2WithoutRecord covers N-36: the trim record is
// dereferenced only after checking it is there.
func TestTrimRejectsFormatV2WithoutRecord(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, metadataName), []byte(`{"format_version":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Trim(src, filepath.Join(t.TempDir(), "out"), TrimOptions{Keep: time.Second, Source: "x", Why: "test"})
	if err == nil {
		t.Fatal("Trim accepted format_version 2 with no trim record")
	}
	if !strings.Contains(err.Error(), "no trim record") {
		t.Errorf("error does not name the missing record: %v", err)
	}
}

// TestLoadLogRejectsUnparseableTimestamp covers N-37: a row whose clock cannot
// be read used to be scheduled at t=0 in silence.
func TestLoadLogRejectsUnparseableTimestamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "main.log")
	body := "timestamp,src_host,src_port,dest_host,dest_port,msg_type,type_name,body_len,body_hex,data_b64\n" +
		"not-a-time,127.0.0.1,51001,127.0.0.1,14001,0x15,LobbyConnect,2,1500,AA==\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadLog(path, map[int]bool{14001: true}); err == nil {
		t.Fatal("loadLog accepted a row with an unparseable timestamp")
	}
}
