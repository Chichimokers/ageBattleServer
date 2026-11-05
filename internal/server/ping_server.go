package server

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"math/big"
	stdnet "net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/luskaner/ageBattleServer/internal/logger"
)

const (
	oobMagicPing = 0x58
	oobMagicTLS  = 0x4C
	oobPingType  = 0x14
)

type PingServer struct {
	port      uint
	certFile  string
	keyFile   string
	listener  stdnet.Listener
	tlsConfig *tls.Config

	// stopped marks Stop()/ctx cancellation so the accept loop can tell a
	// shutdown from a transient Accept error, and conns tracks live
	// connections so Stop can close them (otherwise handleConnection
	// goroutines leak until their remote peers hang up).
	stopped atomic.Bool
	mu      sync.Mutex
	conns   map[stdnet.Conn]struct{}
}

// oobIdleTimeout bounds how long an established OOB connection may stay
// silent: game pings can be minutes apart, but a dead peer must not pin a
// goroutine and its buffers forever.
const oobIdleTimeout = 10 * time.Minute

func NewPingServer(port uint, certFile, keyFile string) *PingServer {
	return &PingServer{
		port:     port,
		certFile: certFile,
		keyFile:  keyFile,
		conns:    make(map[stdnet.Conn]struct{}),
	}
}

func (s *PingServer) Start(ctx context.Context) error {
	cert, err := s.loadOrGenerateCert()
	if err != nil {
		return err
	}
	s.tlsConfig = &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS10,
	}

	addr := fmt.Sprintf(":%d", s.port)
	ln, err := stdnet.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	logger.Info("Listener", "listen", "outOfBandPort listening on %s (TLS)", addr)

	s.mu.Lock()
	s.listener = ln
	s.mu.Unlock()

	// Honor ctx cancellation even when the owner never calls Stop: a blocked
	// Accept would otherwise leak this goroutine forever.
	go func() {
		<-ctx.Done()
		s.closeListener()
	}()

	go s.acceptLoop(ctx, ln)
	return nil
}

func (s *PingServer) closeListener() {
	s.mu.Lock()
	ln := s.listener
	s.listener = nil
	s.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
}

func (s *PingServer) trackConn(conn stdnet.Conn) {
	s.mu.Lock()
	s.conns[conn] = struct{}{}
	s.mu.Unlock()
}

func (s *PingServer) untrackConn(conn stdnet.Conn) {
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
}

func (s *PingServer) loadOrGenerateCert() (tls.Certificate, error) {
	if s.certFile != "" && s.keyFile != "" {
		cert, err := tls.LoadX509KeyPair(s.certFile, s.keyFile)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("load cert: %w", err)
		}
		return cert, nil
	}
	cert, err := generateDefaultCert()
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate default cert: %w", err)
	}
	logger.Info("Listener", "listen", "no -sslCert/-sslKey given, using generated default certificate")
	return cert, nil
}

func generateDefaultCert() (tls.Certificate, error) {
	// ECDSA P-256 default key: matches the certificate the upstream server presents,
	// and its generation and handshakes are far cheaper than RSA-2048 while keeping
	// the same security level. KeyEncipherment is deliberately omitted: the
	// ECDSA key never does RSA key exchange.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "ageLANServer"},
		NotBefore:    time.Now().Add(-time.Hour),   // 1h clock skew tolerance
		NotAfter:     time.Now().AddDate(10, 0, 0), // 10 years
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

func (s *PingServer) acceptLoop(ctx context.Context, ln stdnet.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if s.stopped.Load() {
				return
			}
			// Transient error (e.g. fd exhaustion): back off instead of a
			// busy spin that would pin a core and starve the closer.
			logger.Warning("Listener", "accept", "outOfBand accept: %v", err)
			time.Sleep(10 * time.Millisecond)
			continue
		}
		s.trackConn(conn)
		go s.handleConnection(conn)
	}
}

type tlsRWC struct {
	stdnet.Conn
	r io.Reader
}

func (c tlsRWC) Read(p []byte) (int, error) { return c.r.Read(p) }

func (s *PingServer) handleConnection(conn stdnet.Conn) {
	defer func() {
		s.untrackConn(conn)
		_ = conn.Close()
	}()

	// Handshake/init deadline (TLS record or magic words); lifted once the
	// connection is established.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(conn)

	// Adaptive: TLS record -> wss (TLS always loaded),
	// HTTP upgrade -> plaintext WebSocket, anything else -> raw magic/echo.
	next, err := reader.Peek(1)
	if err != nil {
		return
	}
	if isTLSRecord(next[0]) {
		s.handleTLS(conn, reader)
		return
	}
	if next[0] == 'G' || next[0] == 'g' {
		if b4, err := reader.Peek(4); err == nil && string(b4) == "GET " {
			serveWS(conn, reader)
			return
		}
	}
	s.handleRaw(conn, reader)
}

func (s *PingServer) handleRaw(conn stdnet.Conn, reader *bufio.Reader) {
	// The client first exchanges two 4-byte magic words (0x58, 0x4C) in
	// plaintext, each echoed back, then performs the TLS handshake on the
	// same connection. The handshake-phase deadline from handleConnection
	// covers the first magic word; once one exchange succeeds the connection
	// is proven alive and every later read carries the idle deadline instead of
	// no deadline at all.
	established := false
	for {
		if established {
			_ = conn.SetReadDeadline(time.Now().Add(oobIdleTimeout))
		}
		next, err := reader.Peek(1)
		if err != nil {
			return
		}
		if isTLSRecord(next[0]) {
			s.handleTLS(conn, reader)
			return
		}

		var word [4]byte
		if _, err := io.ReadFull(reader, word[:]); err != nil {
			return
		}
		w := binary.LittleEndian.Uint32(word[:])
		if w == oobMagicPing || w == oobMagicTLS {
			_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
			if _, err := conn.Write(word[:]); err != nil {
				return
			}
			established = true
		}
	}
}

