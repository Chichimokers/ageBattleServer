package protocol

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Peer-message (0x0F) envelope and the inner message types it carries.
//
// Wire layout, as observed on the wire:
//
//	[0]      u8   type = 0x0F
//	[1..15]  15-byte PEERHEADER
//	[16..]   inner message = [innerType u8][fields...], running to the end
//
// The header is read as one 15-byte block, stored as four chunks (u64, u32, u16,
// u8); the field names below come from the two log statements that print them,
// the dispatcher's "PEERHEADER flagBits=... ackBits=... packetNumber=...
// startACKReliableNonSequential=... lastReliableSequentialID=..." and the trace
// formatter's "M#%08u Sack%08u NSack%08u". Both agree on the offsets; they
// disagree on what to call the u32 at offset 11 (the dispatcher says
// lastReliableSequentialID, the trace formatter says Sack), so only the offsets
// are treated as confirmed.
//
// Every parse here mirrors the observed guards: a field that runs past the
// end fails the message, and the message must be consumed exactly.
const (
	// PeerHeaderLen is the size of the PEERHEADER block.
	PeerHeaderLen = 15

	// Inner message types inside a PeerMessage. This numbering is separate
	// from the top-level one: inner 9 is a GameData, and it has nothing to do
	// with the top-level 0x09.
	PeerMsgRequestJoin       = 2
	PeerMsgIntegrityCheck    = 3
	PeerMsgRequestJoinDenied = 5
	PeerMsgAddPeer           = 6
	PeerMsgRemovePeer        = 7
	PeerMsgDropoutPeer       = 8
	PeerMsgGameData          = 9
)

// PeerTypeName returns the inner message name, for logs.
func PeerTypeName(t byte) string {
	switch t {
	case PeerMsgRequestJoin:
		return "REQUESTJOIN"
	case PeerMsgIntegrityCheck:
		return "INTEGRITYCHECK"
	case PeerMsgRequestJoinDenied:
		return "REQUESTJOINDENIED"
	case PeerMsgAddPeer:
		return "ADDPEER"
	case PeerMsgRemovePeer:
		return "REMOVEPEER"
	case PeerMsgDropoutPeer:
		return "DROPOUT"
	case PeerMsgGameData:
		return "GAMEDATA"
	default:
		return fmt.Sprintf("0x%02X", t)
	}
}

// PeerHeader is the 15-byte reliability header that fronts every peer message.
// It is a sequence-number/ack layer that sits inside the already-reliable TCP
// stream: the game uses it to detect or ignore duplicate and out-of-order
// application messages.
//
// The server parses this header but never builds one. Serialising it copies the
// 15 bytes straight out and computes nothing, and every 0x0F this server emits
// carries a header a client sent: the re-broadcast of a RequestJoin or a
// RequestJoinDenied repeats the received message byte for byte, and a forwarded
// relay or proxy message carries its sender's bytes.
// So the ack and retransmission fields are never values we have to fill, and
// their semantics stay unverified on purpose: see the open item in
// SESSION-JOURNAL.md. Serialize exists to pin the wire layout in the round-trip
// test and for any future path that does have to emit a peer message.
type PeerHeader struct {
	FlagBits                      byte
	PacketNumber                  uint32
	StartACKReliableNonSequential uint32
	AckBits                       uint16
	LastReliableSequentialID      uint32
}

// ParsePeerHeader reads the 15-byte block. All multi-byte fields are
// little-endian, which is what the captured traffic shows.
func ParsePeerHeader(b []byte) (PeerHeader, error) {
	if len(b) < PeerHeaderLen {
		return PeerHeader{}, fmt.Errorf("peer header too short: %d < %d", len(b), PeerHeaderLen)
	}
	return PeerHeader{
		FlagBits:                      b[0],
		PacketNumber:                  binary.LittleEndian.Uint32(b[1:5]),
		StartACKReliableNonSequential: binary.LittleEndian.Uint32(b[5:9]),
		AckBits:                       binary.LittleEndian.Uint16(b[9:11]),
		LastReliableSequentialID:      binary.LittleEndian.Uint32(b[11:15]),
	}, nil
}

