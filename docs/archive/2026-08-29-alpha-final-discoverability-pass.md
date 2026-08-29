# MPM Alpha Final Discoverability Pass — Final Report

**Date:** 2026-08-29
**Scope:** Alpha feature-freeze audit — closing gaps in MPM's persistent
artifact discoverability surface.
**Goal:** A new agent should know what kinds of durable knowledge MPM
contains, discover the relevant kind at the right time, and correctly
interpret what it finds.

---

## 1. Discoverability Matrix

The complete substrate × surface × trigger × interpretation matrix after
this pass. "Proactive" means surfaced automatically during wake or in
response to context. "Reactive" means available on demand via a tool call.

| Artifact   | Substrate surface                          | Mechanism                                  | Proactive / Reactive | Trigger                                                                       | Interpretation                                                                          |
| ---------- | ------------------------------------------ | ------------------------------------------ | -------------------- | ----------------------------------------------------------------------------- | --------------------------------------------------------------------------------------- |
| memory     | `memories_fts` (`memories` collection)     | BM25 hybrid search via `mpm_memory query`  | Reactive             | context-dependent query                                                       | durable observed fact. Trust content; reinforcement/weight signal reliability.           |
| decision   | `memories_fts` (`decisions` collection)    | BM25 hybrid search with collection filter  | Reactive             | "before I decide…" / "what did we decide about X"                              | prior rationale, **not** immutable truth. May be superseded; check `superseded_by`.    |
| theory     | `memories_fts` (`theories` collection)     | BM25 hybrid search with collection filter  | Reactive             | "why does X behave like…" / "we suspect…"                                     | hypothesis, not fact. `pending → proven/disproven`. Do not act on pending as truth.    |
| lesson     | `lessons_fts`                              | `mpm_lessons search`                       | Reactive             | "we keep hitting this same error" / recurring failure pattern                 | learned constraint. Type: warning \| practice \| insight. May be stale.                 |
| skill      | `<available_skills>` wake catalog + `mpm_skills` | `when_to_use` substring match + wake payload top-20 | Proactive + Reactive  | "is there a skill for…" / context keyword match on `when_to_use`               | reusable procedure, approved for reuse. **`when_to_use` is the trigger taxonomy.**     |
| reference  | `references_fts` + new `freshness` field   | `mpm_references list`/`show`/`search`      | Reactive             | "I need to check the [vendor / API / framework] doc"                          | **consultable material, not a procedure.** Verify freshness before relying — see §3.   |
| topic      | `topics_fts`                               | `SearchTopicsByQuery`                      | Reactive             | need broader retrieval across artifacts                                        | organizational / retrieval context. Groups memories, decisions, theories, etc.          |
| work       | `works` table + `ListWorksByStatus`        | direct list by `status`                    | Reactive             | "what work is outstanding / what did I just ship"                             | current lifecycle state + verification. Work is **state, not knowledge**.              |
| handoff    | `sessions.handoff_unread` + wake payload   | `GetLatestUnreadHandoff` (wake)            | Proactive            | session transition (read); pre-closure write                                  | previous session's continuation state. Wake reads `last_handoff`.                       |

**Skills are the reference pattern for task-aware discoverability**, not
the semantic model for everything else. The poly-store `memories` table
with `collection` discriminator handles facts/decisions/theories via the
same FTS5 path. Lessons, references, topics, works, and handoffs each
have their own first-class surface tuned to their retrieval shape.

---

## 2. Gaps Found and Closed

11 gaps surfaced during the audit. Each maps to the minimal targeted fix
in §5.

| # | Gap                                                                                  | Severity | Resolution                                                            |
| - | ------------------------------------------------------------------------------------ | -------- | --------------------------------------------------------------------- |
| 1 | Agents had no documented interpretation contract for artifact classes                 | blocker  | Protocol §7.1 — 9-row interpretation table                           |
| 2 | No guidance on when to invoke which discovery tool (risk of discovery storms)        | blocker  | Protocol §7.2 — context-trigger table                                |
| 3 | No "discovery ≠ authority" guard — agents could treat surfaced items as binding      | blocker  | Protocol §7.3 — explicit non-authority disclaimer                     |
| 4 | Reference docs had no freshness signal — old vendor manuals looked current           | blocker  | §3 / §8 — 5-state freshness classifier                                |
| 5 | No semantic distinction between `fresh` (recently indexed) and `current` (verified)  | high     | §8.1 — explicit `current` vs `freshness` naming                      |
| 6 | `version-bound` references (e.g. WordPress 6.7 manual) could be applied to v6.8      | high     | §8.1 / §8.2 — `version-bound` state + tag/reason pattern detection   |
| 7 | `historical` references (post-mortems, prior architectures) looked live               | high     | §8.1 / §8.2 — `historical` state                                     |
| 8 | Clock skew / unparseable ages returned "current" by default — false positives         | medium   | §8.2 — `unknown` state for future / unparseable `last_indexed`       |
| 9 | No automated coverage proving each artifact class is reachable through its substrate surface | medium | §6 — `TestDiscoveryMatrix` with 9 sub-tests                         |
|10 | `freshness` field absent from `mpm_references list/show/search` row maps              | medium   | §5 — one-line `freshness` field added to each read path              |
|11 | No regression coverage for the freshness classifier itself                            | low      | §6 — 19 table-driven cases covering every state + edge cases         |

