# Recovering the host integration assets on the already-installed runtime

> **This is a procedure, not an execution.** Nothing in this document has
> been run against the live installation. Every command is written for
> the operator to run deliberately, with the preconditions and stop
> conditions stated.
>
> **The target revision is pinned.** The procedure below targets
> `2045afcc57f1f7603f38e016a2909b85985e48dc` (`origin/main`, the merge
> of PR #236). Every stage names the revision it expects. A different
> target is only legitimate if a **separate code-change decision** makes
> it necessary — and then the digests in §2, the preflight's digest in
> §2.2 and the check count in §3.3 Step 3 must be recomputed for it.
> Do not "pick a newer revision" to work around a problem: the fix must
> be *in* the revision, and §3.3 Step 2 is what proves it.
>
> **On this host a host-specific procedure governs the execution.**
> `/home/v/src/MPM_CUTOVER_4a870c8a_to_2045afcc.md` is **the** cutover
> procedure for exactly this revision pair (`4a870c8a…` → `2045afcc…`)
> on this host — **the one to follow, once it is approved and
> authorised (§3.8).** Where it and this guide speak to the same act,
> the host-specific one wins — §3.8 says which, and says what this
> guide still adds on top of it.
>
> **That procedure carries no approval yet, and this guide does not
> approve it.** Its current revision declares itself **NOT APPROVED — pending a
> fresh independent review**. **Nothing in either document may be
> executed until both of two separate permissions are in hand:** an
> **independent approval recorded against the procedure's exact current
> SHA-256**, and the **operator's own authorisation of the acts**. Those
> are different permissions; neither is conferred by reading a document,
> and neither follows from the other. §3.2 carries the check as a
> precondition; §3.8 records the live checksum and the **external
> attestation** in which the approval is to live.
>
> **The approval is recorded outside the procedure, and that is not an
> oversight.** Writing the approval into the procedure would edit the
> file, change the checksum, and invalidate the approval it was
> recording. §3.8 sets out the attestation and the reasoning.

## 1. What happened

The first production promotion of `4a870c8a0d10b4567d9362df4c3dda1fbe0a01ae`
(2026-11-07T20:15:37Z) installed a bundle that carried **only the runtime
payload**. It did not carry the release-scoped host assets — the
canonical managed-block source, the behavioural protocol, the shared
scripts and the per-adapter templates and installer modules.

The cause was a **split between two bundle producers**:

| producer | host assets |
|---|---|
| `install.sh` (full install) | provisioned, via `install_runtime_assets.py --groups host-assets` |
| `scripts/assemble-release-bundle.sh` (published release) | provisioned, same call |
| `internal/update`'s `GoBuilder` (`--branch` / `--commit` / `--from`) | **omitted** |

The promotion was made from a commit (`source: git` in
`~/.mpm/update-history.jsonl`), so it went through the third producer.
The bundle it produced passed **every** check the updater made, because
the updater had no check for the host assets at all.

The symptom is a dangling pointer in the installation's own
instructions. `~/.config/opencode/AGENTS.md` says:

> Edit the canonical managed block in
> `~/.mpm/agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md`, not this
> section, for behavioral changes.

> Full contract: `~/.mpm/agent_installation/mpm-agent-protocol.md`

Neither path resolves. `~/.mpm/agent_installation` is a symlink to
`current/agent_installation`, and the active release's subtree holds
only the plugin payload.

A second, quieter symptom: the installed managed block still describes
the **previous** release. Its provenance line reads

```
<!-- mpm-provenance: version=0.1.0-dev commit=77738cfd1663f2aee5d27172c916afe618bf4644 protocol=5.0.0 applied=2026-11-02T16:15:26Z -->
```

and it tells agents that wake context arrives through
`experimental.chat.system.transform` — the **OpenCode V1** hook name.
The promoted runtime uses `ctx.session.hook("context")`; `origin/main`'s
`agent_installation/INSTALL.md` calls the V1 name legacy and states
that OpenCode 2 cannot load it.

## 2. What the installed runtime is missing

Thirteen files: the twelve host assets, plus the declaration that
names them.

**Read the digests carefully — there are two tables, for two
revisions, and they differ.** The *installed* runtime is missing the
files; the *repaired* runtime must carry the **target** revision's
bytes. Two of the twelve assets changed **in the fix itself** (PR #236
added `sys.dont_write_bytecode = True` to two of them), so the two
tables below are not interchangeable.

### 2.1 The twelve host assets

The **installed** column is what the authority produced for the
promoted commit `4a870c8a` — it describes the gap. The **target**
column is what the authority produces for the pinned target
`2045afcc` — it is what a repaired installation must carry, and it is
what §3.3 Step 6 verifies.

| host asset | installed (`4a870c8a`) | **target (`2045afcc`)** |
|---|---|---|
| `agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md` | `9d9f23be6999` | `9d9f23be6999` |
| `agent_installation/mpm-agent-protocol.md` | `00ad1de7e510` | `00ad1de7e510` |
| `agent_installation/mpm-opencode/reconcile.json` | `927c559c7d0a` | `927c559c7d0a` |
| `agent_installation/mpm-opencode/scripts/install_agents_instructions.py` | `b8ae313993f1` | **`05fa8310e32a`** |
| `agent_installation/mpm-opencode/scripts/jsonc_config.py` | `890af38078da` | `890af38078da` |
| `agent_installation/mpm-opencode/templates/AGENTS.md.snippet` | `b3c47c3e744d` | `b3c47c3e744d` |
| `agent_installation/scripts/block_provenance.py` | `364f29677f6f` | `364f29677f6f` |
| `agent_installation/scripts/check_installed_managed_blocks.py` | `5eeeab65063a` | **`c60d49c6fb9f`** |
| `agent_installation/scripts/managed_block_convergence.py` | `883a6c4040e2` | `883a6c4040e2` |
| `agent_installation/scripts/reconcile_managed_blocks.py` | `929f52f58e75` | `929f52f58e75` |
| `agent_installation/scripts/render_managed_blocks.py` | `dc86a5f5b2a8` | `dc86a5f5b2a8` |
| `agent_installation/scripts/stage_runtime_package.sh` | `0737c617f532` | `0737c617f532` |

Plus the declaration itself:

| file | installed (`4a870c8a`) | **target (`2045afcc`)** |
|---|---|---|
| `config/runtime-assets.json` | `db2daf0160ca` | **`24231202b29d`** |

**The declaration's digest moves because the declaration records the
digest of every asset it names**, and two of those assets changed. An
operator who checks a repaired installation against the *installed*
column will see three non-errors. **Check against the target column.**

These digests are the authority's own output — `scripts/install_runtime_assets.py`
— so they are checkable rather than asserted:

```bash
# the authority, run against a checkout of the pinned target
python3 scripts/install_runtime_assets.py \
  --source-root "$BUILD_ROOT/src" --runtime-root "$SCRATCH_BUNDLE" --groups host-assets
```

What the active release **does** carry, and must keep:

```
agent_installation/runtime_payloads.json
agent_installation/mpm-opencode/server.js
agent_installation/mpm-opencode/package.json
agent_installation/mpm-opencode/dist/{index.v2,wake-context,event-envelope,
    event-mapper,event-subscriber,workspace,resolve-mpm-binary}.js
```

The repair **adds** to the release. It never replaces a payload file.

### 2.2 The schema preflight

The bundle also carries `schema/lifecycle_events.sql`, the substrate's
own `CREATE TABLE lifecycle_events` statement, which the
schema-collision preflight compares against the live table. At the
pinned target it is **byte-identical to the canonical extraction**
(`scripts/lib/schema_preflight.py extract`), sha256
`7f122e30bad9b192b68a40a86bceb5ee6cdad704cf1e95528099dfd6630599f2`.

That equality matters for this recovery: it means the preflight
compares the live table against the **genuine** substrate DDL, so the
format's general inability to prove schema identity (see
[`bundle-validation-scopes.md`](bundle-validation-scopes.md)) does not
gate this cutover. It remains a follow-up, not a blocker.

### 2.3 What the installed releases carry that no declaration names

Two files, in the older releases. They are not omissions — they are
files the runtime wrote into the release directory after it was
installed:

```
agent_installation/scripts/__pycache__/block_provenance.cpython-314.pyc
agent_installation/mpm-opencode/dist/mpm-build-provenance.json
```

The first is Python's bytecode cache: the instruction refresher runs
from inside the release and imports `block_provenance` from it. The
second is the adapter's build-provenance sidecar, written by the
adapter's own `install.sh` into the canonical adapter directory.

They are the reason the bundle validators have TWO scopes. A bundle
being **assembled** must account for every file it carries; a bundle
that was **already installed** is checked for integrity, because a
release directory accumulates files after installation and refusing it
for those would push the operator towards deleting an immutable
rollback target. The exact rule is in
[`bundle-validation-scopes.md`](bundle-validation-scopes.md).

Since the fix, the runtime no longer writes the bytecode cache into a
release at all: the two tools that import a sibling module set
`sys.dont_write_bytecode = True` before the import. The tolerance
remains, because the caches are already on disk in the releases that
were installed before the change.

**The active release `4a870c8a…` carries neither a declaration nor a
schema preflight.** It was promoted by the defective builder, so it
is genuinely incomplete. It is still a valid rollback target — a
rollback moves the pointer and does not validate — but it must not be
re-promoted: the repair is a promotion of a revision that carries the
assets, not a re-activation of this one.

## 3. The repair: promote a release built by the fixed builder

The clean repair is a promotion, not an in-place edit. A release is
immutable, and `releases/4a870c8a…` is the **active** release and a
rollback target; mutating it would change what a rollback gives back.
A promotion moves `current` to a new release that carries the assets,
and `~/.mpm/agent_installation` — already a symlink into `current/` —
resolves into them with no change to the symlink itself.

### 3.1 Five acts, and what each one changes

They are separate acts with separate effects and separate revisions.
Do not let one stand in for another.

| act | command | what it changes | revision afterwards |
|---|---|---|---|
| **1. checks** | `mpm-update --dry-run --commit <target>` (§3.3 Steps 3–4) | **nothing.** No backup, no staging in the data root, no lock, no history record, no release directory | unchanged: `4a870c8a` |
| **2. promotion** | `mpm-update --commit <target>` (§3.3 Step 5) | the new release directory, the `current` pointer, a **fresh backup**, the history record, and the stable updater | `2045afcc` |
| **3. service restart** | governed by the **scheduler policy in force** (§3.8): `--restart-services` **on the promotion itself** (it has no effect on a later re-run), or a manual `systemctl --user restart mpm-scheduler` **outside** the promotion window | the **executables the daemons run** | the daemons run `2045afcc`; until then they keep running `4a870c8a`. **The promotion and the destructive restore are governed by different policies** — S1 for the promotion, C1 for the restore, and they never overlap (§3.8) |
| **4. instruction refresh** | §5.2 | `~/.config/opencode/AGENTS.md` (only the marked span) | the provenance line names `2045afcc` |
| **5. OpenCode restart** | by hand | the running agent sessions | only **new** sessions read the refreshed block |

Acts 3, 4 and 5 are **not** part of the promotion. The promotion does
not stop the daemons, does not rewrite the operator's instructions and
does not touch the agent sessions. Each is a separate decision with its
own authorisation; §6 records the residual of leaving them undone.

Act 3 has **one policy in force at a time**, and it is chosen **before**
the promotion, not improvised during it. The host-specific procedure for
this host chooses one — **Policy S1**: the updater owns the unit's
lifecycle, `--restart-services` is passed **once, on the promotion
invocation**, and nothing stops or starts the unit by hand inside the
promotion window (§3.8). **Under that policy, leaving the scheduler on
the old revision is not an available answer.**

### 3.2 Preconditions

| # | precondition | how to check |
|---|---|---|
| P1 | the fix is merged and the target is the pinned revision | `git log --oneline -1 origin/main` → `2045afcc` (§3.3 Step 0) |
| P2 | a Go toolchain, node/npm and python3 are available | `command -v go node npm python3` |
| P3 | the target does **not** already exist under `releases/` — **or** it does and it validates | `ls ~/.mpm/releases/` (§3.4) |
| P4 | disk headroom **and** temporary-space headroom | `df -h / ~/.mpm "$TMPDIR"` (§3.3 Step 1) |
| P5 | no other agent is using the updater | process inventory |
| P6 | the operator has read §5 (the instruction migration) | — |
| **P7** | **the host-specific procedure's current revision carries an approval recorded against its exact SHA-256, covering exactly the three canonical scopes, and the operator has authorised the acts to be performed** | **First half — the approval.** `sha256sum -c /home/v/src/MPM_CUTOVER_4a870c8a_to_2045afcc.md.sha256` must pass, and the **authority register** (§3.8.1) must exist, parse, cover **exactly** the three canonical scopes and carry `accepted: true` for each. Then the **published decision set** for the pull request must be enumerated **completely**, with its completeness demonstrated against **two** independent counts (§3.8.1): the traversal must reach a confirmed terminal state, the retrieved count must reconcile with both the listing's own page cursor and the issue object's comment count, no identifier may repeat, and two consecutive traversals must agree exactly on the id set and on every body hash. A pagination failure, an inaccessible page, a short final page, an unreconciled count, a repeated identifier, a set that changes mid-enumeration, or a body that cannot be parsed **fails closed** — a completeness failure is a **failure, not an empty result**. Then, for **each** scope — `database-safety`, `managed-block-stage-4`, `whole-procedure` — there must be **exactly one** current record, and the candidate is the **highest published version** for that scope: **local filenames never decide which version is current**. That candidate must have a **local** canonical record — whose **filename version equals the body's `attestation_version`**, whose **filename scope equals the body's `scope`**, whose sidecar matches and whose body is canonical — and that record must not be revoked or withdrawn, must carry a known `verdict`, must bind that same `artifact_sha256`, must be listed in the authority register for its scope, and must pass the **publication checks** in §3.8.1 (the publication **retrieved** from its channel, its author equal to the record's `reviewer`, its verdict and scope equal, its `artifact_sha256` equal, its `pr_head` equal where it carries one, and its body byte-identical to the local record). **Revocations are discovered from the same enumeration, not from local files**, and a revocation whose authority cannot be verified **fails closed**. The scope set must be **exactly** those three — a missing scope, an unknown scope, a duplicate, a conflicting claim, a filename/body mismatch or an unauthenticated publication **fails closed**. **Every local record must have a published counterpart**, so a forged local version fails closed; and so does **the local record for the current published version being missing**: **there is no fallback to an older one**. A revoked, withdrawn, inapplicable or invalid highest published version leaves the scope unapproved, and **a newer published decision bound to an obsolete PR head does not hand over to an older one**. **Second half — the authorisation.** The operator's own, act by act (§3.1); the approval of the procedure is not an authorisation of any act in it. **Both halves must hold, and they are separate.** The procedure's own status line reads NOT APPROVED, and it always will: recording the approval inside the file would change the checksum the approval is bound to (§3.8.1) — so **the status line is not the approval state, and the external attestation is**. **If either half fails, P7 fails and nothing is executed** |

### 3.3 Steps

#### Step 0 — pin and verify the target

```bash
TARGET=2045afcc57f1f7603f38e016a2909b85985e48dc
REPO=/path/to/a/checkout/of/this/repository
#   $REPO must NOT be the data root (~/.mpm). Step 1 adds a git worktree,
#   and that writes worktree bookkeeping into $REPO/.git — into the live
#   installation's git directory if $REPO is the data root.

git -C "$REPO" fetch origin
git -C "$REPO" log --oneline -1 origin/main
#   must be: 2045afcc Merge pull request #236 from flowbyte-com/fix/updater-host-assets
```

**Neither checkout on the operator's machine is at that revision**, and
the procedure does not care where the checkout is — only that it can be
put at `TARGET`. Step 1 makes a fresh one.

**Expected revision:** `2045afcc` (the target; the installed revision
is still `4a870c8a` and does not change in this step).

#### Step 1 — build the fixed updater, out of tree

The installed `mpm-update` **is the build from the promoted revision**
`4a870c8a…` — the promotion replaced the stable recovery executable
with the promoted release's own updater, as
`internal/update/orchestrate.go` (`installStableUpdater`) does. So it
is not an older build: it is *this* build, and this is the build that
omits the host assets and has no check for them. It cannot produce a
correct bundle.

Confirm that before going further:

```bash
strings ~/.mpm/mpm-update | grep -oE '\b[0-9a-f]{40}\b' | sort | uniq -c | sort -rn | head -1
#   must name 4a870c8a0d10b4567d9362df4c3dda1fbe0a01ae  (a pre-fix revision)
```

