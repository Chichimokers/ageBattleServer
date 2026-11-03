package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/lxzan/gws"

	"github.com/luskaner/ageBattleServer/internal/protocol"
	"github.com/luskaner/ageBattleServer/internal/session"
)

type frameTimeoutError struct{}

func (frameTimeoutError) Error() string   { return "timeout reading frame" }
func (frameTimeoutError) Timeout() bool   { return true }
func (frameTimeoutError) Temporary() bool { return true }

func wireFrame(msgType byte, body []byte) []byte {
	frame := make([]byte, 0, 3+len(body))
	frame = append(frame, byte((1+len(body))>>8), byte(1+len(body)), msgType)
	frame = append(frame, body...)
	return frame
}

// startServer builds a server from cfg, starts it, and registers the stop on cleanup.
func startServer(tb testing.TB, cfg *Config) *Server {
	tb.Helper()
	if tt, ok := tb.(*testing.T); ok {
		assertNoGoroutineLeaks(tt)
	}
	srv, err := New(cfg)
	if err != nil {
		tb.Fatalf("New error: %v", err)
	}
	if err := srv.Start(tb.Context()); err != nil {
		tb.Fatalf("Start error: %v", err)
	}
	tb.Cleanup(func() { _ = srv.Stop() })
	return srv
}

