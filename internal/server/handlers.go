package server

import (
	"encoding/binary"
	"errors"
	"time"

	"github.com/luskaner/ageBattleServer/internal/logger"
	"github.com/luskaner/ageBattleServer/internal/protocol"
	"github.com/luskaner/ageBattleServer/internal/session"
)

func (s *Server) dispatchMessage(client messageClient, msgType byte, body []byte) {
	s.dispatch(client, msgType, body, 0)
}

// maxNestedMessages bounds how deep messages may nest inside each other: a relay
// (0x13) carries a nested message, and so does a 0x17. The original accepts no
// message over 65535 bytes, and every nesting level costs at least two bytes, so
// no message it accepts nests deeper than this. The bound only stops a larger
// message from growing the dispatch recursion, and the goroutine stack with it,
// without limit.
const maxNestedMessages = 65535 / 2

// dispatch runs one message through the dispatcher. depth is the number of
// messages that enclose it: 0 for a message a client sent on its own, one more
// for each relay or 0x17 it is nested in. A nested message passes the same gates
// and reaches the same handlers as a top-level one, as if its sender had sent it
// directly.
//
// The result is the relay verdict, and only a relay reads it: true asks for the
// enclosing relay to be forwarded. Only a peer message (0x0F) can answer true,
// through handlePeer; every other type answers false.
func (s *Server) dispatch(client messageClient, msgType byte, body []byte, depth int) bool {
	if depth > maxNestedMessages {
		logger.Debug("Client", "dispatch", "dropping type=0x%02x nested %d messages deep from=%s", msgType, depth, client.RemoteAddr())
		return false
	}

	// A client that is registered as an observer may only send the five types
	// observerMessageAllowed lists. The gate applies to nested messages too; it
	// never fires there, because an observer cannot send the two types that nest.
	if client.IsObserver() && !observerMessageAllowed(msgType) {
		logger.Debug("Client", "dispatch", "dropping type=0x%02x from observer from=%s", msgType, client.RemoteAddr())
		return false
	}

	if logger.ShouldLog(logger.LevelDebug) {
		if len(body) <= 64 {
			logger.Debug("Client", "dispatch", "type=0x%02x length=%d body=%x from=%s", msgType, len(body), body, client.RemoteAddr())
		} else {
			logger.Debug("Client", "dispatch", "type=0x%02x length=%d from=%s", msgType, len(body), client.RemoteAddr())
		}
	}

	switch msgType {
	case protocol.MsgLobbyConnect:
		s.handleLobbyConnect(client, body)
	case protocol.MsgLobbyRelay:
		s.handleLobbyRelay(client, body, depth)
	case protocol.MsgGameData:
		s.handleGameData(client, body)
	case protocol.MsgGameDataChecksum:
		s.handleLobbyChecksumAck(client, body, depth)
	case protocol.MsgIntegrityCheck:
		s.handleIntegrityCheck(client, body)
	case protocol.MsgPeer:
		// A top-level 0x0F forwards nothing whatever its verdict; the verdict
		// travels back only to a relay that carried this message.
		return s.handlePeer(client, body)
	case protocol.MsgProxy:
		s.handleProxyData(client, body)
	case protocol.MsgLobbyPing:
		s.handleLobbyPing(client, body)
	case protocol.MsgStartObserve, 0x1C:
		// 0x1C is the versioned StartObserveRequest: one handler, and the type
		// byte selects the layout.
		s.handleStartObserve(client, msgType, body)
	case protocol.MsgActivateObserver:
		s.handleActivateObserver(client, body)
	case protocol.MsgStopObserve:
		s.handleStopObserve(client, body)
	case protocol.MsgSetObservableParameters:
		s.handleSetObservableParameters(client, body)
	case protocol.MsgLeaveSimulation:
		s.handleLeaveSimulation(client, body)
	default:
		// The dispatcher has cases only for 0x09, 0x0F, 0x10, 0x13, 0x14,
		// 0x17, 0x18/0x1C, 0x1A, 0x1B, 0x1D and 0x1F, plus 0x15 which is handled
		// before the switch; everything else lands here and is only logged, as a
		// warning. Types 0x07 GameVoice, 0x08 MultiGameData, 0x0B Nudge, 0x0C
		// RouteCheck, 0x0D Seek, 0x0E SeekResponse and 0x11 Ping are therefore
		// *not* handled: the server never acts on them. Only the type is logged,
		// not the body, so an unhandled page-sized message does not turn into a
		// hex dump.
		//
		// 0x01 RequestJoin is in this list too, and it is the one that used not to
		// be. There is NO case for a top-level 0x01, and the routine that does
		// what a RequestJoin asks for is reached from exactly one place: the
		// type-2 case of the INTERNAL peer switch, which itself is reached from
		// the 0x0F case and from the recursive dispatch of the 0x13. A client that
		// sent a top-level 0x01 got here a session, a registered peer and an
		// AddPeer broadcast, where the server logs the type and returns having
		// touched nothing. TestTopLevelRequestJoinIsAnUnknownMessageType is what
		// keeps it that way.
		logger.Warning("Client", "dispatch", "unhandled message type 0x%02x received from=%s length=%d",
			msgType, client.RemoteAddr(), len(body))
	}
	return false
}

// observerMessageAllowed reports whether an observer client may send the message
// type. It is a fixed set of five, and not a bitmask test written out longhand:
// 0x14, 0x18, 0x1A, 0x1B and 0x1C, with every type above 0x1C refused.
//
// The branch guards observers, not the other way round: the rejection it logs is
// about "invalid message types from observer peer", and the alternative reading is
// impossible anyway, since it would reject 0x09 GameData, which game clients send
// continuously.
//
// Mapped through the dispatcher's own classes (each one's declared size equals
// its type id), those five types are exactly an observer's lifecycle:
// 0x14 LOBBYPING keepalive, 0x18 StartObserveRequest, 0x1A ActivateObserver,
// 0x1B StopObserve and 0x1C StartObserveRequest Vx.
func observerMessageAllowed(msgType byte) bool {
	switch msgType {
	case protocol.MsgLobbyPing, protocol.MsgStartObserve, 0x1C,
		protocol.MsgActivateObserver, protocol.MsgStopObserve:
		return true
	}
	return false
}

