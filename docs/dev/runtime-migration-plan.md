# MPM runtime installation and migration plan

> **Status:** design accepted; the safety prerequisites are implemented and
> tested; **nothing has been deployed.** The live installation at `~/.mpm`
> was read, never written.
>
> **Live revision audited:** `0.1.0-dev+g3df72315.t1792851233`
> (commit `3df723156d7649517508212453de72d347759f43`, built 2026-10-24).
> **Accepted release candidate:** `4111c46dbe30de44e9ed8f7a715fff9e8c3e469e`.
>
> **Prepared:** 2026-10-28, Session A. **Evidence:**
> [`runtime-migration-evidence.md`](runtime-migration-evidence.md).
> **Rehearsal harness (now in-repo):** `scripts/rehearsal/`.

---

## 0. The decision record

These are the settled decisions. Everything below is derived from them.

| # | Decision | Consequence |
|---|---|---|
| **D1** | Versioned, **Git-free** runtime bundles under the existing `~/.mpm` data root. | The data root does not move, so the five host registrations that name `~/.mpm` keep working unchanged. |
| **D2** | The existing `.git` is **preserved** through the initial cutover and the verified rollback window. Removing it is a separate, explicitly authorised operation. | The migration can be undone without a network fetch; the final installation does not depend on Git metadata. |
| **D3** | **Keep every versioned release.** A retention policy is designed but **not applied** during the first migration. | Every rollback target survives the first cutover. |
| **D4** | `MPM_WORKSPACE` is the canonical workspace selector. `MPM_DB_PATH` handling is corrected. Configuration conflicts **fail explicitly**; they never silently select a divergent database. | Landed as `config.ResolveTarget` — one resolver, four binaries. |
| **D5** | Every installed executable, **including telemetry**, must report a trustworthy embedded build identity without needing `.git`. | Landed as the `MPM_BUILD_COMMIT` / `MPM_BUILD_DIRTY` producer assertions plus the release-gate proof. |
| **D6** | Retired adapter cleanup and any separate Hermes removal are **scoped independently** of the runtime migration. | The migration has a single, reviewable blast radius. |
| **D7** | Lifecycle ingestion stays **explicitly opt-in** for the initial RC and its disabled state is documented. The telemetry observation path stays **read-only** until resolvable evidence is available. | The RC changes no write path the operator has not already approved. |

Three additional requirements shape the procedure:

- **The first conversion is not a version switch.** A checkout-to-bundle
  conversion and a later atomic version switch are different operations
  with different failure modes, and the intermediate state after the first
  one must be recoverable (§4.1, §4.3).
- **Runtime rollback is not database rollback.** Rolling the runtime back is
  always safe; rolling the *data* back requires restoring a verified
  backup, and the exact conditions are stated (§4.5).
- **Preflight must not migrate the live database.** A newer binary opening
  the live database **applies pending migrations** — this was discovered
  empirically during the rehearsal, not assumed (§4.4).

---

## 1. The live layout today, verified

`~/.mpm` is a **231 MB Git checkout** serving four roles at once:

| Role | Path | Size |
|---|---|---|
| Git metadata | `~/.mpm/.git` | 16.3 MB |
| Installed binaries | `~/.mpm/bin` | 75.7 MB |
| Build output (duplicate) | `~/.mpm/.build` | 75.7 MB |
| Source + persistent state | `~/.mpm/src` | 17.5 MB |

The persistent state lives at `~/.mpm/src/db/mpm.db` (13.7 MB + a 4.1 MB
WAL), in WAL mode, `integrity_check` **ok**, foreign keys clean, with 18
`schema_migrations` sentinels.

**The live database is AHEAD of the live binary.** `lifecycle_events`
exists in the database and in `main`, but not in the installed revision
`g3df72315`'s schema — residue of the 2026-10-27 `lifecycle_events`
incident. This is harmless today (an extra table), but it proves the
point in §4.4: the live schema and the installed binary's schema are not
the same thing, and only one of them can be assumed.

### 1.1 What names the install path

Five registrations name `~/.mpm` explicitly:

| Registration | Names |
|---|---|
| `~/.config/opencode/opencode.jsonc` | `file:///home/v/.mpm/agent_installation/mpm-opencode` |
| `~/.config/systemd/user/mpm-scheduler.service` | `/home/v/.mpm/bin/mpm-scheduler`, `WorkingDirectory=/home/v/.mpm` |
| `~/.local/bin/mpm` | symlink into `~/.mpm/bin` |
| `~/.local/bin/mpm-mcp` | symlink into `~/.mpm/bin` |
| The scheduler's own environment | `MPM_WORKSPACE=/home/v/.mpm`, `MPM_DB_PATH=/home/v/.mpm/src/db/mpm.db` |

