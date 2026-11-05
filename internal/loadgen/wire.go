package loadgen

import (
	"encoding/binary"

	"github.com/luskaner/ageBattleServer/internal/protocol"
)

// wireFrame frames a message as bare [type][body], the format the game's
// WebSocket clients send (masked with a zero mask) and that the websocket reader
// accepts (both bare and [u16 BE length] prefixed forms).
func wireFrame(msgType byte, body []byte) []byte {
	frame := make([]byte, 0, 1+len(body))
	frame = append(frame, msgType)
	frame = append(frame, body...)
	return frame
}

// lobbyConnectBody builds a valid LobbyConnect (0x15) body accepted by
// protocol.ParseLobbyConnect for the given session ID. The reconnectState
// mirrors the real game: a fresh join uses 0. Every connection gets its own
// secret: the peer is identified by it on a reconnect, and a shared constant made
// every player look like the host to the reconnect lookup.
func lobbyConnectBody(sessionID uint64, reconnectState byte, secret uint64) []byte {
	body := make([]byte, 0, 31)
	var b8 [8]byte
	binary.LittleEndian.PutUint64(b8[:], sessionID)
	body = append(body, b8[:]...)
	binary.LittleEndian.PutUint64(b8[:], secret)
	body = append(body, b8[:]...)
	body = append(body, reconnectState)
	binary.LittleEndian.PutUint16(b8[:2], 5)
	body = append(body, b8[:2]...) // clientMessageVersion = 5
	binary.LittleEndian.PutUint32(b8[:4], 0)
	body = append(body, b8[:4]...) // timeoutOverride
	binary.LittleEndian.PutUint64(b8[:], 0x0102_0304_0506_0708)
	body = append(body, b8[:]...) // profileID
	return body
}

// startObserveBody builds the new-format (Vx) StartObserveRequest body:
// [u64 (titleID<<48)|sessionID][peerIdentifier u64][passwordHash u64] = 24 bytes.
// The message TYPE discriminates the format (DE050 dispatches 0x18 and 0x1C to
// StartObserveRequest): 0x1C = Vx, where titleID lives in the high 16 bits of
// the first u64; 0x18 = legacy, a bare [sessionID u64].
//
// This uses 0x1C because that is the form whose layout is known end to end. It
// is NOT because 0x18 is known to fail: parsing the legacy form leaves titleID =
// 0xFFFF, but whether the server's session lookup compares that titleID is not
// established. If it does not, 0x18 works and this tool could use it. See the
// open item in SESSION-JOURNAL.md.
// The caller must use wireFrame(0x1C, startObserveBody(...)).
func startObserveBody(sessionID uint64, titleID uint16) []byte {
	body := make([]byte, 24)
	var b8 [8]byte
	binary.LittleEndian.PutUint64(b8[:], (uint64(titleID)<<48)|sessionID)
	copy(body[0:8], b8[:])
	return body
}

// gameDataPage serializes a replay-data GameData (0x09) page: inner type 0xFE
// marks the page as replay data (E2E00), the stream a host uploads. The body
// layout must match GameDataMessage.Deserialize: [inner u8][len u16 LE][payload].
func gameDataPage(payload []byte) []byte {
	return (&protocol.GameDataMessage{InnerType: 0xFE, Payload: payload}).Serialize()
}