func (s *PingServer) handleTLS(conn stdnet.Conn, reader *bufio.Reader) {
	// TLS handshake on the same connection (wss / TLS-wrapped OOB).
	tlsConn := tls.Server(tlsRWC{Conn: conn, r: reader}, s.tlsConfig)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_ = tlsConn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := tlsConn.Handshake(); err != nil {
		return
	}
	_ = tlsConn.SetDeadline(time.Time{})

	// Inside TLS it still speaks WebSocket when the
	// client upgrades (wss), otherwise raw RLink pings. The peek carries the
	// handshake deadline: a peer that completed TLS and then says nothing must
	// not hold the goroutine forever.
	tlsReader := bufio.NewReader(tlsConn)
	_ = tlsConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if next, err := tlsReader.Peek(4); err == nil && string(next) == "GET " {
		serveWS(tlsConn, tlsReader)
		return
	}

	// Encrypted raw RLink ping loop: echo every message of type 0x14. The
	// idle deadline bounds dead peers while allowing minute-spaced pings.
	buf := make([]byte, 4096)
	idle := func() { _ = tlsConn.SetReadDeadline(time.Now().Add(oobIdleTimeout)) }
	idle()
	for {
		n, err := tlsReader.Read(buf)
		if err != nil {
			return
		}
		idle()
		if n > 0 && buf[0] == oobPingType {
			_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
			if _, err := tlsConn.Write(buf[:n]); err != nil {
				return
			}
		}
	}
}

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func wsAcceptKey(key string) string {
	h := sha1.New()
	h.Write([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// wsFrameOp builds a FIN-set WS frame of the given opcode, encoding the
// payload length correctly for every size.
func wsFrameOp(opcode byte, payload []byte) []byte {
	frame := make([]byte, 0, 10+len(payload))
	frame = append(frame, 0x80|opcode) // FIN | opcode
	n := len(payload)
	switch {
	case n < 126: // 7-bit inline length
		frame = append(frame, byte(n))
	case n <= 0xFFFF: // 16-bit extended length marker
		frame = append(frame, 126, byte(n>>8), byte(n))
	default: // 64-bit extended length marker
		frame = append(frame, 127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		frame = append(frame, ext[:]...)
	}
	return append(frame, payload...)
}

func wsFrame(payload []byte) []byte {
	return wsFrameOp(0x2, payload)
}

func serveWS(conn stdnet.Conn, reader *bufio.Reader) {
	// The upgrade request carries a deadline too: a connection that opened and
	// sent half a request must not pin the goroutine.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	req, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	if _, err := io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: "+wsAcceptKey(req.Header.Get("Sec-WebSocket-Key"))+"\r\n\r\n"); err != nil {
		return
	}
	_ = conn.SetWriteDeadline(time.Time{})

	// WebSocket frame loop: echo every 0x14-typed payload re-framed, like
	// the ping service does. An idle deadline bounds dead peers while
	// still allowing minute-spaced pings.
	idle := func() { _ = conn.SetReadDeadline(time.Now().Add(oobIdleTimeout)) }
	idle()
	for {
		var hdr [2]byte
		if _, err := io.ReadFull(reader, hdr[:]); err != nil {
			return
		}
		idle()
		opcode := hdr[0] & 0x0F // low 4 bits: close/ping/pong/binary/text
		masked := hdr[1]&0x80 != 0
		length := int64(hdr[1] & 0x7F)
		if length == 126 { // 16-bit extended length marker
			var ext [2]byte
			if _, err := io.ReadFull(reader, ext[:]); err != nil {
				return
			}
			length = int64(binary.BigEndian.Uint16(ext[:]))
		} else if length == 127 { // 64-bit extended length marker
			var ext [8]byte
			if _, err := io.ReadFull(reader, ext[:]); err != nil {
				return
			}
			length = int64(binary.BigEndian.Uint64(ext[:]))
		}
		if length > 1<<20 { // 1 MiB max frame, defense against garbage
			return
		}
		var mask [4]byte
		if masked {
			if _, err := io.ReadFull(reader, mask[:]); err != nil {
				return
			}
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(reader, payload); err != nil {
			return
		}
		if masked {
			for i := range payload {
				payload[i] ^= mask[i%4]
			}
		}
		switch opcode {
		case 0x8: // WS close frame -> close the connection
			return
		case 0x9: // WS ping -> answer pong (0xA), echoing the payload
			_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
			_, _ = conn.Write(wsFrameOp(0xA, payload))
			continue
		}
		if len(payload) > 0 && payload[0] == oobPingType {
			_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
			if _, err := conn.Write(wsFrame(payload)); err != nil {
				return
			}
		}
	}
}

// isTLSRecord reports whether the first byte of a TLS record (content type:
// 0x14 change_cipher_spec, 0x15 alert, 0x16 handshake, 0x17 application data)
// identifies a TLS handshake start.
func isTLSRecord(b byte) bool {
	switch b {
	case 0x14, 0x15, 0x16, 0x17:
		return true
	}
	return false
}

// Stop closes the listener and every live connection so the per-connection
// goroutines exit instead of lingering until their remote peers hang up.
func (s *PingServer) Stop() {
	s.stopped.Store(true)
	s.closeListener()

	s.mu.Lock()
	conns := make([]stdnet.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

func (s *PingServer) GetPort() int {
	s.mu.Lock()
	ln := s.listener
	s.mu.Unlock()
	if ln != nil {
		return ln.Addr().(*stdnet.TCPAddr).Port
	}
	return 0
}
