// Package daemon implements prwatch's shared per-user poller.
//
// One daemon runs per user per machine. It holds an exclusive flock on
// daemon.lock for its whole life, which makes start-up races and stale
// socket recovery safe, then binds the unix socket. Every open client
// connection is an interest registration. The daemon polls the union of
// PRs that open connections care about, never sends requests concurrently,
// and exits after an idle grace period once the last waiter has gone.
//
// There are three ways to shut down. An idle exit and a graceful restart
// (prwatch daemon restart, or SIGHUP) both tell clients to reconnect; the
// first to do so starts a fresh daemon from the executable now on disk. An
// explicit stop (prwatch daemon stop, SIGTERM or SIGINT) tells clients the
// daemon was stopped, and they exit.
package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tvdavies/prwatch/internal/github"
	"github.com/tvdavies/prwatch/internal/paths"
	"github.com/tvdavies/prwatch/internal/protocol"
	"github.com/tvdavies/prwatch/internal/prref"
	"github.com/tvdavies/prwatch/internal/ratelimit"
	"github.com/tvdavies/prwatch/internal/snapshot"
)

type watch struct {
	ref          prref.Ref
	key          string
	nodeID       string
	snap         *snapshot.Snapshot
	lastFetched  time.Time
	watchedSince time.Time
	needsRefresh bool
	subs         map[*sub]struct{}
}

type sub struct {
	op        string
	forCond   string
	all       bool // events stream with no PR filter
	keepAlive bool
	keys      []string
	got       map[string]bool
	out       chan protocol.Message
}

// shutdownMode records why the daemon is shutting down.
type shutdownMode int

const (
	modeIdle    shutdownMode = iota // idle grace elapsed
	modeStop                        // explicit stop: clients exit
	modeRestart                     // graceful restart: clients reconnect
)

// code is the shutdown code sent to connected clients.
func (m shutdownMode) code() string {
	switch m {
	case modeStop:
		return protocol.ShutdownStopped
	case modeRestart:
		return protocol.ShutdownRestarting
	}
	return protocol.ShutdownStopping
}

// Daemon is the running poller and socket server.
type Daemon struct {
	cfg    Config
	log    *slog.Logger
	gh     *github.Client
	gov    *ratelimit.Governor
	ln     *net.UnixListener
	ctx    context.Context // cancelled at shutdown, aborting in-flight requests
	cancel context.CancelFunc
	done   chan struct{}
	kick   chan struct{}
	wg     sync.WaitGroup

	mu           sync.Mutex
	watches      map[string]*watch
	subs         map[*sub]struct{}
	active       int // keep-alive subscriptions
	idleTimer    *time.Timer
	idleGen      int
	shuttingDown bool
	mode         shutdownMode
	startedAt    time.Time
	interval     time.Duration
	lastRound    time.Time
	lastRequest  time.Time
	pendingSince time.Time
	transient    time.Duration
	transientEnd time.Time
	authErr      string // set while authentication is failing
	authUntil    time.Time
	rounds       int
	requests     int
	ids          map[string]string
	heads        map[string]github.ThreadHead
}