// dialPort opens a TCP connection to 127.0.0.1:port and closes it on cleanup.
func dialPort(t *testing.T, port int) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("Dial error: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// readFrameFrom reads one [u16 length][type][body] protocol frame from conn.
func readFrameFrom(t *testing.T, conn net.Conn) (byte, []byte) {
	t.Helper()
	var ln [2]byte
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(conn, ln[:]); err != nil {
		t.Fatalf("read frame length: %v", err)
	}
	length := int(binary.BigEndian.Uint16(ln[:]))
	data := make([]byte, length)
	if _, err := io.ReadFull(conn, data); err != nil {
		t.Fatalf("read frame data: %v", err)
	}
	return data[0], data[1:]
}

func TestBsPortReceivesData(t *testing.T) {
	cfg := &Config{
		EphemeralListenPorts: EphemeralBS,
		LogLevel:             "none",
	}

	srv := startServer(t, cfg)

	if srv.bsListener == nil {
		t.Fatal("bsListener should be created")
	}

	port := srv.bsListener.GetPort()
	if port == 0 {
		t.Fatal("bsPort should have a valid port")
	}

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("Dial error: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// 0x14 LOBBYPING is the echo the original actually performs (its
	// dispatcher has no case for 0x11, so a ping is never answered).
	_, err = conn.Write(wireFrame(protocol.MsgLobbyPing, []byte{0xAB}))
	if err != nil {
		t.Fatalf("Write error: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	reader := bufio.NewReader(conn)
	hdr := make([]byte, 3)
	_, err = reader.Read(hdr)
	if err != nil {
		t.Fatalf("Read error: %v", err)
	}

	if hdr[2] != protocol.MsgLobbyPing {
		t.Errorf("expected LOBBYPING response type 0x%02x, got 0x%02x", protocol.MsgLobbyPing, hdr[2])
	}
	payload := make([]byte, 1)
	if _, err = reader.Read(payload); err != nil {
		t.Fatalf("Read payload error: %v", err)
	}
	if payload[0] != 0xAB {
		t.Errorf("expected the ping body echoed verbatim, got 0x%02x", payload[0])
	}
}

func TestBsPortGameDataExchange(t *testing.T) {
	cfg := &Config{
		EphemeralListenPorts: EphemeralBS,
		LogLevel:             "none",
	}

	srv := startServer(t, cfg)

	port := srv.bsListener.GetPort()
	conn := dialPort(t, port)

	gdMsg := &protocol.GameDataMessage{
		InnerType: 0xFE,
		Payload:   []byte{0xDE, 0xAD, 0xBE, 0xEF},
	}

	body := gdMsg.Serialize()
	if _, err := conn.Write(wireFrame(protocol.MsgLobbyConnect,
		protocol.BuildLobbyConnect(0x1234, 0x2a, 0x0123456789abcdef, 0, 0, 5, 0, 0))); err != nil {
		t.Fatalf("write LobbyConnect: %v", err)
	}
	if msgType, _ := readFrameFrom(t, conn); msgType != protocol.MsgLobbyConnect {
		t.Fatalf("expected LobbyConnect echo, got 0x%02x", msgType)
	}
	_, err := conn.Write(wireFrame(protocol.MsgGameData, body))
	if err != nil {
		t.Fatalf("Write error: %v", err)
	}

	msgType, ack := readFrameFrom(t, conn)
	if msgType != protocol.MsgEndOfGameData {
		t.Fatalf("expected EndOfGameData response, got 0x%02x", msgType)
	}
	if len(ack) == 0 {
		t.Fatal("expected a non-empty game-data acknowledgement")
	}
}

func TestOobPortEcho(t *testing.T) {
	certFile, keyFile := generateTestCerts(t)
	cfg := &Config{
		EphemeralListenPorts: EphemeralOOB,
		SSLCert:              certFile,
		SSLKey:               keyFile,
		LogLevel:             "none",
	}

	srv := startServer(t, cfg)

	raw := dialPort(t, srv.oobListener.GetPort())

	// type u32 LE (0x14) + 4 bytes of payload
	echoData := []byte{0x14, 0x00, 0x00, 0x00, 0x48, 0x65, 0x6C, 0x6C, 0x6F}

	// StartTLS protocol: exchange the two 4-byte magic words and verify echoes.
	magic := []uint32{0x58, 0x4C}
	for _, m := range magic {
		var mw [4]byte
		binary.LittleEndian.PutUint32(mw[:], m)
		if _, err := raw.Write(mw[:]); err != nil {
			t.Fatalf("Write magic 0x%X error: %v", m, err)
		}
		var resp [4]byte
		if _, err := io.ReadFull(raw, resp[:]); err != nil {
			t.Fatalf("Read magic 0x%X echo error: %v", m, err)
		}
		if binary.LittleEndian.Uint32(resp[:]) != m {
			t.Errorf("expected magic 0x%X echo, got 0x%X", m, binary.LittleEndian.Uint32(resp[:]))
		}
	}

	// TLS handshake over the same connection.
	tlsConn := tls.Client(raw, &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS10,
	})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("TLS handshake error: %v", err)
	}

	_, err := tlsConn.Write(echoData)
	if err != nil {
		t.Fatalf("Write error: %v", err)
	}

	_ = tlsConn.SetReadDeadline(time.Now().Add(time.Second))
	response := make([]byte, len(echoData))
	_, err = io.ReadFull(tlsConn, response)
	if err != nil {
		t.Fatalf("Read error: %v", err)
	}

	if !bytes.Equal(response, echoData) {
		t.Errorf("expected echo %v, got %v", echoData, response)
	}
}

func TestWebSocketPortConnects(t *testing.T) {
	cfg := &Config{
		EphemeralListenPorts: EphemeralWS,
		LogLevel:             "none",
	}

	srv := startServer(t, cfg)

	if srv.wsListener == nil {
		t.Fatal("WebSocket listener not created")
	}
}

func TestRelayBroadcastSendsData(t *testing.T) {
	certFile, keyFile := generateTestCerts(t)
	cfg := &Config{
		RelayBroadcastPorts: []uint{1},
		Name:                "TestServer",
		PublicIP:            "192.168.1.100",
		PublicPort:          27012,
		OutOfBandPort:       27013,
		SSLCert:             certFile,
		SSLKey:              keyFile,
		SimulationPeriod:    16.0,
		LogLevel:            "none",
	}

	srv := startServer(t, cfg)

	msg := srv.buildBroadcastMessage()

	if len(msg) < 30 {
		t.Errorf("broadcast message too short: %d bytes", len(msg))
	}

	if msg[0] != protocol.MsgRelayBroadcast {
		t.Errorf("expected broadcast type 0x%02x, got 0x%02x", protocol.MsgRelayBroadcast, msg[0])
	}

	if !bytes.Contains(msg, []byte("TestServer")) {
		t.Error("broadcast should contain server name")
	}

	if bytes.Contains(msg, []byte("192.168.1.100")) {
		t.Error("broadcast should not contain public IP (client uses UDP source address)")
	}
}

func TestMultipleClientsConnect(t *testing.T) {
	cfg := &Config{
		EphemeralListenPorts: EphemeralBS,
		LogLevel:             "none",
	}

	srv := startServer(t, cfg)

	port := srv.bsListener.GetPort()

	connections := make([]net.Conn, 5)
	for i := range 5 {
		conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			t.Fatalf("Dial %d error: %v", i, err)
		}
		connections[i] = conn
	}

	time.Sleep(100 * time.Millisecond)

	srv.mu.Lock()
	clientCount := len(srv.clients)
	srv.mu.Unlock()

	if clientCount != 5 {
		t.Errorf("expected 5 clients, got %d", clientCount)
	}

	for _, conn := range connections {
		_ = conn.Close()
	}
}

func TestClientSendInvalidData(t *testing.T) {
	cfg := &Config{
		EphemeralListenPorts: EphemeralBS,
		LogLevel:             "none",
	}

	srv := startServer(t, cfg)

	conn := dialPort(t, srv.bsListener.GetPort())

	_, err := conn.Write([]byte{0xFF, 0xFF, 0xFF})
	if err != nil {
		t.Fatalf("Write error: %v", err)
	}

	time.Sleep(50 * time.Millisecond)
}

func TestClientLargeMessage(t *testing.T) {
	cfg := &Config{
		EphemeralListenPorts: EphemeralBS,
		LogLevel:             "none",
	}

	srv := startServer(t, cfg)

	conn := dialPort(t, srv.bsListener.GetPort())

	payload := make([]byte, 1024)
	for i := range payload {
		payload[i] = byte(i % 256)
	}

	gdMsg := &protocol.GameDataMessage{
		InnerType: 0xFE,
		Payload:   payload,
	}

	body := gdMsg.Serialize()
	_, err := conn.Write(wireFrame(protocol.MsgGameData, body))
	if err != nil {
		t.Fatalf("Write error: %v", err)
	}

	time.Sleep(50 * time.Millisecond)
}

func TestLobbyConnectionAttempt(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen error: %v", err)
	}
	defer func() { _ = ln.Close() }()

	port := ln.Addr().(*net.TCPAddr).Port

	cfg := &Config{
		LobbyHost: "127.0.0.1",
		LobbyPort: uint(port),
		LogLevel:  "none",
	}

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		buf := make([]byte, 1024)
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		n, _ := conn.Read(buf)
		if n > 0 && buf[0] == 0x15 {
		}
	}()

	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err = srv.Start(ctx)
	if err != nil {
		t.Fatalf("Start error: %v", err)
	}
	defer func() { _ = srv.Stop() }()

	time.Sleep(500 * time.Millisecond)
}

