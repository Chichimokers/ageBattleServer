package loadgen

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// Runner generates realistic game traffic against a battleserver WebSocket
// endpoint. Each match creates one session: a host player (game-data page
// uploader), the remaining players (LobbyRelay + keepalives), and observers
// subscribed to the session's replay stream.
type Runner struct {
	wsAddr  string
	host    string
	port    int
	profile Profile
	stats   *Stats
	// Debug, when set, receives per-setup-step progress for diagnosing runs.
	Debug func(format string, args ...any)
}

func (r *Runner) debugf(format string, args ...any) {
	if r.Debug != nil {
		r.Debug(format, args...)
	}
}

// New builds a Runner for a ws://host:port/ endpoint.
func New(wsURL string, profile Profile) (*Runner, error) {
	if err := profile.validate(); err != nil {
		return nil, err
	}
	u, err := url.Parse(wsURL)
	if err != nil {
		return nil, fmt.Errorf("parse ws url: %w", err)
	}
	host := u.Hostname()
	if host == "" {
		host = "127.0.0.1"
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		return nil, fmt.Errorf("ws url port: %w", err)
	}
	return &Runner{
		wsAddr:  wsURL,
		host:    host,
		port:    port,
		profile: profile,
		stats:   &Stats{},
	}, nil
}

// Stats exposes the live counters (usable during Run).
func (r *Runner) Stats() *Stats { return r.stats }

// Run generates traffic until the profile duration elapses or ctx is done, then
// closes all connections and returns the aggregate report.
func (r *Runner) Run(ctx context.Context) Report {
	before := ReadResourceStats()
	start := time.Now()

	runCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for i := 0; i < r.profile.Matches; i++ {
		wg.Add(1)
		go r.runMatch(runCtx, i, &wg)
	}

	if r.profile.Duration > 0 {
		timer := time.NewTimer(r.profile.Duration)
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
		timer.Stop()
	}
	cancel()
	wg.Wait()

	after := ReadResourceStats()
	diff := before.Diff(after)
	rep := Report{
		Profile:        r.profile,
		Stats:          r.stats.snapshot(),
		Elapsed:        time.Since(start),
		CPUUsedSeconds: diff.CPUUsedSeconds,
		HeapBytes:      diff.HeapBytes,
		AllocsBytes:    diff.AllocsBytes,
		AllocsObjs:     diff.AllocsObjs,
	}
	return rep
}

// playerSecret is the reconnect secret of one connection. It is derived from the
// match and the player index, so it is stable for a run and distinct for every
// player of a match: the server looks a reconnecting peer up by this number
// inside the session, and a constant made the second player resume the first.
func playerSecret(matchIdx, playerIdx int) uint64 {
	return 0x9E37_0000_0000_0000 | (uint64(matchIdx+1) << 16) | uint64(playerIdx+1)
}

