package daemon

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tvdavies/prwatch/internal/paths"
)

// Config controls the daemon. Defaults suit real use; tests shorten them
// through environment variables.
type Config struct {
	StateDir     string
	Version      string
	GraphQLURL   string
	IdleGrace    time.Duration // exit after this long with no waiters
	FastInterval time.Duration // when checks are pending or auto-merge is on
	SlowInterval time.Duration // otherwise
	MinInterval  time.Duration // floor for both intervals
	MinGap       time.Duration // floor between on-demand requests
	Debounce     time.Duration // batch new interest arriving together
	BudgetShare  float64       // share of remaining budget to spend before reset
	MinBackoff   time.Duration // floor for rate-limit back-off without retry-after
	LogLevel     slog.Level
}

// ConfigFromEnv builds a Config from defaults and PRWATCH_* variables.
func ConfigFromEnv(version string) (Config, error) {
	dir, err := paths.StateDir()
	if err != nil {
		return Config{}, err
	}
	c := Config{
		StateDir:     dir,
		Version:      version,
		GraphQLURL:   os.Getenv("PRWATCH_GRAPHQL_URL"),
		IdleGrace:    30 * time.Second,
		FastInterval: 10 * time.Second,
		SlowInterval: 60 * time.Second,
		MinInterval:  5 * time.Second,
		MinGap:       time.Second,
		Debounce:     150 * time.Millisecond,
		BudgetShare:  0.2,
		MinBackoff:   time.Minute,
		LogLevel:     slog.LevelInfo,
	}
	durs := []struct {
		env string
		dst *time.Duration
	}{
		{"PRWATCH_IDLE_GRACE", &c.IdleGrace},
		{"PRWATCH_POLL_FAST", &c.FastInterval},
		{"PRWATCH_POLL_SLOW", &c.SlowInterval},
		// The following exist for tests; lowering them against real GitHub
		// is a bad idea.
		{"PRWATCH_POLL_MIN", &c.MinInterval},
		{"PRWATCH_MIN_GAP", &c.MinGap},
		{"PRWATCH_DEBOUNCE", &c.Debounce},
		{"PRWATCH_BACKOFF_MIN", &c.MinBackoff},
	}
	for _, d := range durs {
		v := os.Getenv(d.env)
		if v == "" {
			continue
		}
		parsed, err := time.ParseDuration(v)
		if err != nil || parsed < 0 {
			return Config{}, fmt.Errorf("%s: invalid duration %q", d.env, v)
		}
		*d.dst = parsed
	}
	if v := os.Getenv("PRWATCH_BUDGET_SHARE"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f <= 0 || f > 1 {
			return Config{}, fmt.Errorf("PRWATCH_BUDGET_SHARE: want a number in (0, 1], got %q", v)
		}
		c.BudgetShare = f
	}
	// For tests: report a different version, to fake an older daemon.
	if v := os.Getenv("PRWATCH_DAEMON_VERSION"); v != "" {
		c.Version = v
	}
	if v := os.Getenv("PRWATCH_LOG_LEVEL"); v != "" {
		var lvl slog.Level
		if err := lvl.UnmarshalText([]byte(strings.ToUpper(v))); err != nil {
			return Config{}, fmt.Errorf("PRWATCH_LOG_LEVEL: %w", err)
		}
		c.LogLevel = lvl
	}
	if c.FastInterval < c.MinInterval {
		c.FastInterval = c.MinInterval
	}
	if c.SlowInterval < c.FastInterval {
		c.SlowInterval = c.FastInterval
	}
	return c, nil
}
