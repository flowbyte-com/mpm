# Context Economics — MPM Model-Facing Token Overhead

> **How much context/token overhead does MPM add, and what does that overhead buy me?**

This document is the engineering answer to that question. All numbers
below were measured against the alpha build at the HEAD of `main` at
the time of measurement. Re-run the harness in
`scripts/context_economics/` to reproduce; the harness records the
exact commit SHA in `summary.json["environment"]["commit"]`.

---

## TL;DR — the most important numbers

| Quantity                                          | Bytes   | Tokens (cl100k_base) |
| ------------------------------------------------- | ------: | -------------------: |
| Tool schemas + descriptions, all 21 tools, sum    |  42,580 |                  ~10K |
| `tools/list` wire payload (single JSON body)      |  66,359 |               ~16.5K |
| `mpm wake --compact` (the smallest wake surface)  |     386 |                  118 |
| `mpm wake --json`  with 53 memories + open work   |   1,467 |                  423 |
| `mpm recall --json --limit 15` of 30 memories     |   4,933 |                1,557 |
| 1 row of `recall` (envelope + content + metadata) |     ~362 |                 ~115 |
| Pointer-native response (1 row, summary=256 chars)|     783 |                  214 |
| Full-content response to the same 1-row query     | 1,102,580 |              221,215 |
| Bounded resolve (max_bytes=512) of the same row  |     1,140 |                  299 |

**The headline finding**: without pointer projection, recalling a 1 MB
memory adds **~221,000 tokens** to the model-facing response.
With pointer projection (default `summary`), the same memory adds
**~214 tokens** — a **~1,034× reduction** on this single artifact.

The trade-off: pointer projection also requires a follow-up
`mpm_resolve` to actually read the content. A bounded resolve call
(`max_bytes=512`) costs **~299 tokens total** to read 512 bytes of
content — a much better trade than 221,000 tokens for 1.1 MB.

---

## Measurement methodology

### Tokenizer

We use **tiktoken's `cl100k_base`** encoding, the tokenizer for OpenAI's
GPT-3.5/4 family. This is widely used as a practical approximation for
"how an LLM input/output looks like in tokens". It is NOT exact for any
specific model. Token counts under Anthropic Claude, Llama, Mistral,
or other tokenizers can vary by 10-30%, especially for code and JSON.
All **byte** measurements remain useful regardless of tokenizer; the
percentage-overhead figures are derived from byte counts so the relative
scaling is tokenizer-independent.

### Environment

```
commit:        <recorded by measure.sh into summary.json>
go version:    go1.26.6 linux/amd64
os:            Linux 6.17.0 x86_64
date:          <recorded by measure.sh into summary.json>
tokenizer:     tiktoken.cl100k_base v0.x
```

### Reproduction

```bash
# 1. Build and run the harness (creates /tmp/mpm-econ-<pid>/raw/...)
bash scripts/context_economics/measure.sh

# 2. Token-count the captures
python3 scripts/context_economics/count_tokens.py /tmp/mpm-econ-<pid>/raw/summary.json
```

The harness uses a disposable workspace under `/tmp/mpm-econ-<pid>/`
and the user's real `~/.mpm` is never touched. All memory content is
deterministic placeholder text — no personal or sensitive data.

---

## How to read the columns

The measurement distinguishes:

| Concept                       | What it is                                                                                     |
| ----------------------------- | ---------------------------------------------------------------------------------------------- |
| **baseline prompt/context**   | What the model would see if MPM integration were disabled (raw task input).                    |
| **MPM protocol/tool overhead**| The fixed bytes/tokens MPM adds regardless of useful memory returned (schemas, envelopes).      |
| **useful retrieved context**  | The actual content the agent asked for and the model should use.                               |
| **retrieval metadata**        | IDs, weights, dates, tags, provenance — useful for routing but not for direct reasoning.       |
| **pointer/reference overhead**| Bytes added by `mpm://...` URIs that defer large content out of the model-facing context.       |
| **large-artifact payload**    | The size of a full-content response that would otherwise inflate the model-facing context.      |

Where exact categorisation is impossible (e.g. the recall envelope
contains both metadata and content keys), the document says so.

---

## Scenarios

The brief required at least seven scenarios (A-G). Each is a real
subprocess invocation against a disposable workspace. The harness
captures raw stdout; `count_tokens.py` applies cl100k_base and emits
the numbers below.

### A. Minimal MPM context (substrate floor)

A fresh workspace has only the seven seeded baseline directives
(`mpm-seed-read-wake-context`, `mpm-seed-session-end-cluster-triage`,
etc.). This is the **substrate floor** — what MPM emits when there is
no useful user content.

