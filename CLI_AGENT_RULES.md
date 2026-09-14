# MPM CLI Agent Rules

This file is a normative contract for agents modifying any MPM command-line interface.

It exists to prevent CLI drift between implementations, binaries, refactors, and agent-generated changes.

These rules apply to the main `mpm` CLI and, where relevant, to sibling operator-facing binaries such as `mpm-telemetry`. A sibling binary may follow the same user-facing conventions without sharing implementation code with `mpm`.

If a proposed CLI change conflicts with this document, either change the proposal or update this contract deliberately as part of the same reviewed change. Do not silently invent a new convention.

---

## 1. Core principles

1. Prefer stable contracts over clever output.
2. Human output must be readable without knowing MPM internals.
3. Machine output must be explicit, structured, and stable.
4. Help must never mutate state or require runtime dependencies.
5. Displayed identifiers and pointers must round-trip through the CLI that displays them.
6. Runtime health, optional capabilities, and maintenance backlog are different states and must not be collapsed into one indicator.
7. Built-binary behavior is the acceptance authority. Source inspection alone is insufficient.
8. Do not call a CLI surface complete while a known acceptance defect remains open.

---

## 2. Visual grammar

Use the established MPM textual grammar.

### Headings

Top-level pages use:

```text
MPM · <Surface>
```

Nested pages use:

```text
MPM · <Surface> · <Subsurface>
```

Examples:

```text
MPM · Dashboard
MPM · Doctor
MPM · Telemetry
MPM · Telemetry · query
```

Rules:

- `MPM` is uppercase.
- Use restrained terminal colour.
- Do not introduce decorative boxes, Bubble Tea layouts, neon palettes, banners, or marketing-heavy UI unless explicitly approved.
- Use sentence-case labels.
- Preserve monospace alignment where useful.
- Do not bold large portions of output.
- Secondary guidance should be visually quieter than primary state.
- Human timestamps should be readable and consistent.
- Raw epoch values should not be shown in normal human output unless the command is explicitly low-level.

The current dashboard visual design is an accepted baseline. Do not redesign it incidentally while fixing unrelated behavior.

---

## 3. Status symbols and severity

Use symbols semantically, not decoratively.

```text
✓  healthy / configured / successful
○  informational / optional / not configured but not a fault
⚠  warning / degraded / actionable attention required
✗  failure
```

Do not map a boolean such as `OK=true` mechanically to `✓` if the state is informational.

Examples:

```text
○  Embedding model        not configured · optional
○  LLM provider           not configured
✓  Scheduler running
⚠  Wake backlog           4 wake(s) overdue
```

Optional absence is not success and is not necessarily a warning.

---

## 4. Help contract

Every public CLI help surface must obey all of these rules.

### Required forms

Where a command supports help, both forms must work:

```text
-h
--help
```

Top-level commands may also support:

```text
help
```

### Required behavior

Help must:

- exit `0`
- be inert
- perform no database mutation
- create no files
- start no daemon
- open no socket
- require no workspace
- require no config
- require no API key
- require no live provider
- require no database
- require no scheduler
- be checked before normal argument/runtime validation

A help request must never fall through to errors such as:

```text
MPM_WORKSPACE is required
--pricing is required
database unavailable
socket unavailable
```

### Public help content

Help should describe:

- what the command does
- usage
- arguments
- options
- relevant environment/config inputs
- examples when they materially help

Do not expose implementation archaeology in public help.

Forbidden examples include:

- source file paths
- internal Go symbol names
- audit IDs
- historical defect IDs
- dated remediation notes
- references to old implementations
- comments such as “fixed in 2026-09-14 pass”

Help describes the current contract, not the history of how humans suffered their way into it.

---

## 5. Bare-command behavior

The behavior of a command with no arguments must be truthful and deliberate.

If no arguments show help, say so and exit successfully.

If no arguments start a daemon, that must be an explicit documented contract.

Never document a default action that the binary does not actually perform.

For `mpm-telemetry`, bare invocation is operator-facing help. The daemon entry point is explicitly:

```text
mpm-telemetry serve
```

Do not change that implicitly.

---

## 6. Human output versus machine output

