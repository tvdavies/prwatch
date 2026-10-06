// Package fakegh is an httptest fake of the parts of GitHub's GraphQL API
// that prwatch uses. It understands exactly the query shapes that
// internal/github generates.
package fakegh

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tvdavies/prwatch/internal/github"
)

// Request records one request the fake received.
type Request struct {
	Kind   string // resolve, poll or threads
	PRs    []string
	At     time.Time
	Status int
}

// Response is an injected canned response.
type Response struct {
	Status  int
	Headers map[string]string
	Body    string
}

// Server is the fake.
type Server struct {
	*httptest.Server

	mu        sync.Mutex
	prs       map[string]*github.RawPR // by lower-case owner/repo#n
	byID      map[string]string        // node id -> key
	heads     map[string]github.ThreadHead
	log       []Request
	inject    []Response
	delay     time.Duration
	limit     int
	remaining int
	cost      int
	nextID    int
	nested    map[string]int // PR key -> responses left with commits nulled by an error
}

// New starts a fake server.
func New() *Server {
	s := &Server{prs: map[string]*github.RawPR{}, byID: map[string]string{}, heads: map[string]github.ThreadHead{}, nested: map[string]int{}, limit: 1000000, remaining: 1000000, cost: 1}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// AddPR adds an open, green, mergeable PR with no review requirement.
func (s *Server) AddPR(owner, repo string, number int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	key := strings.ToLower(fmt.Sprintf("%s/%s#%d", owner, repo, number))
	p := &github.RawPR{
		ID:               fmt.Sprintf("PR_fake%d", s.nextID),
		Number:           number,
		URL:              fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, number),
		Title:            fmt.Sprintf("Fake PR %d", number),
		State:            "OPEN",
		HeadRefOid:       "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BaseRefName:      "main",
		Mergeable:        "MERGEABLE",
		MergeStateStatus: "CLEAN",
		UpdatedAt:        time.Now().UTC().Truncate(time.Second),
	}
	p.Repository.NameWithOwner = owner + "/" + repo
	node := github.RawCommitNode{}
	node.Commit.Oid = p.HeadRefOid
	node.Commit.StatusCheckRollup = &github.RawRollup{State: "SUCCESS"}
	req := true
	node.Commit.StatusCheckRollup.Contexts.TotalCount = 1
	node.Commit.StatusCheckRollup.Contexts.Nodes = []github.RawContext{{Typename: "CheckRun", Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS", IsRequired: &req}}
	p.Commits.Nodes = []github.RawCommitNode{node}
	s.prs[key] = p
	s.byID[p.ID] = key
	return key
}

// Update mutates a PR under the lock.
func (s *Server) Update(key string, f func(p *github.RawPR)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.prs[strings.ToLower(key)]
	f(p)
	p.UpdatedAt = time.Now().UTC().Truncate(time.Second)
}

// SetChecks sets the rollup state and the single check's status.
func SetChecks(p *github.RawPR, state string) {
	r := p.Commits.Nodes[0].Commit.StatusCheckRollup
	r.State = state
	c := &r.Contexts.Nodes[0]
	switch state {
	case "PENDING":
		c.Status, c.Conclusion = "IN_PROGRESS", ""
	default:
		c.Status, c.Conclusion = "COMPLETED", state
	}
}

// FailCommits makes the next n responses that include the PR return it
// with commits nulled and a GraphQL error beneath the PR's alias, as GitHub
// does when a nested field fails.
func (s *Server) FailCommits(key string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nested[strings.ToLower(key)] = n
}

// prData returns the response value for a PR at path, applying any nested
// failure.
func (s *Server) prData(key string, p *github.RawPR, path []any, errs *[]map[string]any) any {
	if s.nested[key] <= 0 {
		return p
	}
	s.nested[key]--
	b, _ := json.Marshal(p)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	m["commits"] = nil
	*errs = append(*errs, map[string]any{"type": "INTERNAL", "path": append(path, "commits"),
		"message": "Something went wrong while executing your query."})
	return m
}

// SetThreadHead sets the first comment the fake returns for a thread.
func (s *Server) SetThreadHead(id, author, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.heads[id] = github.ThreadHead{Author: author, Excerpt: body}
}

// Inject queues canned responses for the next requests.
func (s *Server) Inject(r ...Response) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inject = append(s.inject, r...)
}

// Requests returns a copy of the request log.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.log...)
}

// SetBudget sets the primary rate limit and remaining points.
func (s *Server) SetBudget(limit, remaining int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.limit, s.remaining = limit, remaining
}

// SetDelay delays every response by d.
func (s *Server) SetDelay(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delay = d
}

// SetCost sets the cost reported per request.
func (s *Server) SetCost(c int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cost = c
}