// Run starts the daemon and blocks until it exits. If another daemon already
// holds the lock it returns nil straight away.
func Run(cfg Config) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	sock := paths.Socket(cfg.StateDir)
	lock, ok := acquireLock(cfg.StateDir, sock)
	if !ok {
		log.Debug("another daemon is running; exiting", "pid", os.Getpid())
		return nil
	}
	defer lock.Close()

	// We hold the lock, so any existing socket file is stale.
	_ = os.Remove(sock)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		return err
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		ln.Close()
		return err
	}

	gov := ratelimit.New(paths.Rate(cfg.StateDir))
	gov.Share = cfg.BudgetShare
	gov.MinBackoff = cfg.MinBackoff
	ctx, cancel := context.WithCancel(context.Background())
	d := &Daemon{
		ctx:       ctx,
		cancel:    cancel,
		cfg:       cfg,
		log:       log,
		gh:        github.NewClient(cfg.GraphQLURL, "prwatch/"+cfg.Version, &github.TokenSource{}),
		gov:       gov,
		ln:        ln,
		done:      make(chan struct{}),
		kick:      make(chan struct{}, 1),
		watches:   map[string]*watch{},
		subs:      map[*sub]struct{}{},
		startedAt: time.Now(),
		interval:  cfg.SlowInterval,
		ids:       loadIDs(paths.IDs(cfg.StateDir)),
		heads:     map[string]github.ThreadHead{},
	}
	exe, _ := os.Executable()
	log.Info("daemon started", "pid", os.Getpid(), "version", cfg.Version, "exe", exe, "socket", sock,
		"idleGrace", cfg.IdleGrace, "fast", cfg.FastInterval, "slow", cfg.SlowInterval, "budgetShare", cfg.BudgetShare)
	d.restoreRateState()

	// SIGHUP hands over gracefully, like prwatch daemon restart; SIGTERM
	// and SIGINT are an explicit stop.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigs)
	go func() {
		select {
		case s := <-sigs:
			mode := modeStop
			if s == syscall.SIGHUP {
				mode = modeRestart
			}
			log.Info("signal received", "signal", s.String(), "restart", mode == modeRestart)
			d.shutdown(mode)
		case <-d.done:
		}
	}()

	d.mu.Lock()
	d.startIdleTimerLocked()
	d.mu.Unlock()

	pollDone := make(chan struct{})
	go func() { defer close(pollDone); d.pollLoop() }()
	d.acceptLoop()
	<-pollDone
	// A request cancelled by the shutdown was never observed; record it so
	// the next daemon still keeps the request gap after it.
	d.mu.Lock()
	last := d.lastRequest
	d.mu.Unlock()
	d.gov.RecordRequest(last)
	waitTimeout(&d.wg, 2*time.Second)
	saveIDs(paths.IDs(cfg.StateDir), d.snapshotIDs())
	log.Info("daemon exiting", "rounds", d.rounds, "requests", d.requests, "reason", d.shutdownCode())
	return nil
}

// restoreRateState applies the rate state loaded from rate.json, which an
// earlier daemon or a direct status call wrote. Its back-off and low-budget
// reserve are enforced by the governor; the time of the last recorded
// request seeds lastRequest so the request gap also holds across restarts.
func (d *Daemon) restoreRateState() {
	now := time.Now()
	st := d.gov.Snapshot()
	if !st.UpdatedAt.IsZero() && !st.UpdatedAt.After(now) {
		d.lastRequest = st.UpdatedAt
	}
	if st.BackoffUntil.After(now) {
		d.log.Warn("honouring persisted back-off", "until", st.BackoffUntil.Format(time.RFC3339), "reason", st.BackoffReason)
	}
	if until := d.gov.ReserveUntil(); until.After(now) {
		d.log.Warn("stored budget is below the reserve; waiting for the reset", "remaining", st.Remaining,
			"until", until.Format(time.RFC3339))
	}
}

