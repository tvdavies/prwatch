package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tvdavies/prwatch/internal/prref"
	"github.com/tvdavies/prwatch/internal/snapshot"
)

// ChunkSize is the maximum number of PRs per GraphQL request.
const ChunkSize = 50

// prFragment selects everything except the status check rollup, which needs
// a per-PR argument for isRequired. Nested connections are avoided because
// GitHub's cost model multiplies them per parent node; the one exception is
// recentThreads, kept to a few threads so a round of up to about ten PRs
// still costs one point. Edits are seen only within the fetched window: the
// description, the newest 3 issue comments, the latest review per reviewer
// and the last 10 comments of each of the newest few review threads.
var prFragment = `fragment PRF on PullRequest {
  id number url title state isDraft merged mergedAt
  mergeCommit { oid }
  headRefOid baseRefName mergeable mergeStateStatus
  autoMergeRequest { enabledAt mergeMethod }
  reviewDecision updatedAt lastEditedAt
  repository { nameWithOwner }
  latestReviews(first: 20) { nodes { author { login } state submittedAt lastEditedAt commit { oid } } }
  reviews(first: 0) { totalCount }
  reviewRequests(first: 20) { nodes { requestedReviewer { __typename ... on User { login } ... on Bot { login } ... on Mannequin { login } ... on Team { slug } } } }
  reviewThreads(last: 100) { totalCount nodes { id isResolved isOutdated path line originalLine } }
  recentThreads: reviewThreads(last: ` + recentThreadWindow + `) { nodes { id comments(last: 10) { nodes { lastEditedAt } } } }
  comments(last: 3) { totalCount nodes { author { login } createdAt lastEditedAt bodyText } }
}`

var recentThreadWindow = strconv.Itoa(snapshot.RecentThreadWindow)

const rateLimitSel = `rateLimit { cost limit remaining used resetAt }`

func rollupSel(isRequiredArg string) string {
	return `commits(last: 1) { nodes { commit { oid statusCheckRollup { state contexts(first: 100) { totalCount nodes { __typename ` +
		`... on CheckRun { name status conclusion isRequired(` + isRequiredArg + `) } ` +
		`... on StatusContext { context state isRequired(` + isRequiredArg + `) } } } } } } }`
}

// NotFoundError means the repository or PR does not exist or is not visible.
type NotFoundError struct {
	Ref     prref.Ref
	Message string
}

func (e *NotFoundError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("%s not found: %s", e.Ref, e.Message)
	}
	return fmt.Sprintf("%s not found", e.Ref)
}

// Result is the outcome for one PR in a request.
type Result struct {
	Ref      prref.Ref
	NodeID   string
	Snapshot *snapshot.Snapshot
	Err      error
}

// Target is a PR with a known node id.
type Target struct {
	Ref    prref.Ref
	NodeID string
}

// ResolveQuery builds the aliased repository/pullRequest query used to look
// up node ids. It also returns full PR data, so it doubles as a first poll.
func ResolveQuery(refs []prref.Ref) (string, map[string]prref.Ref) {
	var b strings.Builder
	aliases := map[string]prref.Ref{}
	b.WriteString("query {\n")
	repoIdx := map[string]int{}
	var repos [][]prref.Ref
	for _, r := range refs {
		k := strings.ToLower(r.Owner + "/" + r.Repo)
		i, ok := repoIdx[k]
		if !ok {
			i = len(repos)
			repoIdx[k] = i
			repos = append(repos, nil)
		}
		repos[i] = append(repos[i], r)
	}
	n := 0
	for i, group := range repos {
		fmt.Fprintf(&b, "  r%d: repository(owner: %s, name: %s) {\n", i, strconv.Quote(group[0].Owner), strconv.Quote(group[0].Repo))
		for _, r := range group {
			alias := fmt.Sprintf("q%d", n)
			n++
			aliases[fmt.Sprintf("r%d.%s", i, alias)] = r
			fmt.Fprintf(&b, "    %s: pullRequest(number: %d) { ...PRF %s }\n", alias, r.Number, rollupSel("pullRequestNumber: "+strconv.Itoa(r.Number)))
		}
		b.WriteString("  }\n")
	}
	b.WriteString("  " + rateLimitSel + "\n}\n" + prFragment)
	return b.String(), aliases
}

