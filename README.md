# MPM

> **A persistent reasoning substrate for AI agents.**

MPM stands for **Mnemonic Persistence Maintainer**. It is a *persistent reasoning substrate* — a small, opinionated binary that turns an agent's transient thinking into a durable epistemic trail.

Traditional memory systems answer one question:

> *What does the agent remember?*

MPM attempts to answer four:

* What does the agent believe?
* Why does it believe it?
* How certain is it?
* What evidence could change its mind?

Those questions lead to a different architecture.

Instead of storing conversations, MPM stores reasoning.

Instead of assigning arbitrary confidence scores, confidence is derived from evidence.

Instead of rewriting memories, beliefs evolve while history remains intact.

One persistent cognitive substrate. One SQLite database. One headless daemon. No vector database. No distributed infrastructure. No web UI.

`mpm` runs continuously in the background: a hardened SQLite data plane, an autonomous 03:00 UTC diagnostic critic, and an MCP server (`mpm-mcp`) for machine-to-machine integration. The CLI is an operational convenience for inspecting the daemon's state — not a human-facing application.

MPM models reasoning processes — belief formation, evidence weighting, theory tracking, self-correction. It does not claim to reproduce human cognition. "Cognitive" describes *what kind of object is being persisted*, not a claim about machine minds.

> **MPM is not designed to maximize recall. It is designed to preserve intellectual progress.**

---

## Reading Guide

This is one document. It is long because MPM covers ground. Read by audience:

| If you are… | Read first | Then |
|---|---|---|
| **A curious reader** evaluating MPM | §1 — §3 | §6 (skim) |
| **A new operator** wiring MPM into an agent | §1, §5 (Quick Start) | §8 (CLI), §9 (Runtime) |
| **A contributor** reading code or writing patches | §1 — §7 | Appendices A, B, C |
| **An agent author** integrating via MCP or `mpm call` | §5, §6.4 (MCP), §6.5 (Shared Epistemology) | §8 (CLI parity table) |
| **Future me** returning after months away | §3 (axioms), §10 (reliability) | Appendix C (enforcement patterns) |

Every section is self-contained enough to read in isolation.

### Table of Contents

1. What is MPM?
2. Why this isn't a memory system
3. The Cognitive Model
4. Belief Lifecycle
5. Quick Start
6. System Architecture
   - 6.1 Core vs Runtime
   - 6.2 Confidence Engine
   - 6.3 Retrieval Architecture
   - 6.4 MCP Integration
   - 6.5 Multi-Agent Shared Epistemology (Layers 0–4)
7. Core Stability
8. CLI Reference
9. Runtime Services
10. Reliability
11. Glossary
- **Appendix A: Shared Epistemology Implementation**
- **Appendix B: Arc 2 (Active Dissemination) Implementation**
- **Appendix C: Enforcement Patterns**
- License

---

## 1. What is MPM?

MPM is a single binary that provides long-term memory, behavioral modes, persona management, and an **epistemology engine** — everything stored in one SQLite database with FTS5 full-text search. Zero external services.

It is the reasoning layer for AI agents. It tracks not just *what* the agent knows, but *why* it decided to act, *how* it chose to act, and *what it believes but hasn't proven yet*.

### What MPM is not

MPM is not a workflow engine, an orchestration framework, a planning system, an agent runtime, a distributed database, or an infinitely extensible plugin framework. Those things may be built *on top of* MPM. They are not part of MPM.

This is the project's most important self-defense against feature creep. The regret log (see §7) is the empirical record of pressure to absorb adjacent problems; the non-goals are the standing answer. **The discipline must survive success** — the temptation to make MPM absorb an adjacent problem only grows as more people use it.

At the implementation level, the exclusions are concrete:

- **Not an HTTP server.** No request/response REST surface; machine integration is stdio-only via `mpm-mcp`. The CLI is for operators, not for serving web traffic.
- **Not generic storage.** Built for AI agent cognition: weighted recall, decay, epistemology, proactive hints.
- **Not a human dashboard.** Machine-to-machine interface is primary; CLI is a convenience layer.
- **Not a vector database.** A SQLite-native ANN index handles semantic recall. No Pinecone, no Qdrant, no embeddings service.
- **Not a knowledge graph.** Relationships are first-class artifacts (decisions, theories, evidence, lessons), not edges in a graph store.

---

## 2. Why this isn't a memory system

Most AI memory systems focus on retrieval. An agent stores information, retrieves information, and continues operating. The problem is that real cognition involves more than facts.

Consider a software engineering agent:

- **Fact:** WordPress strips inline style tags.
- **Decision:** Store widget CSS in `wp_options`.
- **Hypothesis:** The CLI parser fails when `--json` appears before positional arguments.
- **Evidence:** Unit tests confirm the parser bug.
- **Conclusion:** Flag ordering caused the issue.

Most memory systems flatten these into generic notes. Months later the agent remembers the conclusion but not the reasoning. The result: repeated investigations, reopened decisions, contradictory conclusions, lost institutional knowledge.

### The distinction

| Capability | Traditional Memory | MPM |
|---|---|---|
| Fact Storage | ✓ | ✓ |
| Long-Term Recall | ✓ | ✓ |
| Semantic Search | ✓ | ✓ |
| Decision Tracking | ✗ | ✓ |
| Hypothesis Management | ✗ | ✓ |
| Evidence Chains | ✗ | ✓ |
| Knowledge Challenges | ✗ | ✓ |
| Reasoning Persistence | ✗ | ✓ |

MPM does not merely remember *what* was decided. It preserves the reasoning that produced the decision — the alternatives considered, the evidence weighed, the hypotheses pending, the challenges raised and resolved.

That distinction transforms memory from passive storage into persistent reasoning.

The goal is not simply remembering information. **The goal is preserving intellectual progress.**

---

## 3. The Cognitive Model

Every persistent object inside MPM exists because it answers a different cognitive question.

### 3.1 The Artifacts

| Artifact | Purpose |
|---|---|
| Memory | Something observed or learned |
| Decision | A choice and its rationale |
| Theory | A testable hypothesis |
| Lesson | Reusable knowledge |
| Evidence | Information supporting or challenging another artifact |

The distinction matters.

A decision is not a memory.

A theory is not a lesson.

Evidence is not a conclusion.

Each has its own lifecycle.

#### Memory

General facts, observations, and synthesized insights.

> Germany leads Group E with +6 goal differential.

Memories are weighted, searchable, reinforced, challenged, and eventually archived.

#### Decision

Choices captured together with context and rationale.

```
CONTEXT: Need CSS injection that survives wp_kses filtering.
CHOICE: Store widget CSS in wp_options.
RATIONALE: WordPress strips inline style tags.
```

This allows agents to reconstruct previous reasoning instead of re-evaluating the same problems repeatedly.

#### Theory

Hypotheses that have not yet been proven.

```
HYPOTHESIS: Flag ordering causes parser failure.
VALIDATION: Run parser tests with positional-first and flag-first inputs.
STATUS: pending
```

Theories create a structured workflow for experimentation and debugging.

#### Lesson

Reusable knowledge that survives across tasks — best practices, warnings, patterns, and insights.

#### Evidence

Information that supports or challenges another artifact. Evidence is the substrate from which confidence is derived, never an artifact-level assertion of truth.

### 3.2 Design Principles

MPM follows a small number of architectural rules. These are stated once here. The rest of the document references them; it does not re-explain them.

1. **History is immutable.** A memory records what was believed. The original artifact is never rewritten.
2. **Interpretation is mutable.** Confidence changes. Evidence accumulates. The original artifact does not.
3. **Truth is external.** Reality determines truth. MPM only maintains its current estimate.
4. **Reasoning is persistent.** Decisions, theories, and evidence are first-class objects — the same permanence as facts.
5. **The contract is stable; the implementation is not.** Formulas, storage, and algorithms may change. The properties, the guarantees, the semantics — these are what matter. The implementation can change; the meaning survives.
6. **Simplicity beats infrastructure.** SQLite provides persistence, transactions, FTS5, JSON, WAL, and indexing. Adding another service is a last resort, not a default.
7. **Keep it boring.** Boring infrastructure is reliable infrastructure. The interesting problems in MPM live in the cognitive model, not in the deployment surface.
8. **Minimize moving parts.** Every component added is a component that must be reasoned about, debugged, secured, and explained. We earn each new piece through usage evidence, not architectural enthusiasm.
9. **Sympathy for unknown unknowns.** We do not know what MPM will be used for in three years. The architecture favors reversible, conservative choices over clever, brittle ones.

### 3.3 Confidence

Confidence is not user input.

Confidence is derived.

```
confidence
  = initial(type)
  + supporting evidence
  − contradicting evidence
  − decay(time)

bounded to [0, 1]
```

The exact formula may change. The properties do not: more positive evidence never decreases confidence; decay never increases it. The formula is a tool for satisfying the properties; the properties are what the system means by "belief."

For the deeper mechanics (atomic recompute, evidence registry, trigger wiring, source-of-truth/cache split), see §6.2 and **Appendix C** (Enforcement Patterns).

> **Implementation note:** The properties are enforced by property tests in `internal/core/confidence_test.go`. The tests describe what the formula means, not what it currently is. The implementation can change; the meaning survives.

### 3.4 Reframe, not rename

MPM stands for **Mnemonic Persistence Maintainer** — a memory aid, not a memory store. The on-disk tables (`memories`, `topics`, `lessons`) keep their historical names for schema stability: renaming them would be a destructive migration across every existing database.

Where the choice is open, prefer:

- "Cognitive artifact" or "reasoning record" over "memory"
- "Belief" over "stored fact" when the artifact has confidence
- "Epistemology" over "knowledge management" when the lifecycle matters
- "Substrate" over "database" when the system is the point

---

## 4. Belief Lifecycle

This is the section that explains how beliefs form, get tested, get updated, and get retired. The underlying subsystem is called the **Epistemology Engine**, but the section title describes what it actually does: a lifecycle for beliefs, not an academic treatise on epistemology.

### 4.1 The cognitive sequence

The forward flow is a typical cognitive sequence — what the agent does over time, not the only possible flow:

```
Observation → Memory → Decision → Theory → Retrieval → New Observation
```

Notice what never happens. The original memory is never edited. Only confidence changes. That distinction allows historical reasoning to remain inspectable months later.

### 4.2 Evidence is cross-cutting

The cognitive sequence above shows how artifacts come into being. The relationship below shows how their truth gets refined over time. The two are different things; conflating them is what made the earlier single-diagram view misleading.

A piece of evidence can support or challenge **any** artifact — memory, decision, or theory — not just the most recently created one. The artifacts that have evidence attached are what confidence is derived from. Decay reduces confidence over time. Retrieval surfaces artifacts; new observations restart the cycle.

```
                    Evidence
                       │
            ┌──────────┼──────────┐
            │          │          │
            ▼          ▼          ▼
         Memory    Decision    Theory
            │          │          │
            └──────────┼──────────┘
                       │
                       ▼
                  Confidence ◄── Decay
                       │
                       ▼
                  Retrieval
                       │
                       ▼
              New Observation
```

A memory can be challenged by a new observation. A decision can be challenged by a new test. A theory can be challenged by an independent reproduction. The evidence ledger doesn't care which kind of artifact it attaches to; confidence is derived uniformly across all three.

The forward flow is creation; the cross-cutting arrow is refinement. Both run continuously, and both are required for belief revision to work.

### 4.3 Decision Ledger

An append-only audit trail of architectural choices. Captures the context, the choice made, and the reasoning — so weeks later, the agent can reconstruct *why* a particular approach was taken instead of blindly second-guessing itself.

```bash
mpm record_decision "CONTEXT: We needed a CSS injection mechanism that survives wp_kses filtering
CHOICE: Route all widget CSS through agentshell_register_widget → wp_options → widgets.php <head> injection
RATIONALE: WordPress strips <style> blocks from post content via wp_kses_post() even for admins. The widget init JS also needs a footer injection point. Both requirements pointed to wp_options as the store."

mpm decisions
```

### 4.4 Theory Tracker

A hypothesis ledger for debugging and design. When the agent forms a causal assumption ("I think X is causing Y"), it logs the hypothesis and a concrete validation test before writing the fix. This forces the assumption to be testable, and often collapses a false hypothesis before it wastes an hour.