// Serialize writes the 15-byte block.
func (h PeerHeader) Serialize() []byte {
	b := make([]byte, PeerHeaderLen)
	b[0] = h.FlagBits
	binary.LittleEndian.PutUint32(b[1:5], h.PacketNumber)
	binary.LittleEndian.PutUint32(b[5:9], h.StartACKReliableNonSequential)
	binary.LittleEndian.PutUint16(b[9:11], h.AckBits)
	binary.LittleEndian.PutUint32(b[11:15], h.LastReliableSequentialID)
	return b
}

// FlagCompressed is the one flagBits bit with a confirmed meaning: the inner
// message is zlib-compressed. The game compresses every RequestJoin and the
// first AddPeer of each connection.
//
// The receive path inflates such a message before it classifies it: the inner
// type and the server-side effect come from the inflated bytes. The message
// itself is not changed, so whatever the server forwards or re-broadcasts still
// carries the compressed bytes it received. See InflatePeerPayload.
const FlagCompressed = 0x04

// MaxInflatedPeerPayload is the most an inflated inner message can hold: the
// server inflates into a buffer of this size and reads nothing past it.
const MaxInflatedPeerPayload = 65535

// inflateResetter is what compress/zlib's reader offers and what the pool needs:
// the reader type itself is unexported, so the pool holds the io.ReadCloser
// NewReader returns and resets it through the Reset method it has.
type inflateResetter interface {
	io.ReadCloser
	Reset(r io.Reader, dict []byte) error
}

// emptyZlibStream is a valid, empty zlib stream. NewReader refuses an input that
// is not a zlib header, so the pool is seeded from a stream that parses; every
// later use resets the same reader onto the message's own bytes.
var emptyZlibStream = func() []byte {
	var b bytes.Buffer
	w := zlib.NewWriter(&b)
	_ = w.Close()
	return b.Bytes()
}()

// inflateReaders and inflateBuffers reuse the zlib reader and the 64 KiB output
// buffer InflatePeerPayload needs. Compressed messages are rare in a real match
// (a RequestJoin, the first AddPeer), so this is not a hot path: it is what stops
// a client that spams compressed messages from making a decompressor and 64 KiB
// of garbage each.
//
// The buffer is pooled and the result is copied out of it, so the caller keeps
// owning what it gets back and nothing has to be released.
var (
	inflateReaders = sync.Pool{New: func() any {
		zr, err := zlib.NewReader(bytes.NewReader(emptyZlibStream))
		if err != nil {
			panic(err) // a constant, valid stream: unreachable
		}
		return zr.(inflateResetter)
	}}
	inflateBuffers = sync.Pool{New: func() any { b := make([]byte, MaxInflatedPeerPayload); return &b }}
)

// InflatePeerPayload inflates the zlib-compressed inner message of a peer
// message whose flagBits carry FlagCompressed. It never fails: it returns the
// bytes the stream produced, at most MaxInflatedPeerPayload of them.
//
// An inflate error does not discard what was inflated before it, because the
// server takes the inflated bytes whatever the inflate reports: a stream that is
// cut short or whose checksum is wrong still yields its first bytes, and those
// are classified like any other inner message. A stream that yields nothing
// (not zlib at all, a header with no data) returns nothing, and the caller then
// has no inner type to read.
func InflatePeerPayload(compressed []byte) []byte {
	zr := inflateReaders.Get().(inflateResetter)
	if err := zr.Reset(bytes.NewReader(compressed), nil); err != nil {
		inflateReaders.Put(zr)
		return nil
	}
	bufp := inflateBuffers.Get().(*[]byte)
	buf := *bufp
	n, _ := io.ReadFull(zr, buf)
	_ = zr.Close()
	inflateReaders.Put(zr)
	out := append([]byte(nil), buf[:n]...)
	inflateBuffers.Put(bufp)
	return out
}

// PeerEnvelope is a parsed 0x0F message: the reliability header plus the inner
// message that follows it.
type PeerEnvelope struct {
	Header    PeerHeader
	InnerType byte
	Inner     []byte
}

// ParsePeerEnvelope parses the body of a 0x0F message, i.e. everything after the
// type byte. A message with no inner part is valid (a 16-byte message: header
// only, InnerType then absent).
func ParsePeerEnvelope(body []byte) (PeerEnvelope, error) {
	env := PeerEnvelope{}
	if len(body) < PeerHeaderLen {
		return env, fmt.Errorf("peer envelope too short: %d < %d", len(body), PeerHeaderLen)
	}
	h, err := ParsePeerHeader(body[:PeerHeaderLen])
	if err != nil {
		return env, err
	}
	env.Header = h
	env.Inner = body[PeerHeaderLen:]
	if len(env.Inner) > 0 {
		env.InnerType = env.Inner[0]
	}
	return env, nil
}

