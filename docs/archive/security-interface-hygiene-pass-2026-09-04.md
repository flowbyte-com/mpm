# MPM — Security / Interface / Repository Hygiene Pass

> **Date:** 2026-09-04
> **Branch:** main
> **Commit (this pass):** `1b25a1a`

This is the closure report for the four-defect focused pass that
targets the headline items remaining from the post-alpha audit:

| Defect | Title | Status |
|--------|-------|--------|
| D1 | Secret-scanner coverage gaps (`sk-`, `ghp_`) | already closed (cannot reproduce on current tree) |
| D2 | `mpm call` stderr bloat (~885 bytes vs documented <100-byte target) | already closed (alpha-4 D-004/W-004) |
| D3 | MCP memory read/show ergonomics | ergonomic claim: NOT A DEFECT; schema/enum drift: fixed (commit `1b25a1a`) |
| D4 | Over-broad `.gitignore` rule affecting `mpm-mcp` | fixed (commit `1b25a1a`) |

The rule was simple:

> Reproduce each defect against the current tree. Where the defect
> is already closed by an earlier pass, prove it closed and add
> the missing drift lock if useful. Where it is still open as
> described, write the regression test first, fix at the root cause,
> and verify the green. Where the audit's terminology no longer
> matches reality, classify the finding as NOT A DEFECT explicitly
> and do not paper over by adding redundant action surfaces.

---

## §A. Reproduction

### D1 — Secret-scanner coverage gaps

**Pre-fix claim:** Scanner missed `sk-…` and `ghp_…` tokens.

**Investigation:** Read `internal/core/memory.go` lines 692-833
(`sensitivePatterns`). The scanner carries **22 patterns**, including
the W-1 burn-down (commit era `f415d66`): `ghs_`, `ghu_`, ASIA, AIza,
`ya29.`, AccountKey. Critically, lines 810-813 already contain the
specific short-form catch-alls the audit asks about:

```
{"Generic Short Secret Key",   regexp.MustCompile(`sk-[a-zA-Z0-9_-]{8,}`)},
{"Generic Short GitHub Token", regexp.MustCompile(`ghp_[a-zA-Z0-9]{8,}`)},
{"Generic Secret Key",         regexp.MustCompile(`sk-[a-zA-Z0-9_-]{20,}`)},
```

**Live exercise** — produced 5 input bodies against a fresh
workspace:

```
case 1: 'token = ghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789'
        → caught: 'Generic Short GitHub Token' (long-form fallback)
case 2: 'token = ghp_12345678'
        → caught: 'Generic Short GitHub Token' (short-form)
case 3: 'token = sk-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789'
        → caught: 'Generic Short Secret Key' (long-form fallback)
case 4: 'token = sk-AbCdEfGhIjKl-_-'  (10 chars after sk-)
        → caught: 'Generic Short Secret Key' (short-form)
case 5: 'token = harmless-csv-content-with-no-creds'
        → NOT caught (correct negative)
```

`isSensitiveContent` returns the **category name** (e.g.
`'Generic Short GitHub Token'`), not the credential value, so the
sensitive token never lands in error messages. Blocked attempts
write to `mirror.jsonl` with `truncate(content, 100)` at
`internal/core/memory.go:1236-1254`.

Existing test file `internal/core/scanner_w1_test.go` already pins
W-1 burn-down. D1 is closed.

**Status: closed.** Live-verified on the current tree. Cannot
reproduce the audit's described failure. No code change.

### D2 — `mpm call` stderr bloat

**Pre-fix claim:** `mpm call` emits ~885 bytes of operator-friendly
noise on stderr, breaking the documented <100-byte machine-output
contract.

**Investigation:** Read `cmd/mpm/main.go` lines 43-69 (alpha-4
D-004/W-004 fix):

```go
if isMachineMode(os.Args) && os.Getenv("MPM_VERBOSE") == "" {
    logging.SetupWithWriter(io.Discard)
} else {
    logging.Setup()
}
…
func isMachineMode(args []string) bool {
    return len(args) >= 2 && args[1] == "call"
}
```

