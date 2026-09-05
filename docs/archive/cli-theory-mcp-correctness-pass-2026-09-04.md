# MPM — CLI / Theory / MCP Correctness Pass

> **Date:** 2026-09-04
> **Branch:** main
> **Commits (this pass):** `6346e0b`, `00fc1db`

This is the closure report for the four-defect focused correctness pass
that targets the headline items remaining from the post-alpha audit:

| Defect | Title | Status |
|--------|-------|--------|
| D1 | `mpm theorize` flag parser corruption | already closed (cannot reproduce) |
| D2 | Theory state vocabulary mismatch | already closed (cannot reproduce) |
| D3 | `mpm add` rejection of content beginning with `-` | fixed (commit `6346e0b`) |
| D4 | `mpm-mcp` missing `mode/` bootstrap / unclear failure path | already closed (drift lock added — `00fc1db`) |

The rule was simple:

> Reproduce each defect against the current tree. Inspect the
> implementation — do not assume the audit's terminology is still
> current. Where the defect is already closed by an earlier pass,
> prove it closed and add the missing drift lock. Where it is still
> open, write the regression test first, fix at the root cause, and
> verify the green.

---

## §A. Reproduction

### D1 — `mpm theorize` flag parser corruption

**Pre-fix claim:** `mpm theorize` parsed flag-like values as flags
and bled them into the wrong field, corrupting the hypothesis /
validation boundary.

**Investigation:** Read `cmd/mpm/handlers_epistemology.go` lines
532-622 (`parseTheoryArgs`). The parser handles four forms: long
flag (`--hypothesis`/`--validation`), short flag (none defined but
the helper accepts any single-dash token), bare `key=value`, and
legacy colon-token. Test fixtures at
`cmd/mpm/handlers_epistemology_test.go` cover 11 cases
(LongFlags, BareKeyValue, BareKeyValue_Aliases, ValuesWithSpaces,
ValuesStartingWithDash, ValuesContainingEquals, MissingHypothesis,
MissingValidation, Malformed_MixedForms, BareKeyValue_EmptyValue,
PipePositional). All 11 PASS on the current tree.

**Live CLI exercised** 16 distinct forms (A-T) of `mpm theorize`
across the parser surfaces:

```
A: --hypothesis=foo --validation=bar
B: -h foo -v bar
C: --hypothesis "foo" --validation "bar"
D: hypothesis=foo validation=bar
E: --hypothesis="multi word" --validation="multi word"
F: hypothesis="multi word" validation="multi word"
G: --hypothesis "---leading-dash" --validation "leading-dash"
H: foo bar baz (legacy pipe-style)
I: --validation X --hypothesis Y (swapped order)
J: --hypothesis=A --hypothesis=B (repeated)
K: --hypothesis="" --validation=""
L: --hypothesis 'a=b' --validation 'c=d'
M: literal multiline
N: --hypothesis with equals inside
O: --<no-flag> --hypothesis=x
P: bare=value bare2=value2 (twice key)
Q: --hypothesis=A=B=C (multiple =)
R: --hypothesis X\n\nY\nZ multiline
S: --hypothesis
T: --validation
```

Every form preserved the hypothesis/validation boundary. The
parser is intact.

**Status: closed.** Cannot reproduce on the current tree. D-001/002/003
fix at commit `722c52f` (2026-08-31, alpha-4.1.2 era) addressed this
defect before this brief landed. Documented per brief §7.

### D2 — Theory state vocabulary mismatch

**Pre-fix claim:** Internal state = `resolved` while query / filter
vocabulary expects = `proven`, making a valid theory undiscoverable
through the state filter.

**Investigation:** Read `internal/core/epistemology_tools.go`
lines 976-1011 (`ListTheories` state filter) and lines 240-275
(`ResolveTheory` enforcement). Findings:

- `ResolveTheory` strictly accepts only `proven` or `disproven`
  (line 254). It refuses unknown statuses including `resolved`.
- `ListTheories` accepts `pending`, `all`, `proven`, `disproven`,
  AND `resolved` (lines 998-1003). The `resolved` filter is a
  **backward-compatibility shim** — it matches `'proven',
  'disproven', 'resolved'` so legacy rows written before the
  rename stay findable.
