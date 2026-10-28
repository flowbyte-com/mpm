# Session B — Issue #139: `mpm-telemetry observe` safety and correctness

> **Scope.** This document reports on GitHub issue #139 — *"the remaining
> telemetry-observation trust-boundary exception"* — and on the technical
> correction shipped alongside it. It answers the six questions the issue
> asked, compares the result with the correction merged for
> `cmd/mpm-critic --telemetry-hunt` (#138), and **proposes** — but does
> not decide — the public CLI policy change that the issue's own framing
> requires an operator to own.
>
> **Nothing live was touched.** Every measurement below used disposable
> databases in temporary directories. No live telemetry ledger, no
> credential, no deployment, no scheduler configuration.

---

## 1. The issue in one paragraph

`mpm-telemetry observe` is the HighTokenNoArtifactHunt: it aggregates
token burn per session from the telemetry ledger, asks MPM how many
durable artifacts that session produced, and — for sessions with a high
burn and no artifacts — writes an **observation lesson** into the
substrate. `docs/SPEC.md` §6.6 describes exactly that, and calls the
lesson `type: "observation"`, *"descriptive, not prescriptive"*.

The write is the trust-boundary exception. `lessons_base` has **no
status column**, so a row there is **trusted on arrival** — the same
surface every other hunt was moved off in P3b. Session A's adjudication
of 2026-10-27 (#138) closed the identical exception in the critic. The
sidecar's copy was left behind, and it was left behind **broken**:
the command has never successfully run.

---

## 2. The six questions, answered

### 2.1 Why does the command call a nonexistent `mpm_provenance`?

Because the **spec** told it to.

`docs/SPEC.md` §6.6, verbatim:

> `observe` is the cross-DB anomaly detector. It aggregates token burn
> per session via the telemetry ledger, then asks MPM (via `mpm call
> mpm_provenance --payload '{"action":"count_by_session",...}'`) how many
> artifacts the session produced.

The registry publishes **21** tools (`internal/core/tools/registry_list.go`)
and `mpm_provenance` is not among them. It never was. Measured:

```
$ mpm call mpm_provenance --payload '{"action":"count_by_session","params":{"session_id":"ses-qualify"}}'
{"error":"unknown tool: mpm_provenance. available: mpm_memory, mpm_theories,
 mpm_decisions, mpm_lessons, mpm_topics, mpm_references, mpm_evidence,
 mpm_confidence, mpm_retrieval_diagnose, mpm_context, mpm_skills, mpm_wakes,
 mpm_handoff, mpm_scratchpad, mpm_system, mpm_log_to_changelog,
 mpm_request_review, mpm_resolve, mpm_blob_read, mpm_blob_search, mpm_work",
 "success":false}
```

And therefore, with a qualifying session in the ledger:

```
$ mpm-telemetry observe --dry-run --since 0 --threshold 100000
artifact_count for session ses-qualify: mpm call mpm_provenance: exit status 1 (output: {...})
$ echo $?
1
```

**Why the defect was invisible to CI.** The hunt's own doc comment said:

> The production implementation shells out to `mpm call mpm_provenance`.
> **Tests inject a fake.**

The test suite injected a fake `mpm` that answered `mpm_provenance` with
`{"count":0}`. The fake was self-consistent with a tool that does not
exist, so the suite passed and the command was dead in production. The
hermetic E2E (`scripts/smoke_telemetry.sh`) worked around the same
defect in the open: it used `--threshold 999999999` *"to avoid cross-DB
shell-out"*, so the broken path was never exercised either.

**A second, deeper reason the subprocess was the wrong shape.** Every
`mpm call` writes a `tool_invocations` row into `mpm.db` — *including a
call that only reads*. Measured on a disposable substrate:

```
before: tool_invocations rows: 1
after one `mpm call mpm_context --payload {"action":"query_global_rules"}`:
        tool_invocations rows: 2
        ('mpm_context', 'query_global_rules', 'success')
```

So a subprocess-based count **can never be a read-only count**. The
"obvious repair" — add `mpm_provenance` to the registry — would have
produced a command whose dry run wrote to the substrate. The count had
to move in-process, and it had to open the substrate read-only.

### 2.2 Why does the telemetry database path differ between code and spec?

**The code's path is authoritative; the spec is wrong.** The evidence is
in the git history:

| When | Commit | What it did |
|---|---|---|
| 2026-08-21 | `341f911` *feat(telemetry): implement serve + ping…* | the collector wrote to `filepath.Join(workspace, "telemetry.db")` — **the spec's path** |
| 2026-08-25 | `2a082bc` *feat(work): provenance env vars, wake-context format, and Claude Code integration* | moved **both** the collector (`serve.go`) and the reader (`query.go`) to `filepath.Join(workspace, "src", "db", "telemetry.db")` — as an incidental side effect of an unrelated feature. The spec was never updated. |
| 2026-08-27 | `664081e` *fix(telemetry): unify default socket path on runtime/ (F16)* | the same drift, again, for the socket: the code moved to `runtime/`, the spec still says `run/` |

`2a082bc` is a "feat(work)" commit. It moved a storage path for
reasons that had nothing to do with telemetry, and it moved it in a
way that silently invalidated a written specification. Nobody noticed,
because the reader moved with the writer: `mpm-telemetry query` kept
working. Only a **third** consumer — the critic — read the spec.

**The cost of the divergence was paid by #138.** The critic's telemetry
hunt, merged 2026-10-27, adopted the spec's path:

```go
telemetryDB := filepath.Join(projectRoot, "telemetry.db")   // cmd/mpm-critic/main.go:183
store, err := telemetry.Open(telemetryDB)                    // applies the DDL
```

`telemetry.Open` **applies the DDL**, so it *created* an empty
`telemetry.db` at the path nothing writes to — and the hunt then found
nothing, every cycle, forever. **The #138 correction was inert.** Its
test (`cmd/mpm-critic/telemetry_hunt_readonly_test.go`) seeded the
*same* wrong path the code used: self-consistent, and unrepresentative
of any real deployment.

> A second divergence in the same clause, found while fixing the first:
> the socket path. The code uses `<workspace>/runtime/mpm-telemetry.sock`
> (F16, `664081e`); the spec still says `$MPM_WORKSPACE/run/mpm-telemetry.sock`.
> It is not part of this correction's code change, but it belongs in the
> same specification amendment (§6 below).

### 2.3 Is dry-run genuinely read-only in every path?

**No. It wrote in two places.**

1. **It created the telemetry database.** `observe --dry-run` called
   `openDefaultStore()` → `telemetry.Open()` → `db.Exec(ddl)`. The DDL
   is `CREATE TABLE IF NOT EXISTS …`, so a dry run on a machine with no
   ledger **materialised an empty ledger**. A dry run that creates the
   database it is looking for is not a dry run.
2. **It wrote a row to the substrate.** The count came from
   `mpm call mpm_provenance`, and every `mpm call` writes a
   `tool_invocations` row (§2.1). The dry run's *only* subprocess was
   the count, and the count was enough to dirty the substrate.

The lesson write itself was *not* reached in dry-run — the loop printed
the payload and `continue`d — so the trusted surface was not touched.
But "the trusted surface was not touched" is a much weaker claim than
"nothing was written", and it was the only claim the old code could
make.

### 2.4 How does the default command reach `mpm_lessons save`?

The chain, before the fix:

```
runObserve (cmd/mpm-telemetry/observe.go)
  └─ telemetry.Hunt(ctx, store, cfg, countFn)
       └─ countFn  →  runMpmCall(ctx, l, "mpm_provenance", payload)   ← FAILS HERE
  └─ for each finding:
       payload = telemetry.FindingLessonPayload(f, cycleTag)
       lessonFn(ctx, payload)
         └─ runMpmCall(ctx, l, "mpm_lessons", b)
              └─ exec: mpm call mpm_lessons --payload {...}
```

The payload the hunt builds is **flat** — no `params` envelope:

```json
{"action":"save",
 "fact":"high-token-no-artifact: session ses-qualify burned 240000 tokens across 1 invocations with zero artifacts",
 "tags":["telemetry","high-token-no-artifact","auto","cycle_1793169503"],
 "type":"observation"}
```

The real surface rejects that payload **twice**:

```
$ mpm call mpm_lessons --payload '{"action":"save","fact":"…","tags":[…],"type":"observation"}'
{"error":"mpm_lessons: payload has top-level field(s) [fact tags type] outside the params envelope —
 expected shape {\"action\":\"<p>\",\"params\":{...}}. Move these into params.<key> and retry.","success":false}

$ mpm call mpm_lessons --payload '{"action":"save","params":{…,"type":"observation"}}'
{"error":"invalid lesson type \"observation\": must be one of warning, practice, or insight","success":false}
```

`observation` is not in `ValidLessonTypes` (`internal/core/db.go`:
`warning | practice | insight`). And with a type the surface *does*
accept, the lesson is created — **trusted on arrival**:

```
$ mpm call mpm_lessons --payload '{"action":"save","params":{…,"type":"insight"}}'
{"credited_sources":0,"id":"ba5577cdc8379708","reinforcement":1,"success":true,"type":"insight"}
```

**So the write path was doubly broken, and only the count defect kept
it from being reached.** Had the count been repaired alone, the hunt
would have attempted a trusted-lesson write and failed with a confusing
envelope error. Had someone "fixed" the envelope and the type together,
the hunt would have written a **trusted** `insight` — a claim about a
lesson, derived from a token count, with no citation and no evidence
that any lesson exists. That is precisely the failure #138 was closed
to prevent.

**The ordering lesson is the critic's, and it is right:** close the
trust boundary *first*, then repair the count. Repairing the count
against a substrate that still accepted a trusted write would have
*armed* the write, not fixed it.

### 2.5 Does an alternative output already represent an untrusted diagnostic finding?

**Yes — and there are two precedents, one of them merged.**

1. **`cmd/mpm-critic --telemetry-hunt` (#138, merged).** It computes the
   observation, reports it, and writes nothing. The report carries a
   `critic_cycle_<n>` correlation tag, so an operator can tie an
   observation to the cycle that saw it — the same correlation the old
   lesson tag provided, without the write. *This is the precedent the
   sidecar now follows.*

2. **`internal/critic`'s post-P3b hunt surface — the "pending
   proposal".** Every hunt that asserts something now emits a proposal
   with typed, resolvable citations.

**The telemetry observation cannot be a proposal**, for the two
structural reasons #138 established, which this work independently
re-confirmed:

- **There is no resolvable evidence source.** P2's citation vocabulary
  is `memory` / `theory` / `decision` / `lesson` / `work` /
  `tool_invocation`, and every one of them resolves *inside* `mpm.db`.
  The observation's own evidence — the token counts — lives in
  `telemetry.db`, a separate database with no foreign keys.
- **The observation is about an absence.** It fires when a session
  recorded *no* artifacts, so the artifact the observation is about does
  not exist, and there is nothing in `mpm.db` to cite. Citing an
  unrelated artifact would be inventing a citation.

So the correct destination for a telemetry observation is **a report** —
not a proposal, and not a trusted lesson.

The old `observe --dry-run` output was not a report. It printed the
*lesson payload*:

```json
{"action":"save","fact":"…","tags":[…],"type":"observation"}
```

A diagnostic shaped like the write it is not permitted to perform
advertises a write. The output is now a diagnostic observation
(§4.5).

### 2.6 Can the critic's `SessionArtifactCount` be reused safely?

**Yes — but only after the copies were counted.** The rule ("how many
durable artifacts did this framework session produce?") existed in
**three** places:

| Copy | Where | Form |
|---|---|---|
| 1 | `internal/critic/artifact_count.go` | a helper: `COUNT(DISTINCT ap.artifact_id)` over `tool_invocations LEFT JOIN artifact_provenance` |
| 2 | `internal/critic/missed_opportunity.go:303-314` | the same expression, inline, inside a `GROUP BY … HAVING` aggregate |
| 3 | the sidecar | needed |

Copies 1 and 2 **agree today**, but #138's claim that *"the two hunts
cannot drift"* was **not enforced** — they were separate SQL in
separate files, and nothing would have caught a divergence. Adding a
third would have made it worse.

**The correction:** the single-session rule now lives in
`internal/attribution` — a leaf package that depends on nothing but
`database/sql`, opens no database, and has no `init()`. Both the critic
and the sidecar call it. One implementation, one name, no drift
possible. `internal/critic/artifact_count.go` is deleted.

**Why not simply import `internal/critic` into the sidecar?** It would
have worked, and it was measured: `internal/critic` imports
`github.com/flowbyte-com/mpm-core`, so the sidecar would link the whole
substrate (**+0.9 MB**, 9.46 MB → 10.38 MB) and would inherit the
substrate's `init()` behaviour. `docs/SPEC.md` §6.6 says the sidecar is
*"deliberately ignorant of MPM's cognitive substrate"*. A leaf package
keeps that property while still reusing the one rule.

The measurement, in fact, came out **better than expected**. The
sidecar's binary is now **9,236,024 bytes** against **9,457,696** on
`origin/main` — **216 KiB smaller**, because the sidecar no longer
links `internal/critic` (and with it the substrate) and no longer
links `internal/runtimebin`. The correction did not buy the count with
a dependency; it removed two.

**Safety of the reuse itself:** the rule is a pure `SELECT` over two
tables. It writes nothing, takes no locks beyond a read, and does not
depend on the substrate's connection discipline. Both call sites open
their handle read-only (`mode=ro`), so the "read-only" claim is
enforced by SQLite, not by convention.

---

## 3. Comparison with the merged #138 correction

| | **#138** — `cmd/mpm-critic --telemetry-hunt` | **This change** — `cmd/mpm-telemetry observe` |
|---|---|---|
| Trust boundary | **closed** — reports, writes nothing | **closed** — reports, writes nothing; the default mode refuses explicitly |
| Count source | in-process, `critic.SessionArtifactCount` | in-process, `attribution.SessionArtifactCount` — the same rule, one implementation |
| Store path | **wrong** — `<root>/telemetry.db` (the spec's path); the hunt could never fire | canonical — `<root>/src/db/telemetry.db`, one shared resolver |
| Store open | `telemetry.Open` → applies the DDL → **creates** the store | `telemetry.OpenReadOnly` → `mode=ro` → cannot create or modify |
| Subprocesses | none | none (was: two) |
| Missing store | created it | *"no observations"*, not an error, and not created |
| Diagnostic output | a structured log line | a structured JSON observation |
| Writes to the substrate | none | none |

**The two corrections are now the same shape.** The difference is that
#138 fixed the trust boundary on a path that was inert, and this change
fixes the trust boundary *and* the path.

---

## 4. The technical correction

Everything in this section is a **defect correction**. None of it
decides the public CLI policy; §6 proposes that separately.

The work is split across **three stacked pull requests**, so that each
correction can be reviewed on its own and merged in a known order.

| PR | Base | What it carries |
|---|---|---|
| **#151** `fix/telemetry-canonical-store-path` | `main` | the store-path foundation, the critic's #138 path correction, and the query surface's write-capable open — **three independently reviewable commits** |
| **#150** `feat/session-b-139-telemetry-observe` | #151 | the observe hunt: the count in-process, the read-only substrate, the observation shape, the tests, this report |
| the policy PR | #150 | the read-only-by-default semantics, the SPEC §6.6 amendment, the real-executable regression coverage |

### 4.1 One path resolver — `internal/telemetry/paths.go` (new) — **#151**

`StorePath(workspace)` → `<workspace>/src/db/telemetry.db`;
`ResolveStorePath(workspace)` → the `MPM_TELEMETRY_DB` override, else
the canonical path, and a **clear error** when neither is available
(the result is never cwd-relative — the ghost-store shape removed after
the 2026-07-21 incident, lesson `59fe3f8ff3e1549e`).

Used by **all four** consumers: the collector (`serve.go`), the reader
(`query.go`), the critic (`cmd/mpm-critic/main.go`), and the sidecar's
observe path.

### 4.2 A read-only store open — `telemetry.OpenReadOnly` — **#151**

Opens with SQLite's `mode=ro`. It cannot create the file and cannot
modify it. A missing store is reported as `ErrStoreNotFound` rather
than materialised, and a store whose ledger table is missing is a clear
error rather than a confusing failure later in the hunt.

### 4.3 The critic's #138 hunt reads the canonical store — **#151**

The hunt looked for the ledger at `<root>/telemetry.db` — the **spec's**
path, which nothing writes to — and `telemetry.Open` created an empty
ledger there on every cycle, so the hunt reported zero observations
forever. It now uses the shared resolver and opens the store read-only;
a missing store is *"no observations"*, and #138's own test (which
seeded the same wrong path the code used) now seeds the canonical one.

### 4.4 The query surface no longer creates the store it reads — **#151**

`mpm-telemetry query` — and `cost`, which shares the helper — opened the
store with `telemetry.Open`, which applies the DDL. A read surface
manufactured the thing it read about, and could not distinguish *"the
collector has not run"* from *"the collector ran and recorded nothing"*.
Both surfaces now open read-only, and a missing store is a **clear
error** naming the path.

### 4.5 One attribution rule — `internal/attribution` (new) — **#150**

`attribution.SessionArtifactCount(ctx, db, sessionID)` — the single
implementation of the rule. `internal/critic/artifact_count.go` deleted;
both call sites updated.

### 4.6 A read-only substrate window — `cmd/mpm-telemetry/substrate.go` (new) — **#150**

`MPM_DB_PATH` → else `<workspace>/src/db/mpm.db`. **Never**
cwd-relative. Opened `mode=ro`, and never created. A missing substrate
is *"no substrate"* (reported on stderr, and every session counts as
zero-artifact); a substrate with no schema is a **clear error**, because
*"the substrate has no `tool_invocations` table"* and *"this session
produced nothing"* are different claims.

### 4.7 `observe` is now in-process and read-only — **#150**

- The count is computed in-process against the read-only substrate.
  **No subprocess is spawned, ever** — the `--mpm` flag is accepted
  (so an existing invocation does not break on an unknown flag), is
  **ignored**, and says so on stderr.
- `--dry-run` prints the **diagnostic observation**:

  ```json
  {
    "observation": "high-token-no-artifact",
    "status": "unverified-observation",
    "session_id": "ses-qualify",
    "invocations": 1,
    "input_tokens": 120000,
    "output_tokens": 120000,
    "total_tokens": 240000,
    "artifacts": 0,
    "detail": "high-token-no-artifact: session ses-qualify burned 240000 tokens across 1 invocations with zero artifacts",
    "wrote": "nothing"
  }
  ```

  It carries no `action` and no `type`: the diagnostic is not shaped
  like the write it is not permitted to perform.
- The **default mode refuses**, loudly, writes nothing, and exits
  non-zero with **stdout empty**. See §5 for why refusing is the
  conservative choice and what it preserves.
- `telemetry.FindingLessonPayload` — the builder for the forbidden
  write — is **deleted**.

### 4.8 The hermetic E2E now exercises the real path — **#150**

`scripts/smoke_telemetry.sh` used `--threshold 999999999` *"to avoid
cross-DB shell-out"*. It now runs the real hunt (a qualifying session,
the real count, the real output shape), asserts the observation is
reported, asserts the default mode refuses, and asserts the ledger's
row count is unchanged.

> The smoke test's threshold workaround and the fake `mpm` responder in
> the old unit tests were the two ways this path escaped being tested.
> Both are gone: the E2E uses a **reachable** threshold and the real
> count, and the unit tests use the **real** substrate attribution rule
> rather than a stub.

---

## 5. What was deliberately NOT changed — and why

The issue's framing is explicit: *"because the documented CLI currently
promises a writing operation, distinguish a technical defect correction
from a public behavioural change requiring operator approval."*

**The default mode's documented semantics — "write a lesson" — are NOT
implemented.** That is an operator decision. Until it is made, the
default mode **refuses**.

This is the conservative choice, and it is *observable-behaviour
preserving*:

| Situation | Before | After |
|---|---|---|
| Default mode, qualifying session | **exit 1**, nothing written, stdout empty | **exit 1**, nothing written, stdout empty, a legible reason on stderr |
| Default mode, quiet store | exit 0, silent | exit 0, silent |

The command has *always* failed on a qualifying session (the count
error). The refusal preserves that observable behaviour exactly and
replaces a misleading error with an accurate one. What it does **not**
do is quietly start writing trusted lessons — which is the whole point
of the issue.

**The dry-run output shape changed.** That is a consequence of the
trust boundary, not a policy decision: a read-only diagnostic must not
be printed in the shape of the write it is forbidden to perform. The
shape is proposed in §6.

**`--mpm` was kept, not removed.** Removing a flag an operator may
already pass is a breaking change; the removal is proposed in §6 and
scheduled for the next release.

---

## 6. Proposed amendment to `docs/SPEC.md` §6.6 — for approval

> **This section is a PROPOSAL. It is not implemented in this PR.**
> The operator must approve it before the spec changes and before the
> default mode's semantics change.

### 6.1 The store path (first paragraph)

**Current:**

> … a separate SQLite database (`$MPM_WORKSPACE/telemetry.db`), no
> foreign keys to `mpm.db`, …

**Proposed:**

> … a separate SQLite database (`$MPM_WORKSPACE/src/db/telemetry.db`,
> overridable with `MPM_TELEMETRY_DB`), no foreign keys to `mpm.db`, …

### 6.2 The socket path (wire-format paragraph)

**Current:**

> … over a Unix socket (`$MPM_WORKSPACE/run/mpm-telemetry.sock`, override
> via `MPM_TELEMETRY_SOCKET`).

**Proposed:**

> … over a Unix socket (`$MPM_WORKSPACE/runtime/mpm-telemetry.sock`,
> override via `MPM_TELEMETRY_SOCKET`).

### 6.3 The `observe` paragraph

**Current:**

> `observe` is the cross-DB anomaly detector. It aggregates token burn
> per session via the telemetry ledger, then asks MPM (via `mpm call
> mpm_provenance --payload '{"action":"count_by_session",...}'`) how many
> artifacts the session produced. Sessions with high token burn and zero
> new artifacts surface as `type: "observation"` lessons — descriptive,
> not prescriptive. The cross-DB call is parameterized so the test suite
> injects a stub; the production shell-out is in
> `cmd/mpm-telemetry/observe.go`.

**Proposed:**

> **`observe` is a read-only diagnostic.** It aggregates token burn per
> session from the telemetry ledger, then counts the durable artifacts
> the substrate attributed to that session. The count is computed
> **in-process**, read-only, against the substrate's own attribution
> rule (`internal/attribution`). There is no cross-DB subprocess,
> because every `mpm call` writes a `tool_invocations` row — even a
> read — and a diagnostic must not write.
>
> Sessions with a high token burn and zero attributed artifacts surface
> as **unverified observations**: a reported diagnostic, never a stored
> artifact. An observation is **not** evidence that a lesson exists. The
> token counts live in `telemetry.db`, which no citation type can
> address; and the observation is about an *absence* — a session that
> recorded nothing — so there is nothing in `mpm.db` to cite. Writing
> it would create a **trusted** lesson (`lessons_base` has no status
> column, so a row there is trusted on arrival). `observe` therefore
> **writes nothing**; arbitration is a human act.
>
> The observation is printed as a JSON document —
> `{"observation": "high-token-no-artifact", "status":
> "unverified-observation", "session_id": …, "invocations": …,
> "input_tokens": …, "output_tokens": …, "total_tokens": …,
> "artifacts": 0, "detail": …, "wrote": "nothing"}` — and carries no
> `action` and no `type`: a diagnostic is not shaped like the write it
> is not permitted to perform. `--dry-run` is retained as an explicit
> alias for the (now only) mode.
>
> The `--mpm` flag (which selected the binary used for the former
> cross-DB call) is **deprecated**: it is accepted and ignored, and
> will be removed in the next release.

### 6.4 The verification paragraph

**Current:**

> `scripts/smoke_telemetry.sh` is a hermetic end-to-end test: boots the
> collector in a temp dir, sends 3 synthetic frames + an idempotent
> retry + a conflicting duplicate + a bad-schema frame, runs `query` /
> `cost` / `observe --dry-run`, asserts the row count via sqlite. Exits
> 0 on success.

**Proposed:**

> `scripts/smoke_telemetry.sh` is a hermetic end-to-end test: boots the
> collector in a temp dir, sends 3 synthetic frames + an idempotent
> retry + a conflicting duplicate + a bad-schema frame, runs `query` /
> `cost` / `observe --dry-run`, asserts that a qualifying session is
> reported as an unverified observation, asserts that the default mode
> refuses to write, and asserts the row count via sqlite. Exits 0 on
> success.

### 6.5 The compatibility implications of making the default read-only

| Surface | Before | After | Classification |
|---|---|---|---|
| `observe --dry-run`, qualifying session | exit **1** (broken) | exit **0**, observation printed | **defect fix** |
| `observe --dry-run`, output shape | lesson payload (`action`, `type`) | diagnostic observation | **proposed change** (§6.3) |
| `observe --dry-run`, no ledger | exit 0 — it **created** one | exit 1, clear error, nothing created | **defect fix** |
| `observe --dry-run`, substrate rows | +1 `tool_invocations` row | none | **defect fix** |
| `observe` (default), qualifying session | exit 1, nothing written | exit 1, nothing written, clearer reason | **compatible** |
| `observe` (default), quiet store | exit 0, silent | exit 0, silent | **compatible** |
| `observe --mpm <path>` | selected the binary | accepted, ignored, noted | **compatible** |
| `observe` (default), *after* the amendment is approved | refuses | reports the observations, exit 0 | **proposed change** — the operator's call |
| `mpm-telemetry query` / `serve` | `<ws>/src/db/telemetry.db` | the same path, via one resolver | **compatible** |
| `mpm-critic --telemetry-hunt` | inert (wrong path) | fires | **defect fix** |
| the substrate | — | the sidecar opens it **read-only**, in-process | **new coupling**, read-only, no writes |

**The one genuinely breaking consequence** of the proposed amendment is
that `mpm-telemetry observe` (default mode) would go from *refusing* to
*reporting*, exit 0 instead of 1, on a store with a qualifying session.
Any automation that treats a non-zero exit from `observe` as "something
needs attention" would change meaning. That is the only consumer-visible
behaviour change the amendment implies, and it is the one the operator
is being asked to approve.

---

## 7. The tests

All tests use **disposable** databases. Nothing touches a live ledger,
a live substrate, a credential, a deployment or a scheduler.

| File | What it pins |
|---|---|
| `internal/telemetry/paths_test.go` | the canonical path; the override wins; an empty workspace is a **clear error**, never cwd-relative; the canonical path is **not** the spec's path (a regression guard) |
| `internal/telemetry/store_readonly_test.go` | `OpenReadOnly` **does not create** a missing store (`ErrStoreNotFound`); it reads an existing one; a write through the handle **fails**; a non-database and a directory are clear errors |
| `internal/attribution/session_count_test.go` | the rule: artifacts are attributed through the invocation; they do **not** cross sessions; the same artifact counted twice is one artifact; an empty session is zero; a nil handle is an error |
| `cmd/mpm-telemetry/observe_readonly_test.go` | the dry run **reports and writes nothing**; it **creates neither store**; the default mode **refuses** (stdout empty, no lesson, no `tool_invocations` row); a quiet store still succeeds; the count is the substrate's own attribution; **no process is spawned** and `--mpm` is ignored; an uninitialised substrate is a **clear error**; an absent substrate means zero artifacts |
| `cmd/mpm-critic/telemetry_store_path_test.go` | the hunt reads the **canonical** path and fires; the **legacy** path is never created; a missing ledger is "no observations"; the `MPM_TELEMETRY_DB` override is honoured |
| `cmd/mpm-critic/telemetry_hunt_readonly_test.go` (merged #138, amended) | the same contract, with the ledger seeded at the **canonical** path |
| `scripts/smoke_telemetry.sh` (hermetic E2E) | the real path end to end: observation reported, default mode refuses, row count unchanged |

**Non-vacuity.** Every "nothing was written" assertion is paired with a
"something was found" assertion — the hunt must have produced an
observation for the "nothing was written" claim to mean anything. The
fault-reinstatement evidence is in §8.

---

## 8. Non-vacuity: the faults, reinstated

Every regression test was checked against a **reinstated** fault, in an
isolated worktree (`git worktree add` at this commit), to confirm it
fails for the right reason. The reinstatements were mechanical
reversions of this PR's own changes, applied one at a time.

| # | Reinstated fault | Test that failed | Observed failure |
|---|---|---|---|
| 1 | the critic's hunt looks at `<root>/telemetry.db` again (the spec's path) and opens it with `Open` | `TestTelemetryHunt_UsesTheCanonicalStorePath` | `expected 1 observation from the canonical ledger, got 0 — the hunt is looking somewhere the collector does not write` |
| 2 | `observe` opens the ledger with `Open` instead of `OpenReadOnly` | `TestObserve_DryRunDoesNotCreateEitherStore` | `runObserve --dry-run succeeded with no telemetry store; it must fail clearly instead of creating the store` — i.e. the write-open **succeeded**, so the ledger was created |
| 3 | the count is taken from a subprocess again | `TestObserve_SpawnsNoProcessAndIgnoresTheMpmFlag` | `runObserve --dry-run = artifact_count for session ses-qualify: unexpected end of JSON input; the hunt must not need an mpm binary` — the hostile `mpm` on `PATH` was executed |
| 3 | (the same fault) | `TestObserve_CountIsTheSubstratesOwnAttribution` | `artifact_count for session ses-with-artifact: mpm call mpm_provenance: exit status 1 (output: {"error":"unknown tool: mpm_provenance …"})` — the original defect, verbatim |
| 4 | the default mode writes the lesson again | `TestObserve_DefaultRefusesAndWritesNothing` | `tool_invocations went from 0 to 1 row(s); the refusal must write nothing` — plus the three missing-message assertions |

**Fault 2 initially passed for the wrong reason.** The first version of
`TestObserve_DryRunDoesNotCreateEitherStore` did not create the
parent directory, so the reinstated write-open failed with *"unable to
open database file"* rather than creating the store — and the assertion
"the store must not exist" passed, proving nothing. The test now
creates the directory first, deliberately, and the reinstated fault
fails it. **A regression test that passes under the fault it exists to
detect is worse than no test**, because it converts a detection into a
false assurance; it was found by reinstating the fault, which is
precisely what the reinstatement is for.

Fault 4's `tool_invocations` delta is the trust boundary in one line:
the old write path left a row in the substrate even when the lesson
itself was refused.


---

## 9. Residual findings — reported, not fixed here

1. **The missed-opportunity hunt still carries its own copy of the
   attribution expression** (`internal/critic/missed_opportunity.go:303-314`,
   inline in a `GROUP BY … HAVING` aggregate). It agrees with
   `internal/attribution` today. Unifying it means restructuring an
   aggregate query, which is a different change with a different risk
   profile. **Recommended follow-up.**
2. **The query surface's write-capable open** was reported here as a
   residual finding and is now **corrected in #151**: the read surfaces
   open the store read-only, and a missing store is a clear error rather
   than a fabricated empty result plus a file on disk.
3. **The socket path divergence** (§2.2) is in the spec, not the code.
   The code is right (`runtime/`). It is included in the §6 amendment.
4. **`f.Reason` embeds the token count**, so a growing count yields a
   *different* reason string each cycle. That mattered for the lesson
   write (content-hash dedup would have created a new lesson per cycle
   rather than reinforcing one). It does not matter for a report, but it
   is worth knowing if the observation ever becomes a stored artifact.
5. **The sidecar now links the substrate's attribution query.** It is a
   leaf package with no substrate dependency and no `init()`, and the
   sidecar's binary actually got **216 KiB smaller**. The spec's §6.6
   wording ("deliberately ignorant of MPM's cognitive substrate") is
   about *data*, not code, and remains true — but a reader who assumed
   a code-level independence should know that the sidecar now knows one
   query about the substrate, and that query is a read.

---

## 10. The one-line summary

`mpm-telemetry observe` never worked: it called a tool that does not
exist, read a path nothing writes to, created the database it was
looking for, and would have written a **trusted** lesson had any of
those been repaired alone. It is now a read-only diagnostic that
resolves one canonical store path, counts artifacts in-process against a
read-only substrate, writes nothing, and refuses the default write with
an explanation — and the specification amendment that would make the
default mode's new semantics official is **proposed for approval, not
applied**.