Human output is the default unless the command contract explicitly says otherwise.

Machine output must require an explicit machine-oriented mode such as:

```text
--json
```

Rules:

- human and JSON output must derive from the same structured underlying data
- do not make one CLI mode parse another CLI mode's rendered output
- do not shell out to the CLI and reparse its JSON inside another CLI handler
- do not duplicate business predicates independently for human and JSON rendering
- JSON envelopes must use stable field names
- counts must match the arrays/records they describe
- human output must not dump raw JSON unless the command is explicitly a raw/debug surface

Example invariant:

```text
JSON count == number of returned results == number of human rows
```

where the command semantics are equivalent.

---

## 7. Canonical terminology

Use precise terms for precise roles.

Prefer:

```text
LLM provider
Embedding model
Embedding provider
Scheduler
Working Context
System health
Attention
```

Avoid vague umbrella labels such as:

```text
AI provider
model backend
system status
```

when they obscure which subsystem is actually being described.

If a feature is optional, state that explicitly.

If a missing feature causes degradation, state exactly what becomes unavailable.

Example:

```text
○  Embedding model        not configured · optional
    → semantic / vector similarity retrieval is unavailable when absent;
      lexical and structured retrieval remain available.
```

Do not imply that all retrieval is broken when only semantic/vector retrieval is unavailable.

---

## 8. Health versus attention

Do not collapse runtime health and cognitive/maintenance backlog into a single status.

The dashboard contract distinguishes:

### System health

Represents substrate/runtime liveness and integrity, such as:

- database health
- service availability
- scheduler/runtime liveness
- other genuine operational failures

Example:

```text
System health   ✓ Healthy
```

### Attention

Represents actionable non-fatal backlog or warnings, such as:

- overdue wakes
- spaced-review backlog
- other Doctor warnings that require operator attention

Example:

```text
Attention       ⚠ 2 warnings — run `mpm doctor`
```

A healthy substrate may still require attention.

Do not make normal review backlog equivalent to database/service failure.

Do not hide Doctor warnings behind a blanket `✓ Healthy`.

Where practical, dashboard attention state should reuse the same data sources and warning logic as Doctor rather than independently reimplementing thresholds.

---

## 9. Provider and model menus

### Provider ordering

In provider-selection menus, `Custom` is always entry 1.
All remaining providers are sorted alphabetically by display name (case-insensitive), with a deterministic ID-based tiebreak.

Sort order is NOT derived from map iteration, hand order, or recommendation metadata. Use the canonical `providersFor(capability)` helper.

### Model ordering

When manual model entry is offered, `Custom model` is entry 1.
Remaining models are sorted alphabetically (case-insensitive), with a deterministic tiebreak on the original model string.

Recommendation metadata may display `(recommended)` but MUST NOT reorder the choices.

### Capability orientation

Configuration and discovery are capability-oriented rather than vendor-exclusive.
A provider that supports `embed` is a valid embedding choice regardless of its vendor name.
A provider that supports `generate` is a valid LLM choice regardless of its vendor name.

The canonical capability enum is `generate` and `embed`. A model may support one, both, or `unknown` (no positive evidence yet). Unknown is NOT false.

Do not encode `provider == "ollama" → embedding` or `provider == "openai" → generation`. Capability is the contract.

### Actionable dashboard state

When a configurable dashboard component is absent or degraded, the hint row must include the canonical command used to configure or repair it.

Examples:

- absent LLM: `run 'mpm config' to configure one`
- absent embedding: `run 'mpm config detect-embedding --apply' to configure one`

Do not make dashboard hints enormous. Detailed capability/degradation explanation can remain in Doctor/help.

---

## 10. Optional provider semantics

### Embeddings

Canonical states:

| State | Marker | Meaning |
|---|---|---|
| not configured | `○` | optional capability absent |
| configured and healthy | `✓` | available |
| configured but unreachable | `⚠` | degraded |
| configured but invalid/misconfigured | `⚠` | degraded |

Absence of an embedding model does not make core MPM unhealthy.

### LLM

An absent LLM is informational unless the command being executed specifically requires LLM capability.

Core MPM CRUD and substrate operations remain usable without an LLM.

Use:

```text
○  LLM provider           not configured
    → synthesis features require an LLM; core CRUD remains fully usable
```

Do not present absent optional LLM configuration as a global system failure.

---

## 11. Counts must use canonical stores

Dashboard and summary counts must use the same canonical definition as their authoritative CLI surfaces.

Do not approximate one artifact type using another table or collection.

Examples:

- active memories must use the canonical active-memory predicate
- lessons must come from the canonical lesson store
- skills must come from the canonical skills source
- decisions must use the canonical decision definition

If a dashboard label says:

```text
Memories (active)
```

its count must match the authoritative active-memory count dynamically.

Do not hard-code production fixture numbers in tests.

Prefer shared helpers over duplicated SQL predicates.

---

## 12. IDs, pointers, and round-trip behavior

Any identifier or URI presented to the user as a usable reference must round-trip through the relevant user-facing CLI.

If MPM displays:

```text
mpm://work/<id>
```

then work-item commands that accept a work ID must accept that canonical pointer form as well as the bare ID.

Pointer normalization should happen at a shared chokepoint, not through duplicated ad-hoc string stripping across subcommands.

Wrong-kind pointers must fail cleanly.

Example:

```text
mpm://work/<id>      accepted by work commands
mpm://memory/<id>    rejected by work commands
mpm://blob/<id>      rejected by work commands
```

Do not silently coerce a pointer from another artifact type.

---

## 13. Work-item ID contract

Commands that accept a work-item ID must support both:

```text
<bare-id>
mpm://work/<id>
```

This applies to current ID-taking operations including:

```text
show
complete
cancel
history
note
reopen
update
resolve-contradiction
```

If more work-item commands are added later, they inherit the same rule unless explicitly documented otherwise.

---

## 14. Handoff semantics

Do not assume every surface means the same thing by “latest handoff”.

The current contract intentionally distinguishes consume and browse semantics.

### Dashboard: Last Session

Semantic:

```text
latest unread handoff
```

Purpose:

```text
what continuity still needs attention?
```

### `mpm call read_wake_context`

Semantic:

```text
latest unread handoff, then mark it read
```

Purpose:

```text
consume continuity for a waking agent
```

### `mpm ops wake`

Semantic:

```text
latest handoff regardless of read state
```

Purpose:

```text
browse what the previous session was doing
```

### `mpm handoff list`

Default:

```text
all handoffs, newest first
```

### `mpm handoff list --unread`

Returns only:

```text
read_at IS NULL
```

Do not “fix” these surfaces merely because their outputs differ. The distinction is intentional and regression-tested.

---

## 15. Wire-contract consistency

CLI/substrate boundaries must use one canonical field name and one canonical parameter name.

Do not tolerate silent mismatches such as:

```text
results      vs handoffs
unread       vs unread_only
```

unless compatibility handling is explicitly required and documented.

For every request/response boundary:

- define canonical request keys
- define canonical response keys
- test them from the caller side
- test human/JSON parity
- test filter behavior
- test empty and non-empty cases

Typed Go values must not be assumed to satisfy generic type assertions without proof. If renderer input requires generic maps, normalize deliberately and test it.

---

## 16. Shared UX does not require shared implementation

Sibling binaries may follow the same CLI conventions without importing the main `mpm` CLI implementation.

This is especially important for `mpm-telemetry`.

`mpm-telemetry` must remain an independent implementation/security boundary.

It may mirror conventions such as:

```text
MPM · Telemetry
MPM · Telemetry · query
```

but should not import the main `mpm` renderer merely for cosmetic consistency.

Future telemetry network/web capabilities must not widen the core MPM dependency or trust boundary.

Prefer:

- separate credentials
- least privilege
- optional deployment
- narrow interfaces
- no unnecessary primary-DB write access
- no unnecessary core network exposure

UX consistency does not justify architectural coupling.

---

## 17. Version surfaces

Operator-facing binaries should expose build identity consistently where practical.

If a binary supports explicit version output, prefer common forms such as:

```text
version
--version
```

and preserve linker-stamped build identity.

Version output must:

- exit `0`
- perform no runtime validation
- perform no side effects

Help pages may include the build identity when useful.

---

