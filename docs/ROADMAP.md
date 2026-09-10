# MPM Roadmap

> Post-alpha roadmap for MPM, the persistent cognitive substrate for long-lived AI agents.

MPM's alpha release establishes the core substrate, agent integrations, persistence model, provenance, wake/context loop, evidence model, and CLI/tool surfaces.

The next phase should focus on **observability, analysis, and usability**, without destabilising the substrate that has just reached alpha.

---

## 1. Post-alpha telemetry

### Goal

Turn MPM's existing operational and cognitive metadata into a useful telemetry and analysis layer.

Telemetry remains deliberately outside the core substrate database.

The telemetry binary and sidecar database should capture enough structured information to answer questions about:

* what agents are doing
* what MPM is storing
* how memory evolves
* how knowledge is retrieved
* how work progresses
* which frameworks/models contribute to that activity
* where system and execution costs occur

### Initial telemetry surface

Capture and aggregate metadata around:

**Memory lifecycle**

* creation
* updates
* revisions
* reinforcement
* weakening
* stale transitions
* shredding
* collection
* tags
* scope
* provenance
* confidence/history changes

**Retrieval**

* queries
* result counts
* retrieval scores
* retrieval rationale
* projection level
* memory age
* weight at retrieval
* collection/framework hit rates

**Agent execution**

* framework
* model
* invocation ID
* parent invocation ID
* session/work context
* tool invocation
* reasoning/thinking metadata where available
* temperature/max-token configuration where available

**Work and epistemic activity**

* work creation/completion
* verification state
* evidence usage
* decisions
* lessons
* theories
* contradictions
* supersession
* cognitive-state transitions

### Design principle

Telemetry should measure **meaningful agent behaviour and intellectual progress**, not simply produce larger piles of counters.

---

## 2. Cognitive and memory analytics

Build derived metrics on top of telemetry and substrate state.

Initial analysis should investigate:

### Memory durability

* memories created per unit time
* proportion subsequently retrieved
* reinforcement/weakening trajectories
* stale-memory rate
* revision frequency
* memory survival/lifecycle distributions

### Memory usefulness

Estimate relationships such as:

```text
created
  ↓
retrieved
  ↓
used
  ↓
reinforced
  ↓
revised / superseded / stale
```

The goal is to distinguish **stored information** from **knowledge that actually contributes to later work**.

### Retrieval quality

Analyse:

* retrieval success/failure
* relevance distributions
* score/rationale patterns
* collection effectiveness
* projection economics
* framework-specific retrieval behaviour

### Agent comparison

Where provenance data permits, compare:

* frameworks
* models
* sessions
* workloads

without turning the telemetry layer into a simplistic leaderboard.

The useful question is not merely “which model does more?” but “which behaviour produces more durable, useful cognitive state?”

---

## 3. Cognitive-state visualisation

Create visualisations of MPM's persistent knowledge and work lifecycle.

Potential views include:

### Knowledge lifecycle

```text
Memory
  ↓
Retrieval
  ↓
Use
  ↓
Reinforcement
  ↓
Revision
  ↓
Lesson / Decision / Theory
  ↓
Evidence
  ↓
Verified / Contradicted / Superseded / Stale
```

### Provenance view

Show how memories, decisions, lessons, theories and work relate to:

* framework
* model
* invocation
* session
* evidence

### Temporal view

Explore how the substrate changes over time:

* memory growth
* cognitive-state changes
* work completion
* stale/revised knowledge
* retrieval activity
* agent activity

The emphasis should remain on **intellectual progress**, not activity volume.

---

## 4. CLI usability

Continue evolving the CLI as the canonical operational interface.

Potential post-alpha improvements:

* clearer human-oriented views
* richer summaries
* improved filtering and grouping
* easier inspection of memory/work/evidence relationships
* safer bulk operations
* better diagnostics
* improved export/reporting

The CLI remains the authoritative interaction model.

Any future UI should expose the same underlying operations rather than creating a competing application contract.

---

## 5. Web interface

Once the telemetry/data model has matured, provide an optional local web interface.

The web interface should initially be a **presentation and interaction layer over MPM**, not a replacement for the substrate.

Potential first views:

### Overview

* current cognitive state
* recent work
* recent memories
* pending/stale items
* system health

### Memory explorer

* search
* filters
* tags
* collections
* provenance
* confidence/weight history
* revisions
* lifecycle

### Work explorer

* active/completed work
* verification state
* evidence
* decisions
* lessons
* theories

### Retrieval explorer

* recent searches
* retrieval scores
* selected results
* projection behaviour
* diagnostics

### Telemetry dashboard

* agent activity
* memory lifecycle
* retrieval behaviour
* execution economics
* framework/model comparisons
* trends over time

The UI must respect MPM's existing security and permission boundaries.

No external service should be required for the default local deployment.

---

## 6. API / service boundary

If the web interface requires a service layer, define a small, explicit local API over existing MPM operations.

The API should:

* reuse core semantics
* preserve existing authorization/security boundaries
* avoid duplicating business logic
* remain optional
* work without making the web UI a requirement for agents

The CLI, MCP surface and web interface should converge on the same underlying operations rather than developing three subtly different versions of MPM.

---

## 7. Telemetry-derived research questions

The telemetry phase should also be used to answer questions that cannot be answered from raw MPM records alone.

Examples:

* Which memories become durable?
* Which memories are repeatedly retrieved but never reinforced?
* Which types of work generate the most reusable knowledge?
* How often does new evidence change an existing belief?
* How frequently are decisions superseded?
* Which retrieval patterns correlate with successful work?
* How much cognitive state is created versus actually used?
* Where does agent activity produce diminishing returns?
* How do different frameworks/models affect memory quality and persistence?
* What does “intellectual progress” look like measurably?

These questions should drive telemetry design, rather than adding fields simply because they are available.

---

## 8. Security and privacy

Telemetry and the web interface must preserve the confidentiality assumptions already established by MPM.

Requirements include:

* local data remains local by default
* telemetry does not unintentionally duplicate sensitive cognitive content
* access controls remain aligned with substrate boundaries
* filesystem permissions remain restrictive
* sensitive fields are minimized in telemetry where full content is unnecessary
* exports are explicit and deliberate
* audit/history operations remain distinguishable from ordinary usage

Particular care should be taken because telemetry can become a secondary copy of the very cognitive data MPM is designed to protect.

---

## 9. Release sequencing

### Alpha release

Focus:

* core stability
* CLI correctness
* agent integration
* persistence
* wake/context
* provenance
* installation
* security
* documentation

**Telemetry is intentionally out of scope.**

### Post-alpha phase 1

* finish telemetry binary
* establish telemetry schema
* define useful metrics
* validate collection overhead
* build initial reports/queries

### Post-alpha phase 2

* cognitive/memory analytics
* retrieval analytics
* provenance analytics
* lifecycle visualisation

### Post-alpha phase 3

* local web interface
* API/service layer where needed
* interactive memory/work exploration
* telemetry dashboards

### Later

* richer cognitive analytics
* comparative framework/model analysis
* advanced graph views
* export/reporting
* additional visualisation and research tooling

---

## 10. Guiding principle

MPM should not become a dashboard product that happens to store agent memory.

The substrate remains the centre of the system.

The purpose of telemetry, analytics, and a future web interface is to make visible and useful the information MPM already captures about:

**memory, work, provenance, evidence, retrieval, and intellectual progress.**

The core remains small, local, persistent, and agent-facing.

The observability layer can become rich without making the substrate itself complicated.

