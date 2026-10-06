package cli_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tvdavies/prwatch/internal/github"
)

// spawns returns the "starting daemon" lines clients wrote to the log.
func (e *env) spawns() []string {
	var out []string
	for _, line := range strings.Split(e.logText(), "\n") {
		if strings.Contains(line, `msg="starting daemon"`) {
			out = append(out, line)
		}
	}
	return out
}

// nodeArch is process.arch for this GOARCH, as in the npm package names.
func nodeArch() string {
	if runtime.GOARCH == "amd64" {
		return "x64"
	}
	return runtime.GOARCH
}

// npmInstall lays out @tvdavies/prwatch in nodeModules the way a global npm
// install does: the platform package nested under the main package, and
// postinstall's hard link of its binary as the main package's bin.
func npmInstall(t *testing.T, nodeModules, build, version string) {
	t.Helper()
	pkg := filepath.Join(nodeModules, "@tvdavies", "prwatch")
	platBin := filepath.Join(pkg, "node_modules", "@tvdavies", "prwatch-"+runtime.GOOS+"-"+nodeArch(), "bin", "prwatch")
	if err := os.MkdirAll(filepath.Dir(platBin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(pkg, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	replace(t, build, platBin)
	if err := os.Link(platBin, filepath.Join(pkg, "bin", "prwatch")); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf(`{"name":"@tvdavies/prwatch","version":%q,"bin":{"prwatch":"bin/prwatch"}}`, version)
	if err := os.WriteFile(filepath.Join(pkg, "package.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

// npmUpgrade replaces the package the way npm does: it renames the package
// directory to a dot-prefixed temporary name, writes the new version's
// package directory in its place, then deletes the renamed one. It returns
// the temporary name.
func npmUpgrade(t *testing.T, nodeModules, build, version string) string {
	t.Helper()
	pkg := filepath.Join(nodeModules, "@tvdavies", "prwatch")
	retired := filepath.Join(nodeModules, "@tvdavies", fmt.Sprintf(".prwatch-%08x", time.Now().UnixNano()&0xffffffff))
	if err := os.Rename(pkg, retired); err != nil {
		t.Fatal(err)
	}
	npmInstall(t, nodeModules, build, version)
	if err := os.RemoveAll(retired); err != nil {
		t.Fatal(err)
	}
	return retired
}

// TestNpmUpgrade upgrades prwatch twice under a running daemon and clients,
// with an npm-style layout: once followed by `prwatch daemon restart`, and
// once followed by a SIGHUP, where only the clients, whose package
// directory npm has deleted, can start the new daemon. With the bin on the
// clients' PATH they find it there; without it they find the npm launcher
// from their own path (on Linux; on macOS os.Executable is the bin symlink
// they were started from, which is still there).
func TestNpmUpgrade(t *testing.T) {
	if testing.Short() {
		t.Skip("builds prwatch three times")
	}
	builds := t.TempDir()
	versions := []string{"0.0.1-old", "0.0.2-new", "0.0.3-newer"}
	for _, v := range versions {
		buildPrwatch(t, filepath.Join(builds, v), v)
	}
	for _, onPath := range []bool{true, false} {
		name := map[bool]string{true: "bin on PATH", false: "bin not on PATH"}[onPath]
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, "PRWATCH_POLL_FAST=1s", "PRWATCH_POLL_SLOW=1s")
			// Inside the state dir, so it outlives the clean-up's daemon stop.
			prefix := filepath.Join(e.dir, "npm")
			nodeModules := filepath.Join(prefix, "lib", "node_modules")
			npmInstall(t, nodeModules, filepath.Join(builds, versions[0]), versions[0])
			binDir := filepath.Join(prefix, "bin")
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("../lib/node_modules/@tvdavies/prwatch/bin/prwatch", filepath.Join(binDir, "prwatch")); err != nil {
				t.Fatal(err)
			}
			e.bin = filepath.Join(binDir, "prwatch")
			path := filepath.Join(e.dir, "empty-path")
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			if onPath {
				path = binDir
			}
			e.extra = append(e.extra, "PATH="+path)

			k1 := e.fake.AddPR("o", "r", 1)
			k2 := e.fake.AddPR("o", "r", 2)
			merged := e.start("wait", "o/r#1", "--for", "merged", "--json", "--timeout", "120s")
			change := e.start("wait", "o/r#2", "--for", "change", "--json", "--timeout", "120s")
			ev := e.start("events", "--pr", "o/r#1", "--pr", "o/r#2", "--json")
			e.waitWatched(2, 4)
			eventually(t, 5*time.Second, "initial events", func() bool { return len(events(t, ev.out.String())) == 2 })
			oldPID, v := e.daemon()
			if v != versions[0] {
				t.Fatalf("daemon version %q", v)
			}

			// npm upgrade, then prwatch daemon restart.
			retired := npmUpgrade(t, nodeModules, filepath.Join(builds, versions[1]), versions[1])
			if runtime.GOOS == "linux" {
				exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", oldPID))
				want := filepath.Join(retired, "bin", "prwatch") + " (deleted)"
				if err != nil || exe != want {
					t.Fatalf("daemon exe %q, %v; want %q", exe, err, want)
				}
			}
			r := e.run("daemon", "restart")
			if r.code != 0 || !strings.Contains(r.stdout, fmt.Sprintf("pid %d (version %s) -> pid ", oldPID, versions[0])) ||
				!strings.Contains(r.stdout, "(version "+versions[1]+")") || !strings.Contains(r.stdout, "3 waiters handed over") {
				t.Fatalf("restart: exit %d: %q %q", r.code, r.stdout, r.stderr)
			}
			midPID := e.waitNewDaemon(oldPID, 2, 4)
			if _, v := e.daemon(); v != versions[1] {
				t.Fatalf("daemon version after restart %q", v)
			}

			// npm upgrade again, then SIGHUP: no restart command, so a
			// client from the first install has to start the new daemon.
			npmUpgrade(t, nodeModules, filepath.Join(builds, versions[2]), versions[2])
			spawnsBefore := len(e.spawns())
			if err := syscall.Kill(midPID, syscall.SIGHUP); err != nil {
				t.Fatal(err)
			}
			e.waitNewDaemon(midPID, 2, 4)
			if _, v := e.daemon(); v != versions[2] {
				t.Fatalf("a client started version %q after SIGHUP, want %s", v, versions[2])
			}
			via, exe := "PATH", e.bin
			switch {
			case runtime.GOOS != "linux":
				via = "executable"
			case !onPath:
				via, exe = "npm", filepath.Join(nodeModules, "@tvdavies", "prwatch", "bin", "prwatch")
			}
			spawns := e.spawns()[spawnsBefore:]
			if len(spawns) == 0 || len(spawns) > 3 {
				t.Fatalf("%d clients started a daemon after SIGHUP, want 1 to 3:\n%s", len(spawns), strings.Join(spawns, "\n"))
			}
			for _, s := range spawns {
				if !strings.Contains(s, "via="+via) || !strings.Contains(s, "version="+versions[2]) {
					t.Fatalf("client started the daemon from the wrong candidate (want via=%s, %s):\n%s", via, exe, s)
				}
				if via != "executable" && !strings.Contains(s, "exe="+exe+" ") {
					t.Fatalf("client started the daemon from the wrong binary (want %s):\n%s", exe, s)
				}
			}
			if n := strings.Count(e.logText(), `msg="daemon started"`); n != 3 {
				t.Fatalf("%d daemons started, want 3", n)
			}

			// The waiters and the stream kept running and still see changes.
			if !merged.running() || !change.running() || !ev.running() {
				t.Fatalf("a client exited: merged %q, change %q, events %q", merged.errb.String(), change.errb.String(), ev.errb.String())
			}
			e.fake.Update(k2, func(p *github.RawPR) { p.Title = "After two upgrades" })
			if res := change.wait(t, 10*time.Second); res.code != 0 || parseSnap(t, res.stdout).Title != "After two upgrades" {
				t.Fatalf("--for change: exit %d: %s %s", res.code, res.stdout, res.stderr)
			}
			e.fake.Update(k1, merge)
			if res := merged.wait(t, 10*time.Second); res.code != 0 || !parseSnap(t, res.stdout).Merged {
				t.Fatalf("--for merged: exit %d: %s %s", res.code, res.stdout, res.stderr)
			}
			want := map[string]string{"o/r#1": "initial | state OPEN→MERGED", "o/r#2": "initial | title"}
			eventually(t, 5*time.Second, "both changes as events", func() bool { return len(events(t, ev.out.String())) >= 4 })
			if got := byPR(events(t, ev.out.String())); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("events:\n got %v\nwant %v", got, want)
			}
			for _, p := range []*proc{merged, change, ev} {
				if s := p.errb.String(); strings.Contains(s, "warning: could not start") {
					t.Fatalf("a client had to retry: %s", s)
				}
			}
		})
	}
}

// copyTestBinary puts a copy of the test binary, which acts as prwatch, at
// dst.
func copyTestBinary(t *testing.T, dst string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		t.Fatal(err)
	}
	replace(t, os.Args[0], dst)
}

// withVanishingBinary runs clients from a copy of the test binary, with an
// empty PATH, and the given PRWATCH_BIN (which may not exist yet), so that
// once the copy is deleted there is nothing to start a daemon from.
func withVanishingBinary(t *testing.T, e *env, prwatchBin string) (remove func()) {
	t.Helper()
	empty := filepath.Join(e.dir, "empty-path")
	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	e.extra = append(e.extra, "PATH="+empty, "PRWATCH_BIN="+prwatchBin)
	e.bin = filepath.Join(e.dir, "vanishing", "prwatch")
	copyTestBinary(t, e.bin)
	return func() {
		if err := os.RemoveAll(filepath.Dir(e.bin)); err != nil {
			t.Fatal(err)
		}
		// list and the clean-up's daemon stop run the test binary.
		e.bin = ""
	}
}

const retryWarning = "warning: could not start a daemon: no usable prwatch binary to start the daemon from"

// With nothing to start a daemon from, clients retry with back-off instead
// of exiting; once a binary appears (here at PRWATCH_BIN) the first back
// starts one daemon, the rest connect to it, and nothing that changed while
// no daemon was running is missed.
func TestRespawnRetriesUntilBinaryAppears(t *testing.T) {
	e := newEnv(t, "PRWATCH_RETRY_MAX=500ms")
	later := filepath.Join(e.dir, "later", "prwatch")
	remove := withVanishingBinary(t, e, later)
	k := e.fake.AddPR("o", "r", 1)
	var waiters []*proc
	for range 5 {
		waiters = append(waiters, e.start("wait", "o/r#1", "--for", "change", "--json", "--timeout", "60s"))
	}
	ev := e.start("events", "--pr", "o/r#1", "--json")
	clients := append(append([]*proc(nil), waiters...), ev)
	e.waitWatched(1, 6)
	eventually(t, 5*time.Second, "initial event", func() bool { return len(events(t, ev.out.String())) == 1 })
	oldPID, _ := e.daemon()

	remove()
	if err := syscall.Kill(oldPID, syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "every client to warn", func() bool {
		for _, p := range clients {
			if !strings.Contains(p.errb.String(), retryWarning) {
				return false
			}
		}
		return true
	})
	e.fake.Update(k, func(p *github.RawPR) { p.Title = "Changed with no daemon" })
	eventually(t, 10*time.Second, "several failed spawns", func() bool {
		return strings.Count(e.logText(), `msg="cannot start daemon"`) >= 3*len(clients)
	})
	if e.daemonRunning() {
		t.Fatal("a daemon is running with no binary to start it from")
	}
	for _, p := range clients {
		if !p.running() {
			r := p.wait(t, time.Second)
			t.Fatalf("a client exited instead of retrying: exit %d: %s", r.code, r.stderr)
		}
	}

	spawnsBefore := len(e.spawns())
	copyTestBinary(t, later)
	for i, w := range waiters {
		r := w.wait(t, 15*time.Second)
		if r.code != 0 || parseSnap(t, r.stdout).Title != "Changed with no daemon" {
			t.Fatalf("waiter %d: exit %d: %s %s", i, r.code, r.stdout, r.stderr)
		}
		if n := strings.Count(r.stderr, retryWarning); n != 1 {
			t.Fatalf("waiter %d warned %d times, want once:\n%s", i, n, r.stderr)
		}
		if !strings.Contains(r.stderr, "retrying with back-off, until the timeout") ||
			!regexp.MustCompile(`reconnected to daemon pid \d+ \(version test\)`).MatchString(r.stderr) {
			t.Fatalf("waiter %d stderr:\n%s", i, r.stderr)
		}
	}
	eventually(t, 5*time.Second, "the change as an event", func() bool { return len(events(t, ev.out.String())) >= 2 })
	if got := byPR(events(t, ev.out.String()))["o/r#1"]; got != "initial | title" {
		t.Fatalf("events: %q", got)
	}
	if s := ev.errb.String(); strings.Count(s, retryWarning) != 1 || !strings.Contains(s, "retrying with back-off, up to 20 attempts") {
		t.Fatalf("events stderr:\n%s", s)
	}
	if !ev.running() {
		t.Fatalf("events exited: %s", ev.errb.String())
	}

	// One daemon came back, started from PRWATCH_BIN; no client started
	// more than one.
	if n := strings.Count(e.logText(), `msg="daemon started"`); n != 2 {
		t.Fatalf("%d daemons started, want 2", n)
	}
	spawns := e.spawns()[spawnsBefore:]
	if len(spawns) == 0 || len(spawns) > len(clients) {
		t.Fatalf("%d spawns after the binary appeared, want 1 to %d:\n%s", len(spawns), len(clients), strings.Join(spawns, "\n"))
	}
	for _, s := range spawns {
		if !strings.Contains(s, "via=PRWATCH_BIN") || !strings.Contains(s, "exe="+later+" ") {
			t.Fatalf("spawn did not use PRWATCH_BIN:\n%s", s)
		}
	}
}

// With nothing to start a daemon from, a wait retries until its deadline
// and then times out as usual (124), however many attempts that takes, and
// an events stream, which has no deadline, gives up after a bounded number
// of attempts (1).
func TestRespawnFailsUntilDeadline(t *testing.T) {
	e := newEnv(t, "PRWATCH_RETRY_MAX=300ms")
	remove := withVanishingBinary(t, e, filepath.Join(e.dir, "never", "prwatch"))
	e.fake.AddPR("o", "r", 1)
	started := time.Now()
	e.extra = append(e.extra, "PRWATCH_RETRY_ATTEMPTS=4")
	w := e.start("wait", "o/r#1", "--for", "merged", "--timeout", "6s")
	ev := e.start("events", "--pr", "o/r#1")
	e.waitWatched(1, 2)
	oldPID, _ := e.daemon()
	remove()
	if err := syscall.Kill(oldPID, syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}

	r := ev.wait(t, 10*time.Second)
	if r.code != 1 || strings.Count(r.stderr, retryWarning) != 1 ||
		!strings.Contains(r.stderr, "gave up after 4 attempts to start or reach the daemon: could not start a daemon: no usable prwatch binary") {
		t.Fatalf("events: exit %d:\n%s", r.code, r.stderr)
	}
	if !w.running() {
		t.Fatalf("the waiter gave up before its deadline: %s", w.errb.String())
	}

	// Under -race each process takes an extra second to exit.
	r = w.wait(t, 12*time.Second)
	took := time.Since(started)
	if r.code != 124 || took < 6*time.Second || took > 8*time.Second {
		t.Fatalf("waiter: exit %d after %s, want 124 after about 6s:\n%s", r.code, took, r.stderr)
	}
	if strings.Count(r.stderr, retryWarning) != 1 || !strings.Contains(r.stderr, "timed out after 6s waiting for merged") {
		t.Fatalf("waiter stderr:\n%s", r.stderr)
	}
	if !strings.Contains(r.stdout, "o/r#1") {
		t.Fatalf("the timed-out waiter should still print the last snapshot: %q", r.stdout)
	}
	if e.daemonRunning() {
		t.Fatal("a daemon is running with no binary to start it from")
	}
}

// Six clients reconnecting at once after each of several restarts end up on
// one new daemon each time, and no client starts more than one daemon per
// handover.
func TestRestartNoStampede(t *testing.T) {
	e := newEnv(t)
	e.fake.AddPR("o", "r", 1)
	e.fake.AddPR("o", "r", 2)
	var clients []*proc
	for i := range 5 {
		clients = append(clients, e.start("wait", fmt.Sprintf("o/r#%d", i%2+1), "--for", "merged"))
	}
	clients = append(clients, e.start("events", "--json"))
	e.waitWatched(2, 5)
	pid, _ := e.daemon()
	for round := range 3 {
		spawnsBefore := len(e.spawns())
		if err := syscall.Kill(pid, syscall.SIGHUP); err != nil {
			t.Fatal(err)
		}
		next := e.waitNewDaemon(pid, 2, 5)
		// The events stream on all PRs is not counted per PR.
		eventually(t, 5*time.Second, "the events stream to reconnect", func() bool {
			r := e.run("list")
			return strings.Contains(r.stdout, "1 events stream on all PRs")
		})
		pid = next
		spawns := e.spawns()[spawnsBefore:]
		if len(spawns) == 0 || len(spawns) > len(clients) {
			t.Fatalf("round %d: %d spawns from %d clients:\n%s", round, len(spawns), len(clients), strings.Join(spawns, "\n"))
		}
		if n := strings.Count(e.logText(), `msg="daemon started"`); n != round+2 {
			t.Fatalf("round %d: %d daemons started, want %d", round, n, round+2)
		}
	}
	for i, p := range clients {
		if !p.running() {
			t.Fatalf("client %d exited: %s", i, p.errb.String())
		}
		if s := p.errb.String(); s != "" {
			t.Fatalf("client %d wrote to stderr: %s", i, s)
		}
	}
}
