// Package snapshot defines prwatch's versioned PR snapshot, its state token,
// the needsAction reasons and the --for wait conditions.
package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// SchemaVersion is the version of the Snapshot JSON schema.
const SchemaVersion = 1

// Snapshot is the state of one pull request at a point in time.
type Snapshot struct {
	SchemaVersion    int        `json:"schemaVersion"`
	PR               string     `json:"pr"`
	Owner            string     `json:"owner"`
	Repo             string     `json:"repo"`
	Number           int        `json:"number"`
	URL              string     `json:"url"`
	Title            string     `json:"title"`
	State            string     `json:"state"`
	IsDraft          bool       `json:"isDraft"`
	Merged           bool       `json:"merged"`
	MergedAt         *time.Time `json:"mergedAt"`
	MergeCommit      *string    `json:"mergeCommit"`
	HeadRefOid       string     `json:"headRefOid"`
	BaseRefName      string     `json:"baseRefName"`
	Mergeable        string     `json:"mergeable"`
	MergeStateStatus string     `json:"mergeStateStatus"`
	AutoMerge        AutoMerge  `json:"autoMerge"`
	ReviewDecision   *string    `json:"reviewDecision"`
	Reviews          []Review   `json:"reviews"`
	ReviewCount      int        `json:"reviewCount"`
	ReviewRequests   []string   `json:"reviewRequests"`
	Threads          Threads    `json:"threads"`
	Checks           Checks     `json:"checks"`
	Comments         Comments   `json:"comments"`
	UpdatedAt        time.Time  `json:"updatedAt"`
	NeedsAction      bool       `json:"needsAction"`
	Reasons          []string   `json:"reasons"`
	Token            string     `json:"token"`
	FetchedAt        time.Time  `json:"fetchedAt"`
	// Incomplete is true when GitHub returned the PR but an error nulled
	// part of it, so some fields (checks, for example) may be missing.
	// An incomplete snapshot never satisfies a wait condition.
	Incomplete       bool   `json:"incomplete"`
	IncompleteReason string `json:"incompleteReason,omitempty"`
}

// AutoMerge describes the PR's auto-merge request.
type AutoMerge struct {
	Enabled bool    `json:"enabled"`
	Method  *string `json:"method"`
}

// Review is the latest review by one author.
type Review struct {
	Author      string     `json:"author"`
	State       string     `json:"state"`
	SubmittedAt *time.Time `json:"submittedAt"`
	Commit      *string    `json:"commit"`
}

// Threads summarises review threads.
type Threads struct {
	Total      int `json:"total"`
	Unresolved int `json:"unresolved"`
	// Truncated is true when the PR has more threads than were fetched
	// (100), so Unresolved may be an undercount.
	Truncated bool     `json:"truncated"`
	Items     []Thread `json:"items"`
}

// Thread is one unresolved review thread.
type Thread struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Line     *int   `json:"line"`
	Outdated bool   `json:"outdated"`
	Author   string `json:"author"`
	Excerpt  string `json:"excerpt"`
}

// Checks summarises the head commit's status check rollup.
type Checks struct {
	// State is the rollup state: SUCCESS, PENDING, FAILURE, ERROR, EXPECTED,
	// or NONE when the head commit has no checks.
	State    string  `json:"state"`
	Total    int     `json:"total"`
	Contexts []Check `json:"contexts"`
}

// Check is one check run or commit status.
type Check struct {
	Name string `json:"name"`
	// Kind is "check_run" or "status".
	Kind string `json:"kind"`
	// Status is QUEUED, IN_PROGRESS, COMPLETED, PENDING etc.
	Status string `json:"status"`
	// Conclusion is SUCCESS, FAILURE, NEUTRAL, SKIPPED etc., or the commit
	// status state for statuses; empty while running.
	Conclusion string `json:"conclusion"`
	Required   *bool  `json:"required"`
}

// Comments summarises issue comments on the PR.
type Comments struct {
	Total  int       `json:"total"`
	Recent []Comment `json:"recent"`
}

