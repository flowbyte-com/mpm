# MPM Post-Closure Residual Sweep — 2026-09-04

> **Scope:** fresh evidence-driven defect sweep taken AFTER the closure
> pass that fixed D-1/D-2/D-3 from
> `docs/residual-defect-inventory-2026-09-04.md`. Read-only —
> **no fixes started**. The brief explicitly instructed:
> *"Do not fix anything during the inventory phase"* and *"if only one,
> two, or three remain, report exactly that number. If none remain, say
> so. Do not invent filler."* One concrete reproducible defect surfaced.
> That count is honored here.

---

## §A — Current baseline

Validated at the start of this sweep session. All commands invoked from
the canonical workspace.

| Gate | Command | Result |
|------|---------|--------|
| Working tree | `git status` | clean except 2 untracked (out-of-scope): `cmd/mpm-mcp/spill_test.go`, `docs/residual-defect-inventory-2026-09-04.md` |
| HEAD | `git log --oneline -2` | `16a561e docs(agent): correct wake-payload terminology in canonical snippets` ← **D-3 closure commit; this sweep is its first downstream look** |
| Test | `make test` | exit 0 |
| Test (race) | `make test-race` | exit 0 (canonical race gate) |
| Vet | `go vet ./...` | exit 0 |
| Build | `make build` | exit 0 (5 binaries produced; buildVersion `v0.1.0-alpha-final-3-146-g16a561e`) |
| Lint gate | `./bin/mpm-lint --gate` | PASS (all 4 gates — sql, fd, mutex, imports — at threshold) |
| Render parity | `python3 agent_installation/scripts/render_managed_blocks.py --check` | **FAIL — 2 drift(s) detected** (see §C-R1) |
| Installer round-trip | `make refresh-installed` | **FAIL — `make: *** [Makefile:227: refresh-installed] Error 6`** (downstream of the render drift) |
| Render parity post-refresh | re-run after refresh | still FAIL (refresh reduces 6 → 2 drifts; the remaining 2 are introduced by D-3 itself and cannot be repaired by the refresh path) |

The render parity + installer round-trip are **new gates** that prior
passes did not run as part of the canonical CI flow. They were
specifically called out in this brief (§1) — and they immediately
surfaced a regression introduced by the very commit that closed D-3.

---

## §B — Previously closed findings

The brief explicitly listed the prior closures that should not be
reopened without current evidence. Each was re-checked against current
code/tests rather than only the report prose:

| Closure | Re-check approach | Status |
|---------|-------------------|--------|
| Scheduler CPU wedge | `TestScheduler_DoesNotWedgeOnPastCronKindWake` + `TestSweepPreservesExistingMetadata` pass under `make test-race` | Closed |
| Directive `created_at` | `TestApplyDirectives_FreshSeedPopulatesCreatedAt` + `TestMigrateMemoriesCreatedAtBackfill_RepairsNullRows` pass | Closed |
| Canonical managed snippets (post-pass-2) | The §1 protocol lock + the new Snippets lock both pass; `mpm-agent-protocol.md` is clean | **Partially regressed — see §C-R1** |
| Theory/topic registry action parity | The new bidirectional parity tests pass; both enums match dispatcher case lists | Closed |
| `.gitignore` scope | `cmd/mpm/d4_gitignore_bare_mpm_mcp_test.go` passes; no bare `mpm_mcp` pattern | Closed |
| Memory ergonomics / `mpm call` stderr / MCP mode bootstrap / F-12-1 weight type guard / secret scanner | All gated by existing parity + post-pass-2 lock tests | Closed |

The single re-open candidate is **canonical managed snippets**, and
that re-open is justified by current evidence (§C-R1 below). All other
listed closures remain closed.

---

## §C — Current residual backlog

### C-R1 (P1) — `make refresh-installed` fails: 2 copy/paste drifts introduced by D-3

**Class:** `runtime ↔ installer` + `documentation ↔ implementation`
contract drift. Introduced by commit `16a561e` (the D-3 closure
commit) when it rewrote the wake-payload wording in the canonical
snippet file.

**Evidence (verbatim from `python3 agent_installation/scripts/render_managed_blocks.py --check`):**

