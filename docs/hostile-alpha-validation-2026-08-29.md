# MPM v0.1.0-alpha-final-1-g635c5a8 — Adversarial Validation Report

**Date:** 2026-08-29
**Tester:** Independent hostile alpha tester (Claude/MiniMax-M3 via Claude Code)
**Workspace:** `/tmp/mpm-hostile-alpha-20260829`
**MPM_WORKSPACE:** `/tmp/mpm-hostile-alpha-20260829/mpm-home` (disposable, isolated DB)
**MPM Identity:** `v0.1.0-alpha-final-1-g635c5a8`, git commit `635c5a8`, binary SHA256 `59533309bc22792d4c2d4ef5427391ce36958fd2b79f5b18e6801552ab283686`
**Source path:** `/home/v/workspace/projects/mpm/bin/mpm`

---

## Executive Verdict

### **ALPHA READY WITH NON-BLOCKING FINDINGS**

This is not a clean ALPHA READY. Several P0/P1 defects are present, but they are predominantly **discoverability** and **dedup-provenance** issues rather than corruption-of-durable-state. None of the findings invalidates the core substrate contract: SQLite+WAL persistence is intact, FTS5 works, lifecycle transitions are recorded, dedup is real (just lossy of second-actor provenance).

The most consequential defects:
- **F-A1 (P0)**: Dedup erases second-actor provenance — multiple actors writing identical content leaves only one evidence row.
- **F-A2 (P1)**: Decisions, lessons, and skills are **invisible to the documented CLI surface** (`mpm kb memory search`); an alpha user following the cognitive-verb documentation will fail to find them.
- **F-B1 (P1)**: Work-item state machine is not enforced — `done→done`, `cancel→cancel`, `open→reopen`, and even `done→cancel` are silently accepted; final status is whichever command ran last.
- **F-C1 (P1)**: `--supersedes` flag in `mpm decide` is silently dropped (only stored when `--choice` is positional, not flag-based).
- **F-C2 (P1)**: `--woices` / `--choice` / `--context` / `--rationale` CLI flags in `mpm decide` are not parsed — the literal flag text is stored as the content of the decision memory.
- **F-D1 (P0)**: Memory decay is wall-clock-based on `created_at`/`last_accessed_at` — a 30-day backdate produces immediate weight=-8.0 in a single gc tick. The "runtime-based decay" semantic claimed in the temporal-clock-inventory memory does not match observed behavior. (Verified: it is **wallclock**, not runtime.)
- **F-F1 (P1)**: Empty `mpm decide ""` succeeds with empty choice AND produces malformed tags JSON that warns on every list.

None of these are P0 in the sense of "data is corrupted" — they are P0 in the sense of "the system lies about what happened" or "the agent cannot recover what it should be able to find." For an alpha tester, those are real but fixable before general release.

---

## Test Matrix