If it names a revision that **already contains the fix**, the recovery
executable is already current and Step 1 can be skipped.

**Build space.** Use a private directory that is **outside the
operator's checkout** and **outside the installed runtime**, and give
it temporary space of its own:

```bash
BUILD_ROOT="$HOME/.cache/mpm-recovery"       # private, disposable, out of the checkout
TMPDIR="$BUILD_ROOT/tmp"                     # the build's scratch space
export TMPDIR
mkdir -p "$BUILD_ROOT/tmp"
df -h "$BUILD_ROOT" "$TMPDIR"
```

**`TMPDIR` must be on a filesystem with headroom.** A checkout build
clones the repository, links six binaries and compiles the plugin
payload; budget about a gigabyte. A rehearsal on a host whose `/tmp`
was a 16 GB tmpfs at 76 % capacity failed with

```
/usr/local/go/pkg/tool/linux_amd64/link: mapping output file failed: disk quota exceeded
```

which reads like a compiler fault and is not one. Point `TMPDIR` at the
same filesystem you checked in P4, or at least a few gigabytes free.

Now the build, from a **clean, isolated** checkout of the pinned
target:

> **`$REPO` must not be the data root.** `git worktree add` writes
> worktree metadata into `$REPO/.git`, so a `$REPO` of `~/.mpm` would
> write into the live installation's own git directory. Any other
> checkout is fine — the procedure does not care where it is. If the
> only checkout available is the data root, **clone** instead:
> `git clone --no-checkout https://github.com/flowbyte-com/mpm.git
> "$BUILD_ROOT/repo"`.

```bash
git -C "$REPO" worktree add --detach "$BUILD_ROOT/src" "$TARGET"
git -C "$BUILD_ROOT/src" status --short          # must print nothing
git -C "$BUILD_ROOT/src" rev-parse HEAD         # must print $TARGET

cd "$BUILD_ROOT/src"
make build                                       # carries the FTS5 flags itself
install -m 0755 .build/bin/mpm-update "$BUILD_ROOT/mpm-update-fixed"
```

**The build flags are not optional.** `AGENTS.md` §1 is explicit:
`CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1`, `go build -tags fts5` **and**
`CGO_LDFLAGS=-lm` are required *together*; the `-lm` omission breaks
CI runners and fresh containers at link time. `make build` sets all
three. If you invoke `go build` directly, set all three yourself:

```bash
CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1 CGO_LDFLAGS=-lm \
  go build -tags fts5 -o "$BUILD_ROOT/mpm-update-fixed" ./cmd/mpm-update
```

**Expected revision:** `2045afcc` (the built binary names it — Step 2
proves it).

#### Step 2 — verify the built updater's identity, before any live change

This step is what keeps the recovery from reproducing the defect it
exists to repair. A build from a pre-fix revision produces a bundle
**without** the host assets, and the pre-fix validator accepts it
without complaint: an operator who promoted with such a build would
install exactly the defective release, silently.

```bash
"$BUILD_ROOT/mpm-update-fixed" --version
strings "$BUILD_ROOT/mpm-update-fixed" | grep -oE '\b[0-9a-f]{40}\b' | sort | uniq -c | sort -rn | head -1
```

**Both must name `2045afcc57f1f7603f38e016a2909b85985e48dc`.** A build
that names anything else — in particular `4a870c8a` — is **rejected**.
Rebuild from the pinned target; do not proceed.

**Expected revision:** `2045afcc`.

#### Step 3 — dry run #1: the candidate, in a scratch workspace

```bash
SCRATCH=$(mktemp -d)
MPM_WORKSPACE="$SCRATCH" "$BUILD_ROOT/mpm-update-fixed" --dry-run --commit "$TARGET"
```

The report must contain **thirteen** `[ok]` lines, and two of them
must be:

```
  [ok] host-assets: the bundle declares and carries its host integration assets
  [ok] schema-preflight: the bundle carries one CREATE TABLE lifecycle_events statement …
```

**STOP if the report has eleven checks, or if it has no
`[ok] host-assets` line.** That is the signature of an updater built
**before the fix** — its builder omits the host assets and its
validator has no host-asset check, so it reports a clean dry run for a
bundle that is missing them. The diagnosis is: *the updater you built
predates the fix*; go back to Step 1. Do not promote.

The scratch workspace must still be **empty** afterwards. A dry run
takes no lock, stages the candidate in a temporary directory outside
the data root, takes no backup and records nothing.

```bash
find "$SCRATCH" -mindepth 1 | head    # must print nothing
```

**Expected revision:** unchanged (`4a870c8a`).

#### Step 4 — dry run #2: the candidate against the **real** live database

Step 3 proves the candidate is well-formed. It does **not** prove the
promotion's schema-collision preflight would pass against the live
table, because a scratch workspace has no live database and
`preflight()` returns early — *"no live database yet; nothing to
collide with"*. The `[ok] schema-preflight` line in Step 3 is about
the preflight **file**, not about the live table.

This second dry run exercises the real check. Run it with
`MPM_WORKSPACE` **unset**, so the updater resolves the real data root
(`$HOME/.mpm`):

```bash
unset MPM_WORKSPACE
"$BUILD_ROOT/mpm-update-fixed" --dry-run --commit "$TARGET"
```

**STOP if it refuses with a schema collision.** A collision means the
live `lifecycle_events` table is not the substrate's table. **Do not
rename or migrate the live table to make the check pass** — a
collision means something else on the host is using that name, and
the refusal is the mechanism working. Read §6.1 of
[`bundle-validation-scopes.md`](bundle-validation-scopes.md) and the
collision message, then decide with the operator.

**This dry run creates no persistent changes.** It is read-only
against the live database: `preflight()` opens the live file with
`openSQLiteRO` and copies it with `VACUUM INTO` into a temporary file
(`os.CreateTemp`), so the live database file is never written and no
transaction is committed. The updater says so on every dry run:

```
  dry run — nothing was changed and nothing was kept:
  (no backup was taken, nothing was staged in the data root, nothing was recorded)
```

Confirm it, rather than trusting it:

```bash
# before
ls -1 "$HOME/.mpm/backups"                      > /tmp/mpm-before-backups.txt
wc -l < "$HOME/.mpm/update-history.jsonl"       > /tmp/mpm-before-history.txt
ls -d "$HOME/.mpm/staging" 2>/dev/null          > /tmp/mpm-before-staging.txt

# … run the dry run …

# after
ls -1 "$HOME/.mpm/backups"                      > /tmp/mpm-after-backups.txt
wc -l < "$HOME/.mpm/update-history.jsonl"       > /tmp/mpm-after-history.txt
ls -d "$HOME/.mpm/staging" 2>/dev/null          > /tmp/mpm-after-staging.txt

diff /tmp/mpm-before-backups.txt  /tmp/mpm-after-backups.txt  && echo "no backup was taken"
diff /tmp/mpm-before-history.txt  /tmp/mpm-after-history.txt  && echo "nothing was recorded"
diff /tmp/mpm-before-staging.txt /tmp/mpm-after-staging.txt   && echo "nothing was staged"
```

The live **database** is written by the running scheduler
continuously, so a byte-for-byte comparison of the database file is
not a valid check while it runs. What the dry run must not do is
create a release directory, a backup, a history entry or a staging
directory — the three diffs above are the check. (A read-only open of
a WAL-mode database touches the existing `-wal`/`-shm` sidecars; their
mtimes move and the WAL index is rewritten. The database file itself
is not written and no transaction is committed.)

**Expected revision:** unchanged (`4a870c8a`).

#### Step 5 — promote, from the build path

```bash
# Policy S1 is in force on this host (§3.8): the flag goes ON the
# promotion, and nothing starts or stops the unit by hand.
# With NO host-specific scheduler policy in force, DROP the flag and
# close the residual afterwards (§3.1, act 3).
"$BUILD_ROOT/mpm-update-fixed" --commit "$TARGET" --restart-services
```

**The flag is on the command line, not only in the prose below.** The
host-specific procedure for this host runs exactly this command, with
exactly this flag, and its own Stage 3 gate refuses the promotion if
the running executable does not resolve into the new release. An
operator who copies the command without the flag leaves the scheduler
on the old revision — which that gate then refuses.

**Run it from its build path.** `mpm-update` does not re-exec the
stable copy (`LocateStableUpdater` is used only by the `mpm update`
command and by the status report), so there is no need to replace
`~/.mpm/mpm-update` by hand before promoting — and doing so would
replace the live recovery executable with a binary that has proved
nothing, before anything has been checked.

**The promotion installs the stable updater itself**, at the designed
moment: it is the last step of the sequence, after the release has
been installed, the pointer moved and the health smoke passed, and it
is written through a temporary file and a `rename(2)` in the data
root. A rehearsal in a disposable installation confirmed the whole
sequence:

```
the pointer moved:      current -> releases/2045afcc57f1f7603f38e016a2909b85985e48dc
the stable updater:     a regular file, byte-identical to the release's own bin/mpm-update,
                        naming commit 2045afcc57f1f7603f38e016a2909b85985e48dc
the fresh backup:       backups/upgrade-<timestamp>/mpm.db
the history record:     outcome: success, backup_dir: …/backups/upgrade-<timestamp>
```

**Before the promotion, take the baseline** that §4.2 compares the
promotion's recovery point against. The counts are the only measure of
freshness the schema fingerprint cannot supply (§4.1), and the baseline
is what makes the comparison possible afterwards:

```bash
# §4.2, Step 1 — the baseline
```

**The scheduler policy is decided before the promotion, not during
it.** A service restart is an operational act with its own
authorisation, so this procedure does not assume one — see §3.1. But
the host-specific procedure for this host **does** require one:

- **If a host-specific scheduler policy is in force** (§3.8 — on this
  host it is **Policy S1**), pass `--restart-services` **once, on this
  promotion invocation**. Do not stop, start or restart the unit by
  hand anywhere inside the promotion window: the updater owns the
  lifecycle, and a manual stop before it creates exactly the
  *"already inactive, so nothing to start"* trap. **Under that policy
  the scheduler must end up on `2045afcc`; leaving it on `4a870c8a` is
  not an available answer.**
- **If no host-specific policy is in force**, this procedure's default
  applies: do not pass `--restart-services`, and close the residual by
  hand afterwards (§3.1, act 3).

**If `--restart-services` was NOT passed**, then after the promotion
`mpm-scheduler` (and `mpm-telemetry`, if it is installed) are still
running the **previous** revision's executables, while the CLI on
`PATH`, the managed block's provenance line and the recovery executable
all name the new one. The residual is real and it lasts until the
daemons are restarted:

```bash
# what the running daemons are actually executing
#
# The units are USER-level (they are installed with `systemctl --user`),
# so the plain `systemctl` reports "inactive" for them and means
# nothing. Use --user.
#
# mpm-scheduler is the unit a stock installation has: install.sh
# installs that one (SERVICE_NAME="mpm-scheduler"). mpm-telemetry is
# OPTIONAL — its unit may simply not exist on this host, and
# "Unit mpm-telemetry.service could not be found" is a red herring in
# a procedure whose purpose is to avoid red herrings. Ask about the
# units that are actually installed:
systemctl --user list-unit-files 'mpm-*' --no-legend
systemctl --user status mpm-scheduler \
  | grep -E "ExecStart|Active"
```

If `mpm-telemetry.service` IS installed, check it the same way.

Two ways to close it, in the operator's order of preference. **Neither
is available while a host-specific scheduler policy is in force** — under
that policy the promotion itself restarts the unit, and the operator does
not touch it:

1. **restart the units by hand**, once the operator has decided that
   is acceptable:

   ```bash
   systemctl --user restart mpm-scheduler
   systemctl --user status mpm-scheduler | grep -E "ExecStart|Active"
   ```

   The running executable must resolve into
   `~/.mpm/releases/2045afcc…`.

   > **Re-running the promotion will NOT do this.** `mpm-update`
   > returns *"already up to date"* as soon as the pointer names the
   > target — that check happens **before** the service handling — so
   > `mpm-update --commit <target> --restart-services` after the
   > promotion reports success and **restarts nothing**. The flag has
   > an effect only during a promotion that actually runs. Confirmed in
   > a disposable installation: the re-run printed *"already up to
   > date at 2045afcc…"* and no unit was touched.

   The alternative is to pass `--restart-services` **on the promotion
   itself** (§3.3 Step 5), if the operator has authorised a restart at
   that point instead.

2. **leave them running** and accept the residual until the next
   reboot or the next service restart. The daemons are long-lived
   schedulers; they do not pick up the new binaries on their own.
   **This is not available under a host-specific scheduler policy**: the
   scheduler would keep running `4a870c8a` while the pointer, the CLI
   and the recovery executable all name `2045afcc`.

The promotion's own report says this in as many words when it leaves
the daemons alone:

```
note: managed service(s) still running (mpm-scheduler) were NOT restarted
(no --restart-services); they may still use the old executable until restarted
```

What this residual does NOT affect: the CLI, the managed block's
provenance line, and the recovery executable all name the new revision
immediately after the promotion. Only the daemons lag.

**Expected revision:** `2045afcc` (the CLI, the pointer, the release
and the recovery executable). The daemons still run `4a870c8a` until
they are restarted.

#### Step 6 — verify the repair

```bash
# the advertised paths now resolve
ls -l ~/.mpm/agent_installation
head -3 ~/.mpm/agent_installation/mpm-agent-protocol.md
test -f ~/.mpm/agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md && echo OK

# the release declares the twelve assets, and every one matches
python3 - "$TARGET" <<'PY'
import hashlib, json, os, sys
root = os.path.join(os.environ["HOME"], ".mpm", "releases", sys.argv[1])
man = json.load(open(os.path.join(root, "config/runtime-assets.json")))
bad = []
for rel, meta in man["assets"].items():
    path = os.path.join(root, rel)
    got = hashlib.sha256(open(path, "rb").read()).hexdigest() if os.path.isfile(path) else ""
    if got != meta["sha256"]:
        bad.append(rel)
print(f"{len(man['assets'])} assets declared, {len(bad)} digest mismatches", bad)
PY

# the binaries, the recovery executable and the pointer are the new ones
~/.mpm/bin/mpm --version
~/.mpm/mpm-update --version
readlink ~/.mpm/current

# the promotion is recorded
tail -1 ~/.mpm/update-history.jsonl
```

**Every one of these must be true:**

| check | expected |
|---|---|
| the advertised paths resolve | yes |
| the declaration | **12** assets, **0** digest mismatches |
| `~/.mpm/bin/mpm --version` | names `2045afcc` |
| `~/.mpm/mpm-update --version` | names `2045afcc` — the promotion installed it |
| `~/.mpm/current` | `releases/2045afcc…` |
| the last history record | `outcome: success`, `to_rev: 2045afcc…`, a `backup_dir` |

The payload must still be present (the repair adds to the release, it
never replaces a payload file): `agent_installation/runtime_payloads.json`
and every file it names.

**Expected revision:** `2045afcc`.

### 3.4 The recovery point: when it is created, and which one to use

**The promotion creates a fresh backup of its own.** The exact point is
inside the promotion's sequence, and it matters:

```
resolve → lock → stage the candidate
      → CREATE THE BACKUP          ← the recovery point is created here
      → validate the bundle → schema preflight
      → [STOP the managed units, only with --restart-services]
      → install the release → move the pointer
      → establish the stable paths
      → health smoke (a live write path — see §6)
      → install the stable updater (last, atomically)
      → [START the stopped units back, and verify their identity]
      → record the outcome
```

So the backup is taken **after** the candidate is staged and **before**
the bundle is validated, before the preflight and before the smoke. It
therefore exists even when the promotion later fails, and it is a
snapshot of the live database at the moment of the cutover.

**It is verified at the moment it is created.** `BackupLiveDB` copies
the live database with `VACUUM INTO` and then verifies the copy: it is
non-empty, it carries the `SQLite format 3` header, `PRAGMA
integrity_check` returns `ok`, and its **table set equals the live
table set**. A backup that fails verification aborts the promotion; it
is never left half-made.

**Which recovery point to use.**

| recovery point | what it is | when to use it |
|---|---|---|
| **the one this promotion creates** — `~/.mpm/backups/upgrade-<timestamp>/` | a copy of the live database **at the moment of the cutover** | **the recovery point for this cutover.** This is the one a rollback-with-restore uses. |
| `upgrade-20261107T201536Z` | the point for the *earlier* promotion (`77738cfd` → `4a870c8a`) | only to recover from **that** promotion. **Never a routine restoration point for this cutover** — restoring it would roll the data back by the distance between that moment and now. |
| `upgrade-20261102T161236Z` | a stale historical point | **never.** It is older than the Nov-7 point by days of live data. |