**This is why the bundle stays under `~/.mpm`.** An XDG move
(`~/.local/share/mpm`) would require editing the live, user-owned OpenCode
JSONC and the systemd unit — a change to operator-owned configuration that
the migration has no mandate to make, and that would break every
registration on the day it lands. Keeping the data root where it is keeps
all five registrations valid.

### 1.2 Build identity is already Git-free at runtime

`internal/buildinfo` never reads Git. The provenance is stamped at **link**
time by `scripts/build-provenance.sh` and the Makefile's `-ldflags`
payload. An installed binary reports its provenance with no `.git`
directory anywhere near it — which is exactly the condition D2 creates.

What was **missing** was the other half: a bundle built from a **Git-free
source export** had no history to ask, so it reported commit `unknown`.
That is now closed by D5's producer assertions.

---

## 2. The target layout

```
~/.mpm/                          ← the data root: unchanged, forever
├── bin/                         → symlink into the active bundle
├── agent_installation/          → symlink into the active bundle
├── current                      → symlink to releases/<rev>   (THE POINTER)
├── releases/
│   ├── 4111c46/                 ← a versioned, Git-free bundle
│   │   ├── bin/                 (5 binaries)
│   │   ├── agent_installation/  (the OpenCode plugin payload)
│   │   └── RELEASE.json         (revision, commit, build epoch, dirty state)
│   └── <future-rev>/
├── src/db/mpm.db                ← the persistent state: never copied, never moved
├── backups/
├── mpm_config.json              ← operator-owned: never in a bundle
├── mode/, persona/              ← operator-owned: never in a bundle
└── .git                         ← preserved through the rollback window (D2)
```

### 2.1 The promotion is one `rename(2)`

`current` is a symlink. Promotion is `rename(2)` of a newly-created
symlink over `current` — atomic, so there is no instant at which
`current` does not resolve. Rollback is the same operation with the
previous release.

### 2.2 What is in a bundle and what is not

| In the bundle (release-scoped) | In the data root (operator-scoped) |
|---|---|
| The five binaries | `src/db/mpm.db` and its WAL sidecars |
| The OpenCode plugin payload | `mpm_config.json` |
| `RELEASE.json` | `mode/`, `persona/` |
| | `backups/`, `run/`, `active.json`, the locks, the logs |

The split is by **ownership lifetime**: a bundle is replaced wholesale on
a version switch; the data root outlives any number of bundles. Putting
operator files in a bundle would mean a version switch could overwrite
them — which is the defect this layout exists to prevent.

### 2.3 The `.git` question, answered precisely

D2 says `.git` is preserved through the cutover and the rollback window.
The reason is not sentiment: **the rollback target is a released bundle,
not a commit**, so `.git` is not what makes rollback possible. `.git` is
preserved because removing it is irreversible on a machine with no
network, and because the operator may want to diff the checkout against
the first bundle. Removing it is a separate, explicitly authorised
operation with its own backup procedure — and it is the operation that
makes the installation Git-free in the strict sense.

---

## 3. Data preservation

1. **The database is never copied or rewritten by the upgrade.** The
   bundle contains no data. The upgrade moves *binaries*, not *state*.
2. **The backup is an online backup**, taken with the SQLite backup API —
   never a file copy of a live database. A file copy of a WAL-mode
   database is not a consistent snapshot.
3. **The backup is verified before the promotion**: `integrity_check`,
   row-count comparison against the live database, and a restore test that
   reads the data back.
4. **The operator's credentials never enter a fixture.** `mpm_config.json`
   is copied into the recovery point with its permissions, and never into
   a bundle.

The rehearsal's fingerprint check proves the property that matters: the
state fingerprint **before** and **after** the upgrade was identical
(`20bc92a66d87ef8491ea03653502038b51bb93373ec9d82d40042be7c8944153`).

---

## 4. The migration procedure

### 4.1 Two different operations

**Operation A — the initial checkout-to-bundle conversion.** The live
`~/.mpm` is a checkout; the target is a checkout + bundles. This is the
only operation that touches the shape of the installation.

**Operation B — a later atomic version switch.** A new bundle is built,
validated, and `current` is renamed onto it. The checkout is already
gone; nothing about the data root changes.

