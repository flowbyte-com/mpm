# CLI Refactor RFC — Cognitive Interface (No Substrate Changes)

**Status:** ✅ GREENLIT — staged rollout, 4 waves, approved 2026-07-29 by v
**Origin:** Architectural design thread between v and 808 on 2026-07-29, formalising the conversation into an RFC after agreement on every principle.
**Supersedes:** None — this is new RFC territory. (The watcher deprecation arc in commits e1bc707 (2026-06-26) and e824ebc/34c57f6 (2026-07-29) cleared the surface first; this RFC operates on the cleared surface.)
**Discussions / memories it formalises:**
- `memory:a65461cc0394d7be` — two-audience principle
- `memory:0598792a1abf5a21` — discoverability over reduction
- `memory:424cf72798e3435d` — introspection as differentiator
- `memory:102ad2d82a751249` — two interface personalities over one engine
- `memory:7e0c8a91` — composition discipline (human-mode commands compose, never duplicate)
- `memory:f1a6b3c4` — cognitive-system tiebreaker (does this make MPM feel more like a cognitive system?)
**Goal:** Refactor the MPM CLI so that it reflects the cognitive architecture rather than the storage architecture. No substrate redesign. No MCP redesign. Primarily interface and discoverability changes. Maximum reuse of existing handlers and routing.

---

## Objective

Refactor the MPM CLI so that it reflects the cognitive architecture rather than the storage architecture.

This is not a substrate redesign.
This is not an MCP redesign.
This is primarily an interface and discoverability refactor.

The implementation should maximise reuse of existing handlers and routing.

The expected implementation should mostly consist of aliases, routing, help improvements, and thin orchestration commands.

## Architectural Principles

These principles override implementation convenience.

### 1. Stable contracts

MCP tool names are API contracts.

Examples:

- `record_decision`
- `save_lesson`
- `save_skill`
- `read_wake_context`
- `flush_scratchpad`

These MUST remain stable.

Do not rename MCP tools.

Compatibility is more important than aesthetics.

### 2. Intent at the CLI

Humans express intentions.

Agents integrate against contracts.

The CLI should therefore expose human intentions.

Examples:

- `mpm remember`
- `mpm continue`
- `mpm recall`
- `mpm why`
- `mpm doctor`

Internally these dispatch to existing MCP handlers.

### 3. One engine, two interfaces

The CLI now has two personalities.

**Human interface**

Small. Discoverable. Intent-driven. Daily use.

**Operator interface**

Explicit. Scriptable. Complete. Stable.

Both interfaces dispatch to the same implementation.

Do not duplicate business logic.

### 4. Progressive disclosure

The number of commands is NOT the problem.

Discoverability is.

Default help should expose approximately 8 primary commands.

Everything else should remain accessible via:

```
mpm help <section>
mpm help --all
```

Help output is **not** an API. Operators who want scriptable command discovery use `mpm help --json` (or, if added later, `mpm commands`). We do **not** maintain a `--legacy` / `--compat` flat-help mode to mirror the pre-RFC help shape — that locks us into drift. New help is the canonical surface; tooling queries it through the JSON boundary.

### 5. Prefer aliases over renames

Do not break existing workflows.

If an improved command name exists:

- Add it.
- Do not remove the old one.

Old commands remain documented under the operator interface.

### 6. Composition discipline

Human commands must compose existing queries, never duplicate them.

`mpm continue` does not contain its own queries for working context, recent decisions, or loaded skills. It calls the existing `wake`, `read_scratchpad`, `query_decisions`, and `list_skills` paths.

If a future contributor starts reimplementing those queries inside `mpm continue` (or any other human-mode command), stop them.

Composition is one of MPM's strongest architectural traits. Duplicated query logic is how cognitive substrate code rots silently — the CLI and the substrate diverge on what "the same data" looks like.

When the architecture has reached coherence, new contributors should be guided to extend the substrate via composition, not reimplementation.