func TestBsPortIssueCaptureSession(t *testing.T) {
	cfg := &Config{
		EphemeralListenPorts: EphemeralBS,
		LogLevel:             "none",
	}

	srv := startServer(t, cfg)

	conn := dialPort(t, srv.bsListener.GetPort())

	// Replay the lobby connect from bs_port_issue.pcapng.
	connectBody := []byte{0x3f, 0xa5, 0xbe, 0x1d, 0x00, 0x00, 0x18, 0x00, 0x7f, 0x46, 0xc2,
		0x8a, 0x3d, 0xfb, 0x91, 0x3d, 0x00, 0x05, 0x00, 0x00, 0x00, 0x00, 0x00,
		0xa3, 0xa3, 0x27, 0x01, 0x00, 0x00, 0x00, 0x00}
	if _, err := conn.Write(wireFrame(protocol.MsgLobbyConnect, connectBody)); err != nil {
		t.Fatalf("Write connect: %v", err)
	}

	// Expect the connect to be echoed back verbatim (the handshake the game needs).
	msgType, body := readFrameFrom(t, conn)
	if msgType != protocol.MsgLobbyConnect || !bytes.Equal(body, connectBody) {
		t.Fatalf("expected connect echo, got type=0x%02x body=%x", msgType, body)
	}

	writeRelay := func(relay []byte) []byte {
		if _, err := conn.Write(wireFrame(protocol.MsgLobbyRelay, relay)); err != nil {
			t.Fatalf("Write relay: %v", err)
		}
		return relay
	}
	gameRelay := func(size int) []byte {
		relay := make([]byte, size)
		relay[0], relay[1], relay[2] = 0x01, 0x01, 0x0f
		for i := 3; i < size; i += 4 {
			relay[i] = byte(0x10 + (i % 0x70))
		}
		return relay
	}

	// The first relay carries the player's compressed RequestJoin for the
	// session it connected to: it is consumed and comes back as the 0x0f peer
	// message it carried, the 3-byte envelope stripped.
	joinKey := session.SessionKey{TitleID: 24, SessionID: 0x1dbea53f}
	relay1 := writeRelay(relayBody(0x01, 0x01, compressedPeerMessage(t, requestJoinInner(joinKey, 0x3d))))
	msgType, body = readFrameFrom(t, conn)
	if msgType != protocol.MsgPeer || !bytes.Equal(body, relay1[3:]) {
		t.Fatalf("expected peer message, got type=0x%02x", msgType)
	}

	// Subsequent relays are echoed verbatim.
	for _, size := range []int{725, 867} {
		relay := writeRelay(gameRelay(size))
		msgType, body = readFrameFrom(t, conn)
		if msgType != protocol.MsgLobbyRelay || !bytes.Equal(body, relay) {
			t.Fatalf("expected relay echo, got type=0x%02x", msgType)
		}
	}

	// Idle: the connection must survive well past the old 5s server-side close.
	_ = conn.SetReadDeadline(time.Now().Add(6 * time.Second))
	buf := make([]byte, 1)
	n, err := conn.Read(buf)
	if err == nil {
		t.Fatalf("unexpected data after idle: %x", buf[:n])
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("connection was closed during idle: %v", err)
	}
}