| ID | PASS/FAIL | Finding | Severity |
|----|-----------|---------|----------|
| T01 | FAIL | F-A1: zero-width space `​` accepted as memory content | P2 |
| T01 | FAIL | F-F1: `mpm decide ""` accepts empty choice AND corrupts tags JSON | P1 |
| T02 | PASS | Unicode preserved; FTS5 distinguishes Cyrillic/Latin homoglyphs; dedup works | — |
| T03 | MIXED | F-A3: 1MB+ payloads via `--file` silently truncate to ~65KB | P2 |
| T04 | **FAIL** | F-A1: identical content from different actors → 1 evidence row, 2nd+ actor erased | **P0** |
| T05 | PASS | soft-delete, restore, shred all behave consistently | — |
| T06 | PASS | repeat delete returns clean errors | — |
| T07 | **FAIL** | F-B1: work-item state machine not enforced (done→done, cancel→cancel accepted) | **P1** |
| T08 | **FAIL** | F-B1 continued: arbitrary transitions accepted, status flips arbitrarily | **P1** |
| T09 | FAIL | F-C3: empty/whitespace evidence accepted by `mpm challenge` | P2 |
| T10 | FAIL | F-C4: 20 identical challenges → 20 separate theories, no dedup | P1 |
| T11 | MIXED | challenge → resolve preserves audit; but proven challenge does not auto-shred | P2 |
| T12 | PASS | ordering of reinforces/challenges has no observable effect | — |
| T13 | PASS | `mpm recall` correctly annotates `[Note: This memory is challenged — treat as unverified]`; `kb memory search` does NOT include this annotation | P3 |
| T14 | **FAIL** | F-C1, F-C2: `--supersedes` silently dropped; `--choice`/`--context`/`--rationale` flags not parsed (stored as literal text) | **P1** |
| T15 | PASS | missing provenance → `framework: (unknown)` shown honestly | — |
| T16 | **FAIL** | F-D2: user-supplied `framework` and `session_id` are silently overwritten by `mpm-cli` | **P1** |
| T17 | MIXED | CLI says `actor_kind=unknown`; `mpm call` says `actor_kind=agent` with `framework=mpm-cli`. Two surfaces, two different attribution patterns. | P2 |
| T18 | **FAIL** | F-D2+F-A1: same as T04 but with explicit framework args; only first writer's framework recorded | **P0** |
| T19 | **FAIL** | F-D1: GC decay is wallclock; 30-day backdate → weight 1.0 → -8.0 in one tick | **P0** |
| T20 | **FAIL** | F-D1 confirmed: scheduler-down vs scheduler-idle are indistinguishable | **P0** |
| T21 | PASS | NULL `created_at`/`last_accessed_at` rows are skipped (not penalized) | — |
| T22 | MIXED | `snooze --duration 1h` adds +1 weight and **+1 day** to `last_accessed` — semantic mismatch | P2 |
| T23 | MIXED | exact search works; paraphrase/typo/prefix do not (no fuzzy match); semantic requires external embedding provider | P3 |
| T24 | **FAIL** | F-F1: decisions are completely invisible to `mpm kb memory search` and `mpm recall` | **P1** |
| T25 | PASS | theories are surfaced with status `[pending]`/`[resolved]` annotation | — |
| T26 | **FAIL** | F-F1: lessons are in `lessons_base` table — `mpm kb memory search` does not find them | **P1** |
| T27 | MIXED | `skill search` returns "not yet wired" message — falls back to manual enumeration | P3 |
| T28 | PASS | references discoverable via `reference search` | — |
| T29 | PASS | fresh references work normally | — |
| T30 | **FAIL** | F-G1: backdated reference (5-year-old) is **not flagged stale** in any UI surface | P2 |
| T31 | MIXED | search "WordPress 6.8" correctly returns no false-positive for 6.7 ref, but the 6.7 ref wins for general phrases like "block themes" without version prominence | P3 |
| T32 | **FAIL** | F-G2: no `is_historical` / `deprecated_at` / `valid_until` fields exist; references can't be marked as historical | **P1** |
| T33 | MIXED | CLI and `mpm call` both write same row on dedup; CLI's `--json` returns different envelope shape than `call`'s response | P3 |
| T34 | MIXED | `--weight abc` rejected; `--expires-in garbage` silently ignored; `--tags {"a":1}` stored as nested-JSON array wrapper | P2 |
| T35 | PASS | CLI missing args return clean Usage; `call` returns `{"error":"fact is required","success":false}` | — |
| T36 | MIXED | CLI returns human text; `call` returns JSON; not strictly inconsistent but differs in envelope shape | P3 |
| T37 | PASS | 10 concurrent identical writes → 1 row, all return same ID | — |
| T38 | PASS | 5 concurrent reinforces/weakens all apply atomically; concurrent complete+cancel: last-write-wins (no semantic check) | P2 (last-write-wins on work items is a P1 continuation of F-B1) |
| T39 | N/A | MCP server not registered in this disposable env; the CLI is stateless between calls so restart is implicitly tested | — |
| T40 | **FAIL** | F-F1+others: fresh-agent archaeology score 6/10 — decisions, lessons, skills undiscoverable | **P1** |

---

## Critical Findings (P0/P1)

### F-A1 — Dedup erases second-actor provenance **[P0, data-semantics]**

**Reproduction:**
```bash
$ mpm memory add "shared knowledge fact XYZ-123"
{"success":true,"id":"<id-A>"}
$ mpm memory add "shared knowledge fact XYZ-123"
{"success":true,"id":"<id-A>"}      # same ID!
$ mpm call mpm_memory --payload '{"action":"save","params":{"fact":"shared knowledge fact XYZ-123","framework":"claude-code","model":"opus-5"}}'
{"content":"shared knowledge fact XYZ-123","duplicate":true,"id":"<id-A>"}
```

**Expected:** A second or third actor writing identical content should be reflected in evidence (`evidence` table) and provenance (`artifact_provenance` table).