// runMatch sets up one session and drives its traffic until ctx is done.
func (r *Runner) runMatch(ctx context.Context, matchIdx int, wg *sync.WaitGroup) {
	defer wg.Done()
	sid := uint64(matchIdx + 1)

	host, err := dialWSPlayer(r.wsAddr, r.stats)
	if err != nil {
		r.debugf("match %d: host dial: %v", matchIdx, err)
		r.stats.recordError()
		return
	}
	defer host.close()
	// The session creator joins with reconnectState 0 (fresh join), and every
	// connection carries its own secret so no player resumes another's peer.
	if err := host.lobbyConnect(sid, 0, playerSecret(matchIdx, 0)); err != nil {
		r.debugf("match %d: host lobbyConnect: %v", matchIdx, err)
		r.stats.recordError()
		return
	}
	r.debugf("match %d: host joined session %d", matchIdx, sid)

	// Seed a couple of game-data pages synchronously so the session is recording
	// (status 3 in StartObserve requires at least one stored page) before any
	// observer subscribes. The host loop below keeps the stream alive.
	seedRng := rand.New(rand.NewPCG(uint64(matchIdx)+7, 2))
	for i := range 2 {
		if err := host.send(0x09, gameDataPage(pagePayload(seedRng, r.profile.PageMaxBytes))); err != nil {
			r.debugf("match %d: seed page %d: %v", matchIdx, i, err)
			r.stats.recordError()
			break
		}
	}
	// The LobbyPing echo is a same-socket FIFO barrier: when it returns, the
	// server has processed the seed pages, so observers below can subscribe.
	if err := host.pingOnce(); err != nil {
		r.debugf("match %d: seed barrier ping: %v", matchIdx, err)
		r.stats.recordError()
	}

	var players []*wsPlayer
	for i := 2; i <= r.profile.Players; i++ {
		p, err := dialWSPlayer(r.wsAddr, r.stats)
		if err != nil {
			r.debugf("match %d: player %d dial: %v", matchIdx, i, err)
			r.stats.recordError()
			continue
		}
		players = append(players, p)
		// A fresh join (state 0) with this connection's own secret. A reconnect
		// (state != 0) only resumes an existing peer and refuses a new one, so it
		// cannot add players; each join is its own peer.
		if err := p.lobbyConnect(sid, 0, playerSecret(matchIdx, i)); err != nil {
			r.debugf("match %d: player %d lobbyConnect: %v", matchIdx, i, err)
			r.stats.recordError()
		}
	}
	r.debugf("match %d: %d players in session", matchIdx, len(players))

	// Start the host uploader and player relays before observers subscribe so the
	// session always has fresh game data to observe (matches the capture flow,
	// where observers join after the game is underway).
	go r.hostLoop(ctx, host, matchIdx)

	for i, p := range players {
		go r.playerLoop(ctx, p, matchIdx, byte(i+2))
	}

	var observers []*observer
	for i := 0; i < r.profile.Observers; i++ {
		o, err := dialObserver(r.host, r.port, r.stats)
		if err != nil {
			r.debugf("match %d: observer %d dial: %v", matchIdx, i, err)
			r.stats.recordError()
			continue
		}
		observers = append(observers, o)
		if err := o.startObserve(sid); err != nil {
			r.debugf("match %d: observer %d startObserve: %v", matchIdx, i, err)
			r.stats.recordError()
			continue
		}
		if err := o.activate(); err != nil {
			r.debugf("match %d: observer %d activate: %v", matchIdx, i, err)
			r.stats.recordError()
			continue
		}
		go o.run()
	}
	r.debugf("match %d: %d observers active", matchIdx, len(observers))

	<-ctx.Done()

	// Close connections explicitly: they are created in loops, so deferring
	// inside the loop would leak until the whole match finishes anyway.
	for _, p := range players {
		p.close()
	}
	for _, o := range observers {
		o.close()
	}
}

// hostLoop uploads game-data pages on the page interval and pings.
func (r *Runner) hostLoop(ctx context.Context, host *wsPlayer, matchIdx int) {
	rng := rand.New(rand.NewPCG(uint64(matchIdx)+1, 2))
	pageTick := time.NewTicker(r.profile.PageInterval)
	defer pageTick.Stop()
	pingTick := time.NewTicker(r.profile.PingInterval)
	defer pingTick.Stop()
	r.debugf("hostLoop: started pageInterval=%s pingInterval=%s", r.profile.PageInterval, r.profile.PingInterval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-pageTick.C:
			if err := host.send(0x09, gameDataPage(pagePayload(rng, r.profile.PageMaxBytes))); err != nil {
				r.debugf("hostLoop: page send error: %v", err)
				r.stats.recordError()
			}
		case <-pingTick.C:
			if err := host.pingOnce(); err != nil {
				r.debugf("hostLoop: ping error: %v", err)
				r.stats.recordError()
			}
		}
	}
}

// playerLoop sends LobbyRelay messages to random peers and pings.
func (r *Runner) playerLoop(ctx context.Context, p *wsPlayer, matchIdx int, srcPeer byte) {
	rng := rand.New(rand.NewPCG(uint64(matchIdx)*100+uint64(srcPeer), 2))
	relayTick := time.NewTicker(r.profile.RelayInterval)
	defer relayTick.Stop()
	pingTick := time.NewTicker(r.profile.PingInterval)
	defer pingTick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-relayTick.C:
			dst := byte(1 + rng.IntN(r.profile.Players))
			if dst == srcPeer {
				dst = byte(1 + rng.IntN(r.profile.Players-1))
				if dst >= srcPeer {
					dst++
				}
			}
			if err := p.send(0x13, relayBody(srcPeer, dst, rng)); err != nil {
				r.debugf("playerLoop: relay send error: %v", err)
				r.stats.recordError()
			}
		case <-pingTick.C:
			if err := p.pingOnce(); err != nil {
				r.debugf("playerLoop: ping error: %v", err)
				r.stats.recordError()
			}
		}
	}
}