type wsTestHandler struct {
	mu     sync.Mutex
	frames [][]byte
}

func (h *wsTestHandler) OnOpen(*gws.Conn)         {}
func (h *wsTestHandler) OnClose(*gws.Conn, error) {}
func (h *wsTestHandler) OnPing(*gws.Conn, []byte) {}
func (h *wsTestHandler) OnPong(*gws.Conn, []byte) {}
func (h *wsTestHandler) OnMessage(_ *gws.Conn, m *gws.Message) {
	data := m.Bytes()
	h.mu.Lock()
	h.frames = append(h.frames, data)
	h.mu.Unlock()
}

func (h *wsTestHandler) takeFrames() [][]byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	frames := h.frames
	h.frames = nil
	return frames
}

func dialWS(t *testing.T, port int) (*gws.Conn, *wsTestHandler) {
	t.Helper()
	h := &wsTestHandler{}
	addr := fmt.Sprintf("ws://127.0.0.1:%d/", port)
	// The WS listener binds on a goroutine, so the dial can race the bind;
	// retry until the port is accepting.
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, _, err := gws.NewClient(h, &gws.ClientOption{Addr: addr})
		if err == nil {
			go conn.ReadLoop()
			return conn, h
		}
		if time.Now().After(deadline) {
			t.Fatalf("WS dial: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// frameType splits a server WebSocket message into type and payload. The
// message is [type][payload]: the server never puts a length prefix in front,
// so none is guessed at.
func frameType(f []byte) (byte, []byte) {
	if len(f) >= 1 {
		return f[0], f[1:]
	}
	return 0, nil
}

func waitForFrame(t *testing.T, h *wsTestHandler, want byte, timeout time.Duration) []byte {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, f := range h.takeFrames() {
			if typ, body := frameType(f); typ == want {
				return body
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for frame type 0x%02x", want)
	return nil
}

// wsJoinHost makes the host create a session, the way the game creates it: a
// LobbyConnect keyed on (titleID 0, sessionID 1), which is the path the original's
// 0x15 handler takes, into the session manager's GetOrCreateSession, and the only
// one it has. It used to send a top-level RequestJoin, which existed here
// because this server had a handler for a message type the original's dispatcher does
// not list; TestTopLevelRequestJoinIsAnUnknownMessageType is what keeps 0x01 inert, and
// this is the replacement for the other half.
//
// The LobbyPing that follows is still what proves the session exists: the echo comes
// back on the same socket, so FIFO says the connect was handled first.
func wsJoinHost(t *testing.T, host *gws.Conn, hostH *wsTestHandler) {
	t.Helper()
	connect := protocol.BuildLobbyConnect(0, 1, 0x0123456789abcdef, 0, 0, 5, 0, 0)
	if err := host.WriteMessage(gws.OpcodeBinary, wsMessage(protocol.MsgLobbyConnect, connect)); err != nil {
		t.Fatalf("host connect: %v", err)
	}
	if err := host.WriteMessage(gws.OpcodeBinary, wsMessage(protocol.MsgLobbyPing, nil)); err != nil {
		t.Fatalf("host ping: %v", err)
	}
	waitForFrame(t, hostH, protocol.MsgLobbyPing, 2*time.Second)
}

// observerConn wraps a net.Conn and can read both WS frames and raw protocol frames
// after the server switches to raw protocol for observers.
type observerConn struct {
	conn net.Conn
	buf  []byte
}

func newObserverConn(conn net.Conn) *observerConn {
	return &observerConn{conn: conn, buf: make([]byte, 0, 4096)}
}

// fillBuffer reads more bytes into oc.buf. It returns true when a read timed
// out (callers should keep polling) and a non-nil error on a real failure.
func (oc *observerConn) fillBuffer() (bool, error) {
	_ = oc.conn.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
	tmp := make([]byte, 1024)
	n, err := oc.conn.Read(tmp)
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return true, nil
		}
		return false, err
	}
	oc.buf = append(oc.buf, tmp[:n]...)
	return false, nil
}

// wsFrameLen returns the payload length and total header size of a WS frame
// whose first two bytes are in oc.buf. ok is false when more bytes are needed.
func (oc *observerConn) wsFrameLen() (payloadLen, offset int, ok bool) {
	payloadLen = int(oc.buf[1] & 0x7F)
	offset = 2
	if payloadLen == 126 {
		if len(oc.buf) < 4 {
			return 0, 0, false
		}
		payloadLen = int(binary.BigEndian.Uint16(oc.buf[2:4]))
		offset = 4
	} else if payloadLen == 127 {
		if len(oc.buf) < 10 {
			return 0, 0, false
		}
		offset = 10
	}
	if oc.buf[1]&0x80 != 0 { // masked
		offset += 4
	}
	return payloadLen, offset, true
}

// readWSFrame reads a WebSocket binary frame (opcode 0x82) and returns the payload.
func (oc *observerConn) readWSFrame(timeout time.Duration) ([]byte, error) {
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return nil, errors.New("timeout reading WS frame")
		}
		// Need at least 2 bytes for header
		for len(oc.buf) >= 2 {
			payloadLen, offset, ok := oc.wsFrameLen()
			if !ok {
				break
			}
			if len(oc.buf) >= offset+payloadLen {
				if oc.buf[0]&0x0F != 0x02 { // not binary: skip ping/pong/close
					oc.buf = oc.buf[offset+payloadLen:]
					continue
				}
				payload := oc.buf[offset : offset+payloadLen]
				oc.buf = oc.buf[offset+payloadLen:]
				return payload, nil
			}
			break
		}
		timedOut, err := oc.fillBuffer()
		if timedOut {
			continue
		}
		if err != nil {
			return nil, err
		}
	}
}

// readFrame reads a raw protocol frame [u16 length][type][body] (used after WS handshake for observers).
// readFrame reads one unmasked server-to-client WebSocket frame and splits its
// payload into [type][body]. An observer is not a raw stream: the server frames
// observer traffic through the WebSocket layer like any other client on the port.
func (oc *observerConn) readFrame(timeout time.Duration) (byte, []byte, error) {
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return 0, nil, frameTimeoutError{}
		}
		for {
			payload, rest, ok := cutWSFrame(oc.buf)
			if !ok {
				break
			}
			oc.buf = rest
			if len(payload) == 0 {
				return 0, nil, nil
			}
			return payload[0], payload[1:], nil
		}
		timedOut, err := oc.fillBuffer()
		if timedOut {
			continue
		}
		if err != nil {
			return 0, nil, err
		}
	}
}

