# Phase 3 Design Brief: Adaptive Retrieval & Progressive Materialization

**Date:** 2026-08-21
**Status:** Design — not implemented

---

## Architectural Sentence

> **A pointer identifies an artifact; a retrieval request specifies how much of that artifact may be materialized.**

> **Representation is consumer policy. Identity is substrate state.**

---

## Context: Where Phase 2 Left Us

Phase 1 created the pointer transport (blob spill on oversize).
Phase 2 made projections pointer-native (`mpm://memory/<id>`, `mpm://lesson/<id>`, `mpm://theory/<id>`).

The system now has a two-stage representation model:

```
                 ┌── inline bounded projection
Tool / query ────┤
                 └── pointer ──► resolver ──► bounded materialization
```

`mpm://<kind>/<id>` is becoming a genuine substrate primitive. Phase 3 builds on this.

---

## Architectural Invariant

**The pointer identifies the artifact, not the representation requested by the consumer.**

```
mpm://<kind>/<id>
        │
        ▼
    artifact
```

The pointer does **not** identify:
- the requesting agent
- the model
- the session
- the context window
- the reasoning mode
- the retrieval budget
- a particular projection
- a particular serialization

Those belong to the **retrieval request**, not the pointer.

```
Pointer     = identity
Request     = policy
Resolution  = materialization
```

This is desirable:
```
mpm://memory/abc123
        │
        ├── budget=256  → short summary
        ├── budget=1024 → larger materialization
        └── budget=8192 → deeper materialization
```

This is undesirable — do not do this:
```
mpm://memory/abc123?agent=foo&budget=256&projection=decision
```

The latter makes representation part of identity. Representation belongs to the request.

---

## Phase 3A: Progressive Materialization

Replace the simplistic model:
```
max_bytes = N
```
with a budget-aware model:
```
budget_bytes = how much can I afford to materialize?
```

The API can still accept a concrete byte ceiling internally. The distinction is semantic:

```json
{
  "uri": "mpm://memory/abc123",
  "budget_bytes": 2048
}
```

Potential response:
```json
{
  "pointer": "mpm://memory/abc123",
  "bounded": true,
  "content": "...",
  "materialized_bytes": 1987,
  "remaining": true
}
```

### bounded is not truncation

A 256-character summary is not a failed retrieval. It is a **representation choice**.

Distinguish clearly:

| Concept | Meaning |
|---------|---------|
| `projection` | Deliberate representation (e.g. summary) |
| `materialization` | Reading the full or partial artifact content |
| `truncation` | Materialization that hit a ceiling |

`bounded=true` currently carries all three meanings. Phase 3 should split these.

---

## Phase 3B: Retrieval Policy

The existing telemetry (`RecordRetrieval`, `RecordRetrievalSuccess`, `retrieval_metadata`) gives Phase 3 something unusual:

```
artifact
   ↓
retrieval
   ↓
outcome
```

The system can observe patterns:
```
memory A: retrieved 17 times, successful use 16 times
memory B: retrieved 14 times, successful use 1 time
```

**Hard rule:** Telemetry may influence retrieval policy, but never artifact truth.

Retrieval history can answer "how aggressively should I materialize this?" It cannot answer "is this memory true?"

---

## Phase 3C: Progressive Retrieval Semantics

Simple progression:

```
                  ┌──────────────┐
                  │   Pointer    │
                  └──────┬───────┘
                         │
                  cheap materialization
                         │
                         ▼
                  ┌──────────────┐
                  │   Summary    │
                  └──────┬───────┘
                         │
                  agent decides
                         │
              ┌──────────┴──────────┐
              │                     │
           sufficient            inspect
              │                     │
              ▼                     ▼
             done              more budget
                                    │
                                    ▼
                              deeper content
```

The agent decides when to cross the boundary. MPM does not automatically materialize the entire artifact.

---

## Phase 3D: Context-Budget Abstraction

**Do not immediately create a complex token-budget framework.**

Too many variables: model tokenizer, MCP serialization, JSON overhead, framework limits, reasoning budgets, prompt overhead.

Phase 3 establishes:
```
materialization budget
```
with bytes as the implementation unit. Framework adapters translate later:
```
remaining context
        ↓
recommended materialization budget
        ↓
mpm_resolve
```

This keeps the core substrate model-independent.

---

## Phase 3E: Representation Independence

Given `mpm://memory/abc123`, the artifact identity is invariant. The following may legitimately vary:
- summary length
- materialization ceiling
- serialization
- retrieval metadata inclusion

But the pointer itself does not vary.

```
               SAME ARTIFACT
                    │
        ┌───────────┼───────────┐
        ▼           ▼           ▼
      Agent A     Agent B     CLI
      budget 512  budget 2K   full
        │           │           │
        ▼           ▼           ▼
     summary     detail       content
```

The right kind of adaptability: **policy at the edge, identity in the substrate**.

---

## Phase 3F: What Telemetry Can Actually Do

### Level 1: Observability (exists)
- retrieval count
- success count
- last retrieval

### Level 2: Advisory Policy (Phase 3 target)
```
high-success artifact → potentially larger initial projection
low-success artifact  → conservative projection
```
Initially deterministic and explainable. Not opaque.

### Level 3: Adaptive Policy (later)
```
retrieval history
     ↓
policy model
     ↓
materialization recommendation
```
**Do not implement Level 3 in the first Phase 3 release.** Establish the policy interface first.

---

## Non-Goals

Explicitly carry forward:
- No new pointer kinds without a demonstrated cognitive primitive
- No agent-specific pointer identities
- No pointer query parameters encoding representation
- No automatic mutation of artifact truth based on retrieval telemetry
- No model-specific tokenizer dependency in `internal/core`
- No opaque adaptive retrieval algorithm in the first implementation
- No background "smart retrieval" daemon
- No replacement of existing full-content retrieval semantics
- No removal of Phase 1 blob fallback
- Phase 3 does not make MPM autonomous about what the agent should know

---

## Acceptance Criteria

### Identity
```
same pointer → same authoritative artifact
```
regardless of consumer or budget.

### Boundedness
No materialization request can bypass its server-side ceiling.

### Progression
```
pointer → summary → deeper materialization
```
without obtaining a different artifact identity.

### Determinism
Given `artifact + budget + policy version`, the same materialization decision is reproducible.

### Telemetry Separation
Retrieval telemetry can affect **policy**, but never artifact contents or epistemic state.

### Consumer Neutrality
The core pointer remains consumer-neutral.

### Backwards Compatibility
Existing tools (`mpm_resolve`, `mpm_blob_read`, `mpm_memory`, `mpm_lessons`, `mpm_context`) continue to work under their established contracts.

---

## The Three-Phase Progression

```
Phase 1
Protect the boundary
        │
        ▼
Phase 2
Make projections pointer-native
        │
        ▼
Phase 3
Make materialization budget-aware
        │
        ▼
Phase 4+
Use retrieval experience to improve policy
```

Do not let Phase 3 quietly become "build an intelligent retrieval system." The architectural achievement is: establish a stable, policy-independent identity layer and a bounded, progressive materialization protocol. Once that exists, the adaptive machinery has somewhere sane to live.
