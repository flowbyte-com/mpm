# F — Dogfooding and isolation runbook

> Session A foundation assignment. Every procedure below was **executed and
> verified** on 2026-10-26, not merely reasoned about.

## 1. The three environments — they are NOT the same thing

| # | Environment | Location | Holds | Who may write |
|---|---|---|---|---|
| **1** | **Installed runtime** | `~/.mpm` | the live knowledge database (`src/db/mpm.db`), the installed binaries, the scheduler's state | **Session A only**, at a milestone |
| **2** | **Shared dogfooding workspace** | `~/.mpm-dogfood` (proposed) | the sprint's shared knowledge: decisions, lessons, handoffs | Sessions A, B, C |
| **3** | **Disposable test workspace** | `$(mktemp -d)` | nothing durable — created, used, destroyed | any automated test |

### Why they must be distinct

- **The live database holds real content that must not be overwritten or
  migrated casually**: 27 reference documents / 116 reference chunks / 73
  memories, including a WordPress and Cloudflare documentation corpus. That
  corpus is *reference material*, not scratch space.
- **The sprint's shared knowledge is disposable.** It is dogfooding: the
  point is to exercise MPM's real multi-agent behaviour, not to build a
  permanent archive inside the live install.
- **Automated tests must touch neither.** A test that connects to the live
  database is a data-integrity incident waiting to happen.

### The current state, measured

The OpenCode MCP wiring that ships with MPM points at the **live** workspace:

`~/.mpm/.mcp.json` → `command: ./.build/bin/mpm-mcp`, with `MPM_WORKSPACE`
set to `${HOME}/.mpm`. Its own comment documents the ghost-DB bug that
occurred when that value was `.`:

> Setting it to `.` made the mpm-mcp server treat the checkout as a runtime
> workspace … the MCP server then bootstrap-created `mode/` and `persona/`
> inside the source tree and resolved its DB there — source pollution, and a
> ghost database distinct from the canonical one.

So **as installed, the adapter's MPM tools write to the live database.**
Switching the sprint to the dogfooding workspace is a one-environment
variable change (below) — and it must be done deliberately, not assumed.

## 2. The verified workspace mechanism

### 2.1 How the workspace is resolved

`internal/core/config/config.go`:

```
GetMPMDir():
  1. $MPM_WORKSPACE            (explicit; MkdirAll'd 0700)
  2. $HOME/.mpm
  3. cwd                        (last-resort failsafe; loudly surfaced)

GetWorkspace():
  1. $MPM_WORKSPACE
  2. $HOME/.mpm
```

And the database path is **absolute and workspace-derived**:

> The database is ALWAYS at `mpm/src/db/mpm.db` regardless of projectRoot.
> — `internal/core/db.go:1241`

There is a second, more specific override, `MPM_DB_PATH`, which the
installed scheduler unit uses:

```
Environment=MPM_WORKSPACE=/home/v/.mpm
Environment=MPM_DB_PATH=/home/v/.mpm/src/db/mpm.db
```

**Conclusion: `MPM_WORKSPACE` fully determines which database is used.**
That is the isolation mechanism. There is no per-project partitioning in the
schema (see `event-contract.md` §4.3).

### 2.2 How the OpenCode adapter resolves it

`agent_installation/mpm-opencode/src/workspace.ts` — the plugin **pins**
`MPM_WORKSPACE` on every `mpm` subprocess it spawns:

```ts
export function resolveWorkspace(): string {
  const env = process.env.MPM_WORKSPACE;
  if (env && env.trim() !== "") return env;
  return path.join(os.homedir(), ".mpm");
}
export function withWorkspace(env = {}) {
  return { ...env, MPM_WORKSPACE: env.MPM_WORKSPACE ?? resolveWorkspace() };
}
```

The binary is resolved separately (`resolve-mpm-binary.ts`):
`$MPM_BINARY` → `$HOME/.mpm/bin/mpm` → `$HOME/.local/bin/mpm`.

**So dogfooding with a developer build is `MPM_WORKSPACE` + `MPM_BINARY`.**

### 2.3 Verified: an isolated workspace is created lazily and stays isolated

```
$ export MPM_WORKSPACE=/tmp/opencode/dogfood-verify
$ /home/v/.mpm/bin/mpm info
...
$ find /tmp/opencode/dogfood-verify -maxdepth 3
/tmp/opencode/dogfood-verify/src/db/mpm.db
/tmp/opencode/dogfood-verify/src/db/mpm.db-shm
/tmp/opencode/dogfood-verify/src/db/mpm.db-wal
/tmp/opencode/dogfood-verify/src/db/mpm.db.lock
```

