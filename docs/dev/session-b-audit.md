# Session B audit — intelligence core, critic, scheduler, evidence, provenance, retention

> **Author:** Session B (intelligence core). **Worktree:** `~/src/mpm-core`, branch
> `dev/session-b-core`, base `70deac2bb3fce3c63da28bfbd9ca82ca2c4d1e2e` (verified against
> `origin/main` — matches the reported integration baseline).
>
> **Scope:** the first deliverable of `docs/dev/launch-gate.md` §3.3 — an audit and a
> recommendation. **No implementation.** No merge, no deployment, no shared-interface
> change, no `SafeMigrations` entry, no change to the tool registry.
>
> **Date:** 2026-10-26. **Addressed to:** Session A. **Copy:** Session C.

---

## 0. Environment: the required MPM environment is NOT inherited

This is first because it changes how every other statement in this report must be read.

The brief requires `MPM_WORKSPACE=$HOME/.mpm-dogfood`, `MPM_DB_PATH=$HOME/.mpm-dogfood/src/db/mpm.db`
and `MPM_BINARY=$HOME/src/mpm/.build/bin/mpm` to be **inherited by the OpenCode process**.
They are not.

| Check | Result |
|---|---|
| OpenCode server process (`opencode-cli serve --service`, pid 539959) | `HOME`, `PATH` present. **No `MPM_WORKSPACE`, no `MPM_DB_PATH`, no `MPM_BINARY`.** |
| Plugin `resolveWorkspace()` (`agent_installation/mpm-opencode/src/workspace.ts:20-23`) | falls back to `$HOME/.mpm` → **the live install** |
| Plugin `resolveMpmBinary()` (`agent_installation/mpm-opencode/src/resolve-mpm-binary.ts:60-96`) | falls back to `$HOME/.mpm/bin/mpm` → commit `3df72315`, **27 commits behind `main`** |

**Proven, not inferred.** One `mpm_context recent_activity` call issued through the plugin's
tool surface produced a `tool_invocations` row in `/home/v/.mpm/src/db/mpm.db` with
`framework_name='opencode'` and `framework_session_id='ses_e8632a4b0ffe9uUwCRJjhOUGbF'`.
The row count in `~/.mpm-dogfood/src/db/mpm.db` was `4` before and `4` after.

**Consequence.** Every plugin-mediated tool call in every session writes into the live
install and uses a stale binary. The dogfooding workspace's own rows carry
`framework_name='opencode'` with an **empty** `framework_session_id`, because they came
from the MCP probe path, which has no host session identity.

**The supported workaround, used for everything in this report.** Drive the dogfooding
workspace through the CLI with the environment pinned:

```bash
export MPM_WORKSPACE="$HOME/.mpm-dogfood"
export MPM_DB_PATH="$HOME/.mpm-dogfood/src/db/mpm.db"
BIN="$HOME/src/mpm/.build/bin/mpm"
"$BIN" call <tool> --payload '<json>'
```

`MPM_DB_PATH` is pinned as well because `mpm-critic` and `mpm-scheduler` honour it while
`mpm` and `mpm-mcp` ignore it (`docs/dev/dogfooding-runbook.md` §8.2 — re-verified).

**Requested of Session A:** either the OpenCode process is launched with these three
variables, or the sprint accepts that the plugin's tool surface writes to the live install
and that all sprint work goes through the CLI path above. The dogfooding runbook §3.3
assumes the former; it is not what is happening.

### 0.1 Two runbook facts corrected by measurement

* **The scheduler-status line is systemd-based, not process-based.** The inherited lesson
  `27edef45019e8e49` ("looking for the PROCESS") is inaccurate for the current build.
  `cmd/mpm/handlers_info.go:417-429` runs `systemctl --user is-active mpm-scheduler` and
  reads `MainPID`. The **conclusion is unchanged and re-confirmed**: the line is
  workspace-blind. Measured with `MPM_WORKSPACE` pointed at a disposable workspace and
  `HOME` unchanged, `mpm info` reported `mpm-scheduler : running (pid 3050)` — the live
  scheduler. A second, different signal exists on stderr (`cmd/mpm/main.go:174` reads
  `$HOME/.mpm/run/scheduler.state`). Neither is workspace-scoped. **The dogfooding
  workspace still has no scheduler of its own**, and `mpm info` must never be read as
  evidence that a workspace is maintained.
* **The dogfooding workspace can be driven on demand without a scheduler.** `mpm-critic`
  is a standalone binary (`cmd/mpm-critic/main.go`) and `mpm-critic` is invoked by the
  scheduler via the `critic_audit` wake (`internal/scheduler/scheduler.go:921-933`).
  Running it by hand against a pinned workspace needs no systemd unit. **The detection
  work recommended in §4 therefore has no scheduler dependency.**

---

## 1. M1–M7 acknowledgement

Recorded durably in the shared dogfooding workspace as decision `3a987eca2c708fac`
(`mpm_decisions record`). **Contract version accepted: `mpm-event/0.1`.**