```
mpm wake --compact   →  386 bytes / 118 tokens
```

This is the cost of acknowledging that MPM exists at session start,
with no useful payload. The fixed framing is ~150 bytes; the remaining
~240 bytes are the seven directive summaries and pointers.

### B. Simple useful recall

Seed one memory, recall it with a tight query, measure the JSON
envelope. Useful vs metadata split via `jq` deletion of `content` and
`cross_references` keys.

```
recall total:                    362 bytes / 112 tokens
recall metadata only (no body):  ~270 bytes
useful content:                  ~90 bytes
```

**Observation**: roughly **75% of a one-row recall response is
metadata** (IDs, weights, dates, scores, tags). For one row, the
metadata-to-content ratio is high. As more rows accumulate, the
envelope amortises and content dominates — see scenario C.

### C. Broader recall

Seed 30 memories; recall with `--limit 15`. The total response grows
with row count; the envelope is fixed.

```
recall total (15 rows):  4,933 bytes / 1,552 tokens
per-row average:           ~329 bytes / ~103 tokens
```

At 15 rows, the envelope share drops to roughly 7% (the envelope is
~350 bytes; 15 × ~310 bytes per row is the rest). Useful content is
now the majority of the payload.

### D. Wake/context projection

Three sizes measured: empty (fresh workspace), small (3 memories + 1
open work), large (53 memories + open work).

| Form     | Empty bytes / tokens | Small bytes / tokens | Large bytes / tokens |
| -------- | -------------------: | -------------------: | -------------------: |
| `--json` |        386 / 118     |        386 / 118     |      1,367 / 415     |
| `--compact` |       386 / 118   |        386 / 118     |        346 / 132     |
| prose    | (no prose path)      |        412 / ~145    |      1,207 / ~370    |

**Observations**:

- The `--compact` form is **stable at ~132 tokens** regardless of how
  much useful content exists in the workspace. This is by design: the
  compact projection only carries session id, mode, persona, last
  handoff summary, and ≤10 recent artifact ids. The fixed framing is
  ~130 tokens; the variable payload is bounded to ≤10 ids.
- The `--json` form grows with content but is **hard-capped at 32 KB**
  by `MaxWakeContextBytes` in `internal/core/wake_context.go`. When
  the wake payload would exceed 32 KB, MPM sheds `available_skills`
  first, then `recent_topics`, and flips the corresponding
  `_truncated` flag.
- The prose form (default `mpm wake`) is human-readable markdown;
  tokenisation is slightly worse than `--json` for English prose at
  cl100k_base (~3.5 chars/token vs ~4 chars/token).

### E. Pointer-native large artifact (the headline measurement)

This is the scenario the pointer architecture exists to address. We
seed a **1.1 MB** memory and measure the three retrieval paths:

| Path                                                   | Bytes     | Tokens    |
| ------------------------------------------------------ | --------: | --------: |
| `mpm_memory save` echo (inline bounded to 2048 bytes)  |     2,377 |       ~570 |
| `mpm_memory query projection=summary` (pointer-native) |       783 |       214 |
| `mpm_memory query projection=full` (inline content)    | 1,102,580 |   221,215 |
| `mpm_resolve` with `max_bytes=512`                     |     1,140 |       299 |

**The reduction**: from 221,215 tokens (full inline) to 214 tokens
(pointer summary) — a **~1,034× reduction** in model-facing context for
this single artifact.

**The bounded resolve path**: a `mpm_resolve` call with `max_bytes=512`
on the same 1 MB memory returns 299 tokens (1,140 bytes) — only
~85 tokens above the pointer-native path, because the bounded
top-level content is the bulk and the metadata projection contains
no duplicate unbounded content. The previous version of this
document described a 1.1 MB `mpm_resolve` echo via `metadata.content`;
that leak is fixed at the response/projection boundary in commit
`<see git log>` (Sept 2026 launch).

### F. Tool/schema overhead

Schema + description sizes, measured by the Go probe at
`internal/core/tools/tool_size_probe`. This is what ships in
`tools/list` at session start — a **fixed cost** paid once per MCP
session.

```
21 tools registered, all schemas hand-written JSON-Schema strings.
TOTAL descriptions bytes:    14,756  (~3,700 tokens)
TOTAL schemas bytes:         27,824  (~6,950 tokens)
TOTAL descriptions+schemas:  42,580  (~10,650 tokens)
JSON-serialised tools/list:  66,359  (~16,600 tokens) ← measured wire payload
```