func (s *Server) handleLobbyConnect(client messageClient, body []byte) {
	info, ok := protocol.ParseLobbyConnect(body)
	if !ok {
		// A LobbyConnect that does not parse is returned without echoing, so a
		// malformed connect produces no session and no reply.
		logger.Debug("Server", "dispatch", "malformed LOBBYCONNECT (%d bytes) from=%s", len(body), client.RemoteAddr())
		return
	}
	// The secret is not logged: it is a credential, and this server does not
	// validate it either way (trusted-LAN v1, recorded in SESSION-JOURNAL.md
	// 16.1). Logging it serves no purpose.
	logger.Debug("Server", "dispatch", "LOBBYCONNECT from=%s titleID=%d sessionID=%d dataReceivedCount=%d clientMessageVersion=%d timeoutOverride=%d",
		client.RemoteAddr(), info.TitleID, info.SessionID, info.DataReceivedCount, info.ClientMessageVersion, info.TimeoutOverride)
	key := session.SessionKey{
		TitleID:   int16(info.TitleID),
		SessionID: int64(info.SessionID),
	}
	// A non-zero reconnect state resumes a peer that already exists, and nothing
	// else: it never creates a session or a peer.
	if info.ReconnectState != 0 {
		s.handleLobbyReconnect(client, body, info, key)
		return
	}
	if s.config.AllowTimeoutOverride {
		client.setTimeoutOverride(info.TimeoutOverride)
	}
	// It is kept on the connection and not only on the peer because the observer
	// end-of-game marker reads it off the observer's connection, and an observer
	// that connected, sent a LobbyConnect and only then observed still has it.
	client.setClientMessageVersion(info.ClientMessageVersion)
	// The session is created/joined on LobbyConnect, keyed by the connect
	// message's sessionID; the original creates it on the RequestJoin that
	// follows (INTENTIONAL-DIVERGENCES.md 1.3).
	sess := s.sessionManager.GetOrCreateSession(key)
	if sess == nil {
		logger.Warning("Server", "dispatch", "max session limit reached, rejecting LOBBYCONNECT from=%s", client.RemoteAddr())
		return
	}
	sess, ok = s.connectPeer(sess, key, client, info.Secret)
	if !ok {
		return
	}

	// The observable flag is NOT set here. Its only two writers are the session
	// constructor (which writes 0) and the 0x1D handler, from that message's
	// first u32. The connect handler does not mention the field at all.
	//
	// An earlier revision set it from the connect's reconnectState, which was a
	// misattribution and made two sites fight over one field: the reconnect state
	// selects the reconnect branch of the connect handler, it is not the
	// observable flag. See SetObservableParameters.
	//
	// The constructor's value of 0 is not reproduced: our NewSession defaults to
	// enabled, because whether the game actually sends 0x1D is unconfirmed and
	// defaulting to disabled would reject every observer with status 4 on any
	// setup that does not send it. This is the single highest-risk unknown left;
	// see SESSION-JOURNAL.md.
	// The announced clientMessageVersion is the field both version gates read.
	// Record it on the peer so those gates can be implemented from the handshake.
	if peerID := sess.PeerIDForConn(client); peerID != 0 {
		sess.SetPeerClientMessageVersion(peerID, info.ClientMessageVersion)
		// The timeout override is the peer's sync-timeout deadline in ms, and it
		// replaces the 60 s default. Only carried with -allowTimeoutOverride.
		if s.config.AllowTimeoutOverride {
			sess.SetPeerTimeoutOverride(peerID, info.TimeoutOverride)
		}
	}

	_ = client.SendMessage(protocol.MsgLobbyConnect, body)
}

// handleLobbyReconnect is a LobbyConnect whose reconnect state is not 0: the
// client resumes, on a new connection, the peer that holds its secret.
//
// When there is no such session, or no peer in it holds the secret, nothing is
// stored on the connection, nothing is echoed, no session is created, and the
// connection is closed.
//
// Otherwise only the socket moves. The peer keeps its slot, its per-peer fields
// and its session, and the new connection takes over its relay route
// (Manager.ReconnectPeer), so everything the connection sends from now on is
// that peer's traffic. The connection it replaced is closed at once; its close
// takes nothing out of the session, because the route has already moved. Then:
//
//	state 1      -> the connect is echoed, done
//	other states -> the resend range [count, frame count - 1] is checked first.
//	                Served: the reply goes out, then the frames, in that order.
//	                Refused: no reply, and the connection is closed.
//
// States 3 and up carry no count. The parser leaves -1 there, which is an
// invalid range, so those states always end in the close.
//
// The reply is the client's own message with one field rewritten: the
// dataReceivedCount becomes the peer's accepted sync-message count, the number
// of 0xFE uploads the server took from that peer. So the client learns its own
// upload count on the way back in. No committed capture contains a reconnect.
//
// The connection-level copies of the announced version and of the timeout
// override are not written here. A reconnect moves only the socket, so the
// values the peer stored at its first connect stay the ones that apply.
// connectPeer gives a connecting client its peer in sess, the session the
// manager handed out for key, and returns the session the peer is in.
func (s *Server) connectPeer(sess *session.Session, key session.SessionKey, client messageClient, secret uint64) (*session.Session, bool) {
	// Apply the observer limits the same way RequestJoin does: the session
	// is created here in the real flow, and observers subscribe to it, so a
	// session without the observer config would reject every StartObserve
	// with "max observers" even though ObserverMax > 0.
	sess.SetObserverConfig(int(s.config.ObserverMax), s.observerUpdateIntervalTicks())
	if sess.PeerIDForConn(client) != 0 {
		return sess, true
	}
	peerID := sess.NextPeerID()
	err := s.sessionManager.AddPeer(sess, sess.NewPeer(peerID, client))
	if errors.Is(err, session.ErrSessionErased) {
		// The session's last connection closed after the lookup and the session
		// was erased: the live one under the same key is the one to join.
		if live := s.sessionManager.GetOrCreateSession(key); live != nil && live != sess {
			return s.connectPeer(live, key, client, secret)
		}
	}
	if err != nil {
		logger.Warning("Server", "dispatch", "REQUESTJOIN rejected from=%s: %v", client.RemoteAddr(), err)
		return nil, false
	}
	// The secret identifies this peer on a later reconnect.
	sess.SetPeerSecret(peerID, secret)
	logger.Debug("Server", "dispatch", "REQUESTJOIN from=%s peerID=%d", client.RemoteAddr(), peerID)
	return sess, true
}

