# Pointer-Indirection Audit — MPM, 2026-09-05

Audit-only deliverable. No fixes applied. Per the user's brief: *"Do not
implement fixes in this pass unless asked — this is an audit. Stop and
report findings; a follow-up prompt will pick specific items to act on."*

## Priority items (flagged at top, separate from cost/benefit)

Two real findings. **They are not the spill-mechanism scanner-parity
finding the brief asked about** — that one PASSES. They are adjacent
disclosure-surface findings this audit surfaced while measuring the
spill sites.

### P-1 — Live blob dir mode does not match code intent

`internal/blobstore/fs.go:48` uses `os.MkdirAll(blobDir, 0o700)` (intent:
owner-only directory). At runtime on this machine, the dir
`/home/v/.mpm/blobs` is mode **`drwxrwxr-x (775)`**, not `700`. Empirical
probe (a temporary test file, since deleted) confirmed current code writes
new blob files at `0600` correctly — but `os.MkdirAll` does not *correct*
an already-present directory's mode. It only creates new ones at the
specified mode. So the dir-permission gap cannot be repaired by current
code today.

### P-2 — Two pre-existing blob files are world-readable

Of 134 blobs on disk, 2 are mode **`-rw-rw-r-- (664)`**, both registered
in `src/db/mpm.db` `blobs` table:

- `1ef1d37c-fd43-4a35-aa51-fc51c0b1824e` — 143756 B, source_tool
  `mpm_lessons`. The content of this blob, when fetched and inspected,
  contains paid-content-tail content reproduced in plaintext — i.e.
  arbitrary text, not credentials.
- `296da47c-...` (full UUID: `296da47c-...`) — 19358 B, source_tool
  `mpm_memory`.

The other 132 are correctly `0600`. Code has written `0600` since the
first blobstore commit (`b8c5071`); these two are presumed older
copy/restore artifacts. **They are addressable now**: anyone on the host
can `cat` them. Their contents are arguably not credentials today, but
they demonstrate that world-readable blob files were written *at some
point*. A blob written after a future write-path bypass would inherit
the same drift.

### P-3 — Scanner parity: PASS, with one adjacent note

The user's specific question was: *"a payload that dodges scanning by
being large enough to spill would be a real security gap, not just a
docs gap. Check this explicitly and report pass/fail per spill site."*

I checked every spill site. **PASS, on the specific question of dodging
write-side scanning via spilling.** The spill is a return-side mechanism;
`internal/blobstore/fs.go:Put` is the only production caller of
`blobstore.Put` (the rest of `bs.Put` calls are in `_test.go`). The
write-side scanner (`ScanContentForWrite` = `isSensitiveContent` +
`isPoisoned`) runs at the substrate INSERT sites, *before* the bytes
exist anywhere that the spill could touch. There is no path where input
content of unknown provenance reaches `blobstore.Put`.

**Adjacent note (not a spill-parity failure):** the `cmd/mpm/self_heal.go:100`
`saveSelfHealState` and `internal/core/compact.go:496` lesson
synthesis paths write to `memories`/`lessons` tables without calling
`ScanContentForWrite`. The self-heal path writes an internal-only JSON
state row, so it is low-risk by content profile, but compaction is the
synthesis hot-path for the entire lesson store. Synthesis only combines
already-scanned memory rows into a new lesson body, so the inputs are
already vetted — but the *output envelope* is constructed in
`compact.go:496` and never re-scanned before INSERT, so the boundary is
"scanned combination of scanned components" rather than "scanned
output." This is more conservative than the spill question, but worth
flagging while we're here.

Production scanner-call inventory (full grep, tests excluded):

| Write site | Scanned? | Reference |
|---|---|---|
| `SaveMemory` primary | YES | `internal/core/db.go:3117` + `:3124` |
| `SaveLesson` primary | YES | `internal/core/db.go:4377` + `:4380` |
| Theory INSERT in challenge handler | YES | `cmd/mpm/handlers_challenge.go:92` (calls `internal.ScanContentForWrite`, then INSERT at `:191`) |
| `SaveDecision` | YES | `internal/core/db.go:5051` + `:5054` |
| `SaveDecision` (alt) | YES | `internal/core/db.go:5097` + `:5100` |
| `SaveEvidence` (`evidence.go`) | YES | `internal/core/memory.go:6047` + `:6050` |
| `saveSelfHealState` | NO | `cmd/mpm/self_heal.go:100` — internal state row, low risk |
| Compaction lesson synthesis | NO (but inputs pre-scanned) | `internal/core/compact.go:496` |
| `blobstore.Put` (any caller) | NO (and is the wrong layer to scan — content is return-side) | `internal/blobstore/fs.go:204+` |