// reader is a cursor over a message body with bounds checks: every accessor
// fails once the cursor would pass the end, and the caller requires exact
// consumption.
type reader struct {
	b   []byte
	pos int
}

var errTruncated = errors.New("truncated message")

func (r *reader) u8() (byte, error) {
	if r.pos+1 > len(r.b) {
		return 0, errTruncated
	}
	v := r.b[r.pos]
	r.pos++
	return v, nil
}

func (r *reader) u32() (uint32, error) {
	if r.pos+4 > len(r.b) {
		return 0, errTruncated
	}
	v := binary.LittleEndian.Uint32(r.b[r.pos : r.pos+4])
	r.pos += 4
	return v, nil
}

func (r *reader) u64() (uint64, error) {
	if r.pos+8 > len(r.b) {
		return 0, errTruncated
	}
	v := binary.LittleEndian.Uint64(r.b[r.pos : r.pos+8])
	r.pos += 8
	return v, nil
}

// blob reads a length-prefixed byte string: a u16 little-endian length followed
// by that many bytes. Both readers use this same shape.
func (r *reader) blob() ([]byte, error) {
	if r.pos+2 > len(r.b) {
		return nil, errTruncated
	}
	n := int(binary.LittleEndian.Uint16(r.b[r.pos : r.pos+2]))
	r.pos += 2
	if n == 0 {
		return nil, nil
	}
	if r.pos+n > len(r.b) {
		return nil, errTruncated
	}
	v := r.b[r.pos : r.pos+n]
	r.pos += n
	return v, nil
}

func (r *reader) done() bool { return r.pos == len(r.b) }

// AddPeerMessage is the inner type 6. Wire layout: type, a length-prefixed
// string, a second length-prefixed blob, peerID, isHost, isLocal, globalID,
// exact end. The first string is logged as addressList; the second one's purpose
// is not identified, so it is kept as Extra.
type AddPeerMessage struct {
	AddressList string
	Extra       []byte
	PeerID      byte
	IsHost      bool
	IsLocal     bool
	GlobalID    uint64
}

// ParseAddPeer parses an inner AddPeer message, type byte included.
func ParseAddPeer(b []byte) (AddPeerMessage, error) {
	var m AddPeerMessage
	r := &reader{b: b}
	t, err := r.u8()
	if err != nil {
		return m, err
	}
	if t != PeerMsgAddPeer {
		return m, fmt.Errorf("add peer: type 0x%02X, want 0x%02X", t, PeerMsgAddPeer)
	}
	addr, err := r.blob()
	if err != nil {
		return m, err
	}
	m.AddressList = string(addr)
	if m.Extra, err = r.blob(); err != nil {
		return m, err
	}
	if m.PeerID, err = r.u8(); err != nil {
		return m, err
	}
	host, err := r.u8()
	if err != nil {
		return m, err
	}
	m.IsHost = host != 0
	local, err := r.u8()
	if err != nil {
		return m, err
	}
	m.IsLocal = local != 0
	if m.GlobalID, err = r.u64(); err != nil {
		return m, err
	}
	if !r.done() {
		return m, fmt.Errorf("add peer: %d trailing bytes", len(r.b)-r.pos)
	}
	return m, nil
}

// RequestJoinMessage is the inner type 2. Wire layout: type, a length-prefixed
// string, a second length-prefixed blob, a packed title/session u64, a peer key
// u64, exact end.
// The packing is the same as LobbyConnectMessage's: title ID in the high 16
// bits, session ID in the low 48.
type RequestJoinMessage struct {
	Name      string
	Extra     []byte
	TitleID   uint16
	SessionID uint64
	PeerKey   uint64
}

// ParseRequestJoin parses an inner RequestJoin, type byte included.
func ParseRequestJoin(b []byte) (RequestJoinMessage, error) {
	var m RequestJoinMessage
	r := &reader{b: b}
	t, err := r.u8()
	if err != nil {
		return m, err
	}
	if t != PeerMsgRequestJoin {
		return m, fmt.Errorf("request join: type 0x%02X, want 0x%02X", t, PeerMsgRequestJoin)
	}
	name, err := r.blob()
	if err != nil {
		return m, err
	}
	m.Name = string(name)
	if m.Extra, err = r.blob(); err != nil {
		return m, err
	}
	packed, err := r.u64()
	if err != nil {
		return m, err
	}
	m.TitleID = uint16(packed >> 48)
	m.SessionID = packed & 0xFFFFFFFFFFFF
	if m.PeerKey, err = r.u64(); err != nil {
		return m, err
	}
	if !r.done() {
		return m, fmt.Errorf("request join: %d trailing bytes", len(r.b)-r.pos)
	}
	return m, nil
}