func (s *Server) handleLobbyReconnect(client messageClient, body []byte, info protocol.LobbyConnectInfo, key session.SessionKey) {
	var replaced session.PeerConnection
	var peer *session.Peer
	resumed := false
	sess := s.sessionManager.GetSession(key)
	if sess != nil {
		peer = sess.PeerBySecret(info.Secret)
		if current := sess.PeerIDForConn(client); current != 0 && peer != nil {
			if current == peer.ID {
				// The original drops a second 0x15 on a connection that already joined.
				// Resuming the peer onto its own connection would close it as replaced.
				logger.Debug("Server", "dispatch", "LOBBYCONNECT reconnect on the connection that holds peerID=%d, ignored from=%s",
					peer.ID, client.RemoteAddr())
				return
			}
			// The connection already holds a different peer, and a connection is one
			// peer at a time: moving the resumed peer here would leave the first one
			// orphaned in the session, with no connection to reach it. Refuse the
			// reconnect and leave the connection as it was. The real client
			// reconnects on a fresh connection, so this path is not in a normal match.
			logger.Debug("Server", "dispatch", "LOBBYCONNECT reconnect refused, connection already holds peerID=%d and the secret resolves to peerID=%d from=%s",
				current, peer.ID, client.RemoteAddr())
			return
		}
	}
	if peer != nil {
		replaced, resumed = s.sessionManager.ReconnectPeer(sess, peer.ID, client, info.Secret)
	}
	if !resumed {
		logger.Debug("Server", "dispatch", "LOBBYCONNECT reconnect refused, no session %d with a peer holding that secret; closing from=%s",
			key.SessionID, client.RemoteAddr())
		_ = client.Close()
		return
	}
	if replaced != nil {
		_ = replaced.Close()
	}
	// The original moves only the socket into the resumed peer, which keeps the
	// override of its first LobbyConnect; this message's own field is not read.
	client.setTimeoutOverride(sess.TimeoutOverrideOf(peer.ID))

	if info.ReconnectState == 1 {
		_ = client.SendMessage(protocol.MsgLobbyConnect, body)
		logger.Debug("Server", "dispatch", "LOBBYCONNECT reconnect session=%d peerID=%d state=1 from=%s",
			sess.ID, peer.ID, client.RemoteAddr())
		return
	}

	// The range starts from the CLIENT's count, read before the reply rewrites it.
	from := int(int32(info.DataReceivedCount))
	frames, servable := sess.ResendFrames(from)
	if !servable {
		logger.Debug("Server", "dispatch", "LOBBYCONNECT reconnect closed, the frame store has no frames from %d on to resend, session=%d peerID=%d state=%d from=%s",
			from, sess.ID, peer.ID, info.ReconnectState, client.RemoteAddr())
		_ = client.Close()
		return
	}
	// Only state 2 gets here, and its message carries the count at this offset.
	reply := append([]byte(nil), body...)
	binary.LittleEndian.PutUint32(reply[17:21], sess.SyncMessagesAccepted(peer.ID))
	_ = client.SendMessage(protocol.MsgLobbyConnect, reply)
	// The frames go back out as they were stored, type included: the store holds
	// the session's 0x0A ack stream.
	for _, f := range frames {
		if err := client.SendMessage(f.Type, f.Body); err != nil {
			logger.Debug("Server", "dispatch", "LOBBYCONNECT reconnect resend stopped by a send error, session=%d from=%s", sess.ID, client.RemoteAddr())
			break
		}
	}
	logger.Debug("Server", "dispatch", "LOBBYCONNECT reconnect session=%d peerID=%d state=%d resent=%d from=%s",
		sess.ID, peer.ID, info.ReconnectState, len(frames), client.RemoteAddr())
}

// lobbyPingEchoBudget is how many 0x14 LOBBYPING messages are echoed to a client
// that has not joined a session yet. The budget starts at 100 and is decremented
// per echo; once it reaches 0 the client is no longer answered, with no error and
// no log. A client that has joined a session is echoed unconditionally, so the
// budget never applies to it.
const lobbyPingEchoBudget = 100

func (s *Server) handleLobbyPing(client ClientConn, body []byte) {
	logger.Debug("Server", "dispatch", "LOBBYPING from=%s", client.RemoteAddr())

	// In a session, or observing one: echo unconditionally. An observer is not a
	// session peer (its entry was removed when it started observing), but the
	// original keeps its connection bound to the session and answers every ping;
	// the budget is only for a connection with no session at all.
	if s.sessionManager.FindSessionByPeer(client) != nil {
		_ = client.SendMessage(protocol.MsgLobbyPing, body)
		return
	}
	s.mu.RLock()
	_, observing := s.observerSessions[client]
	s.mu.RUnlock()
	if observing {
		_ = client.SendMessage(protocol.MsgLobbyPing, body)
		return
	}

	// Not in a session: echo while the budget lasts.
	s.mu.Lock()
	budget, ok := s.pingEchoBudget[client]
	if !ok {
		budget = lobbyPingEchoBudget
	}
	if budget > 0 {
		budget--
		s.pingEchoBudget[client] = budget
		s.mu.Unlock()
		_ = client.SendMessage(protocol.MsgLobbyPing, body)
		return
	}
	s.mu.Unlock()
	logger.Debug("Server", "dispatch", "LOBBYPING echo budget exhausted, not answering from=%s", client.RemoteAddr())
}

// sendEndOfGameDataTicks runs the per-simulation-tick 0x0A response: each
// session that has started sending game data receives one EndOfGameData message
// built from the session-wide ack
// state (a data flush, a standalone checksum ack, or a heartbeat) and sent to
// every player in the session. The server's own lobby ping keepalive still
// runs per client.
//
// PopAckTick has already put that same message in the session's frame store,
// which is where the observers read it from. They are
// deliberately NOT sent a second copy here: an earlier revision broadcast the
// ack to them as well as storing the uploaded 0x09 pages for them, so every
// observer got two unrelated message streams and the two of them were not the
// one an observer is served. Capture 2026-09-29_012528_03cb67c7cf19: an observer
// receives 1346 and 800 messages, all of them 0x0A, all of them
// byte-identical to a message of the player's own 0x0A stream and in order.
func (s *Server) sendEndOfGameDataTicks() {
	s.mu.Lock()
	s.clientSnap = snapshotSlice(s.clientSnap, s.clients)
	s.mu.Unlock()

	s.sessSnap = s.sessionManager.SnapshotSessions(s.sessSnap[:0])
	for _, sess := range s.sessSnap {
		body, notices := sess.PopAckTick()
		if body == nil && len(notices) == 0 {
			continue
		}
		// Session-level ack: every player receives the same body, framed by
		// each transport (bare [type][body] on WebSocket as captured in _6,
		// [u16 len][type][body] on the bsPort).
		//
		// The ack goes out before the desync notices, because reconciliation
		// runs only after the 0x0A has already been sent. Each disagreeing
		// round has its own notice, sent in round order.
		if body != nil {
			sess.SendToAll(protocol.MsgEndOfGameData, body)
		}
		for _, notice := range notices {
			sess.SendToAll(protocol.MsgGameData, notice)
		}
	}

	// No periodic self-pings are sent; the only ping this server ever emits is
	// the echo of an incoming 0x14 LOBBYPING (see handleLobbyPing).
	// 0x11 Ping has no case in the dispatcher and is reported as an unknown
	// message type, so this server does not answer it either. The
	// lobbyPingTick was removed.
}