The schema is created on first use; the live database was **not** touched
(27 / 116 / 73 before and after).

### 2.4 Verified: backup and restore

```
$ MPM_WORKSPACE=$WS mpm ops backup $WS/backups/test.sql
Backup written: .../test.sql (86806 bytes)

$ head -3 test.sql
/* WARNING: Script requires that SQLITE_DBCONFIG_DEFENSIVE be disabled */
PRAGMA foreign_keys=OFF;
BEGIN TRANSACTION;
```

The dump contains **76 `CREATE TABLE` statements** and restores cleanly:

```
$ yes | MPM_WORKSPACE=$WS2 mpm ops restore-db $WS/backups/test.sql
About to restore DB from: .../test.sql
This will overwrite the current database. Continue? [y/N] Restored from: ...
$ python3 -c "... select count(*) from sqlite_master where type='table'"
  tables: 77
  memories: 5
```

**`restore-db` is interactive.** It reads a confirmation from stdin and
aborts without a TTY. In an automated procedure, pipe `yes`.

### 2.5 ⚠ A hazard found during verification — **confirmed, not closed**

`mpm info` reported `mpm-scheduler : running (pid 3050)` while pointed at
the **isolated** workspace. The check is **systemd-unit-based**:
`cmd/mpm/handlers_info.go:418` runs `systemctl --user is-active
mpm-scheduler` and then `systemctl --user show … MainPID`. There is exactly
**one** such unit, and the installed one pins
`MPM_WORKSPACE=/home/v/.mpm` — so the line is evidence about the **live**
workspace and about nothing else.

> **Correction (2026-10-26, A-7).** An earlier revision of this runbook
> described the check as "process-based". That was **wrong**, and the
> conclusion was right for the wrong reason. The correct statement is
> stronger: the check resolves a **single systemd unit**, and that unit is
> pinned to the live workspace by its own `Environment=` lines. Credit to
> Session B for catching it.

**Re-confirmed on 2026-10-26, with the process's own environment read
directly** (§8.5):

```
$ tr '\0' '\n' < /proc/3050/environ | grep ^MPM_
MPM_WORKSPACE=/home/v/.mpm
MPM_DB_PATH=/home/v/.mpm/src/db/mpm.db
MPM_BACKUP_DIR=/home/v/.mpm/backups/critic-pre
MPM_CRITIC_BIN=/home/v/.mpm/bin/mpm-critic
MPM_BIN=/home/v/.mpm/bin/mpm

$ ls -l /proc/3050/fd | grep mpm.db
  6 -> /home/v/.mpm/src/db/mpm.db.lock
  7 -> /home/v/.mpm/src/db/mpm.db
  8 -> /home/v/.mpm/src/db/mpm.db-wal
  9 -> /home/v/.mpm/src/db/mpm.db-shm
```

The running scheduler holds **only** the live database open. There is no
scheduler maintaining `~/.mpm-dogfood`.

**Consequence for the sprint — now a stated fact, not an open issue:** the
dogfooding workspace has **no scheduler of its own**, and the `mpm info`
line "mpm-scheduler : running" must **never** be read as evidence that a
given workspace is maintained. See §8.5.

## 3. The shared dogfooding workspace — initialised 2026-10-26

> **Status change (2026-10-26):** the workspace is no longer a proposal. It
> was created and seeded earlier on 2026-10-26 and has been **verified**;
> see §8 for the verification record and §8.5 for the scheduler status.

### 3.1 Location

```
~/.mpm-dogfood/
├── src/db/mpm.db          the shared knowledge database
├── mode/  persona/  config/  blobs/
└── backups/
```

`~/.mpm-dogfood` is **outside** `~/.mpm`, so a deployment that replaces
`~/.mpm` cannot destroy the sprint's shared knowledge, and the sprint's
writes cannot corrupt the live install.

### 3.2 Initialisation (verified procedure)

The commands below **do** exist in the current build: `mpm ops init
directives`, `mpm ops init skills`, `mpm ops init tasks` and
`mpm ops init topics` are listed by `mpm ops --help` and implemented in
`cmd/mpm/ops_init_*.go`. (They are easy to miss — `mpm ops --help` lists
them at the very end of a long list, and `mpm ops init` on its own prints
the generic `ops` help.)