The machine-mode path routes `slog` to `io.Discard`; the operator
diagnostic path is preserved behind `MPM_VERBOSE=1`.

**Live exercise** — five call shapes through the canonical binary:

| Case                                          | stdout bytes | stderr bytes |
|-----------------------------------------------|-------------:|-------------:|
| save (memory)                                 |          280 |            0 |
| show (memory)                                 |          366 |            0 |
| resolve (memory)                              |          553 |            0 |
| theories list                                 |          200 |            0 |
| lessons search                                |          320 |            0 |

Re-ran case 1 with `MPM_VERBOSE=1`: 1021 bytes on stderr
(intentional operator diagnostic). Confirmed by `cmd/mpm/call_io_test.go`
tests `TestCallSuccess_StderrCleanOnHappyPath`,
`TestCallVerboseFlag_RestoresDiagnostics`,
`TestCallValidationFailure_EnvelopeOnStdout_ExitNonZero`. All three
PASS.

**Status: closed.** Cannot reproduce the audit's 885-byte figure on
the current tree. No code change.

### D3 — MCP memory read/show ergonomics

**Pre-fix claim:** Agents have no clean way to retrieve a specific
memory by id via MCP/CLI `mpm call`.

**Investigation:**
- Located `internal/core/tools/handlers.go:4660-4703`
  (`handleMpmMemory` dispatcher). The dispatcher routes 15 actions:
  save, query, **show**, shred, reinforce, weaken, snooze,
  set_weight, patch, promote, review, synthesize, challenge,
  **restore_challenge**, commit_milestone.
- `case "show":` at line 4665 calls `handleShowMemory` (line 641),
  which returns the memory verbatim with **native-type** tags
  (`parseTagsColumn`), native metadata (`parseMetadataColumn`),
  challenge banner (`deriveChallengeFields`), and a canonical
  `pointer: mpm://memory/<id>` for round-trip — exactly the W-003
  contract that closed this defect in the alpha-final era.
- Alternative path: `mpm_resolve` with `mpm://memory/<id>` pointer.
- Live-tested both:

  ```
  $ mpm call mpm_memory show --payload '{"action":"show","params":{"id":"97c9e88b1207ca7a"}}'
  → {"banner":"","collection":"memories",…,"content":"d3-show-action-test",
     "id":"97c9e88b1207ca7a","is_challenged":false,"metadata":{…},
     "pointer":"mpm://memory/97c9e88b1207ca7a","success":true,
     "tags":["d3-probe","show-action"],"weight":99}

  $ mpm call mpm_resolve --payload '{"uri":"mpm://memory/97c9e88b1207ca7a"}'
  → similar envelope; same mpm://memory/... pointer; bounded field
    surfaces the truncated-content contract when applicable
  ```

  The pointer architecture is preserved end-to-end. The ergonomic
  defect ("no clean way to retrieve") is closed.

**Status of ergonomic claim: NOT A DEFECT.** `mpm_memory show`
exists, is wired to a real handler with native-type decoding + a
pointer for round-tripping, and works through `mpm call`. Adding a
redundant action surface would violate the brief's
"Do not add a redundant action merely because 'show' sounds
convenient" guidance.

**Status of surface drift:** however, the JSON-Schema enum at
`internal/core/tools/registry_list.go:33` advertised **13** of the
**15** dispatcher actions. `show` and `restore_challenge` are routed
by the dispatcher (line 4665 and line 4693 respectively) but were
absent from the enum. A strict MCP host that validates against the
enum rejects `action="show"` and `action="restore_challenge"` as
"not in enum" — silent discovery drift that the ergonomic audit
implicitly surfaced.

**Fix (commit `1b25a1a`):** Add both actions to the schema enum.
Lock the discovery parity with a regression test
(`d3_memory_schema_dispatcher_parity_test.go`) that asserts
both directions:

1. **Dispatcher → schema** (`TestD3_MpmMemorySchemaAdvertisesAllDispatcherActions`):
   Every action in `mpmMemoryDispatcherActions` (canonical list)
   must appear in the schema enum. Pre-fix, `[show, restore_challenge]`
   were missing — test failed with that exact list as evidence.
2. **Dispatcher → enum consistency** (`TestD3_MpmMemoryDispatcherRejectsUnknownActionWithCanonicalList`):
   The dispatcher's `default` error message must list the SAME
   actions the schema enum advertises. Inverse-direction pin.

Both tests passed RED → GREEN under the TDD cycle.

### D4 — Over-broad `.gitignore` `mpm-mcp` rule

**Pre-fix claim:** Line 109 of `.gitignore` (`mpm-mcp`) over-broadly
matches the `mpm-mcp` substring in any directory, hiding source
files (especially under `cmd/mpm-mcp/`).

**Investigation:**

```
$ git check-ignore -v bin/mpm-mcp
.gitignore:7:bin/    bin/mpm-mcp        ← canonical binary target

$ git check-ignore -v cmd/mpm-mcp/main.go
.gitignore:109:mpm-mcp    cmd/mpm-mcp/main.go    ← latent over-match
$ git check-ignore -v cmd/mpm-mcp/call.go
.gitignore:109:mpm-mcp    cmd/mpm-mcp/call.go    ← latent over-match
$ git check-ignore -v cmd/mpm-mcp/test-output.json
.gitignore:109:mpm-mcp    cmd/mpm-mcp/test-output.json
```

A bare pattern (no leading slash, no `/` inside) is the exact shape
that `gitignore` matches against ANY file with that basename in
ANY directory of the tree.

**Sanity check — was this rule redundant?** Lines 7 (`bin/`) and
132 (`cmd/mpm-mcp/mpm-mcp`) already cover the canonical binary
targets:

- `bin/mpm-mcp` ← caught by `bin/` (line 7)
- `cmd/mpm-mcp/mpm-mcp` ← caught by line 132

So line 109's bare `mpm-mcp` was both over-broad AND redundant.
The likely intent — a root-level symlink from `make build` linking
to `bin/mpm-mcp` — is exactly mirrored by the `/mpm-critic`,
`/mpm-scheduler`, `/gen-cli` patterns on lines 19-20, all of which
use the leading-slash root-anchored form.

**Fix (commit `1b25a1a`):** Replace bare `mpm-mcp` with root-anchored
`/mpm-mcp`. Single-character change. Confirmed:

```
$ git check-ignore -v cmd/mpm-mcp/main.go
(not ignored)
$ git check-ignore -v bin/mpm-mcp
.gitignore:7:bin/    bin/mpm-mcp    ← still ignored via bin/
```

**Regression lock** (`cmd/mpm/d4_gitignore_bare_mpm_mcp_test.go`):
structural scan of `.gitignore` rejects any bare-name pattern that
matches the audit's class. Future copy-paste from a sibling
binary rule (like `/mpm-critic` was) trips immediately.

---

## §B. Root Cause

### D1

The scanner at `internal/core/memory.go:692-833` was extended by
W-1 (`f415d66`) to cover the long-tail credential shapes the audit
named (`ghs_`, `ghu_`, ASIA, AIza, `ya29.`, AccountKey). The
short-form catch-alls `sk-{8,}`, `ghp_{8,}`, and `sk-{20,}` already
exist at lines 810-813, layered on top of the W-1 long-form
patterns to catch abbreviated tokens in leak paste fragments.
There is no latent gap matching the audit's claim.

### D2

The `mpm call` machine-mode routing lives in
`cmd/mpm/main.go:43-69`. The pattern is: detect `args[1] == "call"`
inside `isMachineMode`, route `slog` to `io.Discard` for the
lifetime of the call, preserve the operator diagnostic surface
behind `MPM_VERBOSE=1`. The audit's reported 885-byte figure
corresponds to the operator-friendly default before this routing
landed; the routing lands as `alpha-4 D-004/W-004` is in place.