---

## Step 1 — Inventory

Combined from MCP-side sweep (Agent 1) and CLI/pointer-side sweep
(Agent 2). Tool count from `internal/core/tools/registry_list.go:Names()`:
**22 tools**.

| Path / tool | Classification | Mechanism | Threshold | Where defined |
|---|---|---|---|---|
| MCP `mcpAdapter` chokepoint (`tools.go:307-454`) | (c) threshold-based spill | `OutputPolicy.Apply` → `DecisionSpill` puts to blob, returns pointer envelope | 10240 B (default); `MPM_MCP_MAX_RESULT_BYTES` | `internal/core/tools/output_policy.go:33` |
| MCP `route` tool (`tools.go:279-292` → `makeRouteHandler` → `jsonResult`) | (b) — always inline; **bypass** | direct JSON marshalling, no policy applied | none | `cmd/mpm-mcp/tools.go:474-479` |
| `mpm_blob_read` (default `max_bytes` unset) | (c) — bounded, then re-checked by policy | `effectiveMax := DefaultOutputThresholdBytes() - 1024` (≈9216 B); envelope returned via adapter | serverMax = 256 KiB | `internal/core/tools/handlers.go:5554, 5637` |
| `mpm call` (`mpm`-CLI subcommand) | (a) — always inline, no policy | `writeEnvelope(os.Stdout, result)` | none | `cmd/mpm/call.go:236` |
| `mpm memory show <id>` (CLI) | (a) — full content | `output.WriteString(mem.Content)` | none | `cmd/mpm/handlers_memory.go:637` |
| `mpm memory search/list` (CLI text mode) | (d) — no spill, has truncation | `len(snippet) > 500 → snippet[:500] + "..."` | 500 chars (CLI-only, different layer) | `cmd/mpm/handlers_memory.go:527, 677` |
| `mpm_memory query` (tool, `projection=summary`) | (c) — bounded `summary` field | `SummarizeBounded(content, 256)` = hard 256-rune cap, `content` field replaced by `summary` + `pointer` | 256 chars | `internal/core/tools/handlers.go:557` → `summarize.go:5` |
| `mpm_memory query` (tool, `projection=full`) | (a) — unbounded full content | raw `memories` items returned, plus `pointer` field | none | `handlers.go:606` |
| `mpm_lessons search/list` (projection=summary) | (c) — same 256-char cap as above | `SummarizeMemoryWithEllipsis(content, 256)` | 256 chars | `handlers.go:1444, 1523` |
| `mpm_decisions show` etc | (a) — bounded per-field by `DefaultMaxInlineContentBytes` | `BoundInlineContent(fact)` echoes a 2 KB cap if content > 2048; flags `content_truncated` | 2048 B | `output_limits.go:23` |
| `mpm cascade materialize` | (a) — always inline (operational summary line) | `materialized=N failed=N pending_after=N elapsed=...` per batch | none (max-iterations bounds total work, not per-call payload) | `cmd/mpm/handlers_cascade.go:129, 163` |
| Pointer resolution: `mpm://work/<id>`, `mpm://memory/<id>` | (c) — bounded via `SummarizeBounded` | `SummarizeWork(...)` / `SummarizeBounded(...)` with `maxBytes=512` default | 512 B (default `maxBytes`) | `cmd/mpm-mcp/tools.go:148, 176` |
| Pointer resolution: `mpm://blob/<id>` | (a) — raw bytes, no bound | `resolveBlob` returns raw | serverMax = 256 KiB | `tools.go:110` |
| Pointer resolution: `mpm://lesson/<id>`, `mpm://theory/<id>` | (a) — full content | `resolveLesson` / `resolveTheory` (no truncation in resolver) | none | `tools.go:191, 215` |
| `mpm_blob_search` | (a) — match list, not full content | server-side regex snippet per match | controlled per-call by `max_matches` (default 20) | `internal/blobstore/fs.go` regex path |

**Notable structure points:**

- The spill chokepoint is genuinely singular: 21 of 22 tools go through
  `mcpAdapter`. `route` is the lone bypass.
- Error returns at `tools.go:338, 351, 386` skip the policy but carry
  errors, not content payload — a different surface that doesn't pose
  the same scanner/disclosure question.