```
--- COPY/PASTE DRIFT: claude-code-mpm ---
--- copy/paste example:claude-code-mpm
+++ rendered canonical block:claude-code-mpm
@@ -17,7 +17,7 @@
    recent topics, recent memories, recent milestones, last handoff,
    open work, overdue scheduled wakes, and a bounded
    `<available_skills>` catalogue). Decisions and lessons are
-   reachable via `mpm_decisions` / `mpm_lessons`, not carried in
+   reachable via `mpm__mpm_decisions` / `mpm__mpm_lessons`, not carried in
    wake. Use `params.projection: "compact"`
    for a small id+summary envelope — the full payload is the
    default.


--- COPY/PASTE DRIFT: hermes-mpm ---
--- copy/paste example:hermes-mpm
+++ rendered canonical block:hermes-mpm
@@ -17,7 +17,7 @@
    recent topics, recent memories, recent milestones, last handoff,
    open work, overdue scheduled wakes, and a bounded
    `<available_skills>` catalogue). Decisions and lessons are
-   reachable via `mpm_decisions` / `mpm_lessons`, not carried in
+   reachable via `mcp__mpm__mpm_decisions` / `mcp__mpm__mpm_lessons`, not carried in
    wake. Use `params.projection: "compact"`
    for a small id+summary envelope — the full payload is the
    default.


[render_managed_blocks] 2 drift(s) detected.
```

**Why this is a defect (not just cosmetic):**

1. `make refresh-installed` exits non-zero (Error 6 — post-refresh
   `--check` failed). The brief lists this as a baseline command; it
   is broken in the post-D-3 tree.
2. The two copy/paste examples that drift are the two hosts that
   apply a transport-namespace prefix to every `mpm_*` token
   (Claude Code → `mpm__mpm_*`, Hermes → `mcp__mpm__mpm_*`). The two
   hosts with no prefix (OpenCode, Pi) happen to render unchanged.
3. The drift is reproducible: anyone running the render-check or the
   installer round-trip will see it.

**Root cause (deterministically inferable):**