```bash
mpm propose_theory "HYPOTHESIS: passing --json before the positional arg causes the parse bug
VALIDATION_CRITERIA: write a unit test — invoke mpm with --json flag first vs positional-first, compare parse error rate
STATUS: pending"

mpm theories                    # List all theories with status chips
mpm theories pending            # Pending only

mpm resolve_theory abc123 "confirmed: flag order matters, --json consumed before positional processing"
```

### 4.5 Cognitive Immune System (Challenge Lifecycle)

Knowledge becomes obsolete. Most memory systems never address this problem. MPM introduces a challenge workflow:

```
Memory
    ↓
Challenge
    ↓
Theory
    ↓
Evidence Collection
    ↓
Confirmed / Disproven
```

When new evidence appears, a memory is challenged, a theory is created, evidence is gathered, the theory is resolved, and knowledge is updated. This is the mechanism by which the agent actively questions and overturns its own outdated knowledge.

**Challenge lifecycle (atomic transactions):**

```
challenge ─────────────────────────────────────────────────▶ [pending theory]
    │
    ├── challenge restore ──────────────────────────▶ [theory: disproven]
    │
    └── shred ─────────────────────────────────────▶ [theory: deleted]
```

- **`mpm challenge <id> "<evidence>"`** — atomic: patch memory metadata, create theory with back-link, weaken weight by 3.
- **`mpm challenge restore <id>`** — atomic: resolve theory as disproven, clear memory flags.
- **`mpm shred <id>`** — atomic: cascade-delete memory + linked theory + topic memberships.
- **`mpm ops gc --shred-negative`** — shreds only memories with weight<0 AND a proven theory exists. Negative weight alone is never sufficient — the theory provides the evidence chain.

### 4.6 Proactive Recall

Traditional memory systems wait for a search query. MPM actively surfaces relevant knowledge before it is requested.

```bash
mpm kb hint "discussing the CSS injection approach for the widget system"
# → 💡 [Recall] You decided: Route all widget CSS through agentshell_register_widget...
#    RATIONALE: WordPress strips <style> blocks from post content...
```

FTS5 keyword extraction detects semantic overlap with the current context and pushes a low-latency recall hint — with STATUS and RATIONALE displayed directly, not just the content. The `proactive_recall_hint` tool is wired into the OpenClaw agent loop and surfaces the most relevant epistemology memory automatically after context shifts.

### 4.7 What happens when MPM is wrong?

MPM models reasoning under uncertainty. Mistakes happen. The system is designed around explicit recovery — every bad outcome has a paper trail and a documented path back. Nothing is silently overwritten.

Five canonical failure modes and the mechanism that handles each:

**Bad evidence corrupting a decision.** A memory was anchored to evidence that turned out to be misread, fabricated, or context-dependent; the decision now rests on a false foundation. *Recovery:* `mpm challenge <id> "<why this is wrong>"` weakens the memory's weight by 3, atomically creates a back-linked theory in pending status, and preserves the original artifact. The memory is not deleted — its history is, by design, immutable. A future operator can audit *why* the memory was believed, *when* it was challenged, and *what* eventually resolved the dispute.

**Premature theory confirmation.** A theory was marked confirmed with thin evidence, or new evidence has since emerged that contradicts it. *Recovery:* the challenge lifecycle is non-monotonic. A confirmed theory can be challenged again, re-entering the evidence-collection state with a fresh back-link. There is no "settled science" path — theories are revisable for the lifetime of the database.

**Conflicting shared rules.** Agent A writes `Always use snake_case for DB columns`; Agent B writes `Always use camelCase for DB columns`. *Recovery:* every shared write produces a near-miss scan against existing rules via `shared.contradiction_log`. Detection runs opportunistically inside `query_long_term_memory` — no separate ticker. Near-misses carry a protobuf-shape score combining confidence, freshness, and reinforcement. The operator is presented with both rules and chooses via `ResolveArbitrationTheory`; MPM does not silently overwrite either side.

**Confidence decay drift.** A memory's weight decayed to a wrong value because the lifecycle sweep ran in an unexpected state, or because the underlying evidence ledger had a bug. *Recovery:* §10.1 Self-Healing Integrity Loop. Concept-drift detection quarantines affected memories (`concept_drift: true`), proposes pending theories, and survives restarts via SQLite-native dedup. The system errs on the side of *escalating to a theory* rather than silently fixing — wrong weight corrections are far more dangerous than visible ones.

**Embedding model change.** Operator upgrades from one embedding model to another; cosine similarities shift; "similar to X" links become noisy. *Recovery:* this is the hardest case. Mitigations: (1) BM25 keyword search is independent of embeddings, so the recall path still works for exact-term queries; (2) embedding backfill is a separate phase (`mpm ops backfill-embeddings`) and can be re-run on demand; (3) the hybrid score weights keyword ranking alongside semantic recall, so semantic noise degrades gracefully rather than breaking recall outright. There is no automatic re-embedding-on-model-change — this remains an operator-driven migration.

The common pattern: every recovery path leaves a theory, a log entry, or a `concept_drift` flag. History is the system.

---

## 5. Quick Start

Five minutes from zero to first decision. Choose your depth:

- **§5.1 Try it** — CLI only, no daemons. Best for evaluating MPM or one-off scripting.
- **§5.2 Run it as a daemon** — full autonomous operation: opportunistic wake dispatch, system-kind scheduled tasks (critic audits, snapshots, GC, broadcasts), and machine-to-machine integration via MCP.
- **§5.3 First commands** — the cognitive loop in eight lines.

### 5.1 Try it (CLI only — no daemons)

```bash
go install github.com/yourorg/mpm/cmd/mpm@latest
```

The single binary lives at `bin/mpm`. Try it without installing anything — no daemon setup, no service registration, no config files. (The companion daemons `mpm-mcp` and `mpm-scheduler` install separately when you want autonomous operation — see §5.2.)

### 5.2 Run it as a daemon

For autonomous operation — the scheduler dispatches system-kind wakes (critic audits, snapshots, GC, broadcasts) on a 60s ticker, and `mpm-mcp` exposes MPM to MCP hosts (Claude Code, OpenClaw) over stdio:

```bash
git clone https://github.com/yourorg/mpm
cd mpm
make build           # produces bin/mpm, bin/mpm-mcp, bin/mpm-scheduler, bin/mpm-critic
make install         # optional — copies all four to /usr/local/bin
```

**Install the scheduler as a systemd user service:**

```bash
make service-scheduler                              # copies unit to ~/.config/systemd/user/
systemctl --user daemon-reload                      # (make service-scheduler already does this)
systemctl --user enable --now mpm-scheduler         # enable + start
systemctl --user status mpm-scheduler               # verify
journalctl --user -u mpm-scheduler -f               # follow logs
```

The default unit assumes `~/projects/mpm` layout. Override via either:

- **Drop-in** (preferred for path changes): `systemctl --user edit mpm-scheduler`
- **Env file** (preferred for DB / backup paths): write `~/.config/mpm/mpm.env` with `MPM_DB_PATH=...`, `MPM_BACKUP_DIR=...`, `MPM_CRITIC_BIN=...` — it's sourced as `EnvironmentFile=-` in the unit.

**Seed the baseline cognitive directives** (recommended once after install):

```bash
mpm ops init directives
```

MPM provides advanced cognitive machinery — wake-context surfacing, audit-cluster detection, session-end triage — but that machinery requires specific agent behaviors to close the loop. These seed directives are the **Baseline Cognitive Bootstrap**: the "batteries-included" operating manual for that machinery. Without them, the scheduler will dispatch wakes but the cognitive loop won't close. Idempotent — safe to re-run; local edits are preserved.

**Wire up the MCP host** (Claude Code, OpenClaw, etc.). `mpm-mcp` is spawned by the host as a stdio child process — no separate service to manage. Example `.mcp.json`:

```json
{
  "mcpServers": {
    "mpm": {
      "command": "/path/to/mpm/bin/mpm-mcp",
      "env": {
        "MPM_DB_PATH": "/path/to/mpm/src/db/mpm.db"
      }
    }
  }
}
```

### 5.3 First commands

```bash
# Store
mpm add Germany leads Group E with +6 goal differential

# Retrieve
mpm World Cup prediction
mpm recall --semantic "为什么德国队表现这么好"

# Decide
mpm record_decision "CONTEXT: ...
CHOICE: ...
RATIONALE: ..."

# Theorize
mpm propose_theory "HYPOTHESIS: ...
VALIDATION_CRITERIA: ...
STATUS: pending"

# Challenge
mpm challenge <id> "recent data contradicts this"

# Status
mpm status
mpm ops stats
mpm wake               # last session context
```

Done. That's the cognitive loop: observe, decide, theorize, challenge, and (with the bootstrap) keep reasoning alive across sessions and vacations. The rest of this document explains how each piece works and how to operate the system at scale.

---

> **From here on, the document describes how the system is built.** §1–§5 cover what MPM is, why it isn't a memory system, the cognitive model, the belief lifecycle, and how to operate it. The remaining sections and appendices explain how it works — for readers writing patches or evaluating the architecture.

## 6. System Architecture

MPM intentionally separates persistent cognition from runtime behaviour.

### 6.1 Core vs Runtime

```
                    MPM
                     │
        ┌────────────┴────────────┐
        │                         │
        ▼                         ▼

    CORE                    RUNTIME

    Memories                Wake Context
    Decisions               Scheduling
    Theories                Personas
    Lessons                 Modes
    Evidence                Directives
    Confidence              Routing
                            Session Handoffs
```

The distinction is important. The Core describes what the agent knows. The Runtime describes how the agent behaves. Runtime systems may evolve. The Core should change very slowly.

Mature systems often owe their longevity to having a very small, stable core. Every feature that lives in Core must earn its place through years of usage evidence, not through the effort it took to build. Features that fail to justify themselves are removed. Engineers are sentimental about code; the regret log and disciplined review break that sentiment.

### 6.2 Confidence Engine

The Confidence Engine computes and tracks the system's belief in each artifact. The principles are stated in §3.3. This section is the **shape** — the evidence registry, the source-of-truth split, and the operations the agent can call. The mechanics (recompute atomicity, trigger wiring, transaction boundaries) live in Appendix C.

#### Evidence Registry

| Type | Default strength | Meaning |
|---|---|---|
| `observation` | 0.4 | Direct observation of an event |
| `test` | 0.7 | Result of an automated test |
| `reproduction` | 0.85 | Independently reproduced result |
| `challenge` | −0.6 | Evidence against an artifact |
| `decision_outcome` | 0.95 | Outcome of a recorded decision |
| `external_reference` | 0.6 | Cited source from outside the agent |

Override the default with `--strength`. Artifact types are a strict enum — the LLM cannot invent categories.

Initial confidence is also fixed by artifact type:

| Artifact type | Initial |
|---|---|
| memory | 0.8 |
| theory | 0.5 |
| decision | 0.6 |
| lesson | 0.7 |

#### Source of truth + cache

The stored `confidence` column is a performance cache. The evidence ledger is the source of truth. If they disagree, the cache is wrong. `mpm ops confidence recompute` triggers a manual recompute; `query_confidence_history` derives its view from evidence, not the cached column.

This is one of the four enforcement patterns that make MPM's guarantees stick. See **Appendix C** for the rest (property tests, AST guard rails, self-heal whitelist + escalation).

### 6.3 Retrieval Architecture

MPM combines four signals:

- **Keyword ranking** — SQLite FTS5 with BM25.
- **Semantic similarity** — 768-dim embeddings, cosine distance.
- **Reinforcement history** — how often a memory has been re-surfaced and re-used.
- **Recency** — when the memory was last reinforced.

The hybrid score is a weighted sum:

```
score = (reinforcement_count × 2) + (weight × 1.5) + recency_bonus
```

BM25's raw scores are unbounded; they are sigmoid-normalized so the four signals live on a comparable scale before combining. Use `--semantic` to drop BM25 and search by embedding similarity alone.

> **Implementation note:** The hybrid scoring function lives in `internal/core/hybrid_search.go`. The embedding model is `nomic-embed-text`; the 768-dim vectors are what the shared IVF index (§6.5 Layer 1) partitions into Voronoi cells.