### 7. Commands are adapters, not dependencies

The CLI internal architecture mirrors the MCP-vs-substrate separation discipline that produced the architecture split in commit eb77078 (2026-07-07).

**Layered stack:**

```
SQLite
   ↓
Stores                ← query types wrapping SQLite. One per major domain
                          table cluster (working context, wake context,
                          memories, decisions, skills, theories, status).
                          They own ONLY queries; they own no behaviour.
   ↓
Services              ← behavioural units. Earn their existence by owning
                          behaviour that crosses stores or has rules
                          (validation, expiry, orchestration, aggregation).
                          Examples below.
   ↓
Formatters            ← pure data transformation. Reshape a model into a
                          presentation-shaped form (e.g. WorkingContext →
                          DashboardSection). Stateless.
   ↓
Encoders              ← serialisation. JSONEncoder, YAMLEncoder. NOT
                          rendering — they produce machine-readable output
                          for `--json` / scripting pipelines.
   ↓
Renderers             ← output-medium adapters. TerminalRenderer,
                          DashboardRenderer. They consume Formatters
                          (or models directly when no reshape is needed)
                          and write to the terminal.
   ↓
Commands              ← tiny adapters (~30 LOC each). Compose one or
                          more services, hand off to one or more
                          formatters / renderers / encoders.
                          NEVER call another command.
                          NEVER own SQL.
                          NEVER own behaviour (that is what services
                          are for).
                          NEVER own presentation (that is what
                          formatters and renderers are for).
```

**Services earn their existence.** A service that simply wraps a store — `TheoryService{repo.ListRecent()}` — is accidental complexity, not architecture. Removing the service layer and inlining the store call at every call site would lose nothing. That's the test: if the service disappears cleanly with no behaviour loss, the service shouldn't exist.

Real services own real behaviour:

- **WorkingContextService** — `Load()`, `Validate(state)`, `Expire()`, `Promote()`, `Clear()`. Behaviour that spans persistence, validation, lifecycle, and policy. Earns its existence.
- **ContinueService** — `Compose(sectionOwners...)` orchestrates five subsystems to assemble `continue`'s dashboard. Earns its existence (composition IS behaviour).
- **DoctorService** — `Check()` aggregates six+ telemetry paths into one trust signal. Earns its existence (aggregation IS behaviour).

NOT real services — just store wrappers, do not create:

- TheoryService that just calls `repo.ListRecent()` — that's a `TheoryStore`.
- SkillService that just calls `repo.ListByStatus()` — that's a `SkillStore`.
- Anything of the shape `type FooService struct{}; func (s *FooService) Get() { return s.repo.Get() }`.

Stores are fine. Services must pay for themselves through behaviour.

**The general rule:**

> *Commands do not compose commands. Commands compose services.*

**Two anti-patterns that this architecture forecloses:**

1. **The Unix trap.** `exec.Command("mpm", "work", "show")` and parsing stdout *feels* architecturally clean because every CLI command is a program from the outside. Inside Go, it is not. The Unix approach forces you to negotiate stdout, exit codes, JSON/text modes, recursive CLI invocation, and duplicated formatting paths. You end up scripting yourself. Git does not implement `git status` by shelling out to `git diff` and parsing the output; both call the same plumbing.

2. **Model/rendering coupling.** Direct function calls like `work.Render()` look like the right answer to (1), but they couple the model to one particular presentation. The moment `mpm work show --json` lands, or someone wants syntax highlighting in `work show` but compact Markdown in `mpm continue`'s dashboard, the model and the rendering layer have to be untangled again, with no clean migration path. The presentation layer is not the model.

**Formatters, encoders, renderers — three different concerns.** A formatter reshapes the model for a presentation context. An encoder serialises the model to a machine-readable form. A renderer writes a presentation-shaped thing to a terminal. JSON is not rendering; it is serialisation.

