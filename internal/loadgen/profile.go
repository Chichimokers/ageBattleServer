package loadgen

import (
	"fmt"
	"math/rand/v2"
	"time"
)

// Profile describes the load a Runner produces. All rates are per
// connection/role: each match has one host (game-data page uploader), the
// remaining players (LobbyRelay + keepalives) and a set of observers that
// subscribe to the session's replay stream.
//
// The defaults are derived from the real packet captures in
// battle-server-manager (main_log_*.csv / secondary_log_*.csv): players mostly
// receive (EndOfGameData ack flushes, LobbyRelay, LobbyPing) and send little;
// observers receive a PageData stream. The page size mix matches the captured
// payload distribution (p50 ~200B, p95 ~1.1KB, max ~2.6KB).
type Profile struct {
	// Matches is the number of concurrent sessions.
	Matches int
	// Players is the number of players per match, including the host.
	Players int
	// Observers is the number of observers per match.
	Observers int
	// Duration is how long the traffic runs.
	Duration time.Duration
	// PageInterval is the host's game-data page upload cadence (one page per
	// simulation tick is the mid-game pattern).
	PageInterval time.Duration
	// PageMaxBytes bounds the generated page payload size.
	PageMaxBytes int
	// RelayInterval is each player's LobbyRelay cadence to a random peer.
	RelayInterval time.Duration
	// PingInterval is each connection's LobbyPing keepalive cadence.
	PingInterval time.Duration
}

// validate rejects a profile whose intervals would make time.NewTicker panic.
// The load tool is driven from the command line, and "-pageinterval 0" used to
// reach the ticker instead of being refused with an error.
func (p Profile) validate() error {
	switch {
	case p.Matches < 0:
		return fmt.Errorf("matches must not be negative: %d", p.Matches)
	case p.Players < 1:
		return fmt.Errorf("players must be at least 1 (the host): %d", p.Players)
	case p.PageInterval <= 0:
		return fmt.Errorf("page interval must be positive: %s", p.PageInterval)
	case p.RelayInterval <= 0:
		return fmt.Errorf("relay interval must be positive: %s", p.RelayInterval)
	case p.PingInterval <= 0:
		return fmt.Errorf("ping interval must be positive: %s", p.PingInterval)
	}
	return nil
}

// DefaultProfile returns the capture-derived profile. With the default
// simulation period (125ms) the page upload of one page per tick keeps the
// session ack flush busy without flooding the server.
func DefaultProfile() Profile {
	return Profile{
		Matches:       4,
		Players:       8,
		Observers:     8,
		Duration:      30 * time.Second,
		PageInterval:  125 * time.Millisecond,
		PageMaxBytes:  2600,
		RelayInterval: 2 * time.Second,
		PingInterval:  10 * time.Second,
	}
}

// randBytes fills a buffer with random bytes from the generator.
func randBytes(rng *rand.Rand, buf []byte) {
	for i := 0; i < len(buf); i += 8 {
		v := rng.Uint64()
		for j := 0; j < 8 && i+j < len(buf); j++ {
			buf[i+j] = byte(v >> (8 * j))
		}
	}
}

// pagePayload returns a game-data page payload sized like the captured replay
// stream: mostly small pages with periodic large ones.
func pagePayload(rng *rand.Rand, maxBytes int) []byte {
	r := rng.Float64()
	var size int
	switch {
	case r < 0.50:
		size = 64 + rng.IntN(192) // 64..256
	case r < 0.80:
		size = 256 + rng.IntN(768) // 256..1024
	case r < 0.95:
		size = 1024 + rng.IntN(1024) // 1K..2K
	default:
		size = 2048 + rng.IntN(552) // 2K..2.6K
	}
	if size > maxBytes {
		size = maxBytes
	}
	if size < 1 {
		size = 1
	}
	buf := make([]byte, size)
	randBytes(rng, buf)
	return buf
}

// relayBody builds a LobbyRelay body ([target][source][0x0f][payload]) for a
// message from srcPeer to dstPeer.
func relayBody(srcPeer, dstPeer byte, rng *rand.Rand) []byte {
	payload := make([]byte, 16+rng.IntN(48))
	randBytes(rng, payload)
	body := make([]byte, 0, 3+len(payload))
	body = append(body, dstPeer, srcPeer, 0x0f)
	body = append(body, payload...)
	return body
}
