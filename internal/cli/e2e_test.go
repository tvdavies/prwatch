package cli_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/tvdavies/prwatch/internal/cli"
	"github.com/tvdavies/prwatch/internal/fakegh"
	"github.com/tvdavies/prwatch/internal/github"
	"github.com/tvdavies/prwatch/internal/protocol"
	"github.com/tvdavies/prwatch/internal/snapshot"
)

// TestMain lets the test binary act as the prwatch executable, so the CLI
// and the daemon it spawns via os.Executable() run as real processes.
func TestMain(m *testing.M) {
	if os.Getenv("PRWATCH_TEST_EXEC") == "1" {
		os.Exit(cli.Main(os.Args[1:], "test"))
	}
	os.Exit(m.Run())
}

type env struct {
	t     *testing.T
	dir   string
	fake  *fakegh.Server
	extra []string
	bin   string // executable to run; the test binary when empty
}

func newEnv(t *testing.T, extra ...string) *env {
	t.Helper()
	t.Parallel()
	// Unix socket paths are limited to ~104 bytes, and macOS's TMPDIR is
	// long, so use /tmp directly.
	dir, err := os.MkdirTemp("/tmp", "pw")
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, dir: dir, fake: fakegh.New(), extra: extra}
	t.Cleanup(func() {
		e.run("daemon", "stop")
		e.fake.Close()
		if t.Failed() {
			if b, err := os.ReadFile(filepath.Join(dir, "daemon.log")); err == nil {
				t.Logf("daemon.log:\n%s", b)
			}
		}
		os.RemoveAll(dir)
	})
	return e
}

func (e *env) cmd(args ...string) *exec.Cmd {
	bin := e.bin
	if bin == "" {
		bin = os.Args[0]
	}
	c := exec.Command(bin, args...)
	c.Env = append(os.Environ(),
		"PRWATCH_TEST_EXEC=1",
		"PRWATCH_STATE_DIR="+e.dir,
		"PRWATCH_GRAPHQL_URL="+e.fake.URL,
		"GH_TOKEN=fake-token",
		"GITHUB_TOKEN=",
		"PRWATCH_BIN=",
		"PRWATCH_IDLE_GRACE=1s",
		"PRWATCH_POLL_FAST=300ms",
		"PRWATCH_POLL_SLOW=300ms",
		"PRWATCH_POLL_MIN=100ms",
		"PRWATCH_MIN_GAP=20ms",
		"PRWATCH_DEBOUNCE=30ms",
		"PRWATCH_BACKOFF_MIN=300ms",
		"PRWATCH_LOG_LEVEL=debug",
	)
	c.Env = append(c.Env, e.extra...)
	return c
}

type result struct {
	stdout, stderr string
	code           int
}

// textBusyRetries bounds retries of an exec that fails with ETXTBSY. A test
// that has just written an executable can see that when another test forks
// while the file is still open for writing (golang/go#22315).
const textBusyRetries = 50