// cutWSFrame takes one unmasked server-to-client frame off the front of b.
// It reports ok=false when b does not yet hold a whole frame.
func cutWSFrame(b []byte) (payload, rest []byte, ok bool) {
	if len(b) < 2 {
		return nil, b, false
	}
	masked := b[1]&0x80 != 0
	length := uint64(b[1] & 0x7F)
	off := 2
	switch length {
	case 126:
		if len(b) < off+2 {
			return nil, b, false
		}
		length = uint64(binary.BigEndian.Uint16(b[off : off+2]))
		off += 2
	case 127:
		if len(b) < off+8 {
			return nil, b, false
		}
		length = binary.BigEndian.Uint64(b[off : off+8])
		off += 8
	}
	if masked {
		off += 4
	}
	if uint64(len(b)-off) < length {
		return nil, b, false
	}
	payload = b[off : off+int(length)]
	return payload, b[off+int(length):], true
}

// TestWebSocketObserverFlow exercises the observer chain end to end over a real
// WebSocket connection: the LobbyConnect creates the session, the ack tick fills the
// frame store with the 0x0A messages the host also receives, StartObserve is
// accepted, ActivateObserver answers with the estimated start time, and the
// session tick serves the observer those stored frames.
//
// The observer is served the session's 0x0A stream and nothing else: no 0x09
// pages (the store does not hold the players' uploads) and no end-of-game marker
// (the observer update sends it only when no player is left in the session, and here
// there is one). That is the shape capture 2026-09-29_012528_03cb67c7cf19 shows
// for the original.
func TestWebSocketObserverFlow(t *testing.T) {
	cfg := &Config{
		EphemeralListenPorts:     EphemeralWS,
		ObserverMax:              4,
		ObserverUpdateIntervalMS: 250,
		SimulationPeriod:         125,
		// The default store: 8 blocks of 64 KiB, which is half a megabyte of ack
		// history and far more than a test produces.
		MaxPagesPerMatch: 0,
		LogLevel:         "none",
	}

	srv := startServer(t, cfg)
	// The listener bound ":0" and this is the port it got, which is the only one
	// there is: nothing chose it and nothing released it.
	port := srv.wsListener.GetPort()
	if port == 0 {
		t.Fatal("webSocketPort 0 after Start: the listener is not listening on a port")
	}

	host, hostH := dialWS(t, port)
	defer func() { _ = host.WriteClose(1000, nil) }()

	// Observer: use a raw TCP connection after WS handshake to read raw protocol frames.
	obsRaw, err := dialObserver(t, port)
	if err != nil {
		t.Fatalf("dial observer: %v", err)
	}
	defer func() { _ = obsRaw.conn.Close() }()

	// Host creates the first session (ID 1). A LobbyPing echo after the join
	// guarantees (same-socket FIFO) the session exists before pages are sent.
	wsJoinHost(t, host, hostH)

	page := (&protocol.GameDataMessage{InnerType: 0xFE, Payload: []byte{0xAA, 0xBB}}).Serialize()
	for i := range 4 {
		if err := host.WriteMessage(gws.OpcodeBinary, wsMessage(protocol.MsgGameData, page)); err != nil {
			t.Fatalf("host page %d: %v", i, err)
		}
	}
	// Every 0x0A the host receives is also stored (the session tick appends the ack
	// it just built). Keep the bodies: the observer has to be
	// served exactly these, which is the whole claim of this test.
	hostAcks := map[string]bool{}
	ackDeadline := time.Now().Add(2 * time.Second)
	subs := 0
	for subs < 4 || len(hostAcks) < 2 {
		if time.Now().After(ackDeadline) {
			t.Fatalf("timeout waiting for page acks: got %d/4 and %d stored acks", subs, len(hostAcks))
		}
		for _, f := range hostH.takeFrames() {
			typ, body := frameType(f)
			if typ != protocol.MsgEndOfGameData {
				continue
			}
			hostAcks[string(body)] = true
			if len(body) > 0 && body[0] == 0xfe {
				subs += int(body[1])
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Observer: StartObserve (0x1C Vx format, [titleID|sessionID 48b] + 2 u64)
	// -> status 0, which is the arm that registers the observer. The 0x18 legacy
	// form carries no titleID and the original's parser fills it with 0xFFFF, so
	// it cannot match a session keyed on the (titleID, sessionID) pair.
	sid := make([]byte, 24)
	binary.LittleEndian.PutUint64(sid[:8], 1) // titleID 0, sessionID 1
	if err := obsRaw.writeWSFrame(wsMessage(0x1C, sid)); err != nil {
		t.Fatalf("observer start: %v", err)
	}
	msgType, body, err := obsRaw.readFrame(2 * time.Second)
	if err != nil {
		t.Fatalf("observer start response: %v", err)
	}
	if msgType != protocol.MsgStartObserveResp || len(body) != 1 || body[0] != 0 {
		t.Fatalf("expected StartObserveResponse status 0, got type=0x%02x body=%x", msgType, body)
	}

	// Observer: ActivateObserver -> ActivateObserverResponse [status][time].
	if err := obsRaw.writeWSFrame(wsMessage(protocol.MsgActivateObserver, nil)); err != nil {
		t.Fatalf("observer activate: %v", err)
	}
	msgType, body, err = obsRaw.readFrame(2 * time.Second)
	if err != nil {
		t.Fatalf("observer activate response: %v", err)
	}
	if msgType != protocol.MsgActivateObserverResp || len(body) < 9 || body[0] != 0 {
		t.Fatalf("expected ActivateObserverResponse ok+time, got type=0x%02x body=%x", msgType, body)
	}

	// The observer stream stays on the WebSocket framing for the rest of the
	// flow: the server never switches an observer to a raw protocol stream.

	// The session tick (interval 250ms/125ms = 2) serves the stored acks to the
	// observer, starting from the first frame of the match.
	deadline := time.Now().Add(5 * time.Second)
	var frames int
	for frames < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for the replay stream: %d frames", frames)
		}
		msgType, body, err = obsRaw.readFrame(100 * time.Millisecond)
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				continue
			}
			t.Fatalf("observer read frame: %v", err)
		}
		switch msgType {
		case protocol.MsgEndOfGameData:
			if !hostAcks[string(body)] {
				t.Fatalf("the observer got a 0x0A the host never received: %x", body)
			}
			frames++
		case protocol.MsgGameData:
			t.Fatalf("the observer got a 0x09 page: the store holds the session's acks, not the uploads (%x)", body)
		case protocol.MsgEndOfGameDataObserver:
			t.Fatal("the observer got the end-of-game marker while a player is in the session")
		}
	}

	// The host must never see the observer's own traffic.
	deadline = time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		for _, f := range hostH.takeFrames() {
			if typ, _ := frameType(f); typ == protocol.MsgEndOfGameDataObserver {
				t.Fatal("host received observer end-of-game marker")
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// dialObserver creates a raw TCP connection, performs WS handshake manually,
// then returns the net.Conn for sending WS frames and reading raw protocol frames.
func dialObserver(t *testing.T, port int) (*observerConn, error) {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}

	// Send HTTP upgrade request
	key := "dGhlIHNhbXBsZSBub25jZQ==" // standard test key
	req := fmt.Sprintf("GET / HTTP/1.1\r\nHost: 127.0.0.1:%d\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", port, key)
	if _, err := conn.Write([]byte(req)); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("write upgrade request: %w", err)
	}

	// Read HTTP 101 response
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read upgrade response: %w", err)
	}
	if !bytes.Contains(buf[:n], []byte("101 Switching Protocols")) {
		_ = conn.Close()
		return nil, fmt.Errorf("upgrade failed: %s", string(buf[:n]))
	}

	// Upgrade successful - conn is now a raw WebSocket connection
	return newObserverConn(conn), nil
}

