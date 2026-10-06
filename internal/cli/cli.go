// Package cli implements the prwatch command line.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tvdavies/prwatch/internal/client"
	"github.com/tvdavies/prwatch/internal/daemon"
	"github.com/tvdavies/prwatch/internal/github"
	"github.com/tvdavies/prwatch/internal/paths"
	"github.com/tvdavies/prwatch/internal/protocol"
	"github.com/tvdavies/prwatch/internal/prref"
	"github.com/tvdavies/prwatch/internal/ratelimit"
	"github.com/tvdavies/prwatch/internal/snapshot"
)

// Exit codes.
const (
	ExitOK       = 0
	ExitError    = 1
	ExitUsage    = 2
	ExitNotFound = 3
	ExitTimeout  = 124
)

// Stdout and Stderr are swappable for tests.
var (
	Stdout io.Writer = os.Stdout
	Stderr io.Writer = os.Stderr
)

// RemoteFunc resolves bare PR numbers; swappable for tests.
var RemoteFunc prref.RemoteFunc = prref.GitOrigin

const usage = `prwatch: one shared GitHub PR poller per user, for agents and scripts.

Usage:
  prwatch wait <pr> [--for change|checks|review|mergeable|merged|closed]
                    [--since TOKEN] [--timeout DURATION] [--json]
  prwatch status <pr...> [--json]
  prwatch events [--pr <pr>]... [--json]
  prwatch list [--json]
  prwatch rate [--json]
  prwatch daemon status|stop|restart [--force]
  prwatch version

A <pr> is owner/repo#123, a GitHub PR URL, or 123 (or '#123') inside a
checkout whose origin remote is on GitHub.

Exit codes: 0 condition met, 124 timeout, 2 usage or auth error,
3 PR not found, 1 other errors (including temporary GitHub errors and
incomplete data).
`

// Main runs the CLI and returns the process exit code.
func Main(args []string, version string) int {
	version = resolveVersion(version)
	if len(args) == 0 {
		fmt.Fprint(Stderr, usage)
		return ExitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "wait":
		return cmdWait(rest)
	case "status":
		return cmdStatus(rest, version)
	case "events":
		return cmdEvents(rest)
	case "list":
		return cmdList(rest, version)
	case "rate":
		return cmdRate(rest, version)
	case "daemon":
		return cmdDaemon(rest, version)
	case "version", "--version", "-v":
		fmt.Fprintf(Stdout, "prwatch %s (%s/%s, %s)\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
		return ExitOK
	case "__daemon":
		cfg, err := daemon.ConfigFromEnv(version)
		if err != nil {
			fmt.Fprintln(Stderr, "prwatch daemon:", err)
			return ExitUsage
		}
		if err := daemon.Run(cfg); err != nil {
			fmt.Fprintln(Stderr, "prwatch daemon:", err)
			return ExitError
		}
		return ExitOK
	case "help", "-h", "--help":
		fmt.Fprint(Stdout, usage)
		return ExitOK
	}
	fmt.Fprintf(Stderr, "prwatch: unknown command %q\n\n%s", cmd, usage)
	return ExitUsage
}

func resolveVersion(v string) string {
	if v != "" && v != "dev" {
		return v
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return strings.TrimPrefix(bi.Main.Version, "v")
	}
	return "dev"
}

func usageErr(format string, a ...any) int {
	fmt.Fprintf(Stderr, "prwatch: "+format+"\n", a...)
	return ExitUsage
}

func fail(err error) int {
	fmt.Fprintln(Stderr, "prwatch:", err)
	return exitCode(err)
}

// exitCode maps an error to its documented exit code: authentication
// failures are 2, a missing PR or repository is 3, and anything else,
// including transient GitHub errors, is 1.
func exitCode(err error) int {
	var ae *github.AuthError
	var nf *github.NotFoundError
	var ce *codedError
	switch {
	case errors.As(err, &ce):
		return ce.code
	case errors.As(err, &ae):
		return ExitUsage
	case errors.As(err, &nf):
		return ExitNotFound
	}
	return ExitError
}

// parseFlags parses flags that may appear before or after positionals.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		// flag stops at the first positional; "--" ends flag parsing.
		consumed := len(args) - len(rest)
		if consumed > 0 && args[consumed-1] == "--" {
			return append(pos, rest...), nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

func flagError(fs *flag.FlagSet, err error, help string) int {
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(Stdout, help)
		return ExitOK
	}
	return usageErr("%s: %v", fs.Name(), err)
}

// durationFlag accepts Go durations or plain seconds.
type durationFlag struct{ d time.Duration }

func (f *durationFlag) String() string { return f.d.String() }
func (f *durationFlag) Set(s string) error {
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		f.d = time.Duration(n * float64(time.Second))
		return nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q", s)
	}
	f.d = d
	return nil
}

