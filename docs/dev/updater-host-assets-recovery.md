# Recovering the host integration assets on the already-installed runtime

> **This is a procedure, not an execution.** Nothing in this document has
> been run against the live installation. Every command is written for
> the operator to run deliberately, with the preconditions and stop
> conditions stated.

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

Thirteen files. The digests are the ones the authority
(`scripts/install_runtime_assets.py`) produced for the promoted commit,
so they are checkable rather than asserted.

```
agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md        9d9f23be6999
agent_installation/mpm-agent-protocol.md                    00ad1de7e510
agent_installation/mpm-opencode/reconcile.json              927c559c7d0a
agent_installation/mpm-opencode/scripts/install_agents_instructions.py  b8ae313993f1
agent_installation/mpm-opencode/scripts/jsonc_config.py     890af38078da
agent_installation/mpm-opencode/templates/AGENTS.md.snippet  b3c47c3e744d
agent_installation/scripts/block_provenance.py              364f29677f6f
agent_installation/scripts/check_installed_managed_blocks.py 5eeeab65063a
agent_installation/scripts/managed_block_convergence.py     883a6c4040e2
agent_installation/scripts/reconcile_managed_blocks.py      929f52f58e75
agent_installation/scripts/render_managed_blocks.py         dc86a5f5b2a8
agent_installation/scripts/stage_runtime_package.sh         0737c617f532
config/runtime-assets.json                                  db2daf0160ca
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

### 2.1 What the installed releases carry that no declaration names

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

### 3.1 Preconditions

| # | precondition | how to check |
|---|---|---|
| P1 | the fix is merged and the branch is available | `git log --oneline -1 origin/main` |
| P2 | a Go toolchain, node/npm and python3 are available | `command -v go node npm python3` |
| P3 | the target revision does **not** already exist under `releases/` | `ls ~/.mpm/releases/` |
| P4 | disk headroom | `df -h / ~/.mpm` |
| P5 | no other agent is using the updater | process inventory |
| P6 | the operator has read §4 (the instruction migration) | — |

### 3.2 Steps

**Step 1 — build the fixed updater, out of tree.**

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
```

The commit it names must be one that predates the fix. If it names a
commit that already contains the fix, the recovery executable is
already current and Step 2 is unnecessary.

The stable recovery executable at `~/.mpm/mpm-update` is the one the
operator runs, so it is the one that must be replaced — and the old
copy is kept, because it is the recovery tool for the runtime that is
running *now*.

**It is a REGULAR FILE, not a symlink.** `installStableUpdater`
(`internal/update/activate.go`) writes it through a temporary file and
a rename(2), so it never resolves into a release directory. That is
why replacing it cannot mutate an immutable release — the very thing
this document warns against elsewhere — and it is why `install -m
0755` is safe here. Confirm it before replacing it:

```bash
test -L ~/.mpm/mpm-update && echo "symlink — stop and investigate" \
                      || echo "regular file — safe to replace"
```

```bash
# from a checkout of the fixed branch
go build -tags fts5 -o /tmp/mpm-update-fixed ./cmd/mpm-update

# keep the current recovery executable
cp -p ~/.mpm/mpm-update ~/.mpm/mpm-update.pre-host-assets
```

**Step 2 — install the fixed recovery executable.**

```bash
install -m 0755 /tmp/mpm-update-fixed ~/.mpm/mpm-update
```

**Step 3 — dry-run against the target revision, in a disposable
workspace.**

`--dry-run` takes no lock and is not serialised against other agents'
dry-runs, so run it when nobody else is using the updater. Point
`MPM_WORKSPACE` at a scratch directory so the real installation is not
touched.

```bash
MPM_WORKSPACE=$(mktemp -d) ~/.mpm/mpm-update --dry-run --commit <fixed-commit>
```

The report must contain **`[ok] host-assets`**. That line is the proof
that the candidate carries the integration assets — before the fix, the
dry-run reported 11 checks and none of them was this one.

**Stop** if the report shows `FAIL`, if the `host-assets` line is
absent, or if the dry-run changed anything under `MPM_WORKSPACE`
(it must not: `--dry-run` takes no backup, stages nothing in the data
root and records nothing).

**Step 4 — promote.**

