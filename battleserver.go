// Package battleserver exposes the ageLAN BattleServer as an embeddable
// library. A host program can construct a Config, start the server with
// Run/Start, and receive recorded game data through a callback.
package battleserver

import (
	"context"
	"fmt"
	"strconv"

	"github.com/luskaner/ageBattleServer/internal/defaults"
	"github.com/luskaner/ageBattleServer/internal/logger"
	"github.com/luskaner/ageBattleServer/internal/server"
)

// ReplayData is a single chunk of recorded game data as delivered by the
// server's replay path.
type ReplayData struct {
	// SessionID is the match/session the data belongs to, or 0 when the
	// sending client is not (yet) part of a session.
	SessionID uint64
	// Data is the raw proxy (0x10) message body. The server copies the bytes
	// before invoking the handler, so they may be retained safely.
	Data []byte
}

// ReplayDataHandler receives recorded game data chunks. It runs synchronously
// on the sending connection's goroutine, so it must return quickly; dispatch
// to your own goroutines or channel when processing is expensive.
type ReplayDataHandler func(ReplayData)

// Config mirrors the battleserver command-line options. Start from
// DefaultConfig to get the same defaults as the CLI, then override the fields
// you need. Options that only make sense for a process (the -help flags, the
// deprecated log file options, and the unsupported mysql/odbc credentials) are
// intentionally not exposed here.
type Config struct {
	// Name is the readable server name.
	Name string
	// BsPort is the TCP port for incoming game (bsPort/RLink) connections.
	BsPort uint
	// WebSocketPort is the WebSocket listen port (0 = disabled).
	WebSocketPort uint
	// OutOfBandPort is the WebSocket ping OOB TCP port (0 = disabled).
	OutOfBandPort uint
	// RelayBroadcastPorts are the UDP presence-broadcast ports. Empty by
	// default (broadcast disabled), unlike the CLI which uses [9999 8888].
	RelayBroadcastPorts []uint
	// LobbyHost is the heartbeat/lobby server host.
	LobbyHost string
	// LobbyPort is the heartbeat/lobby server port (0 = disabled).
	LobbyPort uint
	// PublicIP is the server public IP (informational only).
	PublicIP string
	// PublicPort is the advertised public port.
	PublicPort uint
	// SSLCert and SSLKey configure TLS for the listeners.
	SSLCert string
	SSLKey  string
	// Region is the server region advertised to the lobby.
	Region string
	// ObserverMax is the maximum number of observers per session.
	ObserverMax uint
	// ObserverUpdateIntervalMS is the observer update interval in ms.
	ObserverUpdateIntervalMS uint
	// ObsUpdateMaxLoadPct is retained for CLI parity (informational only).
	ObsUpdateMaxLoadPct uint
	// ObserverSocketSkipMS is the socket processing skip for observers in ms.
	ObserverSocketSkipMS uint
	// SimulationPeriod is the simulation period in ms (0 = server default).
	SimulationPeriod float64
	// MaxPagesPerMatch caps replay pages per session (0 = no limit).
	MaxPagesPerMatch uint
	// MaxSessions caps concurrent sessions (0 = no limit).
	MaxSessions uint
	// PageSize is the replay page size (informational only).
	PageSize uint
	// AllowTimeoutOverride allows clients to override the socket timeout.
	AllowTimeoutOverride bool
	// HeartbeatSecurityKey is included in the lobby heartbeat when set.
	HeartbeatSecurityKey string
	// ClusterName identifies the cluster in the lobby heartbeat.
	ClusterName string
	// EnableStats enables Munin stats output.
	EnableStats bool
	// MuninLog is the Munin stats log path.
	MuninLog string
	// MuninReportDelay is the Munin report interval in ms.
	MuninReportDelay uint
	// LogLevel is the global logging level (none, error, warning, info,
	// debug). Empty leaves the current global level unchanged.
	LogLevel string
	// LogModName, when set, overrides the level of a single logger module.
	LogModName string
	// LogModLevel is the per-module level used with LogModName.
	LogModLevel string
	// ReplayDataHandler, when set, receives every replay data chunk internally.
	// It runs on the sending connection's goroutine.
	ReplayDataHandler ReplayDataHandler
	// EphemeralListenPorts asks the operating system for the listen port of each
	// transport it names, instead of taking it from BsPort and WebSocketPort, and
	// Server.BoundPorts reports the ones that were assigned. It exists for a host
	// that cannot choose a port and has to keep it: a port picked by binding ":0"
	// and letting go belongs to nobody in between, and on Windows a listener bind
	// is exclusive, so whatever takes it in that window keeps it and Start fails.
	// It is not a command-line option: BsPort 0 still means that transport is off.
	EphemeralListenPorts server.EphemeralPorts
}