type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }
func (l *listFlag) Set(s string) error {
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*l = append(*l, p)
		}
	}
	return nil
}

func parseRefs(args []string) ([]prref.Ref, error) {
	var refs []prref.Ref
	for _, a := range args {
		r, err := prref.Parse(a, RemoteFunc)
		if err != nil {
			return nil, err
		}
		refs = append(refs, r)
	}
	return refs, nil
}

func stateDir() (string, error) { return paths.StateDir() }

const waitHelp = `Usage: prwatch wait <pr> [--for COND] [--since TOKEN] [--timeout DURATION] [--json]

Blocks until the condition holds, then prints the snapshot and its token.

  --for change     any change to the material fields (default)
  --for checks     the check rollup has left PENDING
  --for review     a new review, or a change to review threads
  --for mergeable  approved, green, no unresolved threads and mergeable
  --for merged     the PR is merged
  --for closed     the PR is closed or merged
  --since TOKEN    only report a state that differs from TOKEN; returns at
                   once if the PR already differs
  --timeout D      give up after D (e.g. 90s, 10m) and exit 124
  --json           print the snapshot as JSON
`

func cmdWait(args []string) int {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	forCond := fs.String("for", snapshot.ForChange, "")
	since := fs.String("since", "", "")
	var timeout durationFlag
	fs.Var(&timeout, "timeout", "")
	asJSON := fs.Bool("json", false, "")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return flagError(fs, err, waitHelp)
	}
	if len(pos) != 1 {
		return usageErr("wait takes exactly one PR (got %d)\n\n%s", len(pos), waitHelp)
	}
	if !snapshot.ValidFor(*forCond) {
		return usageErr("unknown --for %q (want change, checks, review, mergeable, merged or closed)", *forCond)
	}
	w := &snapshot.Waiter{For: *forCond}
	if *since != "" {
		tok, err := snapshot.ParseToken(*since)
		if err != nil {
			return usageErr("%v", err)
		}
		w.Since = &tok
	}
	refs, err := parseRefs(pos)
	if err != nil {
		return usageErr("%v", err)
	}
	dir, err := stateDir()
	if err != nil {
		return fail(err)
	}

	var deadline time.Time
	if timeout.d > 0 {
		deadline = time.Now().Add(timeout.d)
	}
	var last *snapshot.Snapshot
	print := func(s *snapshot.Snapshot) {
		if s == nil {
			return
		}
		if *asJSON {
			b, _ := json.Marshal(s)
			fmt.Fprintln(Stdout, string(b))
		} else {
			fmt.Fprint(Stdout, snapshot.Format(s))
		}
	}
	code, done := stream(dir, protocol.Request{Op: protocol.OpWait, PRs: []string{refs[0].String()}, For: *forCond}, deadline,
		func(m protocol.Message) (int, bool) {
			if m.Type != protocol.TypeSnapshot || m.Snapshot == nil {
				return 0, false
			}
			last = m.Snapshot
			if w.Met(m.Snapshot) {
				print(m.Snapshot)
				return ExitOK, true
			}
			return 0, false
		})
	if !done {
		fmt.Fprintf(Stderr, "prwatch: timed out after %s waiting for %s\n", timeout.d, *forCond)
		print(last)
		return ExitTimeout
	}
	return code
}