func acquireLock(dir, sock string) (*os.File, bool) {
	deadline := time.Now().Add(3 * time.Second)
	for {
		f, err := os.OpenFile(paths.Lock(dir), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, false
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return f, true
		}
		f.Close()
		// Someone holds the lock. If they are serving, we are not needed.
		if c, err := net.DialTimeout("unix", sock, 200*time.Millisecond); err == nil {
			c.Close()
			return nil, false
		}
		// Otherwise they may be shutting down; wait briefly to take over.
		if time.Now().After(deadline) {
			return nil, false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func waitTimeout(wg *sync.WaitGroup, d time.Duration) {
	ch := make(chan struct{})
	go func() { wg.Wait(); close(ch) }()
	select {
	case <-ch:
	case <-time.After(d):
	}
}

func (d *Daemon) acceptLoop() {
	for {
		conn, err := d.ln.Accept()
		if err != nil {
			d.mu.Lock()
			closing := d.shuttingDown
			d.mu.Unlock()
			if closing {
				return
			}
			d.log.Warn("accept failed", "err", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			d.handle(conn)
		}()
	}
}

// shutdown stops accepting connections and ends the poller. The mode decides
// what connected clients are told: after an explicit stop they exit, after a
// restart or idle exit they reconnect.
func (d *Daemon) shutdown(mode shutdownMode) {
	d.mu.Lock()
	ok := d.beginShutdownLocked(mode)
	d.mu.Unlock()
	if ok {
		d.finishShutdown()
	}
}

// beginShutdownLocked commits to shutting down; from here register refuses
// new interest, so clients reconnect to a fresh daemon.
func (d *Daemon) beginShutdownLocked(mode shutdownMode) bool {
	if d.shuttingDown {
		return false
	}
	d.shuttingDown = true
	d.mode = mode
	if d.idleTimer != nil {
		d.idleTimer.Stop()
	}
	return true
}

func (d *Daemon) finishShutdown() {
	d.cancel()
	// Closing the listener unlinks the socket file.
	d.ln.Close()
	close(d.done)
}

func (d *Daemon) startIdleTimerLocked() {
	if d.idleTimer != nil {
		d.idleTimer.Stop()
	}
	d.idleGen++
	gen := d.idleGen
	d.idleTimer = time.AfterFunc(d.cfg.IdleGrace, func() {
		d.mu.Lock()
		ok := gen == d.idleGen && d.active == 0 && d.beginShutdownLocked(modeIdle)
		d.mu.Unlock()
		if ok {
			d.log.Info("idle grace elapsed with no waiters; exiting", "grace", d.cfg.IdleGrace)
			d.finishShutdown()
		}
	})
}

type connWriter struct{ c net.Conn }

func (w connWriter) send(v any) error {
	b, err := protocol.Encode(v)
	if err != nil {
		return err
	}
	_ = w.c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err = w.c.Write(b)
	return err
}

func (d *Daemon) handle(conn net.Conn) {
	defer conn.Close()
	w := connWriter{conn}
	if err := w.send(protocol.Hello{Type: "hello", Protocol: protocol.Version, Version: d.cfg.Version, PID: os.Getpid()}); err != nil {
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReaderSize(conn, 64<<10)
	line, err := r.ReadBytes('\n')
	if err != nil {
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	var req protocol.Request
	if err := json.Unmarshal(line, &req); err != nil {
		_ = w.send(protocol.Message{Type: protocol.TypeError, Code: protocol.CodeBadRequest, Message: "malformed request"})
		return
	}
	// stop, restart and info carry nothing but the op, so they are served
	// whatever protocol the client speaks: a client from a future release
	// can still inspect and replace this daemon.
	if req.Protocol != protocol.Version && req.Op != protocol.OpStop && req.Op != protocol.OpRestart && req.Op != protocol.OpInfo {
		_ = w.send(protocol.Message{Type: protocol.TypeError, Code: protocol.CodeBadRequest,
			Message: "protocol version mismatch; run `prwatch daemon restart` after upgrading"})
		return
	}
	switch req.Op {
	case protocol.OpWait, protocol.OpEvents, protocol.OpStatus:
		d.serveSubscription(conn, r, w, req)
	case protocol.OpList:
		l := d.list()
		_ = w.send(protocol.Message{Type: protocol.TypeList, List: &l})
	case protocol.OpRate:
		rt := d.rate()
		_ = w.send(protocol.Message{Type: protocol.TypeRate, Rate: &rt})
	case protocol.OpInfo:
		inf := d.info()
		_ = w.send(protocol.Message{Type: protocol.TypeInfo, Info: &inf})
	case protocol.OpStop:
		_ = w.send(protocol.Message{Type: protocol.TypeOK})
		d.log.Info("stop requested")
		d.shutdown(modeStop)
	case protocol.OpRestart:
		inf := d.info()
		_ = w.send(protocol.Message{Type: protocol.TypeOK, Info: &inf})
		d.log.Info("restart requested; handing over to a new daemon", "clients", inf.Clients, "prs", inf.PRs)
		d.shutdown(modeRestart)
	default:
		_ = w.send(protocol.Message{Type: protocol.TypeError, Code: protocol.CodeBadRequest, Message: "unknown op " + req.Op})
	}
}

func (d *Daemon) serveSubscription(conn net.Conn, r *bufio.Reader, w connWriter, req protocol.Request) {
	var refs []prref.Ref
	seen := map[string]bool{}
	for _, p := range req.PRs {
		ref, err := prref.Parse(p, nil)
		if err != nil {
			_ = w.send(protocol.Message{Type: protocol.TypeError, Code: protocol.CodeBadRequest, Message: err.Error()})
			return
		}
		if !seen[ref.Key()] {
			seen[ref.Key()] = true
			refs = append(refs, ref)
		}
	}
	if len(refs) == 0 && req.Op != protocol.OpEvents {
		_ = w.send(protocol.Message{Type: protocol.TypeError, Code: protocol.CodeBadRequest, Message: "no PRs given"})
		return
	}
	s := &sub{
		op:        req.Op,
		forCond:   req.For,
		all:       len(refs) == 0,
		keepAlive: true,
		got:       map[string]bool{},
		// Sized so that initial deliveries never overflow.
		out: make(chan protocol.Message, 64+4*len(refs)),
	}
	if len(refs) == 0 {
		s.out = make(chan protocol.Message, 1024)
	}
	if s.op == protocol.OpEvents {
		s.forCond = "events"
	} else if s.forCond == "" && s.op == protocol.OpWait {
		s.forCond = snapshot.ForChange
	}
	if !d.register(s, refs) {
		// The request raced shutdown: whatever the reason, it never got a
		// daemon, so it reconnects (and starts a new one).
		_ = w.send(protocol.Message{Type: protocol.TypeShutdown, Code: protocol.ShutdownStopping})
		return
	}
	defer d.unregister(s)

	gone := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, r)
		close(gone)
	}()
	pending := map[string]bool{}
	for _, ref := range refs {
		pending[ref.Key()] = true
	}
	for {
		select {
		case m := <-s.out:
			if err := w.send(m); err != nil {
				return
			}
			if s.op == protocol.OpStatus && (m.Type == protocol.TypeSnapshot || m.Type == protocol.TypeError) {
				if m.PR == "" { // a daemon-wide error ends the request
					return
				}
				delete(pending, strings.ToLower(m.PR))
				if len(pending) == 0 {
					_ = w.send(protocol.Message{Type: protocol.TypeEnd})
					return
				}
			}
		case <-gone:
			return
		case <-d.done:
			_ = w.send(protocol.Message{Type: protocol.TypeShutdown, Code: d.shutdownCode()})
			return
		}
	}
}

func (d *Daemon) shutdownCode() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.mode.code()
}

func (s *sub) deliver(log *slog.Logger, m protocol.Message) {
	select {
	case s.out <- m:
	default:
		log.Warn("client is not keeping up; dropping message", "pr", m.PR)
	}
}

func (d *Daemon) register(s *sub, refs []prref.Ref) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.shuttingDown {
		return false
	}
	now := time.Now()
	d.subs[s] = struct{}{}
	if s.keepAlive {
		d.active++
		d.idleGen++ // invalidate any pending idle timer
		if d.idleTimer != nil {
			d.idleTimer.Stop()
		}
	}
	if d.authUntil.After(now) {
		// Fail fast rather than queueing behind the auth back-off.
		s.deliver(d.log, protocol.Message{Type: protocol.TypeError, Code: protocol.CodeAuth, Message: d.authErr})
		return true
	}
	blockedUntil := d.blockedUntilLocked()
	blocked := blockedUntil.After(now)
	needKick := false
	for _, ref := range refs {
		key := ref.Key()
		w := d.watches[key]
		if w == nil {
			w = &watch{ref: ref, key: key, nodeID: d.ids[key], subs: map[*sub]struct{}{}}
			d.watches[key] = w
		}
		if len(w.subs) == 0 {
			w.watchedSince = now
		}
		w.subs[s] = struct{}{}
		s.keys = append(s.keys, key)
		if w.snap != nil && now.Sub(w.lastFetched) < d.interval {
			s.got[key] = true
			s.deliver(d.log, protocol.Message{Type: protocol.TypeSnapshot, PR: w.ref.String(), Snapshot: w.snap, Changes: []string{"initial"}})
			continue
		}
		if s.op == protocol.OpStatus && blocked {
			// status is one-shot: during back-off serve what we have
			// rather than block until GitHub can be asked again.
			s.got[key] = true
			if w.snap != nil {
				s.deliver(d.log, protocol.Message{Type: protocol.TypeSnapshot, PR: w.ref.String(), Snapshot: w.snap, Changes: []string{"initial"}})
			} else {
				s.deliver(d.log, protocol.Message{Type: protocol.TypeError, PR: w.ref.String(), Code: protocol.CodeRateLimited,
					Message: fmt.Sprintf("%s: not cached and requests are backed off until %s", w.ref, blockedUntil.Local().Format(time.Kitchen))})
			}
			continue
		}
		if d.pendingSince.IsZero() {
			d.pendingSince = now
		}
		w.needsRefresh = true
		needKick = true
	}
	if s.all {
		for _, w := range d.watches {
			if len(w.subs) > 0 && w.snap != nil {
				s.deliver(d.log, protocol.Message{Type: protocol.TypeSnapshot, PR: w.ref.String(), Snapshot: w.snap, Changes: []string{"initial"}})
			}
		}
	}
	d.log.Debug("client registered", "op", s.op, "for", s.forCond, "prs", len(refs), "active", d.active)
	if needKick {
		d.kickLocked()
	}
	return true
}

func (d *Daemon) unregister(s *sub) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.subs, s)
	for _, key := range s.keys {
		if w := d.watches[key]; w != nil {
			delete(w.subs, s)
		}
	}
	if s.keepAlive {
		d.active--
		if d.active == 0 && !d.shuttingDown {
			d.startIdleTimerLocked()
		}
	}
	d.log.Debug("client gone", "op", s.op, "active", d.active)
	d.kickLocked() // the interval may have changed
}