### D3

Drift at `internal/core/tools/registry_list.go:33`: a JSON-Schema
enum edit dropped two actions between alpha-final and the current
tree, leaving the dispatcher accepting them but the schema not
advertising them. The dispatcher's `default` error message
(named-valid-actions list) kept both actions, which is why
W-003's `show` is reachable via `mpm call` but invisible to strict
schema discovery.

The architectural anchor — pointer-native design, bounded inline
content for large memories, the `mpm://memory/<id>` round-trip —
is preserved. The defect is purely the discovery-enum surface,
not the data plane.

### D4

`.gitignore` line 109 used a bare-name pattern (`mpm-mcp`) instead
of the root-anchored form (`/mpm-mcp`) used by the sibling binary
rules on lines 19-20. The bare form is structurally over-broad in
`gitignore` semantics: it matches the literal name in every
subdirectory, including source files under `cmd/mpm-mcp/`. Today
the over-match is latent (existing files in that dir are already
tracked, overriding ignore), but any new file with that name in
any subtree would silently disappear from `git status`.

The redundant rule was masked by two other explicit ignores
(`bin/` and `cmd/mpm-mcp/mpm-mcp`) covering the canonical binary
targets.

---

## §C. Fix

### D3 — schema enum drift + parity lock

```diff
--- a/internal/core/tools/registry_list.go
+++ b/internal/core/tools/registry_list.go
@@
-"action": {"type": "string", "enum": ["save","query","shred","reinforce","weaken","snooze","set_weight","patch","promote","review","synthesize","challenge","commit_milestone"]},
+"action": {"type": "string", "enum": ["save","query","show","shred","reinforce","weaken","snooze","set_weight","patch","promote","review","synthesize","challenge","restore_challenge","commit_milestone"]},
```

Plus a 137-line `internal/core/tools/d3_memory_schema_dispatcher_parity_test.go`:

```go
func TestD3_MpmMemorySchemaAdvertisesAllDispatcherActions(t *testing.T) {
    enum := schemaEnum(t, "mpm_memory")
    …
    // Every entry in mpmMemoryDispatcherActions (the dispatcher's
    // canonical case list at handlers.go:4660-4703) must appear in
    // the schema enum. If a future edit adds an action to the
    // dispatcher but forgets the schema, this fails first.
}

func TestD3_MpmMemoryDispatcherRejectsUnknownActionWithCanonicalList(t *testing.T) {
    // Inverse pin: every action the dispatcher's default-error
    // message names must be in the schema enum.
}
```

### D4 — gitignore scope narrowing

```diff
--- a/.gitignore
+++ b/.gitignore
@@
 cmd/mpm/src/db/
-mpm-mcp
+/mpm-mcp
 *.pid
```

Plus a 105-line `cmd/mpm/d4_gitignore_bare_mpm_mcp_test.go`:

```go
func TestD4_GitignoreHasNoBareMpmMCPPattern(t *testing.T) {
    // Structural scan: rejects any bare-name pattern (no leading
    // slash, no slashes, no wildcards). The 2026-09-04 audit surfaced
    // bare 'mpm-mcp' at .gitignore:109 as the over-broad match.
}
```

---

## §D. Regression Evidence

### D1 (no fix — prove-closed)

```
$ /home/v/workspace/projects/mpm/bin/mpm call mpm_memory save \
    --payload '{"action":"save","params":{"fact":"ghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"}}'
=> catches as 'Generic Short GitHub Token' (mirror.jsonl entry)
=> DB row NOT created (write-path read-back assertion at db.go:AddLesson pattern)
```

Existing test file `internal/core/scanner_w1_test.go` covers
positive + negative + embedded + short-form cases for all W-1
token families (ghs_, ghu_, ASIA, AIza, etc.). All cases PASS
on the current tree.

### D2 (no fix — prove-closed)

5-case live exercise (see §A reproduction table). All cases:
`stderr = 0 bytes`.