```
// mpm work show — default (terminal, full detail)
mpm work show
   ↓
   workingCtxSvc.Load()                       // service
   workingCtxFormatter.Format(model)          // formatter → MarkdownModel
   terminalRenderer.Render(markdownModel)     // renderer → stdout text

// mpm work show --json — machine-readable
mpm work show --json
   ↓
   workingCtxSvc.Load()                       // service
   jsonEncoder.Encode(model)                  // encoder → stdout JSON

// mpm continue — dashboard embed
mpm continue
   ↓
   [services for each section]
   [formatters reshaping for dashboard sections]
   dashboardRenderer.Render(sections...)      // renderer → stdout text
```

The command never sees the model in its raw form. The service never sees a renderer. The renderer never sees a store. Each layer has one concern; commands compose all of them.

## Primary Human Commands

Design the public CLI around cognitive verbs.

```
mpm continue
mpm remember
mpm recall
mpm learn
mpm decide
mpm theorize
mpm why
mpm doctor
```

These are aliases.

They should reuse existing handlers.

### `mpm continue`

`mpm continue` is a **session-resumption command**, not a Working Context command. It composes information from multiple existing sources; it never owns any of them.

**The MVC role.** `mpm continue` is a **view** over multiple models. It owns the **layout** — the structure that turns assembled sections into a single dashboard — and nothing else. Every section in its output belongs to a different command, owned by a different subsystem.

**Section ownership — every section must have a Service owner:**

| Section | Service | Subsystem |
|---|---|---|
| Working Context | `WorkingContextService` | scratchpad (working-context layer) |
| Wake Context | `WakeContextService` | bootstrap-context subsystem |
| Recent Decisions | `DecisionService` | decision subsystem |
| Loaded Skills | `SkillService` | skill subsystem |
| Active Theories | `TheoryService` | theory subsystem |
| Runtime / session metadata | `StatusService` | status subsystem |

Section owners are **services**, not commands. `mpm work show` and the Working Context section in `mpm continue` both invoke `WorkingContextService.Load()` and then hand off to whichever renderer fits the presentation context. They never call each other.

`continue` owns nothing except the layout.

**Implementation pattern:**

```
continue
  ↓
  workingctx.Service.Load()      // Working Context     — service
  wake.Service.Load()            // Wake Context        — service
  decision.Service.Recent()      // Decisions           — service
  skill.Service.Loaded()         // Skills              — service
  theory.Service.Active()        // Theories            — service
  status.Service.Snapshot()      // Runtime             — service
  dashboard.Render(sections...)  // continue owns ONLY the layout
```

The renderers are first-class. `continue` chooses the DashboardRenderer (compact, sectioned). `mpm work show` chooses a MarkdownFormatter + TerminalRenderer pair (full fidelity). Same models. Different presentations. No duplicated formatting, no divergent text, no SQLite queries inside `continue`.

**The architectural advantage.** If a future contributor improves Working Context formatting in `mpm work show`, `mpm continue`'s "Working Context" section improves automatically — no duplicated rendering, no divergence, no bugs where the same information looks different depending on which command you used. That dividend compounds: every improvement to a section owner flows through to every view that consumes it. Six months from now this is the difference between a coherent cognitive interface and a patchwork.

**The line to defend.** The moment `mpm continue` starts querying SQLite directly for scratchpad data, the MVC separation is broken. The moment it calls `mpm work show` (or, ideally, both `continue` and `work` call a shared renderer/service), the architecture stays clean.

**ARCHITECTURE INVARIANT (verbatim, to be embedded at top of `cmd/mpm/handlers_continue.go` when the file is created):**

