// Package client connects to the prwatch daemon, starting it if needed.
package client

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
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

// DialUntil is Dial with a caller deadline (zero for none). Connecting gives
// up after 10s regardless.
func DialUntil(stateDir string, spawn bool, callerDeadline time.Time) (*Conn, error) {
	sock := paths.Socket(stateDir)
	deadline := time.Now().Add(10 * time.Second)
	if !callerDeadline.IsZero() && callerDeadline.Before(deadline) {
		deadline = callerDeadline
	}
	delay := 10 * time.Millisecond
	var lastSpawn time.Time
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
		// Spawn at most every 500ms: a daemon that lost the start-up race
		// exits at once, and one that is shutting down needs a moment.
		if time.Since(lastSpawn) > 500*time.Millisecond {
			if err := Spawn(stateDir); err != nil {
				return nil, fmt.Errorf("start daemon: %w", err)
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

// Spawn starts a detached daemon: the same executable with the hidden
// __daemon subcommand, in a new session, with output to the log file.
func Spawn(stateDir string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logPath := paths.Log(stateDir)
	rotateLog(logPath)
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer devnull.Close()
	cmd := exec.Command(exe, "__daemon")
	cmd.Stdin = devnull
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.Dir = "/"
	cmd.Env = append(os.Environ(), "PRWATCH_STATE_DIR="+stateDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reap the child if it exits while we are still running (for example
	// when it lost the start-up race).
	go func() { _ = cmd.Wait() }()
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

// Request is a convenience for one-shot ops (list, rate, info, stop).
func Request(stateDir string, op string) (protocol.Message, error) {
	c, err := Dial(stateDir, false)
	if err != nil {
		return protocol.Message{}, err
	}
	defer c.Close()
	if err := c.Send(protocol.Request{Op: op}); err != nil {
		return protocol.Message{}, err
	}
	_ = c.c.SetReadDeadline(time.Now().Add(10 * time.Second))
	return c.Recv()
}
