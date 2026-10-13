package breakcheck

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// runSuite runs the check's command once, in its own process group, under the
// check's timeout. A timeout or a cancelled context kills the whole group, so a
// test runner's children (watchers, browsers, servers) cannot outlive it.
func runSuite(ctx context.Context, c Check, dir string, o Options) SuiteRun {
	cctx, cancel := context.WithTimeout(ctx, timeoutOf(c))
	defer cancel()

	report := c.Report
	if report != "" {
		if !filepath.IsAbs(report) {
			report = filepath.Join(dir, report)
		}
		// A report left over from an earlier run would answer for a run that never happened.
		_ = os.Remove(report)
	}

	var out bytes.Buffer
	var w io.Writer = &out
	if o.Verbose && o.Log != nil {
		w = io.MultiWriter(&out, o.Log)
	}
	cmd := shellCommand(cctx, c.Cmd, dir)
	cmd.Stdout, cmd.Stderr = w, w
	err := cmd.Run()
	killGroup(cmd)

	run := SuiteRun{ExitCode: exitCode(err), TimedOut: errors.Is(cctx.Err(), context.DeadlineExceeded)}
	d := detect(formatOf(c), out.Bytes(), report, c.FailPattern)
	run.Failures, run.Format = d.failures, d.format
	run.Tail = lastLines(out.String(), 30)
	return run
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode() // -1 when killed by a signal
	}
	return -1 // the command could not be started at all
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
