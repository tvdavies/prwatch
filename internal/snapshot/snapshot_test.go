package snapshot

import (
	"reflect"
	"testing"
	"time"
)

func base() *Snapshot {
	req := true
	s := &Snapshot{
		PR: "o/r#1", State: "OPEN", Title: "t", HeadRefOid: "abc", BaseRefName: "main",
		Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN",
		Checks: Checks{State: "SUCCESS", Total: 1, Contexts: []Check{{Name: "ci", Kind: "check_run", Status: "COMPLETED", Conclusion: "SUCCESS", Required: &req}}},
	}
	s.Finalise()
	return s
}

func mod(s *Snapshot, f func(*Snapshot)) *Snapshot {
	c := *s
	c.Checks.Contexts = append([]Check(nil), s.Checks.Contexts...)
	c.Threads.Items = append([]Thread(nil), s.Threads.Items...)
	f(&c)
	c.Finalise()
	return &c
}

func str(s string) *string { return &s }

func TestTokenIgnoresNonMaterialFields(t *testing.T) {
	a := base()
	b := mod(a, func(s *Snapshot) {
		s.FetchedAt = time.Now()
		s.UpdatedAt = time.Now()
		s.Threads.Items = append(s.Threads.Items, Thread{ID: "x"})
		s.Threads.Items = s.Threads.Items[:0]
	})
	if a.Token != b.Token {
		t.Fatal("token changed for non-material fields")
	}
	c := mod(a, func(s *Snapshot) { s.Title = "new" })
	if a.Token == c.Token {
		t.Fatal("token unchanged after title change")
	}
	ta, _ := ParseToken(a.Token)
	tc, _ := ParseToken(c.Token)
	if ta.Review != tc.Review {
		t.Fatal("review part changed for a non-review change")
	}
	d := mod(a, func(s *Snapshot) { s.ReviewCount++ })
	td, _ := ParseToken(d.Token)
	if td.Review == ta.Review {
		t.Fatal("review part unchanged after a new review")
	}
}

func TestTokenStableAcrossContextOrder(t *testing.T) {
	a := mod(base(), func(s *Snapshot) {
		s.Checks.Contexts = append(s.Checks.Contexts, Check{Name: "lint", Status: "COMPLETED", Conclusion: "SUCCESS"})
	})
	b := mod(a, func(s *Snapshot) {
		s.Checks.Contexts[0], s.Checks.Contexts[1] = s.Checks.Contexts[1], s.Checks.Contexts[0]
	})
	if a.Token != b.Token {
		t.Fatal("token depends on context order")
	}
}

func TestParseToken(t *testing.T) {
	tok := base().Token
	if _, err := ParseToken(tok); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "1.abc.def", "2.0123456789abcdef.01234567", "1.0123456789abcdeg.01234567"} {
		if _, err := ParseToken(bad); err == nil {
			t.Errorf("ParseToken(%q) succeeded", bad)
		}
	}
}

func TestReasons(t *testing.T) {
	no := false
	cases := []struct {
		name string
		f    func(*Snapshot)
		want []string
	}{
		{"ready without auto-merge", func(s *Snapshot) {}, []string{ReasonReadyAutoMergeOff}},
		{"ready with auto-merge", func(s *Snapshot) { s.AutoMerge.Enabled = true }, []string{}},
		{"required check failed", func(s *Snapshot) {
			s.Checks.State = "FAILURE"
			s.Checks.Contexts[0].Conclusion = "FAILURE"
		}, []string{ReasonRequiredCheckFailed}},
		{"optional check failed", func(s *Snapshot) {
			s.Checks.Contexts[0].Conclusion = "TIMED_OUT"
			s.Checks.Contexts[0].Required = &no
			s.AutoMerge.Enabled = true
		}, []string{ReasonCheckFailed}},
		{"changes requested", func(s *Snapshot) { s.ReviewDecision = str("CHANGES_REQUESTED") }, []string{ReasonChangesRequested}},
		{"unresolved threads", func(s *Snapshot) {
			s.Threads.Items = []Thread{{ID: "t"}}
			s.Threads.Unresolved = 1
		}, []string{ReasonUnresolvedThreads}},
		{"conflict", func(s *Snapshot) { s.Mergeable = "CONFLICTING"; s.MergeStateStatus = "DIRTY" }, []string{ReasonConflict}},
		{"review required is not ready", func(s *Snapshot) { s.ReviewDecision = str("REVIEW_REQUIRED") }, []string{}},
		{"draft is not ready", func(s *Snapshot) { s.IsDraft = true }, []string{}},
		{"merged needs nothing", func(s *Snapshot) { s.State = "MERGED"; s.Merged = true; s.Mergeable = "CONFLICTING" }, []string{}},
	}
	for _, c := range cases {
		s := mod(base(), c.f)
		if !reflect.DeepEqual(s.Reasons, c.want) {
			t.Errorf("%s: reasons %v, want %v", c.name, s.Reasons, c.want)
		}
		if s.NeedsAction != (len(c.want) > 0) {
			t.Errorf("%s: needsAction %v", c.name, s.NeedsAction)
		}
	}
}

