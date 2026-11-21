# Continuity 001, PR 4 — the bounded autonomous dispatcher

## 1. What PR 4 is, and what it is not

PR 3 gave MPM a way to RUN an assignment: it claims the work, mints a
session, persists the intent, launches a contained OpenCode process and
records what the runtime said. That path is explicit — the operator names
the assignment.

PR 4 adds the piece that was missing: someone has to DECIDE which
assignment runs next. That is the dispatcher, and this document is its
contract.

**It is a coordinator.** It finds work MPM already knows about, proves
the tree authorises it, claims it through PR 2's fenced API, and hands
it to PR 3's adapter. It never runs a model itself and it never creates
an assignment.

**It is not a daemon.** It is a bounded `run once`. The operator runs it
from their shell, or the scheduler's tick handler runs it — and the tick
handler is inert unless the operator has enabled it.

**It is not a root creator.** A root is always operator-created. The
dispatcher cannot create one, cannot widen one's budgets, and cannot
grant an approval.

**It does not invent work.** An assignment that is not eligible is
DEFERRED with a reason, never made eligible because the dispatcher had
nothing else to do.

### The two things it must never do

1. **Launch work the operator did not authorise.** Every dispatch is
   justified by the root's own permitted operations and eligible
   recipients, and the service's own budget checks bind it.
2. **Dispatch the same work twice.** A session that might still be
   running is a blocker, not an invitation.

## 2. The three pieces

| Piece | Where | What it is |
|---|---|---|
| The engine | `internal/core/dispatch` | the coordinator: discover, claim, dispatch, report |
| The operator's door | `cmd/mpm/assignment_dispatch_cmds.go` | `mpm assignment dispatch` and `dispatch-status` |
| The scheduler's door | `internal/scheduler/dispatch_tick.go` | one bounded pass per tick, INERT by default |

Both doors use the SAME engine and the SAME configuration file. A tree
behaves identically whether the operator asked for the pass or the
scheduler found time for one.

## 3. Off by default, and the off is proven

The switch is a FILE: `~/.mpm/dispatch.json` (or whatever
`--dispatch-config` names, or `MPM_DISPATCH_CONFIG` for the scheduler).

| The file | What the dispatcher does |
|---|---|
| missing | **nothing.** No row is read, no runtime is built, no session is created. |
| present, `enabled: false` | nothing, the same way. |
| present, `enabled: true`, complete | one bounded pass per invocation. |
| present, `enabled: true`, incomplete | **refused.** A dispatcher that invents its own limits is worse than no dispatcher. |
| present, unreadable or malformed | **refused,** and the error names the file. |

The disabled path is proven by a test that counts the runtime factory's
calls: a disabled dispatcher builds no runtime, because a runtime is
what can spend money.

There is no environment variable that turns it on by accident, and
"installed" is not "enabled".

## 4. The configuration

```json
{
  "enabled": true,
  "concurrency": 1,
  "owner": "mpm-dispatcher",
  "lease_seconds": 1800,
  "runtime_timeout_seconds": 600,
  "lease_margin_seconds": 300,
  "max_dispatch_attempts": 3,
  "retry_backoff_seconds": 30,
  "candidate_limit": 50,
  "drain_timeout_seconds": 120,
  "drain_poll_interval_seconds": 5,
  "workspace_root": "/var/lib/mpm/workspaces",
  "worker_agent": "mpm-worker",
  "reviewer_agent": "mpm-reviewer",
  "reviewer_model": "minimax-coding-plan/MiniMax-M3.1-Flash",
  "review_repo_path": "/srv/repo",
  "model": "mpm-sidecar/permitted-model"
}
```

EVERY DURATION IS IN SECONDS. A configuration file that mixes seconds
with `"10m"` is a file that half of MPM cannot read, and a duration this
file cannot parse is a duration this file does not have. The shape above
is pinned by a test, so the documentation cannot drift away from what
MPM accepts.

`concurrency` is how many assignments ONE run dispatches. The tree's own
`max_concurrency` is the DURABLE one, and the service enforces it
atomically (below), so a second dispatcher cannot exceed it whatever
this says.