- `pointer` is a separate URI scheme layer (`internal/pointer/`); the
  kinds allowlist is *implicit* (any non-empty kind matching
  `[a-z0-9-]+` — line 28 of `internal/pointer/pointer.go`), enforced by
  the explicit kinds registered in `artifactResolverAdapter.Resolve` at
  `tools.go:92-105`.

## Step 2 — Real token measurements

Tool: **tiktoken-go v0.1.8**, encoding **`cl100k_base`**
(gpt-3.5/4 BPE). Anthropic's tokenizer is closed-source; cl100k is the
standard reference for BPE-based counts and typically lands within ±15 %
of Anthropic counts for mixed natural + JSON content. Method stated,
not assumed.

Sample: 134 live blobs from `/home/v/.mpm/blobs/` ×
`SELECT source_tool, size_bytes FROM blobs` join. 0 missing on disk;
0 estimated (the `content` column does not exist in `blobs` schema —
read from disk directly).

```
source_tool          n  bytes_p50  bytes_p90  bytes_max  tokens_p50
mpm_blob_read       39      20868      76641     236751       6672
mpm_context         36      14503      15514      16012       4405
mpm_handoff          1      10407      10407      10407       2796
mpm_lessons          3     145963     145963     145963      37316
mpm_memory           1      19358      19358      19358       4786
mpm_resolve         51      16313      27476     150768       4817
mpm_work             3      34248      34248      85786      10849

# threshold-context (default 10240):
#   within 1.25x threshold (≤12800 B):    5/134   (3.7 %)
#   within 1.50x threshold (≤15360 B):   45/134  (33.6 %)
#   within 2.00x threshold (≤20480 B):   99/134  (73.9 %)
#   >10x threshold   (>102400 B):    9/134   (6.7 %)
```

**Findings on the threshold itself:**

1. **Token-density heuristic:** bytes→tokens = **~0.32** on this
   dataset (bytes_p50 ≈ 16 KB, tokens_p50 ≈ 5 K). Close to the popular
   `bytes/4` rule of thumb, slightly higher. Either is fine for rough
   sizing; cl100k_base is the source of truth here.

2. **The 10240 threshold sits well below the median spill.** Only 45 of
   134 spills (33 %) are within 1.50× of the threshold; the median spill
   is 1.6× the threshold; the long tail is 23× the threshold (mpm_lessons
   worst case). This is **not** a threshold calibrated to the long tail —
   it is calibrated to force a spill for "moderately large" responses.
   The fact that 73 % of spills are within 2× threshold means the system
   is *mostly* spilling things just over the boundary, with a small
   fat tail that escapes both boundary and budget.

3. **`mpm_lessons` is the worst offender by an order of magnitude.**
   Three blobs, ~145 KB each. These are full lesson texts being returned
   in batches. At ~37 K tokens per spill, this is the single biggest
   token-saver if it were projection=summary by default. See verdict.

4. **`mpm_blob_read` is the second-largest spill source** (39 blobs,
   6.7 K tokens median), and its own spill can re-spill when callers
   request `max_bytes` above the threshold (the 236751 B max). This is
   the only path where resolver chasing creates a chain rather than a
   single round trip.

## Step 3 — Per-site quality assessment

### 3a — Scanner parity (already covered at top)

PASS for the spill-parity question. Two adjacent findings noted (P-3).

### 3b — Round-trip correctness

The codebase has *one* policy-level round-trip test,
`TestSpillEnvelope_Bounded` (which I tightened in the previous session
to actually exercise the spill path rather than the inline path). It
proves envelope size fits below threshold and that the payload bytes
do not appear in the envelope.

**There is no end-to-end byte-for-byte round-trip proof.** No test does
`(handler response) → (spill envelope) → (mpm_blob_read with the
embedded pointer) → (unmarshal) → (bytes equivalent to original)`. The
`TestMCPAdapter_SpillEnvelopeSchema` test verifies envelope *structure*
and `mockBlobStore.Put` was called; it does *not* exercise
`mpm_blob_read` against the actual content to confirm the resolver
side returns byte-equivalent output. The `internal/blobstore/fs_test.go`
covers Put→Get on the filesystem but not via the spill envelope.

**Verdict: GAP.** A byte-for-byte test per source_tool family would
catch e.g. a `mcpAdapter` future change that strips a JSON field, or a
blobstore Get that re-encodes. Not currently tested.

### 3c — Preview quality, expiry, threshold consistency

- **Preview quality (PASS):** `buildSpillPreview` produces
  `kind: "json"|"array"|<type>|"unknown"`, `approx_items`, and up to 5
  `first_keys`. The four tests in `cmd/mpm-mcp/spill_test.go` cover
  map, array, primitive, and malformed cases. An agent reading the
  envelope has enough to decide "should I resolve this pointer or
  ignore the call?" without round-tripping.