func (d *Daemon) kickLocked() {
	select {
	case d.kick <- struct{}{}:
	default:
	}
}

func (d *Daemon) activeWatchesLocked() []*watch {
	var out []*watch
	for _, w := range d.watches {
		if len(w.subs) > 0 {
			out = append(out, w)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out
}

func (d *Daemon) baseIntervalLocked(active []*watch) time.Duration {
	base := d.cfg.SlowInterval
	for _, w := range active {
		s := w.snap
		if s == nil || s.State != "OPEN" {
			continue
		}
		if !snapshot.ChecksSettled(s) || s.AutoMerge.Enabled || s.Mergeable == "UNKNOWN" {
			base = d.cfg.FastInterval
			break
		}
	}
	if base < d.cfg.MinInterval {
		base = d.cfg.MinInterval
	}
	return base
}

// nextWakeLocked returns when the poller should next run a round, or false
// when there is nothing to poll.
func (d *Daemon) nextWakeLocked() (time.Time, bool) {
	active := d.activeWatchesLocked()
	if len(active) == 0 {
		return time.Time{}, false
	}
	d.interval = d.gov.Interval(d.baseIntervalLocked(active))
	gap := d.requestGapLocked()
	at := d.lastRound.Add(d.interval)
	if g := d.lastRequest.Add(gap); g.After(at) {
		at = g
	}
	for _, w := range active {
		if w.nodeID == "" || w.needsRefresh {
			p := d.pendingSince.Add(d.cfg.Debounce)
			if g := d.lastRequest.Add(gap); g.After(p) {
				p = g
			}
			if p.Before(at) {
				at = p
			}
			break
		}
	}
	if bo := d.blockedUntilLocked(); bo.After(at) {
		at = bo
	}
	return at, true
}

// requestGapLocked is the minimum spacing between any two requests,
// scheduled or on demand: never below the poll floor, and stretched by the
// budget so new interest cannot outspend the budget share.
func (d *Daemon) requestGapLocked() time.Duration {
	return max(d.cfg.MinGap, d.cfg.MinInterval, d.gov.Interval(0))
}

// blockedUntilLocked returns the time before which no request may be sent:
// a rate-limit back-off, a budget below the reserve, or a transient error
// back-off, whichever ends last.
func (d *Daemon) blockedUntilLocked() time.Time {
	until := d.gov.BackoffUntil()
	if r := d.gov.ReserveUntil(); r.After(until) {
		until = r
	}
	if d.transientEnd.After(until) {
		until = d.transientEnd
	}
	return until
}

// budgetBlocked reports whether the governor forbids a request now, through
// a back-off or a budget below the reserve.
func (d *Daemon) budgetBlocked() bool {
	if blocked, _ := d.gov.Blocked(); blocked {
		return true
	}
	return time.Now().Before(d.gov.ReserveUntil())
}

func (d *Daemon) pollLoop() {
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	for {
		d.mu.Lock()
		at, ok := d.nextWakeLocked()
		d.mu.Unlock()
		var fire <-chan time.Time
		if ok {
			timer.Reset(time.Until(at))
			fire = timer.C
		}
		select {
		case <-d.done:
			timer.Stop()
			return
		case <-d.kick:
			if !timer.Stop() && ok {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-fire:
			d.round()
		}
	}
}

func (d *Daemon) round() {
	d.mu.Lock()
	start := time.Now()
	active := d.activeWatchesLocked()
	full := !start.Before(d.lastRound.Add(d.interval))
	var unresolved []prref.Ref
	var targets []github.Target
	for _, w := range active {
		switch {
		case w.nodeID == "":
			unresolved = append(unresolved, w.ref)
		case full || w.needsRefresh:
			targets = append(targets, github.Target{Ref: w.ref, NodeID: w.nodeID})
		}
	}
	d.pendingSince = time.Time{}
	d.mu.Unlock()
	if len(unresolved) == 0 && len(targets) == 0 {
		return
	}
	if d.budgetBlocked() {
		return
	}

	ctx, cancel := context.WithTimeout(d.ctx, 60*time.Second)
	defer cancel()
	// Checked before every request: a response in this round may have
	// exhausted the budget, or the daemon may be shutting down.
	allowed := func() bool {
		return ctx.Err() == nil && !d.budgetBlocked()
	}
	var (
		results  []github.Result
		requests int
		cost     int
		failed   bool
	)
	send := func(f func() ([]github.Result, github.RateInfo, error)) bool {
		if !allowed() {
			return false
		}
		res, rate, err := f()
		requests++
		d.mu.Lock()
		d.lastRequest = time.Now()
		d.requests++
		d.mu.Unlock()
		if err != nil {
			d.handleError(err)
			return false
		}
		d.gov.Observe(rate)
		cost += rate.Cost
		results = append(results, res...)
		return true
	}
	for _, chunk := range chunks(unresolved, github.ChunkSize) {
		if failed = !send(func() ([]github.Result, github.RateInfo, error) { return d.gh.Resolve(ctx, chunk) }); failed {
			break
		}
	}
	if !failed {
		for _, chunk := range chunks(targets, github.ChunkSize) {
			if failed = !send(func() ([]github.Result, github.RateInfo, error) { return d.gh.Poll(ctx, chunk) }); failed {
				break
			}
		}
	}
	var snaps []*snapshot.Snapshot
	for _, r := range results {
		if r.Snapshot != nil {
			snaps = append(snaps, r.Snapshot)
		}
	}
	d.mu.Lock()
	missing := github.MissingThreads(snaps, d.heads)
	d.mu.Unlock()
	if len(missing) > 0 && !failed {
		for _, chunk := range chunks(missing, 100) {
			if !allowed() {
				break
			}
			heads, rate, err := d.gh.ThreadHeads(ctx, chunk)
			requests++
			d.mu.Lock()
			d.lastRequest = time.Now()
			d.requests++
			d.mu.Unlock()
			if err != nil {
				d.handleError(err)
				break
			}
			d.gov.Observe(rate)
			cost += rate.Cost
			d.mu.Lock()
			// A thread refetched after an edit but not returned keeps its old
			// head, re-stamped, rather than being requested every round.
			for _, id := range chunk {
				if _, ok := heads[id]; !ok {
					if old, ok := d.heads[id]; ok {
						heads[id] = old
					}
				}
			}
			d.mu.Unlock()
			github.StampHeads(snaps, heads)
			d.mu.Lock()
			for k, v := range heads {
				d.heads[k] = v
			}
			d.mu.Unlock()
		}
	}
	if !failed {
		d.mu.Lock()
		d.transient = 0
		d.authUntil = time.Time{}
		d.mu.Unlock()
	}
	idsChanged := d.apply(results)
	if idsChanged {
		saveIDs(paths.IDs(d.cfg.StateDir), d.snapshotIDs())
	}

	d.mu.Lock()
	if full && !failed {
		d.lastRound = start
		d.gov.SetRoundCost(cost)
	}
	d.rounds++
	interval := d.gov.Interval(d.baseIntervalLocked(d.activeWatchesLocked()))
	d.interval = interval
	d.mu.Unlock()
	st := d.gov.Snapshot()
	d.log.Info("poll round", "prs", len(unresolved)+len(targets), "resolved", len(unresolved),
		"requests", requests, "cost", cost, "remaining", st.Remaining, "interval", interval, "full", full, "ok", !failed)
}

func chunks[T any](xs []T, n int) [][]T {
	var out [][]T
	for len(xs) > 0 {
		k := min(n, len(xs))
		out = append(out, xs[:k])
		xs = xs[k:]
	}
	return out
}

func (d *Daemon) handleError(err error) {
	var rl *github.RateLimitError
	var ae *github.AuthError
	switch {
	case d.ctx.Err() != nil:
		return // shutting down
	case errors.As(err, &rl):
		until := d.gov.OnRateLimit(rl)
		d.log.Warn("rate limited; backing off", "secondary", rl.Secondary, "retryAfter", rl.RetryAfter,
			"until", until.Format(time.RFC3339), "reason", d.gov.Snapshot().BackoffReason)
	case errors.As(err, &ae):
		d.log.Error("authentication failed", "err", ae.Message)
		d.mu.Lock()
		d.transientEnd = time.Now().Add(time.Minute)
		d.authErr, d.authUntil = ae.Error(), d.transientEnd
		for s := range d.subs {
			s.deliver(d.log, protocol.Message{Type: protocol.TypeError, Code: protocol.CodeAuth, Message: ae.Error()})
		}
		d.mu.Unlock()
	default:
		d.mu.Lock()
		if d.transient == 0 {
			d.transient = 5 * time.Second
		} else {
			d.transient = min(d.transient*2, 5*time.Minute)
		}
		d.transientEnd = time.Now().Add(d.transient)
		delay := d.transient
		d.mu.Unlock()
		d.log.Warn("request failed; retrying later", "err", err, "retryIn", delay)
	}
}

// apply stores results and notifies subscribers. It reports whether the
// node id cache changed.
func (d *Daemon) apply(results []github.Result) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	idsChanged := false
	for _, r := range results {
		key := r.Ref.Key()
		w := d.watches[key]
		if w == nil {
			continue
		}
		var te *github.TransientError
		if errors.As(r.Err, &te) {
			// Leave the watch in place; the next round retries it.
			d.log.Warn("PR fetch failed; will retry", "pr", r.Ref.String(), "err", r.Err)
			w.needsRefresh = false
			continue
		}
		if r.Err != nil {
			code := protocol.CodeNotFound
			var ae *github.AuthError
			if errors.As(r.Err, &ae) {
				code = protocol.CodeAuth
			}
			d.log.Info("PR unavailable", "pr", r.Ref.String(), "code", code, "err", r.Err)
			for s := range w.subs {
				s.got[key] = true
				s.deliver(d.log, protocol.Message{Type: protocol.TypeError, PR: r.Ref.String(), Code: code, Message: r.Err.Error()})
			}
			delete(d.watches, key)
			if _, ok := d.ids[key]; ok {
				delete(d.ids, key)
				idsChanged = true
			}
			continue
		}
		if r.NodeID != "" && w.nodeID != r.NodeID {
			w.nodeID = r.NodeID
			d.ids[key] = r.NodeID
			idsChanged = true
		}
		github.FillThreads(r.Snapshot, d.heads)
		prev := w.snap
		cur := r.Snapshot
		if cur.Incomplete {
			d.applyIncompleteLocked(w, cur)
			continue
		}
		w.snap = cur
		w.lastFetched = now
		w.needsRefresh = false
		changed := prev == nil || prev.Token != cur.Token
		var changes []string
		if changed {
			changes = snapshot.Changes(prev, cur)
			d.log.Debug("PR changed", "pr", cur.PR, "changes", changes)
		}
		for s := range w.subs {
			switch {
			case !s.got[key]:
				s.got[key] = true
				s.deliver(d.log, protocol.Message{Type: protocol.TypeSnapshot, PR: w.ref.String(), Snapshot: cur, Changes: []string{"initial"}})
			case changed:
				s.deliver(d.log, protocol.Message{Type: protocol.TypeSnapshot, PR: w.ref.String(), Snapshot: cur, Changes: changes})
			}
		}
		if changed {
			for s := range d.subs {
				if s.all {
					s.deliver(d.log, protocol.Message{Type: protocol.TypeSnapshot, PR: w.ref.String(), Snapshot: cur, Changes: changes})
				}
			}
		}
	}
	return idsChanged
}

// applyIncompleteLocked handles a PR that GitHub returned with part of its
// data nulled by an error. The partial snapshot is never stored, never
// compared and never sent to waiters or event streams, so it cannot satisfy
// a condition or produce a change event. The previous good snapshot stays
// in place and the next round fetches the PR again. One-shot status
// requests still need an answer: they get the previous good snapshot, or
// the partial one, flagged incomplete, when there is nothing better.
func (d *Daemon) applyIncompleteLocked(w *watch, cur *snapshot.Snapshot) {
	d.log.Warn("PR returned incomplete; keeping the previous snapshot", "pr", w.ref.String(),
		"havePrevious", w.snap != nil, "reason", cur.IncompleteReason)
	w.needsRefresh = false
	reply := w.snap
	if reply == nil {
		reply = cur
	}
	for s := range w.subs {
		if s.op != protocol.OpStatus || s.got[w.key] {
			continue
		}
		s.got[w.key] = true
		s.deliver(d.log, protocol.Message{Type: protocol.TypeSnapshot, PR: w.ref.String(), Snapshot: reply, Changes: []string{"initial"}})
	}
}

func (d *Daemon) snapshotIDs() map[string]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]string, len(d.ids))
	for k, v := range d.ids {
		out[k] = v
	}
	return out
}