// Comment is one recent issue comment.
type Comment struct {
	Author    string    `json:"author"`
	CreatedAt time.Time `json:"createdAt"`
	Excerpt   string    `json:"excerpt"`
}

// Reasons reported in needsAction.
const (
	ReasonRequiredCheckFailed = "required_check_failed"
	ReasonCheckFailed         = "check_failed"
	ReasonChangesRequested    = "changes_requested"
	ReasonUnresolvedThreads   = "unresolved_threads"
	ReasonConflict            = "conflict"
	ReasonReadyAutoMergeOff   = "ready_auto_merge_off"
)

// Finalise normalises slices, computes reasons, needsAction and the token.
func (s *Snapshot) Finalise() {
	s.SchemaVersion = SchemaVersion
	if s.Reviews == nil {
		s.Reviews = []Review{}
	}
	if s.ReviewRequests == nil {
		s.ReviewRequests = []string{}
	}
	if s.Threads.Items == nil {
		s.Threads.Items = []Thread{}
	}
	if s.Checks.Contexts == nil {
		s.Checks.Contexts = []Check{}
	}
	if s.Comments.Recent == nil {
		s.Comments.Recent = []Comment{}
	}
	if s.Checks.State == "" {
		s.Checks.State = "NONE"
	}
	s.Reasons = computeReasons(s)
	s.NeedsAction = len(s.Reasons) > 0
	s.Token = ComputeToken(s).String()
}

// Failed reports whether a check finished unsuccessfully.
func (c Check) Failed() bool {
	switch c.Conclusion {
	case "FAILURE", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED", "STARTUP_FAILURE", "ERROR":
		return true
	}
	return false
}

func computeReasons(s *Snapshot) []string {
	reasons := []string{}
	if s.State != "OPEN" {
		return reasons
	}
	var reqFailed, otherFailed bool
	for _, c := range s.Checks.Contexts {
		if !c.Failed() {
			continue
		}
		if c.Required != nil && *c.Required {
			reqFailed = true
		} else {
			otherFailed = true
		}
	}
	if reqFailed {
		reasons = append(reasons, ReasonRequiredCheckFailed)
	}
	if otherFailed {
		reasons = append(reasons, ReasonCheckFailed)
	}
	if s.ReviewDecision != nil && *s.ReviewDecision == "CHANGES_REQUESTED" {
		reasons = append(reasons, ReasonChangesRequested)
	}
	if s.Threads.Unresolved > 0 {
		reasons = append(reasons, ReasonUnresolvedThreads)
	}
	if s.Mergeable == "CONFLICTING" {
		reasons = append(reasons, ReasonConflict)
	}
	if Ready(s) && !s.AutoMerge.Enabled {
		reasons = append(reasons, ReasonReadyAutoMergeOff)
	}
	return reasons
}

// ChecksSettled reports whether the rollup has left PENDING. A head commit
// with no checks at all counts as settled.
func ChecksSettled(s *Snapshot) bool {
	return s.Checks.State != "PENDING" && s.Checks.State != "EXPECTED"
}

// Green reports whether the rollup succeeded, or there are no checks.
func Green(s *Snapshot) bool { return s.Checks.State == "SUCCESS" || s.Checks.State == "NONE" }

// Approved reports whether the PR is approved, or needs no review.
func Approved(s *Snapshot) bool {
	return s.ReviewDecision == nil || *s.ReviewDecision == "APPROVED"
}

// Ready reports whether an open PR is approved, green, has no unresolved
// threads and can be merged. An incomplete snapshot is never ready.
func Ready(s *Snapshot) bool {
	if s.Incomplete || s.State != "OPEN" || s.IsDraft {
		return false
	}
	if !Approved(s) || !Green(s) || s.Threads.Unresolved > 0 || s.Threads.Truncated || s.Mergeable != "MERGEABLE" {
		return false
	}
	switch s.MergeStateStatus {
	case "", "CLEAN", "HAS_HOOKS", "UNSTABLE":
		return true
	}
	return false
}