They are separated because their failure modes are different. Operation A
can leave the installation in an **intermediate state** — some paths
converted, some not — and that state must be recoverable. Operation B
cannot: the pointer swap is atomic, so the installation is always either
fully on the old bundle or fully on the new one.

### 4.2 Operation A — the conversion, stage by stage

| Stage | What happens | Recoverable to |
|---|---|---|
| 1 | **Preflight** — read-only inventory. Record the live revision, the database inode, the row counts, the service state, the host registrations. | n/a — nothing has changed |
| 2 | **Backup** — verified online backup; recovery point recorded at `backups/upgrade-<UTC>/`; operator files copied; SQL dump written **from the backup copy** | the pre-migration state |
| 3 | **Build the bundle** — from a Git-free export of the target revision, with the producer's provenance assertions | n/a — the bundle is not yet exposed |
| 4 | **Validate the bundle** — all five binaries present and executable; no `.git`; the staged binary reports the staged revision; `health_check` passes against a **scratch copy** of the state; the plugin payload is present | n/a |
| 5 | **Quiesce** — stop the writers so the state cannot move under the promotion | the pre-migration state (restart the service) |
| 6 | **Promote** — install the bundle at `releases/<rev>`; swap `current`; repoint `bin` and `agent_installation` at `current` | **stage 7** |
| 7 | **Restart and smoke** — start the service, wait for readiness, read the state back through the new runtime, compare the fingerprint | stage 6 (the swap is reversed) |

Stages 1–4 are **read-only with respect to the live installation**. The
first moment the live installation changes is stage 5.

### 4.3 Intermediate-state recovery for the first conversion

The conversion's intermediate states, and how each is undone:

| Intermediate state | How it is recognised | How it is undone |
|---|---|---|
| Bundle built, not exposed | `current` does not exist; `bin` still points at the old binaries | Delete `releases/<rev>`. Nothing else changed. |
| `current` swapped, `bin` not yet repointed | `current` resolves; `bin` still holds the old binaries | Re-point `bin` at `current/bin`, or restore the previous `bin` from the recovery point. The data is untouched. |
| `bin` repointed, `agent_installation` not yet | `bin` resolves into the bundle; `agent_installation` is still the checkout's | Re-point `agent_installation` at `current/agent_installation`. |
| Service stopped, promotion incomplete | The service is inactive | Start it again. The old binaries are still in place. |

**The rule that makes recovery simple: the data root is never touched by
the conversion.** Every intermediate state is a question about *paths*,
and every path can be put back. The rehearsal's fault-injection driver
proves this by failing each stage in turn and asserting the automatic
rollback restores the pre-migration state.

### 4.4 Preflight must not migrate the live database

**A newer binary opening the live database applies pending migrations.**
This was not assumed — the rehearsal's own first implementation took its
SQL dump with the staged (newer) binary against the live database and
**mutated it**. The rule that came out of that incident:

> The live database is opened **only** through the SQLite **online backup
> API**, in read-only mode. The SQL dump is taken from the **verified
> backup copy**, never from the live database.

Consequently:

- Preflight uses `file:…?mode=ro` and the backup API. It never opens the
  live database with a newer binary.
- `health_check` runs against a **scratch copy** of the state, never
  against the live database.
- The `lifecycle_events` investigation (§6.3) is the proof: the live
  schema is byte-compatible with the RC's DDL, and the RC binary accepts
  it unchanged — but that was established on a **copy**.

### 4.5 Runtime rollback is not database rollback

**Schema and data migrations are forward-only.** That is a property of the
migration mechanism, not a policy choice: SQLite's `ALTER TABLE` has no
general inverse, and the substrate's sentinels record what was applied.

| Situation | Runtime rollback | Data rollback |
|---|---|---|
| The new bundle fails to start, or misbehaves | **Safe.** Re-point `current` at the previous release and restart. | **Not needed.** The data is unchanged by a version switch. |
| The new bundle applies a migration and then fails | Safe for the runtime. | **Required.** The schema is now ahead of the old binary. Restore the verified backup from the recovery point. |
| The new bundle applies a migration and the operator wants the old behaviour back | Safe for the runtime. | **Required**, for the same reason. |
| The migration is additive and the old binary tolerates the extra columns | Safe for the runtime. | **Optional.** The old binary reads the new schema correctly; the data does not need restoring. |

**The rule:** restoring the verified database backup is required exactly
when the migration was **not** backward-compatible with the binary being
rolled back to. The operator decides, per upgrade, whether the migrations
in that release are backward-compatible — and the release notes must say.

### 4.6 The rollback procedure

