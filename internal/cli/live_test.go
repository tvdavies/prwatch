package cli_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/tvdavies/prwatch/internal/snapshot"
)

// TestLiveStatus makes one real GitHub request. It is skipped unless
// PRWATCH_LIVE=1; it needs GH_TOKEN, GITHUB_TOKEN or a gh login.
func TestLiveStatus(t *testing.T) {
	if os.Getenv("PRWATCH_LIVE") != "1" {
		t.Skip("set PRWATCH_LIVE=1 to run against api.github.com")
	}
	pr := os.Getenv("PRWATCH_LIVE_PR")
	if pr == "" {
		pr = "cli/cli#9000" // merged, public
	}
	dir, err := os.MkdirTemp("/tmp", "pwlive")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	c := exec.Command(os.Args[0], "status", pr, "--json")
	c.Env = append(os.Environ(), "PRWATCH_TEST_EXEC=1", "PRWATCH_STATE_DIR="+dir, "PRWATCH_GRAPHQL_URL=")
	out, err := c.Output()
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	var snaps []snapshot.Snapshot
	if err := json.Unmarshal(out, &snaps); err != nil || len(snaps) != 1 {
		t.Fatalf("decode %q: %v", out, err)
	}
	s := snaps[0]
	if !strings.EqualFold(s.PR, pr) || s.SchemaVersion != 1 || s.Token == "" || s.State == "" {
		t.Fatalf("unexpected snapshot: %+v", s)
	}
	if pr == "cli/cli#9000" && (!s.Merged || s.State != "MERGED" || s.MergeCommit == nil) {
		t.Fatalf("cli/cli#9000 should be merged: %+v", s)
	}
	rate, err := os.ReadFile(dir + "/rate.json")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("snapshot: %s; rate: %s", snapshot.Summary(&s), strings.Join(strings.Fields(string(rate)), " "))
}