// simulationTicksPerSec is 1000 / the simulation period, the session's period
// expressed as ticks per second. Both 0x1D and 0x1E derive their divisors this
// way.
func (s *Server) simulationTicksPerSec() int {
	if s.config.SimulationPeriod <= 0 {
		return 8 // default 125 ms period
	}
	return int(1000 / s.config.SimulationPeriod)
}

func (s *Server) handleSetObservableParameters(client ClientConn, body []byte) {
	// The body is u32, u64, u32 and nothing else: 16 bytes exactly. A shorter or
	// longer body does not parse and changes nothing.
	if len(body) != 16 {
		logger.Debug("Server", "dispatch", "SETOBSERVABLEPARAMATERS body of %d bytes, want 16, from=%s", len(body), client.RemoteAddr())
		return
	}
	firstU32 := binary.LittleEndian.Uint32(body[0:4])
	hash := binary.LittleEndian.Uint64(body[4:12])
	thirdU32 := binary.LittleEndian.Uint32(body[12:16])

	sess := s.sessionManager.FindSessionByPeer(client)
	if sess == nil {
		logger.Debug("Server", "dispatch", "SETOBSERVABLEPARAMATERS from a peer with no session from=%s", client.RemoteAddr())
		return
	}
	if sess.SetObservableParameters(firstU32, hash, thirdU32, s.simulationTicksPerSec()) {
		logger.Debug("Server", "dispatch", "SETOBSERVABLEPARAMATERS session=%d observable=%v hash=%#x lagSeconds=%d from=%s",
			sess.ID, firstU32 != 0, hash, thirdU32, client.RemoteAddr())
	} else {
		logger.Debug("Server", "dispatch", "SETOBSERVABLEPARAMATERS ignored, session=%d already recording from=%s", sess.ID, client.RemoteAddr())
	}
}

// proxyRecordSize is the fixed size of the per-destination record that
// terminates every ProxyMessage payload.
const proxyRecordSize = 15

func (s *Server) handleProxyData(client ClientConn, data []byte) {
	// One lookup for the recording hook: the match this data belongs to is
	// the sending peer's session.
	sess := s.sessionManager.FindSessionByPeer(client)
	var sessionID uint64
	if sess != nil {
		sessionID = uint64(sess.ID64())
	}
	if h := s.config.ReplayDataHandler; h != nil {
		h(sessionID, append([]byte(nil), data...))
	}
	if len(data) < 4 {
		return
	}
	// ProxyMessage wire format (after the 0x10 type byte):
	//   [srcPeerID u8][dstCount u16 LE][dstPeerIDs dstCount bytes][dataLen u16 LE][data]
	src := data[0]
	dstCount := int(binary.LittleEndian.Uint16(data[1:3]))
	if dstCount == 0 || 3+dstCount+2 > len(data) {
		return
	}
	if sess == nil {
		return
	}
	dataLen := int(binary.LittleEndian.Uint16(data[3+dstCount : 5+dstCount]))
	payloadStart := 5 + dstCount
	if len(data) < payloadStart+dataLen {
		return
	}
	payload := data[payloadStart : payloadStart+dataLen]
	// Payload framing:
	//   [head: dataLen - proxyRecordSize*dstCount][dstCount × 15-byte records]
	// Each destination receives one relay frame carrying [head + its own
	// record]; a payload too short for its destination count is malformed and
	// dropped whole ("dropping PROXY message").
	tail := proxyRecordSize * dstCount
	if tail > len(payload) {
		return
	}
	head := payload[:len(payload)-tail]

	// A proxied peer message is handled here, not just forwarded: it is parsed
	// and runs its server-side effect. Its verdict is not applied on this path:
	// a RequestJoin, a RequestJoinDenied or a 0xFE GameData sent through a proxy
	// still reaches every listed destination. No client sends those through a
	// proxy (the game sends its control messages in relays and its game data as
	// top-level uploads), and the per-destination handling of a proxy is a
	// divergence of its own, recorded in the register.
	if len(head) > 0 && head[0] == protocol.MsgPeer {
		s.handlePeer(client, head[1:])
	}

	for i := 0; i < dstCount; i++ {
		dst := data[3+i]
		peer := sess.GetPeer(dst)
		if peer == nil {
			continue
		}
		record := payload[len(head)+proxyRecordSize*i:][:proxyRecordSize]
		frame := make([]byte, 0, 6+len(head)+proxyRecordSize)
		frame = append(frame, src, 1, 0, dst)
		frame = binary.LittleEndian.AppendUint16(frame, uint16(len(head)+proxyRecordSize))
		frame = append(frame, head...)
		frame = append(frame, record...)
		if !s.sendToPeer(peer.Conn, protocol.MsgProxy, frame, true) {
			continue
		}
	}
}

// handleGameData is the top-level 0x09, [tag][u16 length][payload], and the 0x09
// a 0x17 carries after its checksum entries. The body must be consumed exactly:
// one whose declared length is not its actual length is dropped whole, because
// copying it into the queue would corrupt the 0x0A every player of the session
// receives. Only a chunk tagged 0xFE is queued, and only for a session peer; it
// rides the next 0x0A tagged with the sender's slot.
func (s *Server) handleGameData(client messageClient, body []byte) {
	msg, err := protocol.ParseGameData(body)
	if err != nil {
		logger.Debug("Server", "dispatch", "GAMEDATA dropped: %v from=%s", err, client.RemoteAddr())
		return
	}

	// BytesReceived is counted once, by the transport that read the message, and
	// not again here: the WebSocket handler already counted the whole message and
	// adding the GameData body counted those bytes twice on that transport.
	if !msg.IsReplayData() {
		return
	}
	if !client.pingStarted() {
		client.setPingStarted()
	}
	sess := s.sessionManager.FindSessionByPeer(client)
	if sess == nil {
		return
	}
	// QueueGameData decides whether the upload is taken at all and counts it
	// when it is; both live there because the two are one rule. See its comment
	// for the peer and participating tests it applies. body[1:] is the
	// [length][payload] the 0x0A carries, and the parse above proved the length.
	sess.QueueGameData(sess.PeerIDForConn(client), body[1:])

	// The frame store is NOT fed here, and this is the one place where getting it
	// wrong is invisible until an observer connects. An earlier revision stored
	// the 0x09 GameData bodies the players upload and served those to the
	// observers; what is appended to the store is the 0x0A the tick just built,
	// and observers are served frames out of that store. The 0x09 itself only ever
	// reaches the ack queue, through the GameDataAck message.
	//
	// Capture 2026-09-29_012528_03cb67c7cf19 is the evidence: the observers
	// received 1346 and 800 messages, every one a byte-identical copy of
	// a message of the player's 0x0A stream in the same order, and not one 0x09.
	// Serving the uploads instead is what left the second observer with 70
	// messages against 803.
}

