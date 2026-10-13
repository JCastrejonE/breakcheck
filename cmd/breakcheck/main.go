// Command breakcheck proves a test guards a line of code by breaking the line
// and requiring the suite to go red, then green again once the line is restored.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/JCastrejonE/breakcheck"
)

// version is set by the release build. A `go install` build has no ldflags,
// so it falls back to the module version Go records in the binary.
var version = "dev"

func init() {
	if bi, ok := debug.ReadBuildInfo(); ok && version == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		version = strings.TrimPrefix(bi.Main.Version, "v")
	}
}

const usage = `breakcheck: prove a test guards a line of code.

  breakcheck --file F --from OLD --to NEW --cmd "TEST COMMAND" [flags]
      Apply one mutation, run the suite, restore the exact bytes, run it again.

  breakcheck run [-f breakcheck.yaml] [--only NAME] [--json]
      Run every check in a manifest. Use it in CI so a refactor that quietly
      stops a test from guarding its code turns the build red.

  breakcheck restore [--force]
      Put back the original bytes after a run that was killed.

Exit codes: 0 guarded · 1 not guarded (the test does not catch this) · 2 harness
(no answer: no unique match, the suite never ran, a timeout, an interrupt).
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(args) > 0 {
		switch args[0] {
		case "run":
			return cmdRun(ctx, args[1:], stdout, stderr)
		case "restore":
			return cmdRestore(args[1:], stdout, stderr)
		case "version", "--version":
			fmt.Fprintln(stdout, "breakcheck", version)
			return 0
		case "help", "-h", "--help":
			fmt.Fprint(stdout, usage)
			return 0
		}
	}
	return cmdCheck(ctx, args, stdout, stderr)
}

func cmdCheck(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("breakcheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage, "\nFlags:\n"); fs.PrintDefaults() }
	var c breakcheck.Check
	fs.StringVar(&c.File, "file", "", "file to mutate")
	fs.StringVar(&c.From, "from", "", "exact text to replace; must appear exactly once")
	fs.StringVar(&c.To, "to", "", "replacement text")
	fs.StringVar(&c.Cmd, "cmd", "", "test command, run with /bin/sh -c (cmd /C on Windows)")
	fs.StringVar(&c.Dir, "dir", "", "directory to run the command in (default: current)")
	fs.StringVar(&c.Format, "format", "", "how to read failures: auto, tap, go-json, go, pytest, junit, pattern (default auto)")
	fs.StringVar(&c.Report, "report", "", "JUnit XML file the suite writes (implies --format junit)")
	fs.StringVar(&c.FailPattern, "fail-pattern", "", "regex matching one line per failing test (implies --format pattern)")
	fs.DurationVar(&c.Timeout, "timeout", breakcheck.DefaultTimeout, "kill the suite after this long")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	verbose := fs.Bool("v", false, "stream the suite's output")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if c.File == "" || c.From == "" || c.Cmd == "" {
		fs.Usage()
		return 2
	}
	c.Name = c.File
	fmt.Fprintf(stderr, "breakcheck: %s\n", c.File)
	res := breakcheck.Run(ctx, c, breakcheck.Options{Log: stderr, Verbose: *verbose})
	if *asJSON {
		writeJSON(stdout, res)
	} else {
		printResult(stdout, res)
	}
	return res.Verdict.ExitCode()
}

func cmdRun(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("breakcheck run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("f", "breakcheck.yaml", "manifest file")
	only := fs.String("only", "", "run only the check with this name")
	asJSON := fs.Bool("json", false, "print results as JSON")
	verbose := fs.Bool("v", false, "stream each suite's output")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	checks, err := breakcheck.LoadManifest(*path)
	if err != nil {
		fmt.Fprintln(stderr, "breakcheck:", err)
		return 2
	}
	var results []breakcheck.Result
	worst := 0
	for _, c := range checks {
		if *only != "" && c.Name != *only {
			continue
		}
		fmt.Fprintf(stderr, "breakcheck: %s\n", c.Name)
		res := breakcheck.Run(ctx, c, breakcheck.Options{Log: stderr, Verbose: *verbose})
		results = append(results, res)
		if !*asJSON {
			printResult(stdout, res)
		}
		worst = max(worst, res.Verdict.ExitCode())
		if ctx.Err() != nil {
			break
		}
	}
	if *only != "" && len(results) == 0 {
		fmt.Fprintf(stderr, "breakcheck: no check named %q in %s\n", *only, *path)
		return 2
	}
	if *asJSON {
		writeJSON(stdout, results)
	} else {
		fmt.Fprintln(stdout, summary(results))
	}
	return worst
}

func cmdRestore(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("breakcheck restore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	force := fs.Bool("force", false, "restore even if the file changed after the mutation")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	file, err := breakcheck.Restore(breakcheck.DefaultStateDir, *force)
	switch {
	case errors.Is(err, breakcheck.ErrNoJournal):
		fmt.Fprintln(stdout, "breakcheck: nothing to restore")
		return 0
	case err != nil:
		fmt.Fprintln(stderr, "breakcheck:", err)
		return 2
	}
	fmt.Fprintf(stdout, "breakcheck: restored %s to its original bytes\n", file)
	return 0
}

var labels = map[breakcheck.Verdict]string{
	breakcheck.Guarded:    "GUARDED    ",
	breakcheck.NotGuarded: "NOT GUARDED",
	breakcheck.Harness:    "HARNESS    ",
}

func printResult(w io.Writer, r breakcheck.Result) {
	fmt.Fprintf(w, "%s  %s · %s (%s)\n", labels[r.Verdict], r.Name, r.Reason, time.Duration(r.Seconds*float64(time.Second)).Round(100*time.Millisecond))
	if r.Verdict == breakcheck.Harness && r.Mutated != nil && r.Mutated.Tail != "" {
		fmt.Fprintln(w, indent(r.Mutated.Tail, "    | "))
	}
}

func summary(rs []breakcheck.Result) string {
	n := map[breakcheck.Verdict]int{}
	for _, r := range rs {
		n[r.Verdict]++
	}
	return fmt.Sprintf("%d guarded, %d not guarded, %d harness", n[breakcheck.Guarded], n[breakcheck.NotGuarded], n[breakcheck.Harness])
}

func indent(s, prefix string) string {
	return prefix + strings.ReplaceAll(s, "\n", "\n"+prefix)
}

func writeJSON(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
