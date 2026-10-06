package ratelimit

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/tvdavies/prwatch/internal/github"
)

func newGov(t *testing.T) (*Governor, *time.Time) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	g := New(filepath.Join(t.TempDir(), "rate.json"))
	g.Now = func() time.Time { return now }
	return g, &now
}

func TestRetryAfterIsHonoured(t *testing.T) {
	g, now := newGov(t)
	until := g.OnRateLimit(&github.RateLimitError{Secondary: true, RetryAfter: 90 * time.Second, Remaining: 4000})
	if want := now.Add(90 * time.Second); !until.Equal(want) {
		t.Fatalf("until %s, want %s", until, want)
	}
}

func TestPrimaryExhaustionWaitsForReset(t *testing.T) {
	g, now := newGov(t)
	reset := now.Add(17 * time.Minute)
	until := g.OnRateLimit(&github.RateLimitError{Remaining: 0, ResetAt: reset})
	if !until.Equal(reset.Add(time.Second)) {
		t.Fatalf("until %s, want reset+1s", until)
	}
}

func TestExponentialBackoffFromOneMinute(t *testing.T) {
	g, now := newGov(t)
	var gaps []time.Duration
	for i := 0; i < 7; i++ {
		until := g.OnRateLimit(&github.RateLimitError{Secondary: true, Remaining: -1})
		gaps = append(gaps, until.Sub(*now))
		*now = until
	}
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for i := range want {
		if gaps[i] != want[i] {
			t.Fatalf("gaps %v, want %v", gaps, want)
		}
	}
	// A success resets the sequence.
	g.Observe(github.RateInfo{Known: true, Limit: 5000, Remaining: 4000, ResetAt: now.Add(time.Hour)})
	if until := g.OnRateLimit(&github.RateLimitError{Secondary: true, Remaining: -1}); until.Sub(*now) != time.Minute {
		t.Fatalf("after success: %s", until.Sub(*now))
	}
}

func TestBackoffPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rate.json")
	g := New(path)
	until := g.OnRateLimit(&github.RateLimitError{RetryAfter: time.Hour, Remaining: -1})
	g2 := New(path)
	if !g2.BackoffUntil().Equal(until) {
		t.Fatalf("persisted %s, want %s", g2.BackoffUntil(), until)
	}
	if blocked, _ := g2.Blocked(); !blocked {
		t.Fatal("reloaded governor not blocked")
	}
}

func TestObserveZeroRemainingBlocksUntilReset(t *testing.T) {
	g, now := newGov(t)
	g.Observe(github.RateInfo{Known: true, Limit: 5000, Remaining: 0, ResetAt: now.Add(10 * time.Minute)})
	if blocked, until := g.Blocked(); !blocked || !until.Equal(now.Add(10*time.Minute+time.Second)) {
		t.Fatalf("blocked %v until %s", blocked, until)
	}
}

func TestBudgetStretchesInterval(t *testing.T) {
	g, now := newGov(t)
	base := 10 * time.Second
	if got := g.Interval(base); got != base {
		t.Fatalf("unknown budget: %s", got)
	}
	cases := []struct {
		remaining int
		want      time.Duration
	}{
		{5000, base},                 // 20% of 5000 = 1000 rounds in 1h: 3.6s, below base
		{500, 36 * time.Second},      // 100 rounds in 1h
		{50, 360 * time.Second},      // 10 rounds in 1h
		{4, time.Hour + time.Second}, // under one round: wait for reset
	}
	for _, c := range cases {
		g.Observe(github.RateInfo{Known: true, Limit: 5000, Remaining: c.remaining, ResetAt: now.Add(time.Hour)})
		if got := g.Interval(base); got != c.want {
			t.Errorf("remaining %d: interval %s, want %s", c.remaining, got, c.want)
		}
	}
	g.Share = 0.5
	g.SetRoundCost(2)
	g.Observe(github.RateInfo{Known: true, Limit: 5000, Remaining: 500, ResetAt: now.Add(time.Hour)})
	if got := g.Interval(base); got != 28800*time.Millisecond {
		t.Errorf("share 0.5, cost 2: %s", got)
	}
}

func TestReserveUntil(t *testing.T) {
	g, now := newGov(t)
	if !g.ReserveUntil().IsZero() {
		t.Fatal("unknown budget must not block")
	}
	reset := now.Add(time.Hour)
	g.Observe(github.RateInfo{Known: true, Limit: 5000, Remaining: 5000, ResetAt: reset})
	if !g.ReserveUntil().IsZero() {
		t.Fatal("a healthy budget must not block")
	}
	// 20% of 4 points does not pay for one round at cost 1.
	g.Observe(github.RateInfo{Known: true, Limit: 5000, Remaining: 4, ResetAt: reset})
	if got := g.ReserveUntil(); !got.Equal(reset.Add(time.Second)) {
		t.Fatalf("reserve until %s, want reset+1s", got)
	}
	*now = reset.Add(2 * time.Second)
	if !g.ReserveUntil().IsZero() {
		t.Fatal("the reserve must lift after the reset")
	}
}

func TestReserveSurvivesRestart(t *testing.T) {
	g, now := newGov(t)
	reset := now.Add(time.Hour)
	g.Observe(github.RateInfo{Known: true, Limit: 5000, Remaining: 4, ResetAt: reset})
	g.SetRoundCost(1)
	restarted := New(g.path)
	restarted.Now = g.Now
	if got := restarted.ReserveUntil(); !got.Equal(reset.Add(time.Second)) {
		t.Fatalf("after restart: reserve until %s, want reset+1s", got)
	}
}