// stream runs a subscription, reconnecting if the daemon goes away, and
// feeds snapshot messages to handle until it reports done. It returns
// done=false when the deadline (zero for none) passes, including while
// connecting.
func stream(dir string, req protocol.Request, deadlineAt time.Time, handle func(protocol.Message) (int, bool)) (int, bool) {
	var deadline <-chan time.Time
	if !deadlineAt.IsZero() {
		t := time.NewTimer(time.Until(deadlineAt))
		defer t.Stop()
		deadline = t.C
	}
	failures := 0
	for {
		conn, err := client.DialUntil(dir, true, deadlineAt)
		if errors.Is(err, client.ErrDeadline) {
			return 0, false
		}
		if err != nil {
			return fail(err), true
		}
		if err := conn.Send(req); err != nil {
			conn.Close()
			failures++
			if failures > 5 {
				return fail(err), true
			}
			continue
		}
		msgs := make(chan protocol.Message)
		errc := make(chan error, 1)
		go func() {
			for {
				m, err := conn.Recv()
				if err != nil {
					errc <- err
					return
				}
				msgs <- m
			}
		}()
		reconnect := false
		for !reconnect {
			select {
			case <-deadline:
				conn.Close()
				return 0, false
			case err := <-errc:
				conn.Close()
				failures++
				if failures > 5 {
					return fail(fmt.Errorf("lost connection to daemon: %w", err)), true
				}
				reconnect = true
			case m := <-msgs:
				switch m.Type {
				case protocol.TypeError:
					conn.Close()
					fmt.Fprintln(Stderr, "prwatch:", m.Message)
					switch m.Code {
					case protocol.CodeNotFound:
						return ExitNotFound, true
					case protocol.CodeAuth, protocol.CodeBadRequest:
						return ExitUsage, true
					}
					return ExitError, true
				case protocol.TypeShutdown:
					// After a restart or an idle exit, reconnect: the first
					// client back starts a new daemon. The handler keeps its
					// state, so a wait keeps its deadline, condition and
					// baseline, and events keeps the last state it printed.
					conn.Close()
					if m.Code == protocol.ShutdownStopped {
						fmt.Fprintln(Stderr, "prwatch: daemon was stopped")
						return ExitError, true
					}
					reconnect = true
				default:
					failures = 0
					if code, done := handle(m); done {
						conn.Close()
						return code, true
					}
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

const statusHelp = `Usage: prwatch status <pr...> [--json]

Prints PR snapshots from the daemon cache if a daemon is running, otherwise
with one direct batched request. Does not start a daemon.
`

func cmdStatus(args []string, version string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return flagError(fs, err, statusHelp)
	}
	if len(pos) == 0 {
		return usageErr("status needs at least one PR\n\n%s", statusHelp)
	}
	refs, err := parseRefs(pos)
	if err != nil {
		return usageErr("%v", err)
	}
	dir, err := stateDir()
	if err != nil {
		return fail(err)
	}
	results, err := statusViaDaemon(dir, refs)
	if errors.Is(err, client.ErrNoDaemon) {
		results, err = statusDirect(dir, refs, version)
	}
	if err != nil {
		return fail(err)
	}
	code := ExitOK
	var snaps []*snapshot.Snapshot
	for _, r := range refs {
		res, ok := results[r.Key()]
		switch {
		case !ok:
			fmt.Fprintf(Stderr, "prwatch: no result for %s\n", r)
			code = ExitError
		case res.err != nil:
			fmt.Fprintln(Stderr, "prwatch:", res.err)
			if code == ExitOK {
				code = res.code
			}
		case res.snap.Incomplete:
			// Print it, but don't let a script read partial data as success.
			fmt.Fprintf(Stderr, "prwatch: %s is incomplete: %s\n", res.snap.PR, res.snap.IncompleteReason)
			snaps = append(snaps, res.snap)
			if code == ExitOK {
				code = ExitError
			}
		default:
			snaps = append(snaps, res.snap)
		}
	}
	if *asJSON {
		if snaps == nil {
			snaps = []*snapshot.Snapshot{}
		}
		b, _ := json.MarshalIndent(snaps, "", "  ")
		fmt.Fprintln(Stdout, string(b))
	} else {
		for i, s := range snaps {
			if i > 0 {
				fmt.Fprintln(Stdout)
			}
			fmt.Fprint(Stdout, snapshot.Format(s))
		}
	}
	return code
}

type statusResult struct {
	snap *snapshot.Snapshot
	err  error
	code int
}

func statusViaDaemon(dir string, refs []prref.Ref) (map[string]statusResult, error) {
	conn, err := client.Dial(dir, false)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	var prs []string
	for _, r := range refs {
		prs = append(prs, r.String())
	}
	if err := conn.Send(protocol.Request{Op: protocol.OpStatus, PRs: prs}); err != nil {
		return nil, err
	}
	// The daemon answers from cache, or after one on-demand fetch.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Minute))
	out := map[string]statusResult{}
	for {
		m, err := conn.Recv()
		if err != nil {
			return nil, fmt.Errorf("daemon: %w", err)
		}
		switch m.Type {
		case protocol.TypeEnd:
			return out, nil
		case protocol.TypeShutdown:
			return nil, client.ErrNoDaemon
		case protocol.TypeSnapshot:
			if r, err := prref.Parse(m.PR, nil); err == nil {
				out[r.Key()] = statusResult{snap: m.Snapshot}
			}
		case protocol.TypeError:
			code := ExitError
			switch m.Code {
			case protocol.CodeNotFound:
				code = ExitNotFound
			case protocol.CodeAuth, protocol.CodeBadRequest:
				code = ExitUsage
			}
			if m.PR == "" {
				return nil, &codedError{msg: m.Message, code: code}
			}
			if r, err := prref.Parse(m.PR, nil); err == nil {
				out[r.Key()] = statusResult{err: errors.New(m.Message), code: code}
			}
		}
	}
}

type codedError struct {
	msg  string
	code int
}

func (e *codedError) Error() string { return e.msg }

func statusDirect(dir string, refs []prref.Ref, version string) (map[string]statusResult, error) {
	gov := ratelimit.New(paths.Rate(dir))
	// Checked before every request: a response may exhaust the budget.
	checkBackoff := func() error {
		if blocked, until := gov.Blocked(); blocked {
			return fmt.Errorf("rate-limit back-off in force until %s; not calling GitHub", until.Local().Format(time.Kitchen))
		}
		return nil
	}
	if err := checkBackoff(); err != nil {
		return nil, err
	}
	gh := github.NewClient(os.Getenv("PRWATCH_GRAPHQL_URL"), "prwatch/"+version, &github.TokenSource{})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out := map[string]statusResult{}
	var snaps []*snapshot.Snapshot
	seen := map[string]bool{}
	var uniq []prref.Ref
	for _, r := range refs {
		if !seen[r.Key()] {
			seen[r.Key()] = true
			uniq = append(uniq, r)
		}
	}
	handle := func(err error) error {
		var rl *github.RateLimitError
		if errors.As(err, &rl) {
			until := gov.OnRateLimit(rl)
			return fmt.Errorf("%w; backing off until %s", err, until.Local().Format(time.Kitchen))
		}
		return err
	}
	for i := 0; i < len(uniq); i += github.ChunkSize {
		chunk := uniq[i:min(i+github.ChunkSize, len(uniq))]
		if err := checkBackoff(); err != nil {
			return nil, err
		}
		res, rate, err := gh.Resolve(ctx, chunk)
		if err != nil {
			return nil, handle(err)
		}
		gov.Observe(rate)
		for _, r := range res {
			if r.Err != nil {
				out[r.Ref.Key()] = statusResult{err: r.Err, code: exitCode(r.Err)}
				continue
			}
			out[r.Ref.Key()] = statusResult{snap: r.Snapshot}
			snaps = append(snaps, r.Snapshot)
		}
	}
	if missing := github.MissingThreads(snaps, nil); len(missing) > 0 {
		heads := map[string]github.ThreadHead{}
		for i := 0; i < len(missing) && checkBackoff() == nil; i += 100 {
			h, rate, err := gh.ThreadHeads(ctx, missing[i:min(i+100, len(missing))])
			if err != nil {
				return nil, handle(err)
			}
			gov.Observe(rate)
			for k, v := range h {
				heads[k] = v
			}
		}
		for _, s := range snaps {
			github.FillThreads(s, heads)
		}
	}
	return out, nil
}

const eventsHelp = `Usage: prwatch events [--pr <pr>]... [--json]

Streams one line per PR change (JSON Lines with --json) until interrupted.
Without --pr it streams changes to every PR the daemon is watching. An
events stream counts as a waiter and keeps the daemon alive.
`

func cmdEvents(args []string) int {
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	var prs listFlag
	fs.Var(&prs, "pr", "")
	asJSON := fs.Bool("json", false, "")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return flagError(fs, err, eventsHelp)
	}
	refs, err := parseRefs(append(prs, pos...))
	if err != nil {
		return usageErr("%v", err)
	}
	dir, err := stateDir()
	if err != nil {
		return fail(err)
	}
	var names []string
	for _, r := range refs {
		names = append(names, r.String())
	}
	// The last state printed for each PR. After a reconnect (a daemon
	// restart, say) the new daemon sends each PR's state as "initial": that
	// is suppressed when nothing changed, and reported as a change against
	// what was last printed when something did.
	printed := map[string]*snapshot.Snapshot{}
	code, _ := stream(dir, protocol.Request{Op: protocol.OpEvents, PRs: names}, time.Time{}, func(m protocol.Message) (int, bool) {
		if m.Snapshot == nil {
			return 0, false
		}
		key := strings.ToLower(m.Snapshot.PR)
		if prev := printed[key]; prev != nil {
			if snapshot.ComputeToken(prev) == snapshot.ComputeToken(m.Snapshot) {
				return 0, false
			}
			if len(m.Changes) == 1 && m.Changes[0] == "initial" {
				m.Changes = snapshot.Changes(prev, m.Snapshot)
			}
		}
		printed[key] = m.Snapshot
		if *asJSON {
			b, _ := json.Marshal(struct {
				Type     string             `json:"type"`
				Time     time.Time          `json:"time"`
				PR       string             `json:"pr"`
				Changes  []string           `json:"changes"`
				Snapshot *snapshot.Snapshot `json:"snapshot"`
			}{"change", time.Now().UTC().Truncate(time.Second), m.Snapshot.PR, m.Changes, m.Snapshot})
			fmt.Fprintln(Stdout, string(b))
		} else {
			fmt.Fprintf(Stdout, "%s %s: %s | %s | token %s\n", time.Now().Format("15:04:05"), m.Snapshot.PR,
				strings.Join(m.Changes, "; "), snapshot.Summary(m.Snapshot), m.Snapshot.Token)
		}
		return 0, false
	})
	return code
}

const listHelp = `Usage: prwatch list [--json]

Lists the PRs the daemon is watching. Does not start a daemon or keep one
alive.
`

// warnSkew warns when the daemon runs a different version from this binary,
// typically because prwatch was upgraded while the daemon was running.
func warnSkew(daemonVersion, version string) {
	if daemonVersion == "" || daemonVersion == version {
		return
	}
	fmt.Fprintf(Stderr, "prwatch: warning: the daemon is version %s but this prwatch is %s; run `prwatch daemon restart` to switch to %s\n",
		daemonVersion, version, version)
}

func cmdList(args []string, version string) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return flagError(fs, err, listHelp)
	}
	if len(pos) > 0 {
		return usageErr("list takes no arguments")
	}
	dir, err := stateDir()
	if err != nil {
		return fail(err)
	}
	m, _, err := client.Request(dir, protocol.OpList)
	if errors.Is(err, client.ErrNoDaemon) {
		if *asJSON {
			fmt.Fprintf(Stdout, `{"prs":[],"daemon":null,"daemonVersion":null,"clientVersion":%q}`+"\n", version)
		} else {
			fmt.Fprintln(Stdout, "Nothing is being watched (no daemon running).")
		}
		return ExitOK
	}
	if err != nil {
		return fail(err)
	}
	if m.List == nil {
		return fail(fmt.Errorf("unexpected reply from daemon: %s", m.Type))
	}
	l := m.List
	warnSkew(l.DaemonVersion, version)
	if *asJSON {
		b, _ := json.MarshalIndent(struct {
			PRs           []protocol.Watched `json:"prs"`
			Daemon        any                `json:"daemon"`
			DaemonVersion string             `json:"daemonVersion"`
			ClientVersion string             `json:"clientVersion"`
		}{l.PRs, map[string]any{"pid": l.DaemonPID, "version": l.DaemonVersion, "allStreams": l.AllStreams, "rate": l.Rate},
			l.DaemonVersion, version}, "", "  ")
		fmt.Fprintln(Stdout, string(b))
		return ExitOK
	}
	now := time.Now()
	if len(l.PRs) == 0 {
		fmt.Fprintf(Stdout, "Nothing is being watched (daemon pid %d, version %s, is idle).\n", l.DaemonPID, l.DaemonVersion)
	}
	for _, w := range l.PRs {
		title := w.Title
		if title == "" {
			title = "(not fetched yet)"
		}
		fmt.Fprintf(Stdout, "%s  %s\n", w.PR, title)
		who := plural(w.Waiters, "waiter")
		if w.Streams > 0 {
			who += ", " + plural(w.Streams, "stream")
		}
		lastPoll := "never"
		if w.LastPoll != nil {
			lastPoll = ago(now.Sub(*w.LastPoll)) + " ago"
		}
		fmt.Fprintf(Stdout, "  %s (%s) · watched %s · last poll %s\n", who, strings.Join(w.For, ", "), ago(now.Sub(w.WatchedSince)), lastPoll)
		if w.Snapshot != nil {
			fmt.Fprintf(Stdout, "  %s\n", snapshot.Summary(w.Snapshot))
		}
	}
	if l.AllStreams > 0 {
		fmt.Fprintf(Stdout, "%s on all PRs\n", plural(l.AllStreams, "events stream"))
	}
	fmt.Fprintln(Stdout)
	fmt.Fprintf(Stdout, "poll interval %s · %s · back-off: %s\n", l.Rate.Interval, budget(l.Rate, now), backoff(l.Rate, now))
	fmt.Fprintf(Stdout, "daemon pid %d, version %s\n", l.DaemonPID, l.DaemonVersion)
	return ExitOK
}

func plural(n int, s string) string {
	if n == 1 {
		return "1 " + s
	}
	return fmt.Sprintf("%d %ss", n, s)
}

func ago(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return d.Truncate(time.Second).String()
	case d < time.Hour:
		return d.Truncate(time.Second).String()
	default:
		return d.Truncate(time.Minute).String()
	}
}

