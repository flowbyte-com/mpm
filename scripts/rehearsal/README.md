# The runtime-migration rehearsal

> **What this is:** the executable proof that MPM can move from a Git
> checkout to versioned, Git-free runtime bundles without losing the
> operator's data, without touching anything live, and with a rollback
> that works.
>
> **What it is not:** a deployment. It never writes to the live
> installation, never stops the live service, and never opens the live
> database for writing.

```bash
make rehearsal                    # rehearse the working tree
make rehearsal REHEARSAL_REV=4111c46   # rehearse a committed revision
```

The plan this proves is [`docs/dev/runtime-migration-plan.md`](../../docs/dev/runtime-migration-plan.md).
The evidence it produced is in
[`docs/dev/runtime-migration-evidence.md`](../../docs/dev/runtime-migration-evidence.md).

---

## Why it lives in the repository

The rehearsal was first run as a local one-off under `~/.cache`. That
made its evidence **unreproducible**: nobody else could re-run it, and
the plan that cited it lived in the same cache directory — so the
design and its evidence existed in exactly one place, on one machine,
and would have been lost with it.

The harness is now source. `make rehearsal` reproduces every number the
plan quotes.

---

## What it does

| Stage | What happens |
|---|---|
| `setup.sh` | Exports the source tree **without Git metadata**, builds it with the producer's provenance assertions, compiles the OpenCode payload, and assembles a versioned bundle. |
| `00_simulate.sh` | Builds an isolated simulation of a checkout-style installation under a **disposable HOME**: the binaries, the database, the operator's definitions, the OpenCode registration, the systemd unit. |
| `10_obligations.sh` | Runs the migration and asserts what must be true: the state is unchanged, the bundle carries no `.git`, the services start, the MCP server answers, the operator's files survive, the identity is reported correctly. |
| `20_negative.sh` | Asserts what must **not** happen: a bundle with a broken binary is refused, a failed promotion rolls back, a missing payload is refused, a tampered bundle is refused. Plus the harness's own integrity guards. |
| `30_schemas.sh` | Tests the upgrade path against **five representative existing schemas** — the live shape, the pre-incident shape, a fresh database, an empty file, and a database carrying an unrelated table with the same name. |

The aggregate is reported as one number. A suite that fails to **run** is
reported as a failure, not as a skip: a rehearsal that could not execute
has proved nothing.

---

## The three live-tree detectors

One detector is not enough, so there are three, and they are independent:

1. **A snapshot** of the live tree (path, size, mtime) before and after.
   Any difference is a failure.
2. **A `strace`** of every `open`/`stat`/`access`/`execve` in the test's
   process tree, grepped for the live paths. This catches a *read* that a
   snapshot cannot see.
3. **A path shim** that neutralises the systemd user-manager
   operations. Starting or restarting the operator's unit **is** a
   deployment, so the shim records the request instead of acting on it.

The live artifacts under that protection:

```
$HOME/.mpm                  the live workspace
$HOME/.config/opencode      the live OpenCode configuration
$HOME/.config/systemd/user  the live systemd user units
```

---

## The destructive-command guard

Every recursive delete in this harness goes through `safe_rm`, which
**refuses to remove anything outside the rehearsal root**.

This is not defensive decoration. The harness has already destroyed
itself once: a recursive delete with a mis-composed path took the whole
rehearsal tree with it, and the damage was silent — the script kept
running and only the missing files revealed it afterwards.

`20_negative.sh` additionally **fingerprints the harness's own contents**
before its first test and checks them after every one. A destructive bug
is caught at the point it happens, named, and cannot take anything else
with it.

---

## The rule the harness exists to protect

**The live database is opened only through the SQLite online backup API,
in read-only mode.**

A newer binary opening a database **applies whatever migrations it
believes are pending**. The rehearsal's own first implementation took its
SQL dump with the staged (newer) binary against the live database, and
**mutated the database it was backing up**. The driver's own state
fingerprint changed across the backup step.

So: the SQL dump is taken from the **verified backup copy**, and the
staged binary's `health_check` runs against a **scratch copy** — never
the live one.

---

## The layout

```
scripts/rehearsal/
├── run.sh              the driver: setup → simulate → obligations → negative → schemas
├── setup.sh            the Git-free export and the bundle
├── build-bundle.sh     assembles a versioned bundle
├── mpm-upgrade         the migration driver (11 stages, fault injection, rollback)
├── lib.sh              the assertion helpers and the live-tree detectors
├── 00_simulate.sh      builds the isolated simulation
├── 10_obligations.sh   the obligations
├── 20_negative.sh      the negative suite and the harness-integrity guards
├── 30_schemas.sh       the representative-schema upgrade tests
├── state_fingerprint.py  the state fingerprint used to prove "unchanged"
├── read_jsonc.py         reads a value out of the host's JSONC configuration
└── schema_shape.py       a table's canonical shape, for schema comparison
```

The work products (the source export, the bundle, the simulated home,
the traces, the logs) land in `$XDG_CACHE_HOME/mpm-rehearsal` (or
`~/.cache/mpm-rehearsal`), which is **outside the repository**. The
harness is source; its output is not.

**The work directory must be outside the repository**, and the harness
refuses an in-repository one. The work products include a **copy of the
source tree**, and this repository has repo-wide scanners that walk
every file under the root — the DB-safety guard and the deferral guard
both found the copy and both failed on it. A copy of the repository
inside the repository is incompatible with scanning the repository.

---

## Environment

| Variable | Meaning |
|---|---|
| `REH_REV` | The revision to rehearse against. Defaults to the value `setup.sh` resolved. |
| `REH_WORK` | Where the work products go. Defaults to `$XDG_CACHE_HOME/mpm-rehearsal`. **Must be outside the repository.** |
| `REH_ARCHIVE_COMMIT` | Rehearse a **committed** revision instead of the working tree. |
| `REH_SKIP_SETUP` | Reuse an existing bundle instead of rebuilding it. |
| `MPM_SOURCE_REPO` | The checkout to export from. Defaults to this repository. |

`REH_ARCHIVE_COMMIT` is deliberately **not** named `REH_COMMIT`: that
name is used by the harness's own record of what it staged, and reading
it would make every run an archive run — rehearsing the *committed* tree
and silently ignoring the changes under review.

---

## Requirements

`bash`, `python3`, `sqlite3`, `make`, `git`, `tar`, and `strace`
(optional — without it the syscall detector is skipped and the live-hits
count is reported as 0). The OpenCode plugin's `node_modules` must be
present for the payload to compile; without it the bundle carries no
payload and the promotion is **refused**, which is the intended
behaviour.
