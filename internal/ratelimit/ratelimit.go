// Package ratelimit tracks GitHub's primary rate limit, enforces strict
// back-off after rate-limit responses, and stretches the poll interval so
// prwatch spends at most a configured share of the remaining budget.
package ratelimit

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tvdavies/prwatch/internal/github"
)

// State is persisted to rate.json.
type State struct {
	Limit         int       `json:"limit"`
	Remaining     int       `json:"remaining"`
	Used          int       `json:"used"`
	ResetAt       time.Time `json:"resetAt"`
	LastCost      int       `json:"lastCost"`
	RoundCost     int       `json:"roundCost"`
	BackoffUntil  time.Time `json:"backoffUntil"`
	BackoffReason string    `json:"backoffReason,omitempty"`
	Consecutive   int       `json:"consecutive"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// Governor is safe for concurrent use.
type Governor struct {
	mu         sync.Mutex
	st         State
	path       string
	Now        func() time.Time
	MinBackoff time.Duration // floor for rate limits without retry-after (default 60s)
	MaxBackoff time.Duration // cap for exponential back-off (default 30m)
	Share      float64       // share of remaining budget to spend (default 0.2)
}

// New returns a governor persisting to path ("" disables persistence) and
// loads any previous state.
func New(path string) *Governor {
	g := &Governor{path: path, Now: time.Now, MinBackoff: time.Minute, MaxBackoff: 30 * time.Minute, Share: 0.2}
	g.st.Remaining = -1
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			var st State
			if json.Unmarshal(b, &st) == nil {
				g.st = st
			}
		}
	}
	return g
}

// Snapshot returns a copy of the current state.
func (g *Governor) Snapshot() State {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.st
}

// BackoffUntil returns the time before which no request may be sent.
func (g *Governor) BackoffUntil() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.st.BackoffUntil
}

// Blocked reports whether a back-off is in force now.
func (g *Governor) Blocked() (bool, time.Time) {
	until := g.BackoffUntil()
	return g.Now().Before(until), until
}

// Observe records rate information from a successful response. A
// successful request clears the consecutive back-off counter.
func (g *Governor) Observe(r github.RateInfo) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if r.Known {
		g.st.Limit, g.st.Remaining, g.st.Used = r.Limit, r.Remaining, r.Used
		if !r.ResetAt.IsZero() {
			g.st.ResetAt = r.ResetAt
		}
		g.st.LastCost = r.Cost
	}
	g.st.Consecutive = 0
	g.st.BackoffReason = ""
	if g.st.Remaining == 0 && g.st.ResetAt.After(g.Now()) {
		g.st.BackoffUntil = g.st.ResetAt.Add(time.Second)
		g.st.BackoffReason = "primary budget exhausted"
	}
	g.st.UpdatedAt = g.Now()
	g.saveLocked()
}

// RecordRequest notes that a request was sent at t, even if no response
// was observed (it was cancelled at shutdown, say), so that the next daemon
// keeps the minimum gap after it. It never moves the time backwards.
func (g *Governor) RecordRequest(t time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if t.IsZero() || !t.After(g.st.UpdatedAt) {
		return
	}
	g.st.UpdatedAt = t
	g.saveLocked()
}

// SetRoundCost records the total cost of the last poll round.
func (g *Governor) SetRoundCost(c int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.st.RoundCost = c
	g.saveLocked()
}

// OnRateLimit applies GitHub's documented back-off rules and returns the
// time before which no further request may be sent:
//   - honour retry-after when present;
//   - if the primary budget is exhausted, wait until x-ratelimit-reset;
//   - otherwise wait at least MinBackoff, doubling on each consecutive hit.
func (g *Governor) OnRateLimit(e *github.RateLimitError) time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.Now()
	var until time.Time
	switch {
	case e.RetryAfter > 0:
		until = now.Add(e.RetryAfter)
		g.st.BackoffReason = "retry-after"
	case e.Remaining == 0 && e.ResetAt.After(now):
		until = e.ResetAt.Add(time.Second)
		g.st.BackoffReason = "primary budget exhausted"
	default:
		d := g.MinBackoff
		for i := 0; i < g.st.Consecutive && d < g.MaxBackoff; i++ {
			d *= 2
		}
		if d > g.MaxBackoff {
			d = g.MaxBackoff
		}
		until = now.Add(d)
		g.st.BackoffReason = "rate limited, exponential back-off"
	}
	if e.Secondary {
		g.st.BackoffReason = "secondary limit: " + g.st.BackoffReason
	}
	if e.Remaining >= 0 {
		g.st.Remaining = e.Remaining
	}
	if !e.ResetAt.IsZero() {
		g.st.ResetAt = e.ResetAt
	}
	g.st.Consecutive++
	if until.After(g.st.BackoffUntil) {
		g.st.BackoffUntil = until
	}
	g.st.UpdatedAt = now
	g.saveLocked()
	return g.st.BackoffUntil
}

// allowedRoundsLocked returns how many more rounds Share of the remaining
// budget pays for before the reset, or false when the budget is unknown or
// the reset has passed.
func (g *Governor) allowedRoundsLocked(now time.Time) (float64, bool) {
	if g.st.Remaining < 0 || !g.st.ResetAt.After(now) {
		return 0, false
	}
	// Remaining is -1 until a response reports it, so any other value is
	// known, even if no successful response has reported the limit yet.
	cost := g.st.RoundCost
	if cost < 1 {
		cost = 1
	}
	return g.Share * float64(g.st.Remaining) / float64(cost), true
}

// ReserveUntil returns the time before which no request may be sent because
// the known budget is exhausted or below the reserve (Share of it does not
// pay for one more round). It is zero when the budget allows a request. The
// state comes from rate.json, so it also holds across daemon restarts.
func (g *Governor) ReserveUntil() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	if rounds, ok := g.allowedRoundsLocked(g.Now()); ok && rounds < 1 {
		return g.st.ResetAt.Add(time.Second)
	}
	return time.Time{}
}

// Interval stretches base so that, at the last round cost, prwatch spends no
// more than Share of the remaining budget before the reset.
func (g *Governor) Interval(base time.Duration) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.Now()
	allowedRounds, ok := g.allowedRoundsLocked(now)
	if !ok {
		return base
	}
	untilReset := g.st.ResetAt.Sub(now)
	if allowedRounds < 1 {
		return untilReset + time.Second
	}
	stretched := time.Duration(float64(untilReset) / allowedRounds)
	if stretched > base {
		return stretched
	}
	return base
}

func (g *Governor) saveLocked() {
	if g.path == "" {
		return
	}
	b, err := json.MarshalIndent(g.st, "", "  ")
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(g.path), ".rate-*.json")
	if err != nil {
		return
	}
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmp.Name())
		return
	}
	if err := os.Rename(tmp.Name(), g.path); err != nil {
		_ = os.Remove(tmp.Name())
	}
}

// ErrBackoff is returned when a request is refused because of back-off.
var ErrBackoff = errors.New("rate-limit back-off in force")
