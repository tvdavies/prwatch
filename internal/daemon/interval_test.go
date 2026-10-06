package daemon

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/tvdavies/prwatch/internal/github"
	"github.com/tvdavies/prwatch/internal/ratelimit"
	"github.com/tvdavies/prwatch/internal/snapshot"
)

func testDaemon() *Daemon {
	return &Daemon{
		cfg: Config{FastInterval: 10 * time.Second, SlowInterval: 60 * time.Second, MinInterval: 5 * time.Second, Debounce: 150 * time.Millisecond, MinGap: time.Second},
		gov: ratelimit.New(""),
	}
}

func snap(f func(*snapshot.Snapshot)) *snapshot.Snapshot {
	s := &snapshot.Snapshot{State: "OPEN", Mergeable: "MERGEABLE", Checks: snapshot.Checks{State: "SUCCESS"}}
	f(s)
	s.Finalise()
	return s
}

func TestAdaptiveInterval(t *testing.T) {
	d := testDaemon()
	quiet := &watch{snap: snap(func(s *snapshot.Snapshot) {})}
	pending := &watch{snap: snap(func(s *snapshot.Snapshot) { s.Checks.State = "PENDING" })}
	auto := &watch{snap: snap(func(s *snapshot.Snapshot) { s.AutoMerge.Enabled = true })}
	unknown := &watch{snap: snap(func(s *snapshot.Snapshot) { s.Mergeable = "UNKNOWN" })}
	mergedAuto := &watch{snap: snap(func(s *snapshot.Snapshot) { s.State = "MERGED"; s.AutoMerge.Enabled = true })}
	unfetched := &watch{}
	cases := []struct {
		watches []*watch
		want    time.Duration
	}{
		{[]*watch{quiet}, 60 * time.Second},
		{[]*watch{quiet, pending}, 10 * time.Second},
		{[]*watch{auto}, 10 * time.Second},
		{[]*watch{unknown}, 10 * time.Second},
		{[]*watch{mergedAuto, unfetched}, 60 * time.Second},
	}
	for i, c := range cases {
		if got := d.baseIntervalLocked(c.watches); got != c.want {
			t.Errorf("case %d: %s, want %s", i, got, c.want)
		}
	}
	d.cfg.FastInterval = time.Second
	if got := d.baseIntervalLocked([]*watch{pending}); got != 5*time.Second {
		t.Errorf("minimum not enforced: %s", got)
	}
}

func TestIntervalStretchesWithBudget(t *testing.T) {
	d := testDaemon()
	d.gov.Observe(github.RateInfo{Known: true, Limit: 5000, Remaining: 100, ResetAt: time.Now().Add(time.Hour)})
	w := &watch{snap: snap(func(s *snapshot.Snapshot) { s.Checks.State = "PENDING" }), subs: map[*sub]struct{}{{}: {}}}
	d.watches = map[string]*watch{"o/r#1": w}
	if _, ok := d.nextWakeLocked(); !ok {
		t.Fatal("no wake with an active watch")
	}
	// 20% of 100 points at cost 1 is 20 rounds in an hour: 3 minutes.
	if d.interval < 179*time.Second || d.interval > 181*time.Second {
		t.Fatalf("interval %s, want about 3m", d.interval)
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("PRWATCH_STATE_DIR", t.TempDir())
	t.Setenv("PRWATCH_IDLE_GRACE", "45s")
	t.Setenv("PRWATCH_BUDGET_SHARE", "0.1")
	t.Setenv("PRWATCH_POLL_FAST", "1s")
	c, err := ConfigFromEnv("x")
	if err != nil {
		t.Fatal(err)
	}
	if c.IdleGrace != 45*time.Second || c.BudgetShare != 0.1 || c.FastInterval != 5*time.Second {
		t.Fatalf("config %+v", c)
	}
	t.Setenv("PRWATCH_BUDGET_SHARE", "2")
	if _, err := ConfigFromEnv("x"); err == nil {
		t.Fatal("share > 1 accepted")
	}
}

func TestNewInterestWaitsForBudgetGap(t *testing.T) {
	d := testDaemon()
	now := time.Now()
	d.gov.Observe(github.RateInfo{Known: true, Limit: 5000, Remaining: 4, ResetAt: now.Add(time.Hour)})
	w := &watch{needsRefresh: true, subs: map[*sub]struct{}{{}: {}}}
	d.watches = map[string]*watch{"o/r#1": w}
	d.pendingSince = now
	d.lastRequest = now
	at, ok := d.nextWakeLocked()
	if !ok || at.Before(now.Add(59*time.Minute)) {
		t.Fatalf("pending fetch scheduled at %s; with 4 points left it must wait for the reset", at.Sub(now))
	}
	// With a healthy budget the budget gap is 3.6s, but no two requests are
	// ever closer than the 5s poll floor.
	d.gov.Observe(github.RateInfo{Known: true, Limit: 5000, Remaining: 5000, ResetAt: now.Add(time.Hour)})
	at, _ = d.nextWakeLocked()
	if gap := at.Sub(now); gap < 5*time.Second || gap > 5100*time.Millisecond {
		t.Fatalf("pending fetch after %s; want the 5s minimum interval", gap)
	}
	// The floor also applies to scheduled rounds.
	d.watches["o/r#1"].needsRefresh = false
	d.watches["o/r#1"].nodeID = "PR_1"
	d.watches["o/r#1"].snap = snap(func(s *snapshot.Snapshot) { s.Checks.State = "PENDING" })
	d.cfg.FastInterval = time.Second
	d.lastRound = now
	at, _ = d.nextWakeLocked()
	if gap := at.Sub(now); gap < 5*time.Second {
		t.Fatalf("scheduled round after %s; want at least the 5s minimum interval", gap)
	}
}

func TestRestartWithLowStoredBudgetWaitsForReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rate.json")
	now := time.Now()
	reset := now.Add(time.Hour)
	// An earlier daemon left 4 points in rate.json.
	prev := ratelimit.New(path)
	prev.Observe(github.RateInfo{Known: true, Limit: 5000, Remaining: 4, ResetAt: reset})
	prev.SetRoundCost(1)

	d := testDaemon()
	d.gov = ratelimit.New(path)
	d.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	d.restoreRateState()
	if d.lastRequest.IsZero() {
		t.Fatal("lastRequest not restored from rate.json")
	}
	d.lastRequest = time.Time{} // even with no record of the last request
	d.watches = map[string]*watch{"o/r#1": {needsRefresh: true, subs: map[*sub]struct{}{{}: {}}}}
	d.pendingSince = now
	at, ok := d.nextWakeLocked()
	if !ok || at.Before(reset) {
		t.Fatalf("pending fetch scheduled in %s; with 4 stored points it must wait for the reset", at.Sub(now))
	}
	if !d.budgetBlocked() {
		t.Fatal("a round must not run while the budget is below the reserve")
	}
}
