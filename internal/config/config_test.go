package config

import (
	"flag"
	"os"
	"reflect"
	"strings"
	"testing"
)

func resetFlags() {
	flag.CommandLine = flag.NewFlagSet("test", flag.ContinueOnError)
}

func TestNormalizeArgs(t *testing.T) {
	args := normalizeArgs([]string{"-BSport", "-PublicIP", "127.0.0.1", "-name", "Srv"})
	want := []string{"-bsport", "-publicip", "127.0.0.1", "-name", "Srv"}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("normalizeArgs = %v, want %v", args, want)
	}

	args = normalizeArgs([]string{"--bsport", "27012", "-UploadVia", "lobby"})
	want = []string{"-bsport", "27012", "-uploadvia", "lobby"}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("normalizeArgs = %v, want %v", args, want)
	}

	args = normalizeArgs([]string{"-a", "-B", "--", "-C"})
	want = []string{"-a", "-b", "--", "-C"}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("normalizeArgs = %v, want %v", args, want)
	}

	// The = value is case-sensitive and must survive intact.
	args = normalizeArgs([]string{"-HeartBeatSecurityKey=MySecret", "-BindIP=2001:0DB8", "-UploadVia=lobby"})
	want = []string{"-heartbeatsecuritykey=MySecret", "-bindip=2001:0DB8", "-uploadvia=lobby"}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("normalizeArgs = %v, want %v", args, want)
	}

	args = normalizeArgs([]string{"-Flag="})
	want = []string{"-flag="}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("normalizeArgs = %v, want %v", args, want)
	}

	args = normalizeArgs(nil)
	if args == nil || len(args) != 0 {
		t.Errorf("normalizeArgs(nil) = %v, want empty slice", args)
	}
}

// TestParseFlagsAcceptsDoubleDashAndDashValues covers the two command-line
// normalizations: --Flag is the same flag as -Flag (both case-insensitive), and
// a value that starts with "-" is a value, not the next flag.
func TestParseFlagsAcceptsDoubleDashAndDashValues(t *testing.T) {
	resetFlags()
	os.Args = []string{"battleserver", "--bsPort", "23013", "-name", "-MyServer", "-uploadVia=-db"}
	cfg := ParseFlags()
	if cfg.BsPort != 23013 {
		t.Errorf("--bsPort 23013 gave BsPort = %d, want 23013", cfg.BsPort)
	}
	if cfg.Name != "-MyServer" {
		t.Errorf("-name -MyServer gave Name = %q, want %q", cfg.Name, "-MyServer")
	}
}

// TestParseFlagsAcceptsOriginalFlagNames pins the drop-in contract: every flag
// the original BattleServer.exe accepts must be registered here too, with a
// name that survives normalizeArgs (which lowercases it). Flag parsing is
// strict, so a name we renamed or dropped would make this server exit 2 where
// the real binary starts. The list is the original's full set, read off the
// `_stricmp(v48, ...)` chain in its `main`.
func TestParseFlagsAcceptsOriginalFlagNames(t *testing.T) {
	original := []string{
		"allowTimeoutOverride", "bsPort", "clusterName", "heartbeatSecurityKey",
		"lobbyHost", "lobbyPort", "logFileCount", "logFileSize", "login",
		"maxPagesToStorePerSession", "muninLog", "muninReportDelay", "name",
		"observerMax", "observerSocketSkipMS", "observerUpdateIntervalMS",
		"obsUpdateMaxLoadPct", "odbcDriver", "outOfBandPort", "pageSize",
		"password", "publicIP", "publicPort", "region", "relayBroadcastPort",
		"replayDBName", "replayHost", "replayPort", "serverLog",
		"simulationPeriod", "sslCert", "sslKey", "uploadVia", "webSocketPort",
	}

	resetFlags()
	os.Args = []string{"battleserver"}
	ParseFlags()

	for _, name := range original {
		normalized := strings.ToLower(name)
		if flag.CommandLine.Lookup(normalized) == nil {
			t.Errorf("the original accepts -%s but no such flag is registered here; "+
				"a launcher passing it would start the real binary and get exit 2 from this one", name)
		}
	}
}

// The original spells the page cap -maxPagesToStorePerSession; this server also
// answers to -maxPagesPerMatch. Both must land on the same field.
func TestParseFlagsPageCapAlias(t *testing.T) {
	for _, name := range []string{"maxPagesToStorePerSession", "maxPagesPerMatch"} {
		t.Run(name, func(t *testing.T) {
			resetFlags()
			os.Args = []string{"battleserver", "-" + name + "=7"}
			cfg := ParseFlags()
			if cfg.MaxPagesPerMatch != 7 {
				t.Errorf("-%s=7 gave MaxPagesPerMatch = %d, want 7", name, cfg.MaxPagesPerMatch)
			}
		})
	}
}

