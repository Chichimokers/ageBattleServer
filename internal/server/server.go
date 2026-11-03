package server

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/luskaner/ageBattleServer/internal/logger"
	"github.com/luskaner/ageBattleServer/internal/metrics"
	"github.com/luskaner/ageBattleServer/internal/session"
)

const version = "2.58.0"

type Config struct {
	Name                string
	BsPort              uint
	WebSocketPort       uint
	OutOfBandPort       uint
	RelayBroadcastPorts []uint
	LobbyHost           string
	LobbyPort           uint
	PublicIP            string
	PublicPort          uint
	SSLCert             string
	SSLKey              string
	Region              string
	// ObserverMax is the maximum number of observers per session. It is enforced in
	// the StartObserveRequest handler (see Session.StartObserve): the observer is
	// only accepted when max>0, the session is reconnectable and the observer list
	// is below the cap; the client is then answered with a StartObserveResponse,
	// marked as observer (which throttles its socket processing, see
	// ObserverSocketSkipMS) and added to the observer list, otherwise it gets a
	// rejection status.
	ObserverMax uint
	// ObserverUpdateIntervalMS is the observer update interval. It is handed to the
	// session and is the stride the observer update walks the observer list with,
	// once per simulation tick while the session is reconnectable and has
	// observers: a persistent counter advances by 1 each tick and wraps when it
	// exceeds the interval (period = interval+1); each tick the observers at
	// indices congruent with the counter modulo the interval receive up to 200
	// frames of the store plus an EndOfGameDataMessage (type 34) when finished.
	//
	// It is a TICKS value, and the flag's 8 is only ever used as such when the
	// operator sets it: the server starts the field at 8 ticks and only converts
	// the flag from milliseconds when the flag was given.
	// ObserverUpdateIntervalSet records which of the two happened, because 8 ms at
	// a 125 ms period is 0 ticks and the default is not that.
	ObserverUpdateIntervalMS uint
	// ObserverUpdateIntervalSet records that -observerUpdateIntervalMS was on the
	// command line, which is what makes its value override the 8-tick default.
	ObserverUpdateIntervalSet bool
	// ObsUpdateMaxLoadPct is a setting with no consumer on any live path: the two
	// places that would read it store the result in fields nothing reads.
	ObsUpdateMaxLoadPct uint
	// ObserverSocketSkipMS is the socket processing skip for observer clients,
	// converted from ms to ticks. The whole socket processing is skipped for an
	// observer client that has no pending data while a per-client counter is below
	// the skip value, the counter is reset and the client is processed otherwise.
	// Incoming WS messages are queued per client and processed with the skip
	// applied.
	ObserverSocketSkipMS uint
	SimulationPeriod     float64
	// MaxPagesPerMatch is the operator's -maxPagesToStorePerSession as the
	// frame store's BLOCK count: the session keeps ceil(peers * value/8) blocks
	// of PageSize bytes. 0 is the default, so 8 blocks; the store is always
	// finite, and when a new block is needed and they are all full the oldest is
	// dropped.
	MaxPagesPerMatch uint
	// MaxSessions caps the number of concurrent sessions (matches). When the
	// cap is reached new matches are denied. 0 = unlimited.
	MaxSessions          uint
	PageSize             uint
	AllowTimeoutOverride bool
	HeartbeatSecurityKey string
	ClusterName          string
	EnableStats          bool
	MuninLog             string
	MuninReportDelay     uint
	LogLevel             string
	ServerLog            string
	LogFileCount         uint
	LogFileSize          uint
	// ReplayDataHandler, when non-nil, receives every replay data chunk (the
	// proxy 0x10 message body) as it arrives, together with the session ID of
	// the sending peer when known. The data slice is copied before the call, so
	// the handler may retain it. It runs synchronously on the sender's
	// connection goroutine; keep it fast or fan out to your own goroutines.
	ReplayDataHandler func(sessionID uint64, data []byte)
	// EphemeralListenPorts names the game transports whose listen port is assigned
	// by the operating system at bind time instead of being taken from BsPort and
	// WebSocketPort: each named transport is created even when its configured port
	// is 0, it binds ":0", and the port it got is read back with Server.BoundPorts.
	//
	// It is NOT a command-line flag and no flag sets it: "-bsPort 0" means "this
	// transport is off" and has to keep meaning that. It exists for a caller that
	// starts a server on ports it may not choose and cannot keep: internal/sim
	// starts one server per capture replay, and picking a free port by binding it
	// and letting go leaves the port unowned until the server binds it, which on
	// Windows loses the race (a listener bind is exclusive) and kills the replay.
	// Asking at bind time removes the window: the port is never free.
	//
	// It is a set and not a single switch because naming one transport says which
	// ones are on, and a test that exercises only the WebSocket listener must not
	// silently acquire a bs listener because it asked for an unknown port.
	//
	// The set is not a portability distinction, and there is a stash in this
	// repository's history that said it was, so the answer is written here. Asking
	// the OS at bind time (":0") is the same mechanism on every platform; the one
	// platform fact in this whole comment is that a Windows listener bind is
	// exclusive, which is why a port picked by binding it and letting go is a
	// liability HERE and not a portability question. What a single bool would
	// change is the meaning of "this transport is off": with a switch, a caller
	// that wants an unknown webSocketPort also gets a bs listener it never asked
	// for, on a port it is not going to read, and a test that was measuring one
	// transport ends up measuring two. That is a correctness regression on every
	// platform, so the bool does not survive on its merits.
	EphemeralListenPorts EphemeralPorts
}