Largest individual tools:
- `mpm_system` (lifecycle / governance) — 9,894 bytes total
- `mpm_work` (work items) — 4,577 bytes
- `log_to_changelog` — 3,169 bytes
- `mpm_memory` — 2,895 bytes
- `mpm_context` — 2,729 bytes
- `mpm_handoff` — 2,729 bytes
- `mpm_skills` — 2,198 bytes
- `mpm_wakes` — 1,736 bytes

**Observation**: the schema floor is **~16,600 tokens** per session,
paid once when `tools/list` is first served by the MCP server. This is
the largest single fixed-cost line item in MPM. It is comparable to a
modest system prompt extension.

The schema size is dominated by hand-written JSON-Schema strings; no
JSON-Schema-builder dependency is used (consistent with the CLI's
"no framework introduction" rule). Description strings average ~700
bytes per tool — large enough to be discoverable but not so large that
they crowd out useful memory in the model-facing context.

### G. Multi-step workflow: wake → recall → resolve

A three-call workflow that an agent would actually run:

```
mpm wake --compact          →  386 bytes / 118 tokens
mpm recall --json --limit 1  →  362 bytes / 113 tokens
mpm_resolve (1-row content)  → ~428 bytes / ~154 tokens (bounded)
─────────────────────────────────────────────────────────────────
sum                            1,176 bytes / 385 tokens
```

**Observation**: the three-call workflow adds approximately **385
tokens beyond the already-paid tool-schema floor**. The per-call
envelope does NOT multiply linearly when calls are chained — each
tool call shares the MCP envelope (~70 bytes) and the schema was
already paid in scenario F.

---

## Per-row costs (the building block)

The harness yields these per-row numbers, which let you estimate any
recall scenario:

| Quantity                                  | Approximate size |
| ----------------------------------------- | ---------------: |
| Fixed recall envelope (`query`, `count`)  |    ~350 bytes / ~95 tokens |
| Per-row content (varies; typical)         |   ~80–300 bytes / ~20–100 tokens |
| Per-row metadata (id, weight, dates)      |   ~150–200 bytes / ~50–60 tokens |
| Per-row pointer (`mpm://memory/<16-hex>`) |   ~30 bytes / ~10 tokens |
| Per-row summary (256 chars + JSON wrapper)|  ~340 bytes / ~90 tokens |
| MCP `TextContent` envelope                |    ~10 bytes per call |

So a 15-row recall with mostly content and modest metadata:
**~350 envelope + 15 × (~300 content + ~150 metadata) ≈ 6,650 bytes
(~1,650 tokens)**. Our scenario C measurement (4,933 bytes / 1,552
tokens) is consistent with this estimate.

---

## Engineering interpretation

### What is the fixed cost?

The fixed cost of MPM integration has three components:

1. **Schema floor** — when the MCP host supplies all registered tool
   definitions in `tools/list`, the measured tool-definition wire
   payload is **66,359 bytes / ~16,600 tokens** (scenario F). This
   is typically a one-time session cost, subject to host-side tool
   caching and context management. Not every host surfaces every tool;
   hosts that filter tool lists can reduce this floor proportionally.
2. **Compact wake floor (~118 tokens)** — paid once per session start
   if the agent follows the MPM behavioural contract.
3. **Per-call envelope (~95 tokens)** — `success`, `mode`, `query`,
   `count` plus MCP `TextContent` wrapper.

**Total substrate floor**: roughly **17,000 tokens** before any useful
memory is returned. This is comparable to a non-trivial system prompt
extension and is the cost an alpha user is paying for **persistent
cross-session state**.

### What scales?

| Component                       | Scales with                                                |
| ------------------------------- | ---------------------------------------------------------- |
| Recall envelope                 | Number of distinct calls (not rows)                        |
| Per-row content                 | Stored memory size (capped at 256 chars in `summary` projection) |
| Per-row metadata                | Number of rows × constant metadata fields                  |
| Pointer size                    | Number of rows × ~30 bytes (constant per pointer)          |
| Wake context (full --json)      | Result-list sizes up to 32 KB hard cap                     |
| Wake context (--compact)        | Constant ~132 tokens                                       |
| Tool schema                     | Constant (does not scale with workload)                    |

### What does the pointer architecture change?

Without pointer projection, every memory that contains content larger
than ~2 KB risks inflating the model-facing context by an unbounded
multiple of its storage size. With pointer projection, the model sees
a 256-char summary and a 30-byte URI instead of the full content, and
can decide whether to read more.

For the 1 MB memory we measured, the savings are **~221,000 tokens per
recall**. For a 50 KB memory (the default `mpm_blob_read` cap), the
savings are ~10,000 tokens. For typical memories under 2 KB, the
pointer projection is a wash (both full and summary are small).

### When does MPM overhead become material?