// Token identifies a snapshot's material state. All covers every material
// field; Review covers reviews and review threads only.
type Token struct {
	All    string
	Review string
}

func (t Token) String() string { return "1." + t.All + "." + t.Review }

// ParseToken parses a token produced by Token.String.
func ParseToken(s string) (Token, error) {
	parts := strings.Split(s, ".")
	if len(parts) != 3 || parts[0] != "1" || len(parts[1]) != 16 || len(parts[2]) != 8 || !isHex(parts[1]) || !isHex(parts[2]) {
		return Token{}, fmt.Errorf("invalid state token %q", s)
	}
	return Token{All: parts[1], Review: parts[2]}, nil
}

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

type reviewMaterial struct {
	Decision *string  `json:"d"`
	Reviews  []string `json:"r"`
	Count    int      `json:"c"`
	Total    int      `json:"t"`
	Open     []string `json:"o"`
}

type allMaterial struct {
	State       string   `json:"s"`
	Draft       bool     `json:"dr"`
	Merged      bool     `json:"m"`
	MergeCommit *string  `json:"mc"`
	Head        string   `json:"h"`
	Base        string   `json:"b"`
	Title       string   `json:"ti"`
	Mergeable   string   `json:"mg"`
	MergeState  string   `json:"ms"`
	AutoMerge   bool     `json:"am"`
	AutoMethod  *string  `json:"amm"`
	Requests    []string `json:"rq"`
	Checks      string   `json:"c"`
	Contexts    []string `json:"cx"`
	Comments    int      `json:"cm"`
	LastComment string   `json:"lc"`
	Review      string   `json:"rv"`
	Incomplete  bool     `json:"inc,omitempty"`
}

// ComputeToken hashes the material fields of s.
func ComputeToken(s *Snapshot) Token {
	rm := reviewMaterial{Decision: s.ReviewDecision, Count: s.ReviewCount, Total: s.Threads.Total}
	for _, r := range s.Reviews {
		var at, commit string
		if r.SubmittedAt != nil {
			at = r.SubmittedAt.UTC().Format(time.RFC3339)
		}
		if r.Commit != nil {
			commit = *r.Commit
		}
		rm.Reviews = append(rm.Reviews, r.Author+"|"+r.State+"|"+at+"|"+commit)
	}
	sort.Strings(rm.Reviews)
	for _, t := range s.Threads.Items {
		rm.Open = append(rm.Open, t.ID)
	}
	sort.Strings(rm.Open)
	review := hashOf(rm)[:8]

	am := allMaterial{
		State: s.State, Draft: s.IsDraft, Merged: s.Merged, MergeCommit: s.MergeCommit,
		Head: s.HeadRefOid, Base: s.BaseRefName, Title: s.Title,
		Mergeable: s.Mergeable, MergeState: s.MergeStateStatus,
		AutoMerge: s.AutoMerge.Enabled, AutoMethod: s.AutoMerge.Method,
		Checks: s.Checks.State, Comments: s.Comments.Total, Review: review,
		Incomplete: s.Incomplete,
	}
	am.Requests = append(am.Requests, s.ReviewRequests...)
	sort.Strings(am.Requests)
	for _, c := range s.Checks.Contexts {
		am.Contexts = append(am.Contexts, c.Kind+"|"+c.Name+"|"+c.Status+"|"+c.Conclusion)
	}
	sort.Strings(am.Contexts)
	if n := len(s.Comments.Recent); n > 0 {
		last := s.Comments.Recent[n-1]
		am.LastComment = last.Author + "|" + last.CreatedAt.UTC().Format(time.RFC3339)
	}
	return Token{All: hashOf(am)[:16], Review: review}
}