```bash
~/.mpm/mpm-update --commit <fixed-commit>
```

The promotion now provisions the host assets into the candidate, the
candidate declares them, the validator checks the declaration against
the candidate's own contents, and only then is the release installed
and `current` repointed.

**The daemons are NOT restarted by this command.** `mpm-update` stops
and restarts the managed units only when it is given
`--restart-services`, and this procedure does not: a service restart
is an operational act with its own authorisation, and this document
does not assume one.

So after the promotion, `mpm-scheduler` and `mpm-telemetry` are still
running the **previous** revision's executables, while the CLI on
`PATH` and the managed block's provenance line name the new one. The
residual is real and it lasts until the daemons are restarted:

```bash
# what the running daemons are actually executing
systemctl status mpm-scheduler mpm-telemetry | grep -E "ExecStart|Active"
```

Two ways to close it, in the operator's order of preference:

1. **restart the daemons explicitly**, once the operator has decided
   that is acceptable:

   ```bash
   ~/.mpm/mpm-update --commit <fixed-commit> --restart-services
   ```

   or, equivalently, restart the units by hand.

2. **leave them running** and accept the residual until the next
   reboot or the next service restart. The daemons are long-lived
   schedulers; they do not pick up the new binaries on their own.

What this residual does NOT affect: the CLI, the managed block's
provenance line, and the recovery executable all name the new revision
immediately after the promotion. Only the two daemons lag.

**Stop** if the promotion reports a failure, or if the new release does
not carry `agent_installation/mpm-agent-protocol.md`.

**Step 5 — verify the repair.**

```bash
# the advertised paths now resolve
ls -l ~/.mpm/agent_installation
cat ~/.mpm/agent_installation/mpm-agent-protocol.md | head -3
test -f ~/.mpm/agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md && echo OK

# the release declares them
python3 -c "import json;d=json.load(open('$HOME/.mpm/releases/<fixed-commit>/config/runtime-assets.json'));print(len(d['assets']),'assets declared')"

# the binaries are the promoted ones
~/.mpm/bin/mpm --version
```

### 3.3 Stop conditions

- the dry-run does not report `[ok] host-assets` → **stop**;
- `~/.mpm/releases/<fixed-commit>` already exists → **stop**: a release
  is immutable and will not be replaced; choose another revision;
- the promotion reports `outcome: failed` → **stop** and read
  `~/.mpm/update-history.jsonl` before retrying;
- the live database's schema fingerprint changed → **stop**: the
  updater will refuse a runtime-only rollback, and the recovery
  point named in the history record is the one to use.

## 4. The instruction migration policy

**The updater does not rewrite the operator's instructions, and neither
does this procedure.** The managed block is refreshed by a separate,
explicit step, and the step is governed by an existing policy.

### 4.1 The policy

`agent_installation/mpm-opencode/scripts/install_agents_instructions.py`
implements it:

| rule | behaviour |
|---|---|
| only the marked span is rewritten | the region between `<!-- BEGIN MPM-MANAGED SECTION:opencode-instructions -->` and `<!-- END MPM-MANAGED SECTION:opencode-instructions -->` is replaced; **everything outside is preserved byte-for-byte** |
| a backup is written first | `<target>.bak`, `<target>.bak.1`, … next to the target |
| a BEGIN without a matching END | **refuses to modify** and exits 2 — the conservative case |
| no managed block at all | appends the section, after a backup |
| already current | a no-op; the provenance timestamp is preserved |

So the operator's own text outside the markers is never overwritten,
and the refresh is reversible from the `.bak` file.

### 4.2 The step

Run it **after** step 5, from the installed assets — the refresher
resolves its snippet as a sibling of the adapter directory, so it must
run from the installed tree:

```bash
python3 ~/.mpm/agent_installation/mpm-opencode/scripts/install_agents_instructions.py \
  --scope user \
  --target ~/.config/opencode/AGENTS.md \
  --snippet ~/.mpm/agent_installation/mpm-opencode/templates/AGENTS.md.snippet
```

Verify:

```bash
grep -n "mpm-provenance" ~/.config/opencode/AGENTS.md
grep -n "ctx.session.hook" ~/.config/opencode/AGENTS.md
```