func TestWaiterChange(t *testing.T) {
	w := &Waiter{For: ForChange}
	a := base()
	if w.Met(a) {
		t.Fatal("first snapshot should be the baseline")
	}
	if w.Met(a) {
		t.Fatal("same state met")
	}
	if !w.Met(mod(a, func(s *Snapshot) { s.Title = "x" })) {
		t.Fatal("change not met")
	}
}

func TestWaiterSince(t *testing.T) {
	a := base()
	tok, _ := ParseToken(a.Token)
	b := mod(a, func(s *Snapshot) { s.Title = "x" })
	if (&Waiter{For: ForChange, Since: &tok}).Met(a) {
		t.Fatal("since: unchanged state met")
	}
	if !(&Waiter{For: ForChange, Since: &tok}).Met(b) {
		t.Fatal("since: changed state should be met at once")
	}
	// Level conditions with --since need a state different from the token.
	merged := mod(a, func(s *Snapshot) { s.State = "MERGED"; s.Merged = true })
	mt, _ := ParseToken(merged.Token)
	if (&Waiter{For: ForMerged, Since: &mt}).Met(merged) {
		t.Fatal("since: merged state equal to token met")
	}
	if !(&Waiter{For: ForMerged, Since: &tok}).Met(merged) {
		t.Fatal("since: newly merged not met")
	}
	// review compares only the review part.
	if (&Waiter{For: ForReview, Since: &tok}).Met(b) {
		t.Fatal("review met by a title change")
	}
	if !(&Waiter{For: ForReview, Since: &tok}).Met(mod(a, func(s *Snapshot) { s.ReviewCount = 3 })) {
		t.Fatal("review not met by a new review")
	}
}

func TestWaiterLevelConditions(t *testing.T) {
	pending := mod(base(), func(s *Snapshot) {
		s.Checks.State = "PENDING"
		s.Checks.Contexts[0].Status = "IN_PROGRESS"
		s.Checks.Contexts[0].Conclusion = ""
	})
	cases := []struct {
		cond string
		snap *Snapshot
		want bool
	}{
		{ForChecks, pending, false},
		{ForChecks, mod(pending, func(s *Snapshot) { s.Checks.State = "EXPECTED" }), false},
		{ForChecks, base(), true},
		{ForChecks, mod(base(), func(s *Snapshot) { s.Checks = Checks{} }), true},
		{ForMergeable, base(), true},
		{ForMergeable, pending, false},
		{ForMergeable, mod(base(), func(s *Snapshot) { s.MergeStateStatus = "BEHIND" }), false},
		{ForMergeable, mod(base(), func(s *Snapshot) { s.ReviewDecision = str("APPROVED") }), true},
		{ForMerged, base(), false},
		{ForMerged, mod(base(), func(s *Snapshot) { s.Merged = true; s.State = "MERGED" }), true},
		{ForClosed, base(), false},
		{ForClosed, mod(base(), func(s *Snapshot) { s.State = "CLOSED" }), true},
		{ForClosed, mod(base(), func(s *Snapshot) { s.State = "MERGED"; s.Merged = true }), true},
	}
	for i, c := range cases {
		if got := (&Waiter{For: c.cond}).Met(c.snap); got != c.want {
			t.Errorf("case %d (%s): got %v, want %v", i, c.cond, got, c.want)
		}
	}
}

func TestChanges(t *testing.T) {
	a := mod(base(), func(s *Snapshot) { s.Checks.State = "PENDING" })
	b := mod(a, func(s *Snapshot) {
		s.Checks.State = "SUCCESS"
		s.ReviewDecision = str("APPROVED")
		s.Comments.Total = 2
	})
	got := Changes(a, b)
	want := []string{"checks PENDING→SUCCESS", "review APPROVED", "comments +2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Changes = %v, want %v", got, want)
	}
	if got := Changes(nil, b); !reflect.DeepEqual(got, []string{"initial"}) {
		t.Fatalf("initial: %v", got)
	}
}

func TestExcerpt(t *testing.T) {
	if got := Excerpt("  hello\n\nworld  ", 50); got != "hello world" {
		t.Fatalf("%q", got)
	}
	if got := Excerpt("abcdef", 4); got != "abc…" {
		t.Fatalf("%q", got)
	}
}
