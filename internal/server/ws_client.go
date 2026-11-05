package server

import (
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lxzan/gws"

	"github.com/luskaner/ageBattleServer/internal/logger"
	"github.com/luskaner/ageBattleServer/internal/protocol"
)

type WsClient struct {
	lastSeen time.Time
	lastPing time.Time
	host     ClientHost
	counter  metricsCounter
	conn     *gws.Conn
	incoming [][]byte
	// silentTimeout is the timeout override, 0 while defaultSilentTimeout
	// applies.
	silentTimeout   time.Duration
	mu              sync.Mutex
	timeoutOverride uint32
	// clientMessageVersion is the connection's announced client message version; see
	// ClientMessageVersion.
	clientMessageVersion uint16
	observerSkip         uint32
	skipMS               uint32
	// pinged is set by the first replay-data upload and read on every upload, and
	// observer is read on every dispatch and every upload. Both are atomics rather
	// than fields behind mu: they were two lock acquisitions per message on the
	// busiest path in a real match.
	pinged   atomic.Bool
	observer atomic.Bool
	closed   bool
	// dispatchMu serializes Process and Close. A Close that arrives while a
	// handler is being dispatched cannot take it (the handler may be the one
	// asking for the close, as StopObserve is), so it sets closePending and
	// Process performs the close once the handler returns. dispatching is read
	// without the lock to tell those two cases apart.
	dispatchMu   sync.Mutex
	dispatching  atomic.Bool
	closePending atomic.Bool
	// peerIdentifier is the connection's observer identity, the u64 the observer
	// list is keyed on (session.PeerConnection). Atomic because the session
	// writes it from the message loop while the read goroutine is also on the
	// connection.
	peerIdentifier atomic.Uint64
}

// maxIncomingFrames caps the per-client incoming queue (incoming WS messages
// sit in the queue until the processing pass picks them up).
const maxIncomingFrames = 4096

// wsFramePool reuses the per-frame copies made in enqueue: the gws callback only
// owns message.Bytes() for its duration, so each frame is copied once, queued,
// consumed synchronously by Process and returned here.
var wsFramePool = sync.Pool{
	New: func() any { return make([]byte, 0, 256) },
}

// wsSendPool reuses the per-message [type][body] buffers built in SendMessage.
// Unlike the incoming frames, these are written synchronously by Send (gws
// WriteMessage), so they are returned right after.
var wsSendPool = sync.Pool{
	New: func() any { return make([]byte, 0, 64) },
}

func NewWsClient(conn *gws.Conn, host ClientHost, counter metricsCounter, observerSkip uint32) *WsClient {
	wc := &WsClient{
		conn:     conn,
		host:     host,
		counter:  counter,
		lastSeen: time.Now(),
		lastPing: time.Now(),
		skipMS:   observerSkip,
	}
	host.AddClient(wc)
	return wc
}

// Send writes bytes verbatim to the socket as one WebSocket binary frame, for
// every client including observers. Every outbound send is framed for the
// connection's own transport, and the per-peer send used by the observer update
// asks for exactly that framing. The same framing applies to the out-of-band
// echo on a port that is WebSocket over TLS. An observer is therefore not a raw
// stream: this used to bypass the WebSocket layer and prepend a [u16 BE length],
// which was a guess with no capture behind it, and the guess was wrong.
func (c *WsClient) Send(data []byte) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errors.New("client closed")
	}
	c.mu.Unlock()

	if err := c.conn.WriteMessage(gws.OpcodeBinary, data); err != nil {
		return err
	}
	c.counter.AddToCounter("BytesSent", float64(len(data)))
	return nil
}

func (c *WsClient) SendMessage(msgType byte, body []byte) error {
	// The frame layer already delimits messages, so the payload is the bare
	// [type][body] with no length prefix, exactly as captured on the wire
	// (82 7E 01 9E + 0A FE ...). Observers get the same treatment: see Send.
	msg := wsSendPool.Get().([]byte)
	msg = append(msg[:0], msgType)
	msg = append(msg, body...)
	err := c.Send(msg)
	wsSendPool.Put(msg)
	return err
}

func (c *WsClient) Close() error {
	// A handler being dispatched may be the caller (StopObserve closes its own
	// connection). Taking dispatchMu there would deadlock, so the close is
	// deferred to Process, which owns the gate.
	if c.dispatching.Load() {
		c.closePending.Store(true)
		if c.dispatching.Load() {
			return nil
		}
		// Process finished between the two reads: the pending flag may have been
		// set after its last check, so clear it and close here.
		c.closePending.Store(false)
	}

	c.dispatchMu.Lock()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		c.dispatchMu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	c.dispatchMu.Unlock()

	// Frames queued but not yet dispatched belong to no match any more: return
	// their buffers to the pool instead of leaving them for a Process that will
	// refuse them.
	c.dropIncoming()

	c.host.RemoveClient(c)
	if c.conn != nil {
		_ = c.conn.WriteClose(1000, nil)
	}
	return nil
}