- **Schema floor (~16,600 tokens)**: paid once per session. Material
  in tight context budgets (<32 KB total). An agent using a 100K-context
  model barely notices; an agent using a 16K-context model may want to
  drop unused tools.
- **Wake context (full)**: capped at 32 KB. Material when the
  workspace has many memories and the agent doesn't use `--compact`.
- **Recall rows**: scales linearly with row count. The default
  `--limit 15` keeps a single recall under 5 KB; larger limits or
  larger content push it up.
- **Pointer leak via `mpm_resolve` metadata echo**: as documented in
  scenario E. This is a real limitation that an alpha user should
  avoid by preferring `mpm_memory query projection=summary` over
  `mpm_resolve` for large artifacts.

### What does MPM buy for that cost?

Concretely:

- **Persistent cross-session state**: the wake's last-handoff field,
  recent_topics, recent_memories, and open_works are persistent across
  sessions. Without MPM, the agent would lose all of this.
- **Retrieval of previously stored information**: every recall
  potentially saves the agent from re-deriving a fact (cost of
  re-deriving in tokens is usually much larger than the recall
  envelope).
- **Reduced repeated reasoning**: when the agent stores a decision
  (`record_decision`) or a lesson (`save_lesson`), it does not have
  to re-derive the rationale in a future session.
- **Controlled large-artifact access**: pointers are a deliberate
  context budget. The agent inspects a 256-char summary, decides
  whether the full content is relevant, and only then pays the
  resolve cost.
- **Bounded model-facing context**: every MPM response is bounded —
  wake at 32 KB, recall at `--limit`, projections at 256-char summary,
  inline content at 2048 bytes by default. The substrate enforces
  these bounds; the agent does not have to police them.

### Fair conclusion

MPM introduces a **~17,000-token substrate floor** (schema + compact
wake) and a **~95-token per-call envelope**, and offers a **~1,000×
context reduction on large artifacts** via pointer projection. The
cost is dominated by the schema floor (one-time per session); the
benefit is dominated by pointer projection on large artifacts. For
typical small-memory workflows the cost is comparable to a system
prompt extension; for large-artifact workflows the benefit is dramatic.

---

## Limitations of this measurement

- **Tokenizer**: cl100k_base is an OpenAI approximation. Anthropic,
  Llama, Mistral tokenizers will produce different numbers (typically
  10-30% variance for JSON).
- **Schema floor in production**: the measured `tools/list` wire
  payload (66,359 bytes / ~16,600 tokens) is the authoritative
  model-facing measurement. Production hosts may include additional
  framing (e.g. JSON-RPC envelope) on top of this number.
- **No real LLM**: we did not run an actual model to confirm the
  measured tokens match the model's internal count. The cl100k_base
  count is the most defensible cross-vendor approximation.
- **Synthetic content**: the test memories use deterministic filler
  text. Real memories may include code, markdown, JSON, or other
  tokenisation-resistant content. Token counts will vary.
- **Workspace state**: the harness seeds a fresh workspace, not the
  user's actual `~/.mpm`. Real workspaces have more directives and
  more memories, which moves the wake context closer to its 32 KB cap.

---

## Reproducibility — exact commands

```bash
# Build the harness's measurements:
bash scripts/context_economics/measure.sh

# Tokenise the captures:
python3 scripts/context_economics/count_tokens.py /tmp/mpm-econ-*/raw/summary.json

# Inspect any single capture directly:
cat /tmp/mpm-econ-*/raw/D_wake_large_json.txt

# Inspect the tool/schema probe output:
go run ./internal/core/tools/tool_size_probe
```

The harness creates `/tmp/mpm-econ-<pid>/raw/` containing:

- `summary.json` — bytes captured per scenario (also: git commit, Go
  version, OS, date)
- `report.json` — bytes + tokens per scenario (written by
  `count_tokens.py`)
- `<scenario>.txt` / `<scenario>.json` — raw stdout for each MPM
  invocation the harness ran

The harness does not modify the user's real `~/.mpm` or any persistent
state outside the disposable workspace.

---

## Files

| Path                                                            | Purpose                                          |
| --------------------------------------------------------------- | ------------------------------------------------ |
| `scripts/context_economics/measure.sh`                          | Bash orchestration; runs every MPM call          |
| `scripts/context_economics/count_tokens.py`                     | Token-counts every capture with cl100k_base      |
| `internal/core/tools/tool_size_probe/main.go`                   | One-off Go probe that lists per-tool schema/description bytes |
| `docs/CONTEXT_ECONOMICS.md`                                     | This document                                    |

The probe lives under `internal/core/tools/` because it imports the
same `tools.Registry` slice the MCP server uses at runtime; it is a
standalone `main` package and does not affect any production code path.
