package server

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/luskaner/ageBattleServer/internal/logger"
	"github.com/luskaner/ageBattleServer/internal/protocol"
)

// readPollInterval is the read deadline the read loop sets before every read.
// It only wakes the loop so it can notice a cancelled context; it is not a limit
// on the peer, and a frame still incomplete when it expires is continued by the
// next read (see readMessage).
const readPollInterval = 15 * time.Second

// closeDrainTimeout is how long a server-side close waits for the queued frames
// to reach the peer before it closes the socket anyway.
const closeDrainTimeout = 10 * time.Second

// maxQueuedSendBytes caps the outbound queue of one connection. Sends never wait
// and never time out, so a peer that stops reading makes the queue grow; the cap
// is far above any real burst (the frame store a reconnect resends holds 512 KiB
// by default, and a minute of a full match is a few MB), so reaching it means
// the peer has stopped reading for good. The connection is then closed, never
// left open with frames dropped.
const maxQueuedSendBytes = 64 << 20

// defaultSilentTimeout closes a connection after this long without a complete
// message, counted from its accept until its first one. The original applies it
// to every connection, before and after its LobbyConnect; only a timeout override
// replaces it.
const defaultSilentTimeout = 60 * time.Second

// lobbyPingInterval is how often the server sends its own LobbyPing (0x14)
// keepalive to game clients.
const lobbyPingInterval = 10 * time.Second

type Client struct {
	lastSeen time.Time
	lastPing time.Time
	conn     net.Conn
	host     ClientHost
	counter  metricsCounter
	reader   *bufio.Reader
	writer   *bufio.Writer
	// sendQueue is the outbound FIFO, guarded by mu. queuedBytes counts its bytes
	// plus those of the batch the writer is still writing.
	sendQueue   []outboundMsg
	queuedBytes int
	// sendSignal wakes the writer; one pending signal covers any number of
	// queued frames.
	sendSignal chan struct{}
	// closeCh is closed when the socket is closed.
	closeCh chan struct{}
	// readBuf holds the frame being read, [u16 BE length][type][body], for as
	// long as it is incomplete. readComplete marks that it holds a whole frame
	// the caller has been handed, so the next read starts a new one.
	readBuf      []byte
	readComplete bool
	// readPoll is readPollInterval; a test shortens it before Start.
	readPoll time.Duration
	// maxQueuedBytes is maxQueuedSendBytes; a test lowers it.
	maxQueuedBytes int
	// silentTimeout is the timeout override, 0 while defaultSilentTimeout
	// applies.
	silentTimeout   time.Duration
	mu              sync.Mutex
	timeoutOverride uint32
	// clientMessageVersion is the connection's announced client message version, see
	// ClientMessageVersion.
	clientMessageVersion uint16
	// closed is set by the first close of either kind: the connection takes no
	// new frames and has been removed from the host.
	closed bool
	// draining is a server-side close waiting for the writer to deliver the queue.
	draining bool
	// socketClosed is set once the socket itself is closed.
	socketClosed bool
	started      bool
	// pinged is set by the first replay-data upload and read on every upload, so
	// it is an atomic rather than a field behind mu: the two reads per message
	// were two lock acquisitions on the busiest path in a real match.
	pinged atomic.Bool
	// peerIdentifier is the connection's observer identity, the u64 the observer
	// list is keyed on (session.PeerConnection). Atomic because the
	// session writes it from the message loop while the connection is also
	// touched from the socket reader.
	peerIdentifier atomic.Uint64
}

// outboundMsg is one queued write. pooled marks frames built by SendMessage
// from the frame pool: the writeLoop returns those to the pool once written,
// while plain Send payloads are owned by the caller and must not be pooled.
type outboundMsg struct {
	data   []byte
	pooled bool
}

// framePool reuses the [len u16 BE][type][body] frames built by SendMessage.
// Only SendMessage-produced frames are returned to it (by the writer that
// consumed them), so a caller that builds its own frame with buildFrame (or the
// test helper frameMessage) never shares pooled buffers.
var framePool = sync.Pool{
	New: func() any { return make([]byte, 0, 64) },
}

func NewClient(conn net.Conn, host ClientHost) *Client {
	c := &Client{
		conn:           conn,
		host:           host,
		reader:         bufio.NewReader(conn),
		writer:         bufio.NewWriter(conn),
		sendSignal:     make(chan struct{}, 1),
		closeCh:        make(chan struct{}),
		lastSeen:       time.Now(),
		lastPing:       time.Now(),
		readPoll:       readPollInterval,
		maxQueuedBytes: maxQueuedSendBytes,
	}
	host.AddClient(c)
	return c
}

