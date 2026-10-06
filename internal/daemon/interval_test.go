package daemon

import (
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
