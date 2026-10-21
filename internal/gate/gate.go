// Package gate implements the cooldown gate (design §1.5, §6.3): one deadline
// per key, a streak that widens the window, and full-jitter waiting.
//
// There is no queue and no lock. The data plane only touches atomics, and the
// single piece of cross-writer coordination is the CAS on until, which only ever
// moves the deadline forward. Every waiter owns its own timer, so the wait of
// one request is independent of how many others are waiting.
package gate

import (
	"context"
	"math"
	"math/rand/v2"
	"sync/atomic"
	"time"
)

// Gate is the cooldown state of one key (design §1.5).
type Gate struct {
	until  atomic.Int64 // cooldown deadline, UnixNano, only moves forward
	streak atomic.Int32 // consecutive interceptions of this key
	base   atomic.Int64 // duration of the most recent intercepted attempt, nanoseconds
}

// GateStat is one row of GET /v1/stats (design §5).
type GateStat struct {
	Key     string `json:"key"`
	UntilMS int64  `json:"until_ms"`
	Streak  int32  `json:"streak"`
	BaseMS  int64  `json:"base_ms"`
}

// maxBase bounds base for the overflow argument of design §6.4.1: Caddy's
// response_header_timeout (30m) is the longest an attempt can take, and
// now + 30m<<21 still fits int64 UnixNano. config.Validate bounds
// gate.base_floor_ms to the same value and gate.streak_cap to 22.
const maxBase = 30 * time.Minute

// OnRateLimit records one intercepted attempt and pushes the deadline forward.
//
// elapsed is how long that attempt took; floor is the configured lower bound
// (gate.base_floor_ms). streakCap is the configured ceiling for the streak,
// which config.Validate keeps within 1..23. The stored streak never exceeds
// streakCap, and the window is computed from this call's own base, not from
// whatever a concurrent writer stored meanwhile (design §1.5).
func (g *Gate) OnRateLimit(elapsed, floor time.Duration, streakCap int32) {
	base := min(max(elapsed, floor), maxBase)
	g.base.Store(int64(base))

	var s int32
	for {
		cur := g.streak.Load()
		s = min(cur+1, streakCap)
		if g.streak.CompareAndSwap(cur, s) {
			break
		}
	}
	if s < 1 {
		// Only reachable for a caller that skips config.Validate: a
		// non-positive streak would make the shift negative and panic.
		s = 1
	}

	// Saturate instead of wrapping: config.Validate keeps base<<(s-1) plus the
	// current UnixNano inside int64 (design §6.4.1), and this guards a caller
	// that bypasses it.
	window := time.Duration(math.MaxInt64)
	if shift := s - 1; base <= time.Duration(math.MaxInt64>>shift) {
		window = base << shift
	}
	now := time.Now().UnixNano()
	deadline := now + int64(window)
	if deadline < now {
		deadline = math.MaxInt64
	}
	for cur := g.until.Load(); deadline > cur && !g.until.CompareAndSwap(cur, deadline); cur = g.until.Load() {
	}
}

// OnReleased clears the streak after a released stream (design §6.3).
func (g *Gate) OnReleased() {
	g.streak.Store(0)
}

// Wait sleeps for a uniformly random duration in [0, until-now) and returns nil,
// or returns ctx.Err() as soon as ctx is done, whichever happens first
// (design §1.5).
func (g *Gate) Wait(ctx context.Context) error {
	remain := time.Duration(g.until.Load() - time.Now().UnixNano())
	if remain <= 0 {
		return nil
	}

	t := time.NewTimer(rand.N(remain))
	defer t.Stop()

	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// stat snapshots one gate for /v1/stats.
func (g *Gate) stat(key string) GateStat {
	return GateStat{
		Key:     key,
		UntilMS: g.until.Load() / int64(time.Millisecond),
		Streak:  g.streak.Load(),
		BaseMS:  g.base.Load() / int64(time.Millisecond),
	}
}