`runtime_timeout` and `lease_margin` decide the lease: the lease must
cover the runtime window plus the margin, and a configuration whose
lease does not is REFUSED. The margin is the headroom the adapter needs
to observe the outcome and write it down.

`workspace_root` is where per-assignment worktrees live. The dispatcher
does not choose a workspace it was not told about, and the worktree is
the assignment's own directory inside that root — so a retry finds the
same tree.

`review_repo_path` is the repository a reviewer examines. It must be a
clean tree, because the review path pins an exact revision.

`worker_agent` and `reviewer_agent` are recorded so an operator can SEE
that the reviewer is not the worker. They are also **PINNED**: the
configuration refuses any other value, because the runtime's allow-list
names exactly the two definitions MPM ships. A configuration naming a
third one would be refused by the runtime — after the claim, and before
any attempt exists — and a refusal that creates no attempt never trips
the dispatch-attempt cap, so the assignment would be retried forever
while its live claim starved the tree's concurrency budget. Refusing the
configuration is the same decision, made before anything is claimed.

`model` is the WORKER's model and `reviewer_model` is the REVIEWER's,
and they are different on purpose. The worker's model is the sidecar's
local id, which only means anything inside the worker's namespace with
the sidecar's socket. The reviewer's sandbox has no provider and no
sidecar, so it names a real provider/model — and naming the worker's id
there would ask the reviewer for a model its own configuration does not
define.

**AN UNKNOWN KEY IS AN ERROR.** A silently ignored key is a typo the
operator cannot see: they wrote `lease_second` and the dispatcher used
the default, and the difference between the two is the difference between
a lease that covers the runtime window and one that does not. So is
**CONTENT AFTER THE OBJECT**: a decoder consumes one value and says
nothing about what follows it, and a configuration whose meaning depends
on where the reader stopped is not a configuration.

**THE PATHS MUST BE ABSOLUTE.** The sandbox roots are built from
`workspace_root` and `review_repo_path`, and the containment resolves them
against the filesystem — a relative path would make the containment's own
guarantee depend on the working directory. The check is here, before
anything is claimed, and not at the factory afterwards.

## 5. The budgets are the service's business

PR 1 stored three budgets on the root and deliberately deferred their
enforcement. PR 4 enforces them — in the SERVICE, not in the dispatcher.

```
max_tasks           the number of assignments in the tree (the root included)
max_concurrency     the number of assignments holding a LIVE claim
max_resource_units  the sum of the tree's DECLARED resource_units
```

### What `resource_units` means

It is a **declared reservation**, fixed once when the assignment is
created. The schema says it plainly: "what this assignment costs against
the root's cumulative budget."

It is NOT a measurement of provider spend. MPM does not meter tokens,
and the sidecar's audit records request and response SIZES, not cost.
So this is a ceiling on the work the OPERATOR AUTHORISED, and it must
not be described as a verified monetary or token-spend ceiling.

### A retry does not charge again

A retry is a new ATTEMPT on the same assignment row, and the reservation
lives on the row. One logical task is charged once.

### Why the checks are in the service

A budget check that reads a count and then writes can be crossed: another
dispatcher, or a direct caller of the assignment API, can add a task
between the two operations, and both writers then believe they were
inside the budget.

So the count and the write are ONE STATEMENT. The `INSERT` carries the
budget condition in its `WHERE` clause and the claim `UPDATE` carries it
in its own, so the database evaluates the count against the same
snapshot the write lands on. A writer whose snapshot goes stale is
refused with `SQLITE_BUSY_SNAPSHOT` and the transaction is retried
against a fresh snapshot, which re-evaluates the count.

That is why the checks live in `internal/core/assignment` and not in the
dispatcher: **a dispatcher-side check protects only the dispatcher.**

A LIVE CLAIM is a claim whose lease has not expired AND whose work is
unfinished. The state filter matters: a finished assignment keeps the
lease it was given, so counting leases alone would fill a tree with
FINISHED work and refuse the next real task.

### The mutation probes

Both atomicity claims are proven by breaking them, with deterministic
barriers so the race cannot be hidden by the schedule:

| Mutation | Result |
|---|---|
| read the concurrency budget OUTSIDE the transaction | `8 claims crossed a max_concurrency of 2` |
| read the task budget OUTSIDE the transaction | `6 delegations crossed a max_tasks of 2` |
| remove the resource condition | the resource test fails |

An earlier probe — a read-then-write pre-check INSIDE the transaction —
did NOT fail the tests. That is worth stating plainly: SQLite's write
serialisation plus `WithTx`'s `SQLITE_BUSY_SNAPSHOT` restart already
make that shape safe here. The statement-level condition is what makes
it robust rather than accidentally correct, and the honest claim is
"the condition is evaluated by the writing statement", not "a
read-then-write is definitely broken".

## 6. The lease, and why there is no renewal

PR 2's `Renew` advances the claim epoch. PR 3's attempt writes are
fenced on the epoch. So a renewal in the middle of a run would make the
running attempt unable to record its result — and the next dispatch
would duplicate the work.

**So the dispatcher does not renew during execution.** The lease is
sized ONCE, up front, to cover the whole runtime window, and the
configuration refuses a lease that cannot. Work that does not fit is
REFUSED rather than started.

The relationship, stated once:

| Term | Meaning |
|---|---|
| claim epoch | the fence. Every write carries it, and a stale one matches no row. |
| lease | how long the claim is good for. It is sized to the runtime window. |
| timeout | how long the runtime may run. It is shorter than the lease. |
| renewal | NOT USED during execution. It advances the epoch, which would fence out the running attempt. |

**THE DISPATCHER ALWAYS CLAIMS.** It does not try to recognise "a lease
I already hold" by the owner string: the operator's one-shot and the
scheduler's tick share that string, and a second one would then resume
an attempt the first one is still working on — both would prompt the same
recorded session. So every dispatch takes a claim, and PR 2's
compare-and-swap decides: one claim per epoch, whoever asks first.

**A LIVE LEASE IS NEVER RECLAIMED**, by anybody. The assignment waits for
it to expire, which is the safe direction in both directions: an expired
lease is not evidence that the old process stopped, and a live lease is
not evidence that it did.

## 7. Recovery

A dispatcher run can be interrupted at four places, and each leaves a
different record:

| The crash | The record | What the next run does |
|---|---|---|
| after the claim | a lease, no attempt | the lease expires, the assignment is reclaimed and dispatched |
| after the intent | a durable session id | the lease expires, the assignment is reclaimed, and the recorded session is INSPECTED before anything starts |
| during the execution | a session that may be working | a BLOCKER: no second writer, an operator-visible note |
| after the runtime ended | an outcome nobody recorded | reconciled through the existing fenced path, when the attempt is at the assignment's current epoch |

The inspection is what decides, not the epoch arithmetic. A session the
runtime has never heard of was never created, so re-running the objective
cannot duplicate work; a session that exists and has not reported an
outcome may still be working, and that blocks; a runtime that cannot be
asked at all blocks too, because nobody can then say whether the session
is still running.

### What is never done

- A live lease is never reclaimed.
- An outcome is never INVENTED. A runtime that cannot be asked is
  AMBIGUOUS, and an ambiguous attempt stays ambiguous until a human or a
  later observation resolves it.
- A session is never "stopped". There is no verified host operation that
  safely ends an OpenCode session, and `session.abort` is not one MPM
  may depend on. A session that may still be working is a BLOCKER, and
  the dispatcher says so rather than pretending.

### The drain

The one thing a dispatcher CAN do about a running session is WAIT for it
to report a terminal outcome, and then reconcile that outcome through
the existing fenced path. The drain is bounded by `drain_timeout`, and
it ends either in a reconciled record or in a refusal with an
operator-visible note.

### The operator-visible state

`awaiting_operator` is unreachable from `in_progress` on the role paths,
so the recovery state is expressed as a durable `Note` on the
assignment's ledger, and `mpm assignment dispatch-status` lists it under
**recoverable**. The assignment is NOT moved: inventing a transition to
carry this would be a change to PR 1's contract.

The drain window is REAL time, not the injected clock. The injected clock
governs the lease — when a claim expires and when an epoch moves. A drain
is a WAIT on an external system, and a wait bounded by a clock that
never advances is a wait that never ends.

