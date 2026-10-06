package cli_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/tvdavies/prwatch/internal/fakegh"
	"github.com/tvdavies/prwatch/internal/github"
	"github.com/tvdavies/prwatch/internal/protocol"
)

// daemon returns the running daemon's pid and version from list --json.
func (e *env) daemon() (int, string) {
	e.t.Helper()
	l := e.list()
	if l.Daemon == nil {
		return 0, ""
	}
	return l.Daemon.PID, l.Daemon.Version
}

// waitNewDaemon waits until a daemon other than oldPID serves n PRs, all
// fetched, with `subs` waiters and streams in total, and returns its pid.
func (e *env) waitNewDaemon(oldPID, n, subs int) int {
	e.t.Helper()
	var pid int
	eventually(e.t, 15*time.Second, fmt.Sprintf("a new daemon serving %d PRs and %d subscribers", n, subs), func() bool {
		l := e.list()
		if l.Daemon == nil || l.Daemon.PID == oldPID || len(l.PRs) != n {
			return false
		}
		total := 0
		for _, w := range l.PRs {
			if w.Snapshot == nil {
				return false
			}
			total += w.Waiters + w.Streams
		}
		pid = l.Daemon.PID
		return total == subs
	})
	return pid
}

type event struct {
	PR      string   `json:"pr"`
	Changes []string `json:"changes"`
}

func events(t *testing.T, out string) []event {
	t.Helper()
	var evs []event
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var ev event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("event %q: %v", line, err)
		}
		evs = append(evs, ev)
	}
	return evs
}

// byPR renders events per PR, for comparison: "o/r#1: initial | title".
func byPR(evs []event) map[string]string {
	out := map[string]string{}
	for _, ev := range evs {
		c := strings.Join(ev.Changes, "; ")
		if out[ev.PR] != "" {
			c = out[ev.PR] + " | " + c
		}
		out[ev.PR] = c
	}
	return out
}

