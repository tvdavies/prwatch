// Package protocol defines the JSON-lines protocol between prwatch clients
// and the daemon over the per-user unix socket.
//
// On connect the daemon sends a Hello line. The client sends one Request
// line. For wait, events and status the connection then carries Message
// lines from the daemon; the client's open connection is its interest
// registration, so closing it drops the interest.
//
// Compatibility: the protocol stays at version 1 while changes are
// additive. Clients ignore message fields and shutdown codes they do not
// know, and treat any shutdown code other than "stopped" as a cue to
// reconnect. Daemons answer an op they do not know with a bad_request error
// whose message starts "unknown op", which a newer client can detect.
package protocol

import (
	"encoding/json"
	"time"

	"github.com/tvdavies/prwatch/internal/snapshot"
)

// Version is the protocol version.
const Version = 1

// Operations.
const (
	OpWait   = "wait"
	OpEvents = "events"
	OpStatus = "status"
	OpList   = "list"
	OpRate   = "rate"
	OpInfo   = "info"
	OpStop   = "stop"
	// OpRestart asks the daemon to hand over gracefully: it replies OK with
	// its Info, then shuts down without the "stopped" code, so wait and
	// events clients reconnect and the first of them starts a fresh daemon.
	// Added in 0.1.2; older daemons reply "unknown op restart".
	OpRestart = "restart"
)

// Shutdown codes, sent in a TypeShutdown message's Code.
const (
	// ShutdownStopped is an explicit stop (prwatch daemon stop, SIGTERM or
	// SIGINT). Clients report it and exit rather than reconnecting.
	ShutdownStopped = "stopped"
	// ShutdownStopping is an idle exit or a request that raced shutdown.
	// Clients reconnect, starting a new daemon if needed.
	ShutdownStopping = "stopping"
	// ShutdownRestarting is a graceful restart (prwatch daemon restart or
	// SIGHUP). Clients reconnect; 0.1.1 clients treat it like "stopping".
	ShutdownRestarting = "restarting"
)

// Hello is the daemon's first line.
type Hello struct {
	Type     string `json:"type"` // "hello"
	Protocol int    `json:"protocol"`
	Version  string `json:"version"`
	PID      int    `json:"pid"`
}

// Request is the client's single request line.
type Request struct {
	Protocol int      `json:"protocol"`
	Op       string   `json:"op"`
	PRs      []string `json:"prs,omitempty"`
	For      string   `json:"for,omitempty"`
}

// Message types sent by the daemon.
const (
	TypeSnapshot = "snapshot"
	TypeError    = "error"
	TypeEnd      = "end"
	TypeShutdown = "shutdown"
	TypeList     = "list"
	TypeRate     = "rate"
	TypeInfo     = "info"
	TypeOK       = "ok"
)

// Error codes.
const (
	CodeNotFound    = "not_found"
	CodeAuth        = "auth"
	CodeBadRequest  = "bad_request"
	CodeRateLimited = "rate_limited"
	CodeInternal    = "internal"
)

// Message is one daemon-to-client line.
type Message struct {
	Type     string             `json:"type"`
	PR       string             `json:"pr,omitempty"`
	Snapshot *snapshot.Snapshot `json:"snapshot,omitempty"`
	Changes  []string           `json:"changes,omitempty"`
	Code     string             `json:"code,omitempty"`
	Message  string             `json:"message,omitempty"`
	List     *List              `json:"list,omitempty"`
	Rate     *Rate              `json:"rate,omitempty"`
	Info     *Info              `json:"info,omitempty"`
}

// Watched is one PR in a list response.
type Watched struct {
	PR           string             `json:"pr"`
	Title        string             `json:"title"`
	Waiters      int                `json:"waiters"`
	Streams      int                `json:"streams"`
	For          []string           `json:"for"`
	WatchedSince time.Time          `json:"watchedSince"`
	LastPoll     *time.Time         `json:"lastPoll"`
	Snapshot     *snapshot.Snapshot `json:"snapshot"`
}

// List is the list response.
type List struct {
	PRs           []Watched `json:"prs"`
	AllStreams    int       `json:"allStreams"`
	Rate          Rate      `json:"rate"`
	DaemonPID     int       `json:"daemonPid"`
	DaemonVersion string    `json:"daemonVersion"`
}

// Rate describes the daemon's view of the budget.
type Rate struct {
	Limit         int        `json:"limit"`
	Remaining     int        `json:"remaining"`
	ResetAt       *time.Time `json:"resetAt"`
	LastCost      int        `json:"lastCost"`
	RoundCost     int        `json:"roundCost"`
	Interval      string     `json:"interval"`
	BackoffUntil  *time.Time `json:"backoffUntil"`
	BackoffReason string     `json:"backoffReason,omitempty"`
	LastRound     *time.Time `json:"lastRound"`
	Rounds        int        `json:"rounds"`
	Requests      int        `json:"requests"`
}

// Info describes the daemon itself.
type Info struct {
	PID       int       `json:"pid"`
	Version   string    `json:"version"`
	StartedAt time.Time `json:"startedAt"`
	Socket    string    `json:"socket"`
	Log       string    `json:"log"`
	Clients   int       `json:"clients"`
	PRs       int       `json:"prs"`
}

// BinaryInfo is what `prwatch version --json` prints (0.1.3 and later).
// Clients run it on a binary before starting a daemon from it, to check that
// it speaks their protocol.
type BinaryInfo struct {
	Version  string `json:"version"`
	Protocol int    `json:"protocol"`
	OS       string `json:"os,omitempty"`
	Arch     string `json:"arch,omitempty"`
	Go       string `json:"go,omitempty"`
}

// Encode marshals v as one JSON line.
func Encode(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