func budget(r protocol.Rate, now time.Time) string {
	if r.Limit == 0 || r.Remaining < 0 {
		return "budget unknown"
	}
	s := fmt.Sprintf("budget %d/%d", r.Remaining, r.Limit)
	if r.ResetAt != nil && r.ResetAt.After(now) {
		s += fmt.Sprintf(", resets in %s", ago(r.ResetAt.Sub(now)))
	}
	return s
}

func backoff(r protocol.Rate, now time.Time) string {
	if r.BackoffUntil == nil || !r.BackoffUntil.After(now) {
		return "none"
	}
	s := fmt.Sprintf("for %s", ago(r.BackoffUntil.Sub(now)))
	if r.BackoffReason != "" {
		s += " (" + r.BackoffReason + ")"
	}
	return s
}

func cmdRate(args []string, version string) int {
	fs := flag.NewFlagSet("rate", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "")
	if _, err := parseFlags(fs, args); err != nil {
		return flagError(fs, err, "Usage: prwatch rate [--json]\n")
	}
	dir, err := stateDir()
	if err != nil {
		return fail(err)
	}
	var r protocol.Rate
	running := true
	m, hello, err := client.Request(dir, protocol.OpRate)
	switch {
	case errors.Is(err, client.ErrNoDaemon):
		running = false
		st := ratelimit.New(paths.Rate(dir)).Snapshot()
		r = protocol.Rate{Limit: st.Limit, Remaining: st.Remaining, LastCost: st.LastCost, RoundCost: st.RoundCost, BackoffReason: st.BackoffReason}
		if !st.ResetAt.IsZero() {
			r.ResetAt = &st.ResetAt
		}
		if st.BackoffUntil.After(time.Now()) {
			r.BackoffUntil = &st.BackoffUntil
		}
	case err != nil:
		return fail(err)
	case m.Rate == nil:
		return fail(fmt.Errorf("unexpected reply from daemon: %s", m.Type))
	default:
		r = *m.Rate
		warnSkew(hello.Version, version)
	}
	if *asJSON {
		b, _ := json.MarshalIndent(struct {
			protocol.Rate
			DaemonRunning bool `json:"daemonRunning"`
		}{r, running}, "", "  ")
		fmt.Fprintln(Stdout, string(b))
		return ExitOK
	}
	now := time.Now()
	fmt.Fprintln(Stdout, budget(r, now))
	fmt.Fprintf(Stdout, "last request cost %d, last round cost %d\n", r.LastCost, r.RoundCost)
	if running {
		fmt.Fprintf(Stdout, "poll interval %s, %d rounds, %d requests\n", r.Interval, r.Rounds, r.Requests)
	} else {
		fmt.Fprintln(Stdout, "daemon not running (figures from the last saved state)")
	}
	fmt.Fprintf(Stdout, "back-off: %s\n", backoff(r, now))
	return ExitOK
}