func (c *Client) Start(ctx context.Context) error {
	c.mu.Lock()
	c.started = true
	c.mu.Unlock()
	go c.writeLoop()
	go c.readLoop(ctx)
	return nil
}

func (c *Client) readLoop(ctx context.Context) {
	// The peer's close, a broken stream and a cancelled context all end the
	// connection at once: nothing queued could still be delivered.
	defer c.abort()

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.closeCh:
			return
		default:
		}

		_ = c.conn.SetReadDeadline(time.Now().Add(c.readPoll))

		msgType, body, err := c.readMessage()
		if err != nil {
			var nerr net.Error
			if errors.As(err, &nerr) && nerr.Timeout() {
				continue
			}
			return
		}
		if c.counter != nil {
			// One count per message, on the transport that read it: BytesReceived
			// used to be added only for GameData bodies, so every other message of
			// this transport was invisible in the traffic counters.
			c.counter.AddToCounter("BytesReceived", float64(len(body)+1))
		}

		c.mu.Lock()
		closing := c.closed
		if !closing {
			c.lastSeen = time.Now()
		}
		c.mu.Unlock()
		if closing {
			// A server-side close is delivering the queue. What the peer sends
			// meanwhile is still read, so its bytes do not sit unread in the
			// socket when it closes, but it is no longer acted on.
			continue
		}

		c.handleMessage(msgType, body)
	}
}

func (c *Client) setMetricsCounter(m metricsCounter) {
	c.counter = m
}

// writeLoop moves the outbound queue to the socket in FIFO order, one batch per
// wake-up and one flush per batch. It ends when the socket closes, or after it
// has written the last batch of a server-side close, which it closes itself.
func (c *Client) writeLoop() {
	var batch []outboundMsg
	for {
		select {
		case <-c.sendSignal:
		case <-c.closeCh:
			return
		}

		c.mu.Lock()
		batch, c.sendQueue = c.sendQueue, batch[:0]
		batchBytes := c.queuedBytes
		last := c.draining
		c.mu.Unlock()

		err := c.writeBatch(batch)
		clear(batch)
		c.mu.Lock()
		c.queuedBytes -= batchBytes
		c.mu.Unlock()
		if err != nil {
			c.writeFailed(err)
			return
		}
		if last {
			// Everything queued before the close has been written: the close can
			// complete. A frame cannot have been queued after it, see enqueue.
			c.abort()
			return
		}
	}
}

// writeBatch writes the frames in order and flushes them. Pooled frames go back
// to the pool once copied into the writer or written through it.
func (c *Client) writeBatch(batch []outboundMsg) error {
	for _, m := range batch {
		if _, err := c.writer.Write(m.data); err != nil {
			return err
		}
		c.countSent(len(m.data))
		if m.pooled {
			framePool.Put(m.data)
		}
	}
	return c.writer.Flush()
}

// writeFailed handles a write that returned an error. The peer's stream is then
// at an unknown offset and no later frame could be delivered in order, so the
// connection closes at once. A write that fails because the socket is already
// closed needs no log line; any other is logged with its cause.
func (c *Client) writeFailed(err error) {
	c.mu.Lock()
	gone := c.socketClosed
	c.mu.Unlock()
	if !gone {
		logger.Warning("Client", "send", "closing connection, write failed: %v from=%s", err, c.RemoteAddr())
	}
	c.abort()
}

func (c *Client) countSent(n int) {
	if c.counter != nil {
		c.counter.AddToCounter("BytesSent", float64(n))
	}
}

// readMessage returns the next complete bsPort frame, [u16 BE length][type][body].
// The frame is assembled in readBuf across calls. A read that ends early on a
// timeout keeps the bytes it got, and the next call continues the same frame, so
// the stream stays in step however long the peer stalls in the middle of a
// frame. Any other error ends the stream, and the partial frame with it.
//
// The returned body aliases readBuf: handlers consume it synchronously and copy
// before retaining it, so the next call can reuse the buffer.
func (c *Client) readMessage() (byte, []byte, error) {
	if c.readComplete {
		c.readBuf = c.readBuf[:0]
		c.readComplete = false
	}
	if err := c.fillFrame(2); err != nil {
		return 0, nil, err
	}

	length := int(binary.BigEndian.Uint16(c.readBuf[:2]))
	if length == 0 {
		c.readBuf = c.readBuf[:0]
		return 0, nil, errors.New("empty message")
	}

	if err := c.fillFrame(2 + length); err != nil {
		return 0, nil, err
	}
	c.readComplete = true
	return c.readBuf[2], c.readBuf[3:], nil
}