The history record names the recovery point the promotion took:

```bash
tail -1 ~/.mpm/update-history.jsonl | python3 -m json.tool
#   "backup_dir": "/home/v/.mpm/backups/upgrade-<timestamp>"
```

The promotion also records the database's schema fingerprint before
and after, in the same record (`pre_fp`, `post_fp`). If the two
differ, the live schema changed during the promotion and the record
says so.

The **stop** and the **start** are two events at two different points,
and the order is not incidental. With `--restart-services` the updater
stops the active units **after** the candidate is built and preflighted,
and starts them back **after** the live smoke test and the stable-updater
install — so a restarted unit resolves into the **new** release, and the
updater verifies that it does (`restarted: mpm-scheduler (verified
against 2045afcc…)`). Only units that were **active** are stopped, and
only those are started back: a service that was inactive before the
promotion stays inactive — and a policy that wants a restart it cannot
get is a policy that cannot proceed (§3.8).

**Verify a recovery point before you rely on it.** See §6.1 for an
older point, and **§4.2** for the independent verification of the
recovery point this promotion creates — the one that matters, because it
is the point a rollback would use.

### 3.5 Stop conditions

Stop and read. None of these is a reason to pick a different revision.

| situation | what it means | what to do |
|---|---|---|
| the dry run reports **eleven** checks, or has no `[ok] host-assets` line | the updater you built **predates the fix**; its builder omits the host assets and its validator cannot see that | **STOP.** Rebuild from the pinned target (Step 1). Do not promote |
| any dry-run line is not `[ok]` | the candidate is not valid | **STOP.** Read the message; a `[ok] schema-preflight` that is absent means the preflight file is not one supported statement |
| the real-data-root dry run reports a **schema collision** | the live `lifecycle_events` table is not the substrate's table | **STOP.** Do **not** rename or migrate the live table. Something else on the host owns that name |
| the promotion reports `outcome: failed` | the promotion stopped | **STOP.** Read `~/.mpm/update-history.jsonl` before retrying. The pointer is restored on every failure path |
| the promotion reports *"already up to date"* | the pointer already names the target — the promotion did not run, so **nothing was validated and no service was restarted** | It is not a success. Restart the units by hand if that is what you wanted (§3.3 Step 5). If you expected a promotion, the pointer is already where you thought it was — check `readlink ~/.mpm/current` |
| **partial success — the release directory exists but the pointer did not move** | the promotion failed after the install stage | **Re-run the same command.** An existing release that validates is **re-used**, not refused — see the note at the end of this table. Do **not** choose another revision, and do **not** delete the release directory |
| **partial success — the pointer moved but the stable updater was not replaced** | the promotion failed after the pointer moved | The updater restores the pointer on this path. The stable updater is replaced **only** on success, so `~/.mpm/mpm-update` is still the old build — which is correct, and it is the recovery tool for the runtime that is running. Re-run the same command |
| **failed service identity** — after `--restart-services`, the history record or the promotion's report carries a warning that a unit runs something other than the active release | the daemon was not actually restarted, or it restarted from another path | The updater records a **warning**; it does not claim a clean restart. This is a **post-promotion recovery action, outside the promotion window** — S1 forbids touching the unit *inside* the window, and permits exactly **one** such restart after it. Restart the unit by hand, then **repeat the Stage 3 gate**: the running executable must resolve into `~/.mpm/releases/2045afcc…`, checked through `/proc/<pid>/exe` (§3.8). Do not loop restarts |
| **degraded rollback** — the promotion reports `DEGRADED:` | the live schema changed and the previous release cannot read it, **or** the post-smoke fingerprint failed so the schema state is unknown | The pointer **stays at the new release** (the binaries match the data). Recover with `mpm-update --rollback --with-db-restore --force` — which restores the **pre-update backup the promotion took** and overwrites newer data — or fix forward. **That command is the last step of an operation, not the operation**: §3.6 lists the three things required before it (identify the point, measure the data loss, approve it bound to the measurement) and the journal-mode verification required after it. Do not delete anything |

**About "the release already exists".** An existing
`releases/2045afcc…` is refused **only when it does not validate**. If
it validates — the normal case after an attempt that failed *after* the
release was installed — **re-running the same command is the correct
action**. A rehearsal in a disposable installation confirmed it: with
the release directory present and the pointer moved back, the same
command succeeded, re-used the release and moved the pointer forward.

The refusal that matters is the one for a release that does **not**
validate (for example `4a870c8a`, which carries no declaration):
*"A release is immutable, so it will not be replaced. Do NOT delete it
by hand — it may be the operator's rollback target."*

### 3.6 Rollback

```bash
~/.mpm/mpm-update --rollback
```

The rollback moves the pointer back. **It does not validate its
target**, so the defective `4a870c8a` remains a usable rollback target
— which is the property that makes this whole procedure recoverable.
**Do not delete it.**

A database restore is a **separate, explicit** operation:

```bash
~/.mpm/mpm-update --rollback --with-db-restore --force
```

`--with-db-restore` overwrites newer user data and is refused without
`--force`. It restores **the backup the promotion took** (§3.4) — not
the Nov-7 point, and never the Nov-2 one.

**That command is the last step of an operation, not the operation.**
Three things come before it and one comes after, and this is where an
operator meets them:

| | what | where |
|---|---|---|
| **before** | **identify** the point the command will use — read it out of the record that matches the active revision; do not infer it | §4.3, step 1 |
| **before** | **measure** the data loss, in writing, against a read-only comparison of the point and the live database | §4.3, step 2 |
| **before** | **approve it explicitly, bound to the measured figure** — the approver states the number they are accepting. The approval to *promote* is not the approval to *destroy data* | §4.3, step 3 |
| **before** | **quiesce the writers** — the updater refuses a restore while a definite holder has the database open, and `--force` does not bypass that. On the host procedure's **Policy C1** the managed units are stopped **by hand**, under a **separate authorisation**, inside a recorded **maintenance window** | §4.4, §3.8 |
| **after** | **verify the restored database** — record for record against the recovery point, with the one permitted health-check audit addition (§4.6) | §4.6 |
| **after** | **verify the journal mode** the restored database is actually in, and re-establish it if it is not | §4.5 |

**Do not add `--restart-services` to that command.** The command above
carries no flag, and on this host it must not carry one: with the flag, the
updater stops the unit, restores the database, swaps the pointer, runs its
health check and then **starts the unit itself** — inside the rollback,
before the command returns, and on a database whose journal mode no gate has
yet verified. Without it, the updater starts nothing on any path and
refuses the restore if a managed unit is still active. The host procedure's
**Policy C1** governs that path: the units are stopped by hand, under a
separate authorisation, and started by hand only after the restored database
has been verified (§4.4, §3.8).

**On this host the host-specific procedure carries the executable form of
all of it**, and its §10.5 is normative for its own rollback and
degraded-stop paths: `measure_restore_impact` (the point's SHA-256,
per-table before/after counts, `rows_would_be_lost` /
`rows_would_be_resurrected`), an approval **bound** to that measurement
(`RESTORE <rows_would_be_lost>` — a bare `RESTORE` is refused), the
managed-block state recorded, and `verify_wal_state` /
`reestablish_wal` after the restore. **Follow that form** (§3.8).

A runtime rollback leaves the managed block alone. **That is correct**,
and §3.7 says why — and what *not* to do afterwards.

### 3.7 The managed block after a runtime rollback

**Leave the refreshed block in place.** A runtime rollback moves the
pointer back to `4a870c8a…`; it does not touch
`~/.config/opencode/AGENTS.md`, and that is the correct outcome.

The reason is the hook name. The rolled-back release runs the **V2**
plugin, and the V2 plugin registers `ctx.session.hook("context", …)` —
the name the **refreshed** block carries. The pre-refresh block, and
every `.bak` of it, carries the retired V1 name
`experimental.chat.system.transform`, which the V2 plugin **does not
register**: in `dist/index.v2.js` the V1 name appears only inside a
comment describing the V1→V2 mapping. Restoring the backup therefore
reinstates a block that names a hook the running plugin does not
register — the same defect the refresh exists to fix.

So, after a runtime rollback:

| do | do not |
|---|---|
| leave the refreshed block in place | restore `AGENTS.md.bak*` |
| let the block's `commit=2045afcc…` stand — it records where the block was **rendered from**, and that remains true | try to re-stamp the block: the rolled-back release carries **no** refresher and **no** snippet, so there is nothing to re-stamp it with |
| treat "put the block back to its pre-cutover text" as host-asset remediation with its own authorisation | treat the `.bak` as the block's rollback target |

The `.bak` file remains the right rollback target for the **refresh
itself** — a file-corrupting accident — and the wrong one for a
**runtime rollback**. Those are different acts and they need different
words.

**What a rollback leaves behind, and what it does not.** The corrected
host-specific procedure records this in its §10.5.3, and this guide
agrees with it:

| left behind | why it is left alone |
|---|---|
| the block's provenance still names `2045afcc`, while the runtime is `4a870c8a` | the line records **where the block was rendered from**, and that is still true. Re-stamping it is impossible — the rolled-back release carries no refresher — and hand-editing it is not permitted |
| the **retired V1** block, if the refresh never ran | that is the pre-cutover condition, unchanged by the rollback |
| the block's references to `~/.mpm/agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md` and `…/mpm-agent-protocol.md`, which the `4a870c8a` bundle does not carry | **host-asset remediation**, and it is **separately authorised**: either re-provision the host assets into the `4a870c8a` release, or re-promote `2045afcc` |

So **a rollback after the refresh is not a complete cutover on the old
revision.** The host is back on `4a870c8a` with the host-asset gap
re-exposed; the gap is repaired by re-promoting, not by restoring a
block.

### 3.8 The host-specific cutover procedure for this host

`/home/v/src/MPM_CUTOVER_4a870c8a_to_2045afcc.md` is the cutover
procedure for exactly this revision pair
(`4a870c8a0d10b4567d9362df4c3dda1fbe0a01ae` →
`2045afcc57f1f7603f38e016a2909b85985e48dc`) on this host. Its current
revision is SHA-256
`31ccfb7716b24fb0c04b1775f2700646c43b17ac8184e9d39364f934ca893fa6`,
published beside it in `MPM_CUTOVER_4a870c8a_to_2045afcc.md.sha256`.

> **That revision is NOT APPROVED, and no part of it may be executed.**
> Its own status line says so: the approval of the earlier checksum is
> **void** for it, because the executable sequence changed, and **a
> fresh independent review is required first**. The operator must
> check that checksum before running anything — a procedure that has
> changed under its own feet is not a procedure to run on trust.

> **Only the checksum in the sidecar counts, and it moves.** Four
> earlier checksums appear in this guide's history and **none of them is
> live**, and each is named in the procedure's own approval record:
>
> | checksum | what happened to it |
> |---|---|
> | `9fc056b9ec089276b45f486bd8404242951765a8de5c548960d04c19c7bc7f78` | approved by an earlier independent review; that approval is **void** here, because the executable sequence changed |
> | `64303ebc3b94be196a07db8ea8b1969b0f6384c3bb3ae17bcffe4af4618b71e7` | **never approved** — returned REQUEST CHANGES by the Stage 4 review |
> | `554c8183b4ce696b6ed247cff53f9242a85296010ce5d93f533a9caf0f882ef8` | **never approved** — returned REQUEST CHANGES by the Phase 3 database-safety review, findings N1/N3/N4 with N2 latent |
> | `20ec16887ec3ceafbd998e19ba2b99474817dc676c1268f7e5a7862ad51361ec` | **never approved** — returned REQUEST CHANGES by the Phase 4 review, N4 alone |
>
> The current revision supersedes all four. **Read the sidecar; do not
> trust a checksum quoted anywhere else**, including in this document.

**It is the procedure for this host — once it is approved and
authorised.** Its stages and this guide's steps are the same sequence —
pin, build the bootstrap updater out of tree, verify its identity, dry
run, promote, verify, refresh, relaunch — so there is no second
sequence to choose between, and no reason to assemble one out of this
guide alone.

**Two permissions, and this section grants neither.**

1. **Independent approval of the exact checksum.** The revision must
   carry an independent reviewer's approval recorded against **its
   current SHA-256** — the one in the sidecar — and not an earlier
   revision's. An approval of a superseded checksum does not carry
   forward: the procedure's own record voids it whenever the executable
   sequence changes, and it has already voided one four times.
2. **Separate operator authorisation, act by act.** Even an approved
   procedure is not an instruction to run. Each act in §3.1 — checks,
   promotion, service restart, instruction refresh, OpenCode restart —
   is a separate decision with its own authorisation, and the
   destructive one (a database restore, §4.3) needs its own approval
   **bound to the measured loss**. **Approval of the procedure is not
   approval of any act in it.**

**Both must be in hand before the first command is typed.** If either
is missing, the answer is to obtain it — not to run the procedure and
ask afterwards.

**The precedence rule, in one line.** The host-specific procedure
governs **every act it defines**. This guide governs the acts it
defines and the host-specific procedure does **not**, and it adds the
**database-safety requirements in §4**. Where the two describe the same
act, **follow the host-specific procedure**; where they describe the
same act **differently**, that is a defect in one of them — **stop and
report it** (§8), do not pick the one that reads better. A conflict
that survived one round of reconciliation is exactly what that rule is
for.

The acts they both describe, and what governs each:

| act | this guide (general) | the host-specific procedure | what governs |
|---|---|---|---|
| **the scheduler** | a service restart is a separate, separately-authorised act; the residual may be closed by hand afterwards | **Policy S1** — the updater owns the unit's lifecycle: `--restart-services` is passed **once, on the promotion invocation**, and nothing stops or starts the unit by hand inside the promotion window | **S1.** Pass the flag on the promotion (§3.3 Step 5 carries it); do not touch the unit by hand; **do not leave the scheduler on `4a870c8a`** |
| **the scheduler, on the destructive-restore path** | §4.4 — the units must be stopped and the sessions closed before a restore | **Policy C1** — a *separate* policy, for the destructive restore only: the restore is invoked **without** `--restart-services`; the units are stopped **by hand** under a **separate authorisation**, inside a recorded **maintenance window**; the updater **starts nothing on any path** | **C1**, and it is **not** S1. The two never overlap: S1 governs the promotion, C1 governs the destructive restore. **A restore must not carry `--restart-services`** |
| **the promotion's service gate** | the residual is checked by reading `systemctl --user status` | a mandatory `/proc/<pid>/exe` gate: the running executable must resolve into `releases/<REV>/` | **S1's gate**, which is stronger than reading `status` |
| **the managed-block refresh** | §5.2 above | Stage 4 — the same RELEASE.json-sourced provenance, the same keep decision, acceptance of an `unrepaired` block so a failed refresh does not have to be "repaired" by restoring the retired copy, and a post-check that judges the outcome against a **per-run snapshot** of the file rather than against the pre-cutover backup | **both**; they agree, and this guide carries the same discipline |
| **the managed block after a runtime rollback** | §3.7 — leave the refreshed block in place; never restore the `.bak` | its §10.5.3 and `assess_rollback_block_state` — the block is **never** restored to its pre-refresh copy; the state left behind (stale provenance, retired-V1 state, dangling host-asset references) is **recorded**, and the host-asset gap needs separately authorised remediation | **both**; the current revision removed the restoration that used to contradict §3.7 |
| **the recovery point** | §3.4 and §4.2 | the promotion log must name it, and the history entry must match it, with `pre_fp` and `post_fp` present | **both** |
| **a destructive restore** | §4.3 — measure the delta, write it down, separate approval; §4.5 — the journal-mode verification | its §10.5.1–§10.5.6 — `measure_restore_impact` (the point's SHA-256, per-table before/after counts, `rows_would_be_lost` / `rows_would_be_resurrected`), an approval **bound** to that measurement (`RESTORE <rows_would_be_lost>`), the block state recorded, `verify_wal_state` / `reestablish_wal` after the restore, and the **no-restart** ordering (its §10.5.5) | **both**; the current revision carries what this section previously said it did not |
| **the restored database** | §4.6 — the record-by-record comparison, and the one audit row it may add | its §10.5.6 `verify_restored_database` — the table set, **every** recovery-point record present and **unmodified** (primary-key-aware, content-exact), the one explicitly allowed health-check audit addition, the recovery point's own SHA-256, the measured estimate, and `PRAGMA integrity_check` — **before any start** | **both**; the guide says what it means, the procedure carries the executable form |

**§3.7 and the procedure's §10.5.3 are now the same rule.** The
procedure no longer restores the pre-refresh block after a runtime
rollback: it **records** the state the rollback leaves behind — the
stale provenance, the retired-V1 state if the refresh never ran, and
the dangling host-asset references — and names the remediation as
separately authorised. That is exactly §3.7's rule, and the two
documents no longer disagree about it.

**The procedure's repair path is no longer deadlocked.** An earlier
revision accepted an `unrepaired` block — so that a failed refresh
would be repaired by **re-running** Stage 4 rather than by restoring
the retired V1 copy — but its post-check compared the result against
the **Stage 0b backup**, and the refresher inserts one newline at the
END-marker boundary on every rewriting run. A second rewrite therefore
grows the suffix again, the post-check fails on a **correct** block,
and re-running fails identically, because the refresher is then a
no-op. Measured: suffix 7 → 8 → 9 bytes.

**The current revision fixes it, and this is the change that matters
to the operator.** Stage 4 now takes a private, byte-exact snapshot of
the file immediately before **each** refresh
(`$BLOCK_PRE_RUN` = `AGENTS.md.pre-run`, mode `600` in a mode-`700`
directory) and judges the outcome against **that** snapshot, through
the new `verify_refresh_outcome` helper. The pre-cutover backup is
**kept** as the historical baseline and the recovery copy — it is
still compared for the prefix — and it is simply no longer the suffix
comparator. The refresher's own report classifies the outcome
(`already has current managed section; no-op` vs `refreshed from
snippet`), and an unrecognised report is a **stop**, not a guess. The
`unrepaired` path can therefore complete: the failed first attempt is
followed by a legitimate second rewrite, and the second is judged
against the state the first left behind.

**What is still true, and is the operational point.** The newline
behaviour is unchanged and is now stated as a measured fact: the
rewrite path replaces exactly the `BEGIN`..`END` span and inserts
**one** newline immediately after the `END` marker — at the **start**
of the suffix — while the no-op path writes nothing at all. The content
outside the markers may therefore differ from the pre-cutover baseline
by more than one newline, and **a snapshot taken once at the cutover is
the wrong thing to compare against** after a second attempt.

**What this guide still adds.** §4 is not superseded. It is the part
the host-specific procedure does not carry in the same form:

- **§4.2 — the freshness assessment.** The host-specific procedure
  gates the *identity* of the recovery point (the promotion log names
  it, the history entry matches it, the fingerprints are present) and
  says honestly that a backup count lower than the live baseline is
  normal. The operator's own **pre-promotion baseline** and the
  **count comparison** are §4.2's, and they are what say whether the
  recovery point holds the data you expect.
- **§4.4 — the writer scan, its limits, and the quiescence rules.** The
  host procedure refuses a restore under a live writer; §4.4 is what
  says why, and what `--force` can and cannot bypass.
- **§4.6 — what the restored database must reproduce.** The host
  procedure's `verify_restored_database` carries the executable form
  (its §10.5.6); §4.6 says what the comparison means, what the one
  permitted audit addition is, and what the verification does **not**
  prove.
- **§4.7 — read-only inspection and scratch safety.** The rules for
  handling a copy of the corpus.

**Section numbering.** Every `§` reference below is a section of
**this** document. The host-specific procedure's own sections are
named "its §…", so the two are never confused.

**Two conditions on the cutover.**

1. **The host-specific procedure must be approved before any part of it
   is executed, and the acts must be authorised separately.** Its
   current revision carries no internal approval — its own status line
   says so, and it always will (§3.8.1). Approval means an **attestation
   bound to the procedure's exact current SHA-256**, covering the scopes
   §3.2 P7 requires; authorisation is the operator's own decision, act
   by act (§3.1). Neither is conferred by this guide, and neither
   follows from the other.
2. **§4.3's measured delta, §4.5's journal-mode verification and §4.6's
   record-by-record comparison must be done as §4 describes them.** The
   current revision carries equivalents (its §10.5.1, §10.5.2,
   §10.5.4, §10.5.6), so this is a matter of *running* them, not of
   adding them — with one correction: §4.5's journal-mode step is a
   **verification with a recovery path**, not a gate on the restore,
   because MPM re-establishes WAL by itself on its next connection. The
   premise is corrected below.

#### 3.8.1 The approval attestation — specified, and not yet issued

**The circularity, stated exactly.** The procedure's own approval
record lives **inside** the procedure, and its own rule is that *"any
further edit to this file's executable sequence"* invalidates the
approval. Those two facts cannot both be satisfied by writing the
approval into the file: **editing the file to say APPROVED changes the
file, changes its SHA-256, and therefore changes the very value the
approval is bound to.** The regress does not terminate — "write
APPROVED in" produces a new checksum, which needs a new approval, which
would have to be written in.

**The resolution is to record the approval outside the procedure.** The
procedure already points at it: its approval record says the checksum
"is published in the sidecar …; **a reviewer records the verdict
against that value**" — against the *value*, not in the file. So the
approval is an **attestation**: a separate, small, write-once record
bound to the procedure's exact SHA-256, kept beside it.

**A local file and a checksum beside it are not enough.** A `.sha256`
sidecar is an **integrity** control: it detects corruption and
accidental edits, and anyone who can write the record can recompute it.
Authenticity has to come from somewhere else — from a **publication
that is independently retrievable and whose author the channel
identifies**. The three properties below are what make the attestation
an approval rather than a file.

##### The three canonical scopes

Exactly three, and no others:

| scope | what it covers | the review that decides it |
|---|---|---|
| `database-safety` | the restoration path — the measurement, the bound approval, the writer refusal, the record-by-record verification and the journal mode (N1–N4) | the independent database-safety review |
| `managed-block-stage-4` | the managed-block refresh, and the V2-compatible rollback (§3.7) | the Stage 4 review |
| `whole-procedure` | the procedure as a whole — the promotion path, the service gate, the failure branches | the final independent review |

**A scope name outside this list is not a scope.** Synonyms,
abbreviations and additions are rejected; a superset is rejected as
firmly as a subset, because a superset can smuggle in a scope that was
never reviewed and an unknown name cannot be interpreted at all.
**Each scope is a separate published decision**, so the record carries
one scope and one reviewer per file rather than a single `reviewer`
field over several scopes.

##### The authority boundary — what this mechanism does and does not establish

**Three published approvals are three *decisions*, not three
*authenticated human identities*.** That distinction is the whole of
this subsection, and the specification must not blur it. Four things
are different, and only two of them are mechanically checkable here:

| | what it is | what establishes it |
|---|---|---|
| **authenticated publisher identity** | who the channel says published the comment | the channel's own authentication — mechanically checked, as the `reviewer` equality below |
| **claimed reviewer role** | the role the publication asserts for itself | **nothing but the publication's own text.** It is a claim, and it is recorded as one |
| **independently established reviewer authority** | whether the principal behind that publication is the one authorised to decide that scope | **not established by this mechanism at all.** Nothing inside an attestation can establish it |
| **operator acceptance of that authority** | the operator's own decision to accept a named principal for a named scope | the **authority register**, below — the operator's record, kept outside the attestation set |

**Every current publication on this host is made through the single
shared `flowbyte-com` GitHub account.** The channel therefore reports
the same author for all three reviews, and the `reviewer` field in all
three records will be that same string. **This specification does not
claim that the account distinguishes Bravo, Charlie and Echo, because
it does not.** A party controlling that account can publish a record
for any scope, and can amend the procedure, this guide and the branch
as well — the account is the trust anchor of the whole project.

**The `scope` field is not self-authorising.** A record that names
`whole-procedure` is a *claim* to have decided that scope. It confers
no authority. Until the operator's register accepts a principal for
that scope, the record establishes publication provenance and nothing
more.

**The operator establishes the reviewer-to-scope assignment, and records
it in the register.** The authority register is a separate file the
operator maintains — `MPM_CUTOVER_4a870c8a_to_2045afcc.authority.json`
— kept **beside** the attestations, **in the same directory**. One entry
per canonical scope:

```json
{
  "database-safety": {
    "accepted": true,
    "authority_basis": "<how the operator established this assignment>",
    "principal": "<the principal the operator accepts for this scope>",
    "publications": ["<the publication id the operator accepted for this scope>"],
    "reviewed_at": "<RFC 3339 UTC>"
  }
}
```

The check requires the register to exist, to parse, to cover **exactly**
the three canonical scopes, to carry `accepted: true` for each, and to
list the attestation's `publication.id` among that scope's
`publications`. **A publication the register does not list fails
closed.**

**What the register establishes, and what it does not.** It establishes
that **the operator recorded an assignment** — a dated, attributable
decision. It does **not** establish that the principal is who it claims
to be, that the publication's content came from that principal rather
than from anyone who can post as the account, or that the three
principals are distinct. **A locally created register is an operator's
own record, not independent evidence of anything.**

**The boundary, stated exactly, with no protection invented.** The
register is an **operator-controlled local policy record**. It is **not**
isolated from anyone who can modify the attestation directory: it is
**in** that directory, and **sharing a directory provides no filesystem
isolation**. Anyone who can write the attestations can write the
register; anyone who can read the attestations can read it. **The
register does not independently authenticate a reviewer** — it records
the operator's own acceptance of a reviewer-to-scope assignment, and
that acceptance is the operator's word, not an attestation and not
evidence from any other party.

Keeping the register beside the attestations rather than among them is
**record hygiene**, not a security boundary: the attestations are the
channel's evidence, the register is the operator's policy, and the two
are kept distinguishable because they answer to different things. What
protects the register is exactly what protects the attestation
directory itself — the host's filesystem permissions and the operator's
custody of it. **If that custody is not trustworthy, then neither the
register nor the attestations are**, and the check fails closed on the
attestations long before it matters which file an attacker edited.

**The trust boundary, stated plainly.** The shared account is the
anchor; GitHub account authentication is **not** tamper-proof
protection against compromise or misuse of that account, and nothing
here changes that. **No cryptographic signature is introduced**, and
deliberately so: a signature is only as strong as the separation of
keys, and a key stored beside the account's own credentials attests
*"the holder of some key produced this"* — the same party. It would
prove nothing the account does not already prove. Separate principals —
separate accounts, or keys held by genuinely distinct parties — are
the real remedy, and they are an organisational choice the operator has
not made.

**What the mechanism does buy, and it is not nothing.** Against an
attacker who can write local files only, it is effective: a forged
local record fails, because the comparison is between the *retrieved*
bytes and the *local* bytes, and not between two local claims. It is
against the shared account itself that it offers no protection, and it
does not pretend to.

##### The record — one file per scope, per version

`MPM_CUTOVER_4a870c8a_to_2045afcc.approved.<scope>.v<N>.json`, with its
checksum in the matching `.sha256`. **A record is never edited in
place.** The body is canonical JSON — keys sorted, one key per line,
UTF-8, LF, exactly one trailing newline:

```json
{
  "artifact": "MPM_CUTOVER_4a870c8a_to_2045afcc.md",
  "artifact_sha256": "31ccfb7716b24fb0c04b1775f2700646c43b17ac8184e9d39364f934ca893fa6",
  "attestation_version": 1,
  "pr_head": "<the PR head this reviewer read, or omitted>",
  "publication": {
    "channel": "github",
    "id": "<the publication's identifier at that channel>",
    "kind": "issue-comment",
    "number": 238,
    "repo": "flowbyte-com/mpm"
  },
  "reviewed_at": "<RFC 3339 UTC>",
  "reviewer": "<the author's identity as the channel reports it>",
  "scope": "database-safety",
  "verdict": "APPROVE"
}
```

`publication` names **where to retrieve the evidence**, not the evidence
itself: the channel, the repository, the kind of record and its
identifier. A channel that cannot supply all three — an immutable
identifier, an independent retrieval path, and the author's identity —
is not an acceptable channel. The record **does not contain its own
hash**; nothing here is self-referential.

**The filename is part of the record's identity, and the check binds it
to the body.** The filename is not a label an attacker may rewrite:

- the filename's `<scope>` must equal the body's canonical `scope`;
- the filename's `<N>` must equal the body's `attestation_version`;
- `<N>` must be a **canonical** positive integer — digits only, no
  leading zeros, so `v1` and `v01` are not two spellings of one
  version;
- no two records for a scope may carry the same `<N>`;
- the body must round-trip: re-serialising it canonically must
  reproduce the file's bytes exactly.

**Any mismatch is rejected.** A filename/body version mismatch, a
filename/body scope mismatch, a duplicate version, a malformed version
and a non-canonical body are all stops.

**Why this binding is load-bearing.** Without it, a valid local `v1`
record can simply be **renamed to `v2`** and its sidecar recomputed.
Every other check still passes — the *published* bytes are those of the
original `v1` — so the fabricated `v2` becomes the scope's current
version, and a revocation of `v1` is thereby defeated: the rule that a
revoked version leaves the scope unapproved with no fallback never
fires, because the fabricated version is not the revoked one. The
binding above is what closes that hole, and it is the reason the check
compares the filename to the body rather than trusting either alone.

##### The publication is the evidence, and how it is checked

The operator **retrieves the publication from the channel, independently
of the local file** — for a GitHub issue comment, for example, by
reading the comment's own body and author through the API — and checks
seven things:

| # | what must be established | how |
|---|---|---|
| 1 | **the publication's authentic source and the reviewer's identity** | the retrieved record's **author**, as the channel reports it, must equal the record's `reviewer`. A URL that merely resolves proves nothing |
| 2 | **the reviewer is authorised for the claimed scope** | the retrieved body must itself name the scope it decides, and the record's `scope` must equal it. A reviewer who published a different scope authorises nothing here |
| 3 | **the published verdict and the published scope** | the retrieved body's `verdict` and `scope` must equal the local record's |
| 4 | **the exact procedure SHA-256** | the retrieved body's `artifact_sha256` must equal the local record's, and that must equal the procedure's sidecar |
| 5 | **the final PR head, where relevant** | if the retrieved body carries a `pr_head`, it must equal the PR head in force. A scope that reviewed this guide must carry the head it read — **and a PR-head binding does not travel past an amendment to this guide** |
| 6 | **the publication's content integrity** | the retrieved body must contain the local record's canonical body **verbatim**, between the fixed markers `<!-- MPM-APPROVAL-BEGIN -->` and `<!-- MPM-APPROVAL-END -->`; the bytes strictly between the marker lines must be byte-identical to the local file, and the local file's SHA-256 must match its own `.sha256` sidecar. **A hash stated only by the local record proves nothing** — the comparison is between the *retrieved* bytes and the *local* bytes, not between two local claims |
| 7 | **the evidence is neither superseded nor revoked** | the version rules below |

**What this establishes, and what it does not.** It establishes that the
decision was published by the **named account** on the named channel,
that the retrieved content is byte-identical to the local record, that
the record binds the procedure's exact checksum, the scope and the
verdict, and that a later edit to the publication is **detected**
because the retrieval no longer matches. It does **not** establish
which reviewer decided the scope, that the account is controlled by the
person named, cryptographic non-repudiation, or that the judgement is
correct — the authority boundary above says which of those is
established where, and the trust boundary says what none of them buys.
**No signature is required or claimed**; where the publishing principal
and the signing key would be the same party, a signature would prove
nothing the account does not already prove.

##### The published decision set — and how its completeness is established

**The decision set is what the channel holds, not what the directory
holds.** Approvals and revocations are both discovered by enumerating the
channel; the local directory is a **mirror** of that set, never its
source. The enumeration must be **exhaustive**, and — this is the part
that is easy to get wrong — **exhaustiveness must itself be
demonstrated**. **A short page, an absent next-page link, or an HTTP 200
is not proof that the set was fully retrieved.**

**The channel and the scope of the enumeration.** The authoritative
channel is the GitHub issue-comment API for the pull request that carries
the reviews: repository `flowbyte-com/mpm`, issue **238**, **comments
only**. The scope of the enumeration is **every comment on that issue** —
not only the comments that look like attestations. A narrower scope is
not safe: a decision that has not yet been recognised as an attestation
would be invisible to a filtered query, and the filter is exactly where
a truncation would hide.

**The procedure.** It is reproducible, and every step is a measurement
rather than an assertion:

1. **Establish the independent count.** Read the issue object's own
   comment count — `GET /repos/flowbyte-com/mpm/issues/238`, field
   `comments`. This is a **different endpoint** from the listing, and it
   is what makes the count a reconciliation rather than a self-report.
2. **Traverse the listing to a confirmed terminal state.** Fetch
   `GET /repos/flowbyte-com/mpm/issues/238/comments?per_page=P&page=k`,
   following the `Link` header's `rel="next"` cursor until the response
   carries **no** `rel="next"`. A response with no next cursor is the
   terminal state; reaching it is what "exhaustive" means here. Choose
   `P` small enough that the listing spans several pages, so the
   server's own `rel="last"` cursor exists to be read.
3. **Reconcile the count, from two directions.** The number of items
   retrieved must equal **both**:
   - the total implied by the server's own page cursor —
     `(L − 1) × P + items_on_page_L`, where `L` is the `rel="last"` page
     number from the **first** response's `Link` header; and
   - the independent count from step 1.

   A short final page breaks both at once. A count that reconciles with
   only one of them is not reconciled.
4. **Collect the stable identifiers.** Each item's numeric comment id is
   its stable identifier. Collect them all, and check for **duplicates**:
   an id appearing twice means the traversal repeated or overlapped, and
   the enumeration is rejected.
5. **Enumerate twice, and require the two to agree exactly.** Repeat the
   whole traversal. The two id sets must be identical, and so must the
   hash of every retrieved body. A set that **changed between the two
   traversals** — a decision published, edited or withdrawn in between —
   is not a complete enumeration of a stable set: **re-run the complete
   verified traversal**, and accept it only when two consecutive complete
   traversals agree.
6. **Retrieve and classify, last.** Only then is each retrieved body
   classified: a body carrying the attestation markers is parsed and
   classified as an approval or a revocation; a body carrying **no**
   markers is simply **not a decision** — it is a review report or an
   operator note — and is skipped without comment. Completeness is a
   claim about **retrieval**, not about interpretation, and a body that
   **does** carry the markers but cannot be parsed is a **failure**, not
   a skipped row.

**The exact fail-closed outcome.** If any of the following holds, the
enumeration is **incomplete**, the published decision set is **not
established**, and every scope it concerns has **no approval** — the
check stops, and nothing is executed:

- the traversal did not reach a confirmed terminal state;
- a page was inaccessible, a request failed, or the pagination was
  interrupted;
- the retrieved count does not reconcile with **either** independent
  count;
- any identifier appeared twice;
- the two consecutive traversals disagree on the id set or on any body
  hash;
- the set changed while it was being enumerated, and a repeat traversal
  did not produce two agreeing complete traversals;
- a retrieved body that carries the attestation markers cannot be
  parsed, or names a scope that is not one of the three canonical
  identifiers.

**The same principle governs revocations.** Revocations are discovered by
the same enumeration of the same set, classified by the same markers, and
subject to the same completeness requirement. There is no separate,
weaker discovery path for them, and a revocation is not exempt from the
count reconciliation.

**What the channel's guarantees do not cover.** This is stated because
the enumeration cannot establish it, and a specification that implies
otherwise is worse than one that does not:

- **A current listing is not an immutable historical audit log.** A
  publication that is **edited** at the channel is detectable — the
  retrieved bytes stop matching the local record — but a publication
  that is **deleted** leaves no trace in the listing: the count falls
  with it, the reconciliation still balances, and the deletion is
  invisible to any enumeration. An audit log has to be maintained out of
  band.
- **A channel that truncates every view consistently cannot be detected
  by any client-side procedure.** If the listing is silently short *and*
  the counts it and the independent endpoint report are reduced to match,
  then every reconciliation balances and the truncation is invisible. The
  mitigations are the mirror rule below and an out-of-band record; the
  guarantee itself cannot be claimed.
- **The two counts are only as independent as the API makes them.** Both
  are served by the same host from the same database. They are two
  **views**, not two **systems**, and they do not substitute for an
  archival copy of the publication set.

##### Versions, supersession and revocation

**The filename carries the version.** A fixed filename cannot be
write-once and retain the earlier record at the same time: reusing the
name overwrites, so the history is lost. With
`…approved.<scope>.v<N>.json` the history is retained by construction,
and the version history *is* the audit trail. The version is bound to
the body, as above, so a version cannot be moved by renaming a file.

**A revocation is a published record, retrieved exactly like an
approval.** It is not a local file and not an edit to the record it
revokes. Its canonical body carries the same markers and the same
shape as an approval's, and it binds:

| | |
|---|---|
| the canonical scope | the scope whose approval is revoked |
| the applicable procedure checksum | `artifact_sha256`, so a revocation cannot be carried across a procedure revision |
| the target approval's identity and version | `target_publication` and `revoked_version` |
| the authoritative publication source | its own `publication` object — where the revocation itself lives, and how it is retrieved |
| the revocation decision | `decision: "REVOKE"`, with the reason |
| the authority for the revocation | the reviewer or operator authority, carried in the authority register, exactly as for an approval |

**Discovery is remote, and the local directory is not the discovery
mechanism.** The operator enumerates the channel for revocation records
covering each scope, and retrieves each one. **The absence of a local
`.revoked.json` is not evidence that no revocation exists** — a
revocation published but not mirrored locally is still in force, and a
local revocation file that nobody published is not. **A locally
fabricated revocation does not count; a locally deleted revocation does
not un-revoke anything.**

**Completeness and failure behaviour are defined, and they fail closed.**
The completeness requirement — what "exhaustive" means, how it is
demonstrated, and what is provably outside it — is set out in **"The
published decision set"** above, and it applies to revocations exactly as
it applies to approvals. In summary:

- the enumeration must be **exhaustive** — every page retrieved. A
  pagination failure, a truncated listing or an inaccessible channel is
  a **failure, not an empty result**, and the check stops;
- a revocation whose publication cannot be retrieved, whose author
  cannot be read, or whose authority the register does not list
  **fails closed** — it is not ignored;
- an **ambiguous** revocation — two records for the same version with
  conflicting decisions, or one whose target cannot be resolved — fails
  closed;
- a revocation naming a version that is not the current one does not
  affect the current one; and a revocation **removes** a version, it
  never creates an approval of any other version.

**Selecting the current version, per scope — from the published set,
never from the filenames.** This is the rule the local directory must
not be allowed to decide:

1. enumerate the published decision set for the scope **completely**, as
   above — the same enumeration that discovers revocations;
2. among the published **approval** decisions for the scope, let **N** be
   the **highest** `attestation_version`. **Local filenames do not
   participate in this choice at all**: the directory may hold a higher
   version, a lower one, or neither, and none of that changes which
   published decision is current;
3. **the local record for version N must exist**, and must be the
   canonical record for that decision — same scope, same version, same
   `publication.id`, matching sidecar, canonical bytes — and that
   publication must be listed in the authority register. **If the local
   record for N is missing, the scope has no approval.** There is **no
   fallback** to an older local record;
4. version N must not be **revoked** by a revocation in the published
   set, and it must not be **withdrawn** — a decision whose verdict is
   not `APPROVE` is a decision against, and it leaves the scope
   unapproved;
5. version N must pass every publication check above — retrieved from
   its channel, author equal to `reviewer`, scope, verdict,
   `artifact_sha256` and `pr_head` equal, body byte-identical, listed in
   the authority register.

**Why the candidate must come from the channel.** The rule this
replaces — *"the highest version present locally"* — is a downgrade, and
it is a downgrade that the rest of this mechanism cannot catch. A local
`v2` that says REQUEST CHANGES, deleted from the directory, leaves `v1`
as the highest version **present**; `v1` then passes every remaining
check, because its publication is retrievable and its bytes match, and
the scope is **approved** on a decision the reviewer has superseded.
The point of the no-fallback rule is that neither revocation nor
invalidity of the newest decision may hand over to an older one — and
that only holds if "the newest decision" is decided by the **published**
set. **Deleting or renaming a local file must not change what is
approved; it can only remove evidence, and removed evidence stops the
check rather than moving it.**

**Supersession is by version order, and a newer decision is never
ignored.** If a newer decision has been published for the scope, that
decision **is** the current decision — including when it is bound to an
obsolete PR head, or to a procedure checksum that is no longer the one
in force. It is not filtered out of the choice, and it does not hand
over to an older one. It is **inapplicable**, and an inapplicable
current decision leaves the scope **unapproved**. Two published
decisions for one scope at the same version number are **ambiguous** and
fail closed; so are two decisions that conflict.

**Lower published versions are superseded, not discarded.** They remain
in the published set and in the directory — the version history *is* the
audit trail — but they are not candidates, and they are never activated
by the failure of a higher one.

**The directory mirrors the published set.** Two rules, and both are
load-bearing:

- **every local record must correspond to a published decision** for the
  same scope and version. A local record with no published counterpart
  is a **fabrication**, and it fails closed;
- **the current published version must have a local counterpart** — the
  rule above, and the reason a deleted local record stops the check
  rather than moving it.

The operator therefore **materialises every published decision**, not only
the current one. That is what makes a truncated enumeration detectable in
the common case: a decision the enumeration failed to return is still in
the directory, the two disagree, and the check fails closed.

**A higher version number does not establish approval.** The highest
**published** number is only the *candidate*; the record at that version
must be **valid and authenticated**, or the scope has no approval. A
record whose publication cannot be retrieved, or fails any check, does
not hand over to an older one. **Revocation and invalidity of the highest
applicable version both leave the scope unapproved; neither activates
an older approval.**

**Fail closed on all of these:** no published decision for a scope; a
record whose `artifact_sha256` differs from the sidecar; an unknown
`verdict`; a revoked highest version with no superseding record; two
different records claiming the same version; a malformed version; a
filename/body version or scope mismatch; a publication that cannot be
retrieved, or whose author, verdict, scope or hashes disagree; a
publication the authority register does not list; an incomplete or
unreconciled channel enumeration; a local record with no published
counterpart; **the local record for the current published version
missing**; and any record whose scope is not one of the three canonical
identifiers.

**One consequence worth stating plainly.** Whoever can publish on the
channel can also revoke, and so can deny a valid approval by revoking
it. Revocation power and approval power are the same power here. That
is a property of a single shared account, not of this specification,
and it is why the authority register is the operator's own record.

##### The adversarial cases the check must reject

The check is not trustworthy because it reads well. It is trustworthy
because it has been **exercised against attempts to defeat it**, and
each of the following must be **rejected**:

1. a valid record **renamed from `v1` to `v2`** with the local sidecar
   recomputed;
2. the same rename applied to a **revoked** version — the revocation
   must still stand, and the scope must not acquire an approval;
3. a forged record at a higher version with no publication of its own;
4. a filename/body version mismatch, and a filename/body scope
   mismatch;
5. two records claiming the same version, however they are spelled;
6. a publication body whose canonical bytes were altered after
   publication;
7. a publisher that is not the record's `reviewer`;
8. a publication the authority register does not list;
9. a wrong procedure checksum, and a wrong PR head;
10. an **incomplete** channel enumeration, and an inaccessible one;
11. a revocation published but absent locally — it must still be
    honoured;
12. **a locally deleted higher-version approval** — the scope must stay
    **unapproved**, not fall back to the older local record, even though
    the older record passes every check it is given;
13. a **forged higher-version local record** with no published
    counterpart;
14. a **published decision with no local record** — the scope is
    unapproved;
15. a **newer published decision bound to an obsolete PR head** — it
    does not hand over to an older one;
16. a **short final page**, a count that reconciles with neither
    independent source, a **repeated identifier**, and a set that
    **changes between two traversals**.

The last one is the counterpart of the second: neither a locally
fabricated revocation nor a locally deleted one may silently establish
a different approval state.

##### The operator's check

```bash
cd /home/v/src
# 1. the procedure is the approved one
sha256sum -c MPM_CUTOVER_4a870c8a_to_2045afcc.md.sha256

# 2. the published decision set, enumerated completely and reconciled
#    (see "The published decision set" above for what each step proves)
REPO=flowbyte-com/mpm; ISSUE=238; PER_PAGE=5
WORK="$HOME/.cache/mpm-attestation"; mkdir -p "$WORK"; chmod 700 "$WORK"

# 2a. the independent count, from a different endpoint
gh api "repos/$REPO/issues/$ISSUE" --jq '.comments'

# 2b. the traversal, to a confirmed terminal state.
#     The body is base64-encoded so that one comment is one line, whatever
#     it contains.
gh api --paginate "repos/$REPO/issues/$ISSUE/comments?per_page=$PER_PAGE" \
  --jq '.[] | "\(.id) \(.body | @base64)"' | tee "$WORK/set-1.txt" | wc -l

# 2c. no repeated identifier
cut -d' ' -f1 "$WORK/set-1.txt" | sort | uniq -d

# 2d. the server's own page cursor — the count this enumeration must match
gh api -i "repos/$REPO/issues/$ISSUE/comments?per_page=$PER_PAGE" 2>&1 \
  | grep -i '^link:'

# 2e. enumerate a second time; the two must agree exactly
gh api --paginate "repos/$REPO/issues/$ISSUE/comments?per_page=$PER_PAGE" \
  --jq '.[] | "\(.id) \(.body | @base64)"' > "$WORK/set-2.txt"
cmp -s "$WORK/set-1.txt" "$WORK/set-2.txt" \
  && echo "the two traversals agree exactly" \
  || { echo "STOP: the publication set changed between traversals" >&2; exit 1; }

# 3. per scope: the records present, and their own checksums
for scope in database-safety managed-block-stage-4 whole-procedure; do
  ls -1 MPM_CUTOVER_4a870c8a_to_2045afcc.approved."$scope".v*.json
  sha256sum -c MPM_CUTOVER_4a870c8a_to_2045afcc.approved."$scope".v*.json.sha256
done
```

and then, for each scope, the checks of the publication above —
**retrieved, not asserted**, and applied to the **highest published**
version, not to the highest version that happens to be in the directory.
**Every one of the three scopes must produce exactly one current, valid,
authenticated record.** If any scope does not, the answer is to obtain
the missing approval, not to proceed — and **never** to fall back to an
older record.

#### 3.8.2 Two things this cannot paper over

**The procedure's status line still reads NOT APPROVED, and no edit can
change that without voiding the approval.** That is not a gap in the
attestation; it is the consequence of the circularity above. The status
line is the file's statement that it carries **no internal approval** —
which is true, and is exactly the state an unreviewed revision is in.
The authoritative approval state is the attestation. **An operator who
reads only the file will read NOT APPROVED, and that is not a formality
to be explained away: the attestation has to exist, has to bind this
exact checksum, and has to carry the required scopes.** The guide says
so here because the procedure's own text cannot.

**A scoped approval is not a cutover approval.** The three scopes are
decided separately, and each decision says only what its own scope
covers. An approval of `database-safety` says the restoration path is
sound; it says nothing about the promotion path, the service gate or
the managed block. **No scope's approval is permission to run the
cutover** — the cutover needs **all three** scopes, recorded as three
separate decisions against the same procedure checksum, and then the
operator's own authorisation on top (§3.2 P7). P7 requires the scope set
to be **exactly** `{database-safety, managed-block-stage-4,
whole-procedure}`, so a partial set cannot satisfy it by accident.

## 4. Database safety

> This section is about the **data**, not the binaries. The promotion in §3
> never replaces the live database — it takes a copy of it. A **restore**
> does. That difference is the whole of the database-safety story, and it is
> why the promotion is a normal operation while a restore is not.

### 4.1 What the schema fingerprint establishes — and what it does not

`SchemaFingerprint` hashes the database's **structure**: the `sqlite_master`
entries (each table's name and its `CREATE` statement), every table's
`PRAGMA table_info` columns, that table's **CHECK expressions**, and its
**index and trigger names**. It reads **no rows**.

An index's or a trigger's *name* is therefore part of the fingerprint:
adding or renaming one moves it. That is still structure, never data —
but it means the fingerprint moves for more reasons than "a table
changed shape", and a reader comparing two fingerprints should know it.

So:

| | |
|---|---|
| a matching fingerprint establishes | **compatibility** — the candidate's runtime can read this shape |
| a matching fingerprint does **not** establish | **freshness** — that the copy holds the data you expect |

The verified example on this installation. The live database and the
**November 7** recovery point have the **identical** fingerprint:

```
sha256:c85972e86e05f63623284ad9076b630e34414c0c4aa1e780fedb8f68cd305f5b
```

while their contents are materially different (measured 2026-11-11 with the
updater's own `SchemaFingerprint` and read-only SQLite opens; the live counts
grow every time the installation is used):

| table | live | Nov-7 point | delta |
|---|---|---|---|
| `memories` | 2,405 | 320 | **−2,085** |
| `evidence` | 2,333 | 252 | −2,081 |
| `tool_invocations` | 51,185 | 37,822 | −13,363 |
| `works` | 26 | 10 | −16 |
| `session_handoffs` | 205 | 106 | −99 |
| `lessons` | 39 | 39 | 0 |

**A matching fingerprint is necessary for compatibility and is not evidence of
data preservation.** `PRAGMA integrity_check` returning `ok` is equally
insufficient: restoring the Nov-7 point over the live database leaves the
integrity check green and the data three days old. **Structural integrity is
not proof that the data is there.**

### 4.2 Verifying the recovery point the promotion creates

§3.4 describes **where** the promotion's backup is created. This is how to
verify it **afterwards**, independently of the updater's own verification.

**Two different things are being verified, and they are not the same.**

| | built-in verification (the updater's) | the operator's freshness assessment |
|---|---|---|
| when | at creation, inside the promotion | after the promotion, by the operator |
| what | the copy is **structurally sound**: non-empty, a `SQLite format 3` header, `PRAGMA integrity_check` = `ok`, every table readable, and the backup's **table set equals the live table set** | the copy holds the data you expect: **row counts** measured against a pre-promotion baseline (§4.1 — the fingerprint cannot see them) |
| why not more | `VACUUM INTO` captures an instant *during* the backup, so comparing its counts against a *later* live read compares two different points in time: a legitimate concurrent write produces different counts and a false failure. The updater therefore verifies the copy **against itself**, not against a moving target | only the row counts measure freshness, and only the operator can say which tables matter |

**Step 1 — take a baseline, before the promotion.** Capture the counts you
will compare against, with a timestamp, in a file you keep. Read-only opens
only (§4.7):

```bash
BASELINE="$HOME/.cache/mpm-recovery/baseline-$(date -u +%Y%m%dT%H%M%SZ).txt"
mkdir -p "$(dirname "$BASELINE")"; chmod 700 "$(dirname "$BASELINE")"

python3 - "$HOME/.mpm/src/db/mpm.db" > "$BASELINE" <<'PY'
import sqlite3, sys, datetime
db = sqlite3.connect(f"file:{sys.argv[1]}?mode=ro", uri=True)
print("taken_at_utc:", datetime.datetime.now(datetime.UTC).isoformat())
for t in ("memories", "evidence", "tool_invocations", "works",
          "session_handoffs", "lessons", "schema_migrations"):
    try:
        print(f"{t}: {db.execute(f'SELECT COUNT(*) FROM {t}').fetchone()[0]}")
    except Exception as exc:
        print(f"{t}: ({exc})")
PY
chmod 600 "$BASELINE"
cat "$BASELINE"
```

**Step 2 — identify the recovery point the successful promotion recorded.**
Match the backup directory to the **successful** history entry, not by name
and not by date:

```bash
tail -1 ~/.mpm/update-history.jsonl | python3 -m json.tool
```

The entry must say `outcome: success`, name `to_rev: 2045afcc…`, and carry a
`backup_dir`. That directory is the recovery point this promotion created.
Also read `~/.mpm/.last-update-backup`, which the promotion wrote to the same
value — it is a convenience pointer, not the authority.

**Step 3 — verify that recovery point's integrity and its recorded
fingerprint.** The promotion recorded the pre-update fingerprint in the same
history entry (`pre_fp`), and the backup's own copy must carry it:

```bash
BK=<the backup_dir the successful record names>

python3 - "$BK/mpm.db" <<'PY'
import sqlite3, sys
db = sqlite3.connect(f"file:{sys.argv[1]}?mode=ro", uri=True)
print("integrity:", db.execute("PRAGMA integrity_check").fetchone()[0])
print("tables:", db.execute(
    "SELECT COUNT(*) FROM sqlite_master WHERE type='table' "
    "AND name NOT LIKE 'sqlite_%'").fetchone()[0])
for t in ("memories", "evidence", "tool_invocations", "works",
          "session_handoffs", "lessons", "schema_migrations"):
    try:
        print(f"{t}: {db.execute(f'SELECT COUNT(*) FROM {t}').fetchone()[0]}")
    except Exception as exc:
        print(f"{t}: ({exc})")
PY
```

Compare the shape with the recorded `pre_fp`. **You cannot recompute the
fingerprint with the shipped tooling** — no CLI surface prints one, and the
algorithm's details (the CHECK expressions, the index and trigger names)
are Go-side, so a hand reimplementation would not be byte-identical. Use
the **executable proxies** this step already prints — the table count, the
table set, and `schema_migrations` equality — as the shape check, and let
the updater enforce the fingerprint binding at restore time: it refuses a
recovery point whose copy does not carry the recorded `pre_fp`.

**Step 4 — compare the counts with the baseline, and investigate.**
`schema_migrations` must be **equal**. The row counts must be **equal or
lower** than the baseline, because the backup was taken *before* the
promotion:

- a count **lower** than the baseline is the normal case — the backup
  predates writes that the live database has since received;
- a count **higher** than the baseline is possible (the backup is a snapshot
  taken during concurrent writes) and is not automatically an error;
- a count that is **materially lower than expected**, or a table that
  **vanished**, is a **regression to investigate** — it is the signature of a
  recovery point that does not represent the live data. Do not accept it
  because the integrity check is green.

**Do not impose unconditional per-table non-decreasing counts.** The
scheduler writes to this database every 60 seconds, and agent sessions write
whenever they work, so any table can legitimately move between the baseline
and the comparison. A rule like *"every table's count must be ≥ the
baseline's"* is not a safety property here — it is an assertion that
happened to hold at one moment. The property that matters is **"no table
lost rows it should not have"**, and that is a judgement the operator makes
from the numbers, with the delta in front of them.

**Record the result.** The measured counts, the delta and the timestamp are
what the operator will need if a restore is ever proposed.

### 4.3 A database restore is destructive

`mpm-update --rollback --with-db-restore --force` **overwrites the live
database with a copy taken at an earlier moment.** Everything written since
that moment is gone. The `--force` is what makes it possible, and its own
message says what it means: *"--force confirms overwriting newer data"*.

**The updater selects the recovery point automatically, and it will select
the wrong one if you do not know the rule.** The selection is: the **last**
history entry with `action: update`, whose `to_rev` is the revision being
rolled back **from**, and which carries a `backup_dir`. On this installation
that means:

| when | the active revision is | `--with-db-restore` selects |
|---|---|---|
| **before** the promotion | `4a870c8a` | the **November 7** point — 2,085 memories older than live |
| **after** the promotion | `2045afcc` | the point **this promotion created** (§4.2) |

So the foot-gun is real: run the restore **before** the promotion and the
updater silently selects the November 7 point, because that is the only
`backup_dir` recorded for `4a870c8a`. **This is why the November 7 point must
not be the routine recovery point for this cutover** — it is the point that
corresponds to the *defective* promotion, and it is the one an
unattended `--with-db-restore --force` would pick.

**A restore requires three things, in this order, and the third is the
operator's, not the tool's.**

1. **Identify the point the command will use — explicitly.** Do not infer it.
   Read it out of the history record that matches the active revision
   (§4.2, Step 2), and write the absolute path down:

   ```bash
   readlink ~/.mpm/current
   tail -1 ~/.mpm/update-history.jsonl | python3 -m json.tool
   ```

   The `backup_dir` in that record is the one the restore will use. If the
   path is not the one you expected, **stop** — you are about to restore the
   wrong point.

2. **Measure the data delta.** Compare the point's counts with the live
   database's, with the same script and the same read-only opens. State the
   delta explicitly, in writing, before the next step — *"restoring this
   point discards N memories, M evidence rows, K tool_invocations written
   since <timestamp>"*. §4.2's Step 3 script, pointed at both files, does
   this.

3. **Obtain separate, explicit operator approval** for the destructive step,
   with the delta in front of the approver. The approval to *promote* is not
   the approval to *destroy data*; they are different acts with different
   consequences, and this document grants the first, not the second.

   **Bind the approval to the measurement.** The approver must state the
   figure they are accepting — *"I approve discarding 2,085 memories, 2,081
   evidence rows and 13,363 tool_invocations from the November 7 point"* —
   so the approval cannot be given without the number, and the number
   cannot change afterwards without a new approval. The host-specific
   procedure makes this mechanical: its `authorize_restore` requires
   `RESTORE <rows_would_be_lost>` and refuses a bare `RESTORE` (§3.8).

**The restore also has to survive the writer rules** (§4.4), and it changes
the live database's journal mode (§4.5). Both are reasons to treat it as a
separate, deliberate operation — not a recovery step to reach for because a
promotion failed.

### 4.4 Writers: the scan, its limits, and what quiescence means

The updater scans for processes holding the live database open
(`internal/update/processes.go`, `ScanDBHolders`). It is worth knowing
exactly what that scan does, because it is easy to over-trust.

**What it does.** It walks `/proc`, reads each process's open file
descriptors, and classifies every process that could hold the database:

| class | meaning | effect |
|---|---|---|
| **holder** | the process demonstrably holds `mpm.db`, `mpm.db-wal` or `mpm.db-shm` open | the restore is **refused** |
| **unverifiable** | the process's `/proc/<pid>/fd` could not be read and no lock on the database was found — a **same-uid** process (a hardened agent, a keyring daemon) or a process owned by another user | the restore is **refused unless `--force`**; with `--force` they are **accepted and recorded**, never silently ignored |
| ruled out | the process was positively excluded | nothing |

**What it is not.** It is a **point-in-time `/proc` snapshot**. The code says
so itself: *"a one-time /proc scan is not an atomic all-writers guarantee"*,
and the check is deliberately run **twice** — once before the copy is staged
and once immediately before the `rename(2)` — which *"narrows the
check-then-act window to the rename syscall itself. It cannot close the
window."* A process that opens the database after the last scan is not seen.

**`--force` cannot bypass a definite holder.** Measured against the live
paths with the updater's own scanner:

```
definite holders : 1
   {PID:2977 Name:mpm-scheduler File:/home/v/.mpm/src/db/mpm.db}
unverifiable     : 30   (processes whose /proc/<pid>/fd could not be read)
checkWriters(force=true)  -> refusing to restore the database: 1 process(es) still hold it open:
  pid 2977 mpm-scheduler (/home/v/.mpm/src/db/mpm.db)
Stop the writers, then retry.
--force confirms overwriting newer data; it does not bypass this check.
```

The refusal is identical with and without `--force`. `--force` only relaxes
the **unverifiable** residual — and it records the processes it accepted.

**Who the writers are, on this installation.**

| writer | what it is | can the updater stop it? |
|---|---|---|
| `mpm-scheduler` (the managed unit) | the scheduler, running on a 60-second interval | **yes** — with `--restart-services`, or by hand. It is the definite holder |
| **OpenCode agent sessions** | every session running against this installation | **no.** The updater reports them as *"agent session — restart by hand; the updater will never touch it"*, and never terminates them. A restore under an open session is a restore **under a writer** |
| **MCP servers / other processes** holding the database | anything else that opened it | **no** — they show up as holders and the restore is refused |
| **same-uid processes with unreadable descriptors** | **2** on this host: a `sd-pam` session daemon (pid 2767) and an `ssh-agent -D` (pid 3203) | **no** — and these are the entries to look at **first**: they are the operator's own processes, they are named, and they are the only ones in the class that *could* hold an operator-owned database. A root-owned service normally cannot open it at all — the database is mode `0600` and owned by the operator |
| processes owned by other users | the rest of the unverifiable list | **no** — and no lock on the database was found for any of them |

The count of unverifiable processes is a **point-in-time snapshot of a
moving host population**, not a property of the installation: the scan
reported **30** at the time of measurement (2026-11-11), **42** earlier
the same day and **30** the day before. What is stable is the *shape* of
the class, not its size.

**An ordinary promotion needs none of this.** The promotion takes an **online**
backup (`VACUUM INTO`, a transactionally consistent copy) and **never replaces
the live database**. It does not stop the scheduler, does not stop agent
sessions, and does not need quiescence. The writer scan exists for the
**restore** path, which is the only thing that replaces the live file.

**Quiescence, stated plainly:** before a restore, the scheduler must be
stopped **and** the operator's agent sessions must be closed by their owners.
The updater will never stop the second — it reports agent sessions as
*"restart by hand; the updater will never touch it"* and never terminates them.

**On the destructive-restore path, the updater stops the first too.** The
host procedure's **Policy C1** governs that path, and it is a different act
with a different authorisation:

- the restore is invoked **without** `--restart-services`, and the updater
  then **starts no managed service on any path** — success, restore failure,
  repoint failure, health-check failure, or a restore refused because a
  writer appeared;
- the managed units are stopped **by hand**, under a **separate
  authorisation**, inside a recorded **maintenance window**, and the units
  that were active are recorded so that exactly those are started again;
- the updater's own refusal — *"managed service(s) still running … stop them
  first"* — is the backstop if the manual stop did not take effect. It is an
  error, not a warning;
- **no scheduler may be started until the restored database has been
  verified** (§4.6, §4.5). If a verification fails, **nothing is started**:
  the pointer has moved, the database has been replaced but is not trusted,
  and the units are stopped.

**Why the restore must not carry `--restart-services`.** With the flag, the
updater stops the unit, restores the database, swaps the pointer, runs its
health check and then **starts the unit itself** — inside the rollback,
before the command returns, and on a database whose journal mode no gate has
yet verified. Without the flag the updater refuses the restore if a managed
unit is active, and otherwise starts nothing. So the safe path is the
no-restart path, and C1 exists to authorise the manual stop it needs.

### 4.5 The journal mode after a restore

Three things happen, and they are easy to conflate: **the state of the
restored file**, **what the runtime does about it**, and **what nobody
checks**.

**1 — The restored file's initial state is the rollback-journal mode.**
The recovery point is produced by `VACUUM INTO`, which writes a database
in the **DELETE** (rollback-journal) mode. The live database is in
**WAL** mode. `restoreDatabase` copies the recovery point over the live
file and then removes the live `-wal` and `-shm` sidecars — which is
exactly what leaving WAL mode requires. The updater **never opens the
live database for write** (every one of its opens is read-only) and
**never sets `journal_mode`**, so the file the restore leaves on disk
is in rollback-journal mode, and it stays that way until something
opens it.

Measured on a private copy (2026-11-11), reproducing what the restore
does:

```
before: journal_mode = wal        live header bytes 18/19: 2,2
after : journal_mode = delete     Nov-7 point header bytes 18/19: 1,1
memories after the restore: 320    (live had 2,405)
integrity: ok
```

**2 — MPM re-establishes WAL by itself, on its next connection.** Every
production open path in `internal/core/db.go` runs
`db.Exec("PRAGMA journal_mode = WAL")`, so the first MPM connection
after the restore normally converts the file back to WAL. Measured on
a private copy: the conversion took the header bytes from `1,1` to
`2,2` and `PRAGMA journal_mode` from `delete` to `wal`.

**So a restore does not, as a rule, leave the installation running
without WAL.** The window without WAL is the time between the restore
and the next MPM connection — and in that window a reader blocks a
writer.

**3 — And that is the whole of the residual, because the attempt is
unchecked.** The open path **discards the error** from that `db.Exec`.
A conversion that cannot be performed fails — measured on a private
copy: `database is locked`, when another connection holds the database
— and leaves the file in rollback-journal mode **without anything
reporting it**. That is the failure worth guarding against: not a
permanent downgrade, but a **silent** one, in an installation whose
concurrency assumptions (WAL plus `busy_timeout`) no longer hold.

**Nothing in the upstream procedure says any of this.** It must be said
here.

**If a restore is ever authorised, the journal mode is a separate,
validated step — not an automatic consequence, and not a gate on the
restore.** It is a **verification with a recovery path**:

1. **decide explicitly** whether the installation should run in WAL
   mode, and record the decision. WAL is what the live installation
   uses and what `internal/core/db.go` sets on open;
2. **restore first**;
3. **verify the mode**: the header bytes 18/19 must read `2,2` **and**
   `PRAGMA journal_mode` must report `wal` **and** `PRAGMA
   integrity_check` must still return `ok`. Read the pragma's
   **result** — it is the mode actually in effect, not the mode that
   was requested;
4. **if it is not WAL, re-establish it explicitly**: run
   `PRAGMA journal_mode = WAL` against the restored database **and
   check the error**, then verify again;
5. **if that fails, STOP.** The database stays in rollback-journal
   mode, the failure is recorded, and the operator escalates. **Do not
   start the scheduler on a database whose journal mode is unknown**;
6. **verify the data** against the counts measured in §4.3, step 2;
7. only then restart the scheduler and re-open the agent sessions.

**Do not change the live database's journal settings during a review, a
rehearsal or a dry run.** The mode is a property of the running
installation, not an inspection parameter.

### 4.6 What the restored database must reproduce

A destructive restore replaces the live database. What has to be true
afterwards is not *"the row counts match"* — it is **"the restored
database is the recovery point, record for record"**, plus a small,
explicitly enumerated set of additions the system itself creates.

**Why a count comparison is not enough.** The updater's own health check
runs **inside** the rollback — `restoreDatabase` → `restorePointer` →
`SmokeLive` — and it writes an audit row. A gate that demands equal row
counts therefore **rejects a perfectly good restore**: the database is
exactly the recovery point's, plus one legitimate, system-generated audit
record. This is the defect the host procedure's N4 finding was, and it is
fixed there; §4.6 says what the fixed comparison means, because an
operator has to be able to read the result.

**What the comparison is now: primary-key-aware and content-exact.**

| # | check | what it rejects |
|---|---|---|
| 1 | the **table sets** must be identical | any drift, either way — fail-closed |
| 2 | **every** record of the recovery point must be present in the restored database **and unmodified**: the primary key present *and* every column identical | a **deletion** (reported as missing) and a **modification** (reported as differing). A count comparison could see neither |
| 3 | every record the restored database holds and the recovery point does not must be an **explicitly allowed** system-generated effect | any unexplained addition |
| 4 | the per-table counts must still agree with the recorded estimate | a restore that no longer matches what was approved |
| 5 | `PRAGMA integrity_check` must report `ok` | a corrupt database |
| 6 | the recovery point's **SHA-256** must still be the one the estimate recorded | a recovery point that changed after the measurement |

**The one permitted addition — the health-check audit entry.** One row, in
one table:

```
tool_invocations:
    tool_name    = 'mpm_system'
    action       = 'health_check'
    payload_hash = 'sha256:3e01bce17a689f77cb7e744fea6253ace3392bed1bdd5a3224a6a0053e9ccfcd'
    result_status= 'success'
```

The `payload_hash` is the SHA-256 of the exact payload the updater sends
(`{"action":"health_check"}`). It is a property of the **payload**, not of
the build — measured identical on `2045afcc` and on `4a870c8a` — so a row
carrying it *is* a health-check row of this shape and of no other.
**At most one** such row is accepted: one health-check dispatch is one
audit row.

**Zero such rows is equally acceptable.** The audit hook is best-effort by
design — *"an audit failure (table missing, DB locked) must NEVER block the
underlying tool call"* — so a health check whose audit insert failed leaves
no trace at all. The gate accepts both the one-row and the no-row case.

**The second, narrower allowance.** Opening the database can make the
router log an informational note into `system_audit_log` when the mode or
persona defaults are absent from disk. Those notes are accepted only when
they are router-level (`level='info'`, `component='router'`) and match one
of the message shapes the router can emit, and **at most two** of them.
On this host they never occur: `~/.mpm/mode/default.md` and
`~/.mpm/persona/default.md` both exist, so the measured delta is exactly
the one audit row. **Any other `system_audit_log` record is unexplained and
rejected.**

**The allow-list is never widened.** An unexpected record is a stop, not
an invitation to widen the predicate — and the verification reports what it
accepted, so the operator can see the accepted row in the output.

**What the comparison does not prove.** It proves the restored database
*is* the recovery point, record for record, plus the measured
system-generated addition. It does **not** prove that nothing else wrote to
the database between the restore and the verification: the measurement bounds
what is **accepted**, it cannot prove the absence of a writer. That window
is closed by **Policy C1** — the managed units are stopped and the only
writer in the window is the updater's own health check (§4.4).

**If the verification fails, nothing starts.** The host is left in a visible,
fail-closed state: the pointer moved, the database was replaced but is not
trusted, the units are stopped. The operator escalates — there is no
automatic start after a failed database verification, not by the updater
(it starts nothing) and not by the procedure (the start step is never
reached).

**The three approved gates, and what they are.** The independent
database-safety review of the host procedure examined four behaviours and
approved three of them unchanged and one after correction. They are named
here so that the guide and the procedure use the same names:

| | behaviour | where it lives |
|---|---|---|
| **N1** | **the destructive restore starts nothing** — no managed service is started on any path, so no scheduler can run on an unverified database | the procedure's §10.5.5, and §4.4 |
| **N2** | **incompatible table sets fail safely** — the measurement is *refused*, no estimate is written, and therefore no restore approval can be bound to it, so no destructive restore may run | the procedure's `measure_restore_impact`; the guide's §4.3 step 2 |
| **N3** | **the integrity check is executed and stops recovery** — the header bytes, `PRAGMA journal_mode` and `PRAGMA integrity_check` must all pass | the procedure's `verify_wal_state`; the guide's §4.5 |
| **N4** | **the restored database is verified row by row**, with the one health-check audit addition narrowly permitted | the procedure's §10.5.6; this section |

### 4.7 Inspecting a recovery point safely

A copy of the MPM database is **the operator's complete memory corpus**. It is
secret material, and the rules for handling it are not optional.

**Inspect read-only.** Every open of a recovery point, a backup or the live
database in this procedure uses a URI with `mode=ro`:

```python
sqlite3.connect(f"file:{path}?mode=ro", uri=True)
```

A plain `sqlite3.connect(path)` takes a **write lock** on the database. On a
recovery point that happens to be harmless; pointed at the **live** database,
it takes a write lock on a database the scheduler is actively writing to.
The two scripts in §6.1 must use the same read-only form.

**Keep copies in owner-private directories.** Not in `/tmp`: it is sticky but
world-traversable, and a default `umask` leaves files mode `0644`. Use a
directory under `$HOME` that only the operator can enter, with files only
the operator can read:

```bash
mkdir -p "$HOME/.cache/mpm-recovery" && chmod 700 "$HOME/.cache/mpm-recovery"
install -m 0600 /path/to/copy.db "$HOME/.cache/mpm-recovery/copy.db"
```

Remove the copy when it is no longer needed. A copy that is still there is a
copy that can still leak.

**Two hygiene items to record, not to act on.**

- **A rehearsal copy of the database in a shared temporary directory.** A
  previous rehearsal left a copy of the live database under `/tmp` — a
  world-traversable path. It has since been **removed from that path**, and
  the audit finds **no copy of the MPM database anywhere under `/tmp`** now.
  It is recorded here because the lesson is not the file: **a copy of the
  corpus in shared temporary space is a leak**, whatever later removes it.
- **An off-host copy of the whole installation.**
  `/home/v/pCloudDrive/pCloud Backup/v/.mpm/` is a **complete copy of the
  installation** (650 MB), including `src/db/mpm.db`, and it syncs to
  pCloud. It is **stale** — it holds the **November 1** installation (174
  memories, 18 schema migrations, fingerprint `6fa0883c…`), so it is not a
  copy of the current corpus, but it *is* a copy of an earlier complete
  corpus, and its directories are world-readable on this host.
  **This is recorded for the operator to verify, and nothing about it was
  changed.** The question is not a technical one: *is putting the operator's
  complete memory corpus off the host — and readable by other local users —
  intended?* If it is not, it is a separate change, made deliberately.

## 5. The instruction migration policy

**The updater does not rewrite the operator's instructions, and neither
does this procedure.** The managed block is refreshed by a separate,
explicit step, and the step is governed by an existing policy.

### 5.1 The policy

`agent_installation/mpm-opencode/scripts/install_agents_instructions.py`
implements it:

| rule | behaviour |
|---|---|
| only the marked span is rewritten | the region between `<!-- BEGIN MPM-MANAGED SECTION:opencode-instructions -->` and `<!-- END MPM-MANAGED SECTION:opencode-instructions -->` is replaced; everything outside is preserved, **apart from one newline appended immediately after the outer END marker** — no operator text is affected |
| a backup is written first | `<target>.bak`, `<target>.bak.1`, … next to the target |
| a BEGIN without a matching END | **refuses to modify** and exits 2 — the conservative case |
| no managed block at all | appends the section, after a backup |
| already current | a no-op; the provenance timestamp is preserved |

So the operator's own text outside the markers is never overwritten,
and the refresh is reversible from the `.bak` file — **but that
reversibility is about the refresh, not about a runtime rollback.**
After a runtime rollback the `.bak` is the **wrong** target for the
block; §3.7 says why.

The refresh is also a **correctness** fix, not a cosmetic one: the
installed block names the OpenCode **V1** hook
`experimental.chat.system.transform`, which the running V2 plugin does
not register. The refreshed block names `ctx.session.hook("context")`.

### 5.2 The step

Run it **after Step 6 has passed** and **after the operator has decided
to keep the new release**, from the installed assets. The paths below do
not exist before the promotion: the active release carries only the
runtime payload until the repair adds the assets.

**The keep decision comes first, and it is the operator's.** The
managed block's provenance line names the revision the block was
rendered from. A block that names a revision the operator has rolled
back from is a false claim — and there is no way to re-stamp it, because
the rolled-back release carries no refresher at all (§3.7). The order is
therefore **runtime verification → keep decision → refresh**. If the
operator is not yet sure they are keeping `2045afcc`, **do not
refresh**.

**The command must export the provenance**, or it will stamp
`commit=unknown` and fail its own acceptance test below.
`block_provenance` reads `MPM_BUILD_VERSION` and `MPM_BUILD_COMMIT`
from the environment; `install.sh` and `make refresh-installed` export
both, but a standalone run of the refresher exports nothing.

**Source both values from the promoted release's own declaration — do
not type them.** `RELEASE.json` is written into the release at build
time and is the authority on what was actually installed:

```bash
REL="$HOME/.mpm/current"
RELJSON="$REL/RELEASE.json"
[ -f "$RELJSON" ] || { echo "STOP: the active release declares no RELEASE.json" >&2; exit 1; }

MPM_BUILD_COMMIT=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["commit"])' "$RELJSON")
MPM_BUILD_VERSION=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["version"])' "$RELJSON")

# cross-check 1: the release directory IS the revision it declares
[ "$(basename "$(readlink -f "$REL")")" = "$MPM_BUILD_COMMIT" ] \
  || { echo "STOP: release dir != RELEASE.json commit" >&2; exit 1; }
# cross-check 2: the CLI declares the same revision and the same version
[ "$("$HOME/.mpm/bin/mpm" version --json | python3 -c 'import json,sys;print(json.load(sys.stdin)["commit"])')" = "$MPM_BUILD_COMMIT" ] \
  || { echo "STOP: CLI revision != RELEASE.json commit" >&2; exit 1; }
[ "$("$HOME/.mpm/bin/mpm" version --json | python3 -c 'import json,sys;print(json.load(sys.stdin)["version"])')" = "$MPM_BUILD_VERSION" ] \
  || { echo "STOP: CLI version != RELEASE.json version" >&2; exit 1; }

export MPM_BUILD_COMMIT MPM_BUILD_VERSION
```

**Do not type the version.** A hand-typed constant is a claim about a
release nobody checked, and the version is the one provenance field no
other step verifies. The two cross-checks above are what turn the
provenance from an assertion into a measurement: on a mismatch the
command **stops before it writes anything**.

Now run the refresher with the sourced values:

```bash
python3 "$HOME/.mpm/current/agent_installation/mpm-opencode/scripts/install_agents_instructions.py" \
  --scope user \
  --target ~/.config/opencode/AGENTS.md \
  --snippet "$HOME/.mpm/current/agent_installation/mpm-opencode/templates/AGENTS.md.snippet" \
  || { echo "STOP: the instruction refresh failed" >&2; exit 1; }
```

The refresher **exits 2 and refuses to modify** a target whose BEGIN
marker has no matching END, and exits 0 on a no-op. **Check the exit
status** — a refusal is not a success.

The reconciler (`reconcile_managed_blocks.py --only mpm-opencode
--home $HOME`) resolves the manifest's snippet path relative to the
adapter directory and is an equivalent route — **but it does not export
the provenance either**, so the two exports are still required.

Verify:

```bash
grep -n "mpm-provenance" ~/.config/opencode/AGENTS.md
grep -n "ctx.session.hook" ~/.config/opencode/AGENTS.md

if grep -q "experimental.chat.system.transform" ~/.config/opencode/AGENTS.md; then
  echo "STOP: the retired V1 hook name is still in the block" >&2; exit 1
fi
echo "the retired V1 hook name is gone"

# the drift gate: does the installed block byte-match the canonical render?
python3 "$HOME/.mpm/current/agent_installation/scripts/check_installed_managed_blocks.py"
```

The provenance line must name **`2045afcc57f1f7603f38e016a2909b85985e48dc`**
— not `unknown` — and the block must name `ctx.session.hook("context")`
and must **not** name `experimental.chat.system.transform`: the running
plugin registers the V2 hook, and in its `dist/index.v2.js` the V1 name
appears only inside a comment describing the V1→V2 mapping.

The drift gate's verdict must be **PASS** — *"installed block
byte-matches canonical render"*. A `WARN` means the installed block
differs from the release's canonical render; read the divergent line it
names before going further.

**This changes new sessions only.** The running OpenCode sessions keep
the block they started with. Restarting them is a separate, deliberate
act (§3.1, act 5).

### 5.3 What this step does not do

- it does not touch `~/.mpm/AGENTS.md`, `~/.mpm/CLAUDE.md` or
  `~/.mpm/CLI_AGENT_RULES.md` — those are the project guidance, not the
  host's managed block;
- it does not change the operator's content outside the markers — apart
  from the one newline immediately after the outer END marker that §5.1
  names;
- it does not change the payload, the binaries or the database;
- it does not restart any session.

## 6. What not to do

- **Do not execute the host-specific cutover procedure before it is
  approved against its exact checksum.** Its current revision declares
  itself **NOT APPROVED**, and no document grants what that one
  withholds: an independent approval recorded against the procedure's
  exact current SHA-256, and the operator's separate authorisation of
  the acts, are two different permissions and both must be in hand
  first (§3.2 P7, §3.8).
- **Do not record the procedure's approval inside the procedure.** Editing
  the file to carry the verdict changes its SHA-256 and invalidates the
  approval it was recording. The approval lives in an external
  attestation bound to the checksum (§3.8.1).
- **Do not run a destructive restore with `--restart-services`.** On that
  path the updater starts the unit itself, inside the rollback, before the
  command returns — on a database whose journal mode no gate has verified.
  Policy C1 requires the restore to be invoked **without** the flag, with
  the units stopped by hand under a separate authorisation (§4.4, §3.8).
- **Do not start the scheduler after a destructive restore before the
  restored database has been verified.** The table set, every
  recovery-point record present and unmodified, the one permitted
  health-check audit addition, the recovery point's SHA-256, the measured
  estimate, `integrity_check`, the header bytes and the journal mode must
  all pass. If any fails, nothing starts and the operator escalates
  (§4.6, §4.5).
- **Do not restore the assets into `releases/4a870c8a…` by hand.** It
  is the active release and a rollback target; a release is immutable,
  and an in-place edit changes what a rollback gives back. Promote
  instead.
- **Do not restore the Nov-7 backup as a routine step.** The
  promotion creates its own fresh recovery point (§3.4), and that is
  the one a rollback-with-restore uses. The Nov-7 point belongs to the
  earlier promotion; restoring it would roll the data back by the
  distance between that moment and now.
- **Do not restore the Nov-2 backup.** It is a **stale historical**
  recovery point: it holds the database as it was on 2026-11-02 — a
  *different and older* database than the live one.
- **Do not copy the assets from the old release `77738cfd…` into the
  new one.** They belong to a different revision, and a bundle's
  declaration is a COMPLETE description: it records the digest of every
  asset it names. Copying an asset from another revision means either
  a digest that does not match the declaration (and the bundle is
  refused) or a declaration rewritten to match the copy (and the
  declaration no longer describes what the authority provisioned).

  Measured against the pinned target: **seven** of the twelve assets are
  byte-identical between `77738cfd…` and `2045afcc…`; **five** differ —
  `MPM_AGENT_INTEGRATION_SNIPPETS.md`, `AGENTS.md.snippet` and
  `render_managed_blocks.py`, plus the two the fix itself changed
  (`install_agents_instructions.py`, `check_installed_managed_blocks.py`).
  A PARTIAL copy of the other seven would therefore be accepted by the
  validator, which is exactly why the rule is a rule and not a
  technicality: the validator cannot tell a deliberate partial copy
  from a complete one.
- **Do not hand-edit the managed block.** The refresher writes the
  provenance stamp; a hand-edit loses it and the next refresh cannot
  tell a current block from an edited one.
- **Do not promote a different revision to get around an existing
  release directory.** §3.5 covers the correct action.
- **Do not treat a restore as a promotion step.** A restore replaces the
  live database with an older copy and takes the installation out of WAL
  mode (§4.5). It is a separate, destructive, separately-approved act.
- **Do not change the live database's journal mode as part of an
  inspection, a rehearsal or a dry run.** If a restore is ever authorised,
  the journal mode is restored deliberately and verified afterwards
  (§4.5).
- **Do not run `--with-db-restore --force` without the three things §4.3
  requires**: the recovery point the command will select, **identified
  explicitly**; the **data delta** it would discard, measured and written
  down; and **separate operator approval** for the destructive step. The
  approval to promote is not the approval to destroy data.
- **Do not let the updater choose the recovery point by implication.** It
  selects the last history entry for the revision being rolled back from,
  which on this installation is the **November 7** point until the promotion
  has happened. That is the wrong point for this cutover, and the command
  will not say so (§4.3).
- **Do not leave the scheduler running on the old revision when the
  scheduler policy in force requires `--restart-services` on the
  promotion.** The updater stops the active unit after the candidate is
  built and preflighted and starts it back after the live smoke test,
  so the restarted unit resolves into the new release — and the
  promotion verifies that it does. Under that policy, "leave it running
  and accept the residual" is not an option (§3.8).
- **Do not stop or start the scheduler by hand inside the promotion
  window.** The updater owns the lifecycle there; a manual stop before
  it produces exactly the *"already inactive, so nothing to start"*
  trap (§3.8). **The destructive-restore path is a different act under a
  different policy** — Policy C1, where the units *are* stopped by hand,
  under a separate authorisation, inside a recorded maintenance window, and
  started only after the restored database has been verified (§4.4).
- **Do not type the managed block's provenance values.** Source
  `MPM_BUILD_COMMIT` and `MPM_BUILD_VERSION` from the active release's
  `RELEASE.json` and cross-check them against the release directory and
  the CLI before writing anything (§5.2).
- **Do not restore the managed-block backup after a runtime rollback** —
  neither the installer's `AGENTS.md.bak*` chain nor the host
  procedure's Stage 0b copy (`AGENTS.md.pre-cutover`). They are two
  different files with the same content, and both reinstate the retired
  V1 hook name, which the rolled-back runtime does not register either.
  Leave the refreshed block in place (§3.7).
- **Do not "repair" the block's stale provenance after a rollback, and
  do not restore the block to make it match the runtime.** The
  provenance line records where the block was **rendered from**; the
  rolled-back release carries no refresher to re-stamp it with, and the
  corrected host-specific procedure records the stale state rather than
  editing it (§3.7, §3.8).
- **Do not report a rollback after the refresh as a complete cutover on
  the old revision.** The host-asset gap is re-exposed, and repairing it
  is separately authorised (§3.7).
- **Do not build from a checkout that is the data root.** `git worktree
  add` writes into `$REPO/.git` (§3.3 Step 0, Step 1).
- **Do not read the promotion's health smoke as a check.** The smoke
  is a **live write path**: it runs the newly installed binary against
  the **live** database (`mpm call mpm_system --payload '{"action":
  "health_check"}'`, with `MPM_DB_PATH` pointing at the live file). It
  is the point of no return for the schema. It is *not* part of the
  dry run, and it is why the promotion is a separate, authorised act
  from the checks in §3.3 Steps 3–4.

### 6.1 Verify the recovery point before you rely on it

`~/.mpm/backups/upgrade-<timestamp>/` is created BEFORE the backup is
taken, and it is NOT removed when the backup fails. So the DIRECTORY's
existence proves nothing: a failed backup leaves an empty directory
behind, and restoring from it is restoring nothing.

The promotion's own backup is verified **structurally** at creation
(§3.4), so it does not need this to be *sound*. It still needs the
operator's **freshness** assessment (§4.2) to be *the point you expect* —
structural verification cannot see row counts (§4.1). An **older** recovery
point needs both.

```bash
BK=~/.mpm/backups/upgrade-20261107T201536Z

# 1. the database is there, and it is not empty
ls -l "$BK/mpm.db"

# 2. it opens READ-ONLY, and it is a real database with the expected tables.
#
#    The mode=ro URI is not decoration: a plain sqlite3.connect(path)
#    takes a WRITE LOCK on the database. On a recovery point that is
#    harmless; pointed at the LIVE database it would take a write lock on
#    a database the scheduler is actively writing to. Every open in this
#    procedure is read-only — see §4.7.
#
#    What this establishes is that the copy is STRUCTURALLY sound and
#    that it carries the expected tables. It does NOT establish
#    freshness — the fingerprint cannot see row counts (§4.1). For the
#    counts, compare against a baseline (§4.2).
python3 - "$BK/mpm.db" <<'PY'
import sqlite3, sys
db = sqlite3.connect(f"file:{sys.argv[1]}?mode=ro", uri=True)
# The table count EXCLUDES sqlite_* (sqlite_sequence and friends), so it
# is comparable with the updater's own count. Counting them would make
# two scripts disagree about the same database.
tables = [r[0] for r in db.execute(
    "SELECT name FROM sqlite_master WHERE type='table' "
    "AND name NOT LIKE 'sqlite_%' ORDER BY name")]
print("tables:", len(tables))
print("memories:", db.execute("SELECT COUNT(*) FROM memories").fetchone()[0])
migrations = 0
if "schema_migrations" in tables:
    migrations = db.execute("SELECT COUNT(*) FROM schema_migrations").fetchone()[0]
print("schema_migrations:", migrations)
print("integrity:", db.execute("PRAGMA integrity_check").fetchone()[0])
PY
```

**Every line must succeed.** `integrity_check` must print `ok`, and
`memories` and `schema_migrations` must be plausible for the moment
the backup was taken.

**3. And it must correspond to the intended recovery point.** The
update history records what each promotion did:

```bash
cat ~/.mpm/update-history.jsonl
```

The record for the promotion you are recovering from names the
`backup` directory it took. That is the directory to verify — not one
that merely exists.

For the record at the time of writing, the numbers are:

| point | memories | schema migrations |
|---|---|---|
| `upgrade-20261102T161236Z` (the Nov-2 backup) | 179 | 18 |
| `upgrade-20261107T201536Z` (the point for the defective promotion) | 320 | 20 |

The LIVE count is deliberately not written down here: it grows every
time the installation is used, so a number in a document is a number
that is wrong by the time it is read. Measure it instead — the same
script above, pointed at the live database:

```bash
python3 - ~/.mpm/src/db/mpm.db <<'PY'
import sqlite3, sys
db = sqlite3.connect("file:" + sys.argv[1] + "?mode=ro", uri=True)
print("memories:", db.execute("SELECT COUNT(*) FROM memories").fetchone()[0])
print("schema_migrations:", db.execute("SELECT COUNT(*) FROM schema_migrations").fetchone()[0])
PY
```

The point of the table is the RATIO: the Nov-2 backup is an order of
magnitude behind the live database, and the Nov-7 backup is the one
that matches the promotion being recovered from.

If the verification fails, the backup is not a recovery point. Do not
restore it. Find another, or accept that the promotion is a
forward-only operation for this installation.

### 6.2 Litter worth removing before the cutover

`~/.mpm/staging/candidate.<n>` — 289 MB of abandoned staging from the
interrupted 2026-11-07 update. It is not a blocker, but it is inside
the data root and it is space the promotion wants.

## 7. Cross-references

- the fix: `internal/update/hostassets.go`, `internal/update/source.go`,
  `internal/update/verify.go`, `scripts/lib/runtime_bundle.sh`;
- the authoritative asset list: `scripts/install_runtime_assets.py`
  (`ONBOARDING_SUPPORT`, `AGENT_PROTOCOL_DOCS`, and the per-adapter
  scan in `discover_source_assets`);
- the installer policy for operator-edited assets: cases A–G in
  `scripts/install_runtime_assets.py`;
- the migration policy for the managed block: §5.1 above, implemented by
  `agent_installation/mpm-opencode/scripts/install_agents_instructions.py`;
- the two validation scopes (candidate vs installed) and the exact
  compatibility rule: [`bundle-validation-scopes.md`](bundle-validation-scopes.md);
- the host-specific cutover procedure for this host — the one to run,
  and the one whose scheduler policy governs:
  `/home/v/src/MPM_CUTOVER_4a870c8a_to_2045afcc.md` (§3.8).

## 8. Known follow-ups

These are recorded, not fixed. None of them is a documentation defect, and
none of them belongs in a documentation-only change.

- **`internal/update/backup.go` — the package comment contradicts the
  implementation.** The introductory comment on `BackupLiveDB` says the
  backup is verified three ways, the third being *"every user table's row
  count matches the live database"*. The implementation deliberately does
  **not** compare row counts, and the design note on `verifyBackup` says so
  at length: `VACUUM INTO` captures an instant *during* the backup, so
  comparing its counts against a later live read compares two different
  points in time, and a legitimate concurrent write would produce a false
  failure. The verification is: the file is non-empty, it carries the
  `SQLite format 3` header, `PRAGMA integrity_check` passes, every table is
  readable, and the backup's **table set** equals the live table set.
  **The comment is stale and misleading** — a reader would believe the
  updater checks row counts, and it does not. It should be corrected in a
  separate change, in the same file, by the people who own it. **This
  document does not change production code.**
- **The host-specific cutover procedure carries no internal approval.**
  Its current revision is SHA-256
  `31ccfb7716b24fb0c04b1775f2700646c43b17ac8184e9d39364f934ca893fa6`,
  and its own status line reads **NOT APPROVED — pending a fresh
  independent review**. Four earlier checksums are void or never
  approved (`9fc056b9…` voided, `64303ebc…` returned REQUEST CHANGES by
  the Stage 4 review, `554c8183…` returned REQUEST CHANGES by the Phase 3
  database-safety review for N1/N3/N4 with N2 latent, and `20ec1688…`
  returned REQUEST CHANGES by the Phase 4 review for N4 alone), and this
  revision supersedes all four. The status line **cannot** be changed to
  say APPROVED, because that edit would change the checksum the approval
  is bound to — so the approval is recorded **outside** the procedure, in
  an attestation (§3.8.1). **The instrument is now specified and no
  attestation has been issued.** A local record and a `.sha256` beside
  it are not enough: the record must be published where the channel
  identifies its author, the publication must be **retrieved** and
  compared — author, verdict, scope, artifact hash, PR head where
  carried, and body bytes — and the scopes must be exactly the three
  canonical identifiers, each with its own reviewer's decision, in a
  versioned, retainable, revocable form whose current version is the
  highest one **and** valid and authenticated; and the **reviewer-to-scope
  assignments are the operator's own record**, kept outside the
  attestation set, because a shared publishing account cannot
  distinguish one reviewer from another (§3.8.1). **Approval and
  authorisation are separate**: the approval is a reviewer's verdict bound
  to the exact checksum and scoped; the authorisation is the operator's
  own, act by act (§3.1, §3.2 P7).
- **The three scopes are decided separately, and their bindings differ.**
  The independent database-safety review of `31ccfb77…` is an **APPROVE
  for `database-safety`** — the restoration path — and it found **no
  cutover blockers in the revision**; it was published as version 1
  against PR head `60079492…` and **renewed as version 2** against
  `488b94b8…`. The Stage 4 review is an **APPROVE for
  `managed-block-stage-4`**, bound to the procedure at `31ccfb77…`, also
  published as version 1 against `60079492…` and **renewed as version 2**
  against `488b94b8…`. The final independent gate is an **APPROVE for
  `whole-procedure`**, bound to the procedure at `31ccfb77…`; the PR at
  `c692e791…` was **CONDITIONAL** on it, and its `whole-procedure`
  publication is **still outstanding** — no version of it has been
  published. **A PR-head binding does not travel past an amendment to
  this guide** — so this amendment expires the two version-2 head
  bindings, and both must be published again against the head it
  produces, at the version number that history then dictates.
  **None of the three is permission to run the cutover** — the cutover
  needs all three, recorded, and then the operator's own authorisation on
  top (§3.8.2, §3.2 P7).
- **Five hardening items are open in the procedure, and none of them
  blocks this cutover on this host.** Recorded so they are not lost: (1)
  the router-note allow-list pattern accepts six message shapes where the
  code emits four, and the procedure's own change list says "four" — the
  pattern should be tightened and the wording corrected; (2) the cap of
  two `system_audit_log` rows is exactly this host's maximum, because the
  mode and persona defaults exist — a cap of four, or one derived from the
  emitted shapes, would remove the dependency on those files; (3)
  `HEALTH_PAYLOAD_HASH` is hard-coded, so a future change to the updater's
  payload would make the gate refuse a good restore — fail-closed, but a
  deadlock, and deriving it at run time would remove the coupling; (4)
  **this guide pinned a superseded checksum** — corrected here, in the
  change that carries this approval; (5) `verify_wal_state` prints only
  the first line of the integrity report, which is the banner rather than
  the specific error. All five are general future hardening; none is on a
  reachable path on this host.
- **The procedure's `unrepaired` repair path was deadlocked — CLOSED by
  the current revision.** Stage 4 accepts a block whose body is right
  but whose provenance is wrong, so that a failed refresh is repaired by
  **re-running** Stage 4 rather than by restoring the retired V1 copy.
  That repair could not pass the procedure's own post-check: the
  refresher appends one newline at the END-marker boundary on every
  rewriting run (suffix 7 → 8 → 9 bytes measured), while the post-check
  compared against the **Stage 0b backup** and allowed exactly one
  extra newline — so it failed on a **correct** block, and re-running
  failed identically, because the refresher was then a no-op and the
  suffix did not move. The operator was left with a correct block and a
  permanently failing gate. **The current revision fixes it:** Stage 4
  now takes a private, byte-exact snapshot of the file immediately
  before **each** refresh (`$BLOCK_PRE_RUN`) and judges the outcome
  against **that**, through the new `verify_refresh_outcome` helper;
  the pre-cutover backup is kept as the historical baseline and the
  recovery copy and is no longer the suffix comparator; and the
  refresher's report is classified explicitly, with an unrecognised
  report a **stop** rather than a guess. **The repair path can now
  complete** (§3.8).
- **What the current revision changed, and what it now carries.** The
  automatic restoration of the pre-refresh managed block after a runtime
  rollback is **removed** — it reinstated the retired V1 hook name that
  the rolled-back runtime's plugin does not register either (§3.7) — and
  the state a rollback leaves behind is **recorded** instead. The Stage
  4 post-check is also corrected: it now judges the refresh against a
  **per-run snapshot** of the file, not against the pre-cutover backup,
  so the `unrepaired` repair path can complete (§3.8). A
  destructive restore now requires a **measured** impact (`measure_restore_
  impact`: the point's SHA-256, per-table before/after counts,
  `rows_would_be_lost` / `rows_would_be_resurrected`) and an approval
  **bound** to that measurement. And after any restore the journal mode
  is **verified** (`verify_wal_state`) with an explicit recovery path
  (`reestablish_wal`) and a stop if the conversion fails. **This guide
  does not modify another procedure's text**; the two are reconciled in
  §3.8.
- **§4.5's journal-mode premise is corrected.** An earlier revision of
  this section said a restore leaves the installation running without
  WAL. It does not: MPM re-establishes WAL by itself on its next
  connection, because every production open path runs
  `db.Exec("PRAGMA journal_mode = WAL")`. The real residual is that the
  call **discards its error**, so a conversion that fails is silent.
  The verification step is worth having; it is defence in depth, not a
  gate on the restore.
- **The candidate binary does not expose its own schema.** The bundle's
  schema preflight is a file the bundle asserts; the format cannot prove it
  is the substrate's DDL. The durable fix is for the candidate binary to
  expose the DDL it embeds, so the updater can compare the bundle's file
  against the binary's own. It does not gate this cutover — the DDL is
  byte-identical at both revisions and the bundle's file is byte-identical
  to the canonical extraction — but it is the real fix for the general case.
- **The shell validator has no payload check.** A bundle whose runtime
  payload is missing a declared file is refused by the Go validator and
  **accepted** by the shell validator. The shell assembler can therefore
  publish a bundle the updater then refuses. The promotion path is not
  implicated (the promotion is validated by the Go validator), but the
  divergence is the dangerous direction and is worth reconciling.
