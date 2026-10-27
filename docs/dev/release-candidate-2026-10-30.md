# MPM release-candidate readiness — Friday, 30 October 2026

**Scope.** An OpenCode-only release candidate. This report is the integration
and release authority's record of what was proven, what was not, and the
recommendation.

**Author.** Session A (integration and release authority).
**Date of the report.** 2026-10-27, for the Friday 30 October target.
**Nothing was deployed, tagged or published.** The live installation is
untouched: `MPM 0.1.0-dev+g3df72315`, `mpm-scheduler` MainPID 2804,
NRestarts 0, binary mtime 24 October.

---

## 1. The exact integrated state

| Item | Value |
|---|---|
| **Integrated main SHA** | **`4111c46dbe30de44e9ed8f7a715fff9e8c3e469e`** (`4111c46`, "fix(opencode): the lifecycle observer's delivery accounting must close (P4)") |
| Branch | `release/rc-4111c46`, tracking `origin/main` |
| Base of this sprint | `a8320fb` |
| Main CI run on that SHA | `40599592681` |
| Release-gate evidence | see §3 |
| Substrate binaries built from that SHA | `mpm`, `mpm-mcp`, `mpm-scheduler`, `mpm-critic`, `mpm-telemetry` — all five carry a build identity. The isolated install was verified against the exact **tree** `4111c46` contains: `git diff d04d390 4111c46` is **empty**, and the binary's embedded identity is `Commit=d04d3900…` |

### 1.1 The merged PRs

| PR | Merge SHA | What it is |
|---|---|---|
| #137 | `9406936` | Retire the legacy host adapters (OpenCode-only). |
| #138 | `7d18237` | The telemetry hunt becomes a **read-only diagnostic** until a citation can resolve. |
| #143 | `6e795ec` | The adapter payload is built **once, by the gate** — not by the test files. |
| #132 | `3de18a4` | **D6** — carry the invocation identity into every recording path. |
| #135 | `8ff4e7a` | **D7** — a Go-native `[]string` must not vanish at the parse boundary. |
| #140 | `a4e3da9` | The M5 integrity matrix, and D4's production-path proof. |
| #141 | `434e47e` | **D3-b** — a legacy citation must record the type of what it cites. |
| #144 | `ef7a535` | The trust-blocker table restated. |
| #145 | `a8320fb` | The merged state, verified. |
| **#142** | **`4111c46`** | **P4** — the lifecycle observer's delivery accounting must close. |

Also merged earlier in the same cycle: #130 (D1/D5 — the observation identity
for evidence), #133 (the installation-preservation carrier), #131, #136, #129.

### 1.2 The two PRs that were closed rather than merged

* **#126** — the legacy-adapter-removal *proposal*. **Approved** and closed as
  the historical decision record; its change set was implemented by #137 and
  all five of Session A's open questions are answered in
  `docs/dev/opencode-only-adapter-removal-implementation.md`. The ruling is in
  the PR's own comment thread.
* **#134** — the P4 delivery-counter discrepancy. **Closed as completed** by
  #142; see §7.1 for the ruling and the measurement.

---

## 2. The release gates on the exact SHA

| Gate | Result |
|---|---|
| `make test-race` (the canonical pre-merge validation) | **PASS** — see §3 |
| `make test-scripts` | **PASS** |
| `make test-agent-adapters` | **PASS** — Python 119 (floor 100), TypeScript 108 (floor 15), JavaScript an explicit declared-empty state |
| `mpm-lint --gate` | **PASS** |
| `python3 -m unittest scripts.tests.test_makefile_typescript_test_wiring` | **PASS** — 18 tests |
| Main CI `40599592681` (Full test suite / Build + core tests / Release gate) | **see §3** |

### 2.1 The release gate's own history in this sprint

The release gate went **red once**, and the cause was a test-infrastructure
defect, not a product regression:

* Main run `40564010549` (head `9406936`, the #137 squash) failed in the
  OpenCode TypeScript suite with every lifecycle-events test reporting
  `does not provide an export named 'ADAPTER_SOURCE'`.
* **Root cause:** three test files each ran a full `tsc` build into the same
  `dist/` directory while `node --test` runs the files in parallel processes.
  `tsc` truncates before it writes, so a reader that catches a file
  mid-rewrite sees a **valid module with no exports** — and the failure
  names a TypeScript symbol instead of the race.
* **Fix:** #143 — the gate builds the payload once, before any test file
  runs, and the tests only read it. The pin is
  `scripts/tests/test_makefile_typescript_test_wiring.py` (18 tests): no
  `*.test.ts` may invoke a TypeScript compiler.
* **Proof, not assertion:** four mutations were demonstrated to re-break the
  gate (re-add the self-build; delete the payload build; move the build after
  `node --test`; turn the empty-`dist/` state into a silent pass), and the
  suite passes 89/89 with `dist/` made read-only.

---

## 3. CI evidence

| Run | Head | Jobs | Result |
|---|---|---|---|
| `40599592681` | `4111c46` | Full test suite · Build + core tests · Release gate (cross-agent continuity + public-CLI subprocess suite) | **all three green** |
| `40591593093` | `a8320fb` | the same three | **all three green** |
| `40595867642` | `d04d390` (the #142 head) | the same three | **all three green** — this is the run that gated the #142 merge |

> The run on the exact integrated SHA is `40599592681`. The ledger's rule is
> that a CI result is tied to the commit it was run on: if the head moves
> after a green run, the run is stale.

---

## 4. Phase 2 — the reproducible release candidate (21 / 21)

**Method.** A completely isolated workspace: a disposable `HOME`, disposable
`XDG_*` roots, a disposable MPM prefix, and the repository's own binaries.
The systemd user-manager operations were neutralised by a `PATH` shim that
**records** the request and refuses to act on the real manager — enabling the
operator's unit is a deployment, and this sprint does not deploy.

**Three independent detectors** were used to prove the live tree was not
touched, because one is not enough:

1. a **snapshot** of the live binaries, the live database, the live unit file
   and the live OpenCode config, before and after;
2. a **`strace -f`** of every file access in the test's process tree, grepped
   for the live paths — this catches a *read* of the live tree that a
   snapshot cannot see;
3. the **systemd shim**, whose log records exactly what the installer asked
   the user manager to do.

**The results (21 checks, 0 failures).**

| # | Check | Result |
|---|---|---|
| 1 | The preflight passes | PASS |
| 2 | All five binaries are promoted **transactionally** | PASS |
| 3 | The installer's own `--validate` passes | PASS |
| 4 | The five binaries are installed and executable | PASS |
| 4b | The user-PATH symlinks exist | PASS |
| 4c | The data directory is `0700` | PASS |
| 5 | The OpenCode config was written | PASS |
| 5b | **Exactly the OpenCode plugin is registered — no retired adapter** | PASS |
| 5c | The registered plugin points at the **disposable** staged payload, and that payload exists | PASS |
| 6 | The installed binary carries the build identity (`Commit=d04d3900…` — the **tree** `4111c46` contains; `git diff d04d390 4111c46` is empty) | PASS |
| 7 | The installed binary answers a `health_check` against the disposable database | PASS |
| 7a | The `health_check` **names the disposable database**, not the live one | PASS |
| 7b | The database is created under the disposable data root | PASS |
| 8 | The managed `AGENTS.md` was installed | PASS |
| 8b | The managed block markers are present | PASS |
| 8c | The managed blocks reconcile to the canonical render (`render_managed_blocks.py --check` passes) | PASS |
| 9 | **The user's own content SURVIVES the upgrade** | PASS |
| 9b | The user's unrelated data survives the upgrade | PASS |
| 9c | The managed block is still intact after the upgrade | PASS |
| 10 | The live tree snapshot is unchanged | PASS |
| 11 | **Nothing in the test's process tree touched the live tree** (strace hits on the live paths: **0**) | PASS |
| 12 | The live scheduler unit is still readable | PASS |

**The upgrade path was exercised, not assumed.** The harness installs cleanly,
then appends a user-owned line to the managed file and writes an unrelated
data file, then **re-runs the installer** (a full upgrade) and re-runs the
adapter installer, and then asserts that the user's line, the user's file and
the managed block are all still there. This is the property #133's carrier
was written to protect, now confirmed end to end in an isolated install.

**What the systemd shim recorded** — the installer asked for exactly this and
**none of it happened**:

```
loginctl enable-linger v
systemctl --user daemon-reload
systemctl --user enable mpm-scheduler
systemctl --user is-active --quiet mpm-scheduler
systemctl --user restart mpm-scheduler
```

---

## 5. Phase 3 — the cross-session learning acceptance (27 / 27)

**Method.** The real OpenCode v2.1.4 host (`scripts/dev_host.sh`), the
repository's plugin and binary, **one shared disposable MPM workspace**, and
**two separate host-state roots** — so Session 2 is a genuinely separate
process with a genuinely separate in-memory context. Session 1 was
**terminated**; Session 2 was launched afterwards.

### 5.1 The results

| # | Check | Result |
|---|---|---|
| A1 | The memory artifact Session 1 created exists | PASS |
| A2a | An `artifact_provenance` row exists for it | PASS |
| A2b | The provenance row carries an invocation id | PASS |
| A2c | **The invocation id JOINS to a `tool_invocations` row** | PASS |
| A2d | The producing invocation identifies the **host** session | PASS |
| A2e | The invocation is attributed to the `opencode` framework | PASS |
| A3 | **Lifecycle events are durably recorded** | PASS |
| A3b | The stored events carry a host session id | PASS |
| A4 | **No sensitive raw host content reached the lifecycle store** (payload null; allowlisted meta only; no prompt or tool-output text) | PASS |
| A5a | The memory has a confidence to compare | PASS |
| A5b | **Replaying the SAME evidence does not increase confidence** | PASS |
| B1 | Session 1 and Session 2 are **different** host sessions | PASS |
| B1b | Session 1's host session is identified | PASS |
| B1c | Session 2's host session is identified and is **not** Session 1's | PASS |
| B2a | The witness probe observed Session 2's system prompt | PASS |
| B2b | **Session 2's system prompt CARRIED Session 1's fact** | PASS |
| B3a | Session 2 made its own `mpm_memory` call(s) | PASS |
| B3b | **Session 2 performed a RETRIEVAL, not a save** | PASS |
| B4a | Session 2 reported the theory's **pending** status | PASS |
| B4b | The proposed theory exists in the substrate | PASS |
| B4c | The substrate records the theory as **PENDING**, not trusted | PASS |
| B4d | The saved artifact is a **memory**, not a theory — the two are distinct | PASS |
| B5 | **Session 2 created NO new knowledge artifact** | PASS |
| B6 | **Session 2 USED the preserved fact** for the dependent task (`ANSWER=42`) | PASS |
| C0 | **The control session's system prompt WAS observed** (82 parts) — so the absence below is an observation, not a missing observation | PASS |
| C | **The NON-VACUITY CONTROL: with the retrieval contribution removed, the B2 assertion FAILS** | PASS |
| C2 | The control session could **not** answer the dependent question | PASS |

### 5.2 The non-vacuity control — and the design mistake the first run exposed

The first version of the control pointed at an **empty workspace**. The
control agent did not invent anything — it **routed around the control**,
found the seeded workspace on the filesystem, and retrieved the fact from it
with the MPM CLI. A control the agent can route around proves nothing.

The corrected control runs the Session 2 prompt in a **bubblewrap mount
namespace** in which the seeded root is masked with a `--tmpfs`. The fact
still exists in the substrate under test; the control session has **no route
to it**. The witness probe was moved **outside** the masked root — otherwise
the control observes nothing and the control becomes vacuous in the other
direction. Measured: the control's system prompt was observed (82 parts) and
the marker is **absent** from it, and the control could not answer.

### 5.3 One operator step, stated plainly

Durable lifecycle-event ingestion requires **`MPM_EVENT_INGEST=mpm_system`**
in the host's environment. See §7.2 — this is a product decision, not a
defect, and it was applied as an operator step for this acceptance run.

---

## 6. Phase 4 — the critic acceptance (41 / 41)

**Method.** The critic run manually against controlled fixtures in a
disposable workspace, five consecutive cycles, with a separate telemetry
store for the read-only hunt. The fixtures are seeded so that **every**
supported hunt fires: a survival-asymmetry cohort split (0.40 vs 0.95, ratio
≥ 2.0), a settled-but-stale memory with a baseline and an active-uptime row,
a weak pending theory plus a strong control, two sessions of which one
recorded nothing and one recorded an attributed artifact, and the poison pill
fired on the fifth cycle.

### 6.1 The results (36 verdict checks + 5 gate checks, 0 failures)

| Group | Checks | Result |
|---|---|---|
| **1. Pending, reviewable findings** | The hunts created **pending** theories; each of the five hunts fired (survival asymmetry, stale memory, weak pending theory, missed opportunity, poison pill); every theory row is pending or resolved | 7 / 7 |
| **2. Valid typed citations** | The citations are typed (`memory`, `theory`, `tool_invocation`); every citation carries a **known** source type; **every citation resolves** in the substrate | 5 / 5 |
| **3. No trusted lesson** | `lessons_base` = 0 after one run and after three; the critic made **no** `mpm_lessons` call; the lessons FTS index is empty | 5 / 5 |
| **4. No duplicates on repeated evaluation** | Exactly **one** proposal cites `th_weak_001`; exactly one cites `ti_quiet_0`; exactly one cites `mem_direct_001`; a further evaluation added no second proposal; the pending count **converged** (identical across the last two evaluations); the count stayed **bounded** across five evaluations; convergence did not come from duplicates | 7 / 7 |
| **5. No misidentification** | The session that recorded nothing **is** reported; the session that **did** record knowledge is **not**; the strong control theory is not cited; the productive session's invocations are not cited | 4 / 4 |
| **6. No manufactured corroborating evidence** | Every evidence row is an **observation** (`auto_capture`) — none supports a claim; the critic made **no** `mpm_evidence` call; the cited theory's **confidence did not move** | 3 / 3 |
| **8. The telemetry hunt is read-only** | It reports itself read-only; it reported an observation; the observation was **not** turned into a proposal; it wrote no lesson | 5 / 5 |
| **7. The P2b gates** | The producer-side gate **refuses** to arm a `critic_audit` wake; no such wake is persisted; the consumer-side dispatch gate **refuses** to execute one; the refusal is recorded as processed-and-refused; the `critic_audit` handler was **never** invoked | 5 / 5 |

### 6.2 The duplicate contract is **per observation**, not per total

This is the one measurement worth stating plainly, because the obvious
assertion is the wrong one.

The pending-theory count across five consecutive cycles was
**6 → 11 → 15 → 15 → 15**. It climbs, and then it **converges**. The reason
is structural: the weak-theory hunt legitimately observes the critic's **own**
new proposals — each new proposal is itself a weak theory at the base
confidence, so each is a **different observation** each time. The hunt's
`LIMIT 10` bounds the observed set, and the proposals it creates are stable,
so the observed set saturates.

A total-count duplicate assertion therefore **fails for the right behaviour**,
and the correct assertions are:

* **per observation** — exactly one proposal cites a given cited id
  (`th_weak_001`, `ti_quiet_0`, `mem_direct_001` each stayed at **1** across
  every cycle), and
* **convergence** — the count is identical across the last two evaluations and
  stays bounded.

The D4 correction is what makes the per-observation identity stable: the cycle
number lives in the finding's `Reason`, which is logged and never persisted,
so it is not part of the proposal's identity.

### 6.3 Why a critic finding cannot corroborate what it criticises

The evidence source group **`auto_capture` is excluded from confidence
scoring** (`scoringEvidenceClause`, `internal/core/evidence_store.go`). A
proposal that cites a theory creates an `auto_capture` **observation** row on
the cited artifact — a provenance record, not support. Measured: a theory
cited by the critic at confidence 0.2 stayed at **exactly 0.2** after five
critic runs.

### 6.4 Both P2b gates, exercised for real

* **Producer side:** `mpm_wakes schedule` with `metadata.kind = "critic_audit"`
  is **refused** (`wake kind "critic_audit" is blocked…`) and **no row is
  persisted**.
* **Consumer side:** a blocked wake was seeded **directly by SQL**, bypassing
  the producer — exactly the scenario the dispatch gate exists for (created
  before the gate, by an older binary, or by direct SQL). The real
  `mpm-scheduler` was run against the disposable workspace and the wake was
  **refused**: `wake kind "critic_audit" is blocked…`, the wake was marked
  fired **with the reason in `metadata.last_error`** (processed-and-refused,
  not an endless retry), and the `critic_audit` handler was **never
  invoked**.

**The P2b gate stays CLOSED.** Both halves are intact and both are now
exercised end to end, not merely unit-tested.

---

## 7. Phase 5 — failure injection and safety (38 / 38)

**Method.** Nine controlled failures against disposable workspaces. No
production database writes, no schema experiments, no live scheduler changes.
The live tree was snapshotted before and after and is **unchanged**.

| Group | Failure | Behaviour proved |
|---|---|---|
| **F1** | **Repeated lifecycle-event delivery** | The first delivery is accepted; the repeat is **accepted and reported as a duplicate** (`duplicate: true`, carrying the *original* timestamp); **exactly one row** is stored. Idempotent, and the repeat is not an error. |
| **F2** | **Unavailable ingestion endpoint** | A missing binary → `unavailable`; an unparseable response → `unavailable`; an endpoint that does not know the action → `unavailable`. **Never a false success.** |
| **F3** | **Invalid ingestion receipt** | `success` without a receipt → `malformed_receipt`; a receipt naming a **different** event → `malformed_receipt`; a receipt with wrong field types → `malformed_receipt`; a genuine refusal → `rejected`. **No faulty delivery wrote a row.** |
| **F4** | **Interrupted OpenCode session** (SIGKILL of the whole host tree, mid-session) | The database passes `PRAGMA integrity_check` (**ok**); **no partial or malformed lifecycle row** survived; the substrate is **still usable** afterwards. The in-memory queue is lost — honestly, not mis-recorded. |
| **F5** | **Missing invocation identifier** | With the provenance variables explicitly removed, the artifact is **not** attributed to a foreign framework (`mpm-cli`, the honest fallback), carries **no borrowed host session id**, and its invocation id **joins to a real `tool_invocations` row** — no dangling id. The write itself **succeeds**: a missing id is not a write failure. |
| **F6** | **Conflicting evidence** | Supporting evidence **raises** the confidence (0.800 → 0.9077); contradicting evidence **lowers** it (0.9077 → 0.8437); replaying the **same** evidence does **not** move it; an **unregistered `source_group` is refused** with a truthful, actionable error and the confidence does not move. |
| **F7** | **Stale plugin / binary; mispinned database** | With `MPM_REQUIRED_DB_PATH` naming a different database, the plugin **refuses to boot** — recorded by the host as `failed to load plugin … db_path invariant violated` — and it made **no** host-attributed call. A stale binary that does not speak the protocol is **reported**, not silently trusted. |
| **F8** | **Database unavailable / mispinned** | An unusable workspace exits **non-zero** with a truthful message (`fatal: stat mpm dir …`); a corrupted database file exits **non-zero** (`file is not a database`). |
| **F9** | **Failed or interrupted installation** | The promotion library **refuses** a missing candidate with exit **12** and leaves the destination **byte-identical** — nothing is touched before validation passes. The installer was **SIGTERM**ed at four different points: the prefix **never** held a mixed binary set (always 0 or 5). A clean install **after** the interruptions **succeeds** with all five binaries. |

### 7.1 The P4 delivery-counter discrepancy — **a documented diagnostic limitation, not a blocker**

**Ruling: closed.** The defect was a **diagnostic** defect, never a data-loss
defect: the wire contract, the mapping, the receipt and the endpoint were
correct throughout; only the client-side counter set could mislead an
operator.

The correction (merged as `4111c46`) separates **cumulative** counters from
**instantaneous** state, and the accounting now closes at every reading:

```
enqueued = delivered + failed + dropped_overflow + abandoned + queued + in_flight
```

Measured against a **real store** with the author's own harness:

```
RUN 1 — drain-all
  client: enqueued=10 delivered=10 queued=0 in_flight=0
  core  : stored_total=10
  balance: {'enqueued': 10, 'accounted': 10, 'balanced': True}

RUN 2 — stop-in-flight (THE REPORTED CASE)
  teardown reading: enqueued=10 delivered=9 queued=0 in_flight=1
  core            : stored_total=10
  final reading   : delivered=10 in_flight=0 balance={'enqueued': 10, 'accounted': 10, 'balanced': True}
```

The teardown reading now says **exactly what was true**: nine delivered, one
outstanding. The final reading says ten delivered, none outstanding,
**balanced**. The core held all ten throughout. The ambiguity between
"outstanding" and "lost" — the whole content of #134 — is gone. Issue #134 is
closed.

### 7.2 The one open decision: the lifecycle transport is off by default

**This is a product decision, and this report does not invent it.**

With `MPM_EVENT_INGEST` unset — the state of **every** installation, because
nothing in the repository, the installer, the runtime package or the SPEC
sets that variable — the adapter builds the envelopes, counts them, and
**drops every one of them**:

```
observer.counters: host_events_seen=101, envelopes_built=6, enqueued=0,
                   dropped_unconfigured=6, delivered=0, state={queued:0, in_flight:0}
lifecycle_events rows in the substrate: 0
```

With `MPM_EVENT_INGEST=mpm_system` the same run stores **4–10** rows.

**Why it is not a defect and not mine to close unilaterally:** the adapter's
own harness (`measure/p4_lifecycle_e2e.sh`) **pins** the default as a
contract — the default run asserts `delivered == 0` and
`dropped_unconfigured == envelopes built` — so flipping it is a coordinated
change by construction; and enabling ingestion by default changes what a
default installation **writes** to the operator's database.

**Why it is not a trust risk:** `event_ingest` writes **only** to
`lifecycle_events` plus the tool-invocation audit row. It creates no memory,
no theory, no decision and no lesson; it cannot arm the critic and it
touches no confidence value. The envelopes are `trust: "observation"` by
contract, and the adapter is structurally incapable of promotion.

**Three options were put to the owner on #142:**

| # | Option | Cost |
|---|---|---|
| 1 | The **adapter installer** sets the transport (config `env` or the installed package's runtime env), so the default install observes and the variable remains the override | Nothing changes about the transport's contract — only who sets it |
| 2 | **Flip the default** to `mpm_system` and update the pinned expectation in `measure/p4_lifecycle_e2e.sh` | One line in `src/index.v2.ts` plus the harness |
| 3 | **Leave it** and document it as an operator step | The release notes must say: *lifecycle-event ingestion requires `MPM_EVENT_INGEST=mpm_system` in the OpenCode host's environment* |

**For this release candidate, option 3's operator step was applied** and the
full cross-session acceptance passes on that path — including "the lifecycle
events are durably recorded" and "no sensitive raw host content reached the
lifecycle store". The feature is **verified working**; it is simply **not on
by default**.

---

## 8. The remaining known defects, and whether each blocks the release candidate

| # | Item | Severity | Blocks the RC? | Why |
|---|---|---|---|---|
| 1 | **D2-core** — the lesson surface has no pending state. `lessons_base` has no status column, so a row there is **trusted on arrival**. | Surface property, not a writer defect | **No** | No critic path and no agent path writes the lesson surface in this release. But it **must be in the release notes**: treat the lesson surface as **operator-only** until a pending state exists. Implementing it is a **schema change**. |
| 2 | **D3-a** — the legacy untyped citation path accepts an unresolvable id. | Accepted limitation | **No** | The critic uses the **typed** path on every hunt, and the typed path is verified to reject an unresolvable citation **atomically**. Ruling already in hand: step 1 (infer the type from an unambiguous resolution) approved as the next change — it cannot reject anything accepted today; step 2 (`mpm_decisions` gains `source_type`) approved in principle as its own shared-interface PR; step 3 (rejecting legacy ids) deferred behind a deprecation period. |
| 3 | **F2** — under contention the critic can **silently lose a finding** to `SQLITE_BUSY` (measured 2–4 of 8 concurrent proposals lost; reproduced under `make test-race`). | The most consequential residual | **No** for the RC; **Yes** for lifting the P2b gate | The critic is **gated**: the automatic path is refused at both the producer and the dispatch gate, and the manual path runs one agent at a time. But this is precisely why the P2b gate stays closed. The candidate fix is validated (`BEGIN IMMEDIATE`: 4/8 lost → 0/8) and deserves its own PR **before** the gate is lifted. |
| 4 | **D4 residual / F1** — the proposal identity treats the tag set as a **sequence**, so `["critic","alpha"]` and `["alpha","critic"]` are two proposals; the `m5contract` D4 test stays red pending a ruling. | Cosmetic | **No** | The production-path defect (a cycle-varying tag splitting every cycle's proposal) is **closed** — that was D4, and it is what makes the per-observation idempotency hold. What remains is a tag-ordering cosmetic. |
| 5 | **`mpm-telemetry observe`** (issue #139) — the same trusted-lesson exception, the same missing tool, and a store-path disagreement with the spec (`src/db/telemetry.db` vs the spec'd `<workspace>/telemetry.db`). | Advertised but inert | **No** | The command is currently **inert** — it calls `mpm call mpm_provenance`, a tool that **does not exist** in the registry, then calls `mpm_lessons` with a flat payload the params-envelope check rejects. It cannot write. **The ruling stands: read-only until a resolvable evidence source exists, and the store path must be settled first.** Repairing its count the way #138 repaired the critic's would **arm** a trusted-lesson write — the ordering rule that governs this release candidate. |
| 6 | **The lifecycle transport is off by default** (§7.2). | Product decision | **No** — with a documented operator step | It is not a defect: the feature is verified working, the default is pinned by the adapter's own harness, and the change is a coordinated product decision. **The release notes must state the operator step.** |
| 7 | The OpenClaw `created_by` literal (`"mpm-memory-openclaw-ingest"`) names a host MPM no longer supports, and the ingest handler now has no producer. | Deferred debt | **No** | Deliberately unchanged: the value is operator-visible data on `scheduled_wakes` rows, and renaming it must be **a data migration with its own decision**. |

**Nothing on this list is a trust-boundary violation, and none of them can
create a trusted lesson, a false citation, or a misattributed artifact.**

---

## 9. The recommendation

### **CONDITIONAL GO**

**The conditions are both documentation, and both are one line each.**

1. **The release notes must state the operator step:** lifecycle-event
   ingestion requires `MPM_EVENT_INGEST=mpm_system` in the OpenCode host's
   environment. (Decision pending with the owner; option 3 is the safe
   default because it changes nothing about what an installation writes.)
2. **The release notes must state that the lesson surface is operator-only**
   until a pending state exists (D2-core), and that the legacy untyped
   citation path remains a documented limitation (D3-a).

**Why not an unconditional GO.** Two things are not the integrator's to close,
and one of them is a behaviour an operator would otherwise discover by
surprise:

* the lifecycle transport's default is an open **product** decision that
  changes what a default installation writes;
* the lesson surface still publishes trusted knowledge directly.

Neither is a defect, and neither is a trust violation. But an unconditional
GO would mean shipping without telling the operator that a documented feature
is off by default, and that is exactly the kind of surprise a release should
not contain.

**Why not a NO-GO.** The primary objective is met and met on real host
behaviour:

* the system **observes** — lifecycle events are durably recorded, with host
  session ids and no raw host content;
* it **preserves** — the artifact survives, with provenance that joins to the
  producing invocation;
* it **retrieves** — a genuinely separate second session received the wake
  context carrying the first session's fact, performed a retrieval, and
  **used** the preserved information;
* it **evaluates** — every supported hunt creates pending, reviewable
  findings with typed citations that all resolve, creates **no** trusted
  lesson, creates **no** duplicate for the same observation, does **not**
  misidentify a session that recorded knowledge, and does **not** manufacture
  corroborating evidence;
* and it fails **truthfully** — nine injected failures, each with a bounded,
  honest outcome, and the live installation never touched.

### 9.1 What the operator decides next

| Decision | Owner | Options | Effect on the recommendation |
|---|---|---|---|
| The lifecycle transport default | the adapter's owner (Session C), adjudicated by A | 1 · 2 · 3 (§7.2) | Option 1 or 2 removes condition 1. Option 3 keeps it as a release-note line. |
| The lesson surface's pending state | A (schema change) | defer, or implement before the release | Implementing it removes the second release-note line. |
| Lifting the P2b gate | A, after F2 and D2-core | **not now** | The gate stays closed. Automatic critic scheduling is **not** part of this release candidate. |

**Nothing was deployed, tagged or published. No release was cut. The live
installation is byte-identical to its state before this sprint.**