The recovery pass reports only what it BLOCKED and what it RECONCILED. An
assignment it merely observed is left to the dispatch path, so every
assignment appears in the report exactly once.

**ONE ASSIGNMENT, ONE OUTCOME — AND NO ATTEMPT IS LOST TO A BLOCKER.**
An assignment can carry more than one outstanding attempt — PR 3's
reconcile creates a new attempt when the epoch advances, and the older
one stays outstanding. EVERY attempt is inspected, because an older
session may still be working, and every reconcilable outcome is
recorded: **a blocker is not a reason to lose another attempt's
record**, because the reconciliation IS the write that records a
runtime's outcome, and a write that never happens is work lost rather
than a report tidied up. The REPORT says one thing about the
assignment — the blocker, because that is what the operator has to deal
with — and it says what else was reconciled.

The drain's two endings are kept apart: the WINDOW CLOSING is the drain's
own end, and the CALLER CANCELLING is somebody stopping the run on
purpose. A cancelled run is checked BEFORE an unreachable runtime,
because a command on a cancelled context fails, and reporting that as
"the runtime could not be asked" would be a lie about the runtime. A
cancelled run writes NO note: a note that says "recovery required" for
a deliberate stop would train the operator to ignore the notes that
matter.

## 8. One runtime per assignment, per pass

The sidecar takes an **exclusive lifetime lock** on its socket path, and
the socket path is per assignment: `<mpmDir>/opencode/runs/<id>/sidecar.sock`.
A second sidecar for the same assignment in the same process is therefore
refused by the sidecar's own lock — "another sidecar holds …; refusing to
take it over" — even though the first one is THIS process's own.

And the dispatcher asks for the same assignment more than once per pass:
the recovery pass inspects it, and the dispatch path may then launch it.
Without a cache the second request is refused, the recovery pass can never
reconcile an implementer, and an implementer with an outstanding attempt
is refused FOREVER — a refusal that creates no attempt, so the
dispatch-attempt cap never trips.

So the factory builds **one runtime per assignment for the life of the
pass**, and asking again returns the same one. One sidecar per assignment
per pass is also what the sidecar was designed for: the socket path is
stable per assignment, and the tree is stable per assignment, so a retry
finds the same data store and the same relay.

## 9. The reviewer is not the worker

A reviewer runs through PR 3's REVIEW path, with the pinned
`mpm-reviewer` definition, in its own sandbox, with the plugin that
provides `sandbox_exec`. The reviewer never runs in the worker's
context, and the implementer never gets to record a review's outcome.

The reviewer's boundary gained `Inspect`, because an interface that
could not be inspected could only ever be reported, never recovered. Its
MODEL is its own too: the worker's model is the sidecar's local id, and
the reviewer's sandbox has no provider and no sidecar.

The reviewer's sandbox TREE exists. The runtime's read-only commands run
with their working directory at `<root>/home`, and the review path reads
the reviewer's session through exactly that command — so without the
tree, every inspection fails with `chdir …: no such file or directory`
and a review can only ever be reported, never recovered.

Four facts stay apart, and the report never collapses them:

| Fact | Whose it is |
|---|---|
| the runtime's outcome | the runtime's |
| the review's evidence (revision, findings digest) | the HOST's capture |
| the review's verdict | the reviewer's own fenced transition |
| the approval | the OPERATOR's |

## 10. The report

The report is a VALUE — no live runtime, no open session, no handle
that could be mistaken for a live system — with a CLOSED set of reason
codes.

```
dispatch_disabled          no_eligible_assignment   budget_exhausted
lease_held                 lease_insufficient       stale_epoch
session_still_running      runtime_unavailable      ambiguous_execution
reviewer_unavailable       recovery_required        operator_gate_pending
dispatched                 completed                refused
deferred                   failed
```

The counts are `dispatched`, `completed`, `refused`, `deferred` and
`ambiguous`, and they mean:

| Count | Means |
|---|---|
| `dispatched` | the work was handed to the adapter by this pass |
| `completed` | the assignment reached its terminal state |
| `failed` | the attempt failed |
| `refused` | a rule said no, and it will not be tried again without the operator changing something |
| `deferred` | nothing was launched, and the work will be considered again |
| `ambiguous` | the runtime's outcome could not be determined |

