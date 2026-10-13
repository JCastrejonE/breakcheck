// Package breakcheck proves that a test guards a line of code. It applies one
// named mutation, requires the test suite to run and fail, restores the exact
// original bytes, and requires the suite to pass again.
//
// The three outcomes are kept apart on purpose, because "your test is weak" and
// "the check itself went wrong" must never look the same:
//
//	Guarded     the suite went red with the mutation and green without it
//	NotGuarded  the suite stayed green with the mutation applied
//	Harness     no answer: no unique match, the suite did not run, a timeout,
//	            an interrupt, or a red suite on the restored code
package breakcheck

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// Verdict is the answer to one check.
type Verdict string

const (
	Guarded    Verdict = "guarded"
	NotGuarded Verdict = "not-guarded"
	Harness    Verdict = "harness"
)

// ExitCode maps a verdict to a process exit code: 0 guarded, 1 not guarded, 2 harness.
func (v Verdict) ExitCode() int {
	switch v {
	case Guarded:
		return 0
	case NotGuarded:
		return 1
	default:
		return 2
	}
}

// Check is one named mutation and the command that should notice it.
type Check struct {
	Name        string        `yaml:"name" json:"name,omitempty"`
	File        string        `yaml:"file" json:"file"`
	From        string        `yaml:"from" json:"from"`
	To          string        `yaml:"to" json:"to"`
	Cmd         string        `yaml:"cmd" json:"cmd"`
	Dir         string        `yaml:"dir" json:"dir,omitempty"`
	Format      string        `yaml:"format" json:"format,omitempty"`
	Report      string        `yaml:"report" json:"report,omitempty"`
	FailPattern string        `yaml:"fail_pattern" json:"fail_pattern,omitempty"`
	Timeout     time.Duration `yaml:"timeout" json:"-"`
}

// SuiteRun is what one execution of the test command produced.
type SuiteRun struct {
	ExitCode int    `json:"exit_code"`
	Failures int    `json:"failures"`
	Format   string `json:"format"`
	TimedOut bool   `json:"timed_out,omitempty"`
	Tail     string `json:"tail,omitempty"`
}

// Result is the outcome of one check.
type Result struct {
	Check
	Verdict  Verdict   `json:"verdict"`
	Reason   string    `json:"reason"`
	Mutated  *SuiteRun `json:"mutated,omitempty"`
	Restored *SuiteRun `json:"restored,omitempty"`
	Seconds  float64   `json:"seconds"`
}

// Options say where the crash journal lives and where progress goes.
type Options struct {
	StateDir string    // journal and byte snapshot; default DefaultStateDir
	Log      io.Writer // progress lines; nil discards them
	Verbose  bool      // also stream the suite's own output to Log
}

const (
	DefaultStateDir = ".breakcheck"
	DefaultTimeout  = 10 * time.Minute
)