const daemonHelp = `Usage: prwatch daemon status|stop|restart [--force]

  status   show the running daemon's pid, version, waiters and PRs
  stop     stop the daemon; waiters exit with "daemon was stopped" (exit 1)
  restart  hand over to a new daemon started from the prwatch binary now on
           disk, for example after an upgrade. Waiters and events streams
           reconnect and carry on. Persisted state (rate.json) carries over.
           --force stops a daemon too old to restart gracefully (0.1.1 or
           earlier), ending its waiters with "daemon was stopped".

SIGHUP to the daemon restarts it like restart; SIGTERM and SIGINT stop it.
`

func cmdDaemon(args []string, version string) int {
	if len(args) == 0 {
		return usageErr("%s", strings.TrimSuffix(daemonHelp, "\n"))
	}
	switch args[0] {
	case "status", "stop":
		if len(args) != 1 {
			return usageErr("daemon %s takes no arguments", args[0])
		}
	case "restart":
	case "help", "-h", "--help":
		fmt.Fprint(Stdout, daemonHelp)
		return ExitOK
	default:
		return usageErr("%s", strings.TrimSuffix(daemonHelp, "\n"))
	}
	dir, err := stateDir()
	if err != nil {
		return fail(err)
	}
	switch args[0] {
	case "status":
		return cmdDaemonStatus(dir, version)
	case "restart":
		fs := flag.NewFlagSet("daemon restart", flag.ContinueOnError)
		force := fs.Bool("force", false, "")
		pos, err := parseFlags(fs, args[1:])
		if err != nil {
			return flagError(fs, err, daemonHelp)
		}
		if len(pos) > 0 {
			return usageErr("daemon restart takes no arguments")
		}
		return cmdDaemonRestart(dir, version, *force)
	}
	return cmdDaemonStop(dir)
}

