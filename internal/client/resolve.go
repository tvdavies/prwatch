package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/tvdavies/prwatch/internal/protocol"
)

// Where a daemon binary was found, in the order they are tried.
const (
	// ViaExecutable is the binary this process was started from.
	ViaExecutable = "executable"
	// ViaEnv is the PRWATCH_BIN override.
	ViaEnv = "PRWATCH_BIN"
	// ViaPath is prwatch on PATH.
	ViaPath = "PATH"
	// ViaNpm is the bin of the @tvdavies/prwatch package that the running
	// binary was installed with, found from the running binary's path.
	ViaNpm = "npm"
)

// Resolved is the binary chosen to start a daemon from.
type Resolved struct {
	Path    string
	Via     string // ViaExecutable, ViaEnv, ViaPath or ViaNpm
	Version string // as the binary reported it
	// Skipped says why each earlier candidate was passed over.
	Skipped []string
}

// ErrNoBinary is wrapped by ResolveExecutable's error when no candidate is
// usable.
var ErrNoBinary = errors.New("no usable prwatch binary to start the daemon from")

// ResolveExecutable picks the binary to start a daemon from. It tries, in
// order:
//
//  1. the binary this process was started from, if it is still there. On
//     Linux, os.Executable reads /proc/self/exe, which gains a " (deleted)"
//     suffix once the binary has been replaced; the suffix is stripped, so a
//     binary replaced in place resolves to the new one. A path inside a
//     package directory npm is replacing (node_modules/@tvdavies/.prwatch-*)
//     is skipped: npm is about to delete it.
//  2. PRWATCH_BIN, if set;
//  3. prwatch on PATH;
//  4. the npm launcher: when the running binary came from the npm package,
//     node_modules/@tvdavies/prwatch/bin/prwatch in the same node_modules.
//     npm upgrades by renaming the package directory and deleting it, so
//     the running binary's own path disappears; this is where the new
//     version is.
//
// A candidate is used only if it is an executable file and `version --json`
// reports the protocol version this client speaks.
//
// The check is skipped for the binary this process was started from while
// it is still the same executable file, which needs no exec. Checks stop
// when ctx is done.
func ResolveExecutable(ctx context.Context) (Resolved, error) {
	exe, err := os.Executable()
	return resolver{
		ctx:      ctx,
		exe:      exe,
		exeErr:   err,
		getenv:   os.Getenv,
		lookPath: exec.LookPath,
		check:    CheckBinary,
		self:     isSelf,
	}.resolve()
}

// Version is this binary's version, for logs; cli.Main sets it.
var Version = "dev"

// self is the file this process was started from, as it was at start-up.
var self = func() os.FileInfo {
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	fi, err := os.Stat(exe)
	if err != nil {
		return nil
	}
	return fi
}()