// fillFrame reads until readBuf holds n bytes. On a timeout the bytes read so far
// stay in readBuf; on any other error readBuf is emptied.
func (c *Client) fillFrame(n int) error {
	if cap(c.readBuf) < n {
		grown := make([]byte, len(c.readBuf), n)
		copy(grown, c.readBuf)
		c.readBuf = grown
	}
	for len(c.readBuf) < n {
		got, err := c.reader.Read(c.readBuf[len(c.readBuf):n])
		c.readBuf = c.readBuf[:len(c.readBuf)+got]
		if err == nil || len(c.readBuf) == n {
			continue
		}
		var nerr net.Error
		if !errors.As(err, &nerr) || !nerr.Timeout() {
			c.readBuf = c.readBuf[:0]
		}
		return err
	}
	return nil
}

func (c *Client) Send(data []byte) error {
	return c.enqueue(outboundMsg{data: data})
}

// enqueue appends m to the outbound queue and wakes the writer. It never waits
// and never drops a frame on an open connection: while the peer reads slowly the
// queue grows, up to maxQueuedBytes. The send that would cross that cap closes
// the connection instead, with a log line, and is refused.
func (c *Client) enqueue(m outboundMsg) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errors.New("client closed")
	}
	if c.queuedBytes+len(m.data) > c.maxQueuedBytes {
		queued := c.queuedBytes
		first := c.abortLocked()
		c.mu.Unlock()
		logger.Warning("Client", "send", "closing connection, %d bytes queued and the peer is not reading them from=%s",
			queued, c.RemoteAddr())
		// A send can come from a goroutine that holds a session lock (the 0x0A
		// broadcast, the observer step), and RemoveClient takes that lock again,
		// so the removal runs on its own goroutine.
		if first {
			go c.host.RemoveClient(c)
		}
		return errors.New("send queue full")
	}
	c.sendQueue = append(c.sendQueue, m)
	c.queuedBytes += len(m.data)
	c.mu.Unlock()

	select {
	case c.sendSignal <- struct{}{}:
	default:
	}
	return nil
}

func buildFrame(buf []byte, msgType byte, body []byte) []byte {
	// bsPort framing (RLink over TCP): [u16 BE length][type][body]
	// length field = 1 (type) + len(body). Max body = 65534.
	buf = append(buf[:0], 0, 0, msgType)
	buf = append(buf, body...)
	binary.BigEndian.PutUint16(buf[:2], uint16(1+len(body)))
	return buf
}

func frameMessage(msgType byte, body []byte) []byte {
	return buildFrame(make([]byte, 0, 2+1+len(body)), msgType, body)
}

func (c *Client) SendMessage(msgType byte, body []byte) error {
	if len(body) > 65534 { // max body for a u16-BE length field
		return fmt.Errorf("message too large: %d", len(body))
	}
	frame := buildFrame(framePool.Get().([]byte), msgType, body)
	if err := c.enqueue(outboundMsg{data: frame, pooled: true}); err != nil {
		framePool.Put(frame)
		return err
	}
	return nil
}

func (c *Client) handleMessage(msgType byte, body []byte) {
	c.host.dispatchMessage(c, msgType, body)
}

func (c *Client) pingStarted() bool {
	return c.pinged.Load()
}

func (c *Client) setPingStarted() {
	c.pinged.Store(true)
}

// lobbyPingTick sends the server's own LobbyPing (0x14) keepalive at most
// once per lobbyPingInterval. The body is the server's monotonic counter
// (u32 LE) followed by four zero bytes.
func (c *Client) lobbyPingTick(now time.Time, counter uint32) {
	c.mu.Lock()
	if now.Sub(c.lastPing) < lobbyPingInterval {
		c.mu.Unlock()
		return
	}
	c.lastPing = now
	c.mu.Unlock()

	var body [8]byte
	binary.LittleEndian.PutUint32(body[:4], counter)
	_ = c.SendMessage(protocol.MsgLobbyPing, body[:])
}