func cmdDaemonStatus(dir, version string) int {
	m, _, err := client.Request(dir, protocol.OpInfo)
	if errors.Is(err, client.ErrNoDaemon) {
		fmt.Fprintln(Stdout, "daemon not running")
		return ExitError
	}
	if err != nil {
		return fail(err)
	}
	if m.Info == nil {
		return fail(fmt.Errorf("unexpected reply from daemon: %s", m.Type))
	}
	i := m.Info
	fmt.Fprintf(Stdout, "daemon running: pid %d, version %s, up %s\n", i.PID, i.Version, ago(time.Since(i.StartedAt)))
	fmt.Fprintf(Stdout, "%s, watching %s\n", plural(i.Clients, "waiter"), plural(i.PRs, "PR"))
	fmt.Fprintf(Stdout, "socket %s\nlog %s\n", i.Socket, i.Log)
	warnSkew(i.Version, version)
	return ExitOK
}

func cmdDaemonStop(dir string) int {
	m, hello, err := client.Request(dir, protocol.OpStop)
	if errors.Is(err, client.ErrNoDaemon) {
		fmt.Fprintln(Stdout, "daemon not running")
		return ExitOK
	}
	if err != nil {
		return fail(err)
	}
	if m.Type != protocol.TypeOK {
		return fail(fmt.Errorf("unexpected reply from daemon: %s", m.Type))
	}
	waitGone(dir, hello.PID, 5*time.Second)
	fmt.Fprintln(Stdout, "daemon stopped")
	return ExitOK
}

