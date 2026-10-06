package client

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tvdavies/prwatch/internal/protocol"
)

// fakeResolver resolves against a set of usable binaries instead of running
// them; everything else fails the check.
func fakeResolver(exe string, env map[string]string, onPath string, good ...string) resolver {
	ok := map[string]bool{}
	for _, g := range good {
		ok[g] = true
	}
	return resolver{
		exe:    exe,
		getenv: func(k string) string { return env[k] },
		lookPath: func(name string) (string, error) {
			if onPath == "" {
				return "", errors.New("not on PATH")
			}
			return filepath.Join(onPath, name), nil
		},
		check: func(p string) (protocol.BinaryInfo, error) {
			if ok[p] {
				return protocol.BinaryInfo{Version: "v-" + filepath.Base(filepath.Dir(p)), Protocol: protocol.Version}, nil
			}
			return protocol.BinaryInfo{}, errors.New("not usable")
		},
	}
}

func TestResolveOrder(t *testing.T) {
	const (
		exe    = "/opt/pw/bin/prwatch"
		envBin = "/srv/override/prwatch"
		pathed = "/usr/local/bin/prwatch"
		// A global npm install: the bin npm links onto PATH, its package
		// directory, and the same directory renamed while npm upgrades it.
		npmRoot     = "/home/u/.nvm/lib/node_modules"
		npmBin      = npmRoot + "/@tvdavies/prwatch/bin/prwatch"
		npmRetired  = npmRoot + "/@tvdavies/.prwatch-Qm7hs3aq/bin/prwatch"
		npmPlatform = npmRoot + "/@tvdavies/.prwatch-Qm7hs3aq/node_modules/@tvdavies/prwatch-linux-x64/bin/prwatch"
	)
	withBin := map[string]string{"PRWATCH_BIN": envBin}
	cases := []struct {
		name    string
		r       resolver
		want    string
		via     string
		skipped int
	}{
		{"(a) executable still there", fakeResolver(exe, withBin, "/usr/local/bin", exe, envBin, pathed), exe, ViaExecutable, 0},
		{"(a) replaced in place on Linux", fakeResolver(exe+" (deleted)", nil, "", exe), exe, ViaExecutable, 0},
		{"(b) executable gone, PRWATCH_BIN", fakeResolver(exe+" (deleted)", withBin, "/usr/local/bin", envBin, pathed), envBin, ViaEnv, 1},
		{"(b) bare name in PRWATCH_BIN is looked up on PATH",
			fakeResolver(exe+" (deleted)", map[string]string{"PRWATCH_BIN": "prwatch"}, "/usr/local/bin", pathed), pathed, ViaEnv, 1},
		{"(c) PRWATCH_BIN unusable, PATH", fakeResolver(exe+" (deleted)", withBin, "/usr/local/bin", pathed), pathed, ViaPath, 2},
		{"(c) executable incompatible, PATH", fakeResolver(exe, nil, "/usr/local/bin", pathed), pathed, ViaPath, 1},
		{"(d) npm upgrade, nothing on PATH", fakeResolver(npmRetired+" (deleted)", nil, "", npmBin), npmBin, ViaNpm, 2},
		{"(d) npm upgrade, platform binary (JS launcher install)", fakeResolver(npmPlatform+" (deleted)", nil, "", npmBin), npmBin, ViaNpm, 2},
		// npm has renamed the package but not deleted it yet: the old binary
		// is still there, but it is not used.
		{"(d) retired package dir still present", fakeResolver(npmRetired, nil, "", npmRetired, npmBin), npmBin, ViaNpm, 2},
		{"(c) before (d)", fakeResolver(npmRetired+" (deleted)", nil, "/usr/local/bin", pathed, npmBin), pathed, ViaPath, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.r.resolve()
			if err != nil {
				t.Fatalf("error %v", err)
			}
			if got.Path != c.want || got.Via != c.via || len(got.Skipped) != c.skipped || got.Version == "" {
				t.Fatalf("got %+v; want %s via %s with %d skipped", got, c.want, c.via, c.skipped)
			}
		})
	}

	t.Run("nothing usable", func(t *testing.T) {
		r := fakeResolver(npmRetired+" (deleted)", withBin, "/usr/local/bin")
		_, err := r.resolve()
		if !errors.Is(err, ErrNoBinary) {
			t.Fatalf("error %v, want ErrNoBinary", err)
		}
		for _, want := range []string{"executable " + npmRetired, "PRWATCH_BIN " + envBin, "PATH " + pathed, "npm " + npmBin} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error does not mention %q: %v", want, err)
			}
		}
	})
	t.Run("os.Executable fails", func(t *testing.T) {
		r := fakeResolver("", nil, "/usr/local/bin", pathed)
		r.exeErr = errors.New("no /proc")
		got, err := r.resolve()
		if err != nil || got.Via != ViaPath || !strings.Contains(got.Skipped[0], "no /proc") {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
}

func TestNpmLauncher(t *testing.T) {
	cases := map[string]string{
		// Native binary put in the main package's bin by postinstall.
		"/p/lib/node_modules/@tvdavies/prwatch/bin/prwatch": "/p/lib/node_modules/@tvdavies/prwatch/bin/prwatch",
		// The same, after npm renamed the package directory.
		"/p/lib/node_modules/@tvdavies/.prwatch-Qm7hs3aq/bin/prwatch": "/p/lib/node_modules/@tvdavies/prwatch/bin/prwatch",
		// The platform package's binary, nested (global) or hoisted (local).
		"/p/lib/node_modules/@tvdavies/prwatch/node_modules/@tvdavies/prwatch-darwin-arm64/bin/prwatch": "/p/lib/node_modules/@tvdavies/prwatch/bin/prwatch",
		"/app/node_modules/@tvdavies/prwatch-linux-arm64/bin/prwatch":                                   "/app/node_modules/@tvdavies/prwatch/bin/prwatch",
		"/app/node_modules/@tvdavies/.prwatch-linux-x64-AbC12345/bin/prwatch":                           "/app/node_modules/@tvdavies/prwatch/bin/prwatch",
		// Not an npm install of prwatch.
		"/usr/local/bin/prwatch":                         "",
		"/p/node_modules/@other/prwatch/bin/prwatch":     "",
		"/p/node_modules/@tvdavies/prwatchful/bin/x":     "",
		"relative/node_modules/@tvdavies/prwatch/bin/pw": "",
	}
	for in, want := range cases {
		if got := npmLauncher(in); got != want {
			t.Errorf("npmLauncher(%q) = %q, want %q", in, got, want)
		}
	}
}

func writeScript(t *testing.T, dir, name, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCheckBinary(t *testing.T) {
	dir := t.TempDir()
	current := fmt.Sprintf(`echo '{"version":"0.1.3","protocol":%d,"os":"linux","arch":"amd64"}'`, protocol.Version)
	cases := []struct {
		name, path, version, err string
	}{
		{"current", writeScript(t, dir, "current", current, 0o755), "0.1.3", ""},
		// 0.1.2 and earlier ignore --json and print text; they speak protocol 1.
		{"legacy text", writeScript(t, dir, "legacy", `echo "prwatch 0.1.2 (linux/amd64, go1.26.0)"`, 0o755), "0.1.2", ""},
		{"newer protocol", writeScript(t, dir, "future", `echo '{"version":"9.0.0","protocol":99}'`, 0o755), "", "speaks protocol 99"},
		{"not prwatch", writeScript(t, dir, "other", `echo hello`, 0o755), "", "unexpected output"},
		{"fails", writeScript(t, dir, "fails", `exit 3`, 0o755), "", "exit status 3"},
		{"not executable", writeScript(t, dir, "noexec", current, 0o644), "", "not executable"},
		{"directory", dir, "", "not a regular file"},
		{"missing", filepath.Join(dir, "missing"), "", "not found"},
	}
	for _, c := range cases {
		info, err := CheckBinary(c.path)
		switch {
		case c.err == "" && (err != nil || info.Version != c.version || info.Protocol != protocol.Version):
			t.Errorf("%s: got %+v, %v; want version %s", c.name, info, err, c.version)
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%s: error %v, want one containing %q", c.name, err, c.err)
		}
	}
}

// The real resolver, end to end on the filesystem: the running test binary
// is (a); with it unusable the order is PRWATCH_BIN, PATH, then npm.
func TestResolveWithRealChecks(t *testing.T) {
	dir := t.TempDir()
	good := fmt.Sprintf(`echo '{"version":"%%s","protocol":%d}'`, protocol.Version)
	envBin := writeScript(t, dir, "env-prwatch", fmt.Sprintf(good, "env"), 0o755)
	pathDir := filepath.Join(dir, "path")
	_ = os.Mkdir(pathDir, 0o755)
	writeScript(t, pathDir, "prwatch", fmt.Sprintf(good, "path"), 0o755)
	npmBin := filepath.Join(dir, "node_modules", "@tvdavies", "prwatch", "bin")
	_ = os.MkdirAll(npmBin, 0o755)
	writeScript(t, npmBin, "prwatch", fmt.Sprintf(good, "npm"), 0o755)
	gone := filepath.Join(dir, "node_modules", "@tvdavies", ".prwatch-Qm7hs3aq", "bin", "prwatch") + " (deleted)"

	env := map[string]string{"PRWATCH_BIN": envBin}
	r := resolver{exe: gone, getenv: func(k string) string { return env[k] }, check: CheckBinary,
		lookPath: func(string) (string, error) { return filepath.Join(pathDir, "prwatch"), nil }}
	for _, want := range []string{"env", "path", "npm"} {
		got, err := r.resolve()
		if err != nil || got.Version != want {
			t.Fatalf("want %s: got %+v, %v", want, got, err)
		}
		switch want {
		case "env":
			env = nil
		case "path":
			r.lookPath = func(string) (string, error) { return "", errors.New("not on PATH") }
		}
	}
}

// The binary this process runs is used without running it, as long as it
// is still the same file; a replacement at the same path is checked.
func TestResolveSelf(t *testing.T) {
	const exe = "/opt/pw/bin/prwatch"
	r := fakeResolver(exe, nil, "")
	r.self = func(p string) bool { return p == exe }
	got, err := r.resolve()
	if err != nil || got.Path != exe || got.Via != ViaExecutable || got.Version != Version {
		t.Fatalf("got %+v, %v", got, err)
	}

	running, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if !isSelf(running) {
		t.Fatalf("isSelf(%s) is false for the running binary", running)
	}
	copied := filepath.Join(t.TempDir(), "prwatch")
	b, err := os.ReadFile(running)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copied, b, 0o755); err != nil {
		t.Fatal(err)
	}
	if isSelf(copied) {
		t.Fatal("isSelf is true for a copy")
	}
}