// setTimeoutOverride records a LobbyConnect's timeoutOverride, which is the
// connection's silent timeout in milliseconds; 0 keeps defaultSilentTimeout.
func (c *Client) setTimeoutOverride(v uint32) {
	c.mu.Lock()
	c.timeoutOverride = v
	c.silentTimeout = time.Duration(v) * time.Millisecond
	c.mu.Unlock()
}

// SetObserver is a no-op: observers only exist on the WebSocket transport; raw
// TCP clients are never observers.
func (c *Client) SetObserver(_ bool) {}

func (c *Client) IsObserver() bool { return false }

// SetPeerIdentifier stores the connection's observer identity. Every connection
// starts at 0 and the StartObserve request overwrites it, so it exists on this
// transport too even though a raw TCP client is never an observer.
func (c *Client) SetPeerIdentifier(v uint64) { c.peerIdentifier.Store(v) }

// PeerIdentifier reads it back. See session.PeerConnection for why the observer
// list is keyed on it.
func (c *Client) PeerIdentifier() uint64 { return c.peerIdentifier.Load() }

// ClientMessageVersion is the version the client announced in its LobbyConnect,
// written from the message's own field and read by the observer end-of-game
// marker. Zero until a LobbyConnect announces one, which is what an observer
// connection that never joined a session has.
func (c *Client) ClientMessageVersion() uint16 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.clientMessageVersion
}

func (c *Client) setClientMessageVersion(v uint16) {
	c.mu.Lock()
	c.clientMessageVersion = v
	c.mu.Unlock()
}

// OverrideSilentTimeout replaces defaultSilentTimeout for this connection: from
// now on Process closes it once more than d has passed since its last complete
// message, or since its accept if it has sent none. A d of zero or less restores
// the default.
func (c *Client) OverrideSilentTimeout(d time.Duration) {
	c.mu.Lock()
	c.silentTimeout = max(d, 0)
	c.mu.Unlock()
}

// Process closes a silent connection at once. The limit is the timeout override,
// or defaultSilentTimeout without one; the connection is silent when more than
// the limit has passed between its last complete message and now.
func (c *Client) Process(_ int, now time.Time) {
	c.mu.Lock()
	limit := defaultSilentTimeout
	if c.silentTimeout > 0 {
		limit = c.silentTimeout
	}
	silentFor := now.Sub(c.lastSeen)
	c.mu.Unlock()
	if silentFor > limit {
		logger.Info("Client", "Process", "closing connection, no complete message for %v (limit %v) from=%s",
			silentFor.Round(time.Millisecond), limit, c.RemoteAddr())
		c.abort()
	}
}

// Close is a server-side close. The connection takes no new frame from here on
// and leaves the host at once, but every frame queued before the call still
// reaches the peer: the writer delivers the queue and then closes the socket.
// The delivery gets closeDrainTimeout; a peer that has not taken the queue by
// then is cut off. A connection that was never started has no writer and closes
// at once.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	if c.started {
		c.draining = true
		_ = c.conn.SetWriteDeadline(time.Now().Add(closeDrainTimeout))
	} else {
		c.closeSocketLocked()
	}
	c.mu.Unlock()

	select {
	case c.sendSignal <- struct{}{}:
	default:
	}
	c.host.RemoveClient(c)
	return nil
}

// abort closes the connection at once and drops whatever is still queued. It is
// the close for a connection nothing can or need be delivered to: the peer
// closed or its stream broke, a write failed, the queue overflowed, the context
// ended, the connection went silent, or a server-side close finished delivering.
func (c *Client) abort() {
	c.mu.Lock()
	first := c.abortLocked()
	c.mu.Unlock()

	if first {
		c.host.RemoveClient(c)
	}
}

// abortLocked closes the socket and drops the queue, and reports whether this
// call closed the connection, in which case the caller removes it from the host.
// Callers must hold c.mu.
func (c *Client) abortLocked() bool {
	wasClosed := c.closed
	c.closed = true
	c.closeSocketLocked()
	c.sendQueue = nil
	return !wasClosed
}

// closeSocketLocked closes the socket once. Callers must hold c.mu.
func (c *Client) closeSocketLocked() {
	if c.socketClosed {
		return
	}
	c.socketClosed = true
	close(c.closeCh)
	_ = c.conn.Close()
}

func (c *Client) RemoteAddr() string {
	return c.conn.RemoteAddr().String()
}

func (c *Client) SetDeadline(t time.Time) error {
	return c.conn.SetDeadline(t)
}

func (c *Client) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

func (c *Client) SetWriteDeadline(t time.Time) error {
	return c.conn.SetWriteDeadline(t)
}
