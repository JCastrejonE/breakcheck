package breakcheck

import (
	"bufio"
	"bytes"
	"encoding/json"
	"encoding/xml"
	"io"
	"os"
	"regexp"
)

// detection is what breakcheck could read from one run's output: how many tests
// failed, and which format it read that from. "unknown" means it could not tell
// a failing suite from one that never ran.
type detection struct {
	format   string
	failures int
}

func formatOf(c Check) string {
	switch {
	case c.Format != "":
		return c.Format
	case c.FailPattern != "":
		return "pattern"
	case c.Report != "":
		return "junit"
	}
	return "auto"
}

var (
	tapLine      = regexp.MustCompile(`(?m)^(TAP version \d+|\s*(not )?ok \d+\b)`) // the number keeps `go test`'s "ok  pkg" lines out
	tapFail      = regexp.MustCompile(`(?m)^\s*not ok\b.*$`)
	tapDirective = regexp.MustCompile(`#\s*(TODO|SKIP)\b`)
	goTextRun    = regexp.MustCompile(`(?m)^(=== RUN|\s*--- (PASS|FAIL):|ok\s+\S+|FAIL\s+\S+)`)
	goTextFail   = regexp.MustCompile(`(?m)^\s*--- FAIL: `)
	pytestRun    = regexp.MustCompile(`(?m)^=+ (test session starts|.*\b(passed|failed|error|errors)\b.* =+)$`)
	pytestFail   = regexp.MustCompile(`(?m)^FAILED \S`)
)

func detect(format string, out []byte, report, pattern string) detection {
	switch format {
	case "tap":
		return detection{"tap", tapFailures(out)}
	case "go-json":
		return detection{"go-json", goJSONFailures(out)}
	case "go":
		return detection{"go", len(goTextFail.FindAll(out, -1))}
	case "pytest":
		return detection{"pytest", len(pytestFail.FindAll(out, -1))}
	case "junit":
		n, ok := junitFailures(report)
		if !ok {
			return detection{"unknown", 0} // no report: the suite did not get far enough to write one
		}
		return detection{"junit", n}
	case "pattern":
		return detection{"pattern", len(regexp.MustCompile(`(?m)`+pattern).FindAll(out, -1))}
	}
	// auto: the most specific format first.
	switch {
	case isGoJSON(out):
		return detect("go-json", out, report, pattern)
	case tapLine.Match(out):
		return detect("tap", out, report, pattern)
	case goTextRun.Match(out):
		return detect("go", out, report, pattern)
	case pytestRun.Match(out):
		return detect("pytest", out, report, pattern)
	}
	return detection{"unknown", 0}
}

func tapFailures(out []byte) int {
	n := 0
	for _, line := range tapFail.FindAll(out, -1) {
		if !tapDirective.Match(line) { // `not ok … # TODO` is an expected failure, not a red test
			n++
		}
	}
	return n
}

type goEvent struct {
	Action string
	Test   string
}

func isGoJSON(out []byte) bool {
	s := bufio.NewScanner(bytes.NewReader(out))
	s.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for s.Scan() {
		var e goEvent
		if json.Unmarshal(s.Bytes(), &e) == nil && e.Action != "" {
			return true
		}
	}
	return false
}

// goJSONFailures counts failed tests, not failed packages: a package that fails
// to build reports `fail` with no Test, and that is a suite that never ran.
func goJSONFailures(out []byte) int {
	n := 0
	s := bufio.NewScanner(bytes.NewReader(out))
	s.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for s.Scan() {
		var e goEvent
		if json.Unmarshal(s.Bytes(), &e) == nil && e.Action == "fail" && e.Test != "" {
			n++
		}
	}
	return n
}

// junitFailures counts <testcase> elements with a <failure> or <error> child,
// at any nesting depth. ok is false when the report is missing or unreadable.
func junitFailures(path string) (int, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	type testcase struct {
		Failures []struct{} `xml:"failure"`
		Errors   []struct{} `xml:"error"`
	}
	dec := xml.NewDecoder(f)
	n, cases := 0, 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, false
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "testcase" {
			var tc testcase
			if err := dec.DecodeElement(&tc, &se); err != nil {
				return 0, false
			}
			cases++
			if len(tc.Failures)+len(tc.Errors) > 0 {
				n++
			}
		}
	}
	return n, cases > 0
}