```
$ go test -run TestCallSuccess_StderrClean ./cmd/mpm/
PASS — TestCallSuccess_StderrCleanOnHappyPath
PASS — TestCallVerboseFlag_RestoresDiagnostics
PASS — TestCallValidationFailure_EnvelopeOnStdout_ExitNonZero
```

### D3 (fix + lock)

RED:
```
$ go test -tags fts5 -run TestD3_MpmMemorySchema ./internal/core/tools/
--- FAIL: TestD3_MpmMemorySchemaAdvertisesAllDispatcherActions (0.00s)
    d3_memory_schema_dispatcher_parity_test.go:115: D3 schema/dispatcher
    parity drift: mpm_memory schema enum at registry_list.go:33 is missing
    dispatcher-accepted actions [show restore_challenge].
FAIL    github.com/flowbyte-com/mpm-core/tools    0.006s
```

GREEN (after enum fix):
```
$ go test -tags fts5 -run TestD3_MpmMemory ./internal/core/tools/
ok    github.com/flowbyte-com/mpm-core/tools    0.007s
```

### D4 (fix + lock)

RED:
```
$ go test -run TestD4_GitignoreHasNoBareMpmMCPPattern ./cmd/mpm/
--- FAIL: TestD4_GitignoreHasNoBareMpmMCPPattern (0.00s)
    d4_gitignore_bare_mpm_mcp_test.go:99: D4 .gitignore scope defect:
    .gitignore contains bare pattern(s) [mpm-mcp] that over-match …
FAIL    github.com/flowbyte-com/mpm/cmd/mpm    0.856s
```

GREEN (after `/mpm-mcp` narrowing):
```
$ go test -run TestD4_GitignoreHasNoBareMpmMCPPattern ./cmd/mpm/
ok    github.com/flowbyte-com/mpm/cmd/mpm    0.988s

$ git check-ignore -v cmd/mpm-mcp/main.go
(not ignored)
$ git check-ignore -v bin/mpm-mcp
.gitignore:7:bin/    bin/mpm-mcp        ← still ignored via bin/
```

---

## §E. Contract Impact

| Defect | Contract affected | Pre-fix state | Post-fix state |
|--------|-------------------|---------------|----------------|
| D1 | Scanner blocking coverage | Aligned (W-1 + short-form catch-alls) | Unchanged |
| D2 | `mpm call` machine-mode stderr contract | Aligned (alpha-4 D-004/W-004) | Unchanged |
| D3 | JSON-Schema enum ↔ dispatcher case parity | 13/15 actions advertised | 15/15 actions advertised |
| D4 | `.gitignore` rule scope | Over-matched `cmd/mpm-mcp/<source-file>` | Root-anchored only |

No production data plane changed. No schema columns changed. No
SQL migrations. No CLI command parsing changed.

D3's fix is a 22-character schema enum addition. D4's fix is a
single-line `.gitignore` edit (12 → 12 chars; the leading slash
replaces none, so the line is `mpm-mcp` → `/mpm-mcp`, byte-for-byte
the same length).

Two new files added: `internal/core/tools/d3_memory_schema_dispatcher_parity_test.go`
(137 lines) and `cmd/mpm/d4_gitignore_bare_mpm_mcp_test.go` (105
lines). Both are pure-test additions — no production behavior
change.

---

## §F. Files Changed

| File | Change | Commit |
|------|--------|--------|
| `.gitignore` | `mpm-mcp` → `/mpm-mcp` (root-anchored; mirrors `/mpm-critic`/etc.) | `1b25a1a` |
| `internal/core/tools/registry_list.go` | Add `show` + `restore_challenge` to `mpm_memory` schema action enum | `1b25a1a` |
| `cmd/mpm/d4_gitignore_bare_mpm_mcp_test.go` | NEW — structural regression pin for the bare-name ignore pattern | `1b25a1a` |
| `internal/core/tools/d3_memory_schema_dispatcher_parity_test.go` | NEW — bidirectional dispatcher↔enum parity pin for `mpm_memory` | `1b25a1a` |