| Item | Position | Substance |
|---|---|---|
| **M1** Envelope | **AGREE** | Non-blocking notes: (a) `payload` is null in 0.1 and reserved — by the Projection Test, a field that is always null has no reader; either drop it from 0.1 or declare it explicitly reserved and outside the frozen set's byte accounting. (b) `observed_at_ms` must stay adapter-observation-time; if the host turns out to deliver its own timestamp, that is a **new `meta` allowlist field in a 0.2 minor bump**, not a substitute. |
| **M2** Vocabulary & classification | **AGREE** | The reject/ignore/count-never-map rule is *required* by the substrate's shape: `evidence` is bound to an existing artifact, `work_events` to a work item, `capability_events` to a capability, `tool_invocations` is CHECK-constrained to tool invocations. A mis-mapped observation corrupts an existing table's meaning. **Dependency recorded (not a disagreement):** M2 is unobservable until §6.1 (storage) and §6.4 (counter surface) are decided — A should treat them as prerequisites for M2 and M6, not parallel work. |
| **M3** Identity | **AGREE** | Verified independently: `git config --get remote.origin.url` is `https://github.com/flowbyte-com/mpm` for all three sprint worktrees, which share one `--git-common-dir` → identical derived `project_id`, as specified. **Clarification requested:** `mpm_session_id` must be documented as the MPM *interaction-lifecycle* identity, not a per-conversation one — see §2.6. |
| **M4** Ordering | **AGREE** | Now empirically justified: `tool_invocations.started_at` is whole unix seconds and up to four `opencode` invocations share one second in the live DB, so the substrate's own audit table cannot order rapid activity today. |
| **M5** Trust | **AGREE, with one clause-internal tension referred to A** | The tension is between *"promotion follows the existing review and authority rules"* and *"no automatic promotion"*. The existing rules enforce neither evidence nor review on the critic's own emission path — see §3.2. A must rule which reading governs. |
| **M6** Ingest contract | **AGREE** | **One requirement added:** the receipt's new-or-duplicate determination must come from a **read-back** of the stored row, not from the `INSERT` result (Substrate Defense Triad rule 3). Free — MPM already enforces it on every write path — and it prevents the receipt from becoming a second, weaker claim surface. **Answer to contract §9 Q8:** the rejection-counter surface belongs on **`mpm_system`**, not `mpm_context`. `mpm_context` is the session-start surface and already carries the whole `contextual_focus` projection; ingestion counters would add tokens to every wake for information no agent needs at session start. `mpm_system` already hosts `query_audit_log` and `critic_findings` and is explicitly the "system health, not daily agent work" tool. |
| **M7** Outcomes | **AGREE** | Verified `internal.ToolOutcomeClass` is the closed nine-value enum the contract names (`ok`, `validation`, `not_found`, `conflict`, `degraded`, `substrate`, `integration`, `timeout`, `internal`) and that `tool_invocations.result_status` carries `CHECK (result_status IN ('success','error'))`. **Clarification requested:** `result_status` / `outcome_class` are **MPM-scoped, not host-scoped**. For the §4 `tool.outcome` type the host's status belongs in the opaque `meta.host_result`, and `result_status` must read `unknown` because `operation` is absent. |

### Blocking objections: **NONE**

M1–M7 are each accepted. Two clause-internal clarifications (M3, M7) and one clause-internal
tension (M5) are referred to A. None blocks adoption of `mpm-event/0.1`. **B does not assert
that dependent implementation may begin** — that waits on A's ruling on the M5 tension and
on contract items §6.1 and §6.4, exactly as the launch gate sequences it.

---

## 2. Existing capability inventory

Verdict per area: **works** / **partially works** / **missing**.

### 2.1 Critic — `internal/critic/`

| Facility | Verdict | Evidence |
|---|---|---|
| `SurvivalAsymmetryHunt` | works | `internal/critic/hunts.go:37-120`. Ratio of survival by `memories.metadata.$.source`, min sample 10, ratio ≥ 2.0. Emits `mpm_lessons save`. |
| `StaleMemoryHunt` | works | `hunts.go:167-254`. Wall-clock 30 d **and** cumulative scheduler-active uptime 12 h, both gates required, `LIMIT 20`. Emits `mpm_memory challenge`. |
| `WeakTheoryHunt` | works | `hunts.go:266-315`. `collection='theories'` and `confidence < 0.6`, `LIMIT 10`. Emits `mpm_lessons save`. |
| `PoisonPillHunt` | works | `hunts.go:334-382`. Every 5th cycle; manufactures a counter-theory against the highest-confidence memory. |
| Durable cycle counter | works | `internal/critic/audit.go:230-262`. Atomic `INSERT … ON CONFLICT … RETURNING` on `system_config['critic_cycle']`, fail-closed on malformed state, crash burns a cycle deliberately. |
| Finding emission via CLI | works | `audit.go:117-141`. `mpm call <tool> --payload '{"action":…,"params":…}'`; failures are logged, never fatal. |
| **Detection of *absent* knowledge** | **missing** | All four hunts read `memories` and detect **degradation of what is recorded**. None compares agent activity against recorded artifacts. Repo-wide grep for `missed\|opportunity\|not_recorded\|unrecorded\|forgot\|should_have` across `internal/` and `cmd/` returns no detection logic. |
| **Proposal carrying evidence** | **missing** | See §3.3. |
| **Idempotent emission across cycles** | **missing** | See §3.4. |

### 2.2 Scheduler — `internal/scheduler/`

| Facility | Verdict | Evidence |
|---|---|---|
| Tick loop, wake dispatch | works | `scheduler.go:665-743` (4-way select), `:408-496` (Tick), `:358-386` (due wakes). |
| Singleton safety | works | `scheduler.go:806-819` — `flock(LOCK_EX\|LOCK_NB)` on `MPM_SCHEDULER_LOCK` → `<workspace>/scheduler.lock` → `/tmp/mpm-scheduler.lock`. |
| Atomic wake claim | works | `dispatch.go:79-125` — `UPDATE … WHERE id = (SELECT … LIMIT 1) RETURNING`. Stamps `dispatched_at` + `metadata.dispatched_by` and deliberately leaves `fired=0`. |
| Atomic heartbeat swap | works | `state.go:96-131` — write `<target>.tmp` then `os.Rename`. |
| Active-uptime accrual | works | `active_uptime.go:98-117` → `internal/core/active_uptime.go:90-138`. Monotonic process elapsed, restart contributes nothing, fail-closed on malformed state. |
| Confidence decay trigger | works | `confidence_decay.go:60-118`; cadence-gated to 6 h, bounded 100/pass, min age 24 h, marker advances even on per-artifact failure. |
| Wake retention | works | `wake_expiration.go:106-252` (notification, 7 d) and `:444-564` (cron, 1 h) — retire, never delete, with per-row read-back assertions. |
| Cascade drain / reconcile | works | `cascade_drain.go:122-179` (30 s budget, batch 10, panic-safe); `cascade_wake_reconcile_handler.go:42-58` (crash-window recovery, 100/pass). |
| Drill harness | works | `drill_handler.go:51-247` — scripted conformance test scored from the tool-call audit; scorer is pure. |
| **Idle / dormancy concept** | **missing** | "Idle" appears only as prose about no-op ticks. No state machine, no inactivity threshold. |
| **Any review of past agent activity for gaps** | **missing** | No file under `internal/scheduler/` contains any SQL against `tool_invocations`, `system_audit_log`, `artifact_provenance` or `memories` for gap analysis. |
| **Retry / backoff on wake dispatch** | **missing** | `dispatch.go` has no retry counter, no backoff, no dead-letter. |
| **Workspace-scoped scheduler status** | **missing** | `cmd/mpm/handlers_info.go:417-429` — systemd-unit-based, workspace-blind. |

### 2.3 Telemetry — `internal/telemetry/`

**Verdict: a different substrate. Not reusable for lifecycle observations.** Concrete reasons:

* `internal/telemetry/frame.go:81-83` hard-rejects any `event_type` other than
  `invocation_completed`. A host lifecycle event would be `REJECTED` at parse time.
