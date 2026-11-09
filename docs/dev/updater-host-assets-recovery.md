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

The installed `mpm-update` is the *old* build: it omits the host assets
and has no check for them. It cannot be used to produce a correct
bundle. The stable recovery executable at `~/.mpm/mpm-update` is the
one the operator runs, so it is the one that must be replaced — and the
old copy is kept, because it is the recovery tool for the runtime that
is running *now*.

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
  was on 2026-11-02 (179 memories against the live 335, 18 schema
  migrations against the live 20). Restoring it would silently roll the
  database back five days. The real pre-update recovery point is
  `~/.mpm/backups/upgrade-20261107T201536Z`.
- **Do not copy the assets from the old release `77738cfd…` into the
  new one.** They belong to a different revision; the declaration's
  digests would not match and the validator would refuse the bundle.
- **Do not hand-edit the managed block.** The refresher writes the
  provenance stamp; a hand-edit loses it and the next refresh cannot
  tell a current block from an edited one.

## 6. Cross-references

- the fix: `internal/update/hostassets.go`, `internal/update/source.go`,
  `internal/update/verify.go`, `scripts/lib/runtime_bundle.sh`;
- the authoritative asset list: `scripts/install_runtime_assets.py`
  (`ONBOARDING_SUPPORT`, `AGENT_PROTOCOL_DOCS`, and the per-adapter
  scan in `discover_source_assets`);
- the installer policy for operator-edited assets: cases A–G in
  `scripts/install_runtime_assets.py`;
- the migration policy for the managed block: §4.1 above, implemented by
  `agent_installation/mpm-opencode/scripts/install_agents_instructions.py`.