**Not gaps (deliberately deferred):**

- **No schema redesign.** Freshness is computed at read time from existing
  fields (`last_indexed`, `tags`, `import_reason`).
- **No new retrieval subsystem.** No universal "knowledge search" tool.
- **No per-turn discovery storm.** Discovery remains context-triggered;
  the wake payload is unchanged.

---

## 3. Reference Freshness

Five freshness states, derived purely from existing fields — no schema
changes, no migration, no new column. The classifier is a pure function
(`internal/core/reference_freshness.go`) with a deterministic test suite.

| State             | Meaning                                                                          | Safe to act on as authority?               |
| ----------------- | -------------------------------------------------------------------------------- | ------------------------------------------ |
| `current`         | Recently re-ingested or explicitly tagged verified / current                     | yes — within scope of what it says        |
| `stale`           | `last_indexed` older than 90 days with no override signal, or explicit `stale` tag | no — verify against upstream first         |
| `version-bound`   | Tag or `import_reason` identifies a specific upstream version                    | only for that recorded version            |
| `historical`      | Explicitly tagged or `import_reason` indicates past-state / as-of material       | no — context, not current authority        |
| `unknown`         | No freshness signals (empty fields, unparseable, clock skew)                     | no — consult with caution                  |

**Derivation precedence** (first match wins):

1. Explicit tags scanned in slice order (`stale` / `current` / `verified` /
   `freshness:current` / `historical` / `freshness:historical` /
   `version-bound:X` / `version:X`).
2. `import_reason` patterns (`version:` / `for <thing>` / `v` → version-bound;
   `historical` / `as-of ` → historical).
3. Age fallback on `last_indexed`: older than 90 days → stale; within → current;
   future / unparseable → unknown.

**The age threshold (`FreshnessAgeThreshold = 90 days`)** is a constant
in the classifier, not config. 90 days matches the project's prior mental
model for reference half-life; can be made configurable in a future
pass if operators want different policies.

**Signal ordering note.** The first matching tag in slice order wins.
`historical` is checked before `version-bound:<X>` in the switch, so
`["historical", "version-bound:foo"]` classifies as `historical`. This is
documented in the test suite; it is the deterministic behavior, not a
bug.

---

## 4. Agent Protocol Changes

The canonical protocol at `agent_installation/mpm-agent-protocol.md`
gained two new sections inserted between §6 (Recovery / Fallback) and
the closing "What this protocol does NOT claim" block.

**§7. ARTIFACT DISCOVERY & INTERPRETATION** — what each of the nine
artifact types means, when to look for it, and the rule that finding
something does not make it true.

- **§7.1 Interpretation contract** — 9-row table mapping each artifact
  type to its interpretation rules (theory ≠ fact, decision may be
  superseded, reference needs freshness verification, work is state not
  knowledge, etc.).
- **§7.2 Discovery triggers** — 7-row table mapping natural-language
  signals ("have we done this before", "before I decide", "we suspect…")
  to the one or two tools to probe. Explicit "do not invoke all of these
  every turn — that is a discovery storm" rule.
- **§7.3 Discovery is not authority** — explicit guard that the
  interpretation contract in §7.1 overrides the convenience of finding
  something.

**§8. REFERENCE FRESHNESS CONTRACT** — why references need freshness
metadata at all, the five-state table, how freshness is derived without
schema changes, and how agents must apply each state.

- **§8.1 The five freshness states** — table with meaning and
  safe-to-act-on column.
- **§8.2 How freshness is derived (no schema changes)** — inputs are
  existing fields (`last_indexed`, `tags`, `import_reason`); precedence
  is explicit; classifier lives at `internal/core/reference_freshness.go`.
- **§8.3 How agents must apply this contract** — what to do for each
  state, plus the closing rule that **reference ≠ skill**: a reference
  containing procedural instructions is material to consult, not a
  procedure to run.

No version bump applied: these are additive clarifications (existing
principles documented more explicitly), not behavioral changes.