// DefaultConfig returns a Config with the same defaults as the CLI, except
// that RelayBroadcastPorts is empty (broadcast disabled) instead of [9999 8888].
func DefaultConfig() Config {
	bsPort, _ := strconv.ParseUint(defaults.DefaultBsPort, 10, 32)
	publicPort, _ := strconv.ParseUint(defaults.DefaultPublicPort, 10, 32)

	return Config{
		Name:                     "My Server",
		BsPort:                   uint(bsPort),
		OutOfBandPort:            27212,
		LobbyHost:                "localhost",
		PublicPort:               uint(publicPort),
		Region:                   "global",
		ObserverMax:              512,
		ObserverUpdateIntervalMS: 8,
		ObserverSocketSkipMS:     8,
		PageSize:                 65536,
		MuninReportDelay:         60000,
		LogLevel:                 "debug",
		LogModLevel:              "debug",
	}
}

// Server is a running BattleServer instance.
type Server struct {
	inner *server.Server
}

// New builds a Server from cfg without starting it. Logging options in cfg are
// applied to the global logger as the CLI would.
func New(cfg Config) (*Server, error) {
	if cfg.LogLevel != "" {
		level, err := logger.ParseLevel(cfg.LogLevel)
		if err != nil {
			return nil, fmt.Errorf("invalid LogLevel %q: %w", cfg.LogLevel, err)
		}
		logger.SetGlobalLevel(level)
	}
	if cfg.LogModName != "" {
		level, err := logger.ParseLevel(cfg.LogModLevel)
		if err != nil {
			return nil, fmt.Errorf("invalid LogModLevel %q: %w", cfg.LogModLevel, err)
		}
		logger.SetModule(cfg.LogModName, level)
	}

	innerCfg := &server.Config{
		Name:                     cfg.Name,
		BsPort:                   cfg.BsPort,
		WebSocketPort:            cfg.WebSocketPort,
		OutOfBandPort:            cfg.OutOfBandPort,
		RelayBroadcastPorts:      cfg.RelayBroadcastPorts,
		LobbyHost:                cfg.LobbyHost,
		LobbyPort:                cfg.LobbyPort,
		PublicIP:                 cfg.PublicIP,
		PublicPort:               cfg.PublicPort,
		SSLCert:                  cfg.SSLCert,
		SSLKey:                   cfg.SSLKey,
		Region:                   cfg.Region,
		ObserverMax:              cfg.ObserverMax,
		ObserverUpdateIntervalMS: cfg.ObserverUpdateIntervalMS,
		ObsUpdateMaxLoadPct:      cfg.ObsUpdateMaxLoadPct,
		ObserverSocketSkipMS:     cfg.ObserverSocketSkipMS,
		SimulationPeriod:         cfg.SimulationPeriod,
		MaxPagesPerMatch:         cfg.MaxPagesPerMatch,
		MaxSessions:              cfg.MaxSessions,
		PageSize:                 cfg.PageSize,
		AllowTimeoutOverride:     cfg.AllowTimeoutOverride,
		HeartbeatSecurityKey:     cfg.HeartbeatSecurityKey,
		ClusterName:              cfg.ClusterName,
		EnableStats:              cfg.EnableStats,
		MuninLog:                 cfg.MuninLog,
		MuninReportDelay:         cfg.MuninReportDelay,
		LogLevel:                 cfg.LogLevel,
		ServerLog:                "",
		LogFileCount:             0,
		LogFileSize:              0,
		EphemeralListenPorts:     cfg.EphemeralListenPorts,
	}
	if cfg.ReplayDataHandler != nil {
		handler := cfg.ReplayDataHandler
		innerCfg.ReplayDataHandler = func(sessionID uint64, data []byte) {
			handler(ReplayData{SessionID: sessionID, Data: data})
		}
	}

	inner, err := server.New(innerCfg)
	if err != nil {
		return nil, err
	}
	return &Server{inner: inner}, nil
}

// Start launches all configured listeners and background loops. It returns
// once the listeners are up; call Stop to shut them down.
func (s *Server) Start(ctx context.Context) error {
	return s.inner.Start(ctx)
}

// Run starts the server and blocks until ctx is cancelled, then stops it.
func (s *Server) Run(ctx context.Context) error {
	return s.inner.Run(ctx)
}

// Stop shuts down every listener and background loop started by Start.
func (s *Server) Stop() error {
	return s.inner.Stop()
}

// BoundPorts reports the TCP ports the two game listeners are listening on, and
// 0 for a transport that is off. It is the only way to dial a server started
// with Config.EphemeralListenPorts, whose ports are not in the Config, and it has
// to be read after Start.
func (s *Server) BoundPorts() (bs, ws uint) { return s.inner.BoundPorts() }