func TestRestartHandsOverWaitersAndStreams(t *testing.T) {
	// A slow cadence, so the old daemon cannot see the changes made just
	// before the restart: they happen in the handover gap.
	e := newEnv(t, "PRWATCH_POLL_FAST=5s", "PRWATCH_POLL_SLOW=5s")
	k1 := e.fake.AddPR("o", "r", 1)
	k2 := e.fake.AddPR("o", "r", 2)
	k3 := e.fake.AddPR("o", "r", 3)
	e.fake.Update(k2, func(p *github.RawPR) { fakegh.SetChecks(p, "PENDING") })
	st := e.run("status", "o/r#3", "--json")
	var snaps []struct{ Token string }
	if err := json.Unmarshal([]byte(st.stdout), &snaps); err != nil || len(snaps) != 1 {
		t.Fatalf("status %q: %v", st.stdout, err)
	}

	change := e.start("wait", "o/r#1", "--for", "change", "--json", "--timeout", "60s")
	checks := e.start("wait", "o/r#2", "--for", "checks", "--json", "--timeout", "60s")
	merged := e.start("wait", "o/r#3", "--for", "merged", "--since", snaps[0].Token, "--json", "--timeout", "60s")
	ev := e.start("events", "--pr", "o/r#1", "--pr", "o/r#2", "--pr", "o/r#3", "--json")
	e.waitWatched(3, 6)
	all := e.start("events", "--json")
	eventually(t, 5*time.Second, "initial events", func() bool {
		return len(events(t, ev.out.String())) == 3 && len(events(t, all.out.String())) == 3
	})
	oldPID, _ := e.daemon()

	// A 0.1.1 client speaks the same protocol: it must be told to
	// reconnect, not that the daemon was stopped.
	raw, err := net.Dial("unix", filepath.Join(e.dir, "prwatch.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	rawLines := bufio.NewReader(raw)
	if _, err := rawLines.ReadBytes('\n'); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Write([]byte(`{"protocol":1,"op":"wait","prs":["o/r#3"],"for":"merged"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	e.waitWatched(3, 7)

	before := len(e.fake.Requests())
	e.fake.Update(k1, func(p *github.RawPR) { p.Title = "Renamed in the gap" })
	e.fake.Update(k2, func(p *github.RawPR) { fakegh.SetChecks(p, "SUCCESS") })
	r := e.run("daemon", "restart")
	// The changes really were made in the gap: every request since was sent
	// after the old daemon began handing over, so the old daemon never saw
	// them and the waiters can only be woken by the new one.
	handover := logTime(t, e.logText(), `msg="restart requested`)
	for _, req := range e.fake.Requests()[before:] {
		if req.At.Before(handover) {
			t.Fatalf("the old daemon polled at %s, before the handover at %s; the test did not exercise the gap", req.At, handover)
		}
	}
	if r.code != 0 {
		t.Fatalf("restart: exit %d: %s %s", r.code, r.stdout, r.stderr)
	}
	m := regexp.MustCompile(`daemon restarted: pid (\d+) \(version test\) -> pid (\d+) \(version test\)`).FindStringSubmatch(r.stdout)
	if m == nil || m[1] != fmt.Sprint(oldPID) || m[2] == m[1] {
		t.Fatalf("restart output: %q", r.stdout)
	}
	if !strings.Contains(r.stdout, "6 waiters handed over") {
		t.Fatalf("restart output should count the handed-over waiters: %q", r.stdout)
	}

	// The raw (0.1.1-style) client was told "restarting".
	_ = raw.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		line, err := rawLines.ReadBytes('\n')
		if err != nil {
			t.Fatalf("raw client: %v", err)
		}
		var msg protocol.Message
		_ = json.Unmarshal(line, &msg)
		if msg.Type == protocol.TypeShutdown {
			if msg.Code != protocol.ShutdownRestarting {
				t.Fatalf("shutdown code %q, want %q", msg.Code, protocol.ShutdownRestarting)
			}
			break
		}
	}
	raw.Close()

	// The changes made in the gap wake the right waiters, from the new daemon.
	if res := change.wait(t, 10*time.Second); res.code != 0 || parseSnap(t, res.stdout).Title != "Renamed in the gap" {
		t.Fatalf("--for change: exit %d: %s %s", res.code, res.stdout, res.stderr)
	}
	if res := checks.wait(t, 10*time.Second); res.code != 0 || parseSnap(t, res.stdout).Checks.State != "SUCCESS" {
		t.Fatalf("--for checks: exit %d: %s %s", res.code, res.stdout, res.stderr)
	}
	newPID := e.waitNewDaemon(oldPID, 3, 4)
	if !merged.running() || !ev.running() || !all.running() {
		t.Fatalf("a client exited across the restart: merged %q, events %q, all %q", merged.errb.String(), ev.errb.String(), all.errb.String())
	}
	if n := strings.Count(e.logText(), "daemon started"); n != 2 {
		t.Fatalf("%d daemons started, want 2", n)
	}

	// Each stream reports each gap change once, as a change against what it
	// last printed, and nothing for the PR that did not change.
	want := map[string]string{
		"o/r#1": "initial | title",
		"o/r#2": "initial | checks PENDING→SUCCESS",
		"o/r#3": "initial",
	}
	for name, p := range map[string]*proc{"events --pr": ev, "events (all)": all} {
		eventually(t, 5*time.Second, name+" to report the gap changes", func() bool {
			return len(events(t, p.out.String())) >= 5
		})
		if got := byPR(events(t, p.out.String())); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("%s:\n got %v\nwant %v", name, got, want)
		}
	}

	// The handed-over waiter still works, and its stream sees it once.
	e.fake.Update(k3, merge)
	if res := merged.wait(t, 10*time.Second); res.code != 0 || !parseSnap(t, res.stdout).Merged {
		t.Fatalf("--for merged: exit %d: %s %s", res.code, res.stdout, res.stderr)
	}
	eventually(t, 5*time.Second, "merge event", func() bool { return len(events(t, ev.out.String())) == 6 })
	if got := byPR(events(t, ev.out.String()))["o/r#3"]; got != "initial | state OPEN→MERGED" {
		t.Fatalf("o/r#3 events: %q", got)
	}
	if pid, _ := e.daemon(); pid != newPID {
		t.Fatalf("daemon pid %d, want %d", pid, newPID)
	}
}

// logTime returns the time of the first daemon log line containing what.
func logTime(t *testing.T, log, what string) time.Time {
	t.Helper()
	for _, line := range strings.Split(log, "\n") {
		if !strings.Contains(line, what) {
			continue
		}
		f := strings.Fields(line)
		if len(f) > 0 && strings.HasPrefix(f[0], "time=") {
			ts, err := time.Parse(time.RFC3339Nano, strings.TrimPrefix(f[0], "time="))
			if err != nil {
				t.Fatal(err)
			}
			return ts
		}
	}
	t.Fatalf("no log line with %q", what)
	return time.Time{}
}

func TestSIGHUPHandsOverAndSIGTERMStops(t *testing.T) {
	e := newEnv(t)
	e.fake.AddPR("o", "r", 1)
	timed := e.start("wait", "o/r#1", "--for", "merged", "--timeout", "6s")
	timedStart := time.Now()
	w := e.start("wait", "o/r#1", "--for", "merged")
	ev := e.start("events", "--pr", "o/r#1", "--json")
	e.waitWatched(1, 3)
	eventually(t, 5*time.Second, "initial event", func() bool { return len(events(t, ev.out.String())) == 1 })
	oldPID, _ := e.daemon()

	if err := syscall.Kill(oldPID, syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	newPID := e.waitNewDaemon(oldPID, 1, 3)

	// The handover does not reset a waiter's deadline: it still times out
	// 6s after it started. (Under -race each process takes an extra second
	// to exit.)
	r := timed.wait(t, 12*time.Second)
	took := time.Since(timedStart)
	if r.code != 124 || took < 6*time.Second || took > 7800*time.Millisecond {
		t.Fatalf("timed waiter: exit %d after %s, want 124 after about 6s: %s", r.code, took, r.stderr)
	}
	if !w.running() || !ev.running() {
		t.Fatalf("a client exited on SIGHUP: wait %q, events %q", w.errb.String(), ev.errb.String())
	}
	e.waitPolls(2)
	if n := len(events(t, ev.out.String())); n != 1 {
		t.Fatalf("SIGHUP produced duplicate events:\n%s", ev.out.String())
	}
	if !strings.Contains(e.logText(), `msg="signal received" signal=hangup restart=true`) {
		t.Fatal("expected the SIGHUP restart in the log")
	}

	if err := syscall.Kill(newPID, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	for _, p := range []*proc{w, ev} {
		if r := p.wait(t, 5*time.Second); r.code != 1 || !strings.Contains(r.stderr, "daemon was stopped") {
			t.Fatalf("after SIGTERM: exit %d: %q", r.code, r.stderr)
		}
	}
	if e.daemonRunning() {
		t.Fatal("SIGTERM should not be followed by a new daemon")
	}
}

func TestRestartWithoutDaemon(t *testing.T) {
	e := newEnv(t)
	r := e.run("daemon", "restart")
	if r.code != 0 || !strings.Contains(r.stdout, "daemon not running; nothing to restart") {
		t.Fatalf("exit %d: %q %q", r.code, r.stdout, r.stderr)
	}
	if e.daemonRunning() {
		t.Fatal("restart started a daemon when none was running")
	}
}

func TestRestartIdleDaemonStartsNewOne(t *testing.T) {
	e := newEnv(t, "PRWATCH_IDLE_GRACE=3s")
	e.fake.AddPR("o", "r", 1)
	if r := e.run("wait", "o/r#1", "--timeout", "200ms"); r.code != 124 {
		t.Fatalf("exit %d", r.code)
	}
	oldPID, _ := e.daemon()
	r := e.run("daemon", "restart")
	if r.code != 0 || !strings.Contains(r.stdout, fmt.Sprintf("pid %d (version test) -> pid ", oldPID)) {
		t.Fatalf("exit %d: %q %q", r.code, r.stdout, r.stderr)
	}
	if pid, v := e.daemon(); pid == oldPID || pid == 0 || v != "test" {
		t.Fatalf("daemon after restart: pid %d version %q", pid, v)
	}
	// Nothing is watched, so the usual idle rule applies.
	eventually(t, 6*time.Second, "the new daemon to exit when idle", func() bool { return !e.daemonRunning() })
}

func TestVersionSkewWarning(t *testing.T) {
	e := newEnv(t, "PRWATCH_DAEMON_VERSION=0.0.9")
	e.fake.AddPR("o", "r", 1)
	w := e.start("wait", "o/r#1", "--for", "merged")
	e.waitWatched(1, 1)
	const warning = "the daemon is version 0.0.9 but this prwatch is test; run `prwatch daemon restart`"
	for _, args := range [][]string{{"list"}, {"list", "--json"}, {"rate"}, {"rate", "--json"}, {"daemon", "status"}} {
		r := e.run(args...)
		if r.code != 0 || !strings.Contains(r.stderr, warning) {
			t.Errorf("%v: exit %d, stderr %q", args, r.code, r.stderr)
		}
	}
	l := e.list()
	if l.DaemonVersion == nil || *l.DaemonVersion != "0.0.9" {
		t.Fatalf("list --json daemonVersion: %v", l.DaemonVersion)
	}
	_ = w.cmd.Process.Kill()
}

func TestNoVersionSkewWarning(t *testing.T) {
	e := newEnv(t)
	e.fake.AddPR("o", "r", 1)
	e.start("wait", "o/r#1", "--for", "merged")
	e.waitWatched(1, 1)
	for _, args := range [][]string{{"list"}, {"rate"}, {"daemon", "status"}} {
		if r := e.run(args...); r.code != 0 || r.stderr != "" {
			t.Errorf("%v: exit %d, stderr %q", args, r.code, r.stderr)
		}
	}
}

// fakeOldDaemon serves the socket the way a 0.1.1 daemon does for the ops
// restart uses: restart is an unknown op, info reports waiters, and stop
// stops it. It holds the lock until stopped.
func fakeOldDaemon(t *testing.T, dir string, clients int) {
	t.Helper()
	fakeOldDaemonWithSuccessor(t, dir, clients, -1)
}

// fakeOldDaemonWithSuccessor is fakeOldDaemon, except that from connection
// successorAfter on (counting from 0; never if negative) it greets as a
// 0.1.2 successor, pid 888888, as if a concurrent restart had replaced the
// old daemon. It returns the ops the successor received.
func fakeOldDaemonWithSuccessor(t *testing.T, dir string, clients, successorAfter int) func() []string {
	t.Helper()
	var mu sync.Mutex
	var successorOps []string
	lock, err := os.OpenFile(filepath.Join(dir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(dir, "prwatch.sock"))
	if err != nil {
		t.Fatal(err)
	}
	stop := func() { ln.Close(); lock.Close() }
	t.Cleanup(stop)
	go func() {
		for n := 0; ; n++ {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if successorAfter >= 0 && n >= successorAfter {
				_, _ = c.Write([]byte(`{"type":"hello","protocol":1,"version":"0.1.2","pid":888888}` + "\n"))
				_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
				if line, err := bufio.NewReader(c).ReadBytes('\n'); err == nil {
					var req protocol.Request
					_ = json.Unmarshal(line, &req)
					mu.Lock()
					successorOps = append(successorOps, req.Op)
					mu.Unlock()
				}
				c.Close()
				continue
			}
			_, _ = c.Write([]byte(`{"type":"hello","protocol":1,"version":"0.1.1","pid":999999}` + "\n"))
			line, err := bufio.NewReader(c).ReadBytes('\n')
			if err != nil {
				c.Close()
				continue
			}
			var req protocol.Request
			_ = json.Unmarshal(line, &req)
			switch req.Op {
			case protocol.OpInfo:
				fmt.Fprintf(c, `{"type":"info","info":{"pid":999999,"version":"0.1.1","clients":%d,"prs":1}}`+"\n", clients)
			case protocol.OpStop:
				_, _ = c.Write([]byte(`{"type":"ok"}` + "\n"))
				c.Close()
				stop()
				return
			default:
				fmt.Fprintf(c, `{"type":"error","code":"bad_request","message":"unknown op %s"}`+"\n", req.Op)
			}
			c.Close()
		}
	}()
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), successorOps...)
	}
}

func TestRestartOldDaemon(t *testing.T) {
	// Nothing waits on the new daemon; under -race each command takes an
	// extra second to exit, so keep it around long enough to inspect.
	e := newEnv(t, "PRWATCH_IDLE_GRACE=3s")
	fakeOldDaemon(t, e.dir, 2)
	r := e.run("daemon", "restart")
	if r.code != 1 {
		t.Fatalf("exit %d, want 1: %q %q", r.code, r.stdout, r.stderr)
	}
	for _, want := range []string{
		"daemon pid 999999 is version 0.1.1, which cannot restart gracefully",
		`would end its 2 waiters with "daemon was stopped"`,
		"exit once nothing is waiting",
		"prwatch daemon restart --force",
	} {
		if !strings.Contains(r.stderr, want) {
			t.Fatalf("stderr missing %q:\n%s", want, r.stderr)
		}
	}
	if !e.daemonRunning() {
		t.Fatal("restart without --force must leave the old daemon alone")
	}

	r = e.run("daemon", "restart", "--force")
	if r.code != 0 || !strings.Contains(r.stdout, "daemon restarted: pid 999999 (version 0.1.1) -> pid ") ||
		!strings.Contains(r.stdout, "(version test)") {
		t.Fatalf("--force: exit %d: %q %q", r.code, r.stdout, r.stderr)
	}
	if _, v := e.daemon(); v != "test" {
		t.Fatalf("daemon version after --force: %q", v)
	}
}

func TestRestartOldIdleDaemonNeedsNoForce(t *testing.T) {
	e := newEnv(t)
	fakeOldDaemon(t, e.dir, 0)
	r := e.run("daemon", "restart")
	if r.code != 0 || !strings.Contains(r.stdout, "nothing is waiting on it; stopping it") ||
		!strings.Contains(r.stdout, "-> pid ") {
		t.Fatalf("exit %d: %q %q", r.code, r.stdout, r.stderr)
	}
}

// Two restarts racing on one old daemon: by the time the second goes to
// stop it, a successor owns the socket, and that successor must be left
// alone.
func TestLegacyRestartNeverStopsASuccessor(t *testing.T) {
	e := newEnv(t)
	// Connections 0 and 1 are restart's "restart" and "info"; from the
	// third on, the socket belongs to a successor.
	successorOps := fakeOldDaemonWithSuccessor(t, e.dir, 0, 2)
	r := e.run("daemon", "restart")
	if r.code != 0 || !strings.Contains(r.stdout, "daemon restarted: pid 999999 (version 0.1.1) -> pid 888888 (version 0.1.2)") {
		t.Fatalf("exit %d: %q %q", r.code, r.stdout, r.stderr)
	}
	for _, op := range successorOps() {
		if op == protocol.OpStop {
			t.Fatalf("restart stopped the successor (ops %v)", successorOps())
		}
	}
}

// A request cancelled by the restart was never answered, but the new
// daemon must still keep the request gap after it.
func TestRestartKeepsGapAfterCancelledRequest(t *testing.T) {
	e := newEnv(t, "PRWATCH_MIN_GAP=3s")
	e.fake.AddPR("o", "r", 1)
	w := e.start("wait", "o/r#1", "--for", "merged")
	e.waitWatched(1, 1)
	e.fake.SetDelay(time.Minute)
	started := e.fake.Started()
	eventually(t, 10*time.Second, "a poll in flight", func() bool { return e.fake.Started() > started })
	e.fake.SetDelay(0) // for the new daemon; the poll in flight keeps its delay
	answered := len(e.fake.Requests())
	cancelled := time.Now()
	oldPID, _ := e.daemon()
	if r := e.run("daemon", "restart"); r.code != 0 {
		t.Fatalf("restart: exit %d: %s", r.code, r.stderr)
	}
	e.waitNewDaemon(oldPID, 1, 1)
	reqs := e.fake.Requests()[answered:]
	if len(reqs) == 0 {
		t.Fatal("the new daemon made no request")
	}
	if gap := reqs[0].At.Sub(cancelled); gap < 2900*time.Millisecond {
		t.Fatalf("the new daemon asked GitHub %s after the cancelled request; the gap is 3s", gap)
	}
	if !w.running() {
		t.Fatalf("waiter exited: %s", w.errb.String())
	}
}

func TestRestartCarriesPersistedState(t *testing.T) {
	e := newEnv(t)
	e.fake.AddPR("o", "r", 1)
	w := e.start("wait", "o/r#1", "--for", "merged")
	e.waitWatched(1, 1)
	e.fake.Inject(fakegh.Response{Status: 403, Headers: map[string]string{"Retry-After": "8"},
		Body: `{"message":"You have exceeded a secondary rate limit."}`})
	var limitedAt time.Time
	eventually(t, 5*time.Second, "the secondary limit", func() bool {
		for _, r := range e.fake.Requests() {
			if r.Status == 403 {
				limitedAt = r.At
				return true
			}
		}
		return false
	})
	eventually(t, 2*time.Second, "the back-off in rate.json", func() bool {
		b, _ := os.ReadFile(filepath.Join(e.dir, "rate.json"))
		return strings.Contains(string(b), "secondary limit")
	})
	type rateOut struct {
		Remaining     int        `json:"remaining"`
		BackoffUntil  *time.Time `json:"backoffUntil"`
		BackoffReason string     `json:"backoffReason"`
		DaemonRunning bool       `json:"daemonRunning"`
	}
	rate := func() rateOut {
		var r rateOut
		res := e.run("rate", "--json")
		if err := json.Unmarshal([]byte(res.stdout), &r); err != nil {
			t.Fatalf("rate %q: %v", res.stdout, err)
		}
		return r
	}
	oldPID, _ := e.daemon()
	before := rate()
	if r := e.run("daemon", "restart"); r.code != 0 {
		t.Fatalf("restart: exit %d: %s", r.code, r.stderr)
	}

	// The new daemon reports the carried-over budget and back-off without
	// asking GitHub.
	after := rate()
	if !after.DaemonRunning || after.Remaining != before.Remaining || after.Remaining <= 0 || after.BackoffUntil == nil ||
		!after.BackoffUntil.Equal(*before.BackoffUntil) || !strings.Contains(after.BackoffReason, "retry-after") {
		t.Fatalf("rate before restart %+v, after %+v", before, after)
	}
	if n := strings.Count(e.logText(), "honouring persisted back-off"); n != 1 {
		t.Fatalf("the new daemon should log the persisted back-off once, got %d", n)
	}

	e.waitNewDaemon(oldPID, 1, 1) // after the back-off ends
	var later []fakegh.Request
	for _, r := range e.fake.Requests() {
		if r.At.After(limitedAt) {
			later = append(later, r)
		}
	}
	if len(later) == 0 {
		t.Fatal("no request after the back-off")
	}
	if gap := later[0].At.Sub(limitedAt); gap < 8*time.Second {
		t.Fatalf("the new daemon asked GitHub %s after an 8s retry-after", gap)
	}
	if !w.running() {
		t.Fatalf("waiter exited: %s", w.errb.String())
	}
}

// buildPrwatch builds the real prwatch binary with the given version.
func buildPrwatch(t *testing.T, out, version string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", out, "-ldflags", "-X main.version="+version, "github.com/tvdavies/prwatch")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
}

// replace puts src at dst the way npm does: a new file renamed over the old
// one, so a process running dst keeps its old, now unlinked, binary.
func replace(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	tmp := dst + ".new"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(f, in); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		t.Fatal(err)
	}
}

func TestRespawnAfterBinaryReplaced(t *testing.T) {
	if testing.Short() {
		t.Skip("builds prwatch twice")
	}
	e := newEnv(t)
	builds := t.TempDir()
	oldBuild, newBuild := filepath.Join(builds, "old"), filepath.Join(builds, "new")
	buildPrwatch(t, oldBuild, "0.0.1-old")
	buildPrwatch(t, newBuild, "0.0.2-new")
	// Inside the state dir, so it outlives the clean-up's daemon stop.
	e.bin = filepath.Join(e.dir, "bin", "prwatch")
	if err := os.Mkdir(filepath.Dir(e.bin), 0o700); err != nil {
		t.Fatal(err)
	}
	replace(t, oldBuild, e.bin)

	e.fake.AddPR("o", "r", 1)
	w := e.start("wait", "o/r#1", "--for", "merged", "--timeout", "120s")
	ev := e.start("events", "--pr", "o/r#1", "--json")
	e.waitWatched(1, 2)
	oldPID, v := e.daemon()
	if v != "0.0.1-old" {
		t.Fatalf("daemon version %q", v)
	}

	// Upgrade on disk; the daemon and both clients keep running the old one.
	replace(t, newBuild, e.bin)
	if runtime.GOOS == "linux" {
		exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", oldPID))
		if err != nil || exe != e.bin+" (deleted)" {
			t.Fatalf("daemon exe %q, %v; want the replaced binary", exe, err)
		}
	}
	if r := e.run("list"); !strings.Contains(r.stderr, "the daemon is version 0.0.1-old but this prwatch is 0.0.2-new") {
		t.Fatalf("list should warn about the skew: %q", r.stderr)
	}
	r := e.run("daemon", "restart")
	if r.code != 0 || !strings.Contains(r.stdout, fmt.Sprintf("pid %d (version 0.0.1-old) -> pid ", oldPID)) ||
		!strings.Contains(r.stdout, "(version 0.0.2-new)") {
		t.Fatalf("restart: exit %d: %q %q", r.code, r.stdout, r.stderr)
	}
	midPID := e.waitNewDaemon(oldPID, 1, 2)
	if _, v := e.daemon(); v != "0.0.2-new" {
		t.Fatalf("daemon version after restart %q", v)
	}

	// Now with no restart command: replace the binary again and SIGHUP the
	// daemon. The only process left to start a daemon is the waiter or
	// stream from the first build, whose own executable now reads
	// "… (deleted)" on Linux; it must start the binary now on disk.
	replace(t, oldBuild, e.bin)
	if err := syscall.Kill(midPID, syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	e.waitNewDaemon(midPID, 1, 2)
	if _, v := e.daemon(); v != "0.0.1-old" {
		t.Fatalf("a client respawned version %q, want the binary on disk (0.0.1-old)", v)
	}
	if !w.running() || !ev.running() {
		t.Fatalf("a client exited: wait %q, events %q", w.errb.String(), ev.errb.String())
	}
	if n := len(events(t, ev.out.String())); n != 1 {
		t.Fatalf("restarts produced duplicate events:\n%s", ev.out.String())
	}
}