## 18. Error behavior

Errors should be:

- specific
- actionable
- stable enough for users to understand
- free of irrelevant implementation details

Do not report success after a failed operation.

Do not print an error and continue into a success path.

Do not silently ignore unknown or misspelled contract fields when doing so would change behavior.

Malformed pointers, invalid filters, invalid IDs, and missing required values should fail clearly.

---

## 19. Testing rules

Every CLI defect fixed must receive a regression that fails against the pre-fix behavior.

Use hermetic fixtures:

```text
t.TempDir()
temporary HOME
temporary MPM_WORKSPACE
temporary database
```

Do not mutate the operator's production database during automated tests.

### Minimum acceptance categories

Where relevant, test:

- `-h`
- `--help`
- bare command
- human output
- JSON output
- empty state
- non-empty state
- invalid input
- canonical pointer form
- bare ID form
- no-side-effect behavior
- exit codes
- provider absent/healthy/broken states
- count parity
- warning/attention parity

### Help safety

Help tests should explicitly verify that help:

- exits 0
- does not create files
- does not start services
- does not require runtime state
- does not fall through into normal validation

### Built binary

Do not stop at `go test`.

Build the actual binary and smoke-test the changed surface.

Source inspection and handler tests do not override observed built-binary behavior.

---

## 20. Standard verification commands

For changes affecting the main codebase, run:

```bash
make build
make test
make test-race
```

Also run:

```bash
cd internal/core/tools && go test -tags fts5 -count=1 .
```

Run `golangci-lint` if it is installed.

If lint tooling is unavailable, state that plainly. Do not claim lint passed when it was not run.

For CLI changes, add explicit built-binary smoke commands relevant to the modified surface.

---

## 21. Production-state safety during acceptance

Treat the user's real MPM substrate as production state.

Default to read-only validation.

Do not, without explicit permission:

- resolve theories
- clear wakes
- complete/cancel work
- consume handoffs
- reinforce memories
- shred memories
- add “fixture” memories
- modify config
- mutate scheduler state

A read-only investigation must actually remain read-only.

Use temporary workspaces for mutating acceptance tests.

Do not clean up real-world fixture state before a bug has been reproduced and validated against it.

---

## 22. Release/readiness rules

Do not declare a CLI surface READY when:

- a known acceptance defect remains open
- built-binary output contradicts the report
- human and JSON modes disagree unintentionally
- help still falls through into runtime validation
- a displayed pointer does not round-trip
- dashboard counts disagree with canonical surfaces
- warnings are hidden behind a misleading healthy state
- tests only validate source helpers but not the actual built path

A READY report should include:

1. root cause
2. exact fix
3. regression coverage
4. built-binary verification
5. test-suite result
6. race result
7. lint availability/result
8. commit hash
9. clean/dirty git state
10. any intentional semantic differences between similar surfaces

Do not use “READY” as a synonym for “the patch compiled”.

---

## 23. Scope discipline

When given a targeted CLI defect:

- fix the defect
- add the regression
- verify the built binary
- stop

Do not opportunistically redesign unrelated output, rename unrelated commands, migrate unrelated renderers, clean production state, or broaden the patch into an architectural rewrite.

If a separate issue is discovered, record it and classify it instead of silently expanding scope.

---

## 24. Agent checklist before modifying a CLI surface

Before changing CLI behavior, verify:

```text
[ ] What is the current public contract?
[ ] Is there already a canonical helper/store/query for this data?
[ ] Is this human output, machine output, or both?
[ ] Does the displayed identifier round-trip?
[ ] What should -h/--help do?
[ ] Does the change alter exit codes?
[ ] Is the state OK, INFO, WARN, or FAIL?
[ ] Is the feature optional or required?
[ ] Does another surface intentionally use different semantics?
[ ] Can this be tested hermetically?
[ ] What built-binary smoke proves the fix?
[ ] Am I touching anything outside the requested scope?
```

If any answer is unclear, investigate before editing.

---

## 25. Final rule

Prefer consistency over invention.

The MPM CLI has already paid the cost of discovering these contracts through manual acceptance testing. Future agents should preserve them rather than rediscovering them by introducing the same defects again.