func (s *Server) handleStartObserve(client ClientConn, msgType byte, body []byte) {
	// StartObserveRequest, reached from 0x18 and 0x1C. The old format
	// (0x18) carries [sessionID u64]; the Vx format (0x1C) carries
	// [titleID u16 | sessionID 48 bits][peerIdentifier u64][passwordHash u64].
	// The type byte selects the layout and the body must be exactly that long:
	// a 24-byte 0x18 or an 8-byte 0x1C does not parse and gets no reply.
	// The per-session cap is enforced by the session; the server answers status 0
	// when it REGISTERS the
	// observer, status 3 when the store has already lost the start of the match,
	// 2 when the observer list is at the cap and 4 when observers are off for the
	// session, and the byte alone does not say which arm ran: the registration is
	// what separates them (see Session.StartObserve and SESSION-JOURNAL.md 24).
	//
	// The lookup is by the pair (titleID, sessionID), because that is how the
	// session map is keyed: an ordered tree whose comparator reads the title as a
	// *signed* 16-bit value and the sessionID as a signed 64-bit one.
	// The two message layouts differ in a way that matters:
	//   - 0x1C packs titleID into the high 16 bits of the first u64, so the pair
	//     matches the title the session was created with.
	//   - 0x18 carries no titleID, and the parser leaves -1 in the
	//     field. A client that does not set RL_TITLEID also
	//     sends -1 in its LobbyConnect, so its session is keyed (-1, sessionID)
	//     and the legacy request *does* match it. The legacy form only fails to
	//     match sessions created under a real title, which is a per-client
	//     property, not a structural one.
	// The signedness is load-bearing beyond lookup equality: 0xFFFF sorts as -1,
	// i.e. before title 0, so session iteration order depends on it.
	var sessionID, peerIdentifier, passwordHash uint64
	titleID := int16(-1)
	switch {
	case msgType == 0x1C && len(body) == 24:
		// Vx format (0x1C): [titleID u16 | sessionID 48b] + 2x8 bytes; the
		// session ID is the low 48 bits, passwordHash at offset 16.
		packed := binary.LittleEndian.Uint64(body[:8])
		titleID = int16(packed >> 48)
		sessionID = packed & 0xFFFFFFFFFFFF
		// body[8:16] is the request's peerIdentifier: the third u64 of the
		// StartObserveRequestMessage. It is what the observer list is keyed on, so
		// getting its offset wrong is not cosmetic: the activate and stop lookups
		// both compare it. body here starts AFTER the version byte that the
		// websocket reader left as body[0], so the wire offset 9 is body offset 8.
		peerIdentifier = binary.LittleEndian.Uint64(body[8:16])
		passwordHash = binary.LittleEndian.Uint64(body[16:24])
	case msgType == protocol.MsgStartObserve && len(body) == 8:
		// Old format (0x18): [sessionID u64], and titleID stays 0xFFFF. The
		// legacy form has no peerIdentifier and 0 is written into the field rather
		// than left uninitialised, so 0 is what a legacy observer is known by.
		sessionID = binary.LittleEndian.Uint64(body[:8])
	default:
		logger.Debug("Server", "dispatch", "STARTOBSERVE type=0x%02x with a %d-byte body does not parse from=%s", msgType, len(body), client.RemoteAddr())
		return
	}

	sess := s.sessionManager.GetSession(session.SessionKey{
		TitleID:   titleID,
		SessionID: int64(sessionID),
	})
	if sess == nil {
		logger.Debug("Server", "dispatch", "STARTOBSERVE session %d (titleID %d) not found from=%s", sessionID, titleID, client.RemoteAddr())
		return
	}

	// Status codes, from the cascade in Session.StartObserve: 3 no replay,
	// 0 accepted, 2 observer list at the cap, 4 observers disabled for this
	// session. Every rejection is answered with its own byte, so pass the status
	// straight through rather than collapsing it. Sent through SendMessage so the
	// transport applies its own framing.
	// A connection observes one session at a time. If it was watching another
	// one, the old entry comes off when the new registration is accepted; a
	// rejected request leaves the previous observation untouched.
	s.mu.Lock()
	previous := s.observerSessions[client]
	s.mu.Unlock()

	status, registered := sess.StartObserve(client, peerIdentifier, passwordHash)
	// Status 0 is BOTH "no replay" and "accepted": the same byte goes on the wire
	// either way, and the only thing that separates them is whether the client
	// ended up in the observer list. Capture _26 shows the accepted
	// case: 0x1C -> 0x19=00 -> the client still sends ActivateObserver, gets 0x1E=00
	// and receives the replay. So the accept arm must be keyed on the observer
	// being registered, not on the status value.
	//
	// The registration comes back from StartObserve instead of being looked up
	// afterwards: it is a property of the arm that ran, and one connection can hold
	// two entries (the append is unconditional on every request that reaches the
	// accept arm), in which case a post-hoc "is this connection in the list" says
	// yes for a request that was answered 2, 3 or 4.
	//
	// Arms 2 (list at the cap) and 4 (observers disabled) are the other two
	// constants, and are passed through as they are.
	accepted := registered
	switch {
	case accepted:
		client.SetObserver(true)
		if previous != nil && previous != sess {
			// One connection, one observer entry: the previous session must not
			// keep a live entry for a client that now watches another match.
			previous.RemoveObserverConn(client)
			// That may have been its last observer, in which case nothing else
			// will erase it: no connection is closing on its behalf.
			s.sessionManager.RemoveSessionIfVacant(previous)
		}
		s.mu.Lock()
		s.observerSessions[client] = sess
		s.mu.Unlock()
		// An observer is not a session peer. handleLobbyConnect adds one for every
		// connection that connects, which is what a player needs, but a client that
		// then becomes an observer must be taken back out: the pending-peer list in
		// the 0x0A ack names the session's peers, and leaving the observer in it
		// makes the player's ack say [1,2] where one peer belongs.
		//
		// Capture _26 confirms it: with one human player against the AI and one
		// observer, the heartbeat body is fe 00 01 01 00 00 (one pending
		// peer) for both the player and the observer, while ours was
		// fe 00 02 01 02 00 00. An observer receives the same 0x0A
		// stream as a player without ever being a member.
		if id := sess.PeerIDForConn(client); id != 0 {
			s.sessionManager.RemovePeer(sess, id, client)
		}
		_ = client.SendMessage(protocol.MsgStartObserveResp, []byte{0})
		logger.Debug("Server", "dispatch", "STARTOBSERVE accepted session=%d from=%s", sessionID, client.RemoteAddr())
	case status == 0:
		_ = client.SendMessage(protocol.MsgStartObserveResp, []byte{0})
		logger.Debug("Server", "dispatch", "STARTOBSERVE rejected (no replay) session=%d from=%s", sessionID, client.RemoteAddr())
	case status == 3:
		// The arm that does NOT register the observer writes 3 into the
		// response, and 3 is what the client reads: it is the block the cascade
		// returns from without touching the observer list.
		//
		// This used to fall into the default arm and go out as a 4, which is a
		// different rejection entirely (observers disabled for the session): the
		// client would be told observing is switched off rather than that the
		// store has already lost the start of the match, and the two are not the
		// same bug to go looking for. Found by the synthetic rollover
		// fixture (internal/sim, testdata/synthetic-rollover), which is the only
		// coverage that can put a store in that state.
		_ = client.SendMessage(protocol.MsgStartObserveResp, []byte{3})
		logger.Debug("Server", "dispatch", "STARTOBSERVE rejected (store has lost the start of the match) session=%d from=%s", sessionID, client.RemoteAddr())
	case status == 2:
		_ = client.SendMessage(protocol.MsgStartObserveResp, []byte{2})
		logger.Debug("Server", "dispatch", "STARTOBSERVE rejected (observer cap) session=%d from=%s", sessionID, client.RemoteAddr())
	default:
		_ = client.SendMessage(protocol.MsgStartObserveResp, []byte{4})
		logger.Debug("Server", "dispatch", "STARTOBSERVE rejected (observers disabled) session=%d from=%s", sessionID, client.RemoteAddr())
	}
}

