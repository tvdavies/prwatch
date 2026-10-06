package client

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveExecutable(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "prwatch")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	noPath := func(string) (string, error) { return "", errors.New("not on PATH") }
	onPath := func(string) (string, error) { return "/usr/local/bin/prwatch", nil }

	cases := []struct {
		name, exe string
		look      func(string) (string, error)
		want      string
		wantErr   bool
	}{
		{"plain path", bin, noPath, bin, false},
		// What /proc/self/exe reads once npm has replaced the binary.
		{"replaced on Linux", bin + " (deleted)", noPath, bin, false},
		{"gone, on PATH", filepath.Join(dir, "gone") + " (deleted)", onPath, "/usr/local/bin/prwatch", false},
		{"gone, not on PATH", filepath.Join(dir, "gone"), noPath, "", true},
		{"directory", dir, onPath, "/usr/local/bin/prwatch", false},
	}
	for _, c := range cases {
		got, err := resolveExecutable(c.exe, c.look)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("%s: got %q, %v; want %q (error %v)", c.name, got, err, c.want, c.wantErr)
		}
	}
}