```bash
# 1. re-point the active pointer at the previous revision
ln -sfn "releases/<previous-rev>" ~/.mpm/current.new && mv -T ~/.mpm/current.new ~/.mpm/current

# 2. confirm
~/.mpm/bin/mpm --version          # must report <previous-rev>
systemctl --user restart mpm-scheduler
systemctl --user is-active mpm-scheduler

# 3. ONLY if the data is suspect (a failed or damaging migration):
#    restore the verified backup from the recovery point
mpm ops restore-db --from ~/.mpm/backups/upgrade-<UTC>/mpm.db.backup
```

Step 3 is **not** part of a routine rollback. It is a separate,
deliberate data operation, and it is only correct when the current
binary cannot read the current schema.

---

## 5. Installer changes

The changes below are scoped into the PR sequence in §8. Each is small and
independently reviewable.

### 5.1 `scripts/build-provenance.sh` — the producer assertions ✅ landed

`MPM_BUILD_COMMIT` and `MPM_BUILD_DIRTY` let a Git-free source export
carry the revision it was exported from. The assertions are **validated**
before anything is stamped: a malformed SHA is refused, and an assertion
that contradicts the checkout is refused. See
[`runtime-migration-evidence.md`](runtime-migration-evidence.md) §4.

### 5.2 `scripts/install_runtime_assets.py` — split by ownership lifetime ✅ landed

The asset installer now takes a `--groups` switch:

```
--groups definitions   mode/, persona/, drills/ — operator-owned. The
                       DATA ROOT, where they survive every upgrade.
--groups host-assets   agent_installation/** — release-scoped. The
                       BUNDLE, so they roll back with the release.
```

Without the split, a version switch would replace the operator's
definitions with the upstream copies — and the reconciler's refusal to
overwrite an operator's edit would turn every upgrade into a conflict
report for files the operator had deliberately customised.

One subtlety the split needed: the stale-asset sweep is now **scoped** to
the groups a run owns. A group-scoped reconcile must not release
ownership of assets outside its groups, or provisioning one group into a
root would silently un-manage the other group's files in it.

### 5.3 `install.sh` — a bundle phase and a pointer swap ✅ landed

The installer gains `--bundle`, which installs into the versioned
runtime-bundle layout. The default install path is **unchanged**: the live
installation's cutover is a separately authorised operation and must not be
a side effect of `git pull && ./install.sh`.

`phase_bundle` replaces `phase_binaries` in the bundle layout, and the
order is the whole point:

| # | Step | What it does | If it fails |
|---|---|---|---|
| 1 | **Assemble** | the candidate under `<data root>/staging/<rev>.<pid>/` | nothing was touched |
| 2 | **Promote** | the five binaries into the candidate's `bin/` as one transaction | nothing was touched |
| 3 | **Validate** | the candidate | nothing was touched |
| 4 | **Install** | the candidate at `releases/<rev>` with a `rename(2)` | the previous runtime is still active |
| 5 | **Activate** | swap `current` with a `rename(2)` | the previous pointer is restored |
| 6 | **Convert** | `bin/` and `agent_installation/` become symlinks into the active bundle | the previous pointer is restored and the conversion is undone |

Steps 1–3 are reversible with **no effect at all** on the running
installation. Steps 4–6 change what is running, so a failure after the
pointer was swapped **rolls back**: the previous pointer is restored and
the layout conversion is undone.

Two properties the phase holds:

- **The revision label comes from the binary being installed**, not from
  Git. That is what makes it trustworthy in a Git-free source tree, and
  it cannot disagree with the binary's own reported identity.
- **A release is immutable.** An existing release is validated and
  activated as it stands rather than replaced — which is also the
  recovery path after a promotion that failed between installing the
  release and activating it.

A new phase, `phase_schema_preflight`, runs on **every** install and
refuses a schema collision (§5.6) before the build and before any
promotion.

### 5.4 `scripts/lib/binary_transaction.sh` — retarget the promotion ✅ landed

The promotion library was already parameterised by destination directory,
so it needed no change: the installer now targets the **candidate
bundle's** `bin/` instead of the live `bin/`. A failed promotion cannot
leave the live installation half-updated, and the candidate — which is
disposable — is discarded whole.

### 5.5 `agent_installation/scripts/stage_runtime_package.sh` — name the roots ✅ landed

`mpm_runtime_root_from_bin` conflated the **bundle root** (where the
release lives) with the **data root** (where the persistent state
lives). They are now two explicit resolvers — `mpm_bundle_root_from_bin`
and `mpm_data_root_from_bin` — and the historical name delegates to the
bundle one, so every existing install is unaffected.