// Run performs one check. The file is restored on every path out of Run,
// including a cancelled context (an interrupt) and a panic.
func Run(ctx context.Context, c Check, o Options) (res Result) {
	start := time.Now()
	res.Check = c
	defer func() { res.Seconds = time.Since(start).Seconds() }()
	logf := func(format string, a ...any) {
		if o.Log != nil {
			fmt.Fprintf(o.Log, "  "+format+"\n", a...)
		}
	}
	harness := func(format string, a ...any) Result {
		res.Verdict, res.Reason = Harness, fmt.Sprintf(format, a...)
		return res
	}

	if err := validate(c); err != nil {
		return harness("%v", err)
	}
	state, err := filepath.Abs(stateDir(o.StateDir))
	if err != nil {
		return harness("state dir: %v", err)
	}
	if j, _, _ := loadJournal(state); j != nil {
		return harness("an earlier run was interrupted and %s may still hold its mutation; run `breakcheck restore` first", j.File)
	}
	// Resolve paths now, against the directory breakcheck was started in. Nothing
	// the suite does can move them; the bash predecessor lost a restore when a
	// `cd` in its command changed what a relative path pointed at.
	file, err := filepath.Abs(c.File)
	if err != nil {
		return harness("file: %v", err)
	}
	dir, err := filepath.Abs(orDefault(c.Dir, "."))
	if err != nil {
		return harness("dir: %v", err)
	}
	info, err := os.Stat(file)
	if err != nil {
		return harness("%v", err)
	}
	orig, err := os.ReadFile(file)
	if err != nil {
		return harness("%v", err)
	}
	switch n := bytes.Count(orig, []byte(c.From)); {
	case n == 0:
		return harness("`from` does not appear in %s", c.File)
	case n > 1:
		return harness("`from` appears %d times in %s; make it unique so the break tests what you named", n, c.File)
	}
	mutated := bytes.Replace(orig, []byte(c.From), []byte(c.To), 1)

	j := &journal{File: file, OrigSHA: sum(orig), MutatedSHA: sum(mutated), Mode: uint32(info.Mode().Perm()), PID: os.Getpid(), Started: time.Now().UTC()}
	if err := writeJournal(state, j, orig); err != nil {
		return harness("cannot write the crash journal, so the file was not touched: %v", err)
	}
	restored := false
	defer func() {
		if restored {
			return
		}
		if _, err := Restore(state, false); err != nil {
			res.Verdict, res.Reason = Harness, fmt.Sprintf("RESTORE FAILED: %v", err)
		}
	}()
	if err := writeAtomic(file, mutated, info.Mode().Perm()); err != nil {
		return harness("the mutation could not be written: %v", err)
	}
	if got, err := os.ReadFile(file); err != nil || sum(got) != j.MutatedSHA {
		return harness("the mutation did not land: the file on disk is not the mutated bytes")
	}
	logf("mutation applied: 1 match, sha256 %s -> %s", j.OrigSHA[:12], j.MutatedSHA[:12])

	logf("running with the mutation: %s", c.Cmd)
	m := runSuite(ctx, c, dir, o)
	res.Mutated = &m
	logf("exit %d, %d failing test(s), output read as %s", m.ExitCode, m.Failures, m.Format)

	restored = true
	if _, err := Restore(state, false); err != nil {
		return harness("RESTORE FAILED: %v", err)
	}
	logf("restored: the file matches the original bytes (sha256 %s)", j.OrigSHA[:12])

	switch {
	case ctx.Err() != nil:
		return harness("interrupted; the file was restored")
	case m.TimedOut:
		return harness("the suite timed out after %s with the mutation applied; that is not evidence either way", timeoutOf(c))
	case m.ExitCode == 0:
		res.Verdict = NotGuarded
		res.Reason = "the suite stayed green with the mutation applied: nothing tests this line, or the test checks something other than its effect"
		if m.Failures > 0 {
			res.Reason += fmt.Sprintf(" (it reported %d failure(s) but still exited 0)", m.Failures)
		}
		return res
	case m.Failures == 0:
		return harness("the suite exited %d but no test reported a failure, so it probably never ran: a compile error is not a red test%s", m.ExitCode, unreadHint(m))
	}

	logf("running on the restored code")
	r := runSuite(ctx, c, dir, o)
	res.Restored = &r
	switch {
	case ctx.Err() != nil:
		return harness("interrupted during the restored run; the file was already restored")
	case r.TimedOut || r.ExitCode != 0 || r.Failures > 0:
		return harness("the suite is red on the restored code (exit %d, %d failing): it was not green to begin with, or something the mutated run generated outlived the restore", r.ExitCode, r.Failures)
	}
	res.Verdict = Guarded
	res.Reason = fmt.Sprintf("%d test(s) failed with the mutation applied, and the restored code is green", m.Failures)
	return res
}

func validate(c Check) error {
	switch {
	case c.File == "" || c.From == "" || c.Cmd == "":
		return fmt.Errorf("file, from and cmd are required")
	case c.From == c.To:
		return fmt.Errorf("from and to are identical, so nothing would change")
	}
	switch formatOf(c) {
	case "auto", "tap", "go-json", "go", "pytest":
	case "junit":
		if c.Report == "" {
			return fmt.Errorf("format junit needs `report`, the path of the XML file the suite writes")
		}
	case "pattern":
		if _, err := regexp.Compile(c.FailPattern); err != nil || c.FailPattern == "" {
			return fmt.Errorf("format pattern needs a valid `fail_pattern` regex: %v", err)
		}
	default:
		return fmt.Errorf("unknown format %q (auto, tap, go-json, go, pytest, junit, pattern)", c.Format)
	}
	return nil
}

func unreadHint(m SuiteRun) string {
	if m.Format == "unknown" {
		return "; breakcheck could not read this output as any test format either, so set `format`, `report` or `fail_pattern`"
	}
	return ""
}

func stateDir(s string) string { return orDefault(s, DefaultStateDir) }
func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func timeoutOf(c Check) time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return DefaultTimeout
}