```bash
export MPM_WORKSPACE="$HOME/.mpm-dogfood"
export MPM_DB_PATH="$MPM_WORKSPACE/src/db/mpm.db"   # see §8.2 — this matters
export MPM_BINARY="$HOME/src/mpm/.build/bin/mpm"      # the development build
mkdir -p "$MPM_WORKSPACE" "$MPM_WORKSPACE/home"

# 1. Create the schema (lazy; verified above).
MPM_WORKSPACE="$MPM_WORKSPACE" HOME="$MPM_WORKSPACE/home" \
  "$MPM_BINARY" info

# 2. Seed the standing directives and skills so the workspace is
#    equivalent to a fresh install.
MPM_WORKSPACE="$MPM_WORKSPACE" HOME="$MPM_WORKSPACE/home" \
  "$MPM_BINARY" ops init directives
MPM_WORKSPACE="$MPM_WORKSPACE" HOME="$MPM_WORKSPACE/home" \
  "$MPM_BINARY" ops init skills
MPM_WORKSPACE="$MPM_WORKSPACE" HOME="$MPM_WORKSPACE/home" \
  "$MPM_BINARY" ops init tasks

# 3. Verify.
MPM_WORKSPACE="$MPM_WORKSPACE" HOME="$MPM_WORKSPACE/home" \
  "$MPM_BINARY" ops stats
```

### 3.3 Pointing the sessions at it

For an OpenCode session, the plugin reads `MPM_WORKSPACE` from the process
environment and pins it for every MPM subprocess:

```bash
export MPM_WORKSPACE="$HOME/.mpm-dogfood"
export MPM_BINARY="$HOME/src/mpm/.build/bin/mpm"   # optional: dogfood a developer build
```

**Verification that the switch took effect:**

```bash
MPM_WORKSPACE="$MPM_WORKSPACE" mpm info | grep -i "workspace\|database"
```

### 3.4 The three properties the brief requires

| Property | How it is achieved | Verified? |
|---|---|---|
| All three sessions retrieve the same shared decisions and lessons | one workspace, one database, one `mpm` binary | ✅ by construction — same path ⇒ same file |
| Concurrent writes are serialised | the substrate's existing single-writer SQLite discipline + WAL + `busy_timeout=5000`; **not** re-implemented | existing mechanism, preserved |
| Duplicate knowledge is not silently accumulated | the existing provenance/dedup machinery and the `mpm_memory` de-duplication paths | existing mechanism, preserved |
| Project identity is stable across worktrees | **not implemented.** See `event-contract.md` §4.3 — MPM has no project column; the workspace is the boundary | ⛔ open |
| Provenance identifies the originating session | `framework_name`, `framework_session_id`, `mpm_session_id`, `invocation_id` are already recorded on `tool_invocations` | ✅ existing |
| Retrieval distinguishes relevant project knowledge from unrelated data | `memory_store` (`local` / `shared`), FTS scoping, `contextual_focus` routing | existing mechanism, preserved |
| Crashes or interrupted writes do not corrupt shared state | WAL + transactional writes; `mpm ops backup`/`restore-db` for recovery | ✅ verified above |
| The runtime version in use is recorded and verified | `mpm version` / `mpm --version` report the build provenance; `mpm info` reports installation identity | ✅ available |

**Honest gaps:** *project identity across worktrees* is not implemented, and
the *scheduler* does not follow the workspace (see §2.5). Both are recorded
as open issues. Neither blocks the sprint's read-only investigation phase.

## 4. Concurrent access — what to expect

- SQLite in WAL mode: one writer at a time, concurrent readers.
- `busy_timeout=5000` gives a writer 5 s to acquire the lock.
- The substrate uses `*sql.Tx` discipline, **not** `SetMaxOpenConns(1)` —
  the codebase forbids the latter (it deadlocks). Sessions must not
  introduce it.
- If a write times out, it fails loudly. It does not silently drop.

**Practical guidance:** have one session write at a time when recording
decisions or lessons. The substrate will serialise them; doing it
sequentially avoids 5-second waits.

## 5. Automated test isolation — the rule

> Every potentially destructive, experimental or schema-changing test must run
> with an isolated effective `HOME` and `MPM_WORKSPACE` **throughout its
> entire process tree**.

### 5.1 The mechanism, verified

`MPM_WORKSPACE` is read from the environment by `GetMPMDir()`, and the
database path is derived from it. Setting it in the environment of a process
**and its children** is sufficient: `os.environ` in the parent, `env={...}`
in `subprocess`, and `MPM_WORKSPACE` in a `shell:` block all work.

Two complementary controls are needed, because `HOME` alone is not enough:

| Control | Why |
|---|---|
| `HOME` → a scratch directory | `GetMPMDir()` falls back to `$HOME/.mpm`; the scratch `HOME` keeps the fallback inside the scratch area |
| `MPM_WORKSPACE` → an explicit scratch path | makes the target unambiguous, independent of `HOME` |

`MPM_DB_PATH` is the belt-and-braces override, used by the installed
scheduler unit. Set it too when a test must be certain.