func hashOf(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // material structs always marshal
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Conditions accepted by --for.
const (
	ForChange    = "change"
	ForChecks    = "checks"
	ForReview    = "review"
	ForMergeable = "mergeable"
	ForMerged    = "merged"
	ForClosed    = "closed"
)

// ValidFor reports whether c is a known --for condition.
func ValidFor(c string) bool {
	switch c {
	case ForChange, ForChecks, ForReview, ForMergeable, ForMerged, ForClosed:
		return true
	}
	return false
}

// Waiter evaluates a --for condition over a stream of snapshots.
//
// Without --since, "change" and "review" compare against the first snapshot
// seen; the other conditions are level-triggered and can be met at once.
// With --since, every condition additionally requires that the state differs
// from the token, and "change"/"review" compare against the token.
type Waiter struct {
	For      string
	Since    *Token
	baseline *Token
}

// Met reports whether s satisfies the condition. An incomplete snapshot
// never does, and never becomes the baseline.
func (w *Waiter) Met(s *Snapshot) bool {
	if s.Incomplete {
		return false
	}
	cur := ComputeToken(s)
	ref := w.Since
	if ref == nil {
		if w.baseline == nil {
			b := cur
			w.baseline = &b
		}
		ref = w.baseline
	}
	switch w.For {
	case ForChange, "":
		return cur.All != ref.All
	case ForReview:
		return cur.Review != ref.Review
	}
	if w.Since != nil && cur.All == w.Since.All {
		return false
	}
	switch w.For {
	case ForChecks:
		return ChecksSettled(s)
	case ForMergeable:
		return Ready(s)
	case ForMerged:
		return s.Merged
	case ForClosed:
		return s.State != "OPEN"
	}
	return false
}

// Changes describes what differs between prev and cur, for event streams.
// A nil prev yields ["initial"].
func Changes(prev, cur *Snapshot) []string {
	if prev == nil {
		return []string{"initial"}
	}
	var out []string
	if prev.State != cur.State {
		out = append(out, "state "+prev.State+"→"+cur.State)
	}
	if prev.IsDraft != cur.IsDraft {
		out = append(out, fmt.Sprintf("draft %v", cur.IsDraft))
	}
	if prev.HeadRefOid != cur.HeadRefOid {
		out = append(out, "head "+short(cur.HeadRefOid))
	}
	if prev.BaseRefName != cur.BaseRefName {
		out = append(out, "base "+cur.BaseRefName)
	}
	if prev.Title != cur.Title {
		out = append(out, "title")
	}
	if prev.Checks.State != cur.Checks.State {
		out = append(out, "checks "+prev.Checks.State+"→"+cur.Checks.State)
	} else if strings.Join(contextKeys(prev), ",") != strings.Join(contextKeys(cur), ",") {
		out = append(out, "checks "+cur.Checks.State+" ("+checkCounts(cur)+")")
	}
	if deref(prev.ReviewDecision) != deref(cur.ReviewDecision) {
		out = append(out, "review "+orNone(deref(cur.ReviewDecision)))
	} else if prev.ReviewCount != cur.ReviewCount {
		out = append(out, fmt.Sprintf("reviews +%d", cur.ReviewCount-prev.ReviewCount))
	}
	if prev.Threads.Unresolved != cur.Threads.Unresolved || prev.Threads.Total != cur.Threads.Total {
		out = append(out, fmt.Sprintf("threads %d unresolved", cur.Threads.Unresolved))
	}
	if prev.Mergeable != cur.Mergeable || prev.MergeStateStatus != cur.MergeStateStatus {
		out = append(out, "mergeable "+cur.Mergeable+"/"+cur.MergeStateStatus)
	}
	if prev.AutoMerge.Enabled != cur.AutoMerge.Enabled {
		out = append(out, fmt.Sprintf("auto-merge %s", onOff(cur.AutoMerge.Enabled)))
	}
	if prev.Comments.Total != cur.Comments.Total {
		out = append(out, fmt.Sprintf("comments %+d", cur.Comments.Total-prev.Comments.Total))
	}
	if strings.Join(prev.ReviewRequests, ",") != strings.Join(cur.ReviewRequests, ",") {
		out = append(out, "review requests")
	}
	if len(out) == 0 {
		out = append(out, "updated")
	}
	return out
}

func contextKeys(s *Snapshot) []string {
	var k []string
	for _, c := range s.Checks.Contexts {
		k = append(k, c.Name+c.Status+c.Conclusion)
	}
	sort.Strings(k)
	return k
}

func checkCounts(s *Snapshot) string {
	var pass, fail, pending int
	for _, c := range s.Checks.Contexts {
		switch {
		case c.Failed():
			fail++
		case c.Status != "COMPLETED":
			pending++
		default:
			pass++
		}
	}
	parts := []string{fmt.Sprintf("%d passed", pass)}
	if fail > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", fail))
	}
	if pending > 0 {
		parts = append(parts, fmt.Sprintf("%d pending", pending))
	}
	return strings.Join(parts, ", ")
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// Summary returns a short one-line state summary.
func Summary(s *Snapshot) string {
	state := s.State
	if s.IsDraft && s.State == "OPEN" {
		state = "DRAFT"
	}
	need := "no"
	if s.NeedsAction {
		need = strings.Join(s.Reasons, ",")
	}
	out := fmt.Sprintf("%s  checks %s  review %s  needs action: %s",
		state, s.Checks.State, orNone(deref(s.ReviewDecision)), need)
	if s.Incomplete {
		out += "  (incomplete)"
	}
	return out
}

// Format renders a multi-line human-readable view of s.
func Format(s *Snapshot) string {
	var b strings.Builder
	state := s.State
	if s.IsDraft && s.State == "OPEN" {
		state += " (draft)"
	}
	fmt.Fprintf(&b, "%s  %s  %s\n", s.PR, state, s.Title)
	fmt.Fprintf(&b, "  %s\n", s.URL)
	checks := s.Checks.State
	if s.Checks.Total > 0 {
		checks += " (" + checkCounts(s) + ")"
	}
	fmt.Fprintf(&b, "  checks: %s\n", checks)
	for _, c := range s.Checks.Contexts {
		if c.Failed() {
			req := ""
			if c.Required != nil && *c.Required {
				req = " (required)"
			}
			fmt.Fprintf(&b, "    failed: %s%s\n", c.Name, req)
		}
	}
	am := "off"
	if s.AutoMerge.Enabled {
		am = "on"
		if s.AutoMerge.Method != nil {
			am += " (" + strings.ToLower(*s.AutoMerge.Method) + ")"
		}
	}
	fmt.Fprintf(&b, "  review: %s  mergeable: %s/%s  auto-merge: %s\n",
		orNone(deref(s.ReviewDecision)), s.Mergeable, s.MergeStateStatus, am)
	if len(s.ReviewRequests) > 0 {
		fmt.Fprintf(&b, "  review requested: %s\n", strings.Join(s.ReviewRequests, ", "))
	}
	fmt.Fprintf(&b, "  threads: %d unresolved of %d  comments: %d  head: %s\n",
		s.Threads.Unresolved, s.Threads.Total, s.Comments.Total, short(s.HeadRefOid))
	for _, t := range s.Threads.Items {
		loc := t.Path
		if t.Line != nil {
			loc = fmt.Sprintf("%s:%d", t.Path, *t.Line)
		}
		fmt.Fprintf(&b, "    %s  %s: %s\n", loc, t.Author, t.Excerpt)
	}
	if s.Incomplete {
		fmt.Fprintf(&b, "  incomplete: %s\n", s.IncompleteReason)
	}
	if s.NeedsAction {
		fmt.Fprintf(&b, "  needs action: yes (%s)\n", strings.Join(s.Reasons, ", "))
	} else {
		fmt.Fprintf(&b, "  needs action: no\n")
	}
	fmt.Fprintf(&b, "token: %s\n", s.Token)
	return b.String()
}

// Excerpt collapses whitespace and truncates text to n runes.
func Excerpt(text string, n int) string {
	text = strings.Join(strings.Fields(text), " ")
	r := []rune(text)
	if len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return text
}