The provenance line must name the **new** revision, and the block must
name `ctx.session.hook("context")`.

### 4.3 What this step does not do

- it does not touch `~/.mpm/AGENTS.md`, `~/.mpm/CLAUDE.md` or
  `~/.mpm/CLI_AGENT_RULES.md` — those are the project guidance, not the
  host's managed block;
- it does not change the operator's content outside the markers;
- it does not change the payload, the binaries or the database.

## 5. What not to do

- **Do not restore the assets into `releases/4a870c8a…` by hand.** It
  is the active release and a rollback target; a release is immutable,
  and an in-place edit changes what a rollback gives back. Promote
  instead.
- **Do not restore the Nov-2 backup.** `~/.mpm/backups/upgrade-20261102T161236Z`
  is a **stale historical** recovery point: it holds the database as it
  was on 2026-11-02 — a *different and older* database than the live
  one, and restoring it would silently roll the data back by the
  distance between those two points in time. The live database has
  moved on since; the counts are in §5.1, and they are the reason.
  The pre-update recovery point is
  `~/.mpm/backups/upgrade-20261107T201536Z` — and it must be VERIFIED
  before it is relied upon. See §5.1.
- **Do not copy the assets from the old release `77738cfd…` into the
  new one.** They belong to a different revision, and a bundle's
  declaration is a COMPLETE description: it records the digest of every
  asset it names. Copying an asset from another revision means either
  a digest that does not match the declaration (and the bundle is
  refused) or a declaration rewritten to match the copy (and the
  declaration no longer describes what the authority provisioned).

  Measured: nine of the twelve assets are byte-identical between
  `77738cfd…` and the fixed revision — only
  `MPM_AGENT_INTEGRATION_SNIPPETS.md`, `AGENTS.md.snippet` and
  `render_managed_blocks.py` differ. A PARTIAL copy of the other nine
  would therefore be accepted by the validator, which is exactly why
  the rule is a rule and not a technicality: the validator cannot tell
  a deliberate partial copy from a complete one.
- **Do not hand-edit the managed block.** The refresher writes the
  provenance stamp; a hand-edit loses it and the next refresh cannot
  tell a current block from an edited one.

### 5.1 Verify the recovery point before you rely on it

`~/.mpm/backups/upgrade-<timestamp>/` is created BEFORE the backup is
taken, and it is NOT removed when the backup fails. So the DIRECTORY's
existence proves nothing: a failed backup leaves an empty directory
behind, and restoring from it is restoring nothing.

Verify the named recovery point before you depend on it:

```bash
BK=~/.mpm/backups/upgrade-20261107T201536Z

# 1. the database is there, and it is not empty
ls -l "$BK/mpm.db"

# 2. it opens, and it is a real database with the expected tables
python3 - "$BK/mpm.db" <<'PY'
import sqlite3, sys
db = sqlite3.connect(sys.argv[1])
tables = [r[0] for r in db.execute(
    "SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")]
print("tables:", len(tables))
print("memories:", db.execute("SELECT COUNT(*) FROM memories").fetchone()[0])
migrations = 0
if "schema_migrations" in tables:
    migrations = db.execute("SELECT COUNT(*) FROM schema_migrations").fetchone()[0]
print("schema_migrations:", migrations)
db.execute("PRAGMA integrity_check").fetchone()
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
| `upgrade-20261107T201536Z` (the pre-update recovery point) | 320 | 20 |

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

## 6. Cross-references

- the fix: `internal/update/hostassets.go`, `internal/update/source.go`,
  `internal/update/verify.go`, `scripts/lib/runtime_bundle.sh`;
- the authoritative asset list: `scripts/install_runtime_assets.py`
  (`ONBOARDING_SUPPORT`, `AGENT_PROTOCOL_DOCS`, and the per-adapter
  scan in `discover_source_assets`);
- the installer policy for operator-edited assets: cases A–G in
  `scripts/install_runtime_assets.py`;
- the migration policy for the managed block: §4.1 above, implemented by
  `agent_installation/mpm-opencode/scripts/install_agents_instructions.py`;
- the two validation scopes (candidate vs installed) and the exact
  compatibility rule: [`bundle-validation-scopes.md`](bundle-validation-scopes.md).