// dropIncoming discards every queued frame and returns its buffer to the pool.
// Close uses it, and it is separate so the drain can be tested without a socket.
func (c *WsClient) dropIncoming() {
	c.mu.Lock()
	frames := c.incoming
	c.incoming = nil
	c.mu.Unlock()
	for _, data := range frames {
		wsFramePool.Put(data)
	}
}

func (c *WsClient) RemoteAddr() string {
	return c.conn.RemoteAddr().String()
}

// OverrideSilentTimeout replaces defaultSilentTimeout for this connection: from
// now on Process closes it once more than d has passed since its last complete
// message, or since its accept if it has sent none. Every binary message is
// complete when it arrives, so each one restarts the count. A d of zero or less
// restores the default.
func (c *WsClient) OverrideSilentTimeout(d time.Duration) {
	c.mu.Lock()
	c.silentTimeout = max(d, 0)
	c.mu.Unlock()
}

// Process first closes a silent connection: the limit is the timeout override,
// or defaultSilentTimeout without one, and the connection is silent
// when more than the limit has passed between its last message and now. A live
// connection then has its queued messages dispatched.
func (c *WsClient) Process(maxPackets int, now time.Time) {
	c.dispatchMu.Lock()
	c.dispatching.Store(true)

	c.mu.Lock()
	if c.closed {
		// A frame that arrived before the close but is processed after it must
		// not reach a handler: the connection is no longer part of the match.
		frames := c.incoming
		c.incoming = nil
		c.mu.Unlock()
		c.dispatching.Store(false)
		c.dispatchMu.Unlock()
		for _, data := range frames {
			wsFramePool.Put(data)
		}
		return
	}
	limit := defaultSilentTimeout
	if c.silentTimeout > 0 {
		limit = c.silentTimeout
	}
	if silentFor := now.Sub(c.lastSeen); silentFor > limit {
		c.mu.Unlock()
		c.dispatching.Store(false)
		c.dispatchMu.Unlock()
		logger.Info("WsTransport", "Process", "closing connection, no message for %v (limit %v) from=%s",
			silentFor.Round(time.Millisecond), limit, c.RemoteAddr())
		_ = c.Close()
		return
	}
	// An observer client with no pending data skips the whole socket processing
	// while a per-client counter is below the configured skip; the counter resets
	// and messages are processed otherwise. The period is skip+1 ticks. The skip
	// gates only idle clients: pending data is processed immediately, so under
	// event-driven processing (P1) the skip only affects the coarse safety-net
	// polling.
	if c.observer.Load() && c.skipMS > 0 && len(c.incoming) == 0 {
		c.observerSkip++
		if c.observerSkip < c.skipMS {
			c.mu.Unlock()
			c.dispatching.Store(false)
			c.dispatchMu.Unlock()
			if c.closePending.Swap(false) {
				_ = c.Close()
			}
			return
		}
		c.observerSkip = 0
	}
	frames := c.incoming
	if len(frames) > maxPackets {
		frames = frames[:maxPackets]
	}
	limitHit := len(frames) == maxPackets
	c.incoming = c.incoming[len(frames):]
	c.mu.Unlock()

	for i, data := range frames {
		c.mu.Lock()
		closed := c.closed
		c.mu.Unlock()
		if closed {
			c.dropQueued(frames[i:])
			c.dispatching.Store(false)
			c.dispatchMu.Unlock()
			return
		}
		msgType, body, ok := parseWsMessage(data)
		if !ok {
			logger.Debug("WsTransport", "onMessage", "malformed message %x from=%s", data, c.RemoteAddr())
			wsFramePool.Put(data)
			continue
		}
		c.host.dispatchMessage(c, msgType, body)
		wsFramePool.Put(data)
		if c.closePending.Load() {
			// A handler asked to close this connection. Finish the batch here,
			// release the gate and let Close run: it cannot take dispatchMu from
			// inside the dispatch that is asking for it.
			c.dropQueued(frames[i+1:])
			c.dispatching.Store(false)
			c.dispatchMu.Unlock()
			_ = c.Close()
			return
		}
	}

	c.dispatching.Store(false)
	c.dispatchMu.Unlock()

	// A Close that arrived from another goroutine while the batch was being
	// dispatched may have seen dispatching=true and deferred to this point.
	if c.closePending.Swap(false) {
		_ = c.Close()
		return
	}

	// The maxPackets cap may have left frames queued. Re-signal so the next
	// batch is drained immediately instead of waiting for another message or the
	// coarse safety net. Only fires when the cap was actually hit (leftover is
	// impossible otherwise).
	if limitHit {
		c.mu.Lock()
		leftover := len(c.incoming) > 0
		c.mu.Unlock()
		if leftover {
			c.host.NotifyWork(c)
		}
	}
}