```go
// ------------------------------------------------------------------
//
// ARCHITECTURE INVARIANT
//
// continue presents state.
//
// It MUST NOT:
//   - recommend actions
//   - rank priorities
//   - become a planner
//   - own any data source directly
//   - render any section that belongs to a subsystem
//   - invoke other commands (no exec.Command, no os/exec)
//   - own SQL / persistence logic
//   - own rendering logic
//
// It is intentionally analogous to:
//   git status
// not:
//   project manager
//
// Composition discipline:
//
//   Every section of continue's dashboard must have a Service owner.
//   continue owns ONLY the layout — the structure that turns
//   delegated sections into one dashboard. Everything inside a
//   section is rendered by the section's renderer.
//
//   continue commands services:
//     workingctx.Service.Load()
//     wake.Service.Load()
//     decision.Service.Recent()
//     skill.Service.Loaded()
//     theory.Service.Active()
//     status.Service.Snapshot()
//
//   and chooses its Formatter + DashboardRenderer for its
//   presentation context. mpm work show commands the same
//   WorkingContextService and chooses its own Formatter + Renderer.
//   Neither calls the other.
//
// The lines to defend:
//
//   continue is a view over multiple models.
//   It owns nothing except layout.
//
//   Commands do not compose commands.
//   Commands compose services.
//
// Breaking this invariant changes the cognitive architecture.
//
// ------------------------------------------------------------------
```

This is not a code comment that documents behaviour. It is a contract that future contributors must understand before editing the file. The day a recommendation or priority-rank lands inside this handler, the cognitive architecture is broken.

### `mpm work`

`mpm work` exposes the current **Working Context** — the ephemeral execution state of the agent.

**The contract is simplicity itself.** `mpm work show` returns the raw Working Context exactly as stored. No interpretation, no summarisation, no additional context. If the agent wrote Markdown, the operator sees that Markdown. If the agent wrote JSON, the operator sees that JSON.

Think of it as: "show me the current working memory."

**Subcommands:**

```
mpm work status    # light metadata: age, session, size
mpm work show      # full raw contents, exactly as stored
mpm work clear     # discard the current Working Context
mpm work promote   # promote Working Context → permanent memory
```

The CLI never exposes the implementation name "scratchpad." Internally it continues to use the existing scratchpad APIs (`flush_scratchpad`, `read_scratchpad`, `promote_scratchpad`, `discard_scratchpad`).

**MVC role, refined.** `mpm work` is one *adapter* over the **WorkingContextService**. The service is the behavioural layer; `WorkingContextStore` is the data-access layer underneath it. `mpm work` is the only place in the CLI that knows about the *command surface* — everything else is composed through services, formatters, encoders, and renderers. The command does not own rendering, does not own persistence, does not own the model. `mpm work show` itself is a tiny handler — it composes `WorkingContextService.Load()` with the formatter + renderer appropriate to its presentation context.

### `mpm why <artifact-id>`

**Purpose:** Preferred operator-facing introspection command. Composes existing provenance, confidence, and evidence APIs.

**The questions it should answer:**

- Why does this exist?
- Why was it retrieved?
- Why is confidence high?

**Output includes:** provenance, evidence, confidence history, reinforcement metadata where applicable.

**Recursion depth: ONE LEVEL.** `mpm why` does not traverse source artifacts.

The reason provenance graphs cycle:

```
Memory → Lesson → Theory → Decision → Evidence → Lesson
```

Once you recurse past depth 2 you need cycle detection, which is a non-trivial tax for a feature almost nobody needs at the point of typing `mpm why`. One level answers the real question — "why does this exist?" — without inventing a new class of bugs.

If the operator genuinely wants recursive provenance, that ships LATER as `mpm why <id> --depth N`. Not today. Not next quarter. Add a year from now if a concrete use case appears.

Do not invent new storage. Only surface existing information.

### `mpm doctor`

Promote doctor to a flagship command.

**Purpose:** Quick confidence that the substrate is healthy. Aggregates existing diagnostics.

Markers:

- ✓ healthy
- ⚠ warning
- ✗ failing

**Potential sections:**

- Database
- Embeddings
- Working Context
- Contradictions
- Scheduler
- Retrieval metadata
- Review backlog

Deep diagnostics remain available through existing flags (`--deep-scan`, `--explain`).