func (e *env) run(args ...string) result {
	var out, errb bytes.Buffer
	var err error
	for i := 0; ; i++ {
		c := e.cmd(args...)
		c.Stdout, c.Stderr = &out, &errb
		err = c.Run()
		if !errors.Is(err, syscall.ETXTBSY) || i == textBusyRetries {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		e.t.Fatalf("run %v: %v", args, err)
	}
	return result{out.String(), errb.String(), code}
}

type proc struct {
	cmd  *exec.Cmd
	out  *syncBuf
	errb *syncBuf
	done chan result
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (e *env) start(args ...string) *proc {
	e.t.Helper()
	p := &proc{out: &syncBuf{}, errb: &syncBuf{}, done: make(chan result, 1)}
	for i := 0; ; i++ {
		p.cmd = e.cmd(args...)
		p.cmd.Stdout, p.cmd.Stderr = p.out, p.errb
		err := p.cmd.Start()
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.ETXTBSY) || i == textBusyRetries {
			e.t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	go func() {
		err := p.cmd.Wait()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		p.done <- result{p.out.String(), p.errb.String(), code}
	}()
	e.t.Cleanup(func() { _ = p.cmd.Process.Kill() })
	return p
}

func (p *proc) wait(t *testing.T, d time.Duration) result {
	t.Helper()
	select {
	case r := <-p.done:
		return r
	case <-time.After(d):
		t.Fatalf("process %v still running after %s; stderr: %s", p.cmd.Args[1:], d, p.errb.String())
	}
	return result{}
}

func (p *proc) running() bool {
	select {
	case r := <-p.done:
		p.done <- r
		return false
	default:
		return true
	}
}

type listOut struct {
	PRs    []protocol.Watched `json:"prs"`
	Daemon *struct {
		PID     int    `json:"pid"`
		Version string `json:"version"`
	} `json:"daemon"`
	DaemonVersion *string `json:"daemonVersion"`
}

func (e *env) list() listOut {
	e.t.Helper()
	r := e.run("list", "--json")
	var l listOut
	if err := json.Unmarshal([]byte(r.stdout), &l); err != nil {
		e.t.Fatalf("list output %q: %v", r.stdout, err)
	}
	return l
}

func eventually(t *testing.T, d time.Duration, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", d, what)
}

// waitWatched waits until the daemon has fetched snapshots for n PRs with
// at least `waiters` waiters in total.
func (e *env) waitWatched(n, waiters int) {
	e.t.Helper()
	eventually(e.t, 10*time.Second, fmt.Sprintf("%d watched PRs with %d waiters", n, waiters), func() bool {
		l := e.list()
		total := 0
		for _, w := range l.PRs {
			if w.Snapshot == nil {
				return false
			}
			total += w.Waiters + w.Streams
		}
		return len(l.PRs) == n && total == waiters
	})
}

func (e *env) daemonRunning() bool {
	c, err := net.Dial("unix", filepath.Join(e.dir, "prwatch.sock"))
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func (e *env) logText() string {
	b, _ := os.ReadFile(filepath.Join(e.dir, "daemon.log"))
	return string(b)
}

func parseSnap(t *testing.T, s string) *snapshot.Snapshot {
	t.Helper()
	var snap snapshot.Snapshot
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &snap); err != nil {
		t.Fatalf("snapshot %q: %v", s, err)
	}
	return &snap
}

func merge(p *github.RawPR) {
	p.State, p.Merged = "MERGED", true
	now := time.Now().UTC()
	p.MergedAt = &now
	p.MergeCommit = &github.OID{Oid: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
}

func TestWaitersOnDifferentPRsShareOneRequestPerRound(t *testing.T) {
	e := newEnv(t)
	a := e.fake.AddPR("o", "r", 1)
	b := e.fake.AddPR("o", "other", 2)
	pa := e.start("wait", "o/r#1", "--for", "merged", "--json")
	pb := e.start("wait", "https://github.com/o/other/pull/2", "--for", "merged", "--json")
	e.waitWatched(2, 2)

	before := len(e.fake.Requests())
	time.Sleep(1500 * time.Millisecond) // about five 300ms rounds
	reqs := e.fake.Requests()[before:]
	if len(reqs) < 3 {
		t.Fatalf("expected several poll rounds, got %d requests", len(reqs))
	}
	for i, r := range reqs {
		if r.Kind != "poll" || len(r.PRs) != 2 {
			t.Fatalf("request %d: kind %s with %d PRs, want one poll covering both", i, r.Kind, len(r.PRs))
		}
		if i > 0 && r.At.Sub(reqs[i-1].At) < 200*time.Millisecond {
			t.Fatalf("requests %d and %d only %s apart", i-1, i, r.At.Sub(reqs[i-1].At))
		}
	}

	e.fake.Update(a, merge)
	e.fake.Update(b, merge)
	for _, p := range []*proc{pa, pb} {
		r := p.wait(t, 5*time.Second)
		if r.code != 0 {
			t.Fatalf("exit %d: %s", r.code, r.stderr)
		}
		if s := parseSnap(t, r.stdout); !s.Merged || s.SchemaVersion != 1 {
			t.Fatalf("unexpected snapshot %+v", s)
		}
	}
}

func TestTwoWaitersSamePR(t *testing.T) {
	e := newEnv(t)
	k := e.fake.AddPR("o", "r", 1)
	e.fake.Update(k, func(p *github.RawPR) { fakegh.SetChecks(p, "PENDING") })
	p1 := e.start("wait", "o/r#1", "--for", "checks")
	p2 := e.start("wait", "o/r#1", "--for", "change")
	e.waitWatched(1, 2)
	before := len(e.fake.Requests())
	time.Sleep(700 * time.Millisecond)
	for _, r := range e.fake.Requests()[before:] {
		if len(r.PRs) != 1 {
			t.Fatalf("poll included %d PRs, want the shared PR once", len(r.PRs))
		}
	}
	if !p1.running() || !p2.running() {
		t.Fatal("waiters returned before any change")
	}
	e.fake.Update(k, func(p *github.RawPR) { fakegh.SetChecks(p, "FAILURE") })
	for _, p := range []*proc{p1, p2} {
		r := p.wait(t, 5*time.Second)
		if r.code != 0 || !strings.Contains(r.stdout, "checks: FAILURE") {
			t.Fatalf("exit %d, stdout %q", r.code, r.stdout)
		}
		if !strings.Contains(r.stdout, "required_check_failed") {
			t.Fatalf("expected needsAction reason, got %q", r.stdout)
		}
	}
}

func TestDaemonExitsAfterIdleGrace(t *testing.T) {
	e := newEnv(t)
	e.fake.AddPR("o", "r", 1)
	r := e.run("wait", "o/r#1", "--for", "merged", "--timeout", "500ms")
	if r.code != 124 {
		t.Fatalf("exit %d, want 124: %s", r.code, r.stderr)
	}
	if !e.daemonRunning() {
		t.Fatal("daemon should still be running within the grace period")
	}
	eventually(t, 4*time.Second, "daemon to exit after grace", func() bool { return !e.daemonRunning() })
	if _, err := os.Stat(filepath.Join(e.dir, "prwatch.sock")); !os.IsNotExist(err) {
		t.Fatalf("socket file left behind: %v", err)
	}
	if !strings.Contains(e.logText(), "idle grace elapsed") {
		t.Fatal("expected idle exit in log")
	}
}

func TestKilledWaiterDropsInterest(t *testing.T) {
	e := newEnv(t)
	e.fake.AddPR("o", "r", 1)
	e.fake.AddPR("o", "r", 2)
	keep := e.start("wait", "o/r#2", "--for", "merged")
	victim := e.start("wait", "o/r#1", "--for", "merged")
	e.waitWatched(2, 2)
	if err := victim.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	victim.wait(t, 2*time.Second)
	eventually(t, 3*time.Second, "interest in o/r#1 to drop", func() bool {
		l := e.list()
		return len(l.PRs) == 1 && strings.EqualFold(l.PRs[0].PR, "o/r#2")
	})
	before := len(e.fake.Requests())
	time.Sleep(700 * time.Millisecond)
	for _, r := range e.fake.Requests()[before:] {
		for _, pr := range r.PRs {
			if pr == "o/r#1" {
				t.Fatal("daemon still polls the killed waiter's PR")
			}
		}
	}
	if !keep.running() {
		t.Fatal("other waiter should be unaffected")
	}
}

func TestSinceReturnsAtOnceAfterChangeBetweenCalls(t *testing.T) {
	e := newEnv(t)
	k := e.fake.AddPR("o", "r", 1)
	// An events stream keeps the daemon polling between the agent's calls.
	ev := e.start("events", "--pr", "o/r#1", "--json")
	e.waitWatched(1, 1)
	first := e.run("wait", "o/r#1", "--json", "--timeout", "1s")
	if first.code != 124 {
		t.Fatalf("exit %d", first.code)
	}
	tok := parseSnap(t, first.stdout).Token

	e.fake.Update(k, func(p *github.RawPR) { p.Title = "Renamed" })
	eventually(t, 5*time.Second, "events stream to see the change", func() bool {
		return strings.Count(ev.out.String(), "\n") >= 2
	})

	start := time.Now()
	r := e.run("wait", "o/r#1", "--since", tok, "--json")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("--since took %s; should return at once", took)
	}
	s := parseSnap(t, r.stdout)
	if s.Token == tok || s.Title != "Renamed" {
		t.Fatalf("unexpected snapshot: token %s title %q", s.Token, s.Title)
	}

	// With the daemon gone between calls, a fresh daemon still notices.
	e.run("daemon", "stop")
	e.fake.Update(k, func(p *github.RawPR) { p.Title = "Renamed again" })
	r = e.run("wait", "o/r#1", "--since", s.Token, "--json", "--timeout", "5s")
	if r.code != 0 || parseSnap(t, r.stdout).Title != "Renamed again" {
		t.Fatalf("exit %d: %s %s", r.code, r.stdout, r.stderr)
	}
}

// A waiter that registers while the cached snapshot is still fresh must
// re-arm the poller. The last subscriber leaving parks the poll loop with no
// timer, and serving the cached snapshot used to skip the kick, so the poller
// slept until some other client happened to kick it: the waiter never saw new
// reviews (27 minutes in the field, with a one-minute interval).
func TestWaiterServedFromCacheKeepsPolling(t *testing.T) {
	// A long interval keeps the first snapshot fresh while the second waiter
	// registers, which is the window the bug needs.
	e := newEnv(t, "PRWATCH_POLL_FAST=3s", "PRWATCH_POLL_SLOW=3s")
	k := e.fake.AddPR("o", "r", 1)

	// The first wait starts the daemon, is answered by the first poll, and
	// leaves: the PR has no subscribers for a moment.
	first := e.run("wait", "o/r#1", "--json", "--timeout", "1s")
	if first.code != 124 {
		t.Fatalf("exit %d: %s", first.code, first.stderr)
	}
	tok := parseSnap(t, first.stdout).Token

	// The next wait arrives inside the interval and is served from the cache.
	p := e.start("wait", "o/r#1", "--since", tok, "--json", "--timeout", "15s")
	e.waitWatched(1, 1)
	e.fake.Update(k, func(p *github.RawPR) { p.Title = "Renamed" })

	r := p.wait(t, 20*time.Second)
	if r.code != 0 {
		t.Fatalf("waiter served from the cache never saw the change: exit %d: %s", r.code, r.stderr)
	}
	if s := parseSnap(t, r.stdout); s.Title != "Renamed" {
		t.Fatalf("unexpected snapshot title %q", s.Title)
	}
}

func TestForConditions(t *testing.T) {
	e := newEnv(t)
	type tc struct {
		cond    string
		setup   func(*github.RawPR)
		noise   func(*github.RawPR) // a change that must not satisfy cond
		satisfy func(*github.RawPR)
	}
	approved := "APPROVED"
	required := "REVIEW_REQUIRED"
	cases := []tc{
		{"change", nil, nil, func(p *github.RawPR) { p.Comments.TotalCount++ }},
		{"checks", func(p *github.RawPR) { fakegh.SetChecks(p, "PENDING") }, func(p *github.RawPR) { p.Title = "noise" },
			func(p *github.RawPR) { fakegh.SetChecks(p, "SUCCESS") }},
		{"review", nil, func(p *github.RawPR) { p.Title = "noise" }, func(p *github.RawPR) {
			now := time.Now().UTC()
			p.LatestReviews.Nodes = append(p.LatestReviews.Nodes, github.RawReview{Author: &github.Actor{Login: "rev"}, State: "COMMENTED", SubmittedAt: &now})
			p.Reviews.TotalCount++
		}},
		{"mergeable", func(p *github.RawPR) { p.ReviewDecision = &required; p.MergeStateStatus = "BLOCKED" },
			func(p *github.RawPR) { p.Title = "noise" },
			func(p *github.RawPR) { p.ReviewDecision = &approved; p.MergeStateStatus = "CLEAN" }},
		{"merged", nil, func(p *github.RawPR) { p.Title = "noise" }, merge},
		{"closed", nil, func(p *github.RawPR) { p.Title = "noise" }, func(p *github.RawPR) { p.State = "CLOSED" }},
	}
	keys := make([]string, len(cases))
	procs := make([]*proc, len(cases))
	for i, c := range cases {
		keys[i] = e.fake.AddPR("o", "r", i+1)
		if c.setup != nil {
			e.fake.Update(keys[i], c.setup)
		}
		procs[i] = e.start("wait", fmt.Sprintf("o/r#%d", i+1), "--for", c.cond, "--json")
	}
	e.waitWatched(len(cases), len(cases))
	for i, c := range cases {
		if c.noise != nil {
			e.fake.Update(keys[i], c.noise)
		}
	}
	time.Sleep(800 * time.Millisecond)
	for i, c := range cases {
		if !procs[i].running() {
			t.Fatalf("--for %s returned before its condition held: %s", c.cond, procs[i].out.String())
		}
	}
	for i, c := range cases {
		e.fake.Update(keys[i], c.satisfy)
		r := procs[i].wait(t, 5*time.Second)
		if r.code != 0 {
			t.Fatalf("--for %s: exit %d: %s", c.cond, r.code, r.stderr)
		}
	}
}

func TestLevelConditionMetImmediately(t *testing.T) {
	e := newEnv(t)
	k := e.fake.AddPR("o", "r", 1)
	e.fake.Update(k, merge)
	r := e.run("wait", "o/r#1", "--for", "merged", "--timeout", "5s")
	if r.code != 0 || !strings.Contains(r.stdout, "MERGED") {
		t.Fatalf("exit %d: %q %q", r.code, r.stdout, r.stderr)
	}
}

func TestConcurrentStartLeavesOneDaemon(t *testing.T) {
	e := newEnv(t)
	e.fake.AddPR("o", "r", 1)
	var procs []*proc
	for i := 0; i < 8; i++ {
		procs = append(procs, e.start("wait", "o/r#1", "--for", "merged"))
	}
	e.waitWatched(1, 8)
	if n := strings.Count(e.logText(), "daemon started"); n != 1 {
		t.Fatalf("%d daemons started, want 1\n%s", n, e.logText())
	}
	resolves := 0
	for _, r := range e.fake.Requests() {
		if r.Kind == "resolve" {
			resolves++
		}
	}
	if resolves != 1 {
		t.Fatalf("%d resolve requests, want 1", resolves)
	}
	for _, p := range procs {
		_ = p.cmd.Process.Kill()
	}
}

func TestStaleSocketRecovered(t *testing.T) {
	e := newEnv(t)
	k := e.fake.AddPR("o", "r", 1)
	e.fake.Update(k, merge)
	sock := filepath.Join(e.dir, "prwatch.sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	l.Close()
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("stale socket not created: %v", err)
	}
	r := e.run("wait", "o/r#1", "--for", "merged", "--timeout", "5s")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
}

func TestTimeoutExits124(t *testing.T) {
	e := newEnv(t)
	e.fake.AddPR("o", "r", 1)
	start := time.Now()
	r := e.run("wait", "o/r#1", "--for", "merged", "--timeout", "1", "--json")
	if r.code != 124 {
		t.Fatalf("exit %d, want 124", r.code)
	}
	if d := time.Since(start); d < time.Second || d > 4*time.Second {
		t.Fatalf("timeout took %s", d)
	}
	if s := parseSnap(t, r.stdout); s.State != "OPEN" {
		t.Fatalf("expected the last snapshot on timeout, got %q", r.stdout)
	}
}

func TestNotFoundExits3(t *testing.T) {
	e := newEnv(t)
	e.fake.AddPR("o", "r", 1)
	if r := e.run("wait", "o/r#99", "--timeout", "5s"); r.code != 3 {
		t.Fatalf("missing PR: exit %d: %s", r.code, r.stderr)
	}
	if r := e.run("wait", "nope/nothing#1", "--timeout", "5s"); r.code != 3 {
		t.Fatalf("missing repo: exit %d: %s", r.code, r.stderr)
	}
	if r := e.run("status", "o/r#1", "o/r#99"); r.code != 3 || !strings.Contains(r.stdout, "o/r#1") {
		t.Fatalf("status with one missing PR: exit %d: %q", r.code, r.stdout)
	}
}

func TestAuthErrorExits2(t *testing.T) {
	e := newEnv(t, "GH_TOKEN=bad-token")
	e.fake.AddPR("o", "r", 1)
	if r := e.run("wait", "o/r#1", "--timeout", "5s"); r.code != 2 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if r := e.run("status", "o/r#1"); r.code != 2 {
		t.Fatalf("status: exit %d: %s", r.code, r.stderr)
	}
}

func TestUsageErrorsExit2(t *testing.T) {
	e := newEnv(t)
	for _, args := range [][]string{
		{"wait"},
		{"wait", "o/r#1", "--for", "nonsense"},
		{"wait", "o/r#1", "--since", "garbage"},
		{"wait", "not a pr"},
		{"status", "not a pr"},
		{"events", "--pr", "not a pr"},
		{"events", "--pr", ""},
		{"events", "--pr", ",,"},
		{"bogus"},
	} {
		if r := e.run(args...); r.code != 2 {
			t.Errorf("%v: exit %d, want 2", args, r.code)
		}
	}
}

func TestListDoesNotKeepDaemonAlive(t *testing.T) {
	e := newEnv(t)
	e.fake.AddPR("o", "r", 1)

	r := e.run("list")
	if r.code != 0 || !strings.Contains(r.stdout, "Nothing is being watched") {
		t.Fatalf("list without daemon: exit %d %q", r.code, r.stdout)
	}
	if e.daemonRunning() {
		t.Fatal("list started a daemon")
	}

	p := e.start("wait", "o/r#1", "--for", "merged")
	e.waitWatched(1, 1)
	r = e.run("list")
	for _, want := range []string{"o/r#1", "1 waiter (merged)", "OPEN", "poll interval", "budget"} {
		if !strings.Contains(r.stdout, want) {
			t.Fatalf("list output missing %q:\n%s", want, r.stdout)
		}
	}
	_ = p.cmd.Process.Kill()
	p.wait(t, 2*time.Second)

	stop := time.Now().Add(1300 * time.Millisecond)
	for time.Now().Before(stop) && e.daemonRunning() {
		e.run("list")
		time.Sleep(100 * time.Millisecond)
	}
	eventually(t, 2*time.Second, "daemon to exit despite list calls", func() bool { return !e.daemonRunning() })
}

func TestStatusDirectAndViaDaemon(t *testing.T) {
	e := newEnv(t)
	k := e.fake.AddPR("o", "r", 1)
	e.fake.Update(k, func(p *github.RawPR) {
		line := 12
		p.ReviewThreads.TotalCount = 1
		p.ReviewThreads.Nodes = []github.RawThread{{ID: "T1", Path: "main.go", Line: &line}}
	})
	e.fake.SetThreadHead("T1", "alice", "Please rename this")

	r := e.run("status", "o/r#1", "--json")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if e.daemonRunning() {
		t.Fatal("status started a daemon")
	}
	var snaps []snapshot.Snapshot
	if err := json.Unmarshal([]byte(r.stdout), &snaps); err != nil || len(snaps) != 1 {
		t.Fatalf("status json %q: %v", r.stdout, err)
	}
	th := snaps[0].Threads
	if th.Unresolved != 1 || th.Items[0].Author != "alice" || th.Items[0].Excerpt != "Please rename this" {
		t.Fatalf("threads: %+v", th)
	}
	if !snaps[0].NeedsAction || snaps[0].Reasons[0] != snapshot.ReasonUnresolvedThreads {
		t.Fatalf("reasons: %v", snaps[0].Reasons)
	}

	p := e.start("wait", "o/r#1", "--for", "merged")
	e.waitWatched(1, 1)
	before := len(e.fake.Requests())
	r = e.run("status", "o/r#1")
	if r.code != 0 || !strings.Contains(r.stdout, "main.go:12  alice: Please rename this") {
		t.Fatalf("status via daemon: exit %d %q", r.code, r.stdout)
	}
	for _, req := range e.fake.Requests()[before:] {
		if req.Kind != "poll" {
			t.Fatalf("status via daemon made a %s request", req.Kind)
		}
	}
	_ = p.cmd.Process.Kill()
}

func TestEventsStream(t *testing.T) {
	e := newEnv(t)
	k := e.fake.AddPR("o", "r", 1)
	e.fake.Update(k, func(p *github.RawPR) { fakegh.SetChecks(p, "PENDING") })
	ev := e.start("events", "--pr", "o/r#1")
	all := e.start("events", "--json")
	e.waitWatched(1, 1)
	e.fake.Update(k, func(p *github.RawPR) { fakegh.SetChecks(p, "SUCCESS") })
	eventually(t, 5*time.Second, "change event", func() bool {
		return strings.Contains(ev.out.String(), "checks PENDING→SUCCESS") &&
			strings.Contains(all.out.String(), `"changes":["checks PENDING→SUCCESS"]`)
	})
	l := e.run("list")
	if !strings.Contains(l.stdout, "1 stream") || !strings.Contains(l.stdout, "1 events stream on all PRs") {
		t.Fatalf("list:\n%s", l.stdout)
	}
	// The --pr stream started with the state at subscribe time, so a change
	// made before it started is never lost.
	if first, _, _ := strings.Cut(ev.out.String(), "\n"); !strings.Contains(first, "o/r#1: initial") {
		t.Fatalf("first events line %q, want the initial state", first)
	}
}

func TestStatusReportsBadReferencesAndPrintsTheRest(t *testing.T) {
	e := newEnv(t)
	e.fake.AddPR("o", "r", 1)
	e.fake.AddPR("o", "r", 2)

	r := e.run("status", "--json", "o/r#1", "440#infrastructure", "o/r#2")
	if r.code != 1 || !strings.Contains(r.stderr, `"440#infrastructure"`) {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
	var snaps []snapshot.Snapshot
	if err := json.Unmarshal([]byte(r.stdout), &snaps); err != nil || len(snaps) != 2 || snaps[0].PR != "o/r#1" || snaps[1].PR != "o/r#2" {
		t.Fatalf("status json %q: %v", r.stdout, err)
	}

	r = e.run("status", "--json", "440#infrastructure", "not a pr")
	if r.code != 2 || r.stdout != "" {
		t.Fatalf("no valid reference: exit %d, stdout %q", r.code, r.stdout)
	}
}

func TestStatusReportsBadReferencesWhenTheFetchFails(t *testing.T) {
	e := newEnv(t, "GH_TOKEN=bad-token")
	e.fake.AddPR("o", "r", 1)
	r := e.run("status", "440#infrastructure", "o/r#1")
	if r.code != 2 || !strings.Contains(r.stderr, `"440#infrastructure"`) {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
}

func TestEventsSkipsBadAndUnreadablePRs(t *testing.T) {
	e := newEnv(t)
	k := e.fake.AddPR("o", "r", 1)
	e.fake.Update(k, func(p *github.RawPR) { fakegh.SetChecks(p, "PENDING") })

	if r := e.run("events", "--pr", "440#infrastructure"); r.code != 2 {
		t.Fatalf("events with no valid PR: exit %d, want 2 (never a stream on every PR)", r.code)
	}

	ev := e.start("events", "--json", "--pr", "440#infrastructure", "--pr", "o/r#99", "--pr", "o/r#1")
	eventually(t, 5*time.Second, "an error line for the missing PR", func() bool {
		return strings.Contains(ev.out.String(), `"type":"error","time"`) && strings.Contains(ev.out.String(), `"pr":"o/r#99"`)
	})
	e.fake.Update(k, func(p *github.RawPR) { fakegh.SetChecks(p, "SUCCESS") })
	eventually(t, 5*time.Second, "the valid PR's change", func() bool {
		return strings.Contains(ev.out.String(), `"changes":["checks PENDING→SUCCESS"]`)
	})
	if !ev.running() || !strings.Contains(ev.errb.String(), `skipping`) {
		t.Fatalf("running %v, stderr %q", ev.running(), ev.errb.String())
	}

	only := e.start("events", "--pr", "o/r#98")
	if r := only.wait(t, 5*time.Second); r.code != 3 {
		t.Fatalf("events whose only PR is missing: exit %d, want 3", r.code)
	}
}

func TestSecondaryLimitHonoursRetryAfter(t *testing.T) {
	e := newEnv(t)
	e.fake.AddPR("o", "r", 1)
	e.start("wait", "o/r#1", "--for", "merged")
	e.waitWatched(1, 1)
	e.fake.Inject(fakegh.Response{Status: 403, Headers: map[string]string{"Retry-After": "2"},
		Body: `{"message":"You have exceeded a secondary rate limit."}`})
	before := len(e.fake.Requests())
	eventually(t, 5*time.Second, "request after retry-after", func() bool { return len(e.fake.Requests()) >= before+2 })
	reqs := e.fake.Requests()[before:]
	if reqs[0].Status != 403 {
		t.Fatalf("first request status %d", reqs[0].Status)
	}
	if gap := reqs[1].At.Sub(reqs[0].At); gap < 2*time.Second {
		t.Fatalf("retried after %s, want >= 2s", gap)
	}
	if !strings.Contains(e.logText(), `reason="secondary limit: retry-after"`) {
		t.Fatal("expected retry-after back-off in the log")
	}
}

func TestPrimaryExhaustionWaitsForReset(t *testing.T) {
	e := newEnv(t)
	e.fake.AddPR("o", "r", 1)
	e.start("wait", "o/r#1", "--for", "merged")
	e.waitWatched(1, 1)
	reset := time.Now().Add(3 * time.Second).Unix()
	e.fake.Inject(fakegh.Response{Status: 403, Headers: map[string]string{
		"X-Ratelimit-Limit": "5000", "X-Ratelimit-Remaining": "0", "X-Ratelimit-Reset": fmt.Sprint(reset)},
		Body: `{"message":"API rate limit exceeded"}`})
	before := len(e.fake.Requests())
	eventually(t, 8*time.Second, "request after reset", func() bool { return len(e.fake.Requests()) >= before+2 })
	reqs := e.fake.Requests()[before:]
	if reqs[1].At.Unix() < reset {
		t.Fatalf("retried at %s, before reset %s", reqs[1].At, time.Unix(reset, 0))
	}
	r := e.run("list")
	if !strings.Contains(r.stdout, "back-off: none") {
		t.Fatalf("back-off should have cleared:\n%s", r.stdout)
	}
}

func TestExponentialBackoff(t *testing.T) {
	e := newEnv(t)
	e.fake.AddPR("o", "r", 1)
	e.start("wait", "o/r#1", "--for", "merged")
	e.waitWatched(1, 1)
	limited := fakegh.Response{Status: 403, Body: `{"message":"You have exceeded a secondary rate limit."}`}
	e.fake.Inject(limited, limited, limited)
	before := len(e.fake.Requests())
	eventually(t, 10*time.Second, "four requests", func() bool { return len(e.fake.Requests()) >= before+4 })
	reqs := e.fake.Requests()[before:]
	// PRWATCH_BACKOFF_MIN is 300ms in tests: expect >= 300ms, 600ms, 1.2s.
	want := []time.Duration{300 * time.Millisecond, 600 * time.Millisecond, 1200 * time.Millisecond}
	for i, w := range want {
		if gap := reqs[i+1].At.Sub(reqs[i].At); gap < w {
			t.Fatalf("gap %d is %s, want >= %s", i, gap, w)
		}
	}
	if r := e.run("list"); !strings.Contains(r.stdout, "OPEN") {
		t.Fatalf("daemon should recover:\n%s", r.stdout)
	}
}

func TestStatusHonoursPersistedBackoff(t *testing.T) {
	e := newEnv(t)
	e.fake.AddPR("o", "r", 1)
	until := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if err := os.WriteFile(filepath.Join(e.dir, "rate.json"), []byte(`{"backoffUntil":"`+until+`","remaining":-1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r := e.run("status", "o/r#1")
	if r.code == 0 || !strings.Contains(r.stderr, "back-off") {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if n := len(e.fake.Requests()); n != 0 {
		t.Fatalf("made %d requests during back-off", n)
	}
}

func TestDaemonStatusAndStop(t *testing.T) {
	e := newEnv(t)
	if r := e.run("daemon", "status"); r.code != 1 {
		t.Fatalf("status without daemon: exit %d", r.code)
	}
	e.fake.AddPR("o", "r", 1)
	p := e.start("wait", "o/r#1", "--for", "merged")
	e.waitWatched(1, 1)
	r := e.run("daemon", "status")
	if r.code != 0 || !strings.Contains(r.stdout, "1 waiter, watching 1 PR") {
		t.Fatalf("daemon status: %d %q", r.code, r.stdout)
	}
	if r := e.run("daemon", "stop"); r.code != 0 {
		t.Fatalf("stop: %d", r.code)
	}
	res := p.wait(t, 3*time.Second)
	if res.code != 1 || !strings.Contains(res.stderr, "daemon was stopped") {
		t.Fatalf("waiter after stop: %d %q", res.code, res.stderr)
	}
}

func TestZeroBudgetStopsFurtherRequestsInRound(t *testing.T) {
	e := newEnv(t)
	k := e.fake.AddPR("o", "r", 1)
	e.fake.Update(k, func(p *github.RawPR) {
		p.ReviewThreads.TotalCount = 1
		p.ReviewThreads.Nodes = []github.RawThread{{ID: "T1", Path: "a.go"}}
	})
	e.fake.SetThreadHead("T1", "alice", "hm")
	// One point left: the resolve request spends it and reports zero.
	e.fake.SetBudget(5000, 1)
	r := e.run("status", "o/r#1", "--json")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if n := len(e.fake.Requests()); n != 1 {
		t.Fatalf("%d requests; the thread request must not follow an exhausting response", n)
	}
	if r := e.run("status", "o/r#1"); r.code == 0 || !strings.Contains(r.stderr, "back-off") {
		t.Fatalf("second status should honour the back-off: %d %s", r.code, r.stderr)
	}
}

func TestNewInterestRespectsBudget(t *testing.T) {
	e := newEnv(t)
	for i := 1; i <= 3; i++ {
		e.fake.AddPR("o", "r", i)
	}
	// 100 points left in the hour at 20%: 20 rounds, one per 3 minutes.
	e.fake.SetBudget(5000, 101)
	p1 := e.start("wait", "o/r#1", "--for", "merged")
	e.waitWatched(1, 1)
	e.start("wait", "o/r#2", "--for", "merged")
	e.start("wait", "o/r#3", "--for", "merged")
	time.Sleep(1500 * time.Millisecond)
	if n := len(e.fake.Requests()); n != 1 {
		t.Fatalf("%d requests; new interest must wait for the budget gap", n)
	}
	l := e.run("list")
	if !strings.Contains(l.stdout, "(not fetched yet)") {
		t.Fatalf("list:\n%s", l.stdout)
	}
	_ = p1.cmd.Process.Kill()
}

func TestStopAbortsInFlightRequest(t *testing.T) {
	e := newEnv(t)
	e.fake.AddPR("o", "r", 1)
	e.fake.SetDelay(20 * time.Second)
	p := e.start("wait", "o/r#1", "--for", "merged")
	eventually(t, 5*time.Second, "daemon to start", e.daemonRunning)
	time.Sleep(300 * time.Millisecond) // let the resolve request start
	start := time.Now()
	if r := e.run("daemon", "stop"); r.code != 0 {
		t.Fatalf("stop: %d", r.code)
	}
	eventually(t, 3*time.Second, "daemon to exit", func() bool { return strings.Contains(e.logText(), "daemon exiting") })
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("shutdown took %s with a request in flight", d)
	}
	p.wait(t, 3*time.Second)
}

func TestTimeoutCoversConnecting(t *testing.T) {
	e := newEnv(t)
	e.fake.AddPR("o", "r", 1)
	e.fake.SetDelay(20 * time.Second)
	// A socket that accepts but never greets, and holds the lock so no
	// real daemon can take over.
	sock := filepath.Join(e.dir, "prwatch.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	lock, err := os.OpenFile(filepath.Join(e.dir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	start := time.Now()
	r := e.run("wait", "o/r#1", "--timeout", "1s")
	if r.code != 124 {
		t.Fatalf("exit %d, want 124: %s", r.code, r.stderr)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("timeout took %s", d)
	}
}

func TestStatusManyPRsViaDaemon(t *testing.T) {
	e := newEnv(t)
	var args []string
	for i := 1; i <= 300; i++ {
		e.fake.AddPR("o", "r", i)
		args = append(args, "--pr", fmt.Sprintf("o/r#%d", i))
	}
	ev := e.start(append([]string{"events", "--json"}, args...)...)
	eventually(t, 15*time.Second, "300 initial events", func() bool { return strings.Count(ev.out.String(), "\n") >= 300 })
	var prs []string
	for i := 300; i >= 1; i-- {
		prs = append(prs, fmt.Sprintf("o/r#%d", i))
	}
	r := e.run(append([]string{"status", "--json"}, prs...)...)
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	var snaps []snapshot.Snapshot
	if err := json.Unmarshal([]byte(r.stdout), &snaps); err != nil || len(snaps) != 300 {
		t.Fatalf("got %d snapshots: %v", len(snaps), err)
	}
	if snaps[0].Number != 300 || snaps[299].Number != 1 {
		t.Fatal("status output not in argument order")
	}
}

func TestStatusMixedCachedAndNew(t *testing.T) {
	e := newEnv(t)
	e.fake.AddPR("o", "r", 1)
	e.fake.AddPR("o", "r", 2)
	p := e.start("wait", "o/r#1", "--for", "merged")
	e.waitWatched(1, 1)
	r := e.run("status", "O/R#1", "o/r#2", "--json")
	var snaps []snapshot.Snapshot
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &snaps) != nil || len(snaps) != 2 {
		t.Fatalf("exit %d: %q %s", r.code, r.stdout, r.stderr)
	}
	_ = p.cmd.Process.Kill()
}

func TestRestartedDaemonHonoursLowStoredBudget(t *testing.T) {
	e := newEnv(t)
	e.fake.AddPR("o", "r", 1)
	// An earlier daemon stopped with 4 points left: 20% of that does not pay
	// for a round, so the new daemon must wait for the reset.
	now := time.Now()
	reset := now.Add(3 * time.Second)
	state := fmt.Sprintf(`{"limit":5000,"remaining":4,"used":4996,"resetAt":%q,"roundCost":1,"updatedAt":%q}`,
		reset.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano))
	if err := os.WriteFile(filepath.Join(e.dir, "rate.json"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	p := e.start("wait", "o/r#1", "--for", "merged")
	time.Sleep(1500 * time.Millisecond)
	if n := len(e.fake.Requests()); n != 0 {
		t.Fatalf("%d requests before the reset with 4 stored points", n)
	}
	if r := e.run("status", "o/r#1"); r.code == 0 || !strings.Contains(r.stderr, "backed off") {
		t.Fatalf("status during the reserve: exit %d: %s", r.code, r.stderr)
	}
	e.waitWatched(1, 1)
	reqs := e.fake.Requests()
	if reqs[0].At.Before(reset) {
		t.Fatalf("first request at %s, before the reset at %s", reqs[0].At, reset)
	}
	if !strings.Contains(e.logText(), "stored budget is below the reserve") {
		t.Fatal("expected the stored reserve in the log")
	}
	_ = p.cmd.Process.Kill()
}

func TestDirectStatusExitCodesForPRErrors(t *testing.T) {
	e := newEnv(t)
	e.fake.AddPR("o", "r", 1)
	nullPR := func(typ, msg string) fakegh.Response {
		return fakegh.Response{Status: 200, Body: fmt.Sprintf(`{"data":{"r0":{"q0":null},"rateLimit":{"cost":1,"limit":5000,"remaining":4000,"used":1000,"resetAt":%q}},`+
			`"errors":[{"type":%q,"path":["r0","q0"],"message":%q}]}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339), typ, msg)}
	}
	cases := []struct {
		name string
		resp fakegh.Response
		want int
	}{
		{"forbidden", nullPR("FORBIDDEN", "Resource not accessible by integration"), 2},
		{"SAML", nullPR("FORBIDDEN", "Resource protected by organization SAML enforcement"), 2},
		{"transient", nullPR("INTERNAL", "Something went wrong while executing your query"), 1},
		{"not found", nullPR("NOT_FOUND", "Could not resolve to a PullRequest"), 3},
	}
	for _, c := range cases {
		e.fake.Inject(c.resp)
		if r := e.run("status", "o/r#1"); r.code != c.want {
			t.Errorf("%s: exit %d, want %d: %s", c.name, r.code, c.want, r.stderr)
		}
	}
	if e.daemonRunning() {
		t.Fatal("status started a daemon")
	}
}

func TestNestedErrorNeverSatisfiesWaitersAndKeepsSnapshot(t *testing.T) {
	e := newEnv(t)
	k := e.fake.AddPR("o", "r", 1)
	e.fake.Update(k, func(p *github.RawPR) { fakegh.SetChecks(p, "PENDING") })

	checks := e.start("wait", "o/r#1", "--for", "checks", "--json")
	mergeable := e.start("wait", "o/r#1", "--for", "mergeable", "--json")
	ev := e.start("events", "--pr", "o/r#1", "--json")
	e.waitWatched(1, 3)
	eventsBefore := ev.out.String()
	good := e.list().PRs[0].Snapshot

	// Two polls return the PR with commits nulled by an error. Taken at
	// face value its checks would be NONE: settled, green and mergeable.
	before := len(e.fake.Requests())
	e.fake.FailCommits(k, 2)
	eventually(t, 5*time.Second, "two polls with a nested error", func() bool { return len(e.fake.Requests()) >= before+3 })
	if !checks.running() || !mergeable.running() {
		t.Fatalf("a waiter returned on partial data: checks %q, mergeable %q", checks.out.String(), mergeable.out.String())
	}
	if got := ev.out.String(); got != eventsBefore {
		t.Fatalf("change event built from partial data:\n%s", strings.TrimPrefix(got, eventsBefore))
	}
	kept := e.list().PRs[0].Snapshot
	if kept == nil || kept.Incomplete || kept.Checks.State != "PENDING" || kept.Token != good.Token {
		t.Fatalf("previous good snapshot not kept: %+v", kept)
	}
	if !strings.Contains(e.logText(), "PR returned incomplete; keeping the previous snapshot") {
		t.Fatal("expected the incomplete response in the log")
	}

	// Once GitHub answers fully, the real state is reported.
	e.fake.Update(k, func(p *github.RawPR) { fakegh.SetChecks(p, "SUCCESS") })
	if r := checks.wait(t, 5*time.Second); r.code != 0 || parseSnap(t, r.stdout).Checks.State != "SUCCESS" {
		t.Fatalf("checks waiter: exit %d %q", r.code, r.stdout)
	}
	if r := mergeable.wait(t, 5*time.Second); r.code != 0 || parseSnap(t, r.stdout).Incomplete {
		t.Fatalf("mergeable waiter: exit %d %q", r.code, r.stdout)
	}
}

func TestDirectStatusReportsIncomplete(t *testing.T) {
	e := newEnv(t)
	k := e.fake.AddPR("o", "r", 1)
	e.fake.Update(k, func(p *github.RawPR) { fakegh.SetChecks(p, "PENDING") })
	e.fake.FailCommits(k, 1)
	r := e.run("status", "o/r#1", "--json")
	if r.code != 1 || !strings.Contains(r.stderr, "incomplete") {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	var snaps []snapshot.Snapshot
	if err := json.Unmarshal([]byte(r.stdout), &snaps); err != nil || len(snaps) != 1 {
		t.Fatalf("status json %q: %v", r.stdout, err)
	}
	s := snaps[0]
	if !s.Incomplete || !strings.Contains(s.IncompleteReason, "commits") || snapshot.Ready(&s) {
		t.Fatalf("snapshot: incomplete %v reason %q", s.Incomplete, s.IncompleteReason)
	}
}

// waitPolls waits until the fake has served n more poll requests.
func (e *env) waitPolls(n int) {
	e.t.Helper()
	count := func() int {
		c := 0
		for _, r := range e.fake.Requests() {
			if r.Kind == "poll" {
				c++
			}
		}
		return c
	}
	target := count() + n
	eventually(e.t, 10*time.Second, fmt.Sprintf("%d more polls", n), func() bool { return count() >= target })
}

func TestEditsWakeWaiters(t *testing.T) {
	e := newEnv(t)
	k := e.fake.AddPR("o", "r", 1)
	created := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	e.fake.Update(k, func(p *github.RawPR) {
		p.Comments.TotalCount = 1
		p.Comments.Nodes = []github.RawComment{{Author: &github.Actor{Login: "bob"}, CreatedAt: created, BodyText: "LGTM"}}
		p.Reviews.TotalCount = 1
		p.LatestReviews.Nodes = []github.RawReview{{Author: &github.Actor{Login: "alice"}, State: "COMMENTED", SubmittedAt: &created}}
		line := 3
		p.ReviewThreads.TotalCount = 1
		p.ReviewThreads.Nodes = []github.RawThread{{ID: "T1", Path: "main.go", Line: &line}}
		p.RecentThreads.Nodes = []github.RawRecentThread{{ID: "T1"}}
		p.RecentThreads.Nodes[0].Comments.Nodes = []github.RawEdit{{}}
	})
	e.fake.SetThreadHead("T1", "alice", "Rename this")
	ev := e.start("events", "--pr", "o/r#1", "--json")
	change := e.start("wait", "o/r#1", "--for", "change", "--json")
	review := e.start("wait", "o/r#1", "--for", "review", "--json")
	e.waitWatched(1, 3)

	// Polls that find nothing new wake nobody.
	e.waitPolls(3)
	if !change.running() || !review.running() {
		t.Fatalf("a no-op poll woke a waiter: change %q review %q", change.out.String(), review.out.String())
	}
	if n := strings.Count(ev.out.String(), "\n"); n != 1 {
		t.Fatalf("no-op polls emitted events:\n%s", ev.out.String())
	}

	// Editing an issue comment wakes --for change but not --for review.
	edited := time.Now().UTC()
	e.fake.Update(k, func(p *github.RawPR) {
		p.Comments.Nodes[0].BodyText = "LGTM, with one nit"
		p.Comments.Nodes[0].LastEditedAt = &edited
	})
	r := change.wait(t, 5*time.Second)
	if r.code != 0 {
		t.Fatalf("--for change: exit %d: %s", r.code, r.stderr)
	}
	if s := parseSnap(t, r.stdout); s.Comments.Total != 1 || s.Comments.Recent[0].EditedAt == nil {
		t.Fatalf("snapshot comments: %+v", s.Comments)
	}
	eventually(t, 5*time.Second, "comment edited event", func() bool {
		return strings.Contains(ev.out.String(), `"changes":["comment edited"]`)
	})
	e.waitPolls(2)
	if !review.running() {
		t.Fatalf("--for review woke on a comment edit: %s", review.out.String())
	}

	// Editing a review body wakes --for review.
	e.fake.Update(k, func(p *github.RawPR) {
		at := time.Now().UTC()
		p.LatestReviews.Nodes[0].LastEditedAt = &at
	})
	if r := review.wait(t, 5*time.Second); r.code != 0 {
		t.Fatalf("--for review on review edit: exit %d: %s", r.code, r.stderr)
	}
	eventually(t, 5*time.Second, "review edited event", func() bool {
		return strings.Contains(ev.out.String(), `"changes":["review edited"]`)
	})

	// Editing a review-thread comment wakes --for review, and the thread's
	// cached excerpt is refetched.
	review = e.start("wait", "o/r#1", "--for", "review", "--json")
	e.waitWatched(1, 2)
	e.fake.SetThreadHead("T1", "alice", "Rename this, please")
	e.fake.Update(k, func(p *github.RawPR) {
		at := time.Now().UTC()
		p.RecentThreads.Nodes[0].Comments.Nodes[0].LastEditedAt = &at
	})
	r = review.wait(t, 5*time.Second)
	if r.code != 0 {
		t.Fatalf("--for review on thread edit: exit %d: %s", r.code, r.stderr)
	}
	s := parseSnap(t, r.stdout)
	if len(s.Threads.Edits) != 1 || s.Threads.Items[0].Excerpt != "Rename this, please" {
		t.Fatalf("threads after edit: %+v", s.Threads)
	}
	eventually(t, 5*time.Second, "second review edited event", func() bool {
		return strings.Count(ev.out.String(), `"changes":["review edited"]`) == 2
	})

	// Editing the description wakes --for change.
	change = e.start("wait", "o/r#1", "--for", "change", "--json")
	e.waitWatched(1, 2)
	e.fake.Update(k, func(p *github.RawPR) {
		at := time.Now().UTC()
		p.LastEditedAt = &at
	})
	if r := change.wait(t, 5*time.Second); r.code != 0 || parseSnap(t, r.stdout).BodyEditedAt == nil {
		t.Fatalf("--for change on description edit: exit %d: %s", r.code, r.stderr)
	}
	eventually(t, 5*time.Second, "description edited event", func() bool {
		return strings.Contains(ev.out.String(), `"changes":["description edited"]`)
	})
}

// A conflict caused by the base branch moving is seen even though nothing
// on the PR changes: GitHub first reports mergeability as UNKNOWN while it
// recomputes, then CONFLICTING.
func TestBaseMoveConflictDetected(t *testing.T) {
	e := newEnv(t, "PRWATCH_POLL_SLOW=2s", "PRWATCH_POLL_FAST=200ms")
	k := e.fake.AddPR("o", "r", 1)
	st := e.run("status", "o/r#1", "--json")
	var snaps []snapshot.Snapshot
	if err := json.Unmarshal([]byte(st.stdout), &snaps); err != nil || len(snaps) != 1 {
		t.Fatalf("status %q: %v", st.stdout, err)
	}
	tok := snaps[0].Token
	head := snaps[0].HeadRefOid

	ev := e.start("events", "--pr", "o/r#1", "--json")
	change := e.start("wait", "o/r#1", "--for", "change", "--json")
	mergeable := e.start("wait", "o/r#1", "--for", "mergeable", "--since", tok, "--json")
	e.waitWatched(1, 3)
	e.waitPolls(1) // settle into the slow cadence

	e.fake.OnPoll(k,
		func(p *github.RawPR) { p.Mergeable, p.MergeStateStatus = "UNKNOWN", "UNKNOWN" },
		func(p *github.RawPR) { p.Mergeable, p.MergeStateStatus = "CONFLICTING", "DIRTY" },
	)
	r := change.wait(t, 5*time.Second)
	if r.code != 0 {
		t.Fatalf("--for change: exit %d: %s", r.code, r.stderr)
	}
	first := parseSnap(t, r.stdout)
	if first.Mergeable != "UNKNOWN" || first.HeadRefOid != head {
		t.Fatalf("first wake: mergeable %s head %s", first.Mergeable, first.HeadRefOid)
	}

	// The agent waits again from the token it was given and sees the conflict.
	r = e.run("wait", "o/r#1", "--for", "change", "--since", first.Token, "--json", "--timeout", "5s")
	if r.code != 0 {
		t.Fatalf("--since wait: exit %d: %s", r.code, r.stderr)
	}
	s := parseSnap(t, r.stdout)
	if s.Mergeable != "CONFLICTING" || !s.NeedsAction || !slicesContain(s.Reasons, snapshot.ReasonConflict) || s.HeadRefOid != head {
		t.Fatalf("conflict snapshot: mergeable %s needsAction %v reasons %v head %s", s.Mergeable, s.NeedsAction, s.Reasons, s.HeadRefOid)
	}

	// UNKNOWN switches to the fast interval: the CONFLICTING poll follows
	// the UNKNOWN one far sooner than the 2s slow interval.
	steps := e.fake.StepTimes(k)
	if len(steps) != 2 {
		t.Fatalf("steps served: %d", len(steps))
	}
	var before time.Time
	for _, req := range e.fake.Requests() {
		if (req.Kind == "poll" || req.Kind == "threads") && req.At.Before(steps[0]) {
			before = req.At
		}
	}
	slowGap, fastGap := steps[0].Sub(before), steps[1].Sub(steps[0])
	t.Logf("gap before UNKNOWN %s, UNKNOWN to CONFLICTING %s", slowGap, fastGap)
	if slowGap < 1500*time.Millisecond || fastGap > time.Second {
		t.Fatalf("interval not shortened: before %s, after UNKNOWN %s", slowGap, fastGap)
	}

	eventually(t, 5*time.Second, "mergeable events", func() bool {
		out := ev.out.String()
		return strings.Contains(out, "mergeable UNKNOWN/UNKNOWN") && strings.Contains(out, "mergeable CONFLICTING/DIRTY")
	})
	if !mergeable.running() {
		t.Fatalf("--for mergeable fired on a conflict: %s", mergeable.out.String())
	}
}

func slicesContain(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// When the base branch requires PRs to be up to date, a base move makes the
// PR BEHIND without any change to the PR itself.
func TestBaseMoveBehindWakesChange(t *testing.T) {
	e := newEnv(t)
	k := e.fake.AddPR("o", "r", 1)
	change := e.start("wait", "o/r#1", "--for", "change", "--json")
	e.waitWatched(1, 1)
	e.waitPolls(1)
	if !change.running() {
		t.Fatalf("woke before the base moved: %s", change.out.String())
	}
	e.fake.OnPoll(k, func(p *github.RawPR) { p.MergeStateStatus = "BEHIND" })
	r := change.wait(t, 5*time.Second)
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	s := parseSnap(t, r.stdout)
	if s.MergeStateStatus != "BEHIND" || s.Mergeable != "MERGEABLE" || slicesContain(s.Reasons, snapshot.ReasonReadyAutoMergeOff) {
		t.Fatalf("snapshot: %s/%s reasons %v", s.Mergeable, s.MergeStateStatus, s.Reasons)
	}
}
