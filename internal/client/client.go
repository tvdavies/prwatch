// Package client connects to the prwatch daemon, starting it if needed.
package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tvdavies/prwatch/internal/paths"
	"github.com/tvdavies/prwatch/internal/protocol"
)

// ErrNoDaemon is returned by Dial with spawn=false when no daemon is running.
var ErrNoDaemon = errors.New("no prwatch daemon is running")

// ErrDeadline is returned by DialUntil when the caller's deadline passes.
var ErrDeadline = errors.New("deadline reached while connecting to the daemon")

// Conn is a connection to the daemon.
type Conn struct {
	c     net.Conn
	r     *bufio.Reader
	Hello protocol.Hello
}

// Dial connects to the daemon for stateDir. With spawn, it starts a
// detached daemon when none is listening and retries with a short back-off.
func Dial(stateDir string, spawn bool) (*Conn, error) {
	return DialUntil(stateDir, spawn, time.Time{})
}

// SpawnError is returned by DialUntil when it could not start a daemon at
// all: no usable binary was found, or starting it failed.
type SpawnError struct{ Err error }

func (e *SpawnError) Error() string { return "could not start a daemon: " + e.Err.Error() }
func (e *SpawnError) Unwrap() error { return e.Err }

// DialUntil is Dial with a caller deadline (zero for none). Connecting gives
// up after 10s regardless.
//
// When several clients find no daemon at once, each starts one; the daemon
// that takes the lock and binds the socket wins, the others exit, and every
// client connects to the winner. A client starts another daemon only once
// the one it started has exited without anyone listening (it lost the race
// to a daemon that was still shutting down, or it died at start-up), at
// growing intervals, so a crowd of reconnecting clients cannot stampede.
func DialUntil(stateDir string, spawn bool, callerDeadline time.Time) (*Conn, error) {
	sock := paths.Socket(stateDir)
	deadline := time.Now().Add(10 * time.Second)
	if !callerDeadline.IsZero() && callerDeadline.Before(deadline) {
		deadline = callerDeadline
	}
	delay := 10 * time.Millisecond
	var lastSpawn time.Time
	spawnGap := 500 * time.Millisecond
	var lastErr error
	for {
		c, err := tryDial(sock, deadline)
		if err == nil {
			return c, nil
		}
		lastErr = err
		if !spawn {
			if isNoListener(err) {
				return nil, ErrNoDaemon
			}
			return nil, err
		}
		if time.Now().After(deadline) {
			if deadline.Equal(callerDeadline) {
				return nil, ErrDeadline
			}
			return nil, fmt.Errorf("could not reach the prwatch daemon (see %s): %w", paths.Log(stateDir), lastErr)
		}
		// While the daemon we started is alive it is either starting up or
		// waiting to see whether another one wins, so don't start more.
		// That holds across calls: a stream that retries does not pile up
		// daemons that never came up.
		if exited(liveChild()) && time.Since(lastSpawn) > spawnGap {
			if !lastSpawn.IsZero() {
				spawnGap = min(spawnGap*2, 4*time.Second)
			}
			if err := Spawn(stateDir, deadline); err != nil {
				return nil, &SpawnError{err}
			}
			lastSpawn = time.Now()
		}
		time.Sleep(delay)
		delay = min(delay*2, 200*time.Millisecond)
	}
}

func tryDial(sock string, deadline time.Time) (*Conn, error) {
	c, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		return nil, err
	}
	conn := &Conn{c: c, r: bufio.NewReaderSize(c, 64<<10)}
	helloBy := time.Now().Add(5 * time.Second)
	if !deadline.IsZero() && deadline.Before(helloBy) {
		helloBy = deadline
	}
	_ = c.SetReadDeadline(helloBy)
	line, err := conn.r.ReadBytes('\n')
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("daemon closed the connection: %w", err)
	}
	_ = c.SetReadDeadline(time.Time{})
	if err := json.Unmarshal(line, &conn.Hello); err != nil || conn.Hello.Type != "hello" {
		c.Close()
		return nil, errors.New("unexpected greeting from daemon")
	}
	return conn, nil
}

func isNoListener(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
}

// child is closed when the last daemon this process started exits.
var (
	childMu sync.Mutex
	child   <-chan struct{}
)

func liveChild() <-chan struct{} {
	childMu.Lock()
	defer childMu.Unlock()
	return child
}

func exited(child <-chan struct{}) bool {
	if child == nil {
		return true
	}
	select {
	case <-child:
		return true
	default:
		return false
	}
}

// Spawn starts a detached daemon from the binary ResolveExecutable picks,
// with the hidden __daemon subcommand, in a new session, with output to the
// log file. It logs the binary it chose, and why it skipped any before it,
// to the daemon log. Checking candidates stops at the deadline (zero for
// none), and nothing is started after it.
func Spawn(stateDir string, deadline time.Time) error {
	ctx := context.Background()
	if !deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	logPath := paths.Log(stateDir)
	rotateLog(logPath)
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	log := slog.New(slog.NewTextHandler(logf, nil))
	bin, err := ResolveExecutable(ctx)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		log.Warn("cannot start daemon", "client", os.Getpid(), "err", err)
		return err
	}
	attrs := []any{"exe", bin.Path, "via", bin.Via, "version", bin.Version, "client", os.Getpid()}
	if len(bin.Skipped) > 0 {
		attrs = append(attrs, "skipped", strings.Join(bin.Skipped, "; "))
	}
	log.Info("starting daemon", attrs...)
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer devnull.Close()
	cmd := exec.Command(bin.Path, "__daemon")
	cmd.Stdin = devnull
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.Dir = "/"
	cmd.Env = append(os.Environ(), "PRWATCH_STATE_DIR="+stateDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		log.Warn("cannot start daemon", "exe", bin.Path, "client", os.Getpid(), "err", err)
		return err
	}
	// Reap the child if it exits while we are still running (for example
	// when it lost the start-up race).
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	childMu.Lock()
	child = done
	childMu.Unlock()
	return nil
}

func rotateLog(path string) {
	if fi, err := os.Stat(path); err == nil && fi.Size() > 5<<20 {
		_ = os.Rename(path, path+".1")
	}
}

// Send writes one request line.
func (c *Conn) Send(req protocol.Request) error {
	req.Protocol = protocol.Version
	b, err := protocol.Encode(req)
	if err != nil {
		return err
	}
	_, err = c.c.Write(b)
	return err
}

// Recv reads one message line.
func (c *Conn) Recv() (protocol.Message, error) {
	var m protocol.Message
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(line, &m); err != nil {
		return m, fmt.Errorf("decode daemon message: %w", err)
	}
	return m, nil
}

// SetReadDeadline bounds subsequent Recv calls.
func (c *Conn) SetReadDeadline(t time.Time) error { return c.c.SetReadDeadline(t) }

// Close closes the connection, dropping any interest it registered.
func (c *Conn) Close() error { return c.c.Close() }

// Request is a convenience for one-shot ops (list, rate, info, stop,
// restart). It also returns the daemon's greeting, which carries its
// version and pid.
func Request(stateDir string, op string) (protocol.Message, protocol.Hello, error) {
	c, err := Dial(stateDir, false)
	if err != nil {
		return protocol.Message{}, protocol.Hello{}, err
	}
	defer c.Close()
	if err := c.Send(protocol.Request{Op: op}); err != nil {
		return protocol.Message{}, c.Hello, err
	}
	_ = c.c.SetReadDeadline(time.Now().Add(10 * time.Second))
	m, err := c.Recv()
	return m, c.Hello, err
}