func (d *Daemon) rateLocked() protocol.Rate {
	st := d.gov.Snapshot()
	r := protocol.Rate{
		Limit: st.Limit, Remaining: st.Remaining, LastCost: st.LastCost, RoundCost: st.RoundCost,
		Interval: d.interval.String(), BackoffReason: st.BackoffReason, Rounds: d.rounds, Requests: d.requests,
	}
	if !st.ResetAt.IsZero() {
		t := st.ResetAt
		r.ResetAt = &t
	}
	if st.BackoffUntil.After(time.Now()) {
		t := st.BackoffUntil
		r.BackoffUntil = &t
	} else {
		r.BackoffReason = ""
	}
	if d.transientEnd.After(time.Now()) {
		t := d.transientEnd
		r.BackoffUntil = &t
		r.BackoffReason = "request errors"
	}
	if !d.lastRound.IsZero() {
		t := d.lastRound
		r.LastRound = &t
	}
	return r
}

func (d *Daemon) rate() protocol.Rate {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.rateLocked()
}

func (d *Daemon) list() protocol.List {
	d.mu.Lock()
	defer d.mu.Unlock()
	l := protocol.List{PRs: []protocol.Watched{}, Rate: d.rateLocked(), DaemonPID: os.Getpid(), DaemonVersion: d.cfg.Version}
	for s := range d.subs {
		if s.all {
			l.AllStreams++
		}
	}
	for _, w := range d.activeWatchesLocked() {
		item := protocol.Watched{PR: w.ref.String(), WatchedSince: w.watchedSince, Snapshot: w.snap, For: []string{}}
		if w.snap != nil {
			item.PR, item.Title = w.snap.PR, w.snap.Title
		}
		if !w.lastFetched.IsZero() {
			t := w.lastFetched
			item.LastPoll = &t
		}
		conds := map[string]bool{}
		for s := range w.subs {
			if s.op == protocol.OpEvents {
				item.Streams++
			} else {
				item.Waiters++
			}
			if s.forCond != "" {
				conds[s.forCond] = true
			}
		}
		for c := range conds {
			item.For = append(item.For, c)
		}
		sort.Strings(item.For)
		l.PRs = append(l.PRs, item)
	}
	return l
}

func (d *Daemon) info() protocol.Info {
	d.mu.Lock()
	defer d.mu.Unlock()
	return protocol.Info{
		PID: os.Getpid(), Version: d.cfg.Version, StartedAt: d.startedAt,
		Socket: paths.Socket(d.cfg.StateDir), Log: paths.Log(d.cfg.StateDir),
		Clients: d.active, PRs: len(d.activeWatchesLocked()),
	}
}

func loadIDs(path string) map[string]string {
	ids := map[string]string{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &ids)
	}
	return ids
}

func saveIDs(path string, ids map[string]string) {
	b, err := json.Marshal(ids)
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
}
