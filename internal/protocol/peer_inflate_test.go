package protocol

import (
	"bytes"
	"compress/zlib"
	"testing"
)

func deflate(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatalf("zlib write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zlib close: %v", err)
	}
	return buf.Bytes()
}

func TestInflatePeerPayloadRoundTrip(t *testing.T) {
	inner := append([]byte{PeerMsgRequestJoin}, bytes.Repeat([]byte("addr"), 300)...)
	if got := InflatePeerPayload(deflate(t, inner)); !bytes.Equal(got, inner) {
		t.Errorf("inflated %d bytes, want the %d bytes compressed", len(got), len(inner))
	}
}

// The inflated message is at most 65535 bytes: a longer one is cut there.
func TestInflatePeerPayloadIsCappedAt65535(t *testing.T) {
	inner := bytes.Repeat([]byte{0x5A, 0x01, 0x02}, 30000)
	got := InflatePeerPayload(deflate(t, inner))
	if len(got) != MaxInflatedPeerPayload || MaxInflatedPeerPayload != 65535 {
		t.Fatalf("inflated %d bytes, want the 65535-byte cap", len(got))
	}
	if !bytes.Equal(got, inner[:MaxInflatedPeerPayload]) {
		t.Error("the capped output is not the start of the message")
	}
}

// An inflate error does not discard the output: the bytes inflated before the
// error are returned, and a stream that yields nothing returns nothing.
func TestInflatePeerPayloadKeepsWhatInflatedBeforeAnError(t *testing.T) {
	inner := bytes.Repeat([]byte("0123456789"), 50)
	full := deflate(t, inner)

	badSum := append([]byte(nil), full...)
	badSum[len(badSum)-1] ^= 0xFF
	if got := InflatePeerPayload(badSum); !bytes.Equal(got, inner) {
		t.Errorf("corrupt checksum: inflated %d bytes, want all %d", len(got), len(inner))
	}

	cut := InflatePeerPayload(full[:len(full)/2])
	if len(cut) >= len(inner) || !bytes.Equal(cut, inner[:len(cut)]) {
		t.Errorf("truncated stream: inflated %d bytes, want a strict prefix of the message", len(cut))
	}

	for name, b := range map[string][]byte{
		"nil":              nil,
		"not zlib":         {0x01, 0x02, 0x03, 0x04},
		"header only":      full[:2],
		"reserved block":   {0x78, 0x9C, 0xFF, 0xFF},
		"preset dict flag": {0x78, 0xBB, 0x00, 0x00, 0x00, 0x01},
	} {
		if got := InflatePeerPayload(b); len(got) != 0 {
			t.Errorf("%s: inflated %x, want nothing", name, got)
		}
	}
}

// The reader and the output buffer are pooled, so the same reader is reset onto
// a different stream and the same buffer is reused. A pooled reader that was not
// fully reset would carry the previous stream's state into the next one, and a
// buffer that was not cleared would leak the previous message's tail into a
// shorter one. This walks a long message, a short one and a rejected one in that
// order and checks each answer on its own.
func TestInflatePeerPayloadReusesItsReaderAndBuffer(t *testing.T) {
	long := append([]byte{PeerMsgRequestJoin}, bytes.Repeat([]byte("long"), 2000)...)
	short := []byte{PeerMsgAddPeer, 0x01, 0x02}
	for i := range 3 {
		if got := InflatePeerPayload(deflate(t, long)); !bytes.Equal(got, long) {
			t.Fatalf("round %d: the long message inflated to %d bytes, want %d", i, len(got), len(long))
		}
		if got := InflatePeerPayload(deflate(t, short)); !bytes.Equal(got, short) {
			t.Fatalf("round %d: the short message inflated to %x, want %x: the buffer kept the previous "+
				"message's tail", i, got, short)
		}
		if got := InflatePeerPayload([]byte{0x01, 0x02, 0x03, 0x04}); len(got) != 0 {
			t.Fatalf("round %d: a non-zlib input inflated to %x, want nothing", i, got)
		}
	}
}

// ReadGameData reads tag, length and payload in that order and stops at the
// first field that does not fit, leaving it empty. exact reports whether the
// body was consumed to its last byte.
func TestReadGameData(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    []byte
		tag     byte
		payload []byte
		exact   bool
	}{
		{"exact", []byte{0xFE, 0x02, 0x00, 0xAA, 0xBB}, 0xFE, []byte{0xAA, 0xBB}, true},
		{"empty payload", []byte{0xFE, 0x00, 0x00}, 0xFE, nil, true},
		{"trailing byte", []byte{0xFE, 0x01, 0x00, 0xAA, 0xBB}, 0xFE, []byte{0xAA}, false},
		{"payload short", []byte{0xFE, 0x03, 0x00, 0xAA, 0xBB}, 0xFE, nil, false},
		{"half a length", []byte{0xFE, 0x03}, 0xFE, nil, false},
		{"tag only", []byte{0xFE}, 0xFE, nil, false},
		{"empty", nil, 0x00, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, exact := ReadGameData(tc.body)
			if m.InnerType != tc.tag || !bytes.Equal(m.Payload, tc.payload) || exact != tc.exact {
				t.Errorf("ReadGameData(%x) = tag %02x payload %x exact %v, want tag %02x payload %x exact %v",
					tc.body, m.InnerType, m.Payload, exact, tc.tag, tc.payload, tc.exact)
			}
			_, err := ParseGameData(tc.body)
			if (err == nil) != tc.exact {
				t.Errorf("ParseGameData(%x) error = %v, want an error unless the body is exact", tc.body, err)
			}
		})
	}
}