// isSelf reports whether path is still the very file this process was
// started from: same file, size and modification time.
func isSelf(path string) bool {
	if self == nil {
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 &&
		os.SameFile(fi, self) && fi.Size() == self.Size() && fi.ModTime().Equal(self.ModTime())
}

type resolver struct {
	ctx      context.Context
	exe      string
	exeErr   error
	getenv   func(string) string
	lookPath func(string) (string, error)
	check    func(ctx context.Context, path string) (protocol.BinaryInfo, error)
	self     func(path string) bool // nil: never
}

// deletedSuffix is what Linux appends to /proc/self/exe once the running
// binary has been unlinked.
const deletedSuffix = " (deleted)"

func (r resolver) resolve() (Resolved, error) {
	var res Resolved
	ctx := r.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	try := func(via, path string) bool {
		if err := ctx.Err(); err != nil {
			res.Skipped = append(res.Skipped, fmt.Sprintf("%s %s: not checked: %v", via, path, err))
			return false
		}
		info, err := r.check(ctx, path)
		if err != nil {
			res.Skipped = append(res.Skipped, fmt.Sprintf("%s %s: %v", via, path, err))
			return false
		}
		res.Path, res.Via, res.Version = path, via, info.Version
		return true
	}

	exe := strings.TrimSuffix(r.exe, deletedSuffix)
	switch {
	case r.exeErr != nil:
		res.Skipped = append(res.Skipped, fmt.Sprintf("%s: %v", ViaExecutable, r.exeErr))
	case npmRetired(exe):
		res.Skipped = append(res.Skipped, fmt.Sprintf("%s %s: npm is replacing this package", ViaExecutable, exe))
	case r.self != nil && r.self(exe):
		// Still the binary we are running: it speaks our protocol.
		res.Path, res.Via, res.Version = exe, ViaExecutable, Version
		return res, nil
	default:
		if try(ViaExecutable, exe) {
			return res, nil
		}
	}

	if p := r.getenv("PRWATCH_BIN"); p != "" {
		if abs, err := r.absolute(p); err != nil {
			res.Skipped = append(res.Skipped, fmt.Sprintf("%s %s: %v", ViaEnv, p, err))
		} else if try(ViaEnv, abs) {
			return res, nil
		}
	}

	if p, err := r.lookPath("prwatch"); err != nil {
		res.Skipped = append(res.Skipped, fmt.Sprintf("%s: prwatch not found", ViaPath))
	} else if abs, err := filepath.Abs(p); err == nil && try(ViaPath, abs) {
		return res, nil
	}

	if p := npmLauncher(exe); p != "" && try(ViaNpm, p) {
		return res, nil
	}
	return res, fmt.Errorf("%w (%s)", ErrNoBinary, strings.Join(res.Skipped, "; "))
}

// absolute makes a PRWATCH_BIN value absolute, looking a bare name up on
// PATH: the daemon is started from "/", so a relative path would not work.
func (r resolver) absolute(p string) (string, error) {
	if !strings.Contains(p, "/") {
		lp, err := r.lookPath(p)
		if err != nil {
			return "", errors.New("not found on PATH")
		}
		p = lp
	}
	return filepath.Abs(p)
}

// npmPackage matches the directory of the main package or a platform
// package under node_modules/@tvdavies, including the dot-prefixed name npm
// renames it to while it replaces it (.prwatch-Qm7hs3aq).
var npmPackage = regexp.MustCompile(`^\.?prwatch(-[A-Za-z0-9_-]+)?$`)

// npmScopeIndex returns the index of the package directory in parts (the
// segment after node_modules/@tvdavies), outermost first, or -1.
func npmScopeIndex(parts []string) int {
	for i := 0; i+2 < len(parts); i++ {
		if parts[i] == "node_modules" && parts[i+1] == "@tvdavies" && npmPackage.MatchString(parts[i+2]) {
			return i + 2
		}
	}
	return -1
}

// npmRetired reports whether path is inside a package directory npm has
// renamed out of the way and is about to delete.
func npmRetired(path string) bool {
	parts := strings.Split(filepath.ToSlash(path), "/")
	for i := 0; i+2 < len(parts); i++ {
		if parts[i] == "node_modules" && parts[i+1] == "@tvdavies" && strings.HasPrefix(parts[i+2], ".prwatch") {
			return true
		}
	}
	return false
}

// npmLauncher returns the bin of the @tvdavies/prwatch package in the
// outermost node_modules that holds the binary at path, or "" if path is
// not inside an npm install of prwatch. It covers the native binary that
// postinstall puts in the main package's bin, the platform package's binary
// (nested under the main package, or hoisted beside it), and either one
// inside a package directory npm has renamed while upgrading.
func npmLauncher(path string) string {
	parts := strings.Split(filepath.ToSlash(path), "/")
	i := npmScopeIndex(parts)
	if i < 0 || !filepath.IsAbs(path) {
		return ""
	}
	return strings.Join(parts[:i-1], "/") + "/@tvdavies/prwatch/bin/prwatch"
}

// CheckBinary checks that path is an executable prwatch that speaks this
// client's protocol, by running `path version --json`. It gives up after
// 10s, or sooner if ctx is done.
func CheckBinary(ctx context.Context, path string) (protocol.BinaryInfo, error) {
	var info protocol.BinaryInfo
	fi, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return info, errors.New("not found")
	case err != nil:
		return info, err
	case !fi.Mode().IsRegular():
		return info, errors.New("not a regular file")
	case fi.Mode().Perm()&0o111 == 0:
		return info, errors.New("not executable")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "version", "--json")
	var out bytes.Buffer
	cmd.Stdout = &out
	// Don't wait on grandchildren holding stdout once the check is killed.
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		return info, fmt.Errorf("version --json: %w", err)
	}
	info, err = ParseVersion(out.Bytes())
	if err != nil {
		return info, err
	}
	if info.Protocol != protocol.Version {
		return info, fmt.Errorf("version %s speaks protocol %d, not %d", info.Version, info.Protocol, protocol.Version)
	}
	return info, nil
}

// legacyVersion is what `prwatch version` printed before --json existed
// (0.1.2 and earlier), ignoring the flag. Those versions all speak
// protocol 1.
var legacyVersion = regexp.MustCompile(`^prwatch (\S+) \(`)

// ParseVersion decodes the output of `prwatch version --json`.
func ParseVersion(out []byte) (protocol.BinaryInfo, error) {
	var info protocol.BinaryInfo
	if err := json.Unmarshal(bytes.TrimSpace(out), &info); err == nil && info.Protocol > 0 {
		return info, nil
	}
	if m := legacyVersion.FindSubmatch(out); m != nil {
		return protocol.BinaryInfo{Version: string(m[1]), Protocol: 1}, nil
	}
	return info, fmt.Errorf("unexpected output from version --json: %q", firstLine(out))
}

func firstLine(b []byte) string {
	s, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	if len(s) > 80 {
		s = s[:80] + "…"
	}
	return s
}