// PollQuery builds the batched per-node poll query and its variables.
func PollQuery(targets []Target) (string, map[string]any) {
	var b strings.Builder
	vars := map[string]any{}
	var decls []string
	for i := range targets {
		decls = append(decls, fmt.Sprintf("$i%d: ID!", i))
	}
	fmt.Fprintf(&b, "query(%s) {\n", strings.Join(decls, ", "))
	for i, t := range targets {
		vars[fmt.Sprintf("i%d", i)] = t.NodeID
		fmt.Fprintf(&b, "  p%d: node(id: $i%d) { ...PRF ... on PullRequest { %s } }\n", i, i, rollupSel(fmt.Sprintf("pullRequestId: $i%d", i)))
	}
	b.WriteString("  " + rateLimitSel + "\n}\n" + prFragment)
	return b.String(), vars
}

// ThreadQuery fetches the first comment of each review thread.
const ThreadQuery = `query($ids: [ID!]!) {
  nodes(ids: $ids) { ... on PullRequestReviewThread { id comments(first: 1) { nodes { author { login } bodyText } } } }
  ` + rateLimitSel + `
}`

// Resolve looks up up to ChunkSize PRs by owner/repo#number in one request.
func (c *Client) Resolve(ctx context.Context, refs []prref.Ref) ([]Result, RateInfo, error) {
	if len(refs) > ChunkSize {
		return nil, RateInfo{}, fmt.Errorf("resolve: %d PRs exceeds chunk size", len(refs))
	}
	q, aliases := ResolveQuery(refs)
	resp, err := c.Do(ctx, q, nil)
	if err != nil {
		return nil, RateInfo{}, err
	}
	var out []Result
	now := time.Now()
	for path, ref := range aliases {
		repoAlias, prAlias, _ := strings.Cut(path, ".")
		res := Result{Ref: ref}
		var repo map[string]json.RawMessage
		if raw := resp.Data[repoAlias]; len(raw) > 0 && string(raw) != "null" {
			if err := json.Unmarshal(raw, &repo); err != nil {
				return nil, resp.Rate, fmt.Errorf("decode repository: %w", err)
			}
		}
		prPath := []string{repoAlias, prAlias}
		raw := repo[prAlias]
		if len(raw) == 0 || string(raw) == "null" {
			res.Err = nullPRError(ref, resp.Errors, prPath, prPath[:1])
			out = append(out, res)
			continue
		}
		snap, id, err := decodePR(raw, ref, now)
		if err != nil {
			return nil, resp.Rate, err
		}
		markIncomplete(snap, resp.Errors, prPath)
		res.Snapshot, res.NodeID = snap, id
		out = append(out, res)
	}
	return out, resp.Rate, nil
}

// Poll fetches up to ChunkSize PRs by node id in one request.
func (c *Client) Poll(ctx context.Context, targets []Target) ([]Result, RateInfo, error) {
	if len(targets) > ChunkSize {
		return nil, RateInfo{}, fmt.Errorf("poll: %d PRs exceeds chunk size", len(targets))
	}
	q, vars := PollQuery(targets)
	resp, err := c.Do(ctx, q, vars)
	if err != nil {
		return nil, RateInfo{}, err
	}
	now := time.Now()
	out := make([]Result, 0, len(targets))
	for i, t := range targets {
		alias := fmt.Sprintf("p%d", i)
		res := Result{Ref: t.Ref, NodeID: t.NodeID}
		raw := resp.Data[alias]
		if len(raw) == 0 || string(raw) == "null" || string(raw) == "{}" {
			res.Err = nullPRError(t.Ref, resp.Errors, []string{alias}, nil)
			out = append(out, res)
			continue
		}
		snap, _, err := decodePR(raw, t.Ref, now)
		if err != nil {
			return nil, resp.Rate, err
		}
		markIncomplete(snap, resp.Errors, []string{alias})
		res.Snapshot = snap
		out = append(out, res)
	}
	return out, resp.Rate, nil
}

// ThreadHead is the first comment of a review thread.
type ThreadHead struct {
	Author  string
	Excerpt string
	// Edited is the thread's edit stamp when the head was fetched, so a
	// cached head is refetched after an edit in the thread.
	Edited string
}

