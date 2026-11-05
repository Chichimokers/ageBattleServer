package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/luskaner/ageBattleServer/internal/logger"
	"github.com/luskaner/ageBattleServer/internal/protocol"
)

type LobbyClient struct {
	cfg       *Config
	host      string
	port      uint
	conn      net.Conn
	mu        sync.Mutex
	connected bool
	stopped   bool
	interval  time.Duration
}

func NewLobbyClient(cfg *Config, host string, port uint) *LobbyClient {
	return &LobbyClient{
		cfg:      cfg,
		host:     host,
		port:     port,
		interval: 30 * time.Second,
	}
}

func (c *LobbyClient) Start(ctx context.Context) error {
	c.mu.Lock()
	c.stopped = false
	c.mu.Unlock()
	if err := c.connect(); err != nil {
		logger.Warning("Lobby", "connect", "lobby connect: %v (retrying)", err)
	}

	go c.loop(ctx)
	return nil
}

// dialTimeout bounds a single connect attempt so a firewalled lobby host cannot
// hang the loop on the OS-level connect timeout.
const dialTimeout = 5 * time.Second

func (c *LobbyClient) connect() error {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return errors.New("lobby client stopped")
	}
	c.mu.Unlock()

	addr := net.JoinHostPort(c.host, fmt.Sprintf("%d", c.port))
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		return err
	}

	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		_ = conn.Close()
		return errors.New("lobby client stopped")
	}
	if c.conn != nil {
		_ = c.conn.Close()
	}
	c.conn = conn
	c.connected = true
	c.mu.Unlock()

	// Registration is part of connecting: a failed register leaves the socket
	// torn down (Send does this) so the next tick retries the whole connect.
	if err := c.sendConnect(); err != nil {
		return fmt.Errorf("lobby register: %w", err)
	}

	logger.Info("Lobby", "connect", "Connected to lobby %s", addr)
	return nil
}

func (c *LobbyClient) sendConnect() error {
	cfg := c.cfg

	// The registration fields are LobbyConnectMessage's own defaults: titleID 0xFFFF
	// ("not in a title"), sessionID -1 (the server registers itself, not a game),
	// secret unset, no data received, client message version 5, no flags. The body
	// is built with the shared serialiser so its byte order matches
	// protocol.ParseLobbyConnect; it used to be written big-endian here, which
	// made the message unreadable by this very codebase.
	const (
		titleID   = uint16(0xFFFF)
		sessionID = int64(-1)
		secret    = uint64(0xFFFFFFFFFFFFFFFF)
		version   = uint16(5)
		flags     = uint32(0)
	)
	body := protocol.BuildLobbyConnect(titleID, sessionID, secret, 0, 0, version, flags, 0)

	// Trailing registration data. The upstream server appends its cluster name,
	// region and heartbeat key after the fixed fields; the encoding of that tail
	// is not established, and it also means the message is no longer exactly
	// parseable by ParseLobbyConnect (which requires exact consumption).
	// Kept as it was, for wire compatibility with the lobby service.
	if cfg.ClusterName != "" {
		body = append(body, []byte(cfg.ClusterName)...)
	}

	if cfg.Region != "" {
		body = append(body, []byte(cfg.Region)...)
	}

	if cfg.HeartbeatSecurityKey != "" {
		body = append(body, []byte(cfg.HeartbeatSecurityKey)...)
	}

	msg := append([]byte{protocol.MsgLobbyConnect}, body...)
	if err := c.Send(msg); err != nil {
		return err
	}

	// The secret is not logged: it is a credential, and it is still on the wire
	// in the body above.
	logger.Debug("Lobby", "LobbyConnect", "titleID=%d sessionID=%d dataReceivedCount=%d clientMessageVersion=%d flags=%d",
		titleID, sessionID, 0, version, flags)
	return nil
}

func (c *LobbyClient) loop(ctx context.Context) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.mu.Lock()
			connected := c.connected
			stopped := c.stopped
			c.mu.Unlock()

			if stopped {
				return
			}
			if !connected {
				_ = c.connect()
			} else {
				// Periodic lobby keepalive ping (0x14).
				_ = c.Send([]byte{protocol.MsgLobbyPing})
			}
		}
	}
}

func (c *LobbyClient) Send(data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return errors.New("not connected")
	}

	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.conn.Write(data); err != nil {
		// A failed write means the connection is dead: tear it down so the
		// loop reconnects on the next tick instead of pinging a corpse
		// forever (the old code kept connected=true and never recovered).
		_ = c.conn.Close()
		c.conn = nil
		c.connected = false
		return err
	}
	return nil
}

func (c *LobbyClient) Stop() {
	c.mu.Lock()
	c.stopped = true
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
	c.connected = false
	c.mu.Unlock()
}