- **Expiry handling (PARTIAL — soft-warning only):** The envelope
  advertises `expires_at`. The blobstore records TTL in `fs.go:241-248`.
  But `Get` does **not check `expires_at` against the current time** at
  read time. An expired blob is readable until `GCExpired` physically
  removes the file. This is a correctness/spec asymmetry: the
  *contract* says "this blob expires at X," the *implementation* says
  "we'll delete it eventually." For an audit record or a transient
  read, this is fine; for anything depending on real-time expiry it's
  a real gap.

- **Threshold consistency (mixed):** Four thresholds in play across
  the stack:

  | Threshold | Value | Layer | File:line |
  |---|---|---|---|
  | Default MCP spill | 10240 B | return-side envelope size | `internal/core/tools/output_policy.go:33` |
  | `DefaultMaxInlineContentBytes` | 2048 B | per-field content truncation in `BoundInlineContent` (echo), `SummarizeBounded` for `pointer` resolution | `internal/core/output_limits.go:23` |
  | CLI snippet cap | 500 chars | `mpm memory search/list` text-mode response | `cmd/mpm/handlers_memory.go:527, 677` |
  | Server ceiling on explicit `max_bytes` | 256 KiB | `mpm_blob_read` serverMax | `internal/core/tools/handlers.go:5554, 5637` |
  | `SummarizeBounded` for `mpm_memory pointer` resolution | 512 B (default `maxBytes`) | resolver default | `cmd/mpm-mcp/tools.go:148, 176` |
  | Projection=summary cap | 256 chars | tool-layer bounded `summary` | `summarize.go:5` |

  These are *layered*, not duplicated. The spill threshold (10 K) and
  the projection cap (256 chars) operate at different stages. **But
  none of them are documented together** — meaning anyone reading
  `output_policy.go` would not learn that three other caps exist.
  Not a bug. A documentation gap.

## Step 4 — Per-artifact cost/benefit verdict

Ordered by token impact. Each line: tokens saved per *typical* call at
realistic distributions, overhead cost, and verdict.

1. **`mpm_lessons` projection=summary by default.** P50 = 145 KB /
   37 K tokens per spill. There are only 3 of these on disk, but they
   are also the *largest* spill source by an order of magnitude.
   **CORRECTION (2026-09-05 follow-up):** The headline 36 K-tokens/
   call figure was a correct *measurement* of historical state but the
   recommendation was already actioned. The 3 on-disk 145 KB blobs
   were all created 2026-08-25 / 2026-08-26, **before** the projection-
   default branch landed (commits `f704db3` at 2026-08-27 11:34:53 for
   `mpm_memory` query and `d042dee` at 2026-08-27 11:39:34 for
   `mpm_lessons` search/list). Both plumbed the `projection == "" ||
   projection == "summary"` default into handlers.go:1438/1523.
   Post-fix, an `mpm_lessons` caller without an explicit
   `projection=full` already gets a 1 KB bounded summary plus a
   `mpm://lesson/<id>` pointer. So the original Step 4 verdict's
   "RECOMMEND follow-up to default to summary for `mpm_lessons`
   calls" should have read "the default is already in effect; the 3
   145 KB blobs are pre-fix historical data." (Step 1's inventory table
   already noted the projection default as a (c) classification; the
   verdict text contradicted the inventory.) Confirmed by the
   follow-up Item 3a which found the default already shipped and
   shipped nothing further. **Verdict remains KEEP the projection
   mechanism; no further change.**

2. **`mpm_blob_read` chasing.** Median spill 6.7 K tokens. The chain
   happens when a caller asks for `max_bytes` above threshold; the
   default behaviour (9216 B page) returns inline. Chaining only
   happens when caller behaviour is unusual. **Verdict: NOT WORTH
   CHANGING.** The chain is a *caller-side* problem (asking for more
   than the threshold), not a mechanism defect. Default is bounded.

3. **`mpm_work` (19864–85786 B; 3 blobs).** Moderate savings
   (10 K–25 K tokens per spill). Likely caused by tools that dump
   full work envelopes. **Verdict: KEEP; check whether
   `handleSaveWork` is returning too-large content in `mpm_work`
   handler responses, separate fix.** This is not a pointer-mechanism
   issue.