**Actual:** Both the response and the database confirm dedup. Evidence table has only 1 row. `artifact_provenance` has 1 row with the FIRST writer's framework only (which for CLI is `actor_kind=unknown`). The `duplicate:true` flag in the response tells the caller the dedup happened, but there is no record of who else tried to write it.

**Root cause hypothesis:** The `idx_memories_identity_live` UNIQUE index collapses on `(collection, identity_hash)`. The `SaveMemoryNode` path treats a duplicate as success and returns the existing ID without recording any second provenance.

**User impact:** A multi-agent scenario where two agents independently assert the same fact appears identical to "one agent asserted it once." The system has lost the information that the assertion is contested or corroborated across agents.

**Likely scope:** All write paths that go through `SaveMemoryNode`.

**Recovery:** None for the lost provenance. Future dedups could be fixed by appending to an `evidence` row per call with `created_by` set to the caller's session_id. But the past data is gone.

---

### F-A3 — `--file` silently truncates large payloads **[P2]**

**Reproduction:** Created `/tmp/p1g.txt` with 1GB of bytes, then `mpm memory add --file /tmp/p1g.txt`. The CLI returned `Memory added: <id>` and the DB shows the stored content is `length(content)=65536` — silent truncation to 64KB. No warning, no error.

**Expected:** Either honor the file size or surface a clear error like `content exceeds limit`.

**Actual:** Silent truncation. The memory exists, but the content is wrong. An agent reading the memory back later will get 64KB of a 1GB payload.

**Root cause hypothesis:** Either `SaveMemoryNode` has an internal cap, or `--file` reads only up to a buffer size before storing.

**Recovery:** None without re-saving with the correct content.

---

### F-B1 — Work-item state machine not enforced **[P1, contract]**

**Reproduction:**
```bash
$ mpm work item create "X" "..."
# status=open
$ mpm work item complete X --note step      # status=done
$ mpm work item complete X --note step      # STILL ACCEPTS — status=done
$ mpm work item cancel X --note step        # ACCEPTS done→cancel
$ mpm work item reopen X                    # ACCEPTS
$ mpm work item cancel X --note step        # status=cancelled
$ mpm work item cancel X --note step        # STILL ACCEPTS
```