// EphemeralPorts is the set of game transports that take an OS-assigned listen
// port. See Config.EphemeralListenPorts for why it exists.
type EphemeralPorts uint8

const (
	// EphemeralNone names no transport: every listen port comes from the config.
	EphemeralNone EphemeralPorts = 0
	// EphemeralBS is the bsPort (RLink) transport.
	EphemeralBS EphemeralPorts = 1 << 0
	// EphemeralWS is the webSocketPort transport.
	EphemeralWS EphemeralPorts = 1 << 1
	// EphemeralBoth is every game transport, which is what a replay of a capture
	// with both asks for.
	EphemeralBoth EphemeralPorts = EphemeralBS | EphemeralWS
	// EphemeralOOB is the outOfBandPort listener. It is not a game transport and
	// EphemeralBoth leaves it out: a capture replay never asks for it, a test of
	// the ping responder does.
	EphemeralOOB EphemeralPorts = 1 << 2
)

// Wants reports whether the transport is named in the set.
func (p EphemeralPorts) Wants(want EphemeralPorts) bool { return p&want != 0 }

type Server struct {
	config    *Config
	startTime time.Time
	running   atomic.Bool
	mu        sync.RWMutex

	bsListener       *BsSocketListener
	wsListener       *WsSocketListener
	oobListener      *PingServer
	relayBroadcaster *RelayBroadcaster
	lobbyClient      *LobbyClient

	sessionManager   *session.Manager
	metricsCollector *metrics.Collector

	clients []ClientConn

	// clientSet is the O(1) live-client membership set for the relay path
	// (isLiveClient, through sendToPeer's liveOnly delivery). Keys are
	// identity-based: a ClientConn stored at AddClient is looked up through the
	// session.PeerConnection interface the peers carry, so the map uses `any`
	// and relies on the underlying pointer identity (same idiom as the old
	// `any(cli) == any(conn)` comparisons).
	clientSet map[any]struct{}

	// clientCount mirrors len(clients) for the metrics gauge: the read must not
	// take s.mu (a write lock) on every collection, so it is an atomic counter
	// updated where the slice changes.
	clientCount atomic.Int64

	// workCh carries clients that signaled pending work so the main loop can
	// dispatch their queued messages on demand instead of polling at 1ms. The
	// buffered sends are non-blocking; the coarse safety net (processClients on
	// the stats ticker) drains anything whose signal was dropped.
	workCh chan ClientConn

	maxPacketsPerCycle int

	// Snapshots reused by the main loop (single goroutine) so the per-cycle
	// client/session walks don't allocate a fresh slice every tick.
	clientSnap []ClientConn
	sessSnap   []*session.Session

	// observerSessions tracks which session each observing client watches, so the
	// ActivateObserver/StopObserve messages can be routed without scanning all
	// sessions. The cap itself lives per session (session.MaxObservers).
	observerSessions map[ClientConn]*session.Session

	// pingEchoBudget is the remaining 0x14 LOBBYPING echo allowance for each
	// client that has not joined a session yet. It starts at 100 and is
	// decremented on every echo, and it is only consulted while the client has no
	// session. A client in a session is echoed unconditionally, so the budget
	// never applies to it. Entries are removed with the client.
	pingEchoBudget map[ClientConn]int

	muninFile   *os.File
	muninWriter *bufio.Writer
	muninTicker *time.Ticker
	muninMu     sync.Mutex
}

