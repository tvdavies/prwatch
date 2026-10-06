// Package paths resolves prwatch's per-user state directory and the files in it.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// StateDir returns the per-user state directory, creating it with mode 0700.
//
// Resolution order: PRWATCH_STATE_DIR, $XDG_RUNTIME_DIR/prwatch, then
// ~/Library/Caches/prwatch on macOS or ~/.cache/prwatch elsewhere.
func StateDir() (string, error) {
	dir, err := stateDirPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create state dir: %w", err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	if fi.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return "", fmt.Errorf("restrict state dir permissions: %w", err)
		}
	}
	return dir, nil
}

func stateDirPath() (string, error) {
	if d := os.Getenv("PRWATCH_STATE_DIR"); d != "" {
		return d, nil
	}
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "prwatch"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Caches", "prwatch"), nil
	}
	return filepath.Join(home, ".cache", "prwatch"), nil
}

// Socket is the daemon's unix socket path.
func Socket(dir string) string { return filepath.Join(dir, "prwatch.sock") }

// Lock is the file the daemon holds an exclusive flock on for its lifetime.
func Lock(dir string) string { return filepath.Join(dir, "daemon.lock") }

// Log is the daemon's log file.
func Log(dir string) string { return filepath.Join(dir, "daemon.log") }

// Rate is the persisted rate-limit and back-off state.
func Rate(dir string) string { return filepath.Join(dir, "rate.json") }

// IDs is the persisted PR node id cache.
func IDs(dir string) string { return filepath.Join(dir, "ids.json") }