### 5.2 The canonical pattern

```bash
WS="$(mktemp -d)"
export HOME="$WS/home"
export MPM_WORKSPACE="$WS/workspace"
export MPM_DB_PATH="$WS/workspace/src/db/mpm.db"
mkdir -p "$HOME" "$MPM_WORKSPACE"
trap 'rm -rf "$WS"' EXIT
```

In Go tests, `t.Setenv("HOME", t.TempDir())` plus
`t.Setenv("MPM_WORKSPACE", t.TempDir())` covers the process; **child
processes must receive the environment explicitly** — `subprocess` inherits
`os.environ` by default, so `t.Setenv` propagates, but an explicitly
constructed `env=` dict does not. The repository's own tests use
`env={**os.environ, "HOME": str(self.home)}` for exactly this reason.

### 5.3 The failure mode this prevents

`~/.mpm/.mcp.json`'s own comment records it: a mis-set `MPM_WORKSPACE`
made the server create `mode/` and `persona/` **inside the source tree** and
resolve a *ghost database* distinct from the canonical one. A test that
points at the wrong workspace does not fail loudly — it silently writes
somewhere else.

**Verification, not assumption:** the isolation mechanism above was
executed end-to-end during this assignment. A scratch workspace was created,
backed up, restored into a second scratch workspace, and the live database's
row counts were confirmed unchanged (27 / 116 / 73) before and after.

### 5.4 The scheduler is not isolated

The scheduler is a **systemd user service** with `MPM_WORKSPACE` pinned to
`/home/v/.mpm` in its unit file. It cannot be redirected by an environment
variable in a test's shell.

**Therefore: no automated test may rely on the scheduler being absent.**
Any test whose correctness depends on the scheduler *not* running must
isolate at the **service** level, not the environment level. This is a real
constraint and is why §2.5's "scheduler: running" report matters.

## 6. Milestone promotion procedure — documented, **not executed**

Session A only, and only at a milestone — never for every experimental
commit.

### 6.1 Preconditions

1. The milestone's commits are merged to `main`.
2. The integration gate has passed **on the merged commit**:
   - `make build`
   - `make test-race` (all Go packages, zero data races)
   - `make test-scripts`
   - `make test-agent-installation`
   - CI's three gating jobs green **on that SHA**.
3. A milestone tag or a recorded SHA exists.

### 6.2 The procedure

```bash
# STEP 1 — Record what is currently installed (before anything changes).
mpm --version
mpm info
git -C ~/.mpm rev-parse HEAD

# STEP 2 — Back up the live database and record the provenance.
TS=$(date -u +%Y%m%dT%H%M%SZ)
mkdir -p ~/.mpm/backups/promote
mpm ops backup ~/.mpm/backups/promote/pre-$TS.sql
sha256sum ~/.mpm/backups/promote/pre-$TS.sql

# STEP 3 — Evaluate migration and rollback compatibility.
#   - Which SafeMigrations will run against the live schema?
#   - Is every one of them idempotent and safe on the current schema?
#   - Is there a downgrade path for each? (Migrations here are additive:
#     nullable columns and CREATE INDEX IF NOT EXISTS.)
#   - Is the rollback (restore from the dump) acceptable?

# STEP 4 — Fast-forward the RUNTIME checkout from its verified upstream.
#   ~/.mpm is an independent clone of the same upstream; it is advanced
#   here, NOT from a local branch.
git -C ~/.mpm fetch origin
git -C ~/.mpm checkout main
git -C ~/.mpm pull --ff-only origin main
git -C ~/.mpm rev-parse HEAD        # MUST equal the verified milestone SHA

# STEP 5 — Install using the repository's OWN installer and safety checks.
cd ~/.mpm && ./install.sh
#   install.sh builds from ~/.mpm and installs into ~/.mpm/bin.
#   It owns the transactional five-binary promotion and its rollback.

# STEP 6 — Verify the installed provenance and database health.
mpm --version
mpm info
mpm doctor --deep-scan

# STEP 7 — Validate the scheduler and the OpenCode integration.
systemctl --user status mpm-scheduler
systemctl --user restart mpm-scheduler
#   Then verify the OpenCode plugin can reach the new runtime.

# STEP 8 — Reconnect affected sessions if necessary.
#   Long-lived OpenCode sessions hold an mpm-mcp child process; they must
#   be restarted to pick up the new binary.

# STEP 9 — Record the promotion.
#   - deployed SHA
#   - binary provenance string
#   - backup path and its sha256
#   - the migration entries that ran
#   - the verification results
```

### 6.3 Rollback