* `internal/telemetry/store.go:19-52` — one table, `telemetry_invocation`, invocation-shaped:
  model, provider, token counters, `started_at`/`completed_at`. **No event-type column, no
  payload column.**
* `internal/telemetry/cost.go` — pricing is external and applied at read time; the whole read
  layer is cost-oriented.
* Transport (unix socket, NDJSON, one-way) *could* carry another event type, but using it
  would mean redefining "frame" to mean "event" — the generic-telemetry-framework outcome
  the contract ruled out.

### 2.4 Evidence — `internal/core/evidence*.go`

| Property | Finding |
|---|---|
| Row shape | `internal/core/schema.go:267-287`. `artifact_id TEXT NOT NULL`, `artifact_type TEXT NOT NULL CHECK (artifact_type IN ('memory','theory','decision','lesson','work'))`, `type`, `source_group`, `strength REAL NOT NULL CHECK (-1.0..1.0)`, `independence_factor`, `created_by`, `created_at`, `expires_at`, `notes`, `reference_url`. **No foreign keys** (deliberate — `evidence_store.go:275-283`). |
| Creation | `evidence_store.go:66-86` (`addEvidenceInTx`) → `:200-331` (`AddEvidence`) → `evidence_tools.go:38-101`. Public entry: `mpm_evidence action=add`. Also written by the `memory_source_evidence_ai` trigger (`db.go:3606-3630`) and by `mpm_log_to_changelog` (`changelog_mcp.go:346-366`). |
| **Artifact must pre-exist** | `evidence_store.go:284-290` — `artifactExists()` rejects otherwise. **Evidence cannot stand alone.** |
| **Dedup** | **none.** PK `id` is freshly minted per insert (`evidence_store.go:292`); no content-hash column, no partial unique index. See §3.1. |
| `artifact_type` vocabulary | 5 members in SQL. The MCP schema advertises `skill` (`registry_list.go:249`) which the SQL CHECK rejects — a live tool-schema/DB mismatch. |
| Link to `tool_invocations` | **none.** No FK, no column, no join. `source_group='tool_invocation'` is a *label on how the evidence was obtained*, not a pointer. |
| `reference_url` | Optional, absolute http(s), ≤2048 B, never normalised, never fetched, **zero effect** on strength/confidence (`evidence_reference_url.go:8-23`, `:113-120`). |

### 2.5 Provenance — `internal/core/provenance*.go`

| Property | Finding |
|---|---|
| Table | `schema.go:489-530`. `UNIQUE (artifact_id, artifact_type)` — one provenance row per artifact. `actor_kind CHECK IN ('agent','human','import','system','unknown')`. |
| Writer | `provenance_db.go:59-153` — wrapped in `SAVEPOINT prov_rec`; a provenance failure never poisons the artifact transaction and never returns `error`; surfaced as `ProvenanceRecordResult{Recorded, ValidationReason, SQLError}`. |
| Stamped fields | `framework_name`, `framework_version`, `framework_adapter`, `provider_name`, `model_name`, `session_id` (legacy per-process), `invocation_id`, `parent_artifact_id`, `parent_invocation_id`, `actor_kind`, `actor_id`, `provider_metadata`. |
| **`framework_session_id`** | **NOT on `artifact_provenance`.** It exists only on `tool_invocations`, `system_audit_log` and `session_handoffs`. Host-owned, never synthesized. |
| **`mpm_session_id`** | **NOT on `artifact_provenance`** either — same three tables. |
| `source` ("call" / "direct") | **Not a provenance field.** It is `memories.metadata.source`, set in `memory.go:389-394`, `:526-531`, `:603-608`. **"direct" is written by no current code path** — `hunts.go:87` reads it, but today every new row lands in `"call"`, `"cli"`, `"human"` or `"topic"`, so `SurvivalAsymmetryHunt`'s `directS == nil` guard means it can no longer fire on current data. |
| Git provenance | **No git columns anywhere.** Deliberately removed: `db.go:6705-6758` is a tombstone explaining that ambient git auto-capture inferred a repository from cwd and fabricated a `git` evidence row. *"Git evidence without an explicitly identified repository is unavailable, not inferred."* |
| **External event id** | **No column or JSON field.** The only `event_id` TEXT columns are MPM-internal (`epistemic_cascade_outbox.invalidation_event_id`, `epistemic_provenance.event_id`). |

### 2.6 Confidence — `internal/core/confidence*.go`

* Formula: `confidence = sigmoid(log(initialOdds) + Σ(strength × independence × e^(−μ·ageDays)) − λ·tDays)`, μ = 0.005 (`confidence.go:119-138`).
* Scale **(0,1)**; initial: memory 0.8, theory 0.5, decision 0.6, lesson 0.7 (`confidence.go:16-29`); daily decay λ: decision 0.001, lesson 0.003, memory 0.01, theory 0.02 (`:64-77`).
* Single authoritative recompute: `evidence_store.go:359-362` → `:378-474`. Append-only `confidence_history` ledger with a CHECK-constrained `trigger` vocabulary (`schema.go:296-304`).
* Decay: `confidence_maintenance.go:156-213` — ≤100/pass, min age 24 h, **lessons excluded** (`:45-49`).
* **A proposal has no separate confidence concept.** A theory is an ordinary `memories` row with `collection='theories'`, `metadata.status='pending'`, confidence 0.5, computed by the *same* machinery as a memory. `ProposeTheoryWithExtras` writes **no evidence rows**, so a proposal's confidence only rises if someone attaches evidence separately.

### 2.7 Write paths, review and promotion