func New(cfg *Config) (*Server, error) {
	s := &Server{
		config:             cfg,
		startTime:          time.Now(),
		maxPacketsPerCycle: 100, // processed packets per work-signal processing cycle
		observerSessions:   make(map[ClientConn]*session.Session),
		pingEchoBudget:     make(map[ClientConn]int),
		clientSet:          make(map[any]struct{}),
		workCh:             make(chan ClientConn, 256),
	}

	s.metricsCollector = metrics.NewCollector()
	s.sessionManager = session.NewManagerWithPageSize(cfg.MaxPagesPerMatch, cfg.MaxSessions, cfg.PageSize)

	if cfg.OutOfBandPort > 0 || cfg.EphemeralListenPorts.Wants(EphemeralOOB) {
		s.oobListener = NewPingServer(cfg.OutOfBandPort, cfg.SSLCert, cfg.SSLKey)
	}

	if cfg.BsPort > 0 || cfg.EphemeralListenPorts.Wants(EphemeralBS) {
		s.bsListener = NewBsSocketListener(s, s.metricsCollector, cfg.BsPort)
	}

	if cfg.WebSocketPort > 0 || cfg.EphemeralListenPorts.Wants(EphemeralWS) {
		ws, err := NewWsSocketListener(s, s.metricsCollector, s.observerSocketSkipTicks(), cfg.WebSocketPort, cfg.SSLCert, cfg.SSLKey)
		if err != nil {
			return nil, err
		}
		s.wsListener = ws
	}

	if cfg.LobbyPort > 0 {
		s.lobbyClient = NewLobbyClient(cfg, cfg.LobbyHost, cfg.LobbyPort)
		logger.Info("Server", "New", "lobby enabled, connecting to %s:%d", cfg.LobbyHost, cfg.LobbyPort)
	}

	if cfg.PublicIP != "" {
		logger.Info("Server", "New", "publicIP=%s (informational only, not used by the protocol)", cfg.PublicIP)
	}

	if cfg.ObsUpdateMaxLoadPct > 0 {
		logger.Info("Server", "New", "obsUpdateMaxLoadPct=%d (informational only, not used by the protocol)", cfg.ObsUpdateMaxLoadPct)
	}

	if cfg.PageSize > 0 {
		logger.Info("Server", "New", "pageSize=%d (the frame store's block size: maxPagesPerMatch blocks of this many bytes are what a late observer can still be served)", cfg.PageSize)
	}

	if cfg.MaxPagesPerMatch > 0 {
		logger.Info("Server", "New", "maxPagesPerMatch=%d (frame store blocks per match, scaled by peers/8; the oldest block is dropped once they are all full)", cfg.MaxPagesPerMatch)
	}

	if cfg.MaxSessions > 0 {
		logger.Info("Server", "New", "maxSessions=%d (concurrent match cap; new matches denied once reached)", cfg.MaxSessions)
	}

	if len(cfg.RelayBroadcastPorts) > 0 {
		s.relayBroadcaster = NewRelayBroadcaster(s.buildBroadcastMessages, cfg.RelayBroadcastPorts)
	}

	// Munin only activates when explicitly enabled AND a log path is given:
	// -muninLog alone no longer turns it on, matching the flag contract.
	if cfg.MuninLog != "" && cfg.EnableStats {
		if err := s.initMunin(); err != nil {
			return nil, fmt.Errorf("init munin: %w", err)
		}
	}

	return s, nil
}

func (s *Server) Run(ctx context.Context) error {
	if err := s.Start(ctx); err != nil {
		return err
	}

	<-ctx.Done()
	return s.Stop()
}

func (s *Server) Start(ctx context.Context) error {
	s.running.Store(true)

	if s.oobListener != nil {
		if err := s.oobListener.Start(ctx); err != nil {
			return fmt.Errorf("ping server: %w", err)
		}
	}

	if s.bsListener != nil {
		if err := s.bsListener.Start(ctx); err != nil {
			return fmt.Errorf("bs listener: %w", err)
		}
	}

	if s.wsListener != nil {
		if err := s.wsListener.Start(ctx); err != nil {
			return fmt.Errorf("ws listener: %w", err)
		}
	}

	if s.relayBroadcaster != nil {
		if err := s.relayBroadcaster.Start(ctx); err != nil {
			return fmt.Errorf("relay broadcaster: %w", err)
		}
	}

	if s.lobbyClient != nil {
		if err := s.lobbyClient.Start(ctx); err != nil {
			return fmt.Errorf("lobby client: %w", err)
		}
	}

	if s.muninTicker != nil {
		go s.muninLoop(ctx)
	}

	go s.mainLoop(ctx)

	logger.Log("Server", "Start", 3, "Battleserver %s running on ports %d,%d,%d", version, s.config.BsPort, s.config.WebSocketPort, s.config.OutOfBandPort)

	return nil
}