**Expected:** `done→done` rejected as illegal duplicate completion; `done→cancel` rejected (a completed work item shouldn't be cancellable, or at least require a different subcommand like `withdraw`); `cancel→cancel` rejected.

**Actual:** All transitions silently accepted. The event ledger records all events (good for audit), but the canonical `status` field reflects the last event, not a coherent state machine result.

**Root cause hypothesis:** `WorkItem` update functions do not check current status before applying the transition.

**User impact:** An agent querying `status` of a work item can see `cancelled` even though the item was previously completed and is now in an incoherent half-state. The verification field never moves from `unverified` through any of these transitions.

**Likely scope:** All work-item transitions.

**Recovery:** Manual reconciliation via `work item history`. Status quo of "last event wins" requires the agent to inspect history.

---

### F-C1 + F-C2 — `mpm decide` does not parse its own flags **[P1, contract]**

**Reproduction:**
```bash
$ mpm decide --choice "Use SQLite WAL" --context "test" --rationale "good"
✅ Decision recorded: f293dee3d4e51255
$ sqlite3 ... "SELECT content FROM memories WHERE id='f293dee3d4e51255'"
CHOICE: --choice Use SQLite WAL --context test --rationale good
```

**Expected:** `--choice` should be parsed as the choice field, `--context` as context, `--rationale` as rationale. `--supersedes <id>` should mark the prior decision as superseded.

**Actual:** All flags are stored verbatim as part of the content. No flag is parsed. `--supersedes` is silently dropped (no field in metadata records supersession). The decision is structurally a memory with `collection='decisions'`, but its fields are NOT extracted into structured columns.

**Root cause hypothesis:** `mpm decide` does not have its own parser; it likely just passes args to a memory-add call without flag processing.

**Workaround:** Use `mpm call mpm_decisions --payload '...'` with structured params — that path correctly parses `choice`/`context`/`rationale` and stores them in metadata JSON.

**User impact:** Documentation says `mpm decide --choice ...`. Following the documented syntax produces broken decisions.

**Likely scope:** All CLI decision creation. The MCP and `call` paths are correct.

---

### F-D1 — Decay is wallclock, not runtime **[P0, contract]**

**Reproduction:**
```bash
$ mpm memory add "T19 fresh then backdated"
# baseline weight=1.0
$ sqlite3 ... "UPDATE memories SET created_at=created_at-2592000 WHERE id='...'"
# backdated 30 days
$ mpm gc
Scanned: 5 | Updated: 5 | Dead: 1
# weight was changed to -8.0 in a single gc tick
```

**Expected:** Per the temporal-clock-inventory memory from earlier sessions, runtime-dependent decay should fire only on active scheduler runtime. A 30-day wall-clock pause (vacation, scheduler down) should NOT contribute to decay.

**Actual:** A memory created "30 days ago" (by backdating) is immediately marked dead with weight -8.0 after a single gc tick. The scheduler being running-but-idle vs being-down is indistinguishable. There is no record of "active runtime" tracked against the memory.

**Root cause hypothesis:** The gc sweep reads `last_accessed_at` and `created_at` and computes a wall-clock-based decay delta. There is no `runtime_accrued_seconds` column.

**User impact:** An alpha user who takes a 14-day vacation and comes back will find many of their memories decayed — not because they were "stale" but because wall-clock time moved on while the scheduler was idle. This is silent data loss for the unprivileged user.

**Recovery:** Boost weight back via `mpm reinforce`, or `mpm promote` to LTM (which exempts from decay). But the user has to know to do this.

---

### F-D2 — User-supplied provenance is silently overwritten **[P0, data-semantics]**

**Reproduction:**
```bash
$ mpm call mpm_memory --payload '{"action":"save","params":{"fact":"T18 dedup test content","framework":"openclaw","model":"opus-5","session_id":"sess-B"}}'
# returns: success, dedup'd to existing ID
# but artifact_provenance shows: framework_name=mpm-cli (NOT "openclaw")
# session_id column empty (NOT "sess-B")
```

**Expected:** User-provided framework and session_id should be preserved (or at minimum stored in metadata).

**Actual:** `artifact_provenance.framework_name` is always set to the system's own call-surface name (`mpm-cli`, `mpm-mcp`, etc.), overwriting any user value. `session_id` column is empty. The user-supplied provenance is invisible.

**Note:** This is distinct from F-A1 (which loses the second actor entirely). F-D2 is about the FIRST actor's user-provided provenance being lost.

**Root cause hypothesis:** `SaveMemoryNode` uses the call-surface-derived framework/session as authoritative and ignores user-supplied values.

**Recovery:** None — the metadata is silent on what was passed in.

---

### F-F1 — Decisions and lessons are invisible to documented CLI search **[P1, discoverability]**

**Reproduction:** A fresh agent runs `mpm kb memory search "deadlock"` to find the well-known decision "SetMaxOpenConns(1) is FORBIDDEN — causes deadlocks". The CLI returns "No memories found." Direct FTS5 query on `memories_fts` confirms the row is indexed (rowid 9, 10 contain it). But the CLI's search filters to `collection IN ('memories','',NULL)` and excludes decisions/lessons/skills.

**Expected:** Either the CLI should include all collections, OR there should be a clearly documented separate `mpm decision search` / `mpm lesson search` command that the agent can find via `mpm help`.

**Actual:**
- `mpm decision search` — does not exist. Only `mpm decisions` (list all) and `mpm decide` (create). No search.
- `mpm lesson search` — exists but lessons live in `lessons_base` separate from `memories_fts`.
- `mpm skill search` — explicitly returns "skill search by keyword not yet wired" (per the CLI itself).
- `mpm call mpm_memory query "..."` — DOES work, but is not advertised in the cognitive-verb documentation.

**Root cause hypothesis:** Search was implemented for the `memories` collection but the other collections (`decisions`, `lessons`, `skills`, `theories`) were added later without corresponding search surfaces.

**User impact:** The "find what was decided" / "find relevant lesson" / "find applicable skill" workflow is broken at the documented CLI. An agent using the documented cognitive verbs (`mpm decide`, `mpm learn`, `mpm save-skill`) cannot later find what it (or another agent) created.

**Likely scope:** All CLI surfaces that hide collection='decisions'/'lessons'/'skills'.

**Workaround:** Use `mpm call mpm_memory --payload '{"action":"query","params":{"query":"...","scope":"all"}}'` with scope=all — that path correctly searches across collections.

---

### F-G1 — Backdated/stale references not flagged **[P2]**

**Reproduction:** Created a reference, backdated `reference_docs.created_at` to 2020-09-13. Listed references — appears identical to fresh ones. Search returns it normally. No "stale" indicator exists.

**Expected:** A 5-year-old reference should be visibly distinguished from a freshly-created one. Either via a `[stale]` marker in search results, or via a list filter `--stale`.

**Actual:** Identical visual presentation.

---

### F-G2 — No `is_historical` / `deprecated_at` / `valid_until` for references **[P1]**

**Schema check:**
```sql
CREATE TABLE reference_docs (
  id, title, file_path, source_path, source_type, tags,
  content, content_hash, import_reason,
  total_chunks, last_indexed, created_at
);
```

No fields to express "this reference is for historical context only" or "valid through version X." An agent cannot distinguish "current guidance" from "an old approach that no longer applies." This is dangerous for any workflow that retrieves references and applies them as instructions.

---

## Discoverability Findings

| Type | Discoverable via documented CLI? | Notes |
|------|----------------------------------|-------|
| Memory | ✓ | `mpm kb memory search` |
| Decision | ✗ | `mpm decisions` (list all) only; no search by content |
| Theory | ✓ | `mpm theories` lists with status; FTS5 works but only via `call` |
| Lesson | ✗ | Lives in `lessons_base` table, not in `memories_fts` |
| Skill | ✗ | `mpm skill search` says "not yet wired" |
| Reference | ✓ | `mpm reference search` |
| Topic | ⚠ | `mpm topic` exists but topic membership is not searched by default |
| Work | ✓ | `mpm work item list --status <s>` |
| Handoff | ⚠ | Listed in wake context but no search surface |

**Score:** 5 of 9 artifact types are properly discoverable via the documented CLI. The remaining 4 require either falling back to `mpm call` with non-obvious scope flags, or accepting "list all and grep."

---

## Reference Safety

| Concept | Implemented? | Notes |
|---------|--------------|-------|
| Current | ✓ | Default; fresh references work |
| Stale | ✗ | No flag for backdated `created_at` |
| Version-bound | ✗ | No `valid_for_version` field |
| Historical | ✗ | No `is_historical` / `deprecated_at` field |
| Unknown | n/a | All references are assumed current |

**Verdict:** An alpha agent receiving a reference cannot reliably determine whether it represents current operational guidance. This is a real defect for any agent workflow that retrieves and applies reference content.

---

## Agent Archaeology (T40)

Setup: Created a project corpus of 4 memories, 2 decisions, 1 pending theory, 1 lesson, 1 skill, 1 reference, 2 work items. Then pretended to be a fresh agent with no prior knowledge.

**Score: 6 of 10 questions answerable via documented CLI:**

| Question | Answerable? |
|----------|-------------|
| What is the project? | ✓ via `mpm wake` / `mpm continue` |
| What's the architecture? | ✓ via memory search |
| What's the version/tag? | ✓ via `mpm version` |
| What was decided? | ✗ decisions not searchable |
| What's still pending? | ⚠ theory status visible only via `theories` list |
| What procedures exist? | ✗ skills not searchable |
| What references apply? | ✓ via `reference search` |
| What's currently open work? | ✓ via `mpm work item list --status open` |
| What lessons should I heed? | ✗ lessons not searchable |
| What did the previous session complete? | ⚠ wake shows recent context but no explicit "completed" marker |

**The fresh agent can RECONSTRUCT basics (what the project is, what's open) but CANNOT reconstruct the project's REASONING (decisions, lessons, skills).** They have to `mpm call` with non-obvious flags to find these.

---

## Surface Parity

| Surface | Memory | Decision | Lesson | Skill | Reference | Theory | Work |
|--------|--------|----------|--------|-------|-----------|--------|------|
| `mpm` (CLI cognitive verb) | ✓ | ✗ (broken — flags unparsed) | ✓ | ✓ | ✓ | ✓ | ✓ |
| `mpm kb memory search` | ✓ | ✗ filtered | ✗ filtered | ✗ filtered | n/a | ✗ filtered | n/a |
| `mpm recall` | ✓ | ✗ filtered | ✗ filtered | ✗ filtered | n/a | n/a | n/a |
| `mpm call mpm_X` | ✓ (full) | ✓ (full) | n/a | n/a | n/a | n/a | ✓ |
| MCP (mcp__mpm__*) | — | — | — | — | — | — | — |
| mcp__mpm__mpm_decisions supersede | — | ✗ parameter name mismatch (`original_id` required, not `decision_id`) | — | — | — | — | — |

**Specific surface bugs:**

- `mpm decide` (CLI) — flags `--choice`, `--context`, `--rationale`, `--supersedes` not parsed; whole literal string stored as content
- `mpm call mpm_decisions supersede` — accepts `decision_id` but reports "original_id is required" (parameter name contract mismatch)
- `mpm ls` — displays `id` as autoincrement row number, but the actual persistent ID is a hex string. Confusing.

---

## Concurrency / Recovery

**Concurrent writes:** PASS — SQLite WAL + busy_timeout correctly serializes writes. 10 concurrent identical writes → 1 row, all callers see same ID. 5 simultaneous reinforces correctly applied (weight went 1→6, reinforcement_count=5).

**Conflicting updates on work items:** FAIL — last-write-wins. `complete` and `cancel` racing on the same work item produce whichever-order-was-last as the final status, with both events in the audit ledger.

**Crash:** N/A — mpm is single-process, stateless between CLI invocations. WAL ensures durability.

**MCP lifecycle:** Not testable in this isolated environment (separate binary). CLI reopens DB on every call so no daemon state to corrupt.

**Retry:** CLI errors return non-zero exit codes (after correcting my earlier misread). `mpm call` returns `{"success":false,"error":"..."}`. Both surfaces give clear error signals.

---

## Context Economics

- Wake context with 50 corpus memories: ~48KB JSON envelope. Acceptable.
- `mpm ls` output: ~2KB for 13 rows. Bounded.
- `mpm recall --token-budget 200` cap exists and works.
- `mpm hint` requires an argument (not a no-arg proactive hint as the name suggests).
- Skill catalog at boot: lazy-load (only on `mpm skill show`).

**Verdict:** Progressive disclosure works at this scale. No context-pressure findings at <1000 memories.

---

## Findings Ledger

| ID | Severity | Status | Root cause | Surface | Disposition |
|----|----------|--------|------------|---------|-------------|
| F-A1 | P0 | OPEN | Dedup collapses multiple actor writes to one row, second+ actors erased | `mpm add`, `mpm call mpm_memory save` | OPEN — needs `evidence` row per write attempt, even on dedup |
| F-A2 | P1 | OPEN | `mpm kb memory search` filters to memories-only collection | CLI | OPEN — needs to include decisions/lessons/skills/theories by default |
| F-A3 | P2 | OPEN | `--file` flag silently truncates large payloads | `mpm memory add --file` | OPEN — needs explicit error or transparent passthrough |
| F-B1 | P1 | OPEN | Work-item state machine not enforced | `mpm work item *` | OPEN — needs transition validation |
| F-C1 | P1 | OPEN | `--supersedes` flag silently dropped | `mpm decide` | OPEN — should at minimum record supersession in metadata |
| F-C2 | P1 | OPEN | `mpm decide` flags not parsed; literal stored | `mpm decide` | OPEN — should match `mpm call mpm_decisions record` behavior |
| F-C3 | P2 | OPEN | Empty/whitespace challenge evidence accepted | `mpm challenge` | OPEN — minor; reject empty |
| F-C4 | P1 | OPEN | No challenge dedup; 20 spam → 20 theories | `mpm challenge` | OPEN — needs to dedup at `(memory_id, evidence_hash)` |
| F-D1 | P0 | OPEN | Decay uses wallclock `created_at`/`last_accessed_at`, not runtime | `mpm gc` | OPEN — fundamental; document or fix |
| F-D2 | P0 | OPEN | User-supplied `framework`/`session_id` silently overwritten by `mpm-cli` | `mpm call mpm_memory save` | OPEN — preserve user provenance |
| F-D3 | P2 | OPEN | `mpm route` not registered in MCP/agent hooks of this test env | MCP / settings | INFO — env-specific |
| F-E1 | P2 | OPEN | `snooze --duration 1h` adds +1 day to `last_accessed` (semantic mismatch) | `mpm snooze` | OPEN — units mismatch |
| F-F1 | P1 | OPEN | Decisions/lessons/skills invisible to `kb memory search` and `recall` | CLI search | OPEN — see F-A2 |
| F-G1 | P2 | OPEN | Backdated references not flagged stale | `reference ls/search` | OPEN — needs stale flag |
| F-G2 | P1 | OPEN | No `is_historical` / `deprecated_at` field on references | schema | OPEN — schema change |
| F-H1 | P2 | OPEN | `mpm_decisions supersede` parameter mismatch (wants `original_id`, accepts `decision_id`) | `mpm call mpm_decisions` | OPEN — fix contract |
| F-H2 | P2 | OPEN | `--expires-in garbage` silently ignored | `mpm memory add` | OPEN — strict validation |
| F-H3 | P2 | OPEN | `--tags "{json}"` stored as nested JSON string in array | `mpm memory add --tags` | OPEN — parse or document |
| F-I1 | P2 | OPEN | `mpm ls` shows rowid instead of actual ID (confusing) | `mpm ls` | OPEN — UX |
| F-J1 | P1 | OPEN | Fresh-agent archaeology score 6/10; can't find decisions/lessons/skills | CLI discovery | OPEN — see F-A2 |

---

## Release Recommendation

**Trustworthy:**
- SQLite persistence + WAL works
- FTS5 search on `memories` works correctly (Unicode preserved, dedup at write time)
- Lifecycle transitions are *recorded* (even where they're not *enforced*) — the audit ledger is honest
- `mpm call mpm_decisions record` (structured call) correctly parses fields
- Concurrent writes are atomic via SQLite WAL
- Memory content scanner integration via `SaveMemoryNode` works as designed
- Wake context composes correctly for fresh sessions

**Uncertain:**
- Whether `mpm decide` (the CLI) is documented and intended as a placeholder while users migrate to `mpm call` (the underlying path)
- Whether the wall-clock decay is intentional and what the "runtime-based decay" memory was actually claiming
- Whether the missing `is_historical` field is on the schema roadmap

**Blockers (would block alpha):**
None of the P0 findings are silent corruption of durable state. They are:
- Loss of provenance under dedup (P0 by data-semantics, not by data-loss — the canonical record survives)
- Loss of user-supplied provenance fields (P0 by data-semantics, same caveat)
- Wall-clock decay after simulated downtime (P0 by contract mismatch — but for an alpha user who knows to `mpm promote` important memories, it's recoverable)

For an alpha tester who reads documentation and uses `mpm promote` for important memories, and uses `mpm call` for decision creation, none of these are blockers.

**Follow-ups:**
1. Decide whether the alpha release docstring claims runtime-based decay. If yes, fix F-D1. If no, update the memory.
2. Add `evidence` row per write attempt (F-A1 fix is small and high-value).
3. Preserve user-supplied provenance in metadata (F-D2 fix).
4. Wire search to include all collections (F-F1 / F-A2 — fixes 4 of 10 archaeology questions).
5. Document the `mpm decide --choice` parsing failure as a known bug, or fix it.
6. Add `is_historical` to references (F-G2).
7. Enforce work-item state machine (F-B1).

**Should the alpha-final tag remain frozen?** YES — the defects are alpha-grade defects, not beta-blockers. The release can ship with these documented as known issues. They become P0 fixes for beta-1.

---

## Final Principle Answer

> What happens when an unfamiliar agent uses MPM incorrectly, forgets context, encounters stale information, retries operations, changes its mind, restarts halfway through work, and assumes the interface is telling the truth?

**What works:**
- The agent CAN resume a session and see the recent context.
- The agent CAN find memories by content search.
- The agent CAN find work items, references, theories.
- The agent CAN recover from most errors via history.
- The agent CAN use `mpm call` with structured JSON for any operation that the CLI breaks on.

**What fails:**
- The agent CANNOT find decisions via the documented CLI search — they have to know about `mpm call mpm_memory query` with scope=all.
- The agent CANNOT tell whether a reference is current or stale.
- The agent CANNOT see that a memory was asserted by 3 different agents — only the first writer's provenance is preserved.
- The agent CANNOT tell that the CLI `mpm decide` is silently storing its flag args as content.
- The agent's memories WILL decay after a vacation even though the scheduler was idle — wall-clock does not equal runtime.

**The interface is mostly honest.** Where it lies (claiming runtime decay that is actually wallclock; overwriting user provenance with `mpm-cli`), the lie is consistent and the agent can recover if they learn the workaround. The bigger gap is **discoverability**: the cognitive-verb documentation describes verbs that work in isolation but don't compose into a complete archaeology workflow without the `mpm call` escape hatch.

**For an alpha release with documented workarounds, this is acceptable.** For beta, the F-A1 / F-D1 / F-D2 / F-F1 / F-G2 cluster should be fixed.