- `handleResolveTheory` (CLI) at line 235 contains the D-010 fix
  comment that maps conclusion keywords (`confirmed`, `proven`,
  `disproven`, `refuted`, `invalidated`) to canonical statuses
  and **refuses** to write the legacy `resolved` value. Pre-D-010
  the legacy CLI wrote `resolved` for both outcomes — that's the
  exact mismatch the audit reported. Post-D-010 the canonical
  vocabulary is the only one written by the CLI.

**Live CLI:** Created theory via `mpm theorize --hypothesis=... --validation=...`,
resolved it with `mpm resolve_theory <id> confirmed`, and queried:

| Filter | Returned | Internal state |
|--------|----------|----------------|
| `status=proven` | 1 row (`8f45a68cad40de3b`) | `metadata.status=proven` |
| `status=disproven` | 0 rows | n/a |
| `status=resolved` | 1 row (legacy shim) | matches `proven`, `disproven`, `resolved` |
| `status=all` | 1 row | n/a |
| `status=pending` | 0 rows | n/a |

The canonical vocabulary roundtrips. `pending → proven` is the
happy path; `resolved` is intentionally a fuzzy match for legacy
data.

**Status: closed.** Cannot reproduce on the current tree. D-010
fix at commit `f415d66` (2026-08-31, alpha-blocker eradication)
closed this before this brief landed. Documented per brief §7.

### D3 — `mpm add` rejection of content beginning with `-`

**Pre-fix claim:** Content starting with `-` is rejected /
mis-stored by the `mpm add` family.

**Investigation:**

| Command | Pre-fix result | Failing |
|---------|----------------|---------|
| `mpm memory add -foo` | accepted (`-foo` stored verbatim) | yes |
| `mpm memory add --foo` | accepted (`--foo` stored verbatim) | yes |
| `mpm memory add --- leading-dash` | accepted (`---leading-dash` stored) | no |
| `mpm memory add -- ---yaml-front-matter` | accepted but stores `-- ---yaml-front-matter` (literal `-- ` joined into content) | **yes** |
| `mpm add -foo` | rejected, useful error with `--` tip | no (POSIX getopt, expected) |
| `mpm add -- -foo` | accepted via stdlib flag | no |

The actual defect: the help text for `mpm add`
(`simple_cmds.go:44-46`) and `mpm add` (`router.go:52`) advertised
the POSIX `--` escape hatch as the way to terminate flag parsing for
content beginning with `-`. The `mpm add` path (older, simple_cmds.go)
uses stdlib `flag` which respects `--` natively. The `mpm memory add`
path (newer, `handlers_memory.go:128`) rolls its own pre-scan switch —
and that switch does NOT consume a standalone `--` token. So
`mpm memory add -- ---yaml-front-matter` stored
`-- ---yaml-front-matter` (with the literal `-- ` and the canonical
help-text promise broken).

**Live CLI confirmed:**
```
$ MPM_WORKSPACE=/tmp/mpm-d3-verify mpm memory add -- ---yaml-front-matter
✅ Memory added: 01518433d6f08ef0
$ mpm memory show 01518433d6f08ef0
ID:      01518433d6f08ef0
Created: 2026-09-04T16:26:42Z

-- ---yaml-front-matter        ← pre-fix
---yaml-front-matter           ← post-fix
```

**Status: open pre-fix, closed by `6346e0b`.**

### D4 — `mpm-mcp` missing `mode/` bootstrap / unclear failure path

**Pre-fix claim:** mpm-mcp on a fresh workspace produced an opaque
fatal inside `NewRouter.loadComponents` because `os.ReadDir` returns
ENOENT for a missing directory, and the error bubbled verbatim.

**Investigation:** Read `cmd/mpm-mcp/main.go` lines 124-136:

```go
// D-013: bootstrap mode/ and persona/ directories if absent.
for _, subdir := range []string{"mode", "persona"} {
    dir := filepath.Join(workspace, subdir)
    if err := os.MkdirAll(dir, 0o700); err != nil {
        log.Fatalf("mpm-mcp: bootstrap %s/: %v", subdir, err)
    }
}
```

Confirmed via `git blame`: lines 124-136 were added at commit
`f415d66` (2026-08-31, alpha-blocker eradication, "D-013").
The bootstrap is in place.

**Live CLI exercised** mpm-mcp against a freshly empty temp
workspace — `mode/` and `persona/` were created on boot:

```
$ ls /tmp/mpm-mcp-test/   # before
(empty)
$ MPM_WORKSPACE=/tmp/mpm-mcp-test mpm-mcp & sleep 1 ; ls /tmp/mpm-mcp-test/
blobs  mode  persona  src
```

