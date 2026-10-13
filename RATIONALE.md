# breakcheck — rationale

Status: **analysis, pre-spec.** This document decides whether breakcheck should
exist as a standalone Go tool. If the case below does not hold, we pick a
different piece of Launchpad to extract.

## 1. The problem

A green test suite proves the tests pass. It does not prove they would fail if
the code were wrong. "Code coverage lies by omission: it measures execution, not
verification" ([Trail of Bits, 2026][tob]).

The only direct evidence that a test guards a line is to break the line and watch
the test fail. Developers already do this by hand. By hand, the check itself goes
wrong in ways that produce a *confident wrong answer*. Launchpad recorded all three
in one day (`docs/COMPLIANCE-REGISTRY.md`, rows 8, 33, 63):

| Failure | What it looked like | What was true |
|---|---|---|
| The edit did not apply (anchor text did not match) | "test stays green, so it's weak" — or worse, green read as meaningful | the suite tested the unmodified file |
| Restored with `git checkout` | later runs looked normal | the uncommitted fix under test was deleted |
| The break was a compile error | non-zero exit, so "red, the test works" | no test ran at all |

And the cost of *not* checking: a permission gate stayed broken for ~50 specs
behind green suites, because the tests asserted the argument passed to a boundary
rather than its effect (Launchpad `CLAUDE.md`, verification rule #1).

## 2. What breakcheck does

One mutation you name, one verdict you can trust:

```
breakcheck --file lib/capabilities.ts --from 'role === "OWNER"' --to 'true' --cmd 'pnpm test'
```

1. Byte-snapshot the file.
2. Apply the edit. Refuse unless `--from` matches **exactly once** and the hash changed.
3. Run the suite. Require that it **ran and failed** — a test reported failure,
   not merely a non-zero exit.
4. Restore from the snapshot (never from git). Re-run; require green.

Exit codes separate the two answers that must never be confused:
`0` guarded · `1` **not guarded** (your test is weak) · `2` **harness problem**
(you learned nothing).

## 3. Why someone would use it

### The landscape

| Tool | Question it answers | Mutations | Distinguishes "never ran" from "failed" |
|---|---|---|---|
| Stryker, PIT, mutmut, go-mutesting | how good is my whole suite? | automatic, thousands | per-tool, one language each |
| [universalmutator][awesome], [mewt][tob], [mutation-test][mt] (Dart) | same, any language | automatic from rules (regex / tree-sitter / XML) | by exit code only |
| [Marauder][marauder] (research prototype, 2026) | manage hand-crafted mutants | hand-written | — |
| **breakcheck** | **does *this* test guard *this* line?** | **one, hand-named** | **yes — that is the point** |

The existing tools run *campaigns*: minutes to hours, a score at the end, noise
from equivalent mutants. They are good at auditing a suite and bad at the moment
that matters most: **right after writing the test for a bug fix**, when you need a
yes/no in one suite run. The Marauder paper independently confirms hand-crafted
mutants are a real practice whose tooling is "fragmented".

### Who uses it

1. **A developer fixing a bug.** The "watch it go red" TDD habit, made impossible
   to get wrong. One command instead of edit → run → remember to restore.
2. **A reviewer.** A PR that says "guarded: `breakcheck …` exit 0" carries a
   claim the reviewer (or CI) can re-run, instead of trusting prose.
3. **AI coding agents — the strongest case.** Agents write tests that assert the
   call rather than the behaviour, and then report "verified". Trail of Bits'
   argument is exactly that agents must "demand external validation before
   crystallizing behavior into tests". Launchpad is the evidence: its agent
   hand-rolled a break the day *after* recording the lesson and got it wrong
   (registry row 129), while the script caught the same mistake when used
   (row 125). A tool with a machine-readable verdict is something an agent can be
   *required* to run, and whose output a harness can check.

### The feature that makes it a product, not a script: a checked-in manifest

Today Launchpad records breaks as prose in spec logs — four in spec 0228 alone
("Roto con `break-check.sh`: quitar el `.slice(0, 140)` …"). Nobody can re-run
prose. A later refactor can quietly make that test stop guarding, and the record
still says "guarded".

```yaml
# breakcheck.yaml
- name: commit-status description is truncated to 140 chars
  file: packages/github/src/client.ts
  from: "req.description.slice(0, 140)"
  to: "req.description"
  cmd: pnpm --filter @launchpad/github test
```

`breakcheck run` in CI re-proves every recorded guard on every change. That is
*regression testing for your tests* — and none of the tools above offers it for
hand-picked mutations.

### Honest weaknesses

- **Small evidence base.** One team, ~6 weeks, ~10 recorded uses, mostly one
  agent. The problem is universal; the demand for *this* tool is unproven.
- **The core is ~100 lines.** "That's a shell script" is a fair objection to a
  straight port. The case rests on the manifest, runner adapters and the safety
  properties below — not on the port.
- **"Did the suite run" is framework-specific.** The script only knows TAP
  `not ok`. Being language-agnostic requires adapters (go test, pytest,
  jest/vitest, cargo, JUnit XML). That is the hard, valuable part.
- **Adoption is niche.** Realistic ceiling: a sharp tool some teams and agent
  harnesses adopt, not a popular framework.

## 4. Why Go

Not "because the CV wants Go." These are properties the tool needs, that the
current bash script does not have, and that Go gives with the standard library
alone:

1. **Zero-runtime, single-binary distribution.** A language-agnostic tool must not
   make a Rust or Java project install Node or Python. The script needs bash +
   python3 + `shasum` (missing on Alpine by default; Windows unusable). Go
   cross-compiles one static binary per OS/arch.
2. **The restore must survive interruption — the script's does not.** It has no
   `trap`: Ctrl-C during the suite run leaves the file **mutated**, which is the
   very "restore lost my code" failure (row 33) it exists to prevent. Go gives
   `os/signal`, `defer`, and a crash journal so `breakcheck restore` can recover
   even after `kill -9`.
3. **Process control.** The script has no timeout, and a hung suite hangs forever.
   Test runners spawn children (watchers, browsers, DB containers). Go's
   `exec.CommandContext` + process groups give timeouts that kill the whole tree.
4. **Structured parsing** of JUnit XML, `go test -json` and TAP with
   `encoding/xml` and `encoding/json` — no dependencies.
5. **Parallel manifests.** Thirty guards cannot mutate one working tree at once;
   each needs an isolated copy. Goroutines plus a worker pool are the natural fit.
6. **Peers.** Go is a normal choice for developer CLIs (gh, goreleaser, golangci-lint).

What Go does *not* uniquely give: Rust would satisfy 1–5 equally. Go wins on
simplicity, a stdlib that covers everything above, and fast iteration — not
on capability.

## 5. Verdict

**Justified, with conditions.** A 1:1 port of `break-check.sh` is **not**
justified: it would be a nicer script. The tool earns its existence only if v1
ships:

- crash-safe restore (signals + journal + `breakcheck restore`),
- timeouts that kill the whole process tree,
- a "the suite ran" check for at least TAP, `go test -json` and JUnit XML
  (JUnit XML alone covers pytest, jest, vitest, cargo-nextest, Maven, Gradle),
- the `breakcheck.yaml` manifest with `breakcheck run`,
- `--json` output for agents and CI.

**Kill criterion:** if a reliable "suite ran and a test failed" signal cannot be
built across those three formats, the language-agnostic claim collapses and
breakcheck is only a Launchpad script. In that case we stop and extract a
different piece.

**Validation plan:** Launchpad becomes user #1. Replace `scripts/break-check.sh`,
turn spec 0228/0230's prose breaks into a manifest, run it in CI, and watch
whether it catches a guard that went stale.

[tob]: https://blog.trailofbits.com/2026/04/01/mutation-testing-for-the-agentic-era/
[mt]: https://github.com/domohuhn/mutation-test
[marauder]: https://arxiv.org/abs/2603.07065
[awesome]: https://github.com/theofidry/awesome-mutation-testing