func (s *Server) handleActivateObserver(client ClientConn, body []byte) {
	// handleActivateObserver activates the observer entry of the client in its
	// observed session and answers with an ActivateObserverResponse carrying the
	// estimated game start time. See Session.ActivateObserver. The message is a
	// bare type byte; one with a body does not parse and gets no reply.
	if len(body) != 0 {
		logger.Debug("Server", "dispatch", "ACTIVATEOBSERVER with a %d-byte body does not parse from=%s", len(body), client.RemoteAddr())
		return
	}
	s.mu.Lock()
	sess := s.observerSessions[client]
	s.mu.Unlock()
	if sess == nil {
		logger.Debug("Server", "dispatch", "ACTIVATEOBSERVER not observing from=%s", client.RemoteAddr())
		return
	}
	ticksPerSec := 8 // default ticks/s (simulation period 125ms)
	if s.config.SimulationPeriod > 0 {
		ticksPerSec = int(1000 / s.config.SimulationPeriod)
	}
	if sess.ActivateObserver(client, ticksPerSec) {
		logger.Debug("Server", "dispatch", "ACTIVATEOBSERVER from=%s", client.RemoteAddr())
	}
}

// handleStopObserve takes the client off its session's observer list. The
// message is a bare type byte; one with a body does not parse and changes
// nothing.
func (s *Server) handleStopObserve(client ClientConn, body []byte) {
	if len(body) != 0 {
		logger.Debug("Server", "dispatch", "STOPOBSERVE with a %d-byte body does not parse from=%s", len(body), client.RemoteAddr())
		return
	}
	s.mu.Lock()
	sess := s.observerSessions[client]
	delete(s.observerSessions, client)
	s.mu.Unlock()
	if sess != nil {
		// The original closes the observing connection as part of StopObserve.
		// RemoveObserver keeps its identifier-based lookup; the connection sweep
		// takes any duplicate entries the same connection accumulated.
		sess.RemoveObserver(client)
		sess.RemoveObserverConn(client)
		s.sessionManager.RemoveSessionIfVacant(sess)
	}
	client.SetObserver(false)
	// Close outside the session lock: a connection's close path reaches
	// HandleDisconnect, which takes the manager and session locks.
	_ = client.Close()
	s.metricsCollector.AddToCounter("RemovePeerRequests", 1)
	logger.Debug("Server", "dispatch", "STOPOBSERVE from=%s", client.RemoteAddr())
}

// handleLeaveSimulation runs Manager.HandleLeaveSimulation for the sender. The
// message is a bare type byte; one with a body does not parse and changes
// nothing.
func (s *Server) handleLeaveSimulation(client ClientConn, body []byte) {
	if len(body) != 0 {
		logger.Debug("Server", "dispatch", "LEAVESIMULATION with a %d-byte body does not parse from=%s", len(body), client.RemoteAddr())
		return
	}
	s.sessionManager.HandleLeaveSimulation(client)
	logger.Debug("Server", "dispatch", "LEAVESIMULATION from=%s", client.RemoteAddr())
}

// handleLobbyRelay handles a relay (0x13): [toPeerID u8][fromPeerID u8], then
// one nested message running to the end of the body.
//
// The nested message runs through the dispatcher as if the sender had sent it,
// whatever its type, with every handler and effect a top-level message has. The
// relay is then forwarded only when that dispatch asks for it, which only a peer
// message (0x0F) can do: one whose inner message the server did not take. A
// RequestJoin and a RequestJoinDenied are taken and re-broadcast by the peer
// handler instead, and a 0xFE GameData is taken into the sender's queue. Every
// other nested type, a relay with no nested message and a nested peer message
// too short for its header are forwarded to nobody.
//
// A forwarded relay goes unchanged to the session peer in slot toPeerID, and
// only to it, provided that peer's connection is still live. fromPeerID is never
// read. A body shorter than the two peer IDs does not parse and is dropped.
func (s *Server) handleLobbyRelay(client messageClient, body []byte, depth int) {
	if len(body) < 2 {
		logger.Debug("Server", "dispatch", "LOBBYRELAY of %d bytes does not parse from=%s", len(body), client.RemoteAddr())
		return
	}
	to := body[0]
	nested := body[2:]
	if len(nested) == 0 {
		return
	}
	if !s.dispatch(client, nested[0], nested[1:], depth+1) {
		return
	}
	sess := s.sessionManager.FindSessionByPeer(client)
	if sess == nil {
		return
	}
	peer := sess.GetPeer(to)
	if peer == nil {
		return
	}
	s.sendToPeer(peer.Conn, protocol.MsgLobbyRelay, body, true)
}

// handleLobbyChecksumAck handles a GameDataAndChecksum (0x17): [count u8], count
// checksum entries of 8 bytes ([crc u32 LE][page u32 LE]), then an optional
// nested message running to the end of the body. Entries cut short make the
// message unparseable and it is dropped whole.
//
// The entries go into the sending peer's own vector, and the nested message is
// dispatched like a top-level one, both only for a sender in a session. In
// practice the nested message is the sender's 0x09 upload, which is therefore
// held to the same exact-length rule as a top-level 0x09.
func (s *Server) handleLobbyChecksumAck(client messageClient, body []byte, depth int) {
	if len(body) < 1 {
		return
	}
	n := int(body[0])
	off := 1 + 8*n
	if off > len(body) {
		return
	}
	sess := s.sessionManager.FindSessionByPeer(client)
	if sess == nil {
		return
	}
	// Only a session member has a slot. A connection that stopped being a peer
	// (an observer) resolves to no peer ID, and queueing under slot 0 would grow
	// an entry that nobody owns. The nested message is still dispatched: the
	// original runs it whatever the sender's membership, and only the checksum
	// vector needs a slot.
	slot := sess.PeerIDForConn(client)
	if slot != 0 && sess.GetPeer(slot) != nil {
		// There is one vector per peer, and the vectors are reconciled across
		// peers on the tick.
		entries := make([]byte, 8*n)
		copy(entries, body[1:off])
		sess.QueueChecksumEntries(slot, entries)
	}
	if off < len(body) {
		s.dispatch(client, body[off], body[off+1:], depth+1)
	}
}