// waitGone waits up to d for the daemon with the given pid to exit and its
// socket to go.
func waitGone(dir string, pid int, d time.Duration) bool {
	sock := paths.Socket(dir)
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		_, err := os.Stat(sock)
		if os.IsNotExist(err) && !processAlive(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// cmdDaemonRestart asks the running daemon to hand over gracefully, then
// makes sure a new daemon is up, started from this binary if no waiter got
// there first, and reports both.
func cmdDaemonRestart(dir, version string, force bool) int {
	conn, err := client.Dial(dir, false)
	if errors.Is(err, client.ErrNoDaemon) {
		fmt.Fprintf(Stdout, "daemon not running; nothing to restart (the next wait or events starts version %s)\n", version)
		return ExitOK
	}
	if err != nil {
		return fail(err)
	}
	old := conn.Hello
	err = conn.Send(protocol.Request{Op: protocol.OpRestart})
	var m protocol.Message
	if err == nil {
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		m, err = conn.Recv()
	}
	conn.Close()
	if err != nil {
		return fail(fmt.Errorf("daemon pid %d: %w", old.PID, err))
	}
	handedOver := -1
	switch {
	case m.Type == protocol.TypeOK:
		if m.Info != nil {
			handedOver = m.Info.Clients
		}
	case m.Type == protocol.TypeError && strings.HasPrefix(m.Message, "unknown op"):
		// A daemon from 0.1.1 or earlier: it can only be stopped, which
		// ends its waiters with "daemon was stopped".
		clients := -1
		if im, _, err := client.Request(dir, protocol.OpInfo); err == nil && im.Info != nil && im.Info.PID == old.PID {
			clients = im.Info.Clients
		}
		if clients != 0 && !force {
			who := "its waiters"
			if clients > 0 {
				who = "its " + plural(clients, "waiter")
			}
			fmt.Fprintf(Stderr, "prwatch: daemon pid %d is version %s, which cannot restart gracefully.\n", old.PID, old.Version)
			fmt.Fprintf(Stderr, "Stopping it now would end %s with \"daemon was stopped\".\n", who)
			fmt.Fprintf(Stderr, "Either leave it to exit once nothing is waiting (the next wait or events then starts version %s),\n", version)
			fmt.Fprintln(Stderr, "or run `prwatch daemon restart --force` to stop it now.")
			return ExitError
		}
		if clients == 0 {
			fmt.Fprintf(Stdout, "daemon pid %d (version %s) cannot restart gracefully, but nothing is waiting on it; stopping it\n", old.PID, old.Version)
		} else {
			fmt.Fprintf(Stdout, "daemon pid %d (version %s) cannot restart gracefully; stopping it (its waiters exit with \"daemon was stopped\")\n", old.PID, old.Version)
		}
		sm, _, err := client.Request(dir, protocol.OpStop)
		if err != nil && !errors.Is(err, client.ErrNoDaemon) {
			return fail(err)
		}
		if err == nil && sm.Type != protocol.TypeOK {
			return fail(fmt.Errorf("unexpected reply from daemon: %s", sm.Type))
		}
	case m.Type == protocol.TypeError:
		return fail(fmt.Errorf("daemon refused to restart: %s", m.Message))
	default:
		return fail(fmt.Errorf("unexpected reply from daemon: %s", m.Type))
	}

	// Reconnecting waiters may already have started the new daemon; if not,
	// start it from this binary.
	next, err := awaitSuccessor(dir, old.PID, 20*time.Second)
	if err != nil {
		return fail(fmt.Errorf("the old daemon (pid %d) has gone but no new one came up: %w", old.PID, err))
	}
	fmt.Fprintf(Stdout, "daemon restarted: pid %d (version %s) -> pid %d (version %s)\n", old.PID, old.Version, next.PID, next.Version)
	if handedOver > 0 {
		fmt.Fprintf(Stdout, "%s handed over\n", plural(handedOver, "waiter"))
	}
	if next.Version != version {
		fmt.Fprintf(Stderr, "prwatch: warning: the new daemon is version %s but this prwatch is %s; a waiter started it from another binary\n",
			next.Version, version)
	}
	return ExitOK
}

// awaitSuccessor waits for a daemon other than oldPID to answer on the
// socket, starting one from this binary when nothing is listening.
func awaitSuccessor(dir string, oldPID int, d time.Duration) (protocol.Hello, error) {
	deadline := time.Now().Add(d)
	for {
		c, err := client.DialUntil(dir, true, deadline)
		if err != nil {
			return protocol.Hello{}, err
		}
		h := c.Hello
		c.Close()
		if h.PID != oldPID {
			return h, nil
		}
		// The old daemon has replied but not yet closed its socket.
		if time.Now().After(deadline) {
			return protocol.Hello{}, client.ErrDeadline
		}
		time.Sleep(20 * time.Millisecond)
	}
}