// ThreadHeads fetches the first comment of each given review thread.
func (c *Client) ThreadHeads(ctx context.Context, ids []string) (map[string]ThreadHead, RateInfo, error) {
	resp, err := c.Do(ctx, ThreadQuery, map[string]any{"ids": ids})
	if err != nil {
		return nil, RateInfo{}, err
	}
	var nodes []*struct {
		ID       string `json:"id"`
		Comments struct {
			Nodes []struct {
				Author   *Actor `json:"author"`
				BodyText string `json:"bodyText"`
			} `json:"nodes"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(resp.Data["nodes"], &nodes); err != nil {
		return nil, resp.Rate, fmt.Errorf("decode threads: %w", err)
	}
	out := map[string]ThreadHead{}
	for _, n := range nodes {
		if n == nil || n.ID == "" {
			continue
		}
		h := ThreadHead{}
		if len(n.Comments.Nodes) > 0 {
			h.Author = n.Comments.Nodes[0].Author.name()
			h.Excerpt = snapshot.Excerpt(n.Comments.Nodes[0].BodyText, 160)
		}
		out[n.ID] = h
	}
	return out, resp.Rate, nil
}

// MissingThreads returns unresolved thread ids in snaps not present in
// heads, or whose thread has been edited since the head was fetched.
func MissingThreads(snaps []*snapshot.Snapshot, heads map[string]ThreadHead) []string {
	var ids []string
	for _, s := range snaps {
		for _, t := range s.Threads.Items {
			if h, ok := heads[t.ID]; !ok || h.Edited != editStamp(t.EditedAt) {
				ids = append(ids, t.ID)
			}
		}
	}
	return ids
}

// StampHeads records each thread's current edit stamp on freshly fetched
// heads.
func StampHeads(snaps []*snapshot.Snapshot, heads map[string]ThreadHead) {
	for _, s := range snaps {
		for _, t := range s.Threads.Items {
			if h, ok := heads[t.ID]; ok {
				h.Edited = editStamp(t.EditedAt)
				heads[t.ID] = h
			}
		}
	}
}

func editStamp(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// FillThreads copies thread authors and excerpts from heads into s.
func FillThreads(s *snapshot.Snapshot, heads map[string]ThreadHead) {
	for i := range s.Threads.Items {
		if h, ok := heads[s.Threads.Items[i].ID]; ok {
			s.Threads.Items[i].Author = h.Author
			s.Threads.Items[i].Excerpt = h.Excerpt
		}
	}
}

// aliasError classifies why a PR alias came back null. GitHub reports a
// missing or invisible PR as NOT_FOUND; FORBIDDEN (e.g. SAML enforcement)
// is an access problem; anything else is treated as transient so the
// watch is retried rather than dropped. A null with no error at all is
// treated as not found.
func aliasError(ref prref.Ref, e GQLError) error {
	switch {
	case e.Type == "" && e.Message == "", e.Type == "NOT_FOUND":
		return &NotFoundError{Ref: ref, Message: e.Message}
	case e.Type == "FORBIDDEN" || strings.Contains(e.Message, "SAML"):
		return &AuthError{Message: fmt.Sprintf("%s: %s", ref, e.Message)}
	}
	return &TransientError{Err: fmt.Errorf("%s: %s (%s)", ref, e.Message, e.Type)}
}

// nullPRError classifies a PR alias that came back null. It prefers an
// error at the alias itself; then one beneath it, since GraphQL nulls the
// nearest nullable parent when a non-null field fails but keeps the field's
// own path; then one at parent (the repository, for Resolve); then one with
// no path. An error beneath the alias says a field failed, not that the PR
// is missing, so it is never classified as not found.
func nullPRError(ref prref.Ref, errs []GQLError, alias, parent []string) error {
	if e, ok := errorAt(errs, alias); ok {
		return aliasError(ref, e)
	}
	for _, e := range errs {
		if len(e.Path) > len(alias) && pathHasPrefix(e.Path, alias) {
			e.Message = formatPath(e.Path[len(alias):]) + ": " + e.Message
			err := aliasError(ref, e)
			var nf *NotFoundError
			if errors.As(err, &nf) {
				err = &TransientError{Err: fmt.Errorf("%s: %s (%s)", ref, e.Message, e.Type)}
			}
			return err
		}
	}
	if parent != nil {
		if e, ok := errorAt(errs, parent); ok {
			return aliasError(ref, e)
		}
	}
	e, _ := errorAt(errs, nil)
	return aliasError(ref, e)
}

// errorAt returns the first error whose path is exactly path. A nil path
// matches errors that carry no path at all.
func errorAt(errs []GQLError, path []string) (GQLError, bool) {
	for _, e := range errs {
		if len(e.Path) == len(path) && pathHasPrefix(e.Path, path) {
			return e, true
		}
	}
	return GQLError{}, false
}

// pathHasPrefix reports whether a GraphQL error path starts with prefix.
func pathHasPrefix(path []any, prefix []string) bool {
	if len(path) < len(prefix) {
		return false
	}
	for i, p := range prefix {
		if s, ok := path[i].(string); !ok || s != p {
			return false
		}
	}
	return true
}

// markIncomplete flags a decoded PR as incomplete when any GraphQL error
// lies beneath its alias, or carries no path and so may affect any PR.
// Such an error nulls a nested field (commits, for example), which would
// otherwise decode as a plausible but false value such as "no checks".
func markIncomplete(s *snapshot.Snapshot, errs []GQLError, alias []string) {
	var reasons []string
	for _, e := range errs {
		switch {
		case len(e.Path) == 0:
			reasons = append(reasons, e.Message)
		case len(e.Path) > len(alias) && pathHasPrefix(e.Path, alias):
			reasons = append(reasons, fmt.Sprintf("%s: %s", formatPath(e.Path[len(alias):]), e.Message))
		}
	}
	if len(reasons) == 0 {
		return
	}
	s.Incomplete = true
	s.IncompleteReason = "GitHub returned partial data: " + strings.Join(reasons, "; ")
	s.Finalise()
}

func formatPath(path []any) string {
	parts := make([]string, len(path))
	for i, p := range path {
		parts[i] = fmt.Sprint(p)
	}
	return strings.Join(parts, ".")
}

// Actor is a GraphQL actor.
type Actor struct {
	Login string `json:"login"`
}

func (a *Actor) name() string {
	if a == nil || a.Login == "" {
		return "ghost"
	}
	return a.Login
}

// OID wraps a commit oid.
type OID struct {
	Oid string `json:"oid"`
}

// RawPR mirrors the PRF fragment plus the rollup selection. It is exported
// so the test fake can produce responses of the same shape.
type RawPR struct {
	ID               string        `json:"id"`
	Number           int           `json:"number"`
	URL              string        `json:"url"`
	Title            string        `json:"title"`
	State            string        `json:"state"`
	IsDraft          bool          `json:"isDraft"`
	Merged           bool          `json:"merged"`
	MergedAt         *time.Time    `json:"mergedAt"`
	MergeCommit      *OID          `json:"mergeCommit"`
	HeadRefOid       string        `json:"headRefOid"`
	BaseRefName      string        `json:"baseRefName"`
	Mergeable        string        `json:"mergeable"`
	MergeStateStatus string        `json:"mergeStateStatus"`
	AutoMergeRequest *RawAutoMerge `json:"autoMergeRequest"`
	ReviewDecision   *string       `json:"reviewDecision"`
	UpdatedAt        time.Time     `json:"updatedAt"`
	LastEditedAt     *time.Time    `json:"lastEditedAt"`
	Repository       struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	LatestReviews struct {
		Nodes []RawReview `json:"nodes"`
	} `json:"latestReviews"`
	Reviews struct {
		TotalCount int `json:"totalCount"`
	} `json:"reviews"`
	ReviewRequests struct {
		Nodes []RawReviewRequest `json:"nodes"`
	} `json:"reviewRequests"`
	ReviewThreads struct {
		TotalCount int         `json:"totalCount"`
		Nodes      []RawThread `json:"nodes"`
	} `json:"reviewThreads"`
	RecentThreads struct {
		Nodes []RawRecentThread `json:"nodes"`
	} `json:"recentThreads"`
	Comments struct {
		TotalCount int          `json:"totalCount"`
		Nodes      []RawComment `json:"nodes"`
	} `json:"comments"`
	Commits struct {
		Nodes []RawCommitNode `json:"nodes"`
	} `json:"commits"`
}

// RawAutoMerge is an autoMergeRequest.
type RawAutoMerge struct {
	EnabledAt   *time.Time `json:"enabledAt"`
	MergeMethod string     `json:"mergeMethod"`
}

// RawReviewRequest is a reviewRequests node.
type RawReviewRequest struct {
	RequestedReviewer *struct {
		Typename string `json:"__typename"`
		Login    string `json:"login,omitempty"`
		Slug     string `json:"slug,omitempty"`
	} `json:"requestedReviewer"`
}

// RawCommitNode is a commits node.
type RawCommitNode struct {
	Commit struct {
		Oid               string     `json:"oid"`
		StatusCheckRollup *RawRollup `json:"statusCheckRollup"`
	} `json:"commit"`
}

// RawReview is a latestReviews node.
type RawReview struct {
	Author       *Actor     `json:"author"`
	State        string     `json:"state"`
	SubmittedAt  *time.Time `json:"submittedAt"`
	LastEditedAt *time.Time `json:"lastEditedAt"`
	Commit       *OID       `json:"commit"`
}

// RawThread is a reviewThreads node.
type RawThread struct {
	ID           string `json:"id"`
	IsResolved   bool   `json:"isResolved"`
	IsOutdated   bool   `json:"isOutdated"`
	Path         string `json:"path"`
	Line         *int   `json:"line"`
	OriginalLine *int   `json:"originalLine"`
}

// RawRecentThread is a recentThreads node: one of the newest review
// threads with the edit times of its last comments.
type RawRecentThread struct {
	ID       string `json:"id"`
	Comments struct {
		Nodes []RawEdit `json:"nodes"`
	} `json:"comments"`
}

// RawEdit carries a comment's lastEditedAt.
type RawEdit struct {
	LastEditedAt *time.Time `json:"lastEditedAt"`
}

// RawComment is a comments node.
type RawComment struct {
	Author       *Actor     `json:"author"`
	CreatedAt    time.Time  `json:"createdAt"`
	LastEditedAt *time.Time `json:"lastEditedAt"`
	BodyText     string     `json:"bodyText"`
}

// RawRollup is a statusCheckRollup.
type RawRollup struct {
	State    string `json:"state"`
	Contexts struct {
		TotalCount int          `json:"totalCount"`
		Nodes      []RawContext `json:"nodes"`
	} `json:"contexts"`
}

// RawContext is a CheckRun or StatusContext.
type RawContext struct {
	Typename   string `json:"__typename"`
	Name       string `json:"name,omitempty"`
	Status     string `json:"status,omitempty"`
	Conclusion string `json:"conclusion,omitempty"`
	Context    string `json:"context,omitempty"`
	State      string `json:"state,omitempty"`
	IsRequired *bool  `json:"isRequired"`
}

func decodePR(raw json.RawMessage, ref prref.Ref, now time.Time) (*snapshot.Snapshot, string, error) {
	var p RawPR
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, "", fmt.Errorf("decode pull request %s: %w", ref, err)
	}
	return ToSnapshot(&p, ref, now), p.ID, nil
}

// ToSnapshot converts a raw PR into a finalised snapshot.
func ToSnapshot(p *RawPR, ref prref.Ref, now time.Time) *snapshot.Snapshot {
	owner, repo := ref.Owner, ref.Repo
	if o, r, ok := strings.Cut(p.Repository.NameWithOwner, "/"); ok {
		owner, repo = o, r
	}
	s := &snapshot.Snapshot{
		PR:               fmt.Sprintf("%s/%s#%d", owner, repo, p.Number),
		Owner:            owner,
		Repo:             repo,
		Number:           p.Number,
		URL:              p.URL,
		Title:            p.Title,
		State:            p.State,
		IsDraft:          p.IsDraft,
		Merged:           p.Merged,
		MergedAt:         p.MergedAt,
		HeadRefOid:       p.HeadRefOid,
		BaseRefName:      p.BaseRefName,
		Mergeable:        p.Mergeable,
		MergeStateStatus: p.MergeStateStatus,
		ReviewDecision:   p.ReviewDecision,
		ReviewCount:      p.Reviews.TotalCount,
		UpdatedAt:        p.UpdatedAt,
		BodyEditedAt:     p.LastEditedAt,
		FetchedAt:        now.UTC(),
	}
	if p.Number == 0 {
		s.Number = ref.Number
		s.PR = ref.String()
	}
	if p.MergeCommit != nil {
		s.MergeCommit = &p.MergeCommit.Oid
	}
	if p.AutoMergeRequest != nil {
		s.AutoMerge.Enabled = true
		if p.AutoMergeRequest.MergeMethod != "" {
			m := p.AutoMergeRequest.MergeMethod
			s.AutoMerge.Method = &m
		}
	}
	for _, r := range p.LatestReviews.Nodes {
		rv := snapshot.Review{Author: r.Author.name(), State: r.State, SubmittedAt: r.SubmittedAt, EditedAt: r.LastEditedAt}
		if r.Commit != nil {
			c := r.Commit.Oid
			rv.Commit = &c
		}
		s.Reviews = append(s.Reviews, rv)
	}
	for _, n := range p.ReviewRequests.Nodes {
		rr := n.RequestedReviewer
		switch {
		case rr == nil:
		case rr.Slug != "":
			s.ReviewRequests = append(s.ReviewRequests, "team:"+rr.Slug)
		case rr.Login != "":
			s.ReviewRequests = append(s.ReviewRequests, rr.Login)
		}
	}
	s.Threads.Total = p.ReviewThreads.TotalCount
	threadEdits := map[string]*time.Time{}
	for _, t := range p.RecentThreads.Nodes {
		var last *time.Time
		for _, c := range t.Comments.Nodes {
			if c.LastEditedAt != nil && (last == nil || c.LastEditedAt.After(*last)) {
				last = c.LastEditedAt
			}
		}
		if last == nil {
			continue
		}
		threadEdits[t.ID] = last
		if s.Threads.Edits == nil {
			s.Threads.Edits = map[string]time.Time{}
		}
		s.Threads.Edits[t.ID] = *last
	}
	for _, t := range p.ReviewThreads.Nodes {
		if t.IsResolved {
			continue
		}
		line := t.Line
		if line == nil {
			line = t.OriginalLine
		}
		s.Threads.Items = append(s.Threads.Items, snapshot.Thread{ID: t.ID, Path: t.Path, Line: line, Outdated: t.IsOutdated, EditedAt: threadEdits[t.ID]})
	}
	s.Threads.Unresolved = len(s.Threads.Items)
	// Only the most recent 100 threads are fetched; older ones may hide
	// unresolved threads, so such a PR is never reported as ready.
	s.Threads.Truncated = p.ReviewThreads.TotalCount > len(p.ReviewThreads.Nodes)
	s.Comments.Total = p.Comments.TotalCount
	for _, c := range p.Comments.Nodes {
		s.Comments.Recent = append(s.Comments.Recent, snapshot.Comment{Author: c.Author.name(), CreatedAt: c.CreatedAt, EditedAt: c.LastEditedAt, Excerpt: snapshot.Excerpt(c.BodyText, 160)})
	}
	if len(p.Commits.Nodes) > 0 {
		if r := p.Commits.Nodes[0].Commit.StatusCheckRollup; r != nil {
			s.Checks.State = r.State
			s.Checks.Total = r.Contexts.TotalCount
			for _, c := range r.Contexts.Nodes {
				s.Checks.Contexts = append(s.Checks.Contexts, contextToCheck(c))
			}
		}
	}
	s.Finalise()
	return s
}

func contextToCheck(c RawContext) snapshot.Check {
	if c.Typename == "StatusContext" {
		ch := snapshot.Check{Name: c.Context, Kind: "status", Required: c.IsRequired}
		switch c.State {
		case "PENDING", "EXPECTED":
			ch.Status = c.State
		default:
			ch.Status = "COMPLETED"
			ch.Conclusion = c.State
		}
		return ch
	}
	return snapshot.Check{Name: c.Name, Kind: "check_run", Status: c.Status, Conclusion: c.Conclusion, Required: c.IsRequired}
}