```bash
# Restore the database.
yes | mpm ops restore-db ~/.mpm/backups/promote/pre-$TS.sql

# Roll the runtime checkout back.
git -C ~/.mpm checkout <previous-sha>
cd ~/.mpm && ./install.sh

# Verify.
mpm --version
mpm doctor --deep-scan
systemctl --user restart mpm-scheduler
```

### 6.4 Known limitations of the rollback path — stated plainly

1. **The binary promotion is transactional; the database migration is not.**
   `install.sh` rolls back the *binaries* if a promotion fails. A migration
   that has already run against the live database is **not** undone by
   re-installing an older binary. Rollback of the database requires the
   dump in step 2. The migrations currently in flight (from PRs #88 and #95)
   are additive — a nullable column and a `CREATE INDEX IF NOT EXISTS` — so
   rolling the *binary* back is safe: the extra column and index are
   simply unused by the older binary. **This is the reason to prefer
   additive migrations.**
2. **`restore-db` overwrites the current database.** It is destructive and
   it is interactive. Confirm the target path before running it.
3. **The scheduler's `MPM_WORKSPACE` is pinned in the unit file.** Moving
   the live workspace means editing the unit, not setting an environment
   variable.
4. **A promoted binary is not automatically picked up by running sessions.**
   `mpm-mcp` is a long-lived child process; it must be restarted.

## 7. What this runbook does *not* do

- It does **not** migrate or move the live database. The live reference
  corpus stays where it is.
- It does **not** make `~/.mpm` a development worktree. It is a deployment
  target.
- It does **not** promise that the dogfooding workspace can be pointlessly
  pointed at a *running scheduler*. See §2.5 and §8.5.

---

## 8. Verification record — 2026-10-26

Everything below was **executed**, not reasoned about. The live database was
never a write target.

### 8.0 Binary provenance

The dogfooding workspace is driven by the **development build at `main`**,
not by the installed runtime:

```
$ ~/src/mpm/.build/bin/mpm --version
MPM 0.1.0-dev+gb5b18401.t1793023964
  binary:  mpm
  commit:  b5b18401974ee559a485cda20abf8f7779f11bf2
  build:   development
  built:   2026-10-26T14:12:44Z
  dirty:   no
```

The installed runtime at `~/.mpm/bin/mpm` reports
`0.1.0-dev+g3df72315.t1792851233` — commit `3df72315`, which is **27
behind `main`** — it predates every one of the fifteen corrective PRs.
`MPM_BINARY` selects the development build; see
§8.4 for the proof that the plugin honours it.

### 8.1 Database isolation — proven by inode and by resolved path

```
$ stat -c '%n dev=%d ino=%i size=%s' ~/.mpm/src/db/mpm.db ~/.mpm-dogfood/src/db/mpm.db
/home/v/.mpm/src/db/mpm.db            dev=64513 ino=36984047 size=8441856
/home/v/.mpm-dogfood/src/db/mpm.db    dev=64513 ino=36706413 size=1105920
```

Different inodes, different realpaths, different files. The dogfooding
database holds the sprint's shared knowledge (12 memories, 5 standing
directives, 1 skill, 3 decisions, 1 handoff, 11 evidence rows).

### 8.2 `MPM_DB_PATH` cannot silently redirect the CLI or the MCP server

**This is the precise finding, and it has two halves.**