**Memory provenance (`mpm recall --why`):** every result can be annotated with the score breakdown that retrieved it. Pass `--why` to see per-result `[why]` lines showing reinforcement contribution, weight contribution, recency age, and the FTS5 terms that matched. Useful for "why did the agent pick this memory?" introspection without re-running the search.

> **Retrieval ranking determines what is surfaced. Confidence determines what is believed.** A frequently retrieved artifact is not necessarily a trusted artifact.
>
> The hybrid score above ranks by relevance; it says nothing about how strongly the system believes the surfaced artifact. Confidence is derived from the evidence ledger (§6.2) and evolves through the challenge lifecycle (§4.5). An artifact can be top of the recall list with low confidence, or low in the list with high confidence — both states are common and correct. Conflating the two axes is the most common read of retrieval output; it is also wrong.

**LTM promotion:** `weight ≥ 10` OR explicit `mpm promote` OR auto-ingested `.md` file.

#### Concept Drift Detection

Concept drift detection — autonomously identifying paradigm shifts where historically trusted knowledge is decaying under a sudden barrage of new counter-evidence — is implemented as a **synchronous** operation that runs inline with `query_long_term_memory` (no background ticker). The detection signature:

| Signal | Threshold |
|---|---|
| Baseline | Artifact achieved high status historically (`peak_confidence >= 0.85`) |
| Velocity | ≥ 2 recent challenges or negative-strength observations within the last 7 days |
| Delta | Confidence has dropped ≥ 0.25 and sits below the distrust threshold (`< 0.60`) |
| Lifetime | ≥ 3 total lifetime negative evidence pieces (prevents one-off flukes) |

When a drifting memory triggers this signature, the engine quarantines the memory (sets `concept_drift: true`), proposes a pending theory, and survives restarts via SQLite-native dedup. Drift detection is pure-SQLite — no separate process, no separate timer, no panic-recovery surface to maintain. A drift missed last query is just as catchable next query.

### 6.4 MCP Integration

MPM integrates directly with AI agents as a **single MCP server**. The Go binary (`bin/mpm-mcp`) is the only substrate; agents connect to it via MCP and receive the full MPM tool surface as native function calls. No plugin layer, no Node/TypeScript wrapper, no Python shim — one binary speaking MCP.

The migration from the legacy plugin model (per-agent TypeScript wrappers calling the CLI binary via `child_process`) to the current MCP server model happened in 2026-06-23. The deleted plugin folders and their associated TypeScript sources are gone. The MCP server is the only integration surface for MCP-aware clients.

#### MCP tool surface

The MCP server exposes the full MPM substrate as native function calls. Adding a new tool is a single Go function — no plugin path, no shell wrapper, no parallel documentation.

```
<!-- tools:begin — auto-generated by `go generate ./internal/tools/`. Do not edit by hand. -->
save_to_memory           query_long_term_memory   challenge_memory

snooze_cluster           promote_scratchpad       shred_memory
reinforce_memory         weaken_memory            snooze_memory
set_memory_weight        patch_memory             promote_memory
promote_to_global

save_lesson              search_lessons           list_lessons

create_topic             search_topics            link_topic

add_reference            search_references        list_references

propose_theory           resolve_theory

record_decision

add_evidence             list_evidence

query_confidence_history query_confidence_changes query_confidence_trend
show_confidence          recompute_confidence     explain_confidence

query_audit_log

read_wake_context        read_directives          proactive_recall_hint
route

session_end              session_handoff          list_handoffs

review_memories          synthesize_memory        gc_run

query_memory_quality     list_active_clusters     resolve_cluster
annotate_cluster         flush_scratchpad         read_scratchpad
discard_scratchpad       log_to_changelog         query_global_rules
record_global_rule       schedule_wake            check_wakes
list_wakes
commit_milestone         check_pending_event_wakes
digest_wakes             health_check
<!-- tools:end — auto-generated by `go generate ./internal/tools/`. Do not edit by hand. -->
```

Wiring a new agent: add `mpm-mcp` to its MCP server config (OpenClaw: `mcp.servers.mpm` in `openclaw.json`; Claude Code: `.mcp.json`; any other MCP-aware client). The server binary is at `bin/mpm-mcp` relative to the MPM repo root.

#### Single source of truth: `internal/core/tools/registry.go`

Both the CLI (`mpm call <tool>`) and the MCP server iterate the same registry — a package-level `[]Tool` slice in `internal/core/tools/registry_list.go`. Each entry holds:

- `Name` — the tool identifier (used by both surfaces)
- `Description` — short prose shown to MCP clients
- `Schema` — JSON-Schema (raw bytes, parseable by both surfaces)
- `Handler` — `func(dm *DatabaseManager, ac *ActiveContext, payload map[string]interface{}) (interface{}, error)`. Same function called by both surfaces.

Adding a new tool: write `handleFoo` in `internal/core/tools/handlers.go` (one function), append a `Tool{...}` entry in `internal/core/tools/registry_list.go`. Both the CLI dispatcher and the MCP server pick it up automatically.

#### MCP/CLI parity: what is exposed via both surfaces

Every `mpm call <tool>` entry has a matching MCP tool spec; both call the same `DatabaseManager` methods. The 60+ tools in the surface above cover the agent's daily workflow: read/write memory, lessons, topics, references, theories, decisions, evidence, confidence, references, route, wake, directives, log_to_changelog, the memory feedback loop (`shred`/`reinforce`/`weaken`/`snooze`/`set-weight`/`patch`/`promote`), workflow (`review`/`synthesize`/`gc`), audit cluster triage, multi-agent shared epistemology (`record_global_rule`, `query_global_rules`, `commit_milestone`, `check_pending_event_wakes`), and health/diagnostics (`health_check`, `digest_wakes`).

A handful of CLI commands are intentionally **NOT** exposed via MCP/call because they are operationally distinct (destructive, cron-friendly, or human-gated):

| CLI command | Why CLI-only |
|---|---|
| `mpm ops doctor` | Diagnostic report; agents don't need to run it, scheduled cron does |
| `mpm ops lint` | Pre-commit / CI hook; agents should never invoke regex validation |
| `mpm ops backfill-embeddings` | Bulk operation; runs on a schedule, not per-request |
| `mpm ops synthesize` (bulk) | Per-memory synthesis IS exposed as `synthesize_memory`; bulk scan is human-initiated |
| `mpm ops gc --review` / `--purge` / `--shred-negative` | Destructive mass operations; core safe `gc_run` IS exposed |
| `mpm ops changelog build` | Release engineering; not an agent task |
| `mpm ops maintain` | Maintenance wrapper; individual ops below are exposed |

If an agent needs any of these, the operator should run it explicitly. Tool calls that could damage state are intentionally kept on the human-facing CLI where the cost of a misclick is bounded by the operator's attention.

### 6.5 Multi-Agent Shared Epistemology

Multiple agents on a single workstation can share a single source of truth for house rules, cross-project decisions, and durable conventions, while keeping their per-project tactical memories isolated. The substrate is SQLite `ATTACH DATABASE`. The behavior is a **five-layer stack** that turns the shared DB from passive storage into an active dissemination system.

**Why SQLite ATTACH, not Postgres, not a separate service.** The design contract is single-workstation scope. SQLite ATTACH gives the architecture without adding a server, a network boundary, or a new failure mode. The trade-off is no cross-DB transactions — accepted because rule writes are append-mostly and operator-gated.

#### Layer 0 — Federation

Each workspace has its own `mpm.db` (per-project tactical memory). A second database, `~/.mpm/shared/shared.db`, is ATTACHed as the `shared` schema. Cross-DB queries become plain SQL.

The shared DB uses the **same table schema** as the local DB. Migrations apply to both DBs at startup. FTS5 sync triggers keep the shared FTS5 mirror in lockstep with the shared tables. `getDB()` is a per-process singleton, so one ATTACH, one connection, no leak surface.

`query_long_term_memory` federates across both DBs with a multiplicative Shared Premium: `score = base × 1.20` for shared results, `score = base × 1.35` when the shared row also matches a rule. The boost only applies to rows that already pass the base relevance threshold — house rules outrank noisy local memories without being able to invent matches.

Writing to shared (`record_global_rule`, `promote_to_global`) requires `confirm=true` in the tool payload. The agent cannot autonomously extend the shared rule set — only the operator can.

#### Layer 1 — Shared Vector Search

Local FTS5 is a keyword index. For semantic recall over the shared DB, MPM runs an in-SQLite IVF (Inverted File) ANN index — no separate vector database, no CGo, no parallel files. The index partitions the 768-dim embedding space into Voronoi cells, then restricts each query to the nearest few. A circuit breaker caps worst-case work.

#### Layer 2 — Conflict Detection

When two memories disagree, the system has to notice. `shared.contradiction_log` is the detection surface. Every shared write that produces a near-miss against an existing memory is logged with a protobuf-shape score (a weighted combination of confidence, freshness, and reinforcement). Detection runs **inside `query_long_term_memory`'s result set** — opportunistic, not polled. There is no background ticker, no separate timer.

If the two candidates land within a small margin of each other, the system **does not** auto-resolve. It writes a pending theory and waits for an operator. A clean winner can be auto-resolved; a coin-flip always escalates.

#### Layer 3 — Conflict Resolution + Arbitration Closure

When a contradiction resolves, the system doesn't just delete the loser. It writes a **resolution memory** capturing the choice and rationale, marks the queue resolved, and — as of the Arc 1 Closure epic — appends an evidence row to the winner.

**Resolution-as-evidence asymmetry.** Only the winner gets the `resolution_survived` evidence row. The loser is tagged with `operator_arbitration` and the originating theory ID in its metadata, then archived. A memory that has survived N contradictions accumulates 1.0×N supporting evidence in its confidence ledger — the system is literally rewarded for being right under pressure.

The arbitration auto-slash loop closes when the operator resolves a pending theory: the resolution memory, the queue update, the asymmetric evidence write, and the theory status flip all happen in one transaction. The loop is closed.

#### Layer 4 — Active Dissemination

Arc 1 made the shared DB write and self-heal. But receiving agents had no idea any of this happened unless they ran a search that surfaced the resolution. Arc 2 fixes that.

When an epistemic event lands (rule, resolution, arbitration), it **fans out** as a deterministic event-wake to every active session. Receiving agents see the new state on their next MPM call without going through the federated search at all. The shared DB is no longer passive; it actively pushes its own updates.

The dedup mechanism is a deterministic SHA-256 prefix used as the PRIMARY KEY of `shared.bcast_event_wakes`. `INSERT OR IGNORE` is the dedup mechanism — database-level, O(1), race-free. Re-broadcasting the same content is a no-op. A rule body text change fires a new wake; a metadata-only update does not.

Three operator commands (`record_global_rule --confirm=true`, `resolve-contradictions --apply`, `resolve_theory --winner=<id>`) trigger auto-broadcast as a strict side-effect of the explicit gate. Auto-broadcasts are **non-fatal**: a broadcast failure does NOT roll back the underlying write. The resolution is ground truth; the broadcast is the notification. Internal agent writes (without an operator gate) do NOT trigger broadcasts.

**The receive path:**

```bash
mpm call check_pending_event_wakes --payload '{"session_id":"..."}'
```

The dispatcher pulls pending event wakes after every handler returns, so the agent sees them on every MPM call without an explicit pull. New rules, resolutions, and arbitration verdicts surface inline as an `EventWakesPending` block in the response.

**Why push, not pull.** Federation is pull: an agent decides to query, the system responds. Event wakes are push: the shared DB has decided something changed, and the change is **already in the agent's wake context** on the next call. No search, no cold-start, no missed signal — even a fresh process picks up pending wakes immediately.

#### Operational notes

- Cross-DB transactions are NOT supported. Every shared write is its own atomic transaction on the shared DB.
- WAL mode on both DBs allows concurrent local writers without blocking.
- A drift missed last query is just as catchable next query. Detection and fan-out are both opportunistic.
- Cold-start does NOT replay old wakes. `query_long_term_memory` handles history; event wakes are real-time push only.

For the DDL, fan-out algorithm, auto-broadcast hooks, tests, and smoke behind this stack, see **Appendix A** (Layers 0-3) and **Appendix B** (Layer 4).

---

## 7. Core Stability

