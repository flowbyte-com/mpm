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
| **3. service restart** | `mpm-update --restart-services` **on the promotion itself** (it has no effect on a later re-run), or `systemctl --user restart mpm-scheduler` by hand | the **executables the daemons run** | the daemons run `2045afcc`; until then they keep running `4a870c8a` |
| **4. instruction refresh** | §5.2 | `~/.config/opencode/AGENTS.md` (only the marked span) | the provenance line names `2045afcc` |
| **5. OpenCode restart** | by hand | the running agent sessions | only **new** sessions read the refreshed block |

Acts 3, 4 and 5 are **not** part of the promotion. The promotion does
not stop the daemons, does not rewrite the operator's instructions and
does not touch the agent sessions. Each is a separate decision with its
own authorisation; §6 records the residual of leaving them undone.

### 3.2 Preconditions

| # | precondition | how to check |
|---|---|---|
| P1 | the fix is merged and the target is the pinned revision | `git log --oneline -1 origin/main` → `2045afcc` (§3.3 Step 0) |
| P2 | a Go toolchain, node/npm and python3 are available | `command -v go node npm python3` |
| P3 | the target does **not** already exist under `releases/` — **or** it does and it validates | `ls ~/.mpm/releases/` (§3.4) |
| P4 | disk headroom **and** temporary-space headroom | `df -h / ~/.mpm "$TMPDIR"` (§3.3 Step 1) |
| P5 | no other agent is using the updater | process inventory |
| P6 | the operator has read §5 (the instruction migration) | — |

### 3.3 Steps

#### Step 0 — pin and verify the target

```bash
TARGET=2045afcc57f1f7603f38e016a2909b85985e48dc
REPO=/path/to/a/checkout/of/this/repository

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
"$BUILD_ROOT/mpm-update-fixed" --commit "$TARGET"
```

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

**Do not pass `--restart-services`.** A service restart is an
operational act with its own authorisation, and this procedure does
not assume one — see §3.1.

After the promotion, `mpm-scheduler` (and `mpm-telemetry`, if it is
installed) are still running the **previous** revision's executables,
while the CLI on `PATH`, the managed block's provenance line and the
recovery executable all name the new one. The residual is real and it
lasts until the daemons are restarted:

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

Two ways to close it, in the operator's order of preference:

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
      → [stop and start the services, only with --restart-services]
      → install the release → move the pointer
      → establish the stable paths
      → health smoke (a live write path — see §6)
      → install the stable updater (last, atomically)
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
| **failed service identity** — after `--restart-services`, the history record or the promotion's report carries a warning that a unit runs something other than the active release | the daemon was not actually restarted, or it restarted from another path | The updater records a **warning**; it does not claim a clean restart. Restart the unit by hand and re-check `systemctl --user status <unit>`; the running executable must resolve into `~/.mpm/releases/2045afcc…` |
| **degraded rollback** — the promotion reports `DEGRADED:` | the live schema changed and the previous release cannot read it, **or** the post-smoke fingerprint failed so the schema state is unknown | The pointer **stays at the new release** (the binaries match the data). Recover with `mpm-update --rollback --with-db-restore --force` — which restores the **pre-update backup the promotion took** and overwrites newer data — or fix forward. Do not delete anything |

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

## 4. Database safety

> This section is about the **data**, not the binaries. The promotion in §3
> never replaces the live database — it takes a copy of it. A **restore**
> does. That difference is the whole of the database-safety story, and it is
> why the promotion is a normal operation while a restore is not.

### 4.1 What the schema fingerprint establishes — and what it does not

