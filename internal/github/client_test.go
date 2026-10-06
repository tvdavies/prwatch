package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tvdavies/prwatch/internal/prref"
)

func serve(t *testing.T, status int, headers map[string]string, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "bearer test-token" {
			t.Errorf("authorization header %q", got)
		}
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GH_TOKEN", "test-token")
	return NewClient(srv.URL, "test", &TokenSource{})
}

func TestClassification(t *testing.T) {
	reset := time.Now().Add(time.Hour).Unix()
	cases := []struct {
		name    string
		status  int
		headers map[string]string
		body    string
		check   func(error) bool
	}{
		{"secondary with retry-after", 403, map[string]string{"Retry-After": "30"}, `{"message":"You have exceeded a secondary rate limit"}`,
			func(err error) bool {
				var rl *RateLimitError
				return errors.As(err, &rl) && rl.Secondary && rl.RetryAfter == 30*time.Second
			}},
		{"secondary 429 without retry-after", 429, nil, `{"message":"slow down"}`,
			func(err error) bool {
				var rl *RateLimitError
				return errors.As(err, &rl) && rl.Secondary && rl.RetryAfter == 0
			}},
		{"primary exhausted", 403, map[string]string{"X-Ratelimit-Limit": "5000", "X-Ratelimit-Remaining": "0", "X-Ratelimit-Reset": itoa(reset)}, `{"message":"API rate limit exceeded"}`,
			func(err error) bool {
				var rl *RateLimitError
				return errors.As(err, &rl) && !rl.Secondary && rl.Remaining == 0 && rl.ResetAt.Unix() == reset
			}},
		{"graphql RATE_LIMITED", 200, map[string]string{"X-Ratelimit-Limit": "5000", "X-Ratelimit-Remaining": "0"}, `{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`,
			func(err error) bool { var rl *RateLimitError; return errors.As(err, &rl) && !rl.Secondary }},
		{"bad credentials", 401, nil, `{"message":"Bad credentials"}`,
			func(err error) bool { var ae *AuthError; return errors.As(err, &ae) }},
		{"forbidden", 403, map[string]string{"X-Ratelimit-Limit": "5000", "X-Ratelimit-Remaining": "4000"}, `{"message":"Resource protected by organization SAML enforcement"}`,
			func(err error) bool { var ae *AuthError; return errors.As(err, &ae) }},
		{"server error", 502, nil, `bad gateway`,
			func(err error) bool { var te *TransientError; return errors.As(err, &te) }},
	}
	for _, c := range cases {
		cl := serve(t, c.status, c.headers, c.body)
		_, err := cl.Do(context.Background(), "query{viewer{login}}", nil)
		if !c.check(err) {
			t.Errorf("%s: got %T %v", c.name, err, err)
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestRateFromBody(t *testing.T) {
	cl := serve(t, 200, map[string]string{"X-Ratelimit-Limit": "5000", "X-Ratelimit-Remaining": "4999"},
		`{"data":{"rateLimit":{"cost":1,"limit":5000,"remaining":4990,"used":10,"resetAt":"2026-10-06T13:00:00Z"}}}`)
	resp, err := cl.Do(context.Background(), "query{rateLimit{cost}}", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Rate.Cost != 1 || resp.Rate.Remaining != 4990 || !resp.Rate.Known {
		t.Fatalf("rate %+v", resp.Rate)
	}
}

func TestResolveQueryGroupsByRepo(t *testing.T) {
	refs := []prref.Ref{{Owner: "a", Repo: "x", Number: 1}, {Owner: "b", Repo: "y", Number: 2}, {Owner: "A", Repo: "X", Number: 3}}
	q, aliases := ResolveQuery(refs)
	if strings.Count(q, "repository(owner:") != 2 {
		t.Fatalf("expected two repositories in query:\n%s", q)
	}
	if len(aliases) != 3 || aliases["r0.q1"].Number != 3 {
		t.Fatalf("aliases %v", aliases)
	}
	if !strings.Contains(q, "isRequired(pullRequestNumber: 3)") || !strings.Contains(q, "rateLimit {") {
		t.Fatalf("query:\n%s", q)
	}
}

func TestPollQueryUsesVariables(t *testing.T) {
	q, vars := PollQuery([]Target{{NodeID: "PR_1"}, {NodeID: "PR_2"}})
	if vars["i1"] != "PR_2" || !strings.Contains(q, "isRequired(pullRequestId: $i1)") || !strings.Contains(q, "query($i0: ID!, $i1: ID!)") {
		t.Fatalf("query:\n%s\nvars %v", q, vars)
	}
}

func TestResolveDecodesAndReportsNotFound(t *testing.T) {
	body := `{"data":{"r0":{"q0":{"id":"PR_x","number":5,"url":"u","title":"T","state":"OPEN","mergeable":"MERGEABLE","mergeStateStatus":"CLEAN",
"repository":{"nameWithOwner":"Own/Repo"},
"reviewThreads":{"totalCount":2,"nodes":[{"id":"t1","isResolved":true,"path":"a"},{"id":"t2","isResolved":false,"path":"b","line":null,"originalLine":7}]},
"commits":{"nodes":[{"commit":{"oid":"c","statusCheckRollup":{"state":"PENDING","contexts":{"totalCount":2,"nodes":[
 {"__typename":"CheckRun","name":"build","status":"IN_PROGRESS","conclusion":null,"isRequired":true},
 {"__typename":"StatusContext","context":"ci/legacy","state":"FAILURE","isRequired":false}]}}}}]}},
"q1":null},"rateLimit":{"cost":1,"limit":5000,"remaining":4000,"used":1000,"resetAt":"2026-10-06T13:00:00Z"}},
"errors":[{"type":"NOT_FOUND","path":["r0","q1"],"message":"Could not resolve to a PullRequest with the number of 6."}]}`
	cl := serve(t, 200, nil, body)
	res, rate, err := cl.Resolve(context.Background(), []prref.Ref{{Owner: "own", Repo: "repo", Number: 5}, {Owner: "own", Repo: "repo", Number: 6}})
	if err != nil {
		t.Fatal(err)
	}
	if rate.Cost != 1 {
		t.Fatalf("rate %+v", rate)
	}
	var found, missing int
	for _, r := range res {
		if r.Err != nil {
			var nf *NotFoundError
			if !errors.As(r.Err, &nf) || !strings.Contains(nf.Error(), "number of 6") {
				t.Fatalf("error %v", r.Err)
			}
			missing++
			continue
		}
		found++
		s := r.Snapshot
		if r.NodeID != "PR_x" || s.PR != "Own/Repo#5" || s.Threads.Unresolved != 1 || *s.Threads.Items[0].Line != 7 {
			t.Fatalf("snapshot %+v", s)
		}
		if s.Checks.State != "PENDING" || len(s.Checks.Contexts) != 2 || s.Checks.Contexts[1].Kind != "status" || s.Checks.Contexts[1].Conclusion != "FAILURE" {
			t.Fatalf("checks %+v", s.Checks)
		}
		if len(s.Reasons) != 2 || s.Reasons[0] != "check_failed" || s.Reasons[1] != "unresolved_threads" {
			t.Fatalf("reasons %v", s.Reasons)
		}
	}
	if found != 1 || missing != 1 {
		t.Fatalf("found %d missing %d", found, missing)
	}
}