The following concepts define MPM. They should change rarely.

- Memories
- Decisions
- Theories
- Lessons
- Evidence
- Confidence

Everything else is considered Runtime.

The Core should evolve only when the model of cognition evolves.
Runtime should evolve whenever the operation of agents improves.

Runtime features may evolve rapidly without changing the cognitive model.

### Why this matters

A small, stable core is what makes the rest of the system safe to evolve. Every feature outside Core — wake, routing, personas, scheduling, synthesis, review, audit — can be redesigned, replaced, or removed without invalidating the agent's existing knowledge. Core changes require demonstrated usage evidence and survive only if the regret log proves the change carries weight.

### The discipline

Before adding a new feature or changing an existing one, the answer to these questions must be clear:

- What problem does this solve?
- Who has this problem?
- What is the cost of adding it?
- Will it survive the regret log?

Default to no. The threshold for adding is high. The threshold for removing is the same.

### The regret log

`workspace/REGRET_LOG.md` (created 2026-06-30) is an append-only collection of things the agent considered but didn't add, and things the agent added that turned out not to matter. It is the empirical record of feature-creep pressure and the source of truth for what to delete next.

---

## 8. CLI Reference

The CLI is a convenience surface. The machine-to-machine interface is `mpm call <tool> --payload JSON`. The CLI calls the same handlers internally.

### Daily Commands (root level)

```bash
mpm <query>        # FTS5 search (default when called with a bare string)
mpm add <text>     # Add a new memory
mpm add -i         # Interactive add — opens $EDITOR
mpm snooze <id>    # Bump a memory's relevance (no LTM promotion)
mpm ls             # List recent memories
mpm show <id>      # Show one memory
mpm rm <id>        # Soft delete
mpm wake           # Last session context (mode, persona, topics, recent memories)
mpm call <tool>    # Universal machine interface (JSON payload)
mpm status         # System status dashboard
mpm version        # Version info
mpm help           # Full help
```

### `kb` — Knowledge Base

Entity-centric interface to all MPM collections.

```bash
# Memory
mpm kb memory list
mpm kb memory search <query>
mpm kb memory show <id>
mpm kb memory add <content>
mpm kb memory shred <id>

# Topic
mpm kb topic list
mpm kb topic add <name> [description]
mpm kb topic search <query>
mpm kb topic show <id>
mpm kb topic shred <id>
mpm kb topic link <topicId> <linkTopicId>

# Lesson
mpm kb lesson list
mpm kb lesson list --type warning
mpm kb lesson add <content> [--type warning|practice|insight]
mpm kb lesson search <query>
mpm kb lesson get <id>
mpm kb lesson shred <id>
mpm kb lesson stats

# Session
mpm kb session list
mpm kb session search <query>
mpm kb session show <id>
mpm kb session shred <id>

# Reference library (PDF, EPUB, MD, TXT, HTML)
mpm kb reference add <file> [--tag tag1,tag2] [--reason "..."] [--chunk-size 512]
mpm kb reference ls
mpm kb reference search <query>
mpm kb reference show <id>
mpm kb reference used                    # most-retrieved references
mpm kb reference interactions            # recent retrieval events
mpm kb reference admit [--limit N] [--dry-run]
mpm kb reference shred <id>

# Epistemology
mpm kb theories [pending|resolved|all]
mpm kb decisions
mpm kb propose_theory <theoryId>
mpm kb resolve_theory <theoryId> <status>
mpm kb record_decision <decisionId>
mpm kb hint <topic>
```

### `ops` — Engine Room

Maintenance, diagnostics, synthesis, and power tools.

```bash
# Core engine
mpm ops doctor [--explain|--deep-scan]
mpm ops self-heal [--dry-run|--force|--quiet]
mpm ops maintain
mpm ops synthesize [--dry-run]
mpm ops synthesize status
mpm ops synthesize failures
mpm ops gc [--dry-run|--review|--purge|--shred-negative]
mpm ops backfill-embeddings [--batch-size|--collection|--dry-run]
mpm ops dlq:review [review|clear|retry]
mpm ops lint [--dir <path>]... [--show-clean]
mpm ops changelog build [--since v1.0.0] [--release-version 1.1.0] [--legacy]
mpm call query_audit_log [--level|--component|--days|--limit]
mpm call session_end --payload '{"session_id":"...","summary":"...","commitments":[],"open_questions":[]}'
mpm call session_handoff [--unread|--mark_read]
mpm call list_handoffs [--limit|--unread]

# Diagnostics
mpm ops stats
mpm ops status
mpm ops prune [--older-than <n>d]

# Data
mpm ops export [--jsonl]
mpm ops backup [path]
mpm ops restore-db <path>
mpm ops ingest <path>

# Context
mpm ops switch
mpm ops directives
mpm ops wake

# Mode & Persona
mpm ops mode [list|active|remove|clear]
mpm ops persona [list|active|set|clear]

# XITL Stance (runtime persona hot-swap — no restart)
mpm ops stance assume <mode> <persona> <rationale>
mpm ops stance synthesize <name> [flags]
mpm ops stance promote

# Multi-agent shared epistemology
mpm ops shared [status]
mpm ops resolve-contradictions [--apply] [--json]      # Arc 1: dry-run inspect or apply resolutions
mpm ops broadcast <memory_id>                          # Arc 2: fan-out an event wake
        [--kind=rule|resolution|arbitration|memory]    #    (auto-detected from collection if omitted)
        [--rationale="..."]                            #    (REQUIRED for kind=memory)
        [--to=agent_id,...]                             #    (targeted; otherwise full active fleet)
        [--dry-run] [--json]
mpm ops active-sessions [--json]                       # Arc 2: list the active fleet (last 24h)
mpm resolve_theory <id> <conclusion> --winner=<id>     # Arc 1 closure: arbitration auto-slash

# Entity ops
mpm ops topic|lesson|session|reference|memory|wake|gateway
```

### `debug` — Low-Level Inspection

```bash
mpm debug history <memoryId>         # Version history for a memory
mpm debug diff <memoryId> <v1> <v2>  # Unified diff between two versions
mpm debug diff-lines <text1> <text2>
mpm debug patch-memory <id> <json>
mpm debug shred <id>
mpm debug show <id>
mpm debug gc [--dry-run]
```

---

## 9. Runtime Services

The Runtime is where MPM evolves. Services documented here are intentionally decoupled from Core — they may be redesigned, replaced, or removed without invalidating existing knowledge.

### Embedding Pipeline

Auto-embed on `mpm add` and on one-shot ingestion via `mpm ops ingest`. `mpm ops backfill-embeddings` provides batched, resume-safe backfill for existing memories. tiktoken (`cl100k_base`) drives token-aware chunking.

### Memory Versioning

Every memory has an append-only version history. `mpm debug history` shows all revisions with timestamps. `mpm debug diff` computes unified diffs between any two versions. Terminal state is captured in `memory_revisions` for `--as-of` time-travel.

### Topic Auto-Suggestion

On `mpm add`, the system automatically suggests linking to semantically related existing topics. Topics are also auto-created for epistemology collections (`decisions`, `theories`).

### Cross-Reference Linking

Bounded bidirectional Memory↔Topic↔Reference cross-refs. Links are created on save and surfaced on recall.

### Synthesis Deduplication

Context-aware deduplication: synthesis deletes the triggering memory after LTM save, preserves oldest `created_at`, transfers topic_memberships, excludes epistemology collections, quality gate requires ≥2 candidates.

### Reference Library

PDF, EPUB, HTML, Markdown ingestion with token-aware chunking (`--chunk-size`, 64–2048 tokens, default 512). Sources are diff-keyed by content hash: re-ingesting an unchanged document costs zero embedding work. Embedding is a separate phase from chunk insert, so slow embed calls never block ingest. Reference docs are a *shelf*, not a memory: they live in SQLite, are indexed for full-text and semantic search, and are surfaced to agents on demand. Admission (whether a pattern from a consulted reference is worth promoting to long-term memory) is a per-consult decision, not an ingest-time gate.

### Session Memory Context (`wake`)

`mpm wake` surfaces the last session's mode, persona, topics, and recent memories — the agent's bootstrap context on startup.

### Scheduled Wakes (Stateless, Opportunistic by Default)

The agent can defer work to a future moment with `mpm call schedule_wake` and have the reminder surface automatically on the next call. The database is the queue, the next call is the dispatcher — no scheduler process required for this path.

```
mpm call schedule_wake --payload '{
  "reason": "Check WC2026 R32 result for Germany",
  "target_time": "24h",
  "theory_id": "7383f1572d73bc9a"
}'
```

`target_time` accepts either an absolute unix epoch or a relative duration (`30s`, `5m`, `2h`, `1d`, `7d`). Resolves to absolute at insert time and is returned in `target_iso` for log clarity.

The "agent has initiative" effect: any subsequent `mpm call` (CLI or MCP) that lands after `target_time` surfaces the wake inline as a `WakesPending` block in the response. The agent sees it, evaluates the reason, optionally schedules the next round. No missed reminders even if the agent is asleep — the next call after the target picks it up, marked with `overdue_secs` for honest accounting.

Companion tools: `check_wakes`, `list_wakes`. Architecture: `scheduled_wakes` table + composite index `scheduled_wakes_due(fired, target_time)` + FTS5 virtual table for content search. `CheckPendingWakes` runs in a single transaction (idempotent across concurrent callers).

**Trade-off vs. a real-time push daemon:** MCP has no server-initiated messages over stdio, so `mpm-mcp` cannot fire a wake back to a sleeping agent. The opportunistic fold is the next-best mechanism — at-most-once-on-next-contact, not real-time. For Wimbledon R1, WC2026 group stage, and monthly Meshal reminder use cases this is sufficient. Real-time push would require an SSE transport change and is deferred.

### Autonomous wake execution (mpm-scheduler + mpm-critic)

For system-level actions that must run unattended regardless of user presence (pre-flight snapshots, critic audits, GC sweeps, broadcasts), `cmd/mpm-scheduler` is a companion Go daemon that consumes `scheduled_wakes` on a 60s ticker. Wakes tagged with `metadata.kind=snapshot|critic_audit|gc|broadcast` are dispatched to registered handlers and execute inline; untagged wakes pass through to the opportunistic fold unchanged. `cmd/mpm-critic` is the standalone runner for one audit cycle — the scheduler's `critic_audit` handler shells out to it. Install via `make build`; ship under systemd as a user service for persistence. Both binaries are first-class artifacts (Go, no shell wrappers). The two-way bridge with `mpm-mcp`: `CheckPendingWakes` filters system kinds from the opportunistic fold so the two surfaces don't race for the same wake.

### Event Wakes — Active Dissemination (Arc 2)

Local scheduled wakes (above) are **self-directed** — the agent schedules a reminder for itself. **Event wakes** are **other-directed** — when an epistemic event lands (a new house rule, a contradiction resolution, an arbitration verdict), the shared DB pushes a wake to every other active session on the fleet.

```
mpm call check_pending_event_wakes --payload '{"session_id":"..."}'
```

**Deterministic dedup.** `wake_id = sha256(memory_id + ":" + target_session + ":" + content_hash)[:12]` is the PRIMARY KEY of `shared.bcast_event_wakes`. `INSERT OR IGNORE` is the dedup mechanism — database-level, O(1), race-free, no app-level state. Re-broadcasting the same content is a no-op; a rule body text change fires a new wake; a metadata-only update (e.g., `status=challenged`) does not.

**Three-state wake table.**

| Column | Purpose |
|---|---|
| `wake_id` (PK) | Deterministic sha256 prefix → dedup |
| `target_session` | FK to `shared.bcast_sessions` |
| `source_agent` | Who broadcast (operator + agent ID) |
| `memory_id` | The artifact that triggered the wake |
| `kind` | `rule` / `resolution` / `arbitration` / `memory` |
| `content_hash` | Drift detection |
| `rationale` | The WHY (non-negotiable) |
| `created_at` | Unix epoch |
| `fired` | Boolean; flipped to 1 by `CheckPendingEventWakes` in a transaction |

**Why this is different from a pull-based `query_global_rules`.** Federation (Layer 0 in §6.5) is pull: an agent decides to query, the system responds. Event wakes are push: the shared DB has decided something changed, and the change is **already in the agent's wake context** on the next call. No search, no cold-start, no missed signal — even a fresh process picks up pending wakes immediately.