* `mpm` and `mpm-mcp` **ignore `MPM_DB_PATH` entirely.** `NewDatabaseManager`
  derives the path from the workspace (`internal/core/db.go`: *"The database
  is ALWAYS at `mpm/src/db/mpm.db` regardless of `projectRoot`"*), and there
  is no `MPM_DB_PATH` reference in `internal/core/db.go` or
  `internal/core/config/`. Proven with a sentinel:

  ```
  MPM_WORKSPACE=~/.mpm-dogfood MPM_DB_PATH=/tmp/.../sentinel/mpm.db mpm info
    → database path : /home/v/.mpm-dogfood/src/db/mpm.db
    → the sentinel file was NEVER created
  ```

* **`mpm-critic` and `mpm-scheduler` DO honour `MPM_DB_PATH`** — it is the
  default of their `-db` flag (`cmd/mpm-critic/main.go:57`,
  `cmd/mpm-scheduler/main.go:51`), and `internal/scheduler/scheduler.go:988`
  reads it first as well. Proven against a sentinel:

  ```
  $ MPM_DB_PATH=/tmp/.../sentinel/mpm.db mpm-critic --help
    -db string
       Path to mpm.db (default: $MPM_DB_PATH or src/db/mpm.db via DatabaseManager)
       (default "/tmp/.../sentinel/mpm.db")
  ```

**Therefore the rule for the dogfooding environment is: pin `MPM_DB_PATH`
to the dogfooding database, alongside `MPM_WORKSPACE`.** Without that pin, a
scheduler or a critic started from a dogfooding shell with a live
`MPM_DB_PATH` in its environment would open the **live** database.

### 8.3 Scratch isolation — verified

A scratch area with its own `HOME`, `MPM_WORKSPACE` and `MPM_DB_PATH`:

* the scratch database was created under the scratch workspace;
* neither the scratch `HOME` nor the dogfooding scratch `HOME` acquired a
  `.mpm` directory;
* a write through the scratch environment left the **live** database's
  mtime and size byte-identical.

### 8.4 The OpenCode plugin and the MCP subprocesses inherit the intended configuration

Proven by running the **installed** plugin's compiled modules:

```
$ node -e "import('~/.mpm/agent_installation/mpm-opencode/dist/workspace.js')
             .then(m=>console.log(m.resolveWorkspace()))"
/home/v/.mpm                                     # MPM_WORKSPACE unset → the LIVE workspace
$ MPM_WORKSPACE=~/.mpm-dogfood node -e "... m.resolveWorkspace()"
/home/v/.mpm-dogfood                             # → the dogfooding workspace
$ MPM_WORKSPACE=~/.mpm-dogfood node -e "... m.withWorkspace({})"
{"MPM_WORKSPACE":"/home/v/.mpm-dogfood"}         # pinned into the child's env

$ MPM_BINARY=~/src/mpm/.build/bin/mpm node -e "... m.resolveMpmBinary(process.env)"
/home/v/src/mpm/.build/bin/mpm                   # the development build
$ (no MPM_BINARY) node -e "... m.resolveMpmBinary(process.env)"
/home/v/.mpm/bin/mpm                             # the installed runtime
```

Two consequences worth stating:

1. **The plugin resolves the workspace from `process.env.MPM_WORKSPACE` and
   pins it for every MPM subprocess.** Exporting it in the OpenCode process
   environment is therefore sufficient — and it is the *only* thing that
   switches the plugin off the live workspace.
2. **The OpenCode plugin loaded by the host is the installed payload** at
   `~/.mpm/agent_installation/mpm-opencode`, referenced by
   `~/.config/opencode/opencode.jsonc`. Its source is **unchanged** between
   the installed runtime's commit (`3df72315`) and `main` (`b5b1840`), so
   the behaviour measured above is the behaviour of the repository's current
   plugin source. It will diverge once Session C changes the plugin — until
   then, adapter changes need `install.sh` to become visible to the host.

The MCP server is wired separately: `~/.mpm/.mcp.json` pins
`MPM_WORKSPACE: ${HOME}/.mpm` — **the live workspace**. That file is
project-scoped wiring inside the runtime root and is changed only by a
deployment.

### 8.5 Scheduler status — stated plainly

| Question | Answer |
|---|---|
| Does `~/.mpm-dogfood` have a dedicated scheduler? | **No.** |
| Does it have any scheduler at all? | **No.** |
| Is there a supported isolation mechanism for one? | **Yes, but not deployed.** The scheduler takes `--db` and `--lock`; a dogfooding scheduler would need its own unit file with its own `MPM_WORKSPACE`/`MPM_DB_PATH`/`--lock`. Creating one is a deployment act, and the brief for this round explicitly says **do not modify the live systemd service**. No second unit was created. |
| What does `mpm info` report for the dogfooding workspace? | `mpm-scheduler : running (pid 3050)` — **misleading**. That is the *live* scheduler, whose environment and open file descriptors are shown in §2.5. The check is **systemd-unit-based** (`systemctl --user is-active mpm-scheduler` + its `MainPID`); the unit is pinned to `MPM_WORKSPACE=/home/v/.mpm`, so the line says nothing about any other workspace. |
| What did the live systemd service do during this work? | **Nothing.** It was not stopped, restarted, edited or reloaded. |

**Consequence for the sprint:** the dogfooding workspace's knowledge is
maintained only by the sessions themselves. Anything that depends on the
scheduler — scheduled wakes, confidence decay, retention, the critic's
periodic cycle — **does not run** against it. Sessions must not infer from
`mpm info` that it does.

### 8.6 Concurrency — verified against one workspace

8 concurrent writers and 8 concurrent readers against a single workspace
database, then 16 further serial operations:

* every operation succeeded;
* **all 16 probe rows landed** — no lost writes;
* the substrate's own discipline was used (WAL + `busy_timeout=5000` +
  `*sql.Tx` discipline, **not** `SetMaxOpenConns(1)`, which the codebase
  forbids).

### 8.7 Backup and restore — verified against disposable data only