var (
	repoRe = regexp.MustCompile(`^\s*(r\d+): repository\(owner: ("(?:[^"\\]|\\.)*"), name: ("(?:[^"\\]|\\.)*")\) \{`)
	prRe   = regexp.MustCompile(`^\s*(q\d+): pullRequest\(number: (\d+)\)`)
	nodeRe = regexp.MustCompile(`^\s*(p\d+): node\(id: \$(i\d+)\)`)
)

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if r.Header.Get("Authorization") == "bearer bad-token" {
		s.record("auth", nil, http.StatusUnauthorized)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
		return
	}
	s.mu.Lock()
	delay := s.delay
	s.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	kind := "poll"
	switch {
	case strings.Contains(body.Query, "nodes(ids: $ids)"):
		kind = "threads"
	case strings.Contains(body.Query, "pullRequest(number:"):
		kind = "resolve"
	}
	if len(s.inject) > 0 {
		resp := s.inject[0]
		s.inject = s.inject[1:]
		s.log = append(s.log, Request{Kind: kind, At: now, Status: resp.Status})
		for k, v := range resp.Headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(resp.Status)
		_, _ = w.Write([]byte(resp.Body))
		return
	}
	s.remaining -= s.cost
	reset := now.Add(time.Hour).Unix()
	w.Header().Set("X-Ratelimit-Limit", strconv.Itoa(s.limit))
	w.Header().Set("X-Ratelimit-Remaining", strconv.Itoa(s.remaining))
	w.Header().Set("X-Ratelimit-Used", strconv.Itoa(s.limit-s.remaining))
	w.Header().Set("X-Ratelimit-Reset", strconv.FormatInt(reset, 10))
	w.Header().Set("X-Ratelimit-Resource", "graphql")

	data := map[string]any{
		"rateLimit": map[string]any{"cost": s.cost, "limit": s.limit, "remaining": s.remaining, "used": s.limit - s.remaining, "resetAt": time.Unix(reset, 0).UTC().Format(time.RFC3339)},
	}
	var errs []map[string]any
	var prs []string
	switch kind {
	case "threads":
		var nodes []any
		ids, _ := body.Variables["ids"].([]any)
		for _, id := range ids {
			sid, _ := id.(string)
			h, ok := s.heads[sid]
			if !ok {
				nodes = append(nodes, nil)
				continue
			}
			nodes = append(nodes, map[string]any{"id": sid, "comments": map[string]any{"nodes": []any{
				map[string]any{"author": map[string]any{"login": h.Author}, "bodyText": h.Excerpt},
			}}})
		}
		data["nodes"] = nodes
	case "resolve":
		var repoAlias, owner, name string
		repos := map[string]map[string]any{}
		for _, line := range strings.Split(body.Query, "\n") {
			if m := repoRe.FindStringSubmatch(line); m != nil {
				repoAlias = m[1]
				owner, _ = strconv.Unquote(m[2])
				name, _ = strconv.Unquote(m[3])
				if s.repoExists(owner, name) {
					repos[repoAlias] = map[string]any{}
				} else {
					errs = append(errs, map[string]any{"type": "NOT_FOUND", "path": []any{repoAlias}, "message": "Could not resolve to a Repository"})
				}
				continue
			}
			if m := prRe.FindStringSubmatch(line); m != nil {
				n, _ := strconv.Atoi(m[2])
				key := strings.ToLower(fmt.Sprintf("%s/%s#%d", owner, name, n))
				prs = append(prs, key)
				repo, ok := repos[repoAlias]
				if !ok {
					continue
				}
				if p := s.prs[key]; p != nil {
					repo[m[1]] = s.prData(key, p, []any{repoAlias, m[1]}, &errs)
				} else {
					repo[m[1]] = nil
					errs = append(errs, map[string]any{"type": "NOT_FOUND", "path": []any{repoAlias, m[1]}, "message": "Could not resolve to a PullRequest"})
				}
			}
		}
		for _, line := range strings.Split(body.Query, "\n") {
			if m := repoRe.FindStringSubmatch(line); m != nil {
				if repo, ok := repos[m[1]]; ok {
					data[m[1]] = repo
				} else {
					data[m[1]] = nil
				}
			}
		}
	default:
		for _, line := range strings.Split(body.Query, "\n") {
			m := nodeRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			id, _ := body.Variables[m[2]].(string)
			key := s.byID[id]
			prs = append(prs, key)
			if p := s.prs[key]; p != nil {
				data[m[1]] = s.prData(key, p, []any{m[1]}, &errs)
			} else {
				data[m[1]] = nil
			}
		}
	}
	s.log = append(s.log, Request{Kind: kind, PRs: prs, At: now, Status: http.StatusOK})
	out := map[string]any{"data": data}
	if len(errs) > 0 {
		out["errors"] = errs
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) repoExists(owner, name string) bool {
	prefix := strings.ToLower(owner + "/" + name + "#")
	for k := range s.prs {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

func (s *Server) record(kind string, prs []string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log = append(s.log, Request{Kind: kind, PRs: prs, At: time.Now(), Status: status})
}