**Auto-broadcast hooks.** Three operator commands trigger automatic broadcasts as a strict side-effect of the explicit `--confirm` / `--apply` / `--winner` flag. Auto-broadcasts are **non-fatal**: a broadcast failure does NOT roll back the underlying write. The resolution is ground truth; the broadcast is the notification. See §6.5 Layer 4 for the architecture and §6.5 Layer 3 for how the underlying rule/resolution/arbitration actually lands.

**Why the deterministic ID is the entire architecture.** Without the PRIMARY KEY, dedup is an application-level check: SELECT then INSERT, with a TOCTOU race. With the PRIMARY KEY, dedup is a database-level guarantee: `INSERT OR IGNORE` is atomic, idempotent, and O(1). The whole receiver-correctness story is the deterministic ID; the rest is plumbing.

For the schema, fan-out algorithm, test matrix, and smoke behind this section, see **Appendix B**.

### Ephemeral Scratchpad (Working Thesis Storage)

A per-session scratchpad for hypotheses that aren't ready for permanent memory. Single-row-per-session, 24h decay, atomic promote. The scratchpad sits between "thought I had this turn" and "memory I'm willing to defend."

**The four verbs:**

| Verb | What it does |
|---|---|
| `flush_scratchpad` | Write or update the thesis for a session. Idempotent on `session_id`. |
| `read_scratchpad` | Peek at the current thesis. |
| `discard_scratchpad` | Hard-delete the scratchpad without promoting. |
| `promote_scratchpad` | Atomically promote to permanent memory. |

**The atomic rollback is the entire point.** `promote_scratchpad` runs the security scanner against the synthesized memory *inside* the same transaction as the memory INSERT and the scratchpad DELETE. If the scanner rejects (poison-phrase match, sensitive content, etc.), the entire transaction aborts: the scratchpad row survives for the agent to amend, and no memory row is created. A thought that fails the scanner is **not lost** — it is preserved for revision.

**Wake-context orphan surfacing.** A scratchpad from a session that never promoted becomes an *orphan* on the next wake. `read_wake_context` includes an aggregate header (Fresh / Dormant / Expired counts) and a truncated thesis preview for each orphan. The agent's options: `promote_scratchpad` (commit), `flush_scratchpad` (amend), or `discard_scratchpad` (abandon). This closes the "agent crashed mid-thought" gap — even a process kill between flush and promote doesn't lose the work.

**Decision boundary vs `save_to_memory`:** the scratchpad is for *tentative* thoughts — working hypotheses, half-formed theories, intermediate conclusions that may need revision. `save_to_memory` is for *committed* facts. The promote path is the gate: if you're not ready to defend a thought against the security scanner and future challenges, it belongs on the scratchpad first.

**Trade-off vs. auto-promote-on-flush:** `flush` never auto-creates a memory. The agent must explicitly `promote_scratchpad` and pass the scanner. This is the right friction: tentative thoughts should be deliberate before they become permanent knowledge. A flush that auto-promoted would silently double the agent's memory-write surface, bypassing the scanner's intent.

### XITL Stance Hot-Swap

`mpm ops stance assume <mode> <persona> <rationale>` switches mode/persona at runtime with no restart. The new directive prints to stdout — OpenClaw captures it and injects into session chat history, so the agent reads and adopts it on the very next turn. `mpm ops stance synthesize <name>` generates a JIT ephemeral persona from a prompt; `mpm ops stance promote` flushes it to a permanent `.md` file.

### Directives (Prime Operating Principles)

Directives are the agent's **prime directives** — non-negotiable behavioral principles that govern how it operates. Unlike modes (which govern retrieval parameters) and personas (which govern tone), directives are the hard rules: the things the agent must and must not do on every turn.

#### Reflex Engine — two-tier behavioral rules

| Tier | Lives in | Loaded | Examples |
|---|---|---|---|
| Prime Directives | SQLite `collection='directives'` | Always, via `read_directives` | "Always log contradictions as challenge evidence, never overwrite" |
| Mode Directives | `mode/*.md` files | Only when the agent is in that mode | "Query confidence history on suspicious memories before debugging" |

Storage: `collection='directives'` (MCP path) or legacy `is_prime_directive = 1`. Both read paths query `WHERE (collection = 'directives' OR is_prime_directive = 1) AND deleted_at IS NULL`, so a directive is visible from every consumer regardless of which identifier was set.

Elevation: `mpm call save_to_memory --payload '{"fact": "Always verify before acting", "collection": "directives", "tags": ["prime_directive"]}'`.

The `proactive_recall_hint` engine also elevates directive-adjacent memories when the current conversation context matches their semantic territory.

#### Baseline Cognitive Bootstrap

For fresh installs, MPM ships a small set of reference directives that close the system's most important cognitive loops (wake-context reading, session-end cluster triage). Seed them once with `mpm ops init directives` — idempotent, never overwrites local edits. See §5 step "Initialize baseline directives" for context.

> **Implementation note:** The reference directives live in `internal/seed/directives.go`. The bootstrap command detects existing directives by stable ID and skips them; local edits to a seeded directive are preserved, never silently overwritten.

### Modes & Personas (File-Based)

Modes and personas are `.md` files with YAML frontmatter. **No database, no compile step.** Filename is the identity. Each mode specifies retrieval parameters:

| Mode | retrieval_limit | retrieval_threshold |
|---|---|---|
| `debugging` | 3 | −2.5 |
| `research` | 20 | −1.0 |
| `architect` | 10 | −1.5 |
| `programming` | 5 | −2.0 |
| `standard` | 7 | −1.5 |

#### Auto-Selection (`route` tool)

Zero-latency heuristic router — scores incoming prompts against all loaded modes and personas without any LLM call, network latency, or external dependency.

```bash
mpm call route --payload '{"prompt": "I need to draft a whitepaper about our Q3 architecture"}'
```

Returns:

```json
{
  "selected_modes":   ["architect", "research", "write"],
  "selected_persona": "venkat",
  "scores": { ... }
}
```

Frontmatter schema (every `mode/*.md` and `persona/*.md`):

| Key | Type | Purpose | Routing impact |
|---|---|---|---|
| `name:` | string | Identity | none |
| `title:`, `creature:`, `vibe:`, `voice:` | string | Persona identity / voice | LLM context only |
| `patterns:` | string (regex fragments, comma-separated) | **When this component speaks** | +2 per match |
| `domain_out:` | string (regex fragments, comma-separated) | **When this component refuses** | −1 per match, logged |
| `voice_guards:` | string (prose, comma-separated) | **How this component sounds** | LLM context only |

Scoring: modes use threshold filtering (multi-select); personas use max-pooling (single-select, highest score wins, no implicit `default` fallback). Hot reload: directory mtimes monitored, no server restart needed.

#### Router Frontmatter Linter (`mpm ops lint`)

Proactive defense against the silent-failure class of bugs where hand-curated frontmatter compiles but never matches. Checks: YAML parses, regex compiles with `(?i)` prefix, raw-form `domain_out:` compiles without `regexp.QuoteMeta`. Wired into the pre-commit hook. 9 unit tests pin the contract.

### Security Scanning

Content scanned against **20 regex patterns** (API keys, JWTs, SSH keys, connection strings, password patterns) before any database write. Blocked content goes to `mirror.jsonl` but never reaches the database. Coverage enforced by a static-analysis test that walks every function containing a literal `INSERT INTO memories` and verifies the function (or its caller) calls the scanner.

### Fsnotify Reconciliation

30s startup delay + 10-min periodic sweep (25 file/sweep cap) reconciles the filesystem state. `source_path` metadata check prevents re-ingestion of already-processed files. (Residual from the deprecated watcher; consolidated into on-demand ops.)

### Spaced Reinforcement Review

```bash
mpm ops review --promoted   # Show recently elevated LTM memories
mpm ops review --stale      # Surface forgotten LTM memories
```

### Watcher Deprecation (2026-06-26)

The MPM file-watcher daemon (`mpm watch start|stop|status`, fsnotify goroutine, worker pool, 5-min decay loop) was deprecated and removed on 2026-06-26. Its original purpose — auto-ingest of files written by pre-MCP agents — is obsolete now that `save_to_memory` is a native MCP tool. Replacement matrix:

| Watcher capability | Replacement |
|---|---|
| 5-min decay sweep | `mpm ops maintain` on demand (also runs autonomously in the daemon) |
| External SQLite polling | `mpm ops ingest --source <path>` one-shot |
| LLM synthesis | `mpm ops synthesize [--dry-run]` on demand (also runs autonomously in the daemon) |
| Topic clustering | `mpm ops synthesize` (synthesis pass) |
| Filesystem auto-ingest | Obsolete — `mpm call save_to_memory` covers it |

The reusable parser library (`extractFacts`, `extractFromSessionLine`, `looksLikeFact`, `extractKeywords`, `parseSessionsSnapshot`, `readJSONLines`) was preserved in `cmd/mpm/parsers.go` for future one-shot filesystem tooling.

---

## 10. Reliability

MPM is designed for long-running autonomous operation: SQLite WAL mode, dead-letter queues, synthesis isolation, retry pipelines, event replay buffers, watchdog telemetry, overflow protection, and a closed self-healing integrity loop.

> **The Reliability Model.** MPM's guarantees are not aspirational — they are encoded in four repeating patterns:
>
> - **Source-of-truth + cache.** If two values could disagree, exactly one is authoritative; the other is a derivable cache. (§6.2 example: `confidence` is a cache; the evidence ledger is the source of truth.)
> - **Property tests, not implementation tests.** Pin the *meaning*, not the *formula*. The decay function can change shape forever; "decay never increases confidence" stays true forever.
> - **AST guard rails.** Contracts that span JSON-Schema and a handler are enforced by the type system, not the prose — the schema is a strict superset of the keys the handler reads.
> - **Self-heal whitelist + escalation.** Auto-fix only what is known-safe. Everything else escalates to a pending theory for operator review.
>
> These patterns are documented in depth in **Appendix C**. Every "this is guaranteed" claim in the body of this document maps to one of them.
>
> These patterns exist because the next architectural threat is no longer bad design — it is implementation debt. Too many artifact types, too many lifecycle states, too many special cases, too much intelligence expected from the substrate. The antidote is the same as §7's: strict enums, invariants pinned by property tests, append-only records, boring storage. Default to no.

This section is the operator-facing view of the same model: what runs, when, and what to do when it fails.

### Self-Healing Integrity Loop

Long-running databases accumulate drift. Soft-deleted memories leave ghost rows in FTS5 indexes. Cascade-deletes fail to clean up all related tables. Schema migrations introduce orphans. Without a closed feedback loop, this drift degrades search quality silently and compounds over time.

MPM ships an autonomous self-heal command that detects, classifies, and acts on integrity drift without any human in the loop:

```bash
mpm ops self-heal              # run with all defaults
mpm ops self-heal --dry-run    # report drift without taking action
mpm ops self-heal --force      # bypass the 24h cooldown
mpm ops self-heal --quiet      # silent on clean state (for cron)
```

The companion on-demand audit:

```bash
mpm doctor --deep-scan         # human-readable integrity report
mpm doctor --deep-scan --fix   # clean soft-delete ghosts in place
```

#### How it works

```
                              cron / operator
                                    ↓
                          mpm ops self-heal
                                    ↓
                          runDeepScanCheck
                          (3 integrity queries)
                                    ↓
                ┌───────────────────┴────────────────────┐
                ↓                                        ↓
        known-safe drift                          unknown drift
        (soft-delete ghosts)               (FTS orphans, dangling
                ↓                             topic_memberships)
        auto-fix + lesson                           ↓
        (24h cooldown,                       pending theory
         max 1000/run)                       (theories collection)
                ↓                                        ↓
        audit trail in                              ↓
        lessons table                  next agent boot reads
                ↓                     read_wake_context, sees
        transparent.                  the theory, acts on it.
        silent.                               ↓
                                      self-healing.
                                      escalates.
```

#### Three drift classes detected