// RequestJoinDeniedMessage is the inner type 5. Wire layout: type, a
// length-prefixed blob, a reason code u32, a length-prefixed reason string, the
// peer key u64, exact end.
//
// The server's action is to schedule the peer with that key to disconnect 30 s
// out, the same mechanism RemovePeer uses.
type RequestJoinDeniedMessage struct {
	Extra      []byte
	ReasonCode uint32
	Reason     string
	PeerKey    uint64
}

// ParseRequestJoinDenied parses an inner RequestJoinDenied, type byte included.
func ParseRequestJoinDenied(b []byte) (RequestJoinDeniedMessage, error) {
	var m RequestJoinDeniedMessage
	r := &reader{b: b}
	t, err := r.u8()
	if err != nil {
		return m, err
	}
	if t != PeerMsgRequestJoinDenied {
		return m, fmt.Errorf("request join denied: type 0x%02X, want 0x%02X", t, PeerMsgRequestJoinDenied)
	}
	if m.Extra, err = r.blob(); err != nil {
		return m, err
	}
	if m.ReasonCode, err = r.u32(); err != nil {
		return m, err
	}
	reason, err := r.blob()
	if err != nil {
		return m, err
	}
	m.Reason = string(reason)
	if m.PeerKey, err = r.u64(); err != nil {
		return m, err
	}
	if !r.done() {
		return m, fmt.Errorf("request join denied: %d trailing bytes", len(r.b)-r.pos)
	}
	return m, nil
}

// IntegrityCheckMessage is the inner type 3. Wire layout: type, a length-prefixed
// peer list, the proposed host ID, the migration state, exact end. It is the
// message that carries a host migration: the server applies the proposal when
// the migration state is 0.
type IntegrityCheckMessage struct {
	Peers          []byte
	ProposedHostID byte
	MigrateState   byte
}

// ParseIntegrityCheck parses an inner IntegrityCheck, type byte included.
func ParseIntegrityCheck(b []byte) (IntegrityCheckMessage, error) {
	var m IntegrityCheckMessage
	r := &reader{b: b}
	t, err := r.u8()
	if err != nil {
		return m, err
	}
	if t != PeerMsgIntegrityCheck {
		return m, fmt.Errorf("integrity check: type 0x%02X, want 0x%02X", t, PeerMsgIntegrityCheck)
	}
	if m.Peers, err = r.blob(); err != nil {
		return m, err
	}
	if m.ProposedHostID, err = r.u8(); err != nil {
		return m, err
	}
	if m.MigrateState, err = r.u8(); err != nil {
		return m, err
	}
	if !r.done() {
		return m, fmt.Errorf("integrity check: %d trailing bytes", len(r.b)-r.pos)
	}
	return m, nil
}

// PeerRemovalMessage is the inner type 7 (RemovePeer) and 8 (DropoutPeer).
// Both share one deserialiser, with only the type byte differing: type,
// reasonCode u32, a length-prefixed reason string, peerID, exact end.
type PeerRemovalMessage struct {
	PeerID     byte
	ReasonCode uint32
	Reason     string
}

// ParsePeerRemoval parses an inner RemovePeer or DropoutPeer message, type byte
// included.
func ParsePeerRemoval(b []byte, want byte) (PeerRemovalMessage, error) {
	var m PeerRemovalMessage
	r := &reader{b: b}
	t, err := r.u8()
	if err != nil {
		return m, err
	}
	if t != want {
		return m, fmt.Errorf("peer removal: type 0x%02X, want 0x%02X", t, want)
	}
	if m.ReasonCode, err = r.u32(); err != nil {
		return m, err
	}
	reason, err := r.blob()
	if err != nil {
		return m, err
	}
	m.Reason = string(reason)
	if m.PeerID, err = r.u8(); err != nil {
		return m, err
	}
	if !r.done() {
		return m, fmt.Errorf("peer removal: %d trailing bytes", len(r.b)-r.pos)
	}
	return m, nil
}