// writeWSFrame writes a WebSocket binary frame (opcode 0x82) with the given payload.
// Client-to-server frames must be masked.
func (oc *observerConn) writeWSFrame(payload []byte) error {
	// WS frame: FIN=1, RSV=0, Opcode=0x2 (binary), MASK=1
	var frame []byte
	frame = append(frame, 0x82)                // FIN + binary opcode
	maskKey := [4]byte{0x12, 0x34, 0x56, 0x78} // fixed mask for testing
	if len(payload) < 126 {
		frame = append(frame, byte(len(payload)|0x80)) // MASK bit set
	} else if len(payload) < 65536 {
		frame = append(frame, 126|0x80)
		frame = append(frame, byte(len(payload)>>8), byte(len(payload)))
	} else {
		frame = append(frame, 127|0x80)
		for i := 7; i >= 0; i-- {
			frame = append(frame, byte(len(payload)>>(i*8)))
		}
	}
	frame = append(frame, maskKey[:]...)
	// Apply mask
	masked := make([]byte, len(payload))
	for i := range payload {
		masked[i] = payload[i] ^ maskKey[i%4]
	}
	frame = append(frame, masked...)
	_, err := oc.conn.Write(frame)
	return err
}

// TestGameDataAckOrder verifies that two GameData messages sent back-to-back
// are acknowledged in the exact order they were sent. Regression for capture
// _5: the two final 96-byte replay chunks (t+189861/t+189862) were queued in
// reverse order in the t+189886 ack ([F8 0E][F8 0C] instead of [F8 0C][F8 0E])
// because gws ParallelEnabled dispatched the OnMessage callbacks in parallel,
// which crashed the client. Order must follow arrival order.
func TestGameDataAckOrder(t *testing.T) {
	cfg := &Config{
		EphemeralListenPorts: EphemeralWS,
		SimulationPeriod:     125,
		LogLevel:             "none",
	}

	srv := startServer(t, cfg)
	port := srv.wsListener.GetPort()
	if port == 0 {
		t.Fatal("webSocketPort 0 after Start: the listener is not listening on a port")
	}

	host, hostH := dialWS(t, port)
	defer func() { _ = host.WriteClose(1000, nil) }()

	wsJoinHost(t, host, hostH)

	// Two distinct replay-data chunks sent back-to-back (the _5 crash pattern).
	pageA := (&protocol.GameDataMessage{InnerType: 0xFE, Payload: []byte{0xAA, 0xBB}}).Serialize()
	pageB := (&protocol.GameDataMessage{InnerType: 0xFE, Payload: []byte{0xCC, 0xDD}}).Serialize()
	if err := host.WriteMessage(gws.OpcodeBinary, wsMessage(protocol.MsgGameData, pageA)); err != nil {
		t.Fatalf("send pageA: %v", err)
	}
	if err := host.WriteMessage(gws.OpcodeBinary, wsMessage(protocol.MsgGameData, pageB)); err != nil {
		t.Fatalf("send pageB: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	order := make([]byte, 0, 2)
	for len(order) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("timeout: acks saw subs %q, want both A and B", order)
		}
		for _, f := range hostH.takeFrames() {
			typ, body := frameType(f)
			if typ != protocol.MsgEndOfGameData || len(body) < 3 || body[0] != 0xfe {
				continue
			}
			count := int(body[1])
			off := 2
			for range count {
				if off+3 > len(body) || body[off] != 0x01 {
					break
				}
				ln := int(binary.LittleEndian.Uint16(body[off+1 : off+3]))
				if off+3+ln > len(body) {
					break
				}
				payload := body[off+3 : off+3+ln]
				off += 3 + ln
				switch {
				case bytes.Equal(payload, []byte{0xAA, 0xBB}):
					order = append(order, 'A')
				case bytes.Equal(payload, []byte{0xCC, 0xDD}):
					order = append(order, 'B')
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	if len(order) != 2 || order[0] != 'A' || order[1] != 'B' {
		t.Fatalf("ack sub order = %q, want [A B] (pageA then pageB)", order)
	}
}

// TestMainLoopAdaptiveTickerClientChurn exercises the adaptive 1ms processing
// ticker: it must start on the first client, stop when the last one disconnects,
// and restart on a later connection. WS messages are only dispatched inside
// Process (mainLoop ticker), so the ping echo proves ticker-driven processing
// ran in each cycle; the registry-drain wait guarantees every cycle starts from
// an empty server (i.e. the ticker was stopped and is restarted).
func TestMainLoopAdaptiveTickerClientChurn(t *testing.T) {
	cfg := &Config{
		EphemeralListenPorts: EphemeralWS,
		LogLevel:             "none",
	}

	srv := startServer(t, cfg)
	port := srv.wsListener.GetPort()
	if port == 0 {
		t.Fatal("webSocketPort 0 after Start: the listener is not listening on a port")
	}

	for i := range 3 {
		host, hostH := dialWS(t, port)

		if err := host.WriteMessage(gws.OpcodeBinary, wsMessage(protocol.MsgLobbyPing, []byte{0x01})); err != nil {
			t.Fatalf("cycle %d ping write: %v", i, err)
		}
		waitForFrame(t, hostH, protocol.MsgLobbyPing, 2*time.Second)

		_ = host.WriteClose(1000, nil)

		deadline := time.Now().Add(2 * time.Second)
		for {
			srv.mu.Lock()
			n := len(srv.clients)
			srv.mu.Unlock()
			if n == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("cycle %d: client registry never drained (clients=%d)", i, n)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}
