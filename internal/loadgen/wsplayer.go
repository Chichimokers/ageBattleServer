package loadgen

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/lxzan/gws"
)

// wsPlayer is a real WebSocket game client: it sends relay messages framed as
// bare [type][body] (the game's wire format, also accepted by the Go
// server) and receives the server's bare [type][body] replies.
type wsPlayer struct {
	conn   *gws.Conn
	stats  *Stats
	frames chan []byte
	done   chan struct{}
}

type wsPlayerHandler struct {
	p *wsPlayer
}

func (h *wsPlayerHandler) OnOpen(*gws.Conn)         {}
func (h *wsPlayerHandler) OnClose(*gws.Conn, error) { close(h.p.done) }
func (h *wsPlayerHandler) OnPing(*gws.Conn, []byte) {}
func (h *wsPlayerHandler) OnPong(*gws.Conn, []byte) {}
func (h *wsPlayerHandler) OnMessage(_ *gws.Conn, m *gws.Message) {
	payload := append([]byte(nil), m.Bytes()...)
	h.p.stats.recordRecv(len(payload))
	// Non-blocking: waitType consumes when waiting; otherwise frames are
	// counted and dropped once the buffer is full.
	select {
	case h.p.frames <- payload:
	default:
	}
}

// dialWSPlayer opens a WS player connection to addr ("ws://host:port/").
func dialWSPlayer(addr string, stats *Stats) (*wsPlayer, error) {
	p := &wsPlayer{
		stats:  stats,
		frames: make(chan []byte, 4096),
		done:   make(chan struct{}),
	}
	//goland:noinspection GoResourceLeak
	conn, _, err := gws.NewClient(&wsPlayerHandler{p: p}, &gws.ClientOption{Addr: addr})
	if err != nil {
		return nil, fmt.Errorf("ws dial: %w", err)
	}
	p.conn = conn
	go conn.ReadLoop()
	return p, nil
}

func (p *wsPlayer) send(msgType byte, body []byte) error {
	frame := wireFrame(msgType, body)
	if err := p.conn.WriteMessage(gws.OpcodeBinary, frame); err != nil {
		return err
	}
	// Counted only once the write succeeded: a failed send is an error and not
	// traffic, and the old order made the report claim bytes that never left.
	p.stats.recordSent(len(frame))
	return nil
}

func (p *wsPlayer) close() {
	if p.conn != nil {
		_ = p.conn.WriteClose(1000, nil)
	}
}

// parseReply extracts (type, body) from a server WS payload. The server sends
// bare [type][body]; the length-prefixed [len][type][body] form is also
// accepted (parseWsMessage semantics).
func parseReply(data []byte) (byte, []byte, bool) {
	if len(data) < 2 {
		return 0, nil, false
	}
	declared := int(binary.BigEndian.Uint16(data[:2]))
	if declared == len(data)-2 {
		return data[2], data[3:], true
	}
	return data[0], data[1:], true
}

// waitType consumes server frames until one of the wanted types arrives or the
// deadline passes. Frames of other types are counted and discarded.
func (p *wsPlayer) waitType(want []byte, timeout time.Duration) (byte, []byte, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-p.done:
			return 0, nil, false
		case payload := <-p.frames:
			typ, body, ok := parseReply(payload)
			if !ok {
				continue
			}
			if slices.Contains(want, typ) {
				return typ, body, true
			}
		case <-time.After(time.Until(deadline)):
			return 0, nil, false
		}
	}
	return 0, nil, false
}

// lobbyConnect sends a LobbyConnect for sessionID and waits for the echo,
// which (same-socket FIFO) confirms the client is registered in the session.
func (p *wsPlayer) lobbyConnect(sessionID uint64, reconnectState byte, secret uint64) error {
	body := lobbyConnectBody(sessionID, reconnectState, secret)
	if err := p.send(0x15, body); err != nil {
		return err
	}
	typ, got, ok := p.waitType([]byte{0x15}, 5*time.Second)
	if !ok || typ != 0x15 || len(got) != len(body) {
		return errors.New("no LobbyConnect echo")
	}
	return nil
}

// pingOnce measures one LobbyPing round trip.
func (p *wsPlayer) pingOnce() error {
	if err := p.send(0x14, []byte{0x00}); err != nil {
		return err
	}
	start := time.Now()
	_, _, ok := p.waitType([]byte{0x14}, 2*time.Second)
	if !ok {
		return errors.New("ping timeout")
	}
	p.stats.recordPing(time.Since(start))
	return nil
}