func (s *Server) Stop() error {
	s.running.Store(false)

	if s.bsListener != nil {
		s.bsListener.Stop()
	}
	if s.wsListener != nil {
		s.wsListener.Stop()
	}
	if s.oobListener != nil {
		s.oobListener.Stop()
	}
	if s.relayBroadcaster != nil {
		s.relayBroadcaster.Stop()
	}
	if s.lobbyClient != nil {
		s.lobbyClient.Stop()
	}
	if s.muninTicker != nil {
		s.muninTicker.Stop()
	}
	if s.muninWriter != nil {
		s.muninMu.Lock()
		_ = s.muninWriter.Flush()
		s.muninMu.Unlock()
	}
	if s.muninFile != nil {
		_ = s.muninFile.Close()
	}

	s.mu.Lock()
	s.clients = nil
	// clientSet is emptied and NOT set to nil: a connection accepted while Stop
	// runs is handed to AddClient, and writing to a nil map panics. It is the same
	// window a real client has against a server that is going down, and it killed
	// the whole process (not just the connection) the first time it was met.
	clear(s.clientSet)
	s.clientCount.Store(0)
	s.mu.Unlock()

	return nil
}

// BoundPorts are the TCP ports the two game listeners are listening on, and 0 for
// a transport that is off or not started. They are the ports the listeners
// actually bound, not the configured ones, so a caller that asked for
// OS-assigned ports (Config.EphemeralListenPorts) gets the ones to dial from here
// instead of guessing a free one beforehand and hoping to still own it when the
// server binds.
//
// Both values have to be read AFTER Start, and a Start that failed has bound
// nothing this can report.
func (s *Server) BoundPorts() (bs, ws uint) {
	if s.bsListener != nil {
		if p := s.bsListener.GetPort(); p > 0 {
			bs = uint(p)
		}
	}
	if s.wsListener != nil {
		if p := s.wsListener.GetPort(); p > 0 {
			ws = uint(p)
		}
	}
	return bs, ws
}

func (s *Server) mainLoop(ctx context.Context) {
	// Stats are collected on a fixed 5s cadence; the same tick doubles as the
	// coarse idle-disconnect watchdog and as a safety net that drains any client
	// queue whose work signal was dropped (NotifyWork is non-blocking).
	statsTicker := time.NewTicker(5 * time.Second)
	defer statsTicker.Stop()

	// Default simulation period: 125ms (8 ticks/s), overridable via
	// -simulationPeriod.
	pingPeriod := 125 * time.Millisecond
	if s.config.SimulationPeriod > 0 {
		pingPeriod = time.Duration(s.config.SimulationPeriod * float64(time.Millisecond))
	}
	pingTicker := time.NewTicker(pingPeriod)
	defer pingTicker.Stop()

	for s.running.Load() {
		select {
		case <-ctx.Done():
			return
		case c := <-s.workCh:
			// Event-driven processing: the client signaled pending work, so its
			// queued messages are dispatched now instead of on a fixed 1ms tick.
			c.Process(s.maxPacketsPerCycle, time.Now())
		case <-pingTicker.C:
			s.sendEndOfGameDataTicks()
			s.tickSessions()
		case <-statsTicker.C:
			s.collectMetrics()
			s.processClients()
		}
	}
}

func (s *Server) processClients() {
	s.mu.Lock()
	s.clientSnap = snapshotSlice(s.clientSnap, s.clients)
	s.mu.Unlock()

	now := time.Now()
	for _, c := range s.clientSnap {
		if c == nil {
			continue
		}
		c.Process(s.maxPacketsPerCycle, now)
	}
}

// tickSessions runs the per-simulation-tick session update, which delivers
// pages to observers and kicks observers of sessions that are no longer
// available.
func (s *Server) tickSessions() {
	s.sessSnap = s.sessionManager.SnapshotSessions(s.sessSnap[:0])
	for _, sess := range s.sessSnap {
		sess.Tick()
	}
}

func (s *Server) collectMetrics() {
	s.metricsCollector.SetGauge("Clients", float64(s.clientCount.Load()))
	s.metricsCollector.SetGauge("ServerCpuUsage", 0)
}

func (s *Server) AddClient(c ClientConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients = append(s.clients, c)
	s.clientSet[c] = struct{}{}
	s.clientCount.Add(1)
	s.metricsCollector.AddToCounter("TcpConnections", 1)
}