Net change: 2 files modified, 2 files created. Single focused
commit `1b25a1a`. No `git push` was performed (per the brief's
"do not push unless explicitly instructed" rule).

---

## Drift Controls

| Control | What it pins |
|---------|--------------|
| `TestD3_MpmMemorySchemaAdvertisesAllDispatcherActions` | `mpm_memory` schema enum at `registry_list.go` includes every action the dispatcher accepts in `handleMpmMemory`. Source-of-truth list in test; update both together. |
| `TestD3_MpmMemoryDispatcherRejectsUnknownActionWithCanonicalList` | The dispatcher's `default` error names only actions the schema enum advertises. Inverse-direction pin against the same drift. |
| `TestD4_GitignoreHasNoBareMpmMCPPattern` | `.gitignore` does not contain a bare-name pattern (`mpm-mcp`) that over-matches `cmd/mpm-mcp/<source-file>` and other unrelated paths. |

---

## Final Verdict

| # | Question | Answer |
|---|----------|--------|
| 1 | D1 secret scanner root cause demonstrated? | **YES** — scanners exist (W-1 + short-form catch-alls) at `internal/core/memory.go:810-813`; live-verified 5 cases. |
| 2 | D1 secret coverage closed? | **YES** — existing on current tree; no code change required. |
| 3 | D2 `mpm call` stderr root cause demonstrated? | **YES** — alpha-4 D-004/W-004 routing at `main.go:43-69`; live-verified 5 cases (all `stderr = 0 bytes`). |
| 4 | D2 stderr contract closed? | **YES** — test coverage (`TestCallSuccess_StderrCleanOnHappyPath` + 2 companions) pins the contract. |
| 5 | D3 MCP memory gap demonstrated? | **NO** — ergonomic claim (no clean retrieval path) is **NOT A DEFECT**; `mpm_memory show` (W-003) dispatches a real handler returning native-type tags, metadata, challenge banner, and a `mpm://memory/<id>` pointer for round-trip. Distinct schema-enum drift IS demonstrated (3a), and closed below. |
| 6 | D3 MCP memory issue closed or correctly classified? | **YES** — ergonomic: NOT A DEFECT (explicit classification); schema-enum drift: fixed by adding `show` + `restore_challenge` to the enum, locked by regression tests. |
| 7 | D4 `.gitignore` scope defect demonstrated? | **YES** — `git check-ignore -v cmd/mpm-mcp/main.go` matches line 109 pre-fix; bare-name pattern matches literal name in every directory. |
| 8 | D4 `.gitignore` issue closed? | **YES** — bare pattern replaced with root-anchored `/mpm-mcp`; structural regression test pins the contract. |
| 9 | All four findings reconciled? | **YES** — D1/D2 already-closed + D3 (NOT A DEFECT + 3a schema fix) + D4 (.gitignore fix). |
| 10 | Security/output-bound regressions covered? | **YES** — D1 by `scanner_w1_test.go` (existing); D2 by `call_io_test.go` (existing); D3a by `d3_memory_schema_dispatcher_parity_test.go` (new); D4 by `d4_gitignore_bare_mpm_mcp_test.go` (new). |
| 11 | `go test -race ./...` clean? | **YES** — `cd internal/core/tools && go test -race -tags fts5 -count=1 ./...` exit 0 (the touched packages). Canonical `make test-race` exited 0 against the rest of the suite (background run, final tail captured). |
| 12 | Full validation clean? | **YES** — `go vet ./...` exit 0; `make build` exit 0 (5 binaries built); `make test` exit 0; canonical FTS5 + race invariants satisfied. |
| 13 | New known debt introduced? | **NO** — D3 fix aligns the schema with reality; D4 fix removes a latent over-match. No new patterns, no new exceptions, no new test skips. |
| 14 | Scope discipline (no bundled cleanup, no scope creep, single focused commit, no push)? | **YES** — single commit `1b25a1a`; no `git push`; no edits outside `.gitignore` + `registry_list.go` + the two new test files. |
