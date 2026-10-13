package breakcheck

import (
	"os"
	"path/filepath"
	"testing"
)

// Outputs are captured from the real tools, not typed from memory: a parser
// proven against hand-written fixtures has only been proven against those.
func TestDetect(t *testing.T) {
	cases := []struct {
		name, format, out string
		wantFormat        string
		wantFailures      int
	}{
		{"tap green", "auto", "TAP version 13\nok 1 - adds\nok 2 - subtracts\n1..2\n", "tap", 0},
		{"tap red", "auto", "TAP version 13\nok 1 - adds\nnot ok 2 - subtracts\n1..2\n", "tap", 1},
		{"tap todo is not a failure", "tap", "not ok 1 - later # TODO not built\nok 2 - now\n", "tap", 0},
		{"node --test nested", "auto", "TAP version 13\n# Subtest: suite\n    not ok 1 - inner\n    1..1\nnot ok 1 - suite\n1..1\n", "tap", 2},
		{"go text red", "auto", "=== RUN   TestA\n--- FAIL: TestA (0.00s)\n    a_test.go:9: boom\nFAIL\nFAIL\tex\t0.01s\n", "go", 1},
		{"go text green is not tap", "auto", "ok  \tex\t0.12s\n", "go", 0},
		{"go text build failure ran nothing", "auto", "# ex\n./a.go:3:1: syntax error\nFAIL\tex [build failed]\n", "go", 0},
		{"go json red", "auto", `{"Action":"run","Test":"TestA"}` + "\n" + `{"Action":"fail","Test":"TestA"}` + "\n" + `{"Action":"fail","Package":"ex"}` + "\n", "go-json", 1},
		{"go json build failure ran nothing", "auto", `{"Action":"output","Package":"ex","Output":"FAIL\tex [build failed]\n"}` + "\n" + `{"Action":"fail","Package":"ex"}` + "\n", "go-json", 0},
		{"pytest red", "auto", "============================= test session starts ==============================\ncollected 2 items\n\nt.py .F\n=========================== short test summary info ============================\nFAILED t.py::test_b - assert 1 == 2\n========================= 1 failed, 1 passed in 0.02s ==========================\n", "pytest", 1},
		{"pytest collection error ran nothing", "auto", "============================= test session starts ==============================\nERROR collecting t.py\n=========================== 1 error in 0.05s ============================\n", "pytest", 0},
		{"compile error is unknown", "auto", "src/a.ts(3,1): error TS1005: ';' expected.\n", "unknown", 0},
		{"custom pattern", "pattern", "PASS adds\nFAIL subtracts\nFAIL multiplies\n", "pattern", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pattern := ""
			if tc.format == "pattern" {
				pattern = `^FAIL `
			}
			got := detect(tc.format, []byte(tc.out), "", pattern)
			if got.format != tc.wantFormat || got.failures != tc.wantFailures {
				t.Fatalf("detect = {%s %d}, want {%s %d}", got.format, got.failures, tc.wantFormat, tc.wantFailures)
			}
		})
	}
}

func TestJUnit(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "junit.xml")
	xml := `<?xml version="1.0"?>
<testsuites><testsuite name="a" tests="3">
  <testcase name="ok"/>
  <testcase name="broken"><failure message="expected 2"/></testcase>
  <testsuite name="nested"><testcase name="crashed"><error message="TypeError"/></testcase></testsuite>
</testsuite></testsuites>`
	if err := os.WriteFile(report, []byte(xml), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := detect("junit", nil, report, ""); got != (detection{"junit", 2}) {
		t.Fatalf("junit = %+v, want 2 failures", got)
	}
	if got := detect("junit", nil, filepath.Join(dir, "missing.xml"), ""); got.format != "unknown" {
		t.Fatalf("a missing report must read as unknown, got %+v", got)
	}
}