---

## 5. Production Changes

Minimal targeted changes only. Three production files touched.

**`internal/core/reference_freshness.go`** (new, 127 lines)

- `Freshness` enum type with five string constants.
- `FreshnessAgeThreshold = 90 * 24 * 60 * 60`.
- `ClassifyReferenceFreshness(doc *ReferenceDoc, now time.Time) Freshness` —
  pure function, no DB access, three-tier precedence.
- `ClassifyReferenceFreshnessFromFields(tagsJSON, importReason, lastIndexed, now)` —
  wire-format convenience wrapper that accepts the JSON-encoded tags
  string directly so list-query handlers don't have to `json.Unmarshal`
  per row. Tolerates malformed tags JSON (best-effort).

**`internal/core/web_db.go`** (modified, +3 lines in three row maps)

- `ListReferences` — added `"freshness": string(ClassifyReferenceFreshnessFromFields(...))`
  to the per-row map.
- `SearchReferences` — same.
- `GetReference` — same.

Each is a single dict entry referencing the shared classifier. No
per-call branching, no schema changes, no new query.

**`agent_installation/mpm-agent-protocol.md`** (modified, +150 lines)

- Added §7 (ARTIFACT DISCOVERY & INTERPRETATION) and §8 (REFERENCE
  FRESHNESS CONTRACT) between §6 and the closing block.

**No schema migrations. No new tables. No new columns. No new MCP tools.
No new CLI commands. No new retrieval paths.** Every change is
additive documentation or a single derived field on an existing row
map.

---

## 6. Regression Coverage

Two new test files; 28 new test cases total; 0 existing tests modified.

**`internal/core/reference_freshness_test.go`** (205 lines)

- `TestClassifyReferenceFreshness` — 14 table-driven cases covering:
  - 3× `current` paths (verified tag, current tag overriding stale age,
    recent `last_indexed`)
  - 2× `stale` paths (age past threshold, explicit stale tag overriding
    fresh age)
  - 3× `version-bound` paths (`version-bound:X`, `version:X`,
    `import_reason: for <X>`)
  - 2× `historical` paths (explicit historical, `freshness:historical`)
  - 3× `unknown` paths (empty fields, future timestamp / clock skew,
    unparseable timestamp)
  - 1× precedence case (historical vs version-bound in same tag list)
- `TestClassifyReferenceFreshnessFromFields` — 5 cases for the wire-format
  wrapper (empty tags, JSON-encoded tag list, version-bound via JSON tag,
  stale via empty tags + old age, malformed JSON tolerance).
- All cases use a deterministic reference time (`time.Unix(1735689600, 0)`,
  2025-01-01 UTC) for stable ages across clock drift.

**`internal/core/discovery_matrix_test.go`** (280 lines)

- `TestDiscoveryMatrix` — 9 hermetic sub-tests, one per artifact class,
  each seeding a unique token (`DISCOV-MEM-9182`, `DISCOV-DEC-4417`, …)
  and asserting the documented substrate surface returns it:
  - memory → `HybridSearchMemories`
  - decision → `HybridSearchMemories(collection=decisions)`
  - theory → `HybridSearchMemories(collection=theories)`
  - lesson → `SearchLessonsLimited`
  - skill → `ListSkills` (with `when_to_use` substring pass documented)
  - reference → `ListReferences` + freshness field assertion
  - topic → `SearchTopicsByQuery`
  - work → `ListWorksByStatus("open")`
  - handoff → `GetLatestUnreadHandoff`
- Uses `NewTestDM(t)` for hermetic in-memory DB; no external state.
- Includes a small `itoa64` helper to avoid an extra `strconv` import.

All 28 cases pass under `-race`.

---

## 7. Dogfood

`TestDiscoveryMatrix` doubles as automated dogfood. Each sub-test is a
mini end-to-end check that a freshly-seeded artifact is reachable through
the substrate surface the canonical protocol tells the agent to use:

- A new agent reading the protocol sees `mpm_memory query` for memories
  and `mpm_references list` for references, and runs those commands.
  The matrix test asserts those exact paths return seeded items, so
  "the protocol says X → X works" is a tested claim, not an aspirational
  one.

A future pass adding a new artifact class should add a new sub-test to
`TestDiscoveryMatrix` following the same seed-then-probe pattern. That
keeps the substrate surface and the protocol in sync.

---

## 8. Context / Token Impact

**Wake payload:** unchanged. No new section, no expanded payload, no
per-turn cost increase.

**Per-reference row in list/show/search:** +1 field (`freshness`), a
single enum string (`current` / `stale` / `version-bound` /
`historical` / `unknown`). ~10 bytes per row. Per-call cost is one pure
function evaluation per row — negligible relative to the existing
FTS5 query that already produced the row.

