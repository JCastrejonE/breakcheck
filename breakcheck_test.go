//go:build !windows

package breakcheck

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// project builds a throwaway "codebase": guard.txt holds the line under test,
// and suite.sh is a TAP test suite whose behaviour each case chooses.
type project struct {
	t     *testing.T
	dir   string
	state string
}

const guardFile = "limit = 10\nmode = strict\n"

func newProject(t *testing.T, suite string) *project {
	t.Helper()
	dir := t.TempDir()
	p := &project{t: t, dir: dir, state: filepath.Join(dir, ".breakcheck")}
	p.write("guard.txt", guardFile)
	p.write("suite.sh", "#!/bin/sh\n"+suite)
	if err := os.Chmod(filepath.Join(dir, "suite.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func (p *project) write(name, body string) {
	p.t.Helper()
	path := filepath.Join(p.dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		p.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		p.t.Fatal(err)
	}
}

func (p *project) read(name string) string {
	p.t.Helper()
	b, err := os.ReadFile(filepath.Join(p.dir, name))
	if err != nil {
		p.t.Fatal(err)
	}
	return string(b)
}

func (p *project) check(from, to string) Check {
	return Check{File: filepath.Join(p.dir, "guard.txt"), From: from, To: to, Cmd: "./suite.sh", Dir: p.dir, Timeout: 20 * time.Second}
}

func (p *project) run(c Check) Result {
	p.t.Helper()
	var log bytes.Buffer
	res := Run(context.Background(), c, Options{StateDir: p.state, Log: &log})
	p.t.Logf("%s: %s\n%s", res.Verdict, res.Reason, log.String())
	return res
}

// mustBeRestored is the property every test ends on: whatever the verdict,
// the code under test is byte-for-byte what it was, and no journal is left.
func (p *project) mustBeRestored() {
	p.t.Helper()
	if got := p.read("guard.txt"); got != guardFile {
		p.t.Fatalf("guard.txt was not restored:\n%q", got)
	}
	if _, err := os.Stat(filepath.Join(p.state, journalName)); err == nil {
		p.t.Fatal("a journal was left behind after a clean run")
	}
}

// A suite that genuinely checks the line.
const guardingSuite = `if grep -q 'limit = 10' guard.txt; then echo "ok 1 - limit is 10"; else echo "not ok 1 - limit is 10"; exit 1; fi
echo "1..1"
`

func TestGuarded(t *testing.T) {
	p := newProject(t, guardingSuite)
	res := p.run(p.check("limit = 10", "limit = 11"))
	if res.Verdict != Guarded || res.Mutated.Failures != 1 || res.Restored.ExitCode != 0 {
		t.Fatalf("want guarded with 1 failure and a green restore, got %+v", res)
	}
	p.mustBeRestored()
}

func TestNotGuarded(t *testing.T) {
	p := newProject(t, "echo 'ok 1 - something else'\necho '1..1'\n")
	res := p.run(p.check("limit = 10", "limit = 11"))
	if res.Verdict != NotGuarded {
		t.Fatalf("want not-guarded, got %s: %s", res.Verdict, res.Reason)
	}
	p.mustBeRestored()
}

// The failure that motivated the tool: a break that does not compile makes the
// suite exit non-zero without running a single test. That is not a red test.
func TestCompileErrorIsNotRed(t *testing.T) {
	p := newProject(t, `if grep -q 'limit = 10' guard.txt; then echo "ok 1 - fine"; else echo "guard.txt:1: syntax error" >&2; exit 2; fi
`)
	res := p.run(p.check("limit = 10", "limit = "))
	if res.Verdict != Harness || !strings.Contains(res.Reason, "never ran") {
		t.Fatalf("want harness (suite never ran), got %s: %s", res.Verdict, res.Reason)
	}
	p.mustBeRestored()
}

func TestNoMatchAndAmbiguousMatchTouchNothing(t *testing.T) {
	for _, from := range []string{"limit = 99", "= "} { // absent; present twice
		p := newProject(t, guardingSuite)
		res := p.run(p.check(from, "x"))
		if res.Verdict != Harness || res.Mutated != nil {
			t.Fatalf("from=%q: want harness before any run, got %s: %s", from, res.Verdict, res.Reason)
		}
		p.mustBeRestored()
	}
}

// The suite is red even without the mutation, so a red run proves nothing.
func TestRedBaselineIsHarness(t *testing.T) {
	p := newProject(t, "echo 'not ok 1 - always broken'\nexit 1\n")
	res := p.run(p.check("limit = 10", "limit = 11"))
	if res.Verdict != Harness || !strings.Contains(res.Reason, "red on the restored code") {
		t.Fatalf("want harness (red baseline), got %s: %s", res.Verdict, res.Reason)
	}
	p.mustBeRestored()
}

// Regression for break-check.sh: its restore used a relative path, and a `cd`
// in the test command moved what that path meant, so the file stayed mutated.
func TestCdInCommandCannotMoveTheFile(t *testing.T) {
	// The suite finds guard.txt relative to itself, so it works from any directory.
	p := newProject(t, `f="$(dirname "$0")/guard.txt"
if grep -q 'limit = 10' "$f"; then echo "ok 1 - limit"; else echo "not ok 1 - limit"; exit 1; fi
`)
	p.write("sub/keep.txt", "")
	t.Chdir(p.dir)
	c := p.check("limit = 10", "limit = 11")
	c.File = "guard.txt" // relative: resolved against the directory breakcheck started in
	c.Cmd = "cd sub && ../suite.sh"
	if res := p.run(c); res.Verdict != Guarded {
		t.Fatalf("want guarded, got %s: %s", res.Verdict, res.Reason)
	}
	p.mustBeRestored()
}

// A suite that crashes before writing its report must not be credited with the
// failures in a report left by an earlier run. Read stale, this would come out
// "guarded" (old failures, then a green restored run); deleted first, the
// mutated run has no report at all and is correctly "never ran".
func TestStaleJUnitReportIsIgnored(t *testing.T) {
	p := newProject(t, `if grep -q 'limit = 10' guard.txt; then echo '<testsuite><testcase name="limit"/></testsuite>' > junit.xml; exit 0; fi
exit 1
`)
	p.write("junit.xml", `<testsuite><testcase name="old"><failure/></testcase></testsuite>`)
	c := p.check("limit = 10", "limit = 11")
	c.Report = "junit.xml"
	res := p.run(c)
	if res.Verdict != Harness || !strings.Contains(res.Reason, "never ran") {
		t.Fatalf("a report from an earlier run was counted: got %s: %s", res.Verdict, res.Reason)
	}
	p.mustBeRestored()
}

// A hung suite is killed with every process it started, and the file comes back.
func TestTimeoutKillsTheProcessGroup(t *testing.T) {
	p := newProject(t, "sleep 300 &\necho $! > child.pid\nwait\n")
	c := p.check("limit = 10", "limit = 11")
	c.Timeout = 3 * time.Second // generous: the shell must start its child before the deadline, even on a loaded machine
	res := p.run(c)
	if res.Verdict != Harness || !res.Mutated.TimedOut {
		t.Fatalf("want harness on timeout, got %s: %s", res.Verdict, res.Reason)
	}
	raw, err := os.ReadFile(filepath.Join(p.dir, "child.pid"))
	if err != nil {
		t.Fatalf("the suite never started its child within the timeout, so this run proves nothing: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if syscall.Kill(pid, 0) == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("the suite's child %d outlived the timeout", pid)
	}
	p.mustBeRestored()
}

func TestInterruptRestores(t *testing.T) {
	p := newProject(t, "sleep 300\n")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	res := Run(ctx, p.check("limit = 10", "limit = 11"), Options{StateDir: p.state})
	if res.Verdict != Harness || !strings.Contains(res.Reason, "interrupted") {
		t.Fatalf("want harness (interrupted), got %s: %s", res.Verdict, res.Reason)
	}
	p.mustBeRestored()
}

// If the file is edited while the suite runs (an editor, a formatter), the
// restore must not overwrite that edit with the snapshot.
func TestRestoreRefusesToClobberAnEdit(t *testing.T) {
	p := newProject(t, "printf 'limit = 10\\nmode = strict\\n# edited\\n' > guard.txt\necho 'not ok 1 - x'\nexit 1\n")
	res := p.run(p.check("limit = 10", "limit = 11"))
	if res.Verdict != Harness || !strings.Contains(res.Reason, "RESTORE FAILED") {
		t.Fatalf("want a refused restore, got %s: %s", res.Verdict, res.Reason)
	}
	if got := p.read("guard.txt"); !strings.Contains(got, "# edited") {
		t.Fatalf("the edit was clobbered: %q", got)
	}
	if _, err := Restore(p.state, true); err != nil { // --force puts the original back
		t.Fatal(err)
	}
	p.mustBeRestored()
}

func TestPendingJournalBlocksANewRun(t *testing.T) {
	p := newProject(t, guardingSuite)
	j := &journal{File: filepath.Join(p.dir, "guard.txt"), OrigSHA: sum([]byte(guardFile)), MutatedSHA: "x"}
	if err := writeJournal(p.state, j, []byte(guardFile)); err != nil {
		t.Fatal(err)
	}
	res := p.run(p.check("limit = 10", "limit = 11"))
	if res.Verdict != Harness || !strings.Contains(res.Reason, "breakcheck restore") {
		t.Fatalf("want harness pointing at `breakcheck restore`, got %s: %s", res.Verdict, res.Reason)
	}
}

func TestManifest(t *testing.T) {
	p := newProject(t, guardingSuite)
	p.write("breakcheck.yaml", `- name: limit is guarded
  file: guard.txt
  from: "limit = 10"
  to: "limit = 11"
  cmd: ./suite.sh
  timeout: 20s
- name: mode is not
  file: guard.txt
  from: "mode = strict"
  to: "mode = loose"
  cmd: ./suite.sh
`)
	checks, err := LoadManifest(filepath.Join(p.dir, "breakcheck.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if checks[0].Timeout != 20*time.Second || checks[1].Dir != p.dir {
		t.Fatalf("manifest fields not resolved: %+v", checks)
	}
	want := []Verdict{Guarded, NotGuarded}
	for i, c := range checks {
		if got := p.run(c).Verdict; got != want[i] {
			t.Fatalf("%s: got %s, want %s", c.Name, got, want[i])
		}
	}
	p.mustBeRestored()

	p.write("bad.yaml", "- name: x\n  file: guard.txt\n  from: a\n  to: b\n  cmd: c\n  tmeout: 5s\n")
	if _, err := LoadManifest(filepath.Join(p.dir, "bad.yaml")); err == nil {
		t.Fatal("a misspelled field must be an error, not silently ignored")
	}
}
