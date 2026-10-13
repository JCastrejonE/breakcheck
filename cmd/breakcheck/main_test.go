//go:build !windows

package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// The binary is built once and driven as a user would drive it, because the
// properties under test (signals, a hard kill) only exist for a real process.
var bin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "breakcheck-bin-")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(dir, "breakcheck")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		panic(string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

const original = "limit = 10\n"

func project(t *testing.T, suite string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "guard.txt"), []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "suite.sh"), []byte("#!/bin/sh\n"+suite), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func start(t *testing.T, dir string, args ...string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd, &out
}

func exitOf(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

// waitMutated polls until the run has applied its mutation, so the signal
// lands while the suite is running and not before.
func waitMutated(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, _ := os.ReadFile(filepath.Join(dir, "guard.txt")); string(b) != original {
			time.Sleep(200 * time.Millisecond) // and the suite has started
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the mutation was never applied")
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var args = []string{"--file", "guard.txt", "--from", "limit = 10", "--to", "limit = 11", "--cmd", "./suite.sh"}

func TestCtrlCRestores(t *testing.T) {
	dir := project(t, "sleep 300\n")
	cmd, out := start(t, dir, args...)
	waitMutated(t, dir)
	_ = cmd.Process.Signal(syscall.SIGINT)
	if code := exitOf(cmd.Wait()); code != 2 {
		t.Fatalf("exit %d, want 2 (harness)\n%s", code, out)
	}
	if got := read(t, filepath.Join(dir, "guard.txt")); got != original {
		t.Fatalf("not restored after Ctrl-C: %q\n%s", got, out)
	}
}

// SIGKILL cannot be caught. The journal written before the mutation is what
// lets `breakcheck restore` put the bytes back afterwards.
func TestKillNineThenRestore(t *testing.T) {
	dir := project(t, "sleep 300\n")
	cmd, _ := start(t, dir, args...)
	waitMutated(t, dir)
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	exec.Command("pkill", "-f", filepath.Join(dir, "suite.sh")).Run() // the orphaned suite

	if got := read(t, filepath.Join(dir, "guard.txt")); got == original {
		t.Fatal("the file is already original; the kill landed before the mutation and proves nothing")
	}
	again, out := start(t, dir, args...)
	if code := exitOf(again.Wait()); code != 2 {
		t.Fatalf("a new run over a pending journal must refuse with 2, got %d\n%s", code, out)
	}
	restore, out := start(t, dir, "restore")
	if code := exitOf(restore.Wait()); code != 0 {
		t.Fatalf("restore exit %d\n%s", code, out)
	}
	if got := read(t, filepath.Join(dir, "guard.txt")); got != original {
		t.Fatalf("restore did not put the original back: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, ".breakcheck", "journal.json")); err == nil {
		t.Fatal("the journal outlived a successful restore")
	}
}

func TestExitCodes(t *testing.T) {
	cases := []struct {
		name, suite string
		want        int
	}{
		{"guarded", "grep -q 'limit = 10' guard.txt && echo 'ok 1 - limit' || { echo 'not ok 1 - limit'; exit 1; }\n", 0},
		{"not guarded", "echo 'ok 1 - unrelated'\n", 1},
		{"harness", "grep -q 'limit = 10' guard.txt || { echo 'parse error' >&2; exit 3; }\n", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := project(t, tc.suite)
			cmd, out := start(t, dir, args...)
			if code := exitOf(cmd.Wait()); code != tc.want {
				t.Fatalf("exit %d, want %d\n%s", code, tc.want, out)
			}
			if got := read(t, filepath.Join(dir, "guard.txt")); got != original {
				t.Fatalf("not restored: %q", got)
			}
		})
	}
}