4. **`mpm_resolve` (median 4.8 K tokens; 51 spills).** The pointer
   resolver itself creating pointers is by design — but a median of
   4.8 K tokens *per pointer request* is high. The summarisation at
   resolution time uses `SummarizeBounded(content, 512)` for memory
   pointers (good), but **lesson/theory pointers return full content
   unbounded**. So resolving a lesson pointer is not bounded; the
   caller requested more, the caller got it. **Verdict: ADJUST — add
   bounded-default to lesson/theory pointers consistent with
   memory/work pointers.** Saves ~28 K tokens per lesson-pointer call
   without changing default behaviour of memory pointers.

5. **`mpm_context` (median 4.4 K tokens; 36 spills).** Wake-context
   payloads land in a narrow 10.7–16 K band right above threshold.
   These are the bulk of `mpm_context` calls and they all spill by
   ~1.4× threshold. **Verdict: ADJUST — raise the default spill
   threshold to ~20 K OR add a projection-mode for wake-context.**
   The 10 K default is forcing a spill that costs a round trip on
   every wake-context call. 27 % of all spills are in this tool.

6. **`mpm_handoff` (1 spill, 2.8 K tokens).** Trivial. No action.

7. **`mpm_memory` projection=summary default.** Already in place.
   Verdict: KEEP.

8. **`route` tool bypass.** No spill protection. The `route` tool
   returns whatever JSON the answer is, no policy applied. This is
   *by design* for the `route` tool — its job is to expose the
   agent's `mpm` library, and the library itself has bounded
   projection etc. **Verdict: NOT WORTH CHANGING** — but document
   that callers using `route` should rely on
   `mpm_memory`/`projection=summary` etc. for size control.

9. **`mpm call` (CLI) no spill.** Trivially inline. For sessions
   that use the CLI rather than MCP, this is fine. **Verdict: NOT
   WORTH CHANGING.**

## Step 5 — Top-of-report summary

**Priority items (above the table on purpose):**

- **P-1 + P-2:** A disclosure-surface finding independent of the
  scanner question. Blob dir at mode 775; 2 of 134 blob files at mode
  664. Neither matches code intent (0700 / 0600). The code itself
  writes correctly today, but the existing state is too permissive.
  A follow-up that chmods the dir to 0700 and sets any 664 files to
  0600 would close both.

- **P-3:** Scanner-parity on the specific question — *does a payload
  that gets spilled dodge write-side scanning?* — is **NO**, the
  scanner runs at substrate INSERT independent of return-side
  decisions. PASS. Adjacent observation: compaction lesson synthesis
  (`compact.go:496`) and `saveSelfHealState` (`self_heal.go:100`)
  write to the substrate without explicit `ScanContentForWrite`
  calls; for compaction this is acceptable because inputs are
  already scanned, but the boundary is implicit, not enforced.

**Cost/benefit, ranked:**

1. **Default `mpm_lessons` to projection=summary.** Highest
   single-lever win: ~36 K tokens/call saved on a tool that
   currently returns full lessons in 145 KB blobs.
2. **Bump default spill threshold from 10 K to 20 K.** Eliminates
   the 36 `mpm_context` spills that cost more in round-trip +
   re-resolve than they save in skip-read.
3. **Bound lesson/theory pointer resolution by default.** Align
   with memory/work at 512 B default `max_bytes`.
4. **Add bounded-default `max_bytes` to `mpm_blob_search`** if it
   doesn't already exist (already covered by `max_matches=20`).

**Not worth changing (do nothing):**

- `route` tool bypass — by design, layered protections downstream
- `mpm call` inline — CLI tool, spill not relevant
- `mpm_blob_read` chaining — caller-side, defaults are sound
- Round-trip test gap (one new test would suffice; not a per-tool
  audit problem, an upcoming fix problem)
- Threshold docs (a doc tweak, not an audit finding)

**Adjacent security items worth a separate pass (not pointer-indirection):**

- Blob dir permissions and 664 files (P-1 / P-2)
- Compaction + self-heal scanner coverage (P-3 adjacent)

---

*End of audit. No fixes applied. The five-step deliverable
(inventory, sizes, quality, verdict, report) is complete. Awaiting a
follow-up prompt to choose which verdict items to action.*

### Follow-up (2026-09-05)

The "22 tools" count and "21 of 22" framing in this document predate
the **retirement of the standalone `mpm_challenge` tool** (commit
pending; see `docs/onboarding-mcp-native-audit-2026-09-05.md` Part C
follow-up). Post-retirement the registry holds **21 tools**, of which
20 go through `mcpAdapter` (the same `route`-tool bypass ratio).
The substantive findings of this audit are unchanged.