TTY-output matters: doctor renders ✓/⚠/✗ for terminals and falls back to text labels (`OK / WARN / FAIL`) for non-TTY (CI/agent log capture). Detect via the existing `isatty(os.Stdout)` helper.

### Skills

Expose skills consistently. Support both:

```
mpm skill add
mpm skill list
mpm skill show
mpm skill search
```

```
mpm kb skill ...
```

Internally reuse existing implementations.

### Decisions and Theories

The CLI exposes noun-verb forms:

```
mpm decision add
mpm decision resolve
mpm theory add
mpm theory resolve
```

These map directly onto existing MCP contracts. No backend changes required.

## Help System

Refactor help around progressive disclosure.

**Default `mpm help` output should look approximately like:**

```
MPM

Daily
-----
continue
remember
recall
doctor
why

Knowledge
---------
memory
lesson
skill
decision
theory

Working Context
---------------
work

Maintenance
-----------
backup
restore
review

Need more?
----------
mpm help knowledge
mpm help work
mpm help --all
```

Avoid dumping dozens of commands at first contact.

## Tour

Add `mpm tour`.

**Purpose:** Teach MPM through interaction.

The tour covers:

1. Remember something
2. Recall it
3. Create a lesson
4. Record a decision
5. Continue
6. Explain ("why")

The user should experience the workflow rather than read documentation.

**Tour placement: Wave 4, only after Wave 1 + 2 + 3 stabilise.** A tour built on top of unstable interface text becomes stale the moment a command is renamed, the moment a flag changes, the moment a help section regroups. Ship the tour last so the surface it teaches is the surface that ships.

## Non-Goals

Do NOT:

- rename MCP tools
- redesign storage
- redesign SQLite schema
- change routing semantics
- introduce workflow automation
- introduce planning behaviour
- recommend actions
- remove compatibility aliases
- maintain a `--legacy` flat-help mode (operators query help JSON instead)
- ship recursive provenance in `mpm why` v0

## Success Criteria

The implementation is successful if:

1. Existing scripts continue working unchanged.
2. Existing MCP integrations continue working unchanged.
3. The CLI feels like a cognitive interface rather than a database interface.
4. New users can discover core functionality from `mpm help` alone.
5. Daily usage naturally converges on:

```
mpm continue
mpm recall
mpm remember
mpm why
mpm doctor
```

rather than requiring knowledge of the underlying storage model.

6. Each wave is independently shippable and independently revertable.
7. Wave ordering follows the cognitive-system question: does this make MPM feel more like a cognitive system? If yes, ship it.

## Final Design Principle

Optimise for trust and introspection rather than additional capability.

MPM's differentiator is no longer persistent memory.

It is an observable substrate — closed under observation: every operation on MPM is itself observable through MPM's own tools.

Whenever there is a choice between exposing more storage operations or making the existing cognition more observable, prefer observability.

---

## Implementation Rollout (Staged, 4 Waves)

Each wave must answer the cognitive-system question: **does this make MPM feel more like a cognitive system?** If no, defer or redesign.

### Wave 1 — Daily Experience ⭐⭐⭐⭐⭐

The first impression. ~700-900 LOC.

The significantly larger estimate than the original RFC draft reflects the layered-architecture foundation. Wave 1 ships not just the three commands but the **Stores + Services + Formatters + Encoders + Renderers layer** that makes the rest of the cognitive interface possible without re-architecture.

**What ships:**

- Layered architecture skeleton — Stores that already exist via `internal/core/CoreDB` get extracted/wrapped as needed; Services earn their existence by owning behaviour.
- `WorkingContextStore` (SQLite queries for scratchpad — pure data access).
- `WorkingContextService` (load + validate + expire + promote + clear — real behaviour that justifies the service layer).
- `ContinueService` (orchestrates 5 subsystem reads — composition IS behaviour).
- `mpm work {status, show, clear, promote}` — thin adapter commands composing `WorkingContextService` + formatters + renderers/encoders.
- `mpm continue` — section composition over services + DashboardRenderer.
- Progressive help (`mpm help <section>`, `mpm help --all`).
- Common formatter / encoder / renderer primitives: TerminalRenderer, DashboardRenderer, JSONEncoder, Formatter base type.