func (s *Server) handleIntegrityCheck(client ClientConn, body []byte) {
	// The inner IntegrityCheck (type 3) layout is
	// [03][peers blob][proposedHostID u8][migrateState u8].
	if len(body) < 4 || body[0] != protocol.PeerMsgIntegrityCheck {
		logger.Debug("Server", "IntegrityCheck", "not an inner integrity check: %x", body)
		return
	}
	msg, err := protocol.ParseIntegrityCheck(body)
	if err != nil {
		logger.Debug("Server", "IntegrityCheck", "deserialize failed: %v", err)
		return
	}
	logger.Debug("Server", "dispatchPeer", "INTEGRITYCHECK proposedHostID=%d peers=[%x] migrateState=%d",
		msg.ProposedHostID, msg.Peers, msg.MigrateState)

	sess := s.sessionManager.FindSessionByPeer(client)
	if sess == nil {
		return
	}
	// migrateState 0 is the server accepting the proposed host without
	// arbitrating. Any other state is left alone: there is no other branch here,
	// and the remaining states have not been traced.
	if msg.MigrateState != 0 {
		return
	}
	sess.SetHostID(msg.ProposedHostID)
	logger.Debug("Session", "hostAccepted", "session host changed; hostPeerID=%d", msg.ProposedHostID)
}

// handlePeer handles a peer message (0x0F): the 15-byte reliability header,
// then one inner message running to the end of the body. body is the message
// without its type byte. It runs the inner message's server-side effect and
// returns the relay verdict for whatever carried it: true to forward it, false
// when the server took the inner message.
//
// The verdict depends on the inner type alone:
//
//	2 RequestJoin, 5 RequestJoinDenied   taken, always; re-broadcast for a sender in a session
//	9 GameData tagged 0xFE               taken when the sender has a session
//	everything else                      forwarded
//
// "Everything else" is the other handled types (3, 6, 7), the unhandled ones
// (4, 8 and any other value), a GameData with another tag, a bare header with
// no inner message, and a compressed inner message that inflates to nothing. A
// body too short for the header is not a peer message at all and is forwarded
// by nobody.
//
// The effects run on every path that carries a peer message: a relay, which
// applies the verdict to its destination; the proxy, which does not apply it
// (see handleProxyData); and a top-level 0x0F, which forwards nothing whatever
// the verdict.
//
// A compressed inner message (FlagCompressed) is inflated first, and the type
// and the effect come from the inflated bytes; anything sent on carries the
// received, compressed bytes. A malformed inner message of a handled type keeps
// its type's verdict but has no effect here, with one exception: a GameData is
// taken as far as it parses, because its tag alone decides whether it is taken.
func (s *Server) handlePeer(client ClientConn, body []byte) bool {
	env, err := protocol.ParsePeerEnvelope(body)
	if err != nil {
		logger.Debug("Server", "peer", "malformed PEERHEADER: %v", err)
		return false
	}

	if len(env.Inner) == 0 {
		// A 16-byte message is a bare header with no inner part: valid, nothing
		// for the server to take, so it is forwarded.
		logger.Debug("Server", "peerHeader", "from=%s flagBits=%x ackBits=%x packetNumber=%d "+
			"startACKReliableNonSequential=%d lastReliableSequentialID=%d",
			client.RemoteAddr(), env.Header.FlagBits, env.Header.AckBits, env.Header.PacketNumber,
			env.Header.StartACKReliableNonSequential, env.Header.LastReliableSequentialID)
		return true
	}

	from := byte(0)
	if sess := s.sessionManager.FindSessionByPeer(client); sess != nil {
		from = sess.PeerIDForConn(client)
	}
	logger.Debug("Server", "peerHeader", "fromPeerID=%d flagBits=%x ackBits=%x packetNumber=%d "+
		"startACKReliableNonSequential=%d lastReliableSequentialID=%d",
		from, env.Header.FlagBits, env.Header.AckBits, env.Header.PacketNumber,
		env.Header.StartACKReliableNonSequential, env.Header.LastReliableSequentialID)

	inner := env.Inner
	if env.Header.FlagBits&protocol.FlagCompressed != 0 {
		inner = protocol.InflatePeerPayload(env.Inner)
		if len(inner) == 0 {
			// Nothing inflated, so there is no inner type to read. It is treated
			// as an unhandled type: no effect, forwarded unchanged.
			logger.Debug("Server", "dispatchPeer", "fromPeerID=%d compressed inner message of %d bytes inflates to nothing",
				from, len(env.Inner))
			return true
		}
	}

	// Each inner deserialiser expects the inner message's own type byte, so they
	// all get the whole inner message.
	switch inner[0] {
	case protocol.PeerMsgAddPeer:
		s.handlePeerAdd(client, inner)
	case protocol.PeerMsgRemovePeer:
		s.handlePeerRemove(client, inner, from)
	case protocol.PeerMsgGameData:
		return !s.handlePeerGameData(client, inner)
	case protocol.PeerMsgIntegrityCheck:
		s.handleIntegrityCheck(client, inner)
	case protocol.PeerMsgRequestJoin:
		s.handlePeerRequestJoin(client, body, inner, from)
		return false
	case protocol.PeerMsgRequestJoinDenied:
		s.handlePeerJoinDenied(client, body, inner, from)
		return false
	default:
		// The handled inner types are 2, 3, 5, 6, 7 and 9, and every other one
		// is logged and forwarded verbatim.
		//
		// Two values land here and must not be given a handler: 4 and 8. A
		// DropoutPeer (8) is unhandled exactly like 4: no host-only forward for
		// it is ever sent, so it is forwarded like any other unhandled type.
		logger.Debug("Server", "dispatchPeer", "fromPeerID=%d messageType=%d", from, inner[0])
	}
	return true
}

// rebroadcastPeerMessage sends a peer message the server took (a RequestJoin or
// a RequestJoinDenied) to every peer of the session as a 0x0F: the joiners and
// the members, the sender and a denied joiner included, each whose connection is
// still live. body is the received message without its type byte, sent as it
// came: the same header, and the inner message still compressed if it came
// compressed. Observers are not session peers and receive nothing.
func (s *Server) rebroadcastPeerMessage(sess *session.Session, body []byte) {
	for _, p := range sess.GetPeers() {
		s.sendToPeer(p.Conn, protocol.MsgPeer, body, true)
	}
}