The discriminator is the parent's **name** (`releases`), not the presence
of `src/db`: a resolver that depends on the database existing is a
resolver that fails exactly when it is first needed, on a fresh install.

### 5.6 Fail closed on a missing plugin payload ✅ landed

If the staged host runtime payload is absent, the promotion must
**refuse**, not warn. The host's registration points into the bundle; a
bundle without the payload breaks every agent session on the host —
silently, because the plugin registry stays empty.

So a payload that cannot be staged, or a payload the release **declares
but does not ship**, refuses the promotion. A release that declares no
payload at all is fine: there is nothing to stage.

The staging is **data-driven and host-agnostic**. The release's payload
manifest (`scripts/runtime_payloads.json`) names the adapters, their
plugin ids, their validators and their explicit file allowlists; the
staging code iterates over whatever the manifest declares and names no
adapter itself. The root installer therefore stays host-agnostic —
adding an adapter is a data change, not a code change — and the
repository's own boundary test keeps it that way.

### 5.6a Refuse a schema collision before a new binary reaches the database ✅ landed

`CREATE TABLE IF NOT EXISTS` does not repair an unrelated table of the
same name (§6.3, case S5). The installer's preflight compares the live
table's shape against the shape the substrate **itself** would create —
by extracting the substrate's own DDL from `internal/core/schema.go`,
applying it to an empty scratch database, and comparing the two — and
**refuses** when they differ, naming every difference.

The live database is read through SQLite's online backup API into a
scratch file. A newer MPM binary applies pending migrations the moment
it opens a database, so the check never runs one against the live
database (§4.4).

### 5.7 `uninstall.sh` — remove the bundle, keep the data ✅ landed

The runtime-only uninstall removes `releases/`, `current/`, `staging/`,
and the `bin/` and `agent_installation/` **symlinks**, and keeps
`src/db/`, `backups/`, `mpm_config.json`, `mode/`, `persona/` and
`drills/`. `--purge` removes the data as well.

The symlinks are removed as links — `rm` never follows a symlink into its
target — so a release is removed by the releases step, not by deleting
five files out of it. The operator's own directories that the layout
conversion moved aside are preserved, and the record of where they went
(`<data root>/.legacy-layout-paths`) is preserved with them.

### 5.8 Version reporting — no change to the format ✅ landed

The format is unchanged. The fix was to ensure the **telemetry sidecar**
carries the same identity as the other four — it already did; an earlier
finding that it did not was an artefact of probing with `head -1` and is
**withdrawn** (evidence §4.2).

What was genuinely missing was the other half: a bundle built from a
**Git-free source export** had no history to report, so it reported
commit `unknown`. That is closed by D5's producer assertions (§5.9).

### 5.9 `scripts/build-provenance.sh` — the producer assertions ✅ landed

`MPM_BUILD_COMMIT` and `MPM_BUILD_DIRTY` let a Git-free source export
carry the revision it was exported from. The assertions are **validated**
before anything is stamped: a malformed SHA is refused, and an assertion
that contradicts the checkout is refused. See
[`runtime-migration-evidence.md`](runtime-migration-evidence.md) §4.4.

---

## 6. The rehearsal

The rehearsal ran against the accepted release candidate in an isolated
workspace with its own `HOME`, `MPM_WORKSPACE` and `MPM_DB_PATH`. It
never touched the live installation.

| Suite | Assertions | Failures |
|---|---|---|
| `00_simulate.sh` — the eleven-stage migration, with fault injection and automatic rollback | 56 obligations | 0 |
| `20_negative.sh` — the harness's own integrity guards | 54 | 0 |
| `30_schemas.sh` — the upgrade path against representative existing schemas | 25 | 0 |
| **Total** | **135** | **0** |

The harness is now **in the repository** at `scripts/rehearsal/`, with a
Makefile target, so the evidence is reproducible rather than a local
one-off (requirement 6). The work products — the source export, the
bundle, the simulated home, the traces — land **outside** the
repository, in `$XDG_CACHE_HOME/mpm-rehearsal`; the harness refuses an
in-repository work directory, because the work products include a copy
of the source tree and this repository's repo-wide scanners would find
that copy and fail on it.

### 6.1 What the rehearsal proved

- The migration completes with the **state fingerprint unchanged**.
- Every injected fault is recovered by the automatic rollback.
- The bundle carries **no `.git`**, and the staged binary reports the
  staged revision.