**What does NOT ship in this wave:**

- `mpm why` — not yet
- `mpm doctor` — not yet
- Other services that wrap stores without owning behaviour (no `TheoryService`, no `SkillService`, no thin `DecisionService`). Each of those is just a Store. Wave 2 adds services only when their `mpm doctor` / `mpm why` work demands behaviour, not because the wave name suggests it.

**Why this ordering:** the daily surface lands first because it's the wave operators hit every morning. `mpm continue` is the command v identified as the single best-defining verb of MPM. Until `mpm continue` works, none of the introspection features are reachable through it, so they belong after, not before.

**Why the larger LOC:** the layered architecture is one-time cost. Wave 2 (doctor + why) and Wave 3 (remember/recall/etc.) inherit the pattern and stay small. Without laying the foundation here, every subsequent wave would re-couple model and rendering, and the cumulative cost would be much larger than this first-wave spike.

### Wave 2 — Trust ⭐⭐⭐⭐⭐

The "I trust the substrate" release. ~400 LOC.

**What ships:**

- `mpm doctor` (telemetry aggregator with ✓/⚠/✗ markers)
- `mpm why <id>` (one level deep; no recursion)

**Together these change how people feel about the system.** Doctor gives reassurance; `why` gives debuggability. Both serve introspection as differentiator. Both compose existing substrate rather than adding storage.

### Wave 3 — Vocabulary ⭐⭐⭐⭐☆

Pure ergonomics. ~80 LOC.

**What ships:**

- `mpm remember` (alias for `mpm add`)
- `mpm learn` (alias for `mpm kb lesson add`)
- `mpm decide` (alias for `mpm kb decision add`)
- `mpm theorize` (alias for `mpm kb theory add`)
- `mpm skill {add, list, show, search}` (parallel to `mpm kb skill ...`)
- `mpm decision {add, resolve}` and `mpm theory {add, resolve}` (parallel to MCP contracts)

**No behaviour changes.** Just named verbs humans can guess. Aliases only — existing surfaces keep working.

### Wave 4 — Discovery ⭐⭐⭐⭐☆

Onboarding. ~200 LOC.

**What ships:**

- `mpm tour` — interactive walkthrough covering remember, recall, learn, decide, continue, why.

**Why this is last:** a tour built on top of unstable interface text becomes stale the moment a command is renamed or a help section regroups. Wave 4 only begins after Waves 1-3 have stabilised across at least one deploy cycle.

---

### Wave Sequencing Notes

- Each wave is independently shippable and independently revertable.
- Branch: `feat/cognitive-interface`, off `feat/skills-layer` HEAD (which carries the watcher-removal work at `34c57f6`).
- Each wave = one commit, one PR-able diff, one deploy via the existing `sudo ./deploy.sh` dance.
- Pre-commit hook (build + synthesis/reliability/lifecycle + router lint + GPG sign) verified per commit.
- Total estimate across all waves: ~960 LOC, 4 commits, 4 deploys.

### Branching Strategy

```
feat/skills-layer   [34c57f6 — current HEAD]
   │
   └── feat/cognitive-interface
         ├── wave-1-daily-experience      ← mpm continue + mpm work + help
         ├── wave-2-trust                 ← mpm doctor + mpm why
         ├── wave-3-vocabulary            ← remember/learn/decide/theorize aliases
         └── wave-4-discovery             ← mpm tour
```

Each wave lands as its own commit on `feat/cognitive-interface`. Squash or keep-separate decisions made per-wave based on diff size and review feedback.

---

*Status: GREENLIT. Open for next-step direction from v on whether to start Wave 1 implementation in a follow-up session.*