// handlePeerGameData is the inner GameData (type 9). It is taken when its tag is
// 0xFE and the sender has a session, and it reports whether it was taken: the
// chunk then rides the next 0x0A like a top-level upload, and the message that
// carried it is not forwarded.
//
// The inner message is taken as far as it parses, because the tag alone decides:
// a payload that runs past the end is queued empty, and bytes after the declared
// payload are ignored. What is queued is always a consistent [length][payload],
// so a malformed message cannot corrupt the 0x0A. Whether the queue then accepts
// the chunk (see Session.QueueGameData) does not change the answer.
func (s *Server) handlePeerGameData(client ClientConn, inner []byte) bool {
	msg, _ := protocol.ReadGameData(inner[1:])
	if !msg.IsReplayData() {
		return false
	}
	sess := s.sessionManager.FindSessionByPeer(client)
	if sess == nil {
		return false
	}
	sess.QueueGameData(sess.PeerIDForConn(client), msg.Serialize()[1:])
	return true
}

func (s *Server) handlePeerAdd(client ClientConn, inner []byte) {
	m, err := protocol.ParseAddPeer(inner)
	if err != nil {
		logger.Debug("Server", "dispatchPeer", "ADDPEER parse failed: %v", err)
		return
	}
	logger.Debug("Server", "dispatchPeer", "ADDPEER peerID=%d isHost=%d isLocal=%d globalID=%d addressList=[%s]",
		m.PeerID, b2i(m.IsHost), b2i(m.IsLocal), m.GlobalID, m.AddressList)
	if sess := s.sessionManager.FindSessionByPeer(client); sess != nil {
		if !sess.NotePeerAdded(m.PeerID, m.GlobalID, m.IsHost) {
			logger.Debug("Server", "dispatchPeer", "ADDPEER for unknown peer %d in session %d", m.PeerID, sess.ID64())
		}
	}
}

// handlePeerRequestJoin is the inner RequestJoin (type 2). It is always taken.
// For a sender in a session it records the sender's peer key and re-broadcasts
// the message to the session (see rebroadcastPeerMessage); this is how the host
// learns that a player joins. A sender without a session gets no effect.
//
// The session is not looked up or created here: this server creates it from the
// LobbyConnect, so a joining player is already a peer of its session when its
// RequestJoin arrives. That LobbyConnect session stands in for the session a
// connection's first RequestJoin would find or create, which is why the
// message's own (titleID, sessionID) is not compared with it: a first RequestJoin
// is never compared with anything, it is what decides the session. A later
// RequestJoin that names another session, or that follows an AddPeer which gave
// the sender a slot, would be taken without effect; this server re-broadcasts it
// like the first one, a difference recorded in the register (every capture shows
// one RequestJoin per connection).
//
// body is the whole peer message without its type byte, the bytes the
// re-broadcast sends; inner is the inflated inner message.
func (s *Server) handlePeerRequestJoin(client ClientConn, body, inner []byte, from byte) {
	m, err := protocol.ParseRequestJoin(inner)
	if err != nil {
		logger.Debug("Server", "dispatchPeer", "REQUESTJOIN parse failed: %v", err)
		return
	}
	logger.Debug("Server", "dispatchPeer", "fromPeerID=%d REQUESTJOIN titleID=%d sessionID=%d peerKey=%d name=[%s]",
		from, m.TitleID, m.SessionID, m.PeerKey, m.Name)
	sess := s.sessionManager.FindSessionByPeer(client)
	if sess == nil {
		logger.Debug("Server", "dispatchPeer", "REQUESTJOIN from a connection with no session, taken without effect from=%s", client.RemoteAddr())
		return
	}
	sess.SetPeerKey(sess.PeerIDForConn(client), m.PeerKey)
	s.rebroadcastPeerMessage(sess, body)
}

// handlePeerJoinDenied is the inner RequestJoinDenied (type 5). It is always
// taken. When the sender has a session, the peer the message names by key is
// scheduled to disconnect 30 s out, and the message is re-broadcast to the
// session whether or not that peer was found (see rebroadcastPeerMessage). A
// denial from a sender without a session arrived before the join it answers:
// it is logged as out of order and has no effect.
func (s *Server) handlePeerJoinDenied(client ClientConn, body, inner []byte, from byte) {
	m, err := protocol.ParseRequestJoinDenied(inner)
	if err != nil {
		logger.Debug("Server", "dispatchPeer", "REQUESTJOINDENIED parse failed: %v", err)
		return
	}
	logger.Debug("Server", "dispatchPeer", "fromPeerID=%d REQUESTJOINDENIED peerKey=%d reasonCode=%d reason=[%s]",
		from, m.PeerKey, m.ReasonCode, m.Reason)
	sess := s.sessionManager.FindSessionByPeer(client)
	if sess == nil {
		logger.Debug("SessionManager", "joinDenied", "no session for this connection, so the "+
			"denial arrived before the join: the messages are out of order")
		return
	}
	if !sess.DenyPeerJoin(m.PeerKey, m.ReasonCode, m.Reason, time.Now()) {
		logger.Debug("SessionManager", "joinDenied", "no peer with key %d in session %d",
			m.PeerKey, sess.ID64())
	}
	s.rebroadcastPeerMessage(sess, body)
}

func (s *Server) handlePeerRemove(client ClientConn, inner []byte, from byte) {
	m, err := protocol.ParsePeerRemoval(inner, protocol.PeerMsgRemovePeer)
	if err != nil {
		logger.Debug("Server", "dispatchPeer", "REMOVEPEER parse failed: %v", err)
		return
	}
	logger.Debug("Server", "dispatchPeer", "fromPeerID=%d REMOVEPEER peerID=%d reasonCode=%d reason=[%s]",
		from, m.PeerID, m.ReasonCode, m.Reason)
	sess := s.sessionManager.FindSessionByPeer(client)
	if sess == nil {
		// Logged as an ordering problem and carried on: a RemovePeer that
		// overtakes the join is not a protocol error.
		logger.Debug("SessionManager", "removePeer", "no session for this connection, so the "+
			"removal arrived before the join: the messages are out of order")
		return
	}
	if !sess.RemovePeerGracefully(m.PeerID, from, m.ReasonCode, m.Reason, time.Now()) {
		logger.Debug("SessionManager", "removePeer", "REMOVEPEER for unknown peer %d in session %d",
			m.PeerID, sess.ID64())
	}
}

func b2i(v bool) int {
	if v {
		return 1
	}
	return 0
}