**`deferred` AND `refused` ARE DIFFERENT FACTS.** A live lease, a moved
epoch and a session that may still be working are WAITS: nothing was
launched, and the work will be considered again. A spent budget, an
unavailable runtime and a lease too short to record the result are NOT:
they will not be tried again without the operator changing something. The
status command agrees — it lists the same states under **blocked**.

**THE MAPPING IS ONE FUNCTION**, used by the dispatch path and the
recovery path alike, so the same reason cannot count one way in one pass
and another way in the next — the same live lease would otherwise be a
refusal in one pass and a deferral in the next.

A claim that was REFUSED is not a dispatch: nothing was launched. A
**failed** attempt is a dispatch that did not complete — the work reached
the adapter and failed there — so it is counted as `dispatched` and
`failed`, and not as `refused`: nothing refused it. A **recovered**
completion is counted as `completed` and not as `dispatched`, because the
run that recovered it did not launch it: it only recorded what the runtime
had already said. So `completed` can exceed `dispatched` in a run that
only recovered. A recovered **failure** is counted as `failed` too — an
outcome that no counter reflects is a fact the operator cannot see.

And every attempt counts against the RUN's concurrency whatever its
outcome was: a dispatch that FAILED still spent a claim and a session, so
it is not free to try another one.

`lease_held` and `lease_insufficient` are different facts and are not
folded together: the first says a **live lease blocks the claim**,
whoever holds it — and the detail names the holder and the expiry — and
the second says the lease the dispatcher holds is too short to record the
result.

`mpm assignment dispatch-status` shows what is **pending**, what is
**blocked** and what is **recoverable**, plus the budget picture of
every tree involved. It is a READ: it builds no runtime and writes
nothing, and it works while the dispatcher is disabled — which is
exactly when an operator wants to see what enabling it would do.

Nothing in the report carries a credential, a prompt or a transcript.

## 11. The selection

The candidate list is ordered by `(created_at, id)`, so two dispatchers
that see the same rows consider them in the same order, and a report is
reproducible.

The states are FIXED by the service: `accepted` and `in_progress`. They
are the two states work can be dispatched from, and a caller that could
name its own states could name one that is waiting for the operator.

The dispatcher's own policy sits on top: an assignment that has used its
dispatch attempts, or that failed too recently, is DEFERRED. That is
what stops a repeatedly failing assignment from monopolising the
dispatcher.

## 12. What PR 4 does not do

- It does not publish evidence to GitHub (PR 5).
- It does not merge, deploy or approve anything.
- It does not create a root, a project or an approval.
- It does not run a pilot on #253.
- It does not add a credential manager, a model router, a dashboard or a
  new daemon.
- It does not replace OpenCode.
- It does not run a HAL loop.

## 13. Known limitations, stated plainly

- **The dispatcher's view of "the runtime cannot be asked" is a
  refusal, not a decision.** The attempt stays ambiguous until a human
  resolves it. That is deliberate — an invented outcome is worse than an
  unfinished one — but it means an unreachable runtime can strand work.
- **The recovery pass inspects one session per outstanding attempt per
  run.** A large backlog takes several runs to drain, by design.
- **The enabled path has never run against the real OpenCode
  boundary.** Every test uses a fake runtime, and the real factory is
  driven only as far as building a runtime — the credential that was
  exposed still has to be rotated, so no real inference was attempted.
  The wiring is tested; the inference is not.
- **The reviewer's reconciliation is refused, not performed.** A review
  session that ended without a record is reported as needing an
  operator: the review path refuses to run a second reviewer while an
  attempt is outstanding, and the dispatcher does not work around that.
- **`resource_units` is declared, not metered.** It is a ceiling on the
  work the operator authorised, not on measured spend.
- **PR 3's O2 stands**: the authorship guard's call site is untested,
  because the `opencode` package has no mock harness.
- **The dispatcher's workspace is a directory, not a worktree.** It does
  not create a git worktree; it creates the assignment's own directory
  under the configured root and hands that path to PR 3's adapter.