`SchemaFingerprint` hashes the database's **structure**: the `sqlite_master`
entries (each table's name and its `CREATE` statement) and every table's
`PRAGMA table_info` columns. It reads **no rows**.

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
only (§4.6):

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

python3 - "$BK/mpm.db" "$BK/../.." <<'PY'
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

Compare the shape with the recorded `pre_fp` — the updater will refuse a
restore whose copy does not carry it, and so should you.

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
| **unverifiable** | the process's `/proc/<pid>/fd` could not be read — root-owned system services — and no lock on the database was found | the restore is **refused unless `--force`**; with `--force` they are **accepted and recorded**, never silently ignored |
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
unverifiable     : 42   (root-owned system daemons; /proc/<pid>/fd unreadable)
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
| root-owned system daemons | 42 unverifiable on this host | **no** — and no lock on the database was found for any of them |

**An ordinary promotion needs none of this.** The promotion takes an **online**
backup (`VACUUM INTO`, a transactionally consistent copy) and **never replaces
the live database**. It does not stop the scheduler, does not stop agent
sessions, and does not need quiescence. The writer scan exists for the
**restore** path, which is the only thing that replaces the live file.

**Quiescence, stated plainly:** before a restore, the scheduler must be
stopped **and** the operator's agent sessions must be closed by their owners.
The updater can stop the first and will never stop the second.

### 4.5 The journal mode a restore changes

The recovery point is produced by `VACUUM INTO`, which writes a database in
the **DELETE** (rollback-journal) mode. The live database is in **WAL** mode.
`restoreDatabase` copies the recovery point over the live file and then
removes the live `-wal` and `-shm` sidecars — which is exactly what leaving
WAL mode requires.

Measured on a private copy (2026-11-11), reproducing what the restore does:

```
before: journal_mode = wal        live header bytes 18/19: 2,2
after : journal_mode = delete     Nov-7 point header bytes 18/19: 1,1
memories after the restore: 320    (live had 2,405)
integrity: ok
```

**So a restore does not only roll the data back — it takes the live
installation out of WAL mode.** The consequence is not a detail of the
restore: the installation then runs without WAL, so there is no concurrent
reader during a write and no shared-memory index, and the next writer
re-establishes the sidecars from scratch.

**Nothing in the upstream procedure says so.** It must be said here.

**If a restore is ever authorised, the journal mode is a separate, validated
step — not an automatic consequence.** The recovery procedure for the
journal mode is:

1. **decide explicitly** whether the installation should return to WAL, and
   record the decision. WAL is what the live installation uses and what
   `internal/core/db.go` sets on open;
2. **restore first**, then **re-establish the mode** with an explicit
   `PRAGMA journal_mode = WAL` against the restored database, and **verify**
   it: the header bytes 18/19 must read `2,2`, `PRAGMA journal_mode` must
   report `wal`, and `PRAGMA integrity_check` must still return `ok`;
3. **verify the data** against the counts measured in §4.3, step 2;
4. only then restart the scheduler and re-open the agent sessions.

**Do not change the live database's journal settings during a review, a
rehearsal or a dry run.** The mode is a property of the running
installation, not an inspection parameter.

### 4.6 Inspecting a recovery point safely

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
and the refresh is reversible from the `.bak` file.

The refresh is also a **correctness** fix, not a cosmetic one: the
installed block names the OpenCode **V1** hook
`experimental.chat.system.transform`, which the running V2 plugin does
not register. The refreshed block names `ctx.session.hook("context")`.

### 5.2 The step

Run it **after Step 6**, from the installed assets. The paths below do
not exist before the promotion: the active release carries only the
runtime payload until the repair adds the assets.

**The command must export the provenance**, or it will stamp
`commit=unknown` and fail its own acceptance test below.
`block_provenance` reads `MPM_BUILD_VERSION` and `MPM_BUILD_COMMIT`
from the environment; `install.sh` and `make refresh-installed` export
both, but a standalone run of the refresher exports nothing:

```bash
MPM_BUILD_VERSION=0.1.0-dev \
MPM_BUILD_COMMIT="$TARGET" \
python3 ~/.mpm/agent_installation/mpm-opencode/scripts/install_agents_instructions.py \
  --scope user \
  --target ~/.config/opencode/AGENTS.md \
  --snippet ~/.mpm/agent_installation/mpm-opencode/templates/AGENTS.md.snippet
```

The reconciler (`reconcile_managed_blocks.py --only mpm-opencode
--home $HOME`) resolves the manifest's snippet path relative to the
adapter directory and is an equivalent route — **but it does not export
the provenance either**, so the two exports are still required.

Verify:

```bash
grep -n "mpm-provenance" ~/.config/opencode/AGENTS.md
grep -n "ctx.session.hook" ~/.config/opencode/AGENTS.md
```

The provenance line must name **`2045afcc57f1f7603f38e016a2909b85985e48dc`**
— not `unknown` — and the block must name `ctx.session.hook("context")`.

**This changes new sessions only.** The running OpenCode sessions keep
the block they started with. Restarting them is a separate, deliberate
act (§3.1, act 5).

### 5.3 What this step does not do

- it does not touch `~/.mpm/AGENTS.md`, `~/.mpm/CLAUDE.md` or
  `~/.mpm/CLI_AGENT_RULES.md` — those are the project guidance, not the
  host's managed block;
- it does not change the operator's content outside the markers;
- it does not change the payload, the binaries or the database;
- it does not restart any session.

## 6. What not to do

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
#    procedure is read-only — see §4.6.
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
  compatibility rule: [`bundle-validation-scopes.md`](bundle-validation-scopes.md).

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