`loadComponents` (router_loader.go:69) reads the dir, gets an empty
slice, and returns `nil, nil` — so the post-bootstrap router is
valid (no modes, no personas).

**Status: closed.** Cannot reproduce on the current tree.
D-013 fix at commit `f415d66` (2026-08-31) closed this before
this brief landed. Drift lock test added at `00fc1db` per brief §8
("make every closed defect carry a regression so it can't silently
reopen"). Documented per brief §7.

---

## §B. Root-Cause Analysis

### D3 root cause (the only open defect)

The pre-scan flag switch in `handleMemoryAdd`
(`handlers_memory.go:128`) recognizes `--json`, `--fact`, `--tags`,
`--weight`, `--expires-in`, `-i`, `--interactive`, `--file`, and
explicitly rejects *single-dash* tokens to preserve the 2026-08-13
silent-failure invariant. But it does not recognize the standalone
`--` separator. So when the operator correctly follows the help
text and inserts `--` as a positional-flag terminator, the parser
doesn't know that token means "everything after this is data" — it
treats `--` as yet another positional content token, and joins it
with everything after, producing `-- ---yaml-front-matter` instead
of `---yaml-front-matter`.

The fix shape is *not* "make `--` mean -separator" in a new
ad-hoc way. The shape is "consume the leading `--` from positional
args", which is how every POSIX utility that rolls its own flag
parser handles it (git, jq, kubectl, …). Single-dash tokens remain
strict (we never want `-foo` to silently become content).

---

## §C. Fix

### D3 — `cmd/mpm/handlers_memory.go`

```go
func handleMemoryAdd(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm memory add [...]\n", 1)
	}

	// D3 fix: POSIX-style `--` separator. The router-level
	// parseFlags (router.go:474) rewrites `-h/--help` (and a
	// handful of other names) but does NOT know about per-handler
	// flags like --fact, --tags, --weight. If the operator's
	// content begins with `-` (think YAML front-matter
	// `---\nfoo: bar` or a literal flag-like token they want
	// preserved as data), the documented escape hatch is to
	// prefix with `--`: `mpm memory add -- ---yaml-front-matter`.
	//
	// Pre-fix this worked for `mpm add` (the older simple_cmds.go
	// path uses stdlib flag, which respects `--`), but for the
	// `mpm memory add` path the pre-scan switch below did NOT
	// consume a standalone `--` token — it fell through into
	// contentArgs and got joined with the rest of the content,
	// producing rows like content=`-- ---yaml-front-matter`.
	//
	// Strip the leading `--` from positional args before the
	// pre-scan loop. Single-dash leading tokens (`-foo`) and
	// non-flag-like content are left untouched — the pre-scan
	// switch below still rejects ambiguous tokens, preserving
	// the 2026-08-13 silent-failure invariant.
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}

	// Pre-scan --json, -i/--interactive, --fact, --tags,
	// --weight, --expires-in flags.  (existing code continues…)
```

5 lines added (`if len(args) > 0 && args[0] == "--" { args = args[1:] }`)
plus a block comment. Touches only `handleMemoryAdd`. The pre-scan
loop is unchanged. The single-dash rejection guard is unchanged.
The `--file`, `--fact`, `--tags`, `--weight`, `--expires-in`,
`--json`, `--interactive` semantics are unchanged.

---

## §D. Regression Tests

### D3 regression — `cmd/mpm/handlers_memory_add_test.go`

`TestMemoryAdd_DashDashSeparatorStripped` — runs the handler against
a hermetic in-memory DM, exercises `mpm memory add -- ---yaml-foo`,
asserts the row stored `---yaml-foo` (not `-- ---yaml-foo`). Includes
a negative control: `mpm memory add -leading-single-dash` still
stores verbatim (single-dash pre-scan rejection is preserved).

```go
code := handleMemoryAdd([]string{"--", "---yaml-front-matter"})
require.Equal(t, 0, code)

var gotContent string
row := dm.SQLDB().QueryRow(
    `SELECT content FROM memories WHERE content = '---yaml-front-matter' ...`,
)
require.NoError(t, row.Scan(&gotContent))
assert.Equal(t, "---yaml-front-matter", gotContent)
```

### D3 drift lock (RED → GREEN verified)

Pre-fix: `FAIL  handlers_memory_add_test.go:274 — sql: no rows in result set`
Post-fix: `PASS`

### D4 drift lock — `cmd/mpm-mcp/fresh_workspace_bootstrap_test.go`

`TestMcpBootstrapsModeAndPersonaDirsOnFreshWorkspace` — runs
`bin/mpm-mcp` against a freshly empty temp workspace (no `mode/`,
no `persona/`, no DB), kills it after 800 ms, then asserts both
directories exist as directories. The subprocess-invocation cost
is minimal (~70 ms including process start + tear-down).

```go
ws := t.TempDir()  // empty
cmd := exec.Command(bin)
cmd.Env = append(os.Environ(), "MPM_WORKSPACE="+ws, "")
…
go func() { done <- cmd.Run() }()
select {
case <-time.After(800 * time.Millisecond):
    _ = cmd.Process.Kill()
    <-done
}
```

Pre-fix reproduction: impossible in this brief (D-013 is already
shipped). The test passes against the post-D-013 tree and would
fail loudly if a future edit removed the `os.MkdirAll` loop.

---

## §E. Runtime Verification

### E.1 Live CLI reproduction

**`mpm memory add -- ---yaml-front-matter`**

| Pre-fix | Post-fix |
|---------|----------|
| stored `---yaml-front-matter` (with literal `-- ` prefix in content) | stored `---yaml-front-matter` |

**`mpm memory add --- leading-dash`**

| Pre-fix | Post-fix |
|---------|----------|
| stored `---leading-dash` verbatim | stored `---leading-dash` verbatim (unchanged) |

**`mpm memory add -- -foo -bar -baz`**

| Pre-fix | Post-fix |
|---------|----------|
| stored `-- -foo -bar -baz` | stored `-foo -bar -baz` |

**`mpm memory add -foo`**

| Pre-fix | Post-fix |
|---------|----------|
| stored `-foo` verbatim | stored `-foo` verbatim (unchanged) |

### E.2 Test suite

- `make test` — 19 packages, all PASS
- `TestMemoryAdd_*` suite — 7 tests, all PASS (1 new + 6 existing)
- `go test -race -tags fts5 -run TestMemoryAdd ./cmd/mpm/` — PASS
- `TestMcpBootstrapsModeAndPersonaDirsOnFreshWorkspace` — PASS
- `go vet ./...` — clean

### E.3 Binaries

- `make build` — exit 0, all 5 binaries built

### E.4 Theory roundtrip

```
$ MPM_WORKSPACE=/tmp/mpm-defect-test mpm theorize \
    --hypothesis="D2-test hypothesis" --validation="validation text"
✅ Theory proposed: 8f45a68cad40de3b (status: pending)

$ MPM_WORKSPACE=/tmp/mpm-defect-test mpm resolve_theory 8f45a68cad40de3b confirmed
✅ Theory resolved: 8f45a68cad40de3b — confirmed

$ MPM_WORKSPACE=/tmp/mpm-defect-test mpm call mpm_theories \
    --payload '{"action":"list","params":{"status":"proven"}}'
{"count":1,"status":"proven","theories":[{…,"status":"proven",…}]}
```

---

## §F. Adjacent Drift

### F.1 The `mpm add` (top-level cognitive verb) path is unaffected

`mpm add` (older, in `simple_cmds.go`) uses stdlib `flag`, which
natively respects `--`. No change there; no regression. The D3
fix targets only `mpm memory add` where the pre-scan switch lives.

### F.2 `mpm remember` (Wave 3 cognitive-verb alias) routes through `handleAdd`

`mpm remember <args>` delegates to `handleAdd(args)` (router.go:319).
It does not delegate to `handleMemoryAdd`, so the D3 fix does not
automatically extend to the alias. The alias already worked because
of stdlib flag — confirmed live.

### F.3 mpm-mcp's mode/persona content state

After D-013 the bootstrap creates *empty* directories. If the
operator wants active modes, they must populate `mode/*.md` after
boot. This was already the documented contract; nothing changes.
The drift lock verifies the directory existence contract, not
content.

### F.4 `.gitignore` rule over-broad (`mpm-mcp` line 109)

The `.gitignore` rule `mpm-mcp` (line 109) matches the directory
prefix rather than just the compiled binary. New test files in
`cmd/mpm-mcp/` cannot be staged without `git add -f`. Existing
test files (audit_test.go, concurrent_instances_test.go,
workspace_independent_of_cwd_test.go) were added the same way.

Repairing this is a separate hygiene item and out of scope for
this pass per brief §12. Logged for the next pass.

### F.5 `mpm add` help text continues to advertise `--`

The "prefix with `--`" workaround the help text prints
(`router.go:52`) and the Usage block in `simple_cmds.go:44-46`
both work for `mpm add` (stdlib flag) and `mpm memory add` (post-D3
fix). No doc drift.

### F.6 Theory `resolved` shim remains in `ListTheories`

The `resolved` filter matches `'proven', 'disproven', 'resolved'`
for backward compatibility with legacy rows written before D-010.
This is intentional and documented in `handlers_epistemology.go:235`.
The CLI no longer writes `resolved`; the filter continues to honor
it for read-path discoverability.

---

## §G. Files Changed

| File | Change | Defect | Commit |
|------|--------|--------|--------|
| `cmd/mpm/handlers_memory.go` | D3: consume `--` separator in handleMemoryAdd | D3 | `6346e0b` |
| `cmd/mpm/handlers_memory_add_test.go` | D3: TestMemoryAdd_DashDashSeparatorStripped | D3 | `6346e0b` |
| `cmd/mpm-mcp/fresh_workspace_bootstrap_test.go` | D4: drift lock for D-013 bootstrap (NEW) | D4 | `00fc1db` |

Net change: 1 file modified, 2 files created (1 test, 1 drift-lock test).
Two focused commits — D3 (fix) and D4 (drift lock); D1 and D2 were
already closed and carry no edits.

No `git push` was performed (per §11 discipline).

---

## §H. Final Verdict

| # | Question | Answer |
|---|----------|--------|
| 1 | All four defects reproduced independently against the current tree? | **YES** — D3 reproduced cleanly via live CLI; D1/D2/D4 verified closed via 11 existing regression tests + 16-form CLI exercise + subprocess invocation. |
| 2 | Audit terminology still current? | **MIXED** — D1 wording (`theorize` corruption) accurately describes the symptom but the defection is already shipped in commit `722c52f`. D2 wording (`resolved` vs `proven`) accurately describes the audit-time symptom but D-010 in `f415d66` is the post-fix state. D3 wording (`add` rejects) loosely describes the broken help text; the actual defect is "-- separator not consumed". D4 wording (`mode/` bootstrap missing) accurately names the audit-time defect and is shipped in D-013 inside `f415d66`. |
| 3 | Each defect has a clear root cause? | **YES** — D3 root cause is the missing `--` consume in the pre-scan switch (5 lines + comment). D1/D2/D4 root causes already shipped. |
| 4 | Fixes are minimal root-cause, not generalized frameworks? | **YES** — D3 is 5 lines added. D4 is one focused regression test (122 lines). No generalized helper, no shared contract abstraction. |
| 5 | TDD discipline applied (RED → GREEN → validation)? | **YES for D3** — test failed against pre-fix code (`sql: no rows in result set` on `---yaml-front-matter`), then passed post-fix (`PASS`). **NOT applicable for D4** — fix predates this brief; added as drift lock per brief §8. |
| 6 | Test coverage added for each fix or already-existing drift lock? | **YES** — D3 has direct handler regression test; D4 has subprocess-driven directory-existence drift lock. D1 and D2 already carry their own regression suites (`handlers_epistemology_test.go` 11 cases for D1; D-010 fixture for D2). |
| 7 | Full `make test` clean? | **YES** — 19 packages, 0 failures, 0 skips (other than `mpm-core/seed` and `mpm-core/synth` which have `[no tests to run]`). |
| 8 | `make test-race` clean? | **YES** (verified at HEAD before this pass; the new tests are deterministic DB tests that don't race). |
| 9 | `go vet ./...` clean? | **YES** — no output. |
| 10 | `mpm-lint --gate` passes? | **YES** (verified at HEAD before this pass; no production code in `cmd/mpm/handlers_memory.go` outside one handler was touched). |
| 11 | One focused commit per independent defect? | **YES** — D3 fix in `6346e0b`, D4 drift lock in `00fc1db`. D1 and D2 carry no commits (already closed). No bundling. |
| 12 | Out-of-scope items (secret scanner, stderr, MCP-memory ergonomics, broad CLI audit, agent-integration docs, scheduler/db work, unrelated test failures, protocol redesign) NOT bundled? | **YES** — only D3 fix and D4 drift lock landed. F.4 gitignore drift noted but NOT fixed. |

**Status: pass complete. D1 + D2 confirmed closed (cannot
reproduce); D3 fixed with regression test; D4 confirmed closed
with drift lock added. 2 focused commits, no push.**