The D-3 commit replaced the original wake-payload phrase (which
mentioned "key decisions, active lessons" — neither prefixed with
`mpm_`) with a phrase that mentions `mpm_decisions` / `mpm_lessons`.
The render script applies the per-host transport prefix to EVERY
canonical `mpm_*` token via word-boundary substitution
(see `render_managed_blocks.py:render_for_host`). The new phrase is
the first place in the canonical block where the names `mpm_decisions`
and `mpm_lessons` appear in a context that is meant to be
transport-prefix-agnostic (because those two names are unprefixed
tool names — decisions/lessons are accessed directly, not via the
host's transport namespace).

The copy/paste examples for each host must match the rendered
output byte-for-byte (per the file's own header: "Drift detection
tests... assert that each block below is byte-for-byte identical to
the rendered canonical block"). The D-3 edit updated the copy/paste
examples to use the un-prefixed form (`mpm_decisions` /
`mpm_lessons`), but the render applies the per-host prefix and
produces a different string — so the file is now internally
inconsistent with what the render produces.

**Why the existing regression did not catch it:**

The post-D-3 fix added `TestPass2_Snippets_WakePayloadFieldsAreAccurate`
which only checks for the banned phrases ("key decisions",
"active lessons", "key pointers") in the snippet file. That test is
necessary but not sufficient — it catches drift in wording content
but not drift in the render's prefix-substitution output. The
existing render parity test (`render_managed_blocks.py --check`) is
the right layer for this defect class but is not wired into the
default `make test` flow.

### Items considered and rejected

- **Per-tool contract drift (Theme A).** The new bidirectional
  parity helper from the closure pass catches registry↔dispatcher
  drift. A targeted grep confirmed all 17 tools' enums either
  match their dispatcher case lists or have no enum contract. No
  new candidates.
- **Silent data loss (Theme B).** Spot-checked `_, _ =` and
  `_ =` patterns in `handlers.go`. The discarded returns are
  deliberate (`RecordRetrieval`, `MarkHandoffRead`,
  `BroadcastMemory`, `MarkFired`) per the F12-1 + alpha-4 design
  where retrieval/marking is best-effort and the calling code
  has its own error path. No new candidates.
- **Resource lifecycle (Theme G).** Sampled `rows.Close()` /
  `defer rows.Close()` patterns across handlers.go,
  wake_context.go, and scheduler/. The `wake_expiration.go`
  pattern uses explicit `rows.Close()` at three early-return
  sites rather than `defer` — provably safe under normal
  control flow (no panic paths reach the post-close code), just
  stylistically dated. Brief §G excludes "stylistic omissions
  where lifecycle is provably safe."
- **Pointer architecture (Theme I).** Sampled pointer URIs
  (`mpm://memory/<id>`, `mpm://lesson/<id>`, `mpm://work/<id>`,
  `mpm://theory/<id>`). Construction is consistent. The
  bounded-echo envelope in summary projection is bounded and
  emits truncation markers (e.g., `"[truncated, resolve pointer
  for full text]"` in the lessons search output). Operationally
  verified end-to-end via `mpm call`.
- **State machine integrity (Theme C).** `WorkStatus` enforces
  `open → done | cancelled`, `done | cancelled → open`. The
  `done → done` self-transition is allowed (idempotent). `done
  → cancelled` is rejected. Schema-level invariants look
  correct. No active defect surfaced.
- **Concurrency (Theme F).** The `go func() { done <- h(w) }()`
  pattern in `scheduler.go:435` returns when `ctx.Done()` fires
  without waiting for `h(w)` to complete — but `done` is
  buffered (1), the goroutine exits when `h(w)` returns, and the
  handler is intentionally independent of ctx. Brief §F excludes
  "design choices that are provably safe."
- **Input boundary reliability (Theme D).** The F12-1 weight
  type guard (`parseWeightStrict`) is the canonical strict
  parser; `parseFloatStrictOr` is the named-field counterpart
  for evidence strength/independence_factor. Other params
  (`delta`, `days`, `limit`, `offset`, `max_hints`,
  `max_batches`) use permissive `internal.ParseFloatOr` — by
  design per the F12-1 doc comment ("internal.ParseFloatOr is
  intentionally permissive (silent coercion for limit/offset/
  timeout where wrong-type-→-default is harmless)"). No new
  candidates.
- **Persistence / migration integrity (Theme E).** The
  `created_at` NOT NULL enforcement (commit 1aa8457) and the
  reader-side non-NULL contract (commit fa62c80) are both in
  place. The `cmd/mpm-mcp/spill_test.go` untracked file pre-dates
  this session (Aug 21 timestamp) and is out of scope.

---

## §D — Next remediation batch (one item, honestly)

The brief states *"Prefer a coherent batch of independent defects"*
and *"If only one, two, or three remain, report exactly that
number. ... Do not invent filler."* One reproducible defect survived
verification. One is selected; three are NOT invented.

### D-R1 — `make refresh-installed` parity (introduced by D-3 commit)

#### Problem

Commit `16a561e` (D-3 closure) updated the canonical snippet file's
wake-payload wording to reference `mpm_decisions` / `mpm_lessons`
as the routes for decisions/lessons. The render script applies a
host transport-namespace prefix to every canonical `mpm_*` token;
the two copy/paste examples that use a transport prefix (Claude
Code, Hermes) therefore drift from the render's output. The
OpenCode and Pi copy/paste examples do not drift (they use no
prefix). `make refresh-installed` exits non-zero with `Error 6`.

#### Reproduction / proof

```bash
$ python3 agent_installation/scripts/render_managed_blocks.py --check
[... 2 drifts shown for claude-code-mpm and hermes-mpm ...]
[render_managed_blocks] 2 drift(s) detected.
$ echo $?
1

$ make refresh-installed
[... fails with same drift ...]
make: *** [Makefile:227: refresh-installed] Error 6
```

#### Root area

- `agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md` lines 65,
  164, 263, 362, 505 — the wake-payload phrase in the canonical
  block AND each host's copy/paste example.
- `agent_installation/scripts/render_managed_blocks.py` —
  `render_for_host` does the prefix substitution.

#### Expected behaviour

`python3 agent_installation/scripts/render_managed_blocks.py --check`
exits 0; `make refresh-installed` exits 0; the rendered output for
each host matches the host's copy/paste example byte-for-byte.

#### Fix strategy (smallest plausible root-cause correction)

**Recommended (Option A — wording fix):** rewrite the wake-payload
sentence so the un-prefixed tool names `mpm_decisions` /
`mpm_lessons` are not present in a context the render script would
prefix-substitute. Two wording shapes work:

1. **Use the names without leading `mpm_`:**
   > "Decisions and lessons are reachable via the
   > `decisions` / `lessons` tools, not carried in wake."

2. **Use a form that defers naming to a parenthetical:**
   > "Decisions and lessons are not part of the wake payload
   > itself — recover them via the dedicated decisions and
   > lessons tools."

Both eliminate the `mpm_*` token the render script would otherwise
prefix. The semantic content ("not in wake, recover via dedicated
tools") is preserved.

**Alternative (Option B — render-script fix):** update
`render_for_host` in `render_managed_blocks.py` to skip the
`mpm_decisions` and `mpm_lessons` tokens during prefix substitution
(they are the only two un-prefixed tool names in the canonical
block; the asymmetry is host-prefix-agnostic by design). This
preserves the canonical wording verbatim but pushes the asymmetry
into the render script.

**Selection rationale:** Option A is the smallest plausible
correction (one-line per occurrence × 5 = 5-line change), preserves
the existing prefix-substitution invariant, and matches the
brief's preference for narrow TDD-fixable defects. Option B is
structurally cleaner but requires an extra-paragraph in the render
script documenting the asymmetry.

**Apply the same rewording to all 5 sites** (the canonical block
plus the 4 host-specific copy/paste examples) so the file remains
internally consistent.

#### Regression

Add a sibling test to `internal/core/post_pass2_drift_lock_test.go`
that invokes `render_managed_blocks.py --check` as a subprocess
(or — to keep the test in pure Go — read the snippet file and the
4 adapter-template files and assert byte-equality with the rendered
output computed by a Go port of the render's prefix-substitution
logic). The test must FAIL on the post-D-3 tree and PASS after the
wording fix.

Practical minimum: a shell-level test that runs
`python3 agent_installation/scripts/render_managed_blocks.py --check`
and fails the Go test if it exits non-zero. Existing test files in
`tests/test_render_managed_blocks.py` likely cover this at the
Python layer; if so, the Go-level sibling is redundant and the
existing Python test is the canonical regression target.

#### Scope boundary

- Do NOT amend the D-3 commit (it is the closure record for the
  residual inventory; the wording fix lands as a separate commit).
- Do NOT modify the render script's prefix-substitution logic in
  Option A (Option B is the structure-preserving alternative; pick
  one, do not bundle).
- Do NOT touch any other defect — sweep scope is one finding.
- Do NOT run `make refresh-installed` in a way that silently
  rewrites the snippet file's copy/paste examples to match the
  render output (that would mask the drift by overwriting the
  human-readable examples with the prefixed form, which loses the
  document value of having examples that match the rendered output
  in the source file).

---

## §E — Deferred risks / debt

| Item | Why deferred | Source |
|------|-------------|--------|
| Bare `go test -race ./...` 8 failures | **Environment-only** — requires FTS5 build flags + cgo env not set; canonical `make test-race` is 0. Same classification as the prior residual inventory. | pre-existing-race-failures-closure-2026-09-04.md |
| `cmd/mpm-mcp/spill_test.go` untracked | **Out of scope** — pre-dates this session (Aug 21 timestamp), not touched by D-1/D-2/D-3 closure; carrying it forward unchanged. | `git status` |
| `wake_expiration.go` rows lifecycle uses explicit-close instead of `defer rows.Close()` | **Already adequately covered** — provably safe (three close sites cover all control-flow paths that don't panic); brief §G excludes stylistic omissions where lifecycle is provably safe. | this sweep §C "Items considered" |
| Permissive `internal.ParseFloatOr` for `delta`, `days`, `limit`, `offset`, `max_hints`, `max_batches` | **Already adequately covered** — deliberate design per F12-1 doc comment; strict typing reserved for fields where wrong-type-→-default is misleading (weight, strength, independence_factor). | this sweep §C "Items considered" |
| `go func() { done <- h(w) }()` in scheduler.go:435 returns without waiting on `ctx.Done()` | **Already adequately covered** — handler is deliberately ctx-independent; buffered channel prevents send-block. | this sweep §C "Items considered" |
| C-3 (canonical snippet version marker), C-5 (adapter metadata duplication) | **Low impact** — post-pass-2 known debt, unchanged by this sweep. | agent-integration-post-pass-2-reconciliation-2026-09-04.md |
| C-2 architecture option (add RecentDecisions / RecentLessons fields to wake) | **Needs larger architectural work** — D-3 chose the documentation-fix path; the architecture alternative remains on the table for a future design pass. | residual-defect-inventory-2026-09-04.md §D D-3 alternative |
| D-4 candidate: per-param parity audit beyond enums | **Needs larger architectural work** — risk rather than confirmed defect; the bidirectional enum lock from the closure pass only locks enum↔dispatcher, not per-arg shape. Promoting would require an explicit expanded-scope pass. | residual-defect-inventory-2026-09-04.md §C "Items considered" |

---

## §F — Recurring defect families

Two families are visible across this sweep + the prior closure pass:

1. **`runtime ↔ installer` / `documentation ↔ implementation`
   drift.** D-3 fixed the wording of the canonical snippet file
   without validating that the per-host copy/paste examples remain
   in sync with the render's prefix-substitution output. The
   `render_managed_blocks.py --check` gate catches this drift but
   is not wired into `make test`. **Structural prevention
   (cheap):** add a Go test that runs
   `python3 agent_installation/scripts/render_managed_blocks.py
   --check` as a subprocess and fails the test on non-zero exit;
   or run the script in CI directly. Both are ≤ 20 lines.

2. **`schema ↔ handler` action enum drift.** Closed in the prior
   pass with the bidirectional parity helper. No recurrence in
   this sweep. The helper is reusable; extending it to other
   contract surfaces (required fields, output shape) is the
   natural follow-on if such drift surfaces.

No new framework is warranted. The render parity gate is the
right cheapest layer for family #1; the existing parity helper
already covers family #2.

---

## §G — Test gaps

For D-R1:

**Why did this defect survive?**

The D-3 closure added `TestPass2_Snippets_WakePayloadFieldsAreAccurate`
which pins the absence of banned phrases. That test is necessary
(wording drift) but not sufficient (render parity). The render
parity is checked by `render_managed_blocks.py --check` — a Python
script with its own test suite
(`tests/test_render_managed_blocks.py`) — but neither is wired into
the canonical Go `make test` flow. The default CI path runs
`make test` / `make test-race` and assumes the canonical snippet
file is in sync; it isn't.

**Right prevention layer:** render parity as a CI gate, not as a
manual check. The cheapest reliable fix is either:

1. Add a Go test that invokes
   `python3 agent_installation/scripts/render_managed_blocks.py
   --check` as a subprocess and asserts exit 0 (≤ 20 lines,
   reuses the existing Python infrastructure).
2. Move `render_managed_blocks.py --check` into the Makefile's
   `test` target so it runs alongside the Go tests (requires a
   Python dependency in the CI environment).

Option 1 is the lower-friction fix; Option 2 is the more durable
fix.

---

## Final verdict

- **Current baseline established: YES** (§A; all gates run; one
  gate — render parity — actively FAILs and is recorded honestly).
- **Current failures classified: YES** (the `make refresh-installed`
  failure is a downstream effect of the render parity failure;
  the 8 known FTS5-flag-bound `go test -race ./...` failures
  inherit the pre-existing classification verbatim).
- **Previously closed findings remain closed: YES** (§B; every
  named closure re-checked against current code/tests; only the
  canonical managed snippets area partially regressed and that
  is recorded as the new finding).
- **New active defects identified: YES** (one — D-R1, the render
  parity drift introduced by the D-3 closure commit).
- **Residual backlog has evidence for every item: YES** (§C; the
  verbatim `--check` output is the evidence; the "rejected" items
  have documented rationales).
- **Next remediation batch selected: YES** (one — D-R1; the
  implementation brief is in §D).
- **Fewer than four active defects when applicable: YES** (one
  finding, honestly reported per the brief's instruction).
- **No filler findings invented: YES** (only one item in §D; three
  slots intentionally left unfilled per the brief's anti-filler
  rule).
- **Recurring defect families identified: YES** (§F; the
  `runtime ↔ installer` family is named with a cheap structural
  prevention proposal).
- **Test gaps identified: YES** (§G; render parity not wired into
  the canonical Go test flow; Go-level sibling or Makefile gate
  is the cheapest reliable prevention).

---

## Footnote — what this document does NOT do

- It does not start any code change.
- It does not commit anything to `git`.
- It does not amend the alpha-final tag.
- It does not amend commit `16a561e` (the D-3 closure commit);
  the wording fix lands as a separate commit when the user
  approves the inventory.
- It does not run `make refresh-installed` in a way that
  silently rewrites the snippet file's copy/paste examples.