* `mpm ops backup` against the dogfooding workspace produced a 141 771-byte
  SQL dump.
* `mpm ops restore-db` into a **scratch** workspace produced a database with
  the same memory count (12) as the source.
* No restore was performed against the live database. `restore-db` is
  destructive and interactive; it is a milestone-only operation (§6.3).

### 8.8 The live database's contents — unchanged

| Table | Before | After | |
|---|---|---|---|
| `reference_docs` | 27 | 27 | unchanged |
| `reference_chunks` | 116 | 116 | unchanged |
| `memories` | 85 | 85 | unchanged |
| `lessons_base` | 2 | 2 | unchanged |
| `evidence` | 61 | 61 | unchanged |
| `system_audit_log` | 5 | 5 | unchanged |
| `tool_invocations` | 4516 | 4657+ | **grows** — see below |

**`tool_invocations` grows, and that is expected.** It is the live
*OpenCode plugin's own* traffic: the plugin polls `read_wake_context` every
few seconds, and each poll is a tool invocation that the plugin's audit hook
records. In the observed window it added ~190 `mpm-cli` / `mpm_context` /
`read_wake_context` rows. The live scheduler contributes its own activity.

**This is the strongest practical argument for the dogfooding workspace:**
an agent session that has not been switched to `~/.mpm-dogfood` writes its
audit trail into the live install, continuously.

### 8.9 Strict isolation for automated tests — enforced, not merely documented