| Class | Detection query | Behavior |
|---|---|---|
| Soft-delete ghosts | `memories_fts` rows whose joined `memories` row has `deleted_at IS NOT NULL` | **Auto-fix** (known-safe) |
| FTS orphans | Any `*_fts` row whose rowid is missing from the source table | **Escalate** (unknown) |
| Dangling memberships | `topic_memberships` referencing non-existent topics | **Escalate** (unknown) |

#### Four safety boundaries on auto-fix

1. **Whitelist-only** — only soft-delete ghosts are auto-fixed.
2. **Bounded blast radius** — if ghost count exceeds `SelfHealMaxFix` (default 1000), refuse to auto-fix and escalate.
3. **Rate-limited** — 24h cooldown between same-signature auto-fixes.
4. **Audit trail** — every auto-fix writes a `lessons` table entry tagged `source=self-heal`.

#### Why known-safe vs unknown

The asymmetry is deliberate. Soft-delete ghosts are the only drift class where we have a precise model of the correct state AND a proven-safe mechanism that should have produced that state. For FTS orphans and dangling memberships, we cannot tell from inside the scan whether the FTS row is the bug or the source-table row is the bug. Auto-fixing novel drift is exactly the kind of guesswork that causes data corruption. Escalate.

The cognitive loop closes through the existing wake context: when self-heal escalates unknown drift, it injects a pending theory; the next `mpm call read_wake_context` surfaces it; the agent acts on it. **No new wake code is needed.**

#### Recommended cron

```cron
0 4 * * 0 cd /home/v/workspace/projects/mpm && /usr/local/bin/mpm ops self-heal || echo "MPM drift detected: $(date)" | mail -s "MPM Self-Heal Alert" v
```

### Self-Audit Log

Runtime telemetry has historically lived in flat files (`watchdog.jsonl`, `mirror.jsonl`) — readable by humans grepping logs, blind to the agent. MPM moves that telemetry into the cognitive surface so the agent can see what went wrong across sessions.

```bash
mpm call query_audit_log --days 1 --limit 20
mpm call query_audit_log --level error --days 7
mpm call query_audit_log --component relay --days 1
```

The wake context surfaces a single-line summary when errors or fatals were logged in the last 24h.

#### Storage

| Column | Type | Purpose |
|---|---|---|
| `id` | TEXT PK | Unique identifier |
| `level` | TEXT | `warn` / `error` / `fatal` (CHECK constraint) |
| `component` | TEXT | `relay`, `synthesis`, `watcher`, `security` |
| `message` | TEXT | Human-readable description |
| `stack_trace` | TEXT | Auto-captured Go stack at log point (truncated to 4KB) |
| `context` | JSON | Structured 3–5 field key-value pair |
| `created_at` | DATETIME | UTC timestamp in SQLite `YYYY-MM-DD HH:MM:SS` |

Indexes: `(level, created_at)`, `(component)`, `(created_at)`. 30-day retention enforced by the gc sweep.

#### Wired subsystems

| Component | Trigger |
|---|---|
| `security` | Poison phrase or sensitive content blocked during memory write |
| `relay` | Non-trivial HTTP error during SSE broadcast |
| `synthesis` | All LLM vendors failed; event routed to DLQ |
| `watcher` | DEPRECATED — new audit rows not expected |

#### Why a SQLite table, not more jsonl files

The audit log is queryable from the agent via the JSON boundary, surfaceable in wake context, and pruneable by the existing gc sweep — all without new infrastructure.

#### Audit cluster proposals — three-layer self-healing

Raw audit events are the *nervous system* of the substrate, but a single error doesn't tell the agent what's been breaking. MPM runs a write-side cluster detector that aggregates repeated (component, message) pairs into structured proposals, then exposes them through the wake context and a dedicated MCP tool. The architecture is intentionally three layers so the agent can act on what it sees.

| Layer | What | Where |
|---|---|---|
| Write | Atomic UPSERT into `audit_cluster_proposals` on every `LogAudit` call. Rolling 7d window. Default threshold: 3 events. | `internal/core/audit.go::upsertClusterCounter` |
| Read | Deduped view partitioned into `known` (cluster_key in any pending theory / recent decision / resolved theory) vs `unknown`. Surfaced as a tiered string in `read_wake_context`. | `internal/core/cluster_proposals.go::ActiveClusters` + `internal/core/wake_context.go::auditSummaryRich` |
| Act | `mpm call list_active_clusters` returns structured JSON (cluster_key strings) so the agent can triage at session end. Three management verbs (snooze / resolve / annotate) close the loop on each proposal. | `internal/core/tools/handlers.go::handleListActiveClusters` |

Tunables centralized in `internal/core/cluster_proposals.go`: `ClusterThreshold`, `ClusterWindowDays`, and the status constants (`active` / `snoozed` / `resolved`).

**Three management verbs — the wire format enforces the distinction.** These are not interchangeable; the JSON-Schema for each is mutually exclusive (e.g. `snooze_until` is required for `snooze_cluster`, forbidden for `resolve_cluster`). The AST guard rail (Appendix C, Pattern 3) locks the contract.

| Verb | State change | Reversible? | When to use |
|---|---|---|---|
| `snooze_cluster` | `status='snoozed'`, auto-reactivates at `snooze_until` | yes (timer-based) | Known noisy signal that's distracting from real clusters right now |
| `resolve_cluster` | `status='resolved'` permanently | no (forensic-preserving) | Root cause is known and fixed; cluster will never re-surface |
| `annotate_cluster` | none — append-only audit row | n/a | Insight arrives on its own schedule; record it for forensics without changing disposition |

`annotate_cluster` is the unusual one: it **cannot** back-door a reactivation. The DM layer writes only to `system_audit_log`, never to `audit_cluster_proposals.status` / `snooze_until` / `count`. The invariant is enforced at the data layer and tested explicitly. This is the right shape for "I learned something about this cluster later" — the historical insight is preserved without altering the disposition the previous operator chose.

`snooze_until` accepts both Go-relative (`24h`, `7d`, `1h30m`) and ISO 8601 absolute; the DB stores normalized RFC3339 for filter comparison.

**Why three verbs?** Audit trail integrity. The cluster table is a forensic record of "what signals has the system seen and how were they disposed of?" — collapsing snooze and resolve into one verb loses the distinction between "I'm choosing not to see this for now" and "this is closed forever." Annotate preserves historical insight without altering disposition. The asymmetry is deliberate: there is no `reactivate_cluster` tool — resolve is final, and the path back to `active` runs through the underlying cause (which the original operator presumably fixed).

Read-side dedup uses `LIKE '%cluster_key%' ESCAPE '\'` against `memories.content` (free text in pending theories / recent decisions / resolved theories). It catches operators who paste the cluster_key into a theory's rationale without tagging a structured field. The schema has no `cluster_key` column by design — keeping the detection primitive as a side-effect of normal theory/decision writing avoids imposing a tagging convention.

```bash
mpm call list_active_clusters
# → {"known_clusters": [...], "unknown_clusters": [...], "count": {...}}

mpm call read_wake_context
# → "Audit Summary (Last 7 Days):\n- N errors, M warnings logged.\n
#    - Active Clusters (Unknown):\n  * [security] 4 events since 2026-07-02 (ID: security:...)"
```

---

## 11. Glossary

The conceptual vocabulary of MPM. Implementation-specific terms (decay, wake, LTM, recency, challenge lifecycle) are defined where they appear, not here. The glossary is for understanding the architecture, not for looking up every noun.

**Artifact.** A persistent cognitive object: Memory, Decision, Theory, Lesson, or Evidence. Each has its own lifecycle.

**Confidence.** A derived `[0, 1]` estimate of how much the system believes an artifact. Computed from supporting evidence minus contradicting evidence minus time decay. The stored `confidence` column is a cache; the evidence ledger is authoritative.

**Core.** The small, stable part of MPM: Memories, Decisions, Theories, Lessons, Evidence, Confidence. Changes here require demonstrated usage evidence.

**Decision.** A choice with explicit context, the choice made, and the rationale.

**Evidence.** A piece of information that supports or challenges another artifact. Confidence is derived from the evidence ledger.

**Memory.** A general fact, observation, or synthesized insight.

**Runtime.** The evolvable part of MPM: wake, scheduling, personas, modes, directives, routing, audit, review. Changes here are cheap; changes to Core are not.

**Theory.** A testable hypothesis with explicit validation criteria. Theories have lifecycle states: pending → confirmed | disproven.

---

# Appendices

The appendices are part of this document. They hold the implementation detail that contributors and curious readers will reach for. The narrative above stays focused on architecture and philosophy; the details live below.

If you only care about *what* MPM is and *how to use it*, stop at §10. If you are extending MPM, writing tests, or reviewing invariants, continue.

---

# Appendix A: Shared Epistemology Implementation

**Cross-references:** §6.5 Layers 0-3, §10 (audit cluster proposals, where applicable).

This appendix is the deep dive behind §6.5. The narrative above says *what*; this section says *how*. The atomic-epic hashes in each section header let you find the original commit if a future change makes prose stale.

## A.1 Layer 0 — Federation

**Atomic epics:** `6aee1d7` (federation), `61a8418` (FTS sync + singleton), `433dd9f` (architecture split), `c4ba2aa` (singleton migration).

### The two databases

- **Local DB** — `~/.mpm/<workspace>/mpm.db`. Per-project tactical memory.
- **Shared DB** — `~/.mpm/shared/shared.db`. Cross-project house rules, operator-gated.

Each MPM process attaches both via `ATTACH DATABASE '<shared_path>' AS shared`. Cross-DB queries become plain SQL: `SELECT … FROM shared.memories WHERE …`. There is no separate service, no IPC, no serialization layer.

### Shared Premium (the score boost)

```
shared rule match:                score = base × 1.20
shared rule that also matches
  a query_global_rules scan:      score = base × 1.35
local memory:                     score = base × 1.00
```

The boost is multiplicative, not additive. House rules outrank noisy local memories without being able to *invent* matches — the boost only applies to rows that already pass the base relevance threshold. A bad rule can't bury good local facts by sheer score.

### FTS5 sync triggers

Local FTS5 is unaffected. Shared FTS5 (`shared.memories_fts`) is kept in lockstep with `shared.memories` via three triggers:

```sql
CREATE TRIGGER IF NOT EXISTS shared.shared_memories_ai
AFTER INSERT ON shared.memories
BEGIN
    INSERT INTO shared.memories_fts(rowid, content, tags) VALUES (new.rowid, new.content, new.tags);
END;

CREATE TRIGGER IF NOT EXISTS shared.shared_memories_ad
AFTER DELETE ON shared.memories
BEGIN
    INSERT INTO shared.memories_fts(shared.memories_fts, rowid, content, tags) VALUES('delete', old.rowid, old.content, old.tags);
END;

CREATE TRIGGER IF NOT EXISTS shared.shared_memories_au
AFTER UPDATE ON shared.memories
BEGIN
    INSERT INTO shared.memories_fts(shared.memories_fts, rowid, content, tags) VALUES('delete', old.rowid, old.content, old.tags);
    INSERT INTO shared.memories_fts(rowid, content, tags) VALUES (new.rowid, new.content, new.tags);
END;
```

The triggers are idempotent (`IF NOT EXISTS`) and live in the shared DB itself, so a fresh process picks them up at attach time without a separate migration step. The "already exists" warnings on subsequent attaches are harmless.

### The `is_global` column

`shared.memories.is_global` marks rows that originated as shared rules:

- Local writes always set `is_global = 0` (never the local write path).
- `record_global_rule` always sets `is_global = 1`.
- `promote_to_global` flips it on an existing memory.

This is the audit trail: any `is_global = 1` row was either created by the operator (`record_global_rule --confirm=true`) or explicitly promoted (`promote_to_global`).

### Configuration

| Env var | Effect |
|---|---|
| `MPM_SHARED_DB` | Path to the shared DB. If unset or path missing, shared features are disabled (local-only mode). |
| `MPM_SHARED_READONLY` | If `1`, attach in read-only mode. Default: read+write. |

### Concurrency

- **SQLite ATTACH is per-connection.** Each MPM instance attaches its own connection.
- **WAL mode on both DBs** allows concurrent local writers without blocking.
- **Cross-DB transactions are NOT supported.** Every shared write is its own atomic transaction on the shared DB. This is fine because shared writes are append-mostly and operator-gated.
- **`getDB()` singleton** (`c4ba2aa`) means one `DatabaseManager` per process, one ATTACH, one connection, no leak surface.