- The live installation recorded **zero syscalls** against the live tree
  and **zero snapshot differences**.

### 6.2 What the rehearsal changed

The rehearsal was not a rubber stamp. It changed the design twice:

1. **The backup-mutation incident.** The rehearsal's own first
   implementation mutated the live database by dumping it with the staged
   binary. That produced the read-only rule in §4.4.
2. **`CREATE TABLE IF NOT EXISTS` does not repair an unrelated table.**
   A database carrying an unrelated table named `lifecycle_events` keeps
   it, and the substrate's own schema is **not** applied. That is a latent
   silent incompatibility, not a repair (§6.3, case S5).

### 6.3 The `lifecycle_events` discrepancy

The live database's `lifecycle_events` has **26 columns and 4 CHECK
constraints**. The RC's DDL has the same 26 columns in the same order,
with the same NOT NULL flags and the same four CHECK constraints. The
live table is **byte-compatible** with the RC's DDL; the only textual
difference is the quoting of the `meta` default (`'{}'` vs `{}`).

The upgrade path was then tested against five representative schemas:

| Case | Shape | Result |
|---|---|---|
| **S1** | the live shape | The RC binary accepts it **unchanged**. No table lost, no row count moved, `integrity_check` ok. |
| **S2** | the pre-incident shape (no `lifecycle_events`) | The schema advances correctly. |
| **S3** | a fresh database | The RC's own bootstrap. |
| **S4** | an empty file (0 bytes) | The full bootstrap. |
| **S5** | an unrelated table named `lifecycle_events` | The unrelated table's **content is preserved** and the RC's own schema is **not** applied — the latent hazard. |

S5 is the one that needs an operator's attention: a database that
already has a table with that name will silently not receive the
substrate's schema for it. The migration should detect this case and
refuse, rather than proceed.

---

## 7. Risks and decisions requiring operator approval

### 7.1 Risks

