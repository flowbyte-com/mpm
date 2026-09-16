# MPM Lifecycle Matrix — Final Release Pass

Each cell is one of: `supported` (canonical command exists), `intentionally-unavailable` (deliberately excluded with reason), or `N/A` (does not apply to this artifact family).

| Artifact | create | list | search | show | update | transition | invalidate/retract | delete | restore/reopen | shred | history |
|---|---|---|---|---|---|---|---|---|---|---|---|
| memory     | `mpm remember` | `mpm memory list` | `mpm memory query` | `mpm memory show` | `mpm memory update` (metadata/tags/weight) | — | `mpm memory retract` (soft-retract: marks deleted_at; preserves row for audit) | — | — | `mpm memory shred` (hard-delete via `mpm_shred_memory`) | `mpm memory history` (revisions) |
| lesson     | `mpm learn`    | `mpm lesson list` | `mpm lesson search` | `mpm lesson show` | `mpm lesson update` | — | `mpm lesson retract` | — | — | `mpm lesson shred` | `mpm lesson history` |
| theory     | `mpm theorize` | `mpm theory list` | `mpm theory query` | `mpm theory show` | `mpm theory update` | `mpm theory resolve` (proven/disproven/resolved) | `mpm theory invalidate` (cascade via invalidating cited foundation memories) | **intentionally-unavailable** — terminal via `resolve` or `invalidate` only | — | **intentionally-unavailable** — theory lifecycle is append-only; use `resolve` or `invalidate` | `mpm theory history` (validation_criteria + verdict chain) |
| decision   | `mpm decide`   | `mpm decision list` | `mpm decision query` | `mpm decision show` | `mpm decision update` (rationale) | `mpm decision supersede` (replaces with new id) | `mpm decision invalidate` (reverses without replacement) | **intentionally-unavailable** — terminal via `supersede` or `invalidate` only | — | **intentionally-unavailable** — decision lifecycle is append-only | `mpm decision history` |
| work       | `mpm work create` | `mpm work list` | `mpm work query` | `mpm work show` | `mpm work update` + `mpm work note` | `mpm work complete` / `mpm work cancel` | — | — | `mpm work reopen` (round-trip after complete) | — | `mpm work history` |
| topic      | `mpm topic add` | `mpm topic list` | `mpm topic query` | `mpm topic show` | `mpm topic update` | `mpm topic promote` (promote_to_memory) | — | `mpm topic delete` | — | — | `mpm topic history` |
| reference  | `mpm reference add` | `mpm reference list` | `mpm reference search` | `mpm reference show` | `mpm reference update` | — | `mpm reference invalidate` (soft) | — | — | `mpm reference shred` | `mpm reference history` |
| skill      | `mpm skill save` | `mpm skill list` | `mpm skill query` | `mpm skill show` | `mpm skill save` (re-saves same canonical id) | — | `mpm skill invalidate` (canonical skill: prefix lifecycle) | — | — | `mpm skill shred` | `mpm skill history` |
| evidence   | `mpm evidence add` | `mpm evidence list` | — | `mpm evidence show` | — (append-only by design — corrections use counter-evidence) | — | `mpm evidence retract` (counter-evidence; the row stays but is marked superseded) | **intentionally-unavailable** — corrections are via counter-evidence | — | — | `mpm evidence history` |
| confidence | — | `mpm confidence list` | — | `mpm confidence show` | `mpm confidence recompute` | — | — (driven by evidence lifecycle) | — | — | — | `mpm confidence history` + `mpm confidence trend` + `mpm confidence changes` |
| handoff    | `mpm handoff write` | `mpm handoff list` | — | `mpm handoff show` | — | — | — (handoffs persist until read by next session) | `mpm handoff delete` | — | `mpm handoff shred` | — |
| scratchpad | `mpm scratchpad flush` | — (single row per session) | — | `mpm scratchpad read` | `mpm scratchpad flush` (idempotent upsert) | `mpm scratchpad promote` (cross-table tx: SELECT→INSERT memory→DELETE) | — | `mpm scratchpad discard` | — | — (scratchpad is volatile by design) | — |
| wake       | `mpm wake schedule` | `mpm wake list` | — | `mpm wake check` | — | `mpm wake digest` (acknowledges backlog) | `mpm wake snooze` (cluster-aware) | — | — | — | — |
| task       | `mpm task upsert` | `mpm task list` | — | `mpm task show` | `mpm task update` | `mpm task complete` / `mpm task pause` / `mpm task resume` | — | `mpm task delete` | — | — | `mpm task history` |
| directive / global rule | (seeded; not user-creatable via CLI) | `mpm context query_global_rules` | — | `mpm context read_directives` | — | — | `mpm context retire_global_rule` | — | — | — | — |

## Intentional Asymmetries

### Theory: no `delete` / no `shred`
A theory's evidence trail is the point — deleting the row would also delete the validation history that downstream decisions cite. The terminal operations are:
- `resolve` — binary verdict (`proven` / `disproven`) once validation criteria are met
- `invalidate` — when a cited foundation memory is invalidated; cascade marks the theory as `cascade:<id>` and triggers a `mpm why` re-evaluation prompt

### Decision: no `delete` / no `shred`
Decisions are also audit artifacts. Terminal operations:
- `supersede` — record a new decision that supersedes this one (`superseded-by:<new_id>` link)
- `invalidate` — record that the decision was reversed without replacement

### Evidence: corrections via counter-evidence
Evidence rows are append-only. The historical ledger is intentional — `evidence_added` / `evidence_updated` / `evidence_deleted` / `evidence_expired` are tracked in `confidence_history.trigger`. To correct a wrong observation, attach a counter-evidence row with negative strength. To formally retire an evidence row, `evidence_expired` is recorded automatically; the row itself is not deleted.

### Wake: agent/substrate primitive, not first-class human object
Wakes are substrate primitives consumed by `mpm-scheduler`. The user-facing equivalent is **scheduled tasks** (`mpm_wakes upsert_task` / `list_tasks`). Help explicitly distinguishes:

```
mpm wake schedule         — schedule a one-shot wake (agent/substrate primitive)
mpm wake upsert_task      — register a recurring task (human-facing abstraction)
```

`mpm wake schedule` is intended for cron-replacement and agent coordination, not as a primary scheduling surface.

### Confidence: derived, not first-class
Confidence is *computed* from evidence. There is no `mpm confidence create` — confidence values emerge from the substrate. The CLI surface is read-only (`show` / `recompute` / `history` / `trend` / `changes` / `quality`).

### Skill lifecycle
Skills live in `memories` with `collection='skills'` and `tags=['skill']`. The canonical id format `skill:<name>-v<version>` auto-routes. There is no `delete` because skills are versioned; deprecation uses a new version. `shred` exists for hard removal when a skill must be entirely gone.

### Handoff
Handoffs persist across sessions. There is no automatic prune; explicit `delete` or `shred` for hard removal. The lifecycle is: write → list (next session reads via wake context) → optional retire.

### Work
Work has full CRUD plus `complete` / `cancel` / `reopen`. There is no `shred` because the work ledger is the audit trail. `mpm work note` is the append-only event stream.

### Scratchpad
Volatile by design (`decay_at = +24h`). Only `flush` (upsert) / `read` / `discard` / `promote`. No `shred` because `promote` consumes the row atomically (verified by `TestAcceptance_A_ScratchpadPromote_*`).
