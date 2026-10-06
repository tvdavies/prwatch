// Package prref parses pull request references such as owner/repo#123,
// GitHub pull request URLs, and bare numbers resolved against a git remote.
package prref

import (
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// Ref identifies a pull request.
type Ref struct {
	Owner  string
	Repo   string
	Number int
}

// String returns owner/repo#number.
func (r Ref) String() string { return fmt.Sprintf("%s/%s#%d", r.Owner, r.Repo, r.Number) }

// Key is the case-insensitive identity used for de-duplication.
func (r Ref) Key() string { return strings.ToLower(r.String()) }

var (
	ownerRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?$`)
	repoRe  = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	fullRe  = regexp.MustCompile(`^([^/\s#]+)/([^/\s#]+)#(\d+)$`)
	numRe   = regexp.MustCompile(`^#?(\d+)$`)
)

// ErrNoRemote is returned when a bare number cannot be resolved from git.
var ErrNoRemote = errors.New("not inside a git checkout with a GitHub origin remote")

// RemoteFunc returns the URL of the origin remote. It is swappable in tests.
type RemoteFunc func() (string, error)

// GitOrigin runs `git remote get-url origin` in the current directory.
func GitOrigin() (string, error) {
	out, err := exec.Command("git", "remote", "get-url", "origin").Output()
	if err != nil {
		return "", ErrNoRemote
	}
	return strings.TrimSpace(string(out)), nil
}

// Parse parses a PR reference. remote is consulted only for bare numbers.
func Parse(s string, remote RemoteFunc) (Ref, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Ref{}, errors.New("empty PR reference")
	}
	if m := fullRe.FindStringSubmatch(s); m != nil {
		return build(m[1], m[2], m[3], s)
	}
	if m := numRe.FindStringSubmatch(s); m != nil {
		if remote == nil {
			return Ref{}, ErrNoRemote
		}
		u, err := remote()
		if err != nil {
			return Ref{}, err
		}
		owner, repo, ok := ParseRemote(u)
		if !ok {
			return Ref{}, fmt.Errorf("origin remote %q is not a GitHub repository", u)
		}
		return build(owner, repo, m[1], s)
	}
	if strings.Contains(s, "://") || strings.HasPrefix(s, "github.com/") {
		return parseURL(s)
	}
	return Ref{}, fmt.Errorf("unrecognised PR reference %q (use owner/repo#123, a PR URL, or a number inside a checkout)", s)
}

func parseURL(s string) (Ref, error) {
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || !strings.EqualFold(u.Hostname(), "github.com") {
		return Ref{}, fmt.Errorf("unrecognised PR URL %q", s)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 4 || (parts[2] != "pull" && parts[2] != "pulls") {
		return Ref{}, fmt.Errorf("unrecognised PR URL %q", s)
	}
	return build(parts[0], parts[1], parts[3], s)
}

func build(owner, repo, num, orig string) (Ref, error) {
	repo = strings.TrimSuffix(repo, ".git")
	if !ownerRe.MatchString(owner) || !repoRe.MatchString(repo) {
		return Ref{}, fmt.Errorf("invalid owner or repository in %q", orig)
	}
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		return Ref{}, fmt.Errorf("invalid PR number in %q", orig)
	}
	return Ref{Owner: owner, Repo: repo, Number: n}, nil
}

// ParseRemote extracts owner and repo from a GitHub remote URL in https,
// ssh:// or scp-like (git@github.com:owner/repo.git) form.
func ParseRemote(u string) (owner, repo string, ok bool) {
	u = strings.TrimSpace(u)
	var path string
	switch {
	case strings.Contains(u, "://"):
		pu, err := url.Parse(u)
		if err != nil || !strings.EqualFold(pu.Hostname(), "github.com") {
			return "", "", false
		}
		path = pu.Path
	case strings.Contains(u, ":"):
		i := strings.Index(u, ":")
		host := u[:i]
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:]
		}
		if !strings.EqualFold(host, "github.com") {
			return "", "", false
		}
		path = u[i+1:]
	default:
		return "", "", false
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 2 {
		return "", "", false
	}
	owner, repo = parts[0], strings.TrimSuffix(parts[1], ".git")
	if !ownerRe.MatchString(owner) || !repoRe.MatchString(repo) {
		return "", "", false
	}
	return owner, repo, true
}