| Path | Dedup / idempotency | Evidence |
|---|---|---|
| `mpm_memory save` | **Idempotent.** `memory_tools.go:124-167` pre-check → `db.go:4170-4173` insert-time identity → race fallback `db.go:4203-4210`. Identity = `sha256(contentHash ‖ 0x00 ‖ json(tags) ‖ 0x00 ‖ json(stableMetadata))` (`db.go:6269-6287`), backed by `idx_memories_identity_live` (`schema.go:992`). Second save returns the same id with `duplicate: true`. | works |
| `mpm_lessons save` | **Idempotent in row count, not in state.** `db.go:5382-5397` — content-hash probe; a repeat **increments `reinforcement_count`** and returns the existing lesson. The dedup key ignores `type` and `tags`, and `lessons` has **no unique index on `content_hash`** — the probe is read-then-write, so it is race-prone. | partially works |
| `mpm_theories propose` | **Idempotent only when hypothesis + tags + collection match.** `epistemology_tools.go:136-205`; writes `metadata.status='pending'`, `validation_criteria`, `dependencies` (both JSON forms), `epistemic_provenance` citations, one `artifact_provenance` row, confidence 0.5. Atomic (one `WithTx`). | partially works — see §3.4 |
| `mpm_theories resolve` | `pending → proven \| disproven`, validated, `metadata.status` patched, transition detected by an in-tx `UPDATE … WHERE status='pending'` — a second resolve is a silent no-op success. | works |
| `mpm_evidence add` | **No dedup.** See §3.1. | missing |
| `mpm_memory promote` | Clears TTL, `ReinforceMemory(+9)`, sets `weight=10, is_long_term=1` (`memory_tools.go:1106-1149`). **No reviewer, no confirmation, no evidence requirement.** An ordinary agent-callable action. | **works — and is the problem** (§3.2) |
| `mpm_memory review` | **Read-only.** Lists LTM/high-weight memories not accessed in N days (`epistemology_tools.go:705-723`). Does not promote or record. | works |
| `mpm_request_review` | LLM reviewer fan-out (`handlers.go:5936-6024`). **Performs no DB writes and is not a gate on promotion.** | works |
| `mpm_handoff write` | UPSERTs `session_handoffs` (`handoff.go:107-224`) with `summary` (required), `commitments`, `open_questions`, `ended_state`, `mpm_session_id`, `framework_session_id`. **Records no artifact ids, no evidence, no invocation ids.** The handler **silently discards** the caller's `framework_session_id` and `mpm_session_id` (`handlers.go:4181-4183`) — and the schema does not declare them, with `additionalProperties:false`. | partially works |

### 2.8 The tool-call audit — `tool_invocations`

The **only** durable record of what an agent actually did. `schema.go:1327-1369`.

| Column | Fact |
|---|---|
| `id` | `TEXT PRIMARY KEY`, a fresh `uuid.NewString()` per invocation (`cmd/mpm/audit_hook.go:71`) — **not content-derived, carries no idempotency**. |
| `tool_name`, `action` | `NOT NULL` — the table is schema-constrained to *tool invocations*. |
| `invocation_id` | the substrate's cross-call correlation id. |
| `payload_hash` | `sha256:<hex>` over the JSON payload (`audit_hook.go:197`) — deterministic, **informational only, not a uniqueness constraint**. |
| `result_status` | `CHECK (result_status IN ('success','error'))`. |
| `outcome_class` | nullable; the closed `internal.ToolOutcomeClass` enum. |
| `started_at`, `completed_at` | **whole unix seconds** — up to four `opencode` invocations share one second in the live DB. |
| `mpm_session_id`, `framework_session_id` | host/workspace identity; nullable. |
| Writer | **best-effort.** `audit_hook.go` inserts; on failure it logs `slog.Warn` and returns — *"an audit failure … must NEVER block the underlying tool call."* |

### 2.9 Session identity — measured

| Path | `framework_session_id` populated |
|---|---|
| OpenCode plugin | **101 / 101** |
| `mpm-cli` | 0 / 5484 |
| MCP server | 0 / 26 |

Mechanism: the plugin's `buildProvenanceEnv(model, sessionID)` sets
`MPM_PROVENANCE_FRAMEWORK_SESSION_ID` (`agent_installation/mpm-opencode/src/index.v2.ts:229`),
read into `ActiveContext.FrameworkSessionID` (`internal/core/active_state.go:637`), written by
`cmd/mpm/audit_hook.go:140` and `cmd/mpm-mcp/audit_hook.go:112`.

**`mpm_session_id` is workspace-scoped, not per-conversation.** Every `opencode` row in the
live DB spans seven hours and shares one value (`mpm-89cf6d76785dfbb67dfa3bfa6f2c980f`)
across at least four distinct host sessions. It is allocated once per workspace in
`<workspace>/active.json` under a cross-process flock (`db.go:2468-2491`).

**Therefore: retrospective grouping must use `framework_session_id`.** A detector that groups
by `mpm_session_id` merges unrelated conversations and produces false gap detections.

### 2.10 The closest existing facility to the target capability — and why it is dead

`internal/telemetry/observe.go:35-93` — `HighTokenNoArtifactHunt`. Its own header states the
intent precisely:

> *"this hunt is an **anomaly detector, NOT an MPM ROI metric**. A session with high token
> burn and zero new artifacts may still have been valuable… The finding is observation;
> human arbitration decides."*

That is the exact shape of the target capability. **It cannot run.** Three independent
breakages, all verified:

1. **No producer.** The OpenCode plugin emits no telemetry frames; `invocation_completed`
   appears only inside telemetry's own parser (`frame.go:81`). `telemetry_invocation` is
   always empty, so the hunt's `min_invocations` gate can never be met.
2. **Wrong tool.** The artifact-count function shells out to
   `mpm call mpm_provenance {"action":"count_by_session"}` (`cmd/mpm-critic/main.go:152`,
   `cmd/mpm-telemetry/observe.go:88`), but **`mpm_provenance` is not a registered tool**.
   `cmd/mpm/call.go:143` returns `unknown tool`; `countFn` errors; `telemetry.Hunt`
   propagates; `runTelemetryHunt` **exits the whole critic process with status 1**.
3. **Wrong database.** The critic opens `filepath.Join(projectRoot, "telemetry.db")`
   (`cmd/mpm-critic/main.go:135`) while the collector writes `<workspace>/src/db/telemetry.db`
   (`cmd/mpm-telemetry/serve.go:43`).

Additionally the scheduler's `CriticAuditHandler` invokes `mpm-critic` with **no
arguments**, so `-telemetry-hunt` defaults to `false` and the path is never taken from the
scheduler at all.

**Verdict: the facility that is closest to the target does not work, and the reason it does
not work is not a missing feature but three ordinary defects.**

---

## 3. Demonstrated missing capabilities

Each item below was **demonstrated**, not inferred. All behavioural measurements were taken
in a disposable scratch workspace (`/tmp/opencode/session-b-verify-*`) with its own `HOME`,
`MPM_WORKSPACE` and `MPM_DB_PATH`, per `docs/dev/dogfooding-runbook.md` §5. The live
database was read read-only throughout.

### 3.1 Duplicate evidence inflates confidence — a trust-boundary defect

Two byte-identical `mpm_evidence action=add` calls against the same artifact produce **two
evidence rows**, and the artifact's confidence rises with each one.

```
theory confidence: 0.5  →  0.6224593183800414  →  0.7310585651678245
confidence_history: (0.622459318380041, evidence_count=1, trigger=evidence_added)
                   (0.731058565167825, evidence_count=2, trigger=evidence_added)
evidence rows for the theory: 2
```

