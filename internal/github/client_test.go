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
	"github.com/tvdavies/prwatch/internal/snapshot"
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

func TestNullAliasClassification(t *testing.T) {
	ref := prref.Ref{Owner: "o", Repo: "r", Number: 1}
	var nf *NotFoundError
	var ae *AuthError
	var te *TransientError
	if err := aliasError(ref, GQLError{}); !errors.As(err, &nf) {
		t.Errorf("no error: %T", err)
	}
	if err := aliasError(ref, GQLError{Type: "NOT_FOUND", Message: "x"}); !errors.As(err, &nf) {
		t.Errorf("NOT_FOUND: %T", err)
	}
	if err := aliasError(ref, GQLError{Type: "FORBIDDEN", Message: "Resource not accessible by integration"}); !errors.As(err, &ae) {
		t.Errorf("FORBIDDEN: %T", err)
	}
	if err := aliasError(ref, GQLError{Message: "Although you appear to have the correct authorization credentials, the org has enabled SAML"}); !errors.As(err, &ae) {
		t.Errorf("SAML: %T", err)
	}
	if err := aliasError(ref, GQLError{Type: "INTERNAL", Message: "Something went wrong"}); !errors.As(err, &te) {
		t.Errorf("INTERNAL: %T", err)
	}
}

func TestTruncatedThreads(t *testing.T) {
	p := &RawPR{State: "OPEN", Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN"}
	p.ReviewThreads.TotalCount = 150
	p.ReviewThreads.Nodes = []RawThread{{ID: "a", IsResolved: true}}
	s := ToSnapshot(p, prref.Ref{Owner: "o", Repo: "r", Number: 1}, time.Now())
	if !s.Threads.Truncated || s.Threads.Unresolved != 0 {
		t.Fatalf("threads %+v", s.Threads)
	}
	for _, r := range s.Reasons {
		if r == "ready_auto_merge_off" {
			t.Fatal("a PR with unseen threads must not be ready")
		}
	}
}

func TestPollNestedErrorMarksIncomplete(t *testing.T) {
	// GitHub returned the PR but an error nulled commits: checks would
	// otherwise decode as NONE, which counts as green and settled.
	body := `{"data":{
"p0":{"id":"PR_1","number":1,"state":"OPEN","mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","commits":null},
"p1":{"id":"PR_2","number":2,"state":"OPEN","mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","commits":{"nodes":[]}},
"rateLimit":{"cost":1,"limit":5000,"remaining":4000,"used":1000,"resetAt":"2026-10-06T13:00:00Z"}},
"errors":[{"type":"INTERNAL","path":["p0","commits"],"message":"Something went wrong"}]}`
	cl := serve(t, 200, nil, body)
	res, _, err := cl.Poll(context.Background(), []Target{
		{Ref: prref.Ref{Owner: "o", Repo: "r", Number: 1}, NodeID: "PR_1"},
		{Ref: prref.Ref{Owner: "o", Repo: "r", Number: 2}, NodeID: "PR_2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := res[0].Snapshot
	if res[0].Err != nil || s == nil || !s.Incomplete || !strings.Contains(s.IncompleteReason, "commits: Something went wrong") {
		t.Fatalf("p0: err %v snapshot %+v", res[0].Err, s)
	}
	for _, r := range s.Reasons {
		if r == "ready_auto_merge_off" {
			t.Fatal("an incomplete PR was reported ready")
		}
	}
	if res[1].Snapshot == nil || res[1].Snapshot.Incomplete {
		t.Fatalf("p1 must not inherit p0's error: %+v", res[1].Snapshot)
	}
}

func TestResolveNestedErrorDoesNotLeakToSiblings(t *testing.T) {
	// q0 has a nested error; q1 is null with no error of its own and must be
	// classified as not found, not as q0's error.
	body := `{"data":{"r0":{
"q0":{"id":"PR_1","number":1,"state":"OPEN","mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","commits":null},
"q1":null},
"rateLimit":{"cost":1,"limit":5000,"remaining":4000,"used":1000,"resetAt":"2026-10-06T13:00:00Z"}},
"errors":[{"type":"INTERNAL","path":["r0","q0","commits","nodes",0],"message":"Something went wrong"}]}`
	cl := serve(t, 200, nil, body)
	res, _, err := cl.Resolve(context.Background(), []prref.Ref{{Owner: "o", Repo: "r", Number: 1}, {Owner: "o", Repo: "r", Number: 2}})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		switch r.Ref.Number {
		case 1:
			if r.Snapshot == nil || !r.Snapshot.Incomplete || !strings.Contains(r.Snapshot.IncompleteReason, "commits.nodes.0: Something went wrong") {
				t.Fatalf("q0: %+v", r.Snapshot)
			}
		case 2:
			var nf *NotFoundError
			if !errors.As(r.Err, &nf) {
				t.Fatalf("q1: %T %v", r.Err, r.Err)
			}
		}
	}
}

func TestNullPRWithNestedErrorIsNotNotFound(t *testing.T) {
	// A failing non-null field nulls the whole PR but the error keeps the
	// field's path. That is a failure to read the PR, not a missing PR.
	rate := `"rateLimit":{"cost":1,"limit":5000,"remaining":4000,"used":1000,"resetAt":"2026-10-06T13:00:00Z"}`
	cl := serve(t, 200, nil, `{"data":{"p0":null,"p1":null,`+rate+`},
"errors":[{"type":"INTERNAL","path":["p0","mergeable"],"message":"Something went wrong"}]}`)
	res, _, err := cl.Poll(context.Background(), []Target{
		{Ref: prref.Ref{Owner: "o", Repo: "r", Number: 1}, NodeID: "PR_1"},
		{Ref: prref.Ref{Owner: "o", Repo: "r", Number: 2}, NodeID: "PR_2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var te *TransientError
	var nf *NotFoundError
	if !errors.As(res[0].Err, &te) || !strings.Contains(res[0].Err.Error(), "mergeable") {
		t.Fatalf("p0: %T %v", res[0].Err, res[0].Err)
	}
	if !errors.As(res[1].Err, &nf) {
		t.Fatalf("p1 must not inherit p0's error: %T %v", res[1].Err, res[1].Err)
	}

	cl = serve(t, 200, nil, `{"data":{"r0":{"q0":null,"q1":null},`+rate+`},
"errors":[{"type":"FORBIDDEN","path":["r0","q0","commits"],"message":"Resource not accessible by integration"},
{"type":"NOT_FOUND","path":["r0","q1","headRepository"],"message":"Could not resolve"}]}`)
	res, _, err = cl.Resolve(context.Background(), []prref.Ref{{Owner: "o", Repo: "r", Number: 1}, {Owner: "o", Repo: "r", Number: 2}})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		var ae *AuthError
		switch r.Ref.Number {
		case 1:
			if !errors.As(r.Err, &ae) {
				t.Fatalf("q0: %T %v", r.Err, r.Err)
			}
		case 2:
			if !errors.As(r.Err, &te) {
				t.Fatalf("q1: a nested NOT_FOUND is not a missing PR: %T %v", r.Err, r.Err)
			}
		}
	}
}

func TestEditTimesDecoded(t *testing.T) {
	at := func(s string) *time.Time { v, _ := time.Parse(time.RFC3339, s); return &v }
	p := &RawPR{State: "OPEN", Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN", LastEditedAt: at("2026-10-01T10:00:00Z")}
	p.LatestReviews.Nodes = []RawReview{{Author: &Actor{Login: "a"}, State: "COMMENTED", LastEditedAt: at("2026-10-01T11:00:00Z")}}
	p.Comments.TotalCount = 1
	p.Comments.Nodes = []RawComment{{Author: &Actor{Login: "b"}, LastEditedAt: at("2026-10-01T12:00:00Z")}}
	p.ReviewThreads.TotalCount = 2
	p.ReviewThreads.Nodes = []RawThread{{ID: "t1"}, {ID: "t2", IsResolved: true}}
	p.RecentThreads.Nodes = make([]RawRecentThread, 2)
	p.RecentThreads.Nodes[0].ID = "t1"
	p.RecentThreads.Nodes[0].Comments.Nodes = []RawEdit{{}, {LastEditedAt: at("2026-10-01T13:00:00Z")}}
	p.RecentThreads.Nodes[1].ID = "t2"
	p.RecentThreads.Nodes[1].Comments.Nodes = []RawEdit{{LastEditedAt: at("2026-10-01T14:00:00Z")}}
	s := ToSnapshot(p, prref.Ref{Owner: "o", Repo: "r", Number: 1}, time.Now())
	if s.BodyEditedAt == nil || s.Reviews[0].EditedAt == nil || s.Comments.Recent[0].EditedAt == nil {
		t.Fatalf("edit times not decoded: %+v", s)
	}
	// The resolved thread's edit counts for the PR; only open threads are items.
	if !s.Threads.EditedAt.Equal(*at("2026-10-01T14:00:00Z")) || !s.Threads.Items[0].EditedAt.Equal(*at("2026-10-01T13:00:00Z")) {
		t.Fatalf("thread edits: %+v %+v", s.Threads.EditedAt, s.Threads.Items[0].EditedAt)
	}
	q, _ := PollQuery([]Target{{NodeID: "PR_1"}})
	if !strings.Contains(q, "recentThreads: reviewThreads(last: 5)") || strings.Count(q, "lastEditedAt") != 4 {
		t.Fatalf("query:\n%s", q)
	}
}

func TestEditedThreadHeadIsRefetched(t *testing.T) {
	edit := time.Date(2026, 10, 1, 13, 0, 0, 0, time.UTC)
	s := &snapshot.Snapshot{Threads: snapshot.Threads{Items: []snapshot.Thread{{ID: "t1"}}}}
	heads := map[string]ThreadHead{"t1": {Author: "a", Excerpt: "old"}}
	if m := MissingThreads([]*snapshot.Snapshot{s}, heads); len(m) != 0 {
		t.Fatalf("unedited head refetched: %v", m)
	}
	s.Threads.Items[0].EditedAt = &edit
	if m := MissingThreads([]*snapshot.Snapshot{s}, heads); len(m) != 1 {
		t.Fatalf("edited head not refetched: %v", m)
	}
	fresh := map[string]ThreadHead{"t1": {Author: "a", Excerpt: "new"}}
	StampHeads([]*snapshot.Snapshot{s}, fresh)
	if m := MissingThreads([]*snapshot.Snapshot{s}, fresh); len(m) != 0 {
		t.Fatalf("stamped head refetched: %v", m)
	}
}