### What lives in shared vs local

| Lives in shared | Lives in local |
|---|---|
| House rules (cross-project conventions) | Project-specific facts |
| Cross-project decisions | Session state |
| Durable operator rules | Tactical context |
| Resolution/arbitration memory (the audit trail of "what we decided") | Theories tied to local artifacts |
| Federated contradiction log | Workspace-specific lessons |

## A.2 Layer 1 — Shared Vector Search

**Atomic epic:** `2ff0bda` (IVF).

### Why IVF in SQLite, not a vector DB

The shared DB holds house rules. Semantic recall over them is the operator's only escape hatch from "I know the rule exists but I don't remember the exact wording." The choices were:

1. Brute-force cosine over the shared DB.
2. An external vector database (Qdrant, Milvus, etc.).
3. An in-SQLite ANN index.

MPM chose (3). The constraint was to avoid parallel infrastructure — no new service, no second storage surface to back up, no second connection to coordinate. The in-SQLite IVF satisfies that: it lives in `shared.memory_vectors_ivf`, is queried via the same `DatabaseManager` connection, and survives restarts without re-loading.

### The IVF structure

```
nlist  = 16           # number of Voronoi cells
nprobe = 4            # cells visited per query
dim    = 768          # nomic-embed-text output
```

A query:

1. Computes the embedding of the query text (768d).
2. Finds the `nprobe=4` nearest centroids (out of 16 cells) by cosine distance.
3. Returns the union of `postings[centroid]` — the actual memory IDs in those cells.
4. Brute-force cosine within the returned set, sorted descending.

Speedup vs. brute-force: ~3-4× at this corpus size, with negligible recall loss (the nprobe/nlist ratio is tuned to the embedding model + corpus size).

### The VectorMatch circuit breaker

The `VectorMatch` circuit breaker refuses to start a vector scan that would touch more than `nlist × nprobe` centroids. The breaker caps worst-case work and prevents runaway scans if the index is corrupted or the centroids are wildly imbalanced.

## A.3 Layer 2 — Conflict Detection

**Atomic epic:** `3fb2b75` (Arc 1: Conflict Resolution loop).

### The detection surface

`shared.contradiction_log` is the queue. Every shared write that produces a near-miss against an existing memory is logged with:

- `memory_id_a`, `memory_id_b` (the symmetric pair, not winner/loser)
- The provenance link that flagged the near-miss
- The detection timestamp
- The protobuf-shape score for each side
- The resolution status (`pending` / `resolved` / `arbitration_needed`)

The detection runs **inside `query_long_term_memory`'s result set**, not as a background ticker. This is opportunistic: detection happens exactly when an agent is looking at the shared DB anyway, so there is no separate timer, no panic-recovery surface.

### Protobuf-shape scoring

```
score = 0.5·confidence + 0.3·freshness + 0.2·reinforcement
```

Bounded to `[0, 1]`. The 50/30/20 split is intentional:

- **confidence (0.5)** dominates — what the system currently believes the artifact says.
- **freshness (0.3)** catches stale knowledge that hasn't been reinforced. A rule from 2024-01-01 with weight=5 scores lower on freshness than one from 2026-07-07 with the same weight.
- **reinforcement (0.2)** is a tiebreaker for facts that get re-surfaced often.

### The close-call margin

If the two candidates in a near-miss land within `0.10` of each other on the protobuf score, the system **does not** auto-resolve. It writes a `pending` row in `shared.theories` and waits for an operator. The asymmetry is deliberate:

- A clean winner can be auto-resolved (low risk).
- A coin-flip always escalates (high risk, operator judgment required).

## A.4 Layer 3 — Conflict Resolution + Arbitration Closure

**Atomic epics:** `3fb2b75` (Arc 1), `8bfe737` (Arc 1 Closure).

### applyResolution

When a contradiction resolves (either by auto-resolve or operator action), `applyResolution` performs four operations in a single transaction:

1. **Write a resolution memory** — captures the choice and rationale as a new `shared.memories` row.
2. **Mark the queue resolved** — UPDATE the `shared.contradiction_log` row with `resolved_at` and `resolution_memory_id`.
3. **Append evidence to the winner** — INSERT a `shared.evidence` row tagged `type='resolution_survived'`, `strength=1.0`, `source_group='resolution:<queue_id>'`, `independence_factor=1.0`. Asymmetric: only the winner, not the loser.
4. **Tag the loser with arbitration metadata** — patch the loser's metadata with `status='archived'`, `operator_arbitration=true`, and (if applicable) `arbitration_theory=<id>`.

The asymmetric evidence write is the entire reward mechanism. A memory that has survived N contradictions accumulates 1.0×N supporting evidence in its confidence ledger — the system is literally rewarded for being right under pressure.

### ResolveArbitrationTheory (the operator path)

When the operator resolves an arbitration theory via `mpm resolve_theory <id> <conclusion> --winner=<memory_id>`, the system does the same four operations, but:

- The metadata flip is done via `UpdateSharedMemoryMetadata` (not `applyResolution`'s path) because arbitration theories live in `shared.memories`, not in the local theories table.
- The loser is tagged with `operator_arbitration=true` and the originating `arbitration_theory` ID in its metadata.
- The theory status flips to `resolved` (or `disproven` depending on conclusion).

### JSON-merge gotcha

SQLite's `||` operator on JSON columns is **plain string concat**, not JSON merge. The naive `metadata = COALESCE(metadata, '{}') || ?` produces `{"status":"pending"}{"status":"resolved",…}` — invalid JSON, `json_extract` returns the old value.

Fix: read existing metadata, deep-merge in Go (`UpdateSharedMemoryMetadata` helper), write the merged result. Patch keys win on collision. This is the single most important SQLite-specific lesson from the Arc 1 closure; it's reapplied in `broadcast.go` for the same reason.

### The arbitration auto-slash loop

```
operator:  mpm resolve_theory <arbitration_id> <conclusion> --winner=<memory_id>
    ↓
mpm resolve_theory handler
    ↓
ResolveArbitrationTheory(arbitrationID, winnerID, conclusion)
    ↓ (single transaction)
    ├── write resolution memory
    ├── mark queue resolved
    ├── UpdateSharedMemoryMetadata(loser) → status=archived, operator_arbitration=true, arbitration_theory=<id>
    ├── UpdateSharedMemoryMetadata(winner) → confidence bump
    ├── Append resolution_survived evidence row
    └── UpdateSharedMemoryMetadata(arbitration_theory) → status=resolved
    ↓
Auto-broadcast hook (Appendix B, Layer 4)
    ↓
INSERT wake rows for every active session except the broadcaster
```

The loop closes. The auto-broadcast hook is the final step — every other piece of state is updated atomically; the broadcast is a side-effect notification.

## A.5 End-to-end smoke

`scripts/smoke_arc1.sh` (8 steps, all green):

1. Seed two contradictory rules into the shared DB.
2. Run `mpm ops resolve-contradictions` (dry-run) — see the queue.
3. Apply one with `--apply` — auto-resolves, writes resolution memory, asymmetric evidence row.
4. Verify the winner has the `resolution_survived` evidence row.
5. Verify the loser has `status='archived'` and `operator_arbitration=true` in its metadata.
6. Create a close-call contradiction.
7. Run `mpm ops resolve-contradictions` on the close-call — should write a `pending` arbitration theory, not auto-resolve.
8. Operator resolves the arbitration theory with `mpm resolve_theory <id> <conclusion> --winner=<id>` — verifies the auto-slash loop.

## A.6 What "shared" means in practice

A memory is shared if `is_global = 1`. A contradiction is shared if its `memory_id_a` / `memory_id_b` both have `is_global = 1`. A resolution memory is shared if the originating queue was shared. A theory is shared if it was created from a shared contradiction.

The asymmetry: local contradictions are NOT written to `shared.contradiction_log`. They stay in the local theories table and are surfaced via local wake context. The shared log is exclusively for cross-project house rules, where one agent's local belief might contradict another's — that's the case that needs federated resolution.

---

# Appendix B: Arc 2 (Active Dissemination) Implementation

**Cross-references:** §6.5 Layer 4, §9 "Event Wakes."

This appendix is the runtime deep dive behind Arc 2. The narrative above says *what* the layer does; this section says *how* it does it.

Arc 2 is shipped at `efec046` (2026-07-07). All smoke tests + unit tests green. The pre-implementation design rationale is preserved in the commit message and the `arc-2-design.md` history entry (no longer a living document — superseded by this appendix).

## B.1 Schema (as-built)

```sql
-- Active-agent registry. Every MPM call silently bumps last_heartbeat.
CREATE TABLE IF NOT EXISTS shared.bcast_sessions (
    session_id      TEXT PRIMARY KEY,
    agent_id        TEXT NOT NULL,
    hostname        TEXT,
    first_seen_at   TEXT NOT NULL,
    last_heartbeat  TEXT NOT NULL,
    total_calls     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS shared.idx_bcast_sessions_heartbeat
    ON bcast_sessions(last_heartbeat);

-- The fan-out surface. PRIMARY KEY on wake_id → O(1) dedup.
CREATE TABLE IF NOT EXISTS shared.bcast_event_wakes (
    wake_id         TEXT PRIMARY KEY,    -- sha256(memory_id + ":" + target + ":" + content_hash)[:12]
    target_session  TEXT NOT NULL,
    source_agent    TEXT NOT NULL,
    memory_id       TEXT NOT NULL,
    kind            TEXT NOT NULL CHECK(kind IN ('rule','resolution','arbitration','memory')),
    content_hash    TEXT NOT NULL,
    rationale       TEXT NOT NULL,
    created_at      INTEGER NOT NULL,
    fired           INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS shared.idx_bcast_wakes_target
    ON bcast_event_wakes(target_session, fired);
CREATE INDEX IF NOT EXISTS shared.idx_bcast_wakes_memory
    ON bcast_event_wakes(memory_id, created_at);

-- Cross-reboot identity cache.
CREATE TABLE IF NOT EXISTS shared.bcast_agents (
    agent_id            TEXT PRIMARY KEY,
    first_seen_at       TEXT NOT NULL,
    last_session_id     TEXT,
    total_heartbeats    INTEGER NOT NULL DEFAULT 0,
    total_broadcasts    INTEGER NOT NULL DEFAULT 0
);
```

**Why the `bcast_` prefix.** `internal/core/schema.go` installs a local `sessions` table into the shared DB on attach via `rewriteTablePrefix(ddl, "shared.")`. Renaming Arc 2's tables with the `bcast_` prefix avoids a silent collision (CREATE TABLE IF NOT EXISTS would have been a no-op for the second one).

## B.2 The wake_id math

```
wake_id       = sha256(memory_id + ":" + target_session + ":" + content_hash)[:12]
content_hash  = sha256(content + "|" + sorted(tags))
```

- **48 bits of entropy** (`12 hex chars × 4 bits = 48 bits`) → ~10¹⁴ unique IDs before 50% collision probability (birthday bound).
- **PRIMARY KEY, not UNIQUE INDEX** → `INSERT OR IGNORE` is the dedup mechanism at the storage layer. No app-level check, no TOCTOU race.
- **content_hash includes only (content + sorted tags).** A rule body text change → new content_hash → new wake_id → new wake fires. A metadata-only update (e.g., `status=challenged`) does NOT change the content_hash, so re-broadcasts are no-ops. This matches the rule that the receiver doesn't need to re-hear the same content because of bookkeeping churn.

## B.3 Fan-out algorithm (Go-side)

```
func BroadcastMemory(memoryID string, opts BroadcastOpts) (BroadcastReport, error):
    1. Resolve memory: SELECT content, tags FROM shared.memories WHERE id = memoryID
    2. Compute content_hash = sha256(content + "|" + sorted(tags))
    3. Discover active sessions:
         a. With opts.ToAgents: lookup agent_id → session_id pairs; refuse any offline target
         b. Without:           SELECT session_id, agent_id FROM shared.bcast_sessions
                                 WHERE last_heartbeat > now - 24h
    4. For each active target:
         a. If target.session_id == opts.SourceSessionID: continue  (self-skip)
         b. wake_id = sha256(memoryID + ":" + target.session_id + ":" + content_hash)[:12]
         c. INSERT OR IGNORE INTO shared.bcast_event_wakes (..., fired=0)
         d. If rows affected == 0: deduped++; else new++
    5. Return BroadcastReport{MemoryID, Kind, ContentHash, Targets, NewWakes, DedupedWakes}
```

## B.4 Auto-broadcast hooks

Three operator commands trigger auto-broadcast as a strict side-effect of an explicit gate. **All three are non-fatal**: a broadcast failure does NOT roll back the underlying write. The resolution is ground truth; the broadcast is the notification.

| Command | What auto-broadcasts | Implementation site |
|---|---|---|
| `mpm ops record_global_rule --confirm=true` | The new rule (kind=rule) | `internal/core/tools/handlers.go::handleRecordGlobalRule` |
| `mpm ops resolve-contradictions --apply` | The resolution memory (kind=resolution, source=arc1-resolver) | `internal/core/contradiction_log.go::ResolveOneContradiction` |
| `mpm resolve_theory <id> <conclusion> --winner=<id>` | The arbitration verdict (kind=arbitration, source=arc1-arbitrator) | `internal/core/contradiction_log.go::ResolveArbitrationTheory` |

Internal agent writes (without an operator gate) do NOT trigger broadcasts. The asymmetry is deliberate — auto-broadcasts are a notification, not a control surface.

## B.5 Receive path

```
mpm call check_pending_event_wakes --payload '{"session_id":"..."}'
```

Mirrors `check_wakes`:

```sql
WITH pending AS (
    SELECT wake_id, ... FROM shared.bcast_event_wakes
    WHERE target_session = ? AND fired = 0
    ORDER BY created_at ASC LIMIT 100
)
UPDATE shared.bcast_event_wakes SET fired = 1
WHERE wake_id IN (SELECT wake_id FROM pending)
RETURNING *;
```

The CTE-updating-read pattern is atomic — concurrent callers see disjoint wake_id sets.

**Opportunistic fold.** `cmd/mpm/call.go` calls `CheckPendingEventWakes(ac.SessionID)` after every handler returns and folds the result into the `EventWakesPending` block on the response. So every MPM call surfaces pending event wakes automatically, without an explicit pull.

## B.6 Wake payload shape

| Field | Meaning |
|---|---|
| `wake_id` | Deterministic sha256 prefix — receiver uses for idempotency |
| `source_agent` | Who broadcast (operator + agent ID) |
| `memory_id` | What to fetch — receiver does its own `mpm query memory <id>` if it needs the full text |
| `kind` | `rule` / `resolution` / `arbitration` / `memory` |
| `content_hash` | Receiver-side drift detection |
| `rationale` | The WHY (non-negotiable) |
| `created_at` | Unix epoch |

The full memory content is NOT in the wake — signal, not transport. 100-target fan-out ≈ 100KB total, not 10MB. The receiving agent does its own `mpm query memory <id>` if it needs the full text. If we ever need a "firehose" mode, that's a separate `--include-content` flag and a separate review.

## B.7 Tests (12 unit tests, all green)

- Heartbeat lifecycle: `TestHeartbeat_FirstCallInserts` / `TestHeartbeat_SecondCallBumps` / `TestHeartbeat_ZeroHeartbeats`
- Discovery: `TestDiscoverActiveSessions_FiltersExpired` — 24h window
- Dedup: `TestBroadcastMemory_DeterministicID` — same input → same wake_id, dedupe counter
- Drift: `TestBroadcastMemory_DifferentContent` — content change → new wake_id
- Targeting: `TestBroadcastMemory_TargetedFanout` / `TestBroadcastMemory_RejectsOfflineTargets` — `--to` semantics
- Pickup: `TestCheckPendingEventWakes_MarksFired` — atomic pickup + idempotency
- Dry-run: `TestBroadcastMemory_DryRun` — populates report, writes 0 rows
- Rationale: `TestBroadcastMemory_AutoExtractResolutionRationale` — kind=resolution auto-builds rationale
- Validation: `TestBroadcastMemory_RejectsNoRationale` — kind=memory without `--rationale` refused
- Self-skip: `TestBroadcastMemory_SkipsSelf` — source session not in target list

End-to-end: `scripts/smoke_arc2.sh` (6 steps: heartbeat → seed → broadcast → dedup → cross-agent → pickup).

## B.8 SQLite limitations discovered

Documented in the Arc 2 implementation lesson:

1. `CREATE INDEX shared.idx ON shared.table(col)` is rejected when shared is attached ("near '.': syntax error"). Workaround: prefix INDEX NAME with `shared.`, use bare table name.
2. `CREATE TABLE IF NOT EXISTS shared.X` is ACCEPTED (the opposite of indexes — easy to confuse).
3. `FOREIGN KEY ... REFERENCES shared.X` is rejected with the same syntax error. Workaround: drop the FK, treat as advisory.
4. `sessions` and `agents` collide with the local BaseTables. The `bcast_` prefix avoids this.
5. SQLite `||` is plain string concat, not JSON merge. Already documented from Arc 1 closure — reapplied in `broadcast.go` for metadata writes.

## B.9 Operational notes

- **Cross-DB transactions are NOT supported.** Each `INSERT OR IGNORE` is its own transaction on the shared DB.
- **WAL mode on both DBs** allows concurrent local writers without blocking.
- **Heartbeat is passive.** Every MPM call writes a heartbeat via `cmd/mpm/call.go`'s pre-handler. Optional `mpm ops heartbeat` exists for tooling that doesn't run agents.
- **Cold-start does NOT replay old wakes.** `query_long_term_memory` handles history; event wakes are real-time push only.
- **Self-skip is by session_id, not agent_id.** An agent running in multiple sessions gets the wake in each one (except the broadcasting one).

---

# Appendix C: Enforcement Patterns

**Cross-references:** §3.2 (Design Principles), §6.2 (Confidence Engine), §10 (Reliability, audit cluster proposals, self-heal).

The README states invariants in prose. This appendix is where the enforcement patterns live. Every claim of "this is guaranteed" in MPM maps to one of the patterns below. Once you have internalized these, the architecture reads differently — every "X is enforced" statement in the body of the document is one of these four patterns in a costume.

## C.1 Pattern 1: Source-of-truth + derived cache

**The principle:** If two values could disagree, exactly one of them is authoritative. The other is a derived cache that can be regenerated.

**The canonical example — confidence.**

The `confidence` column on `memories` is a performance cache. The evidence ledger (`shared.evidence` + `local.evidence`) is the source of truth. If they disagree, the cache is wrong.

How this is enforced:

- `add_evidence` recomputes confidence in the same transaction as the evidence INSERT. If recompute fails (e.g., CHECK constraint on `confidence_history.trigger`), the entire transaction rolls back — no orphan evidence rows, no stale confidence.
- `recompute_confidence` triggers a manual recompute from the evidence ledger.
- `query_confidence_history` derives its view from evidence, not the cached column.

**The invariant:** `confidence = f(evidence, decay) at most-recent recompute`.

## C.2 Pattern 2: Property tests over implementation tests

**The principle:** Where the formula drifts but the meaning is fixed, test the meaning, not the formula.

**Why this matters.** If a property test says "decay never increases confidence" and you change the decay function from `linear` to `exponential`, the property test stays green. If a unit test says `assert(decay(0) == 1.0)`, it breaks the moment you tune the curve. The properties are forever-true; the formulas are forever-drifting.

**MPM properties pinned this way:**

- More positive evidence never decreases confidence.
- Decay never increases confidence.
- Cached confidence always matches `f(evidence, decay)` at most-recent recompute.
- `mpm ops gc` never shreds a memory with `weight >= 0` (regardless of the threshold).
- The decision ledger is append-only (no UPDATE on `record_decision` rows after insert).

The confidence properties are pinned in `internal/core/confidence_test.go`. When the formula changes, the tests stay green and the meaning survives.

## C.3 Pattern 3: AST guard rails

**The principle:** When a contract spans a JSON-Schema and a handler, the AST enforces it. A test asserts that the JSON-Schema is a strict superset of the keys the handler reads — so the schema can never lie about what the handler will accept.

**The canonical example — audit cluster verbs.**

`snooze_cluster`, `resolve_cluster`, and `annotate_cluster` are three different verbs with three different state-transition semantics. A buggy JSON-Schema (e.g., accepting `snooze_until` on `resolve_cluster`) would let the caller invoke a snooze while thinking they're resolving. The wire format is the safety boundary.

How this is enforced:

- `TestSchemaSupersetOfHandlerPayloadReads` walks every tool's JSON-Schema and every handler's payload reads, asserting the schema is a strict superset of the handler's keys.
- Schema validation at the boundary rejects malformed payloads before they reach the handler.

**Why this matters more than the example.** A reader of the audit-cluster code can reason about each verb in isolation; the schema validation is the thing that prevents the verbs from accidentally being confused. The contract is encoded in the type system, not the prose.

## C.4 Pattern 4: Self-heal whitelist + escalation

**The principle:** Auto-fix only what's known-safe. Escalate everything else.

**The canonical example — self-heal drift classification.**

`mpm ops self-heal` runs `runDeepScanCheck` and classifies drift into three classes:

| Class | Detection query | Behavior |
|---|---|---|
| Soft-delete ghosts | `memories_fts` rows whose joined `memories` row has `deleted_at IS NOT NULL` | **Auto-fix** (known-safe) |
| FTS orphans | Any `*_fts` row whose rowid is missing from the source table | **Escalate** (unknown) |
| Dangling memberships | `topic_memberships` referencing non-existent topics | **Escalate** (unknown) |

**Four safety boundaries on auto-fix:**

1. **Whitelist-only** — only soft-delete ghosts are auto-fixed.
2. **Bounded blast radius** — if ghost count exceeds `SelfHealMaxFix` (default 1000), refuse to auto-fix and escalate.
3. **Rate-limited** — 24h cooldown between same-signature auto-fixes.
4. **Audit trail** — every auto-fix writes a `lessons` table entry tagged `source=self-heal`.

**Why the asymmetry.** Soft-delete ghosts are the only drift class where we have a precise model of the correct state AND a proven-safe mechanism that should have produced that state. For FTS orphans and dangling memberships, we cannot tell from inside the scan whether the FTS row is the bug or the source-table row is the bug. Auto-fixing novel drift is exactly the kind of guesswork that causes data corruption. Escalate.

The cognitive loop closes through the existing wake context: when self-heal escalates unknown drift, it injects a pending theory; the next `mpm call read_wake_context` surfaces it; the agent acts on it. **No new wake code is needed.**

## C.5 When to use which pattern

| Invariant type | Use |
|---|---|
| Value is derivable from another value | Pattern 1 (source-of-truth + cache) |
| Property holds across all formulas, current and future | Pattern 2 (property tests) |
| Contract spans JSON-Schema and handler | Pattern 3 (AST guard rail) |
| Drift detected in the substrate | Pattern 4 (whitelist + escalate) |

**Pattern 1 + Pattern 2 together:** confidence is a derived cache (Pattern 1), and the formula that derives it is pinned by property tests, not unit tests (Pattern 2). The combination means: even if someone rewrites the entire confidence engine, the system stays correct.

**Pattern 3 + Pattern 4 together:** audit clusters are protected from schema confusion (Pattern 3) and self-heal is protected from guesswork (Pattern 4). Both patterns reject the "let the system figure it out" trap.

## C.6 What these patterns reject

- **Auto-fix by inference.** "If the FTS row is missing, the source table must be the bug" — sometimes true, sometimes catastrophic. Pattern 4 rejects this.
- **Lying schemas.** A JSON-Schema that accepts `snooze_until` on `resolve_cluster` would let callers confuse the two. Pattern 3 rejects this.
- **Stale caches as truth.** A `confidence` column that disagrees with the evidence ledger is, by definition, a bug. Pattern 1 rejects this.
- **Brittle unit tests on forever-drifting formulas.** A test that pins `assert(decay(0) == 1.0)` breaks the moment the formula changes. Pattern 2 rejects this.

The discipline is the same in every case: when in doubt, escalate. The agent's wake context is the escalation surface. The user's attention is the final arbiter.

---

## License

See [LICENSE](LICENSE).