Mechanism: `evidence` has no unique index beyond its PK, no content-hash column, and
`AddEvidence` mints a fresh id on every insert (`evidence_store.go:292`). The confidence
recompute sums every non-expired, non-`auto_capture` row (`evidence_store.go:511-554`).

**Consequence.** A periodic critic hunt that attaches evidence to a proposal accumulates
duplicate rows and **monotonically inflates that proposal's confidence on every cycle** —
repetition mistaken for corroboration. That is the exact opposite of the trust boundary
`mpm-event/0.1` M5 demands. The `auto_capture` trigger rows are excluded from scoring, so
the substrate's own capture path is unaffected; the defect is latent for any new hunt.

Contrast: `mpm_lessons save` is idempotent in row count and `mpm_memory save` is idempotent
by identity hash. **Only the evidence path leaks.**

### 3.2 The critic already auto-promotes trusted knowledge

`SurvivalAsymmetryHunt` and `WeakTheoryHunt` emit `mpm_lessons save` on **every cycle**, with
no evidence, no review and no pending state. `lessons_base` carries `rowid, id, type,
content, tags, reinforcement_count, source_session_id, created, content_hash,
retrieval_priority, importance, confidence` — **no status column**. A critic-written lesson is
therefore *immediately trusted* and *immediately retrievable* through `lessons_fts`.

This is in tension with M5's *"no automatic promotion"* and with *"neither the adapter nor the
critic promotes"*. It is a **pre-existing** behaviour, not something the event contract
introduces — but M5 defers to "the existing review and authority rules", and those rules
currently enforce neither evidence nor review on this path. Hence the tension referred to A
in §1.

Two further measurements sharpen it:
* `mpm_memory promote` is agent-callable with **no reviewer and no evidence requirement**.
* `mpm_request_review` performs **no DB writes** and is **not a gate** on promotion.

### 3.3 The proposal shape cannot carry resolvable evidence

M5 requires that a proposal cite resolvable evidence and that an unresolvable citation be
**rejected**. Today:

* `mpm_theories propose` accepts `hypothesis`, `validation_criteria`, `tags`, `dependencies`,
  `source_ids` — **there is no `evidence_ids` parameter** (`registry_list.go:80-88`).
* `source_ids` are written by `recordSourceCitationsNode` (`epistemology_tools.go:676-698`),
  which calls `recordProvenanceNode(node, src, "", downstreamID, downstreamType, downstreamID, "")`
  — `source_type` is **empty**, `event_id` is **hardcoded to the downstream id**.
* **The cited id is never validated to exist.** Measured: proposing a theory whose `source_ids`
  contains a `tool_invocations.id` succeeded with `"success":true`, and the citation landed as

  ```
  source_id = 93cb8205-3ae1-4595-a0a1-788e28e079bd   (a tool_invocations row)
  source_type = memory                                (MISLABELLED)
  downstream_type = theory
  ```

  The mislabelling is deterministic: `ResolveArtifactType` (`cascade_provenance.go:368-416`)
  fails on a non-artifact id, and `recordProvenanceNode` falls back to `"memory"`
  (`cascade_provenance.go:168-178`). **MPM cannot currently distinguish a citation of a
  memory from a citation of a tool invocation.**

**This is contract §9 Q4, answered:** *yes, the proposal shape must change.* And §9 Q5,
answered: *no — the `evidence` table's `artifact_type` vocabulary does not need a new member,
and `evidence` is the wrong home for this.* See §4.2.

### 3.4 Proposals are not idempotent once a cycle tag is present

`mpm_theories propose` is idempotent for identical hypothesis + tags + collection — two calls
returned the same id `11c65cb1b0625239` and left one row. But the identity hash includes
`json(tags)` (`db.go:6269-6287`), so a cycle-varying tag defeats dedup:

```
identical hypothesis, tags [...,"cycle_5"]  → id b783efc90b611b07
identical hypothesis, tags [...,"cycle_6"]  → id 15ac012a743c2343
theory rows: 3
```

`PoisonPillHunt` uses exactly this pattern (`hunts.go:373-378`). **Any new hunt that emits a
cycle-tagged proposal accumulates an unbounded stream of duplicate proposals, one per cycle,
each then needing its own review.**

### 3.5 `evidence`'s tool-schema / SQL mismatch

`registry_list.go:249` advertises `artifact_type` including `"skill"`; the SQL CHECK is
`('memory','theory','decision','lesson','work')` (`schema.go:270`). The tool schema offers a
value the database rejects.

---

## 4. Minimal recommended approach

### 4.1 The answer

> **The smallest change that lets MPM retrospectively recognize meaningful knowledge an agent
> failed to record, without inventing or automatically trusting it, is a single new critic hunt
> that compares `tool_invocations` against `artifact_provenance` — plus a typed, validated
> citation on the proposal it emits — and nothing else. No new event framework. No new table.
> No schema change.**

Why this is the smallest: **the two inputs and the join between them already exist, and the
join was measured working on live data.**

```sql
-- "artifacts produced by host session", measured against the live DB (read-only)
SELECT ti.framework_session_id,
       count(DISTINCT ti.invocation_id) AS invocations,
       count(DISTINCT ap.artifact_id)  AS artifacts
FROM tool_invocations ti
LEFT JOIN artifact_provenance ap ON ap.invocation_id = ti.invocation_id
WHERE ti.framework_name = 'opencode' AND coalesce(ti.framework_session_id,'') <> ''
GROUP BY ti.framework_session_id;
```

```
ses_e8c0b0018ffelRWTCMDRK6013G | 32 invocations | 4 artifacts
ses_e8cfc33ebffedrNxAlMOA3ASQv | 14 invocations | 3 artifacts
ses_e8827ae2dffeY07NGFUbGbTyF5 |  6 invocations | 3 artifacts
ses_e91587cc3ffellWkz2sRZdO2o2 | 21 invocations | 2 artifacts
ses_e8dea1636ffeF4t1WRV5QiOaSv | 12 invocations | 0 artifacts   <-- a real observed gap
```

`artifact_provenance.invocation_id → tool_invocations.invocation_id → framework_session_id`
is exactly the join MPM already uses for framework/model resolution (`db.go:6405`). **No new
persistence is needed to compute the comparison.**

### 4.2 The three changes, in order

**(a) A typed, validated citation on the proposal — no schema change.**

