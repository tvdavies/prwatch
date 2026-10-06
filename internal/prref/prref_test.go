package prref

import (
	"errors"
	"testing"
)

func TestParse(t *testing.T) {
	remote := func() (string, error) { return "git@github.com:tvdavies/prwatch.git", nil }
	cases := []struct {
		in   string
		want string
	}{
		{"owner/repo#123", "owner/repo#123"},
		{"lleverage-ai/lleverage#7786", "lleverage-ai/lleverage#7786"},
		{"my.org-x/re_po.js#1", ""},
		{"o/re_po.js#1", "o/re_po.js#1"},
		{"https://github.com/cli/cli/pull/42", "cli/cli#42"},
		{"https://github.com/cli/cli/pull/42/files#diff-abc", "cli/cli#42"},
		{"https://github.com/cli/cli/pull/42?w=1", "cli/cli#42"},
		{"github.com/cli/cli/pull/7", "cli/cli#7"},
		{"http://github.com/cli/cli/pulls/7", "cli/cli#7"},
		{"#12", "tvdavies/prwatch#12"},
		{"12", "tvdavies/prwatch#12"},
		{" 12 ", "tvdavies/prwatch#12"},
		{"https://gitlab.com/a/b/pull/1", ""},
		{"https://github.com/cli/cli/issues/42", ""},
		{"owner/repo#0", ""},
		{"owner/repo", ""},
		{"owner#1", ""},
		{"", ""},
		{"-bad/repo#1", ""},
	}
	for _, c := range cases {
		got, err := Parse(c.in, remote)
		if c.want == "" {
			if err == nil {
				t.Errorf("Parse(%q) = %v, want error", c.in, got)
			}
			continue
		}
		if err != nil || got.String() != c.want {
			t.Errorf("Parse(%q) = %v, %v; want %s", c.in, got, err, c.want)
		}
	}
}

func TestParseNumberWithoutRemote(t *testing.T) {
	if _, err := Parse("12", func() (string, error) { return "", ErrNoRemote }); !errors.Is(err, ErrNoRemote) {
		t.Fatalf("got %v", err)
	}
	if _, err := Parse("12", func() (string, error) { return "https://gitlab.com/a/b.git", nil }); err == nil {
		t.Fatal("non-GitHub remote should fail")
	}
}

func TestParseRemote(t *testing.T) {
	cases := map[string]string{
		"git@github.com:tvdavies/prwatch.git":       "tvdavies/prwatch",
		"git@github.com:tvdavies/prwatch":           "tvdavies/prwatch",
		"https://github.com/tvdavies/prwatch.git":   "tvdavies/prwatch",
		"https://github.com/tvdavies/prwatch":       "tvdavies/prwatch",
		"https://user@github.com/tvdavies/prwatch/": "tvdavies/prwatch",
		"ssh://git@github.com/tvdavies/prwatch.git": "tvdavies/prwatch",
		"ssh://git@github.com:22/tvdavies/prwatch":  "tvdavies/prwatch",
		"git@gitlab.com:tvdavies/prwatch.git":       "",
		"/local/path":                               "",
		"https://github.com/tvdavies/prwatch/extra": "",
	}
	for in, want := range cases {
		o, r, ok := ParseRemote(in)
		got := ""
		if ok {
			got = o + "/" + r
		}
		if got != want {
			t.Errorf("ParseRemote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKeyIsCaseInsensitive(t *testing.T) {
	a, _ := Parse("Owner/Repo#1", nil)
	b, _ := Parse("owner/repo#1", nil)
	if a.Key() != b.Key() {
		t.Fatal("keys differ by case")
	}
}