func TestParseFlagsDefaults(t *testing.T) {
	resetFlags()
	os.Args = []string{"battleserver"}
	cfg := ParseFlags()

	if cfg.Name != "My Server" {
		t.Errorf("Name = %q, want %q", cfg.Name, "My Server")
	}
	if cfg.BsPort != 27012 {
		t.Errorf("BsPort = %d, want 27012", cfg.BsPort)
	}
	if cfg.OutOfBandPort != 27212 {
		t.Errorf("OutOfBandPort = %d, want 27212", cfg.OutOfBandPort)
	}
	if cfg.ObserverMax != 512 {
		t.Errorf("ObserverMax = %d, want 512", cfg.ObserverMax)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
	}
	if cfg.Region != "global" {
		t.Errorf("Region = %q, want global", cfg.Region)
	}
	if !reflect.DeepEqual(cfg.RelayBroadcastPorts, []uint{9999, 8888}) {
		t.Errorf("RelayBroadcastPorts = %v, want [9999 8888]", cfg.RelayBroadcastPorts)
	}
	if cfg.SimulationPeriod != 0 {
		t.Errorf("SimulationPeriod = %v, want 0", cfg.SimulationPeriod)
	}
}

func TestParseFlagsValues(t *testing.T) {
	resetFlags()
	os.Args = []string{"battleserver",
		"-name", "TestSrv",
		"-bsport", "12345",
		"-websocketport", "8080",
		"-outofbandport", "0",
		"-relaybroadcastport", "9998",
		"-lobbyhost", "lobby.example.com",
		"-lobbyport", "1234",
		"-publicip", "1.2.3.4",
		"-publicport", "5678",
		"-sslcert", "/tmp/cert.pem",
		"-sslkey", "/tmp/key.pem",
		"-uploadvia", "lobby",
		"-replayhost", "replay.example.com",
		"-replayport", "7777",
		"-login", "user",
		"-password", "pass",
		"-replaydbname", "db1",
		"-odbcdriver", "driver1",
		"-region", "eu",
		"-observermax", "64",
		"-observerupdateintervalms", "16",
		"-obsupdatemaxloadpct", "50",
		"-observersocketskipms", "16",
		"-simulationperiod", "16.5",
		"-maxpagespermatch", "4",
		"-maxsessions", "10",
		"-pagesize", "32768",
		"-allowtimeoutoverride",
		"-heartbeatsecuritykey", "hkey",
		"-clustername", "cluster1",
		"-enablestats",
		"-muninlog", "/tmp/munin.log",
		"-muninreportdelay", "1000",
		"-loglevel", "info",
		"-serverlog", "/tmp/server.log",
		"-logfilecount", "5",
		"-logfilesize", "1024",
	}
	cfg := ParseFlags()

	if cfg.Name != "TestSrv" {
		t.Errorf("Name = %q, want TestSrv", cfg.Name)
	}
	if cfg.BsPort != 12345 {
		t.Errorf("BsPort = %d, want 12345", cfg.BsPort)
	}
	if cfg.WebSocketPort != 8080 {
		t.Errorf("WebSocketPort = %d, want 8080", cfg.WebSocketPort)
	}
	if cfg.OutOfBandPort != 0 {
		t.Errorf("OutOfBandPort = %d, want 0", cfg.OutOfBandPort)
	}
	if !reflect.DeepEqual(cfg.RelayBroadcastPorts, []uint{9998}) {
		t.Errorf("RelayBroadcastPorts = %v, want [9998]", cfg.RelayBroadcastPorts)
	}
	if cfg.LobbyHost != "lobby.example.com" {
		t.Errorf("LobbyHost = %q, want lobby.example.com", cfg.LobbyHost)
	}
	if cfg.LobbyPort != 1234 {
		t.Errorf("LobbyPort = %d, want 1234", cfg.LobbyPort)
	}
	if cfg.PublicIP != "1.2.3.4" {
		t.Errorf("PublicIP = %q, want 1.2.3.4", cfg.PublicIP)
	}
	if cfg.PublicPort != 5678 {
		t.Errorf("PublicPort = %d, want 5678", cfg.PublicPort)
	}
	if cfg.SSLCert != "/tmp/cert.pem" {
		t.Errorf("SSLCert = %q, want /tmp/cert.pem", cfg.SSLCert)
	}
	if cfg.SSLKey != "/tmp/key.pem" {
		t.Errorf("SSLKey = %q, want /tmp/key.pem", cfg.SSLKey)
	}
	// The replay flags are parsed and deliberately discarded: the original's
	// arguments stay accepted, but there is no upload to configure.
	if cfg.Region != "eu" {
		t.Errorf("Region = %q, want eu", cfg.Region)
	}
	if cfg.ObserverMax != 64 {
		t.Errorf("ObserverMax = %d, want 64", cfg.ObserverMax)
	}
	if cfg.ObserverUpdateIntervalMS != 16 {
		t.Errorf("ObserverUpdateIntervalMS = %d, want 16", cfg.ObserverUpdateIntervalMS)
	}
	if cfg.ObsUpdateMaxLoadPct != 50 {
		t.Errorf("ObsUpdateMaxLoadPct = %d, want 50", cfg.ObsUpdateMaxLoadPct)
	}
	if cfg.ObserverSocketSkipMS != 16 {
		t.Errorf("ObserverSocketSkipMS = %d, want 16", cfg.ObserverSocketSkipMS)
	}
	if cfg.SimulationPeriod != 16.5 {
		t.Errorf("SimulationPeriod = %v, want 16.5", cfg.SimulationPeriod)
	}
	if cfg.MaxPagesPerMatch != 4 {
		t.Errorf("MaxPagesPerMatch = %d, want 4", cfg.MaxPagesPerMatch)
	}
	if cfg.MaxSessions != 10 {
		t.Errorf("MaxSessions = %d, want 10", cfg.MaxSessions)
	}
	if cfg.PageSize != 32768 {
		t.Errorf("PageSize = %d, want 32768", cfg.PageSize)
	}
	if !cfg.AllowTimeoutOverride {
		t.Error("AllowTimeoutOverride = false, want true")
	}
	if cfg.HeartbeatSecurityKey != "hkey" {
		t.Errorf("HeartbeatSecurityKey = %q, want hkey", cfg.HeartbeatSecurityKey)
	}
	if cfg.ClusterName != "cluster1" {
		t.Errorf("ClusterName = %q, want cluster1", cfg.ClusterName)
	}
	if !cfg.EnableStats {
		t.Error("EnableStats = false, want true")
	}
	if cfg.MuninLog != "/tmp/munin.log" {
		t.Errorf("MuninLog = %q, want /tmp/munin.log", cfg.MuninLog)
	}
	if cfg.MuninReportDelay != 1000 {
		t.Errorf("MuninReportDelay = %d, want 1000", cfg.MuninReportDelay)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want info", cfg.LogLevel)
	}
	if cfg.ServerLog != "/tmp/server.log" {
		t.Errorf("ServerLog = %q, want /tmp/server.log", cfg.ServerLog)
	}
	if cfg.LogFileCount != 5 || cfg.LogFileSize != 1024 {
		t.Errorf("LogFileCount/LogFileSize = %d/%d, want 5/1024", cfg.LogFileCount, cfg.LogFileSize)
	}
}