`epistemic_provenance` is already the substrate's typed, idempotent edge log:
`UNIQUE (source_id, downstream_id, event_id)` (`schema.go:451`), a **free-text `source_type`**
column, and `ON CONFLICT … DO NOTHING` on insert (`cascade_provenance.go:181-187`).
`ListDownstreamCitations` queries **by `source_id`** and filters on **downstream** type
(`cascade_provenance.go:244`), so a citation whose source is a `tool_invocations` row is
**never walked by the cascade materializer** — which only ever looks up citations starting
from an artifact id.

The change: give `mpm_theories propose` an optional `source_type` (or a parallel
`source_types` array), pass it through instead of `""`, and **validate each cited id against
that type before the insert**, rejecting the proposal when one does not resolve. Because
`ProposeTheoryWithExtras` is already one `WithTx`, a validation failure rolls the artifact
insert back with it.

*Why not `evidence`:* `evidence` requires the artifact to pre-exist, has no dedup (§3.1), and
its `artifact_type` vocabulary would need a new member for "this evidence is a lifecycle
observation" — a schema change, and the highest-collision file in the sprint. `epistemic_provenance`
gives the same properties **without a schema change**: typed source, idempotent insert,
already readable by an existing surface.

*Why this is required:* without it, M5's third clause is unimplementable and a proposal
cannot distinguish what it rests on. It is the **one** hard dependency in the whole plan.

**(b) A new hunt — `internal/critic/`, no schema change, no new tool.**

Reads `tool_invocations` × `artifact_provenance` (the §4.1 join), groups by
`framework_session_id`, and emits **one pending proposal** per qualifying session:
`mpm_theories propose`, `metadata.status='pending'`, confidence 0.5, **no automatic
promotion**, evidence attached through (a).

Three rules the hunt must obey, each derived from a measurement above:

* **Stable content.** No cycle tag in the hypothesis or tags — §3.4. The cycle number, if
  wanted, belongs in `validation_criteria`, which is not part of the identity hash.
* **No duplicate evidence.** Cite through `epistemic_provenance`, whose unique key makes
  re-emission a no-op — §3.1. Do **not** attach rows to `evidence`.
* **Degrade honestly.** If `framework_session_id` is absent (the CLI and MCP paths, §2.9),
  the hunt must skip the session rather than fall back to `mpm_session_id`, which merges
  unrelated conversations (§2.9).

**(c) The read surface for the proposals.**

`mpm_system action=critic_findings` already exists and already reads
`tool_invocations WHERE action='challenge'` (`handlers.go:7090-7133`). Extending it to
include the new proposal type is the natural home. **This is a tool-registry/handler change
and therefore Session A's to arbitrate** (`docs/dev/coordination.md` §2).

### 4.3 What this deliberately does *not* do

* **No new event framework.** The substrate already records what the agent did.
* **No new table.** The comparison is computable from existing tables at read time — the
  Projection Test applies and is satisfied.
* **No dependency on the event store.** The hunt cites `tool_invocations` ids, so it is
  implementable **before** contract §6.1 is decided. When the lifecycle-event store lands,
  the same typed citation mechanism accepts `source_type='lifecycle_event'` and the hunt
  widens — **without changing the proposal shape**. This is why the citation mechanism must
  be source-agnostic and typed from day one.
* **No scheduler dependency.** `mpm-critic` runs on demand against a pinned workspace (§0.1).
* **No automatic promotion.** The output is a *pending proposal* at confidence 0.5, exactly
  like `PoisonPillHunt`'s output. Promotion stays with the reviewing agent and A.

### 4.4 The honest limit of this approach

**It detects "MPM-mediated effort without MPM-mediated recording."** It cannot detect an
agent that did all its work outside MPM and never called MPM at all — such a session leaves
no `tool_invocations` trace. That is precisely the case the contract's lifecycle events
exist to close, and it is why §6.1 (storage) remains worth doing — but it is **not** on the
critical path for a first working capability, and the sprint should not block on it.

### 4.5 Sequencing

```
PR-1  (a) typed + validated proposal citation      ← no schema change, no new tool
PR-2  (b) the gap-detection hunt                    ← depends on PR-1
PR-3  (c) read surface for the proposals            ← A arbitrates the registry change
PR-4  contract §6.1 lifecycle-event storage         ← A decides; independent of PR-1..3
```

PR-1 is the enabling change and is the recommended first PR.

---

## 5. Required interface with Session C

Session C owns what the **host** can deliver. B needs three things, all of which are
questions for C, not new requirements.

| # | What B needs from C | Why it matters to B's work |
|---|---|---|
| **C-1** | **Does the OpenCode v2.1.4 host deliver a stable per-tool-call key** (a `callID` or equivalent) alongside `sessionID`? | It decides strict vs best-effort idempotency for `tool.outcome` (§3.5 of the contract). **B's detection does not depend on it** — B cites `tool_invocations.id`, which MPM mints itself. But it decides whether MPM's *own* event store can ever dedup, and the contract's §9 Q3 answer depends on it. |
| **C-2** | **Does the host deliver any event for a session that never called an MPM tool?** (session start/observe, session end, compaction) | This is the boundary of §4.4 — the class of gap B's hunt provably cannot see. It sets the value of §6.1. |
| **C-3** | **Does the host deliver a wall-clock timestamp on its events?** | If yes, it is a **new `meta` allowlist field in a 0.2 minor bump** — never a replacement for `observed_at_ms` (§1, M1). |

**The interface B asks C to keep stable**, so the two sides can be built independently:

* The **proposal citation** must accept a **typed source id** from a small set
  (`tool_invocation`, `lifecycle_event`, and the existing artifact types). C's event ids must
  therefore be **stable, resolvable strings** — which the contract's `ev1_`/`ev1b_` prefixes
  already provide. **B needs no change to the envelope for this.**
* C's events must carry **`framework_session_id`** (host-owned, never synthesized) — the
  contract already requires this, and B's §4.1 join depends on it.
* **B does not require the event store to exist** for PR-1..PR-3. C is not on B's critical
  path, and B is not on C's.

**B's answers to the contract's open questions for C:**

* §9 Q1 — *which host capabilities does v2.1.4 expose?* — C's to answer; B has no evidence.
* §9 Q2 — *does the host's plugin input carry a stable project id?* — **B's partial answer:**
  the git-remote derivation already yields one identical id across all three sprint
  worktrees (§1, M3), and the contract's §3.3 chain already prefers an explicit
  `MPM_PROJECT_ID` or `.mpm-project-id` over it. If the host supplies a stable project id
  that is **identical across linked worktrees**, it should supersede the derivation; if it
  **differs per worktree**, it must not be used, because it would reintroduce exactly the
  failure mode §3.3 was written to prevent. **C should measure that specifically.**