The rule in §5 ("every potentially destructive test runs with an isolated
`HOME` / `MPM_WORKSPACE` / `MPM_DB_PATH`") is not left to discipline. The
repository enforces it with a **repo-wide AST guard**,
`internal/core/test_db_safety_test.go`, which is part of the test suite
that CI runs.

**What it detects.** The dangerous calls are *constructors*, so the guard
walks the AST rather than grepping for strings (a grep hits comments and
doc examples; the guard's own rule table would trip it):

| Constructor | Rule | Why it leaks |
|---|---|---|
| `NewDatabaseManager("")` | empty first argument only | falls through to `config.GetMPMDir()` → `$HOME/.mpm/src/db/mpm.db` |
| `NewMemoryStore(<anything>)` | **every** call | the path argument is *discarded* (`func NewMemoryStore(_ string)`), so every call resolves `config.GetMPMDir()`; the store stays inert until something calls `InitSQLite()`, which then writes the full DDL to the live database |

**Legitimate isolation does not go through those constructors at all** — it
goes through `NewTestDM` / `NewTestLocalOnlyDM` / `NewTestSharedDM` (all of
which route through `t.TempDir()` or an in-memory DSN), or through
`NewDatabaseManager(t.TempDir())`, whose non-empty argument the rule does
not match. A test that genuinely needs a raw constructor must add itself to
`testDBSafetyWhitelist` **by repo-relative path with a justification** — the
guard fails the build if a whitelist entry has an empty justification,
because an unexplained exception is indistinguishable from a hole in the
guard.

**The guard is itself pinned, so it cannot quietly decay:**

| Pinning test | What it prevents |
|---|---|
| `TestDBSafety_ScanCoversEveryModuleArea` | the scan resolving the *nearest* `go.mod` and stopping before `cmd/mpm`, `internal/scheduler`, `internal/core/tools` and `release_acceptance` — 265+ test files, one of them in a module only `make test-release` executes. **This is exactly how the previous version of the guard became decorative.** |
| `TestDBSafety_NotShortSkipped` | the guard skipping under `-short`, which would make it never run in either of the two places it exists to protect (`make test-core-precommit` and CI's "Core tests" step) |
| `TestDBSafety_DetectsProductionPathConstructorInTestFile` | the detection logic silently ceasing to detect |
| `TestDBSafety_AllowsIsolatedFixture` | the guard becoming so broad that legitimate isolated fixtures are rejected |
| `TestDBSafety_EmptyScanScopeIsAnError` | the scan silently covering nothing and passing vacuously |

**Verified on 2026-10-26** (run from the `internal/core` module, which is
where it lives — it is a separate Go module, `github.com/flowbyte-com/mpm-core`):

```
$ cd internal/core && go test -tags fts5 -run 'TestDBSafety' -v ./
--- PASS: TestDBSafety_NoLiveDBLeakInTests (0.23s)
--- PASS: TestDBSafety_ScanCoversEveryModuleArea (0.01s)
--- PASS: TestDBSafety_NotShortSkipped (0.00s)
--- PASS: TestDBSafety_DetectsProductionPathConstructorInTestFile (0.00s)
--- PASS: TestDBSafety_AllowsIsolatedFixture (0.00s)
--- PASS: TestDBSafety_EmptyScanScopeIsAnError (0.00s)
PASS
ok  	github.com/flowbyte-com/mpm-core	0.244s
```

**What this does and does not cover.** It covers *the repository's own test
suite*. It does **not** cover a script, a manual command, or an agent that
constructs a `DatabaseManager` outside the test tree — which is why §8.3's
scratch discipline and §8.2's `MPM_DB_PATH` pinning still matter for
anything a session runs by hand.

### 8.11 The environment mechanism — a separately launched, pinned host

> **Added 2026-10-26.** Measured, not assumed. See
> [`session-a-adjudication.md`](session-a-adjudication.md) §3 for the full
> record.

**The defect.** The running OpenCode processes do not inherit the sprint's
pins. Read from `/proc/<pid>/environ`, the desktop app, its renderer and
`opencode-cli serve --service` each have **no** `MPM_WORKSPACE`, no
`MPM_DB_PATH` and no `MPM_BINARY`. The plugin therefore resolves
`~/.mpm` (the **live** workspace) and `~/.mpm/bin/mpm` (the **stale**
binary `0.1.0-dev+g3df72315`). Plugin traffic lands in the live database;
shell-side `mpm call` lands in `~/.mpm-dogfood`. The two channels are
**split across two databases**, and neither is attributed to a session.

**What is NOT available.** The host's **plugin-options** channel is not
usable in opencode v2.1.4: the config normaliser rejects the tuple form
(`$.plugins.0 kind=invalid action="skipped malformed recognized value"`),
and a probe confirms `effect(input, options)` is called with
`options = null`. There is no first-class "configure this plugin" mechanism
in this host version.

**What IS available: a separately launched, pinned, isolated development
host.** A probe plugin, started by an isolated `opencode run --standalone`
with the pins exported, observed `MPM_WORKSPACE`, `MPM_DB_PATH` and
`MPM_BINARY` **in its own process environment**. The **real** MPM plugin,
run the same way, wrote its rows into the scratch database and **nothing**
into `~/.mpm` or `~/.mpm-dogfood`.

The launch must:

1. **require** the three pins and **refuse to start** if any is missing or
   resolves to `~/.mpm` — so it **cannot silently fall back** to the live
   workspace;
2. redirect `XDG_{CONFIG,DATA,STATE,CACHE}_HOME` and `OPENCODE_CONFIG_DIR`
   to a disposable tree, so the host's own state, session store and database
   are never touched;
3. register the **repository's** plugin, not the installed payload;
4. use `opencode run --standalone` (or `opencode serve` on a free port with
   `OPENCODE_PASSWORD` read from its own log), so no shared managed service
   is contacted;
5. point `resolveMpmBinary` at the **development** build.

**Verification is from inside the plugin process**, never from the
launching shell's variables: the plugin writes its own resolved
`MPM_WORKSPACE` / `MPM_BINARY` to a probe log, and the acceptance harness
reads the **actual system-prompt array** the agent received.

**The desktop application is not touched.** Changing how a running
user-facing process is launched is an operator decision, recorded as an open
item in [`session-a-adjudication.md`](session-a-adjudication.md) §7.

### 8.10 Summary

| Requirement | Status | Evidence |
|---|---|---|
| The workspace uses its own SQLite database | ✅ | §8.1 — distinct inode |
| The binary has recorded provenance | ✅ | §8.0 — `b5b18401` |
| `MPM_WORKSPACE` is pinned correctly | ✅ | §8.4 — the plugin honours and pins it |
| `MPM_DB_PATH` cannot silently redirect into the live database | ✅ for `mpm`/`mpm-mcp`; ⚠ for `mpm-critic`/`mpm-scheduler`, which is **mitigated by pinning it** | §8.2 |
| The plugin and MCP subprocesses inherit the intended configuration | ✅ for the plugin (the MCP wiring in `~/.mpm/.mcp.json` still points at the live workspace, by design) | §8.4 |
| Scratch tests use independently isolated `HOME` / `MPM_WORKSPACE` / DB overrides | ✅ | §8.3 |
| The live database's contents remain unchanged | ✅ for every knowledge table; `tool_invocations` grows from the live plugin's own traffic | §8.8 |
| Concurrent reads and writes work | ✅ | §8.6 |
| Backup and restore verified only against disposable data | ✅ | §8.7 |
| The scheduler situation is documented honestly | ✅ — **no scheduler** for the dogfooding workspace; the `mpm info` line is not evidence | §8.5 |
| The live systemd service was not modified | ✅ | §8.5 |
| Automated tests are strictly isolated from the live database | ✅ — enforced in-repo by an AST guard, not by convention | §8.9 |
