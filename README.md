# breakcheck

**Prove a test guards a line of code: break the line, require the suite to go red, put the exact bytes back, require it to go green.**

A green test suite tells you the tests pass. It does not tell you they would fail if the code were wrong. The only direct evidence is to break the code and watch a test catch it. Everyone does this by hand, and by hand the check itself goes wrong in ways that produce a confident wrong answer:

- **The edit doesn't land.** The text didn't match, the suite stays green, and a guarded line looks unguarded (or the reverse).
- **The break doesn't compile.** The suite exits non-zero without running a test, and "build failed" looks like "test caught it".
- **The restore loses work.** `git checkout` puts back the *committed* version, deleting the uncommitted fix you were testing.
- **The run is interrupted.** Ctrl-C mid-suite leaves the file broken.

breakcheck does the break, the run and the restore as one operation that cannot report an answer it does not have.

```
$ breakcheck --file src/auth.ts --from 'role === "OWNER"' --to 'true' --cmd 'npm test'
breakcheck: src/auth.ts
  mutation applied: 1 match, sha256 5edc1e593707 -> 33cfae8ccfe7
  running with the mutation: npm test
  exit 1, 2 failing test(s), output read as tap
  restored: the file matches the original bytes (sha256 5edc1e593707)
  running on the restored code
GUARDED      src/auth.ts · 2 test(s) failed with the mutation applied, and the restored code is green (14.2s)
```

## Three answers, never confused

| Exit | Verdict | Meaning |
|---|---|---|
| `0` | **guarded** | The suite went red with the mutation and green on the restored code. |
| `1` | **not guarded** | The suite stayed green with the mutation. Nothing tests this line, or the test checks something other than its effect. |
| `2` | **harness** | No answer. `from` matched zero or several times; the suite exited non-zero but no test reported a failure (it never ran); it timed out; you interrupted it; or it was red on the restored code too. |

"Your test is weak" and "the check went wrong" must never look the same. That separation is the point of the tool.

## Guarantees

- **Exactly one match.** `from` must appear exactly once in the file, or nothing is touched.
- **The mutation is verified.** After writing, the file is read back and its SHA-256 must be the mutated content's.
- **A red run must be red tests.** A non-zero exit counts only if the output shows at least one failing test. A compile error, a crashed runner or a missing report is `harness`.
- **The restore is byte-exact and verified.** The original bytes are restored from a snapshot, not from git, and read back against their checksum.
- **It survives interruption.** The original bytes and a journal are written (and fsynced) *before* the file is touched. Ctrl-C restores immediately. After `kill -9`, `breakcheck restore` puts the bytes back, and any new run refuses to start until you do.
- **It will not clobber an edit.** If the file changed while the suite ran (an editor, a formatter), the restore refuses and tells you where the original is. `breakcheck restore --force` puts it back.
- **Hung suites die with their children.** The suite runs in its own process group with a timeout (default 10 minutes). On timeout or interrupt the whole group is killed, so watchers, browsers and test servers cannot outlive it. *(Windows has no process groups; there, only the shell is killed.)*
- **Paths can't move under it.** The file is resolved to an absolute path before the suite starts, and the suite runs as a child process, so a `cd` in your command cannot change what gets restored.

## Reading failures

breakcheck needs to know whether *tests* failed, not just whether the command did. It reads:

| Format | How it is detected | What counts as a failure |
|---|---|---|
| `tap` | `TAP version N`, `ok N`, `not ok N` | `not ok` lines, except `# TODO` / `# SKIP` |
| `go-json` | `go test -json` events | `fail` events that name a test (a package that failed to build is not a failing test) |
| `go` | `=== RUN`, `--- FAIL:`, `ok  pkg` | `--- FAIL:` lines |
| `pytest` | `test session starts`, the summary line | `FAILED ` lines (a collection error is not a failure) |
| `junit` | `--report path/to/junit.xml` | `<testcase>`s with a `<failure>` or `<error>`. The report is deleted before each run, so a stale one cannot answer for a run that never happened. |
| `pattern` | `--fail-pattern REGEX` | lines matching your regex, one per failing test |

`auto` (the default) tries `go-json`, `tap`, `go` and `pytest` in that order. For Jest, Vitest, Mocha, JUnit/Maven/Gradle, cargo-nextest and most others, use their JUnit reporter with `--report`.

## Keep the guards guarded: `breakcheck.yaml`

A one-off check proves a test guards a line today. A refactor next month can quietly make it stop, and the suite stays green. Record the breaks in a manifest and run them in CI:

```yaml
# breakcheck.yaml
- name: only owners can promote
  file: src/auth.ts
  from: 'role === "OWNER"'
  to: "true"
  cmd: npm test -- --reporter=junit --outputFile=junit.xml
  report: junit.xml
  timeout: 5m

- name: the release refuses an unstaged branch
  file: src/release.go
  from: "if !branch.Staged {"
  to: "if false {"
  cmd: go test -count=1 -json ./...
  format: go-json
```

```
$ breakcheck run            # every check; exit code is the worst verdict
$ breakcheck run --only "only owners can promote"
$ breakcheck run --json     # machine-readable, for CI annotations or agents
```

Relative paths in the manifest are resolved against the manifest's own directory. Unknown fields are an error, so a typo like `tmeout` cannot silently disable a timeout.

This repository's own [`breakcheck.yaml`](breakcheck.yaml) breaks seven lines of breakcheck and CI requires all seven to be caught.

## Install

```
go install github.com/OWNER/breakcheck/cmd/breakcheck@latest
```

or download a binary from the releases page (Linux, macOS, Windows; amd64 and arm64).

## Why it exists

breakcheck started as a 150-line bash script in a project built largely by AI coding agents. Its habit, "break every new guard once and watch it go red before trusting it", was the single most productive check in that project's defect record. It also failed three times in one day, and every failure was the harness: a break that silently did not apply, a restore through `git checkout` that deleted the fix under test, and a compile error read as a red test. The script was written to make those three impossible.

Then the script had its own bug. Used in a comparison of agent setups, a test command that began with `cd` changed what the script's relative path pointed at, the restore failed silently, the script deleted its snapshot, and the file stayed mutated. That is the class of failure it existed to prevent. This Go version resolves paths up front, never evaluates your command in its own shell, verifies every write, and keeps the snapshot until the restore is proven.

It is also useful for AI coding agents, which write tests that assert the call they made rather than its effect, and then report "verified". A tool with a machine-readable verdict is something an agent can be required to run and a harness can check.

Building this version found two of its own weak tests the same way: a JUnit check that passed whether or not the stale report was deleted, and a timeout test that was flaky under load. Both are fixed, and both are now in the manifest.

## How it compares

Mutation-testing tools (Stryker, PIT, mutmut, go-mutesting, universalmutator) generate thousands of mutants across a codebase and report a score. They answer "how good is my suite overall?", take minutes to hours, and need a tool per language. breakcheck answers a narrower question, "does *this* test guard *this* line?", in one suite run, for any language that can print TAP, `go test` output, pytest output or JUnit XML. Use mutation testing to audit a suite; use breakcheck at the moment you write the test for a fix, and in CI to keep that guarantee.

## Limits

- One mutation per check, exact text only (no AST or regex rewrites). Keep `from` unique and small.
- Checks run one after another; two checks never mutate the tree at once.
- On Windows, a timeout kills the shell but not its children.

## License

MIT