// NotifyWork tells the main loop that c has queued messages to process. The
// send is non-blocking so a full workCh cannot stall a client's read goroutine;
// the leftover re-signal in WsClient.Process and the coarse processClients
// safety net guarantee every frame is eventually drained.
func (s *Server) NotifyWork(c ClientConn) {
	select {
	case s.workCh <- c:
	default:
	}
}

// RemoveClient is the close path of a connection: the transport calls it once
// the socket is gone. It is the only path that takes a peer out of its session.
//
// The connection's peer leaves its session as soon as the close is seen, with
// its slot, its game-data queue record and its checksum vector, so the players
// left are no longer held up by a peer that will never send again. When the
// session is recording and no member left is in the game, the match is over.
// The session itself is erased only once it has no member and no observer left:
// while somebody watches, the observer step serves the rest of the store and the
// end-of-game marker. See session.Manager.HandleDisconnect. A LeaveSimulation
// (0x1F) does NOT remove the peer; see Manager.HandleLeaveSimulation.
//
// The session work runs after the server lock is released: it takes the manager
// and session locks, and nothing here needs the client list any more.
func (s *Server) RemoveClient(c ClientConn) {
	s.mu.Lock()
	for i, cli := range s.clients {
		if cli == c {
			s.clients[i] = s.clients[len(s.clients)-1]
			s.clients = s.clients[:len(s.clients)-1]
			break
		}
	}
	delete(s.clientSet, c)
	s.clientCount.Add(-1)
	observed := s.observerSessions[c]
	delete(s.observerSessions, c)
	delete(s.pingEchoBudget, c)
	s.mu.Unlock()

	s.sessionManager.HandleDisconnect(c, observed)
}

// isLiveClient reports whether conn is still a registered client. It replaces
// the removed Server.peers routing map: relays are only delivered to peers whose
// connection is live. A closed connection leaves the client list before its peer
// leaves the session, so this is what keeps a relay off a connection in that
// window. The clientSet membership check makes it O(1) regardless of the client
// count.
func (s *Server) isLiveClient(conn session.PeerConnection) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.clientSet[conn]
	return ok
}

// sendToPeer writes msgType+body to the peer's connection when it implements
// messageSender and, when liveOnly is set, is still a registered client. It
// returns false when the message was not delivered.
func (s *Server) sendToPeer(conn session.PeerConnection, msgType byte, body []byte, liveOnly bool) bool {
	cc, ok := conn.(messageSender)
	if !ok {
		return false
	}
	if liveOnly && !s.isLiveClient(conn) {
		return false
	}
	_ = cc.SendMessage(msgType, body)
	return true
}

func (s *Server) Config() *Config {
	return s.config
}

func (s *Server) SessionManager() *session.Manager {
	return s.sessionManager
}

func (s *Server) MetricsCollector() *metrics.Collector {
	return s.metricsCollector
}

type ClientConn interface {
	Send(data []byte) error
	SendMessage(msgType byte, body []byte) error
	Close() error
	RemoteAddr() string
	Process(maxPackets int, now time.Time)
	OverrideSilentTimeout(d time.Duration)
	SetObserver(v bool)
	IsObserver() bool
	// SetPeerIdentifier/PeerIdentifier are the connection's observer identity, the
	// u64 the observer list is keyed on (session.PeerConnection has the argument
	// and the addresses).
	SetPeerIdentifier(v uint64)
	PeerIdentifier() uint64
	// setClientMessageVersion/ClientMessageVersion are the connection's announced
	// client message version, written from the LobbyConnect and read by the
	// observer end-of-game marker (session.PeerConnection has the addresses).
	setClientMessageVersion(v uint16)
	ClientMessageVersion() uint16
}

// messageSender is the transport-specific framed sender used when a session
// peer must be written back to without reaching into the full ClientConn type.
type messageSender interface {
	SendMessage(msgType byte, body []byte) error
}

// ClientHost is the server-side surface a transport client needs: message
// dispatch and client registry removal. Transport components (Client, WsClient,
// and the socket listeners) depend on it instead of the concrete Server.
type ClientHost interface {
	AddClient(c ClientConn)
	RemoveClient(c ClientConn)
	// NotifyWork tells the host that c has queued messages ready to process.
	NotifyWork(c ClientConn)
	dispatchMessage(client messageClient, msgType byte, body []byte)
}

// metricsCounter is the narrow counter surface the WebSocket handler uses.
type metricsCounter interface {
	AddToCounter(name string, value float64)
}

type messageClient interface {
	ClientConn
	pingStarted() bool
	setPingStarted()
	setTimeoutOverride(v uint32)
	lobbyPingTick(now time.Time, counter uint32)
}
