package loadgen

import (
	"testing"

	"github.com/luskaner/ageBattleServer/internal/protocol"
)

// TestProfileRejectsNonPositiveIntervals covers N-20: a zero interval used to
// reach time.NewTicker and panic.
func TestProfileRejectsNonPositiveIntervals(t *testing.T) {
	base := DefaultProfile()
	cases := []struct {
		name string
		edit func(*Profile)
	}{
		{"page", func(p *Profile) { p.PageInterval = 0 }},
		{"relay", func(p *Profile) { p.RelayInterval = 0 }},
		{"ping", func(p *Profile) { p.PingInterval = 0 }},
		{"players", func(p *Profile) { p.Players = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			tc.edit(&p)
			if _, err := New("ws://127.0.0.1:1/", p); err == nil {
				t.Error("New accepted a profile with a non-positive value")
			}
		})
	}
}

// TestPlayerSecretsAreDistinct covers N-21: the reconnect secret identifies one
// peer inside a session, so two players must not share it.
func TestPlayerSecretsAreDistinct(t *testing.T) {
	seen := map[uint64]bool{}
	for match := 0; match < 2; match++ {
		for player := 0; player < 8; player++ {
			s := playerSecret(match, player)
			if seen[s] {
				t.Fatalf("secret %#x is used twice", s)
			}
			seen[s] = true
			body := lobbyConnectBody(1, 0, s)
			info, ok := protocol.ParseLobbyConnect(body)
			if !ok {
				t.Fatal("LobbyConnect body does not parse")
			}
			if info.Secret != s {
				t.Errorf("parsed secret = %#x, want %#x", info.Secret, s)
			}
		}
	}
}

// TestObserverRecvMsgsCountsFramesNotReads covers N-26: a two-byte frame header
// plus its payload is one message, not two.
func TestObserverRecvMsgsCountsFramesNotReads(t *testing.T) {
	s := &Stats{}
	s.recordRecvBytes(2)
	s.recordRecvBytes(2)
	s.recordRecvMsg()
	if got := s.snapshot().RecvMsgs; got != 1 {
		t.Errorf("RecvMsgs = %d, want 1", got)
	}
	if got := s.snapshot().RecvBytes; got != 4 {
		t.Errorf("RecvBytes = %d, want 4", got)
	}
}