// dropQueued returns a batch of frames and whatever is still queued to the pool.
// It is used when a close is pending or the connection is already closed, and it
// empties incoming so the close path cannot return the same buffer twice.
func (c *WsClient) dropQueued(frames [][]byte) {
	c.mu.Lock()
	rest := c.incoming
	c.incoming = nil
	c.mu.Unlock()
	for _, data := range frames {
		wsFramePool.Put(data)
	}
	for _, data := range rest {
		wsFramePool.Put(data)
	}
}

// enqueue buffers a received WS frame for processing by Process. The incoming
// message sits in the queue until the processing pass picks it up; here the
// callback only queues the frame. The main loop is signaled on the
// empty->non-empty transition so each queued batch is processed exactly once.
func (c *WsClient) enqueue(data []byte) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.lastSeen = time.Now()
	if len(c.incoming) >= maxIncomingFrames {
		c.mu.Unlock()
		logger.Debug("WsTransport", "onMessage", "dropping message, queue full from=%s", c.RemoteAddr())
		return
	}
	frame := wsFramePool.Get().([]byte)
	frame = append(frame[:0], data...)
	wasEmpty := len(c.incoming) == 0
	c.incoming = append(c.incoming, frame)
	c.mu.Unlock()

	// Signal the host outside the lock: a blocking main loop must never stall a
	// client's read goroutine, and the signal must not deadlock against Process.
	if wasEmpty {
		c.host.NotifyWork(c)
	}
}

func (c *WsClient) SetObserver(v bool) {
	c.observer.Store(v)
	c.mu.Lock()
	c.observerSkip = 0
	c.mu.Unlock()
}

func (c *WsClient) IsObserver() bool { return c.observer.Load() }

// SetPeerIdentifier stores the connection's observer identity. Every connection
// starts at 0 and the StartObserve request overwrites it; on this transport it is
// the field both observer lookups compare.
func (c *WsClient) SetPeerIdentifier(v uint64) { c.peerIdentifier.Store(v) }

// PeerIdentifier reads it back.
func (c *WsClient) PeerIdentifier() uint64 { return c.peerIdentifier.Load() }

// ClientMessageVersion is the version the client announced in its LobbyConnect,
// written from the message's own field and read by the observer end-of-game
// marker. Zero until a LobbyConnect announces one, which is the case for an
// observer: the observer connections of the committed captures connect and go
// straight to 0x1C, and the recorded server left them at 0 too.
func (c *WsClient) ClientMessageVersion() uint16 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.clientMessageVersion
}

func (c *WsClient) setClientMessageVersion(v uint16) {
	c.mu.Lock()
	c.clientMessageVersion = v
	c.mu.Unlock()
}

func (c *WsClient) touch() {
	c.mu.Lock()
	c.lastSeen = time.Now()
	c.mu.Unlock()
}

func (c *WsClient) pingStarted() bool { return c.pinged.Load() }

func (c *WsClient) setPingStarted() { c.pinged.Store(true) }

func (c *WsClient) lobbyPingTick(now time.Time, counter uint32) {
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
func (c *WsClient) setTimeoutOverride(v uint32) {
	c.mu.Lock()
	c.timeoutOverride = v
	c.silentTimeout = time.Duration(v) * time.Millisecond
	c.mu.Unlock()
}

// parseWsMessage splits one binary WebSocket message into its type and payload.
// On webSocketPort the WebSocket frame delimits the message, so the message is
// [type][payload] and nothing else: the game sends no length prefix (every
// committed capture), and none is guessed at. A guess would misread any message
// whose first two bytes happen to equal its length minus two, which is how a
// 2-byte 00 00 crashed the server and a 4101-byte 0x10 starting 10 03 was
// handled as a 0x07. An empty message has no type and is rejected.
func parseWsMessage(data []byte) (byte, []byte, bool) {
	if len(data) < 1 {
		return 0, nil, false
	}
	return data[0], data[1:], true
}