// TestParseFlagsAcceptsAnyUploadVia: the upload is gone, so -uploadVia has no
// value left to validate. Any value must parse, because a launcher that passes
// the original's own "mysql" or "odbc" must not be refused by a flag that does
// nothing.
func TestParseFlagsAcceptsAnyUploadVia(t *testing.T) {
	for _, value := range []string{"lobby", "mysql", "odbc", "bogus"} {
		t.Run(value, func(t *testing.T) {
			resetFlags()
			os.Args = []string{"battleserver", "-uploadvia", value}
			cfg := ParseFlags()
			if cfg.Name != "My Server" {
				t.Errorf("Name = %q, want the default: the rest of the config must be unaffected", cfg.Name)
			}
		})
	}
}

func TestParseFlagsRelayBroadcastDisabled(t *testing.T) {
	resetFlags()
	os.Args = []string{"battleserver", "-relaybroadcastport", "0"}
	cfg := ParseFlags()
	if len(cfg.RelayBroadcastPorts) != 0 {
		t.Errorf("RelayBroadcastPorts = %v, want empty (disabled)", cfg.RelayBroadcastPorts)
	}
}

func TestParseFlagsSimulationPeriodOK(t *testing.T) {
	resetFlags()
	os.Args = []string{"battleserver", "-observersocketskipms", "8", "-simulationperiod", "16"}
	cfg := ParseFlags()
	if cfg.SimulationPeriod != 16 {
		t.Errorf("SimulationPeriod = %v, want 16", cfg.SimulationPeriod)
	}
}

func TestPrintUsage(t *testing.T) {
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe error: %v", err)
	}
	os.Stdout = w

	printUsage()
	_ = w.Close()
	os.Stdout = old

	buf := make([]byte, 8192)
	n, _ := r.Read(buf)
	_ = r.Close()
	out := string(buf[:n])

	for _, want := range []string{
		"Usage: battleserver", "Network Options:", "Session Options:",
		"Logging Options:", "-help, -h, -?",
		// The original's replay arguments stay listed, even though the upload
		// is gone: a launcher reads the help to decide what it may pass.
		"-uploadVia", "-replayHost", "-replayPort", "-login", "-password",
		"-replayDBName", "-odbcDriver", "accepted and ignored",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("usage output missing %q", want)
		}
	}
}