| # | Risk | Severity | Mitigation |
|---|---|---|---|
| R1 | A newer binary opens the live database and **applies a migration** during a preflight. | **High** | §4.4: the live database is opened only through the read-only online backup API. Verified by the rehearsal's own incident. |
| R2 | The conversion leaves the installation in an **intermediate state** that the operator cannot recognise. | Medium | §4.3 lists every intermediate state and its undo. The driver fails each stage in turn and asserts the rollback. |
| R3 | A version switch **overwrites operator-owned files** (mode/, persona/, `mpm_config.json`). | Medium | §2.2's ownership split, implemented by §5.2's `--groups` switch. |
| R4 | The promotion leaves `current` dangling or pointing at a removed release. | Medium | The swap is a single `rename(2)`; `uninstall.sh` refuses to remove a release that `current` points at. |
| R5 | The `lifecycle_events` name collision (S5) is encountered in the wild and the substrate's schema is silently not applied. | **Medium — now closed** | §5.6a: the installer's preflight compares the live table's shape against the substrate's own DDL and refuses a collision, before the build and before any promotion. |
| R6 | The `.git` removal (D2's follow-up) is irreversible without a network fetch. | Medium | It is a separate, explicitly authorised operation with its own backup procedure. |
| R7 | A divergent `MPM_DB_PATH` silently opens a different database. | **High — now closed** | D4: one resolver, explicit conflict errors. Executable-level tests. |
| R8 | A malformed `mpm_config.json` crashes the CLI with a stack trace. | **High — now closed** | F3: a controlled validation error naming the file. |
| R9 | A Git-free bundle reports `unknown` provenance, so a rollback target cannot be named. | Medium — now closed | D5: the producer assertions, validated, plus the release-gate proof. |
| R10 | The scheduler's singleton lock and its database point at different workspaces. | Low | The lock follows the resolved workspace when `--db` selects one. Residual: an explicitly-passed `--lock` is honoured verbatim. |
| R11 | The lifecycle ingestion path stays off in the initial RC and an operator expects it to be on. | Low | D7: the disabled state is documented in the release notes. |

### 7.2 Decisions the operator must still make

1. **The retention policy.** D3 keeps every release. The policy that will
   eventually prune them is designed but not applied: keep the current
   release, the previous release, and any release younger than 30 days;
   keep the recovery points named in `backups/upgrade-*/`; never prune
   the release the operator last approved. **The operator chooses the
   retention window.**
2. **The `lifecycle_events` collision policy.** Refuse the migration
   (recommended), or rename and migrate the colliding table.
3. **When `.git` is removed.** After the rollback window closes, or on a
   separate schedule.
4. **The telemetry store path.** `mpm-telemetry observe` (#139) needs the
   store path settled between `<workspace>/telemetry.db` and
   `<workspace>/src/db/telemetry.db`.
5. **`MPM_EVENT_INGEST`'s default.** Session C's call, on #142.

### 7.3 Explicitly **not** done, and why

- **The live migration was not performed.** The brief forbids it, and it
  is a separate approval gate.
- **The `.git` was not removed.** D2.
- **No release was pruned.** D3.
- **Nothing was deployed, tagged or published.**
- **The live installation was not touched**: no file written, no service
  stopped or reloaded, no configuration edited, no database opened for
  writing. Verified after the rehearsal: the scheduler is still pid
  2804, `NRestarts=0`, the installed binary still reports `g3df72315`,
  and `.git` is intact.

---

## 8. The implementation PR sequence

Four PRs, in order. Each is independently reviewable and independently
mergeable; the live migration is not among them.

### PR 1 — packaging: the bundle primitives

| Change | File | State |
|---|---|---|
| The producer assertions for a Git-free build | `scripts/build-provenance.sh` | ✅ landed |
| The assertion contract tests | `scripts/tests/test_build_provenance.py` | ✅ landed |
| The release-gate proof that all five binaries carry the identity | `release_acceptance/gitfree_build_identity_test.go` | ✅ landed |
| The asset split by ownership lifetime | `scripts/install_runtime_assets.py` | ✅ landed |
| The asset-split contract tests | `scripts/tests/test_runtime_asset_groups.py` | ✅ landed |
| The bundle root and the data root as separate resolvers | `agent_installation/scripts/stage_runtime_package.sh` | ✅ landed |
| The transactional promotion retargeted at the bundle | `scripts/lib/binary_transaction.sh` (unchanged; the installer targets the candidate's `bin/`) | ✅ landed |

### PR 2 — the database-path safeguards ✅ landed

| Change | File | State |
|---|---|---|
| One resolver for the database path | `internal/core/config/dbpath.go` | ✅ landed |
| The resolver's contract tests | `internal/core/config/dbpath_test.go` | ✅ landed |
| The startup gates in all four binaries | `cmd/mpm/main.go`, `cmd/mpm-mcp/main.go`, `cmd/mpm-scheduler/main.go`, `cmd/mpm-critic/main.go` | ✅ landed |
| The executable-level proofs | `cmd/mpm/db_path_resolution_test.go` and its siblings | ✅ landed |
| A malformed configuration is a validation error, not a panic | `internal/core/config/config.go`, `internal/core/memory.go` | ✅ landed |

### PR 3 — cutover preparation

| Change | File | State |
|---|---|---|
| The rehearsal harness, in-repo and reproducible | `scripts/rehearsal/` + a Makefile target | ✅ landed |
| The bundle library: staging, validation, the active pointer, the layout conversion | `scripts/lib/runtime_bundle.sh` | ✅ landed |
| The bundle library's contract tests | `scripts/tests/test_runtime_bundle.py` | ✅ landed |
| The `lifecycle_events` collision detection | `scripts/lib/schema_preflight.py` + `install.sh`'s preflight | ✅ landed |
| The collision detection's contract tests | `scripts/tests/test_schema_preflight.py` | ✅ landed |
| The bundle phase and the pointer swap | `install.sh` (`--bundle`) | ✅ landed |
| The runtime-only uninstall keeps the data | `uninstall.sh` | ✅ landed |
| The uninstall's contract tests | `scripts/tests/test_uninstall_bundle.py` | ✅ landed |
| The seven proofs of the migration gate | `scripts/tests/test_migration_gate.py` | ✅ landed |

### PR 4 — the live migration ⬜ NOT AUTHORISED

Run the rehearsed procedure against the real revision, at a time the
operator chooses, with the operator present. This PR is the *record* of
the migration; the migration itself is an operation, not a pull request.

---

## 9. The operator's checklist

### 9.1 Before the migration

- [ ] The safety prerequisites are **landed**: F3 (a malformed
      configuration is a validation error), D4 (one resolver for the
      database path), D5 (a Git-free build carries a real commit).
- [ ] The rehearsal has been re-run against the **exact** target
      revision, in-repo: `make rehearsal REHEARSAL_REV=<rev>` — **135 assertions, 0 failures**.
- [ ] The rollback has been rehearsed against the **real** revision, with
      the fault injection enabled.
- [ ] The live state has been inventoried and **frozen**: revision,
      database inode, row counts, service state, and the five host
      registrations recorded.
- [ ] The host registrations are understood and **will not change**: the
      OpenCode JSONC, the systemd unit, the two `~/.local/bin` symlinks,
      and the scheduler's environment.
- [ ] The build and the bundle are correct: all five binaries present,
      no `.git` in the bundle, the staged binary reports the staged
      revision, and the plugin payload is present.
- [ ] The recovery point is known and verified: `backups/upgrade-<UTC>/`
      holds an online backup whose `integrity_check` is ok, whose row
      counts match, and which has been restore-tested.
- [ ] The rollback window is **agreed**: how long the previous release
      stays, and when the `.git` removal is authorised.
- [ ] The `lifecycle_events` collision check has run against the live
      schema.
- [ ] The database selectors **agree**: `MPM_WORKSPACE` and
      `MPM_DB_PATH` name the same workspace, or `MPM_DB_PATH` is unset.
      A divergence is now a hard error at startup — which is correct,
      and it means a divergent value must be resolved *before* the
      migration, not during it.
- [ ] The boundaries were respected: the live installation, the systemd
      service, the OpenCode configuration, the real database and the Git
      metadata are untouched by the rehearsal.

### 9.2 During the migration

- [ ] Stage 1 — preflight: read-only inventory recorded.
- [ ] Stage 2 — backup: verified, with the recovery point named.
- [ ] Stage 3 — bundle built from a Git-free export, with the producer's
      provenance assertions.
- [ ] Stage 4 — bundle validated, including `health_check` against a
      scratch copy.
- [ ] Stage 5 — writers quiesced, service confirmed stopped.
- [ ] Stage 6 — promoted; `current` swapped; the stable paths repointed.

  The installer's own `--bundle` mode performs stages 3–6 in that order
  (`./install.sh --bundle`), with the schema-collision preflight running
  before the build. The conversion moves the previous `bin/` and
  `agent_installation/` **aside** rather than deleting them; their
  locations are listed in `~/.mpm/.legacy-layout-paths`, and they are the
  rollback material for the conversion itself.

- [ ] Stage 7 — restarted, readiness confirmed, the state fingerprint
      compared with the pre-migration one.

### 9.3 After the migration

- [ ] `mpm --version` reports the new revision, and the **same** revision
      is reported by `mpm-mcp`, `mpm-scheduler`, `mpm-critic` and
      `mpm-telemetry`.
- [ ] `systemctl --user is-active mpm-scheduler` — active.
- [ ] The state fingerprint is unchanged.
- [ ] `mpm doctor` — no new failures.
- [ ] The OpenCode plugin resolves into the new bundle and an agent
      session round-trips.
- [ ] `ls ~/.mpm/releases/` — the previous release is still there.

### 9.4 The rollback

The runtime rollback is **always safe**; the data rollback is **not part
of it**.

- [ ] Re-point the active pointer at the previous revision:

  ```bash
  ln -sfn "releases/<previous-rev>" ~/.mpm/current.new
  mv -T ~/.mpm/current.new ~/.mpm/current
  ```

- [ ] `~/.mpm/bin/mpm --version` reports `<previous-rev>`.
- [ ] `systemctl --user restart mpm-scheduler` and
      `systemctl --user is-active mpm-scheduler` → `active`.
- [ ] The state fingerprint is compared with the pre-migration one.
- [ ] **Only if the data is suspect** — a failed or damaging migration —
      restore the verified backup from the recovery point:

  ```bash
  mpm ops restore-db --from ~/.mpm/backups/upgrade-<UTC>/mpm.db.backup
  ```

  This step is **not** part of a routine rollback. It is required
  exactly when the migration was **not** backward-compatible with the
  binary being rolled back to (§4.5), and it is a separate, deliberate
  data operation.

### 9.5 The explicit authorisation

- [ ] **The operator has authorised the live migration.** It is a
      separate gate from every PR above, and no PR in this sequence
      performs it.

---

## Appendix A — where the evidence lives

| Evidence | Location |
|---|---|
| This plan | `docs/dev/runtime-migration-plan.md` |
| The evidence appendix | `docs/dev/runtime-migration-evidence.md` |
| The rehearsal harness | `scripts/rehearsal/` (`make rehearsal REHEARSAL_REV=<rev>`) |
| The migration driver (the model for `install.sh`'s bundle phase) | `scripts/rehearsal/mpm-upgrade` |
| The provenance assertions | `scripts/build-provenance.sh` |
| The database-path resolver | `internal/core/config/dbpath.go` |