* §9 Q6 — *lifecycle events B needs that are not in §4's vocabulary* — **B needs none for
  PR-1..PR-3.** The §4 vocabulary covers what B will eventually cite.
* §9 Q7 — *is the 8192-byte envelope ceiling workable for the richest event B expects?* —
  **B's answer: yes.** The richest event B would reason over is a `tool.outcome`, whose
  allowlisted `meta` is bounded by the per-field limits (§3.8). B reasons over
  `tool_invocations` rows directly, not over envelope payloads, so the ceiling is not
  load-bearing for B.

---

## 6. Required coordination or schema decisions from Session A

| # | Decision | Why it is A's | Recommendation |
|---|---|---|---|
| **A-1** | **Adjudicate the M5 tension** (§3.2): does the critic's existing automatic lesson-writing count as "automatic promotion" that M5 forbids, or is it a pre-existing behaviour the sprint does not fix? | M5 is a shared-interface item and A arbitrates it. | Rule explicitly. Either M5 is amended to state that the critic may only write to a *pending* surface (which would require a status column on `lessons_base` — a schema change), or A records the behaviour as known and out of scope, with an explicit note that the event contract does not endorse it. **B's preference:** the second, for this sprint — but it must be recorded, not left implicit. |
| **A-2** | **Contract §6.1 — the storage path for lifecycle observations.** | Explicitly A's, after the B/C audits. | A new core-owned table. B's input: `tool_invocations` is unusable as-is (CHECK-constrained to tool invocations, `id` is a fresh UUID with no idempotency, `started_at` is whole seconds). Required columns: `event_id` **PRIMARY KEY** (strict idempotency), `idempotency` mode, `observed_at_ms`, `host_seq`, plus the envelope's identity fields, plus an index on the contract's ordering key `(observed_at_ms, host_seq, event_id)`. |
| **A-3** | **Contract §6.4 — the exact surface of the ingest entry point, the receipt and the rejection counter.** | A's; §9 Q8 asked A. | **`mpm_system`**, not `mpm_context` (§1, M6). A new action `mpm_system action=event_counters` alongside `critic_findings` and `query_audit_log`, or an extension of `health_check`. The receipt must be backed by a read-back, not by the `INSERT` result. |
| **A-4** | **Tool-registry change for the typed proposal citation** — `mpm_theories propose` gains an optional `source_type` / `source_types`. | `internal/core/tools/registry_list.go` is A's shared interface; both B and C depend on it. | Approve as a **single PR together with its core change**, per `docs/dev/coordination.md` §4.3. |
| **A-5** | **Tool-registry change for the read surface** — extend `mpm_system action=critic_findings` (or add a sibling action) to surface the new proposal type. | Same. | Same PR discipline. |
| **A-6** | **Environment wiring** — the OpenCode process does not inherit `MPM_WORKSPACE` / `MPM_DB_PATH` / `MPM_BINARY` (§0). | Deployment/wiring is A's. | Either launch OpenCode with those three variables, or record that sprint work runs through the pinned-CLI path and that the plugin's live-DB writes are accepted for the sprint's duration. |
| **A-7** | **Runbook correction** — the scheduler-status line is systemd-based, not process-based (§0.1). | `docs/dev/dogfooding-runbook.md` is A's document. | Correct §2.5/§8.5's mechanism description; the conclusion stands. |
| **A-8** | **Two pre-existing defects to triage** (not part of the sprint's scope unless A wants them): (i) `mpm-critic --telemetry-hunt` and `mpm-telemetry observe` call the non-existent tool `mpm_provenance` and exit non-zero; (ii) the critic opens `<root>/telemetry.db` while the collector writes `<workspace>/src/db/telemetry.db`. | Neither is B's scope; B is the intelligence core, and fixing the telemetry hunt would be a *new* capability, which §4 says is not wanted yet. | Triage separately. **Do not** fix by adding an `mpm_provenance` tool — that would resurrect a half-designed surface. If the hunt is ever wanted, it should be rewritten on `tool_invocations` per §4.2. |
| **A-9** | **`evidence` schema mismatch** — `registry_list.go:249` advertises `artifact_type='skill'`, which the SQL CHECK rejects (§3.5). | Registry is A's. | One-line fix either way; not urgent. |

**No `SafeMigrations` entry is requested by this report.** B's recommendation (§4.2) needs
**none** — which is the point. A's schema-collision risk stays closed.

---

## 7. Deterministic regression and behavioural acceptance tests

### 7.1 Deterministic regression tests (Go, `internal/core`)

| Test | Pins |
|---|---|
| `TestProposalCitation_TypedSourceTypeIsPersisted` | `mpm_theories propose` with `source_type='tool_invocation'` writes that exact `source_type` into `epistemic_provenance` (not the `"memory"` fallback). |
| `TestProposalCitation_UnresolvableSourceIsRejected` | a `source_id` that resolves to no row ⇒ the proposal is **rejected** and **no** theory row exists afterwards (the `WithTx` rolls the artifact back). |
| `TestProposalCitation_IsIdempotentAcrossCycles` | the same proposal + the same cited invocation ⇒ one `epistemic_provenance` row (`ON CONFLICT DO NOTHING`), one theory row, second call returns the same id. |
| `TestProposalCitation_DoesNotDisturbTheCascadeMaterializer` | a citation whose `source_type='tool_invocation'` is not walked by the materializer (the materializer only looks up by an artifact `source_id`). |
| `TestEvidenceDuplicate_DoesNotInflateConfidence` | two byte-identical `mpm_evidence action=add` calls ⇒ **one** evidence row and **one** confidence change. **Currently fails** (§3.1) — it is the regression that closes the trust-boundary defect. If A prefers to fix this by deduplicating at the `AddEvidence` layer rather than by a unique index, this test pins the behaviour either way. |
| `TestProposalWithCycleTag_StaysIdempotent` | two proposals with identical content but differing cycle tags ⇒ **one** theory row. **Currently fails** (§3.4). |
| `TestGapHunt_EmitsNoProposalWithoutHostSessionID` | a session with invocations but no `framework_session_id` produces **no** proposal (the honest-degradation rule). |
| `TestGapHunt_GroupsByFrameworkSessionID` | two host sessions with the same `mpm_session_id` are detected as **two** sessions, not one (§2.9). |
| `TestGapHunt_IsDeterministicAcrossRuns` | the same seeded `tool_invocations` + `artifact_provenance` ⇒ byte-identical proposals across two runs, in the same order. |
| `TestGapHunt_EmitsPendingProposalOnly` | the hunt's output is `metadata.status='pending'`, confidence 0.5, and creates **no** lesson, memory, decision or promotion. |
| `TestGapHunt_CitesOnlyResolvableInvocations` | every cited id resolves to a `tool_invocations` row. |

### 7.2 Behavioural acceptance tests (the sprint's real criterion)

| # | Scenario | Pass condition |
|---|---|---|
| **B-1** | An agent session reads MPM repeatedly and records nothing. | The hunt emits **exactly one** pending proposal naming the session and its invocation ids; the proposal is **not** promoted; the row count of `memories`/`lessons_base`/`decisions` is **unchanged**. |
| **B-2** | The same session, next cycle. | **No new proposal** (idempotent). `epistemic_provenance` row count unchanged. **Confidence unchanged.** |
| **B-3** | A session that records a memory. | **No proposal** for that session. |
| **B-4** | Two distinct host sessions sharing one `mpm_session_id`, one recording and one not. | Exactly **one** proposal, for the non-recording session only. |
| **B-5** | A session whose cited invocation id is deleted before the next cycle. | The proposal's citation is **rejected** on re-emission, and no second proposal is created. |
| **B-6** | The hunt runs twice in a row against the same workspace. | The second run produces **zero** new proposals and **zero** new rows in any table. |
| **B-7** | A reviewer accepts a proposal and promotes it. | Promotion is an explicit, separate action by a human or the reviewing agent. The hunt itself never promotes. |
| **B-8** | The hunt runs against the live workspace by accident. | It cannot — the hunt is read-only against `tool_invocations`/`artifact_provenance` and writes only through the proposal path, which is gated on the host session id and the cited-id resolution. |
| **B-9** | A session whose activity is entirely outside MPM. | **No proposal** — and this is recorded as the known limit (§4.4), not silently passed over. |

### 7.3 Regression tests for the defects found

| Test | Pins |
|---|---|
| `TestCriticTelemetryHunt_DoesNotCallUnregisteredTool` | the telemetry hunt must not shell out to a tool that is not in the registry. **Currently fails** (§2.10). |
| `TestEvidenceToolSchema_MatchesSQLCheckConstraint` | the `mpm_evidence` schema's `artifact_type` enum equals the SQL CHECK vocabulary. **Currently fails** (§3.5). |

These two are *defect pins*, not new capability. They belong in whichever PR A chooses to
triage them (§6, A-8/A-9).

---

## 8. Recommended first implementation PR

**Title:** `feat(critic): typed, validated evidence citations on proposals`

**Scope — one PR, no schema change, no new tool, no new table:**

1. `internal/core/epistemology_tools.go` — accept an optional `source_type` alongside
   `source_ids` on the propose path; validate each cited id against that type **before** the
   insert; pass the type through to `recordProvenanceNode` instead of `""`.
2. `internal/core/tools/registry_list.go` — the one registry change: the new optional
   parameter on `mpm_theories propose`.
3. `internal/core/tools/handlers.go` — read and forward the parameter.
4. The contract tests in §7.1 that belong to this change
   (`TestProposalCitation_*`, `TestProposalWithCycleTag_StaysIdempotent`).

**Explicitly NOT in this PR:**

* the gap-detection hunt (PR-2) — it depends on this PR landing;
* the read surface (PR-3);
* the lifecycle-event store (PR-4, A's §6.1 decision);
* any `SafeMigrations` entry;
* any change to `evidence`, `lessons_base`, `tool_invocations` or the `ToolOutcomeClass`
  vocabulary;
* any fix to the telemetry hunt's `mpm_provenance` defect (A-8 — separate triage).

**Why this PR first.** It is the **only** hard dependency in the whole plan. Without a
typed, validated, idempotent citation, M5's trust clause is unimplementable, the hunt's
proposals cannot say what they rest on, and the event store (PR-4) would have to invent a
second citation mechanism later. It changes **no** shared interface except one optional
parameter on one tool, and it needs **no** schema change — so it cannot collide with A's
migration authority.

**Second PR** is the hunt itself (PR-2), which is then ~150 lines in `internal/critic/` plus
its tests, and demonstrably answers the sprint's central question.

---

## 9. What B did not do, and why

* **No implementation.** This is an audit deliverable (`launch-gate.md` §3.3).
* **No merge, no deployment.** Only A merges to `main` and deploys.
* **No `SafeMigrations` entry.** B's recommendation needs none.
* **No tool-registry change.** The registry is A's shared interface; §6 lists what B needs
  approved.
* **No change to `~/src/mpm` or `~/src/mpm-opencode`.**
* **No write to the live database.** The live DB was read read-only throughout. The one
  plugin-mediated tool call this session issued landed in the live DB (§0) as a consequence
  of the environment defect, not as a development experiment; it was a `read_wake_context`-class
  read plus a `recent_activity` read, and it added one `tool_invocations` row and no
  knowledge rows.
* **All destructive measurement** was done in a disposable scratch workspace with its own
  `HOME`, `MPM_WORKSPACE` and `MPM_DB_PATH`.

## 10. Where this knowledge now lives

Recorded in the shared dogfooding workspace `~/.mpm-dogfood`:

| Artifact | id | What |
|---|---|---|
| decision | `3a987eca2c708fac` | the M1–M7 acknowledgement, item by item, with reasoning and evidence |
| lesson | `05794d3c4768693b` | duplicate evidence inflates confidence (defect) |
| lesson | `5f5cca04d7a9b434` | the telemetry hunt is dead in three independent ways |
| lesson | `18e213b3a4772350` | cycle-tagged proposals defeat idempotency |
| lesson | `6911879eb9b1eb91` | the critic detects degradation, never absence |
| lesson | `64abe497b6d98572` | the proposal shape cannot carry resolvable evidence (§9 Q4, Q5 answered) |
| lesson | `196d17dcc2a6bf6d` | `framework_session_id` population is path-dependent (101/101 plugin, 0/5484 CLI, 0/26 MCP) |
| lesson | `a43693e53d7e3792` | `mpm_session_id` is workspace-scoped, not per-conversation |
| lesson | `cc86a851339b1914` | the required MPM environment is not inherited by the OpenCode process |
| lesson | `560103b0df9cc187` | correction: the scheduler-status line is systemd-based, not process-based |

This report is committed on `dev/session-b-core` as `docs/dev/session-b-audit.md` and is
retrievable from any worktree with
`git show dev/session-b-core:docs/dev/session-b-audit.md`.