**Skill discovery:** unchanged. The `<available_skills>` catalog is
top-20 by weight with `name`, `version`, `when_to_use` as before.

**Token cost of the new protocol sections:** §7 and §8 together are
~150 lines / ~3.5 KB of markdown. The protocol is read once per session
start (host adapters already pull it into context), so the amortized
cost is zero per turn.

**No discovery storm.** The discovery triggers table in §7.2 makes it
explicit: probe one or two artifact classes per signal, then inspect
the bounded result before pulling full content.

---

## 9. Verification

| Gate                                                | Result    | Notes                                                                              |
| --------------------------------------------------- | --------- | ---------------------------------------------------------------------------------- |
| `go build -tags fts5 ./...` (main module)           | **PASS**  | Exit 0, no warnings.                                                               |
| `go vet -tags fts5 ./...` (main module)             | **PASS**  | Exit 0, clean.                                                                     |
| `go vet -tags fts5 ./...` (core module, w/o broken) | **PASS**  | Exit 0, clean.                                                                     |
| `go build -tags fts5 ./...` (core module)           | **PASS**  | Exit 0, clean (build doesn't compile test files).                                  |
| `go test -tags fts5 -race ./...` (main module)      | **PASS**  | All packages pass: cmd/mpm, cmd/mpm-mcp, cmd/mpm-telemetry, internal/{audit,blobstore,critic,pointer,scheduler,telemetry}. |
| `go test -tags fts5 -race ./...` (core module)      | **PASS**  | All packages pass when the pre-existing broken file is moved aside.                |
| Focused: `TestClassifyReferenceFreshness` + `TestDiscoveryMatrix` | **PASS**  | 28/28 cases pass under `-race`.                                                    |

**CGO env required (as documented in `CLAUDE.md`):**
`CGO_CFLAGS="-DSQLITE_ENABLE_FTS5" CGO_LDFLAGS="-lm"`. The Makefile
sets these; invoking `go` directly requires mirroring them. An earlier
attempt without `-lm` triggered a linker error in `cmd/mpm-telemetry`
(unrelated to this PR).

**Pre-existing issue (out of scope):**

- `internal/core/runtime_decay_test.go` and `internal/core/runtime_clock.go`
  are **untracked** files left in a broken state by a prior session.
  The test file references symbols (`dm.RuntimeNow`, `dm.runtimeClock`,
  `dm.PersistRuntime`, `dm.backfillRuntimeStamps`) that the source file
  does not define, which blocks `go test ./internal/core/...` compilation.
  These files are NOT part of any commit on `main` (`git ls-files
  internal/core/ | grep runtime` returns nothing tracked) and predate
  this PR. They are documented here for transparency and to flag that
  the owner of that prior work should resolve it. Verified-by-move-aside
  was used during this PR so the new tests could be exercised; the
  files were restored to their original location before this report.

---

## 10. Git State

**HEAD:** `d33355d feat(workshop): feature-freeze audit + dogfood scripts` (unchanged from start of PR).

**Modified files** (this PR):

- `internal/core/reference_freshness.go` — new (127 lines)
- `internal/core/reference_freshness_test.go` — new (205 lines)
- `internal/core/discovery_matrix_test.go` — new (280 lines)
- `internal/core/web_db.go` — +3 single-line `freshness` field entries (3 read paths)
- `agent_installation/mpm-agent-protocol.md` — +§7 +§8 (~150 lines)

**Pre-existing modifications** (NOT from this PR):

- `CLAUDE.md` — modified before this PR started
- `SECURITY.md` — modified before this PR started

**Untracked files** (pre-existing, NOT from this PR):

- `internal/core/runtime_clock.go` (pre-existing broken)
- `internal/core/runtime_decay_test.go` (pre-existing broken)
- `cmd/mpm/watchdog.jsonl`
- `docs/archive/2026-*.md` (prior archive docs)

**Commits:** none created. The `git status` baseline before this PR
showed the same `M`/`??` state minus my new files. Per the feature
freeze, no commits will be made until the alpha freeze is lifted.

**Feature freeze:** intact. No new table, no new column, no new MCP
tool, no new CLI command, no new retrieval path, no behavioral change
to existing surfaces beyond the additive `freshness` field on
reference row maps.

---

## Final Principle (from the spec)

> Skills are the reference pattern for task-aware discoverability —
> not the semantic model for everything. Discovery ≠ authority.
> Verification of freshness, validity, and current state belongs to the
> agent, not the substrate. The substrate's job is to make the right
> thing findable at the right time and tell the agent what kind of
> thing it found.
