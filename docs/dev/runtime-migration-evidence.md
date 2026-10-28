# Runtime migration — the evidence

> **Companion to** [`runtime-migration-plan.md`](runtime-migration-plan.md).
> Every claim in the plan points at a section here.
>
> **Collected:** 2026-10-28, Session A. **Method:** read-only inspection
> of the live installation, plus isolated rehearsals and executable-level
> tests. **Nothing was deployed; the live installation was never written
> to.**

---

## 1. The live installation, as measured

### 1.1 The layout

```
$ ls -la ~/.mpm/
drwx------ 22 v v  4096 Oct 24 15:13 .
drwxr-x--- 46 v v  4096 Oct 28 08:19 ..
drwxr-xr-x  3 v v  4096 Oct 23 11:41 .build      75.7 MB   build output (a duplicate of bin/)
drwxr-xr-x  7 v v  4096 Oct 27 07:42 .git        16.3 MB   Git metadata
drwxr-xr-x  3 v v  4096 Oct 23 11:39 .github
-rw-r--r--  1 v v    8801 Oct 23 11:39 .gitignore
...
drwxr-xr-x 17 v v  4096 Oct 24 15:13 bin         75.7 MB   the five installed binaries
...
drwxr-xr-x 17 v v  4096 Oct 23 11:39 src         17.5 MB   source + the persistent state
```

231 MB in total, serving four roles at once: a Git checkout, a binary
prefix, a data root and a configuration root.

### 1.2 The database

```
$ ls -la ~/.mpm/src/db/mpm.db*
-rw------- 1 v v 14364288 Oct 28 07:31 mpm.db
-rw------- 1 v v  4117504 Oct 28 07:31 mpm.db-wal
```

Read through the read-only online backup API (never opened for
writing):

| Property | Value |
|---|---|
| `PRAGMA integrity_check` | `ok` |
| `PRAGMA foreign_key_check` | clean |
| journal mode | WAL |
| `schema_migrations` sentinels | 18 |

### 1.3 The running service

```
$ systemctl --user show mpm-scheduler -p NRestarts -p MainPID -p ExecMainStartTimestamp
MainPID=2804
NRestarts=0
ExecMainStartTimestamp=Tue 2026-10-27 06:27:22 GMT
```

The service was **verified against the running process**, not assumed
from the unit file: `/proc/2804/environ` was read to establish the
environment the daemon actually has.

```
MPM_WORKSPACE=/home/v/.mpm
MPM_DB_PATH=/home/v/.mpm/src/db/mpm.db
MPM_BACKUP_DIR=/home/v/.mpm/backups/critic-pre
MPM_CRITIC_BIN=/home/v/.mpm/bin/mpm-critic
MPM_BIN=/home/v/.mpm/bin/mpm
```

### 1.4 The five host registrations

| Registration | Names |
|---|---|
| `~/.config/opencode/opencode.jsonc` | `file:///home/v/.mpm/agent_installation/mpm-opencode` |
| `~/.config/systemd/user/mpm-scheduler.service` | `ExecStart=/home/v/.mpm/bin/mpm-scheduler`, `WorkingDirectory=/home/v/.mpm` |
| `~/.local/bin/mpm` | a symlink into `~/.mpm/bin` |
| `~/.local/bin/mpm-mcp` | a symlink into `~/.mpm/bin` |
| the scheduler's environment | `MPM_WORKSPACE` and `MPM_DB_PATH` (above) |

**This is why the bundle stays under `~/.mpm`.** An XDG move would
require editing the live, user-owned OpenCode JSONC and the systemd
unit — operator-owned configuration the migration has no mandate to
change.

### 1.5 The installed identity

```
$ ~/.mpm/bin/mpm --version
MPM 0.1.0-dev+g3df72315.t1792851233
  binary:  mpm
  commit:  3df723156d7649517508212453de72d347759f43
  build:   development
  built:   2026-10-24T14:13:53Z
  dirty:   no
```

### 1.6 The live schema is AHEAD of the live binary

`lifecycle_events` exists in the live database and in `main`, but **not**
in the installed revision `g3df72315`'s schema. It is residue of the
2026-10-27 `lifecycle_events` incident.

It is harmless — an extra table — but it proves the point that matters
for the preflight: the live schema and the installed binary's schema are
not the same thing, and only one of them can be assumed.

---

## 2. The findings

| # | Finding | Status |
|---|---|---|
| **F1** | `~/.mpm` is a Git checkout doing four jobs, with a 75.7 MB duplicate of `bin/` in `.build`. | Addressed by D1. |
| **F2** | The live schema is ahead of the live binary. | Investigated — §5. |
| **F3** | A malformed `mpm_config.json` is a **SEGMENTATION FAULT**, not an error. | **Fixed** — §3. |
| **F4** | `MPM_DB_PATH` handling is inconsistent across the binaries, and the scheduler/critic silently open a different database. | **Fixed** — §4. |
| **F5** | "The telemetry sidecar does not carry the build metadata." | **WITHDRAWN** — §4.2. |
| **F6** | `CREATE TABLE IF NOT EXISTS` preserves an unrelated table of the same name. | Recorded — §5, case S5. |
| **F7** | A newer binary opening a database applies pending migrations. | Recorded — §6. |

---

## 3. F3 — the malformed-configuration crash

### 3.1 The defect

```go
// internal/core/memory.go, before the fix
cfg, _ := config.LoadConfig()
memoryPath := config.ResolveEnvPath(cfg.MemoryDir)
```

`config.LoadConfig()` returns `(nil, err)` when `mpm_config.json` does
not unmarshal. The error was discarded with `_`, and the nil config was
dereferenced on the next line.

### 3.2 Why it is easy to hit

`components` maps a substrate-component to a **profile NAME**, so its
values must be strings. A boolean is a one-character slip:

```json
{ "components": { "memory": true } }
```

That single token turned `mpm memory save` into a SIGSEGV with a stack
trace — and every other command that consulted the configuration with
it.

### 3.3 The fix

Two halves, both landed:

1. `config.ValidateConfig()` reports the problem as an error that
   **names the file**, the reason, and the repair command:

   ```
   fatal: the configuration file /home/v/.mpm/mpm_config.json is not valid JSON: ...
     MPM cannot start with a malformed configuration.
     Fix the file, or run `mpm config set` to rewrite it.
   ```

   A **missing** configuration file is not an error: an install with no
   configuration yet is a normal state.

2. `DefaultMemoryPaths()` resolves the canonical paths anyway, so a
   caller that reaches it before the startup gate does not crash. That
   is defence in depth *behind* the gate — a panic cannot name the file
   that is wrong.

### 3.4 The gate

The CLI and the MCP server refuse to start with a malformed
configuration. The CLI's **`config` command family is exempt**: it is
how an operator *repairs* a malformed configuration, so gating it on the
configuration's own validity would lock the operator out of the only
tool that fixes it.

The gate runs **after** the version interception: `mpm --version` must
answer when everything else is broken, including the configuration.

### 3.5 The proofs

| Proof | Where |
|---|---|
| The validation error names the file | `internal/core/config/config_malformed_test.go` |
| The constructor does not panic | `internal/core/memory_malformed_config_test.go` |
| The CLI refuses, and `--version` still answers | `cmd/mpm/db_path_resolution_test.go` (`TestD4_VersionSurfaceStaysFreeOfTheGate`) |

---

## 4. F4 and F5 — the database-path divergence

### 4.1 The divergence

`cmd/mpm-scheduler/main.go` and `cmd/mpm-critic/main.go` derived the
workspace from an explicit `--db` / `MPM_DB_PATH` with:

```go
projectRoot = filepath.Dir(filepath.Dir(filepath.Dir(*dbPath)))
```

The derivation **assumes** the path has the canonical
`<root>/src/db/mpm.db` shape and **never checks it**. Measured:

```
  /home/v/.mpm/src/db/mpm.db       -> workspace /home/v/.mpm -> db /home/v/.mpm/src/db/mpm.db
  /tmp/other.db                    -> workspace /           -> db /src/db/mpm.db
  /srv/data/mpm.db                 -> workspace /           -> db /src/db/mpm.db
```

The operator asked for one database and got another. Nothing said so, at
startup or ever afterwards, and every write landed in a database they did
not name.

### 4.2 F5 is withdrawn

An earlier finding claimed the telemetry sidecar did not carry the build
metadata. **It was wrong**, and the error was mine: the probe used
`head -1`, which reads the banner, not the identity.

```
$ ~/.mpm/bin/mpm-telemetry --version
MPM · Telemetry · version

MPM 0.1.0-dev+g3df72315.t1792851233
  binary:  mpm-telemetry
  commit:  3df723156d7649517508212453de72d347759f43
  build:   development
  built:   2026-10-24T14:13:53Z
  dirty:   no
```

The telemetry sidecar reports the **full** embedded identity, exactly
like the other four. `cmd/mpm-telemetry/help.go:writeVersion` calls
`buildinfo.FormatHuman(buildinfo.Current("mpm-telemetry"))`.

What was genuinely missing was the other half of D5: a binary built from
a **Git-free source export** had no history to report, so it reported
commit `unknown`. That is now closed by the producer assertions (§4.4).

### 4.3 The rule, and the resolver

`MPM_WORKSPACE` is the canonical workspace selector. `MPM_DB_PATH` may
only ever **restate** the workspace's own database.

One resolver — `internal/core/config/dbpath.go`,
`config.ResolveTarget` — so there is exactly one place where the answer
is computed and exactly one set of rules:

| `MPM_DB_PATH` | Result |
|---|---|
| unset | the workspace's database |
| names the workspace's database | accepted — it agrees |
| names something else | **explicit error**, naming both paths |

An explicit `--db` with the canonical shape names a **workspace**, and
that workspace is used. A path without the shape is **refused**.

Two selectors that disagree — `--db` and `MPM_WORKSPACE`, or `--db` and
`MPM_DB_PATH` — are a conflict, not a precedence rule to be resolved
silently.

The conflict error names both paths, because the operator's next action
depends on which one they meant:

```
fatal: database path conflict: MPM_DB_PATH names the database of workspace
  /srv/other, but the effective workspace is /home/v/.mpm
  MPM_DB_PATH / --db names : /srv/other/src/db/mpm.db
  workspace /home/v/.mpm resolves to : /home/v/.mpm/src/db/mpm.db
  MPM will not open one database while another is named. Unset MPM_DB_PATH,
  or set MPM_WORKSPACE to the workspace that owns the named database.
```

### 4.4 The Git-free identity — the producer assertions

A source export has no history to ask, so its binaries reported
`unknown`. The producer now **states** the revision it exported:

```
MPM_BUILD_COMMIT=<full sha>   the revision this source was exported from
MPM_BUILD_DIRTY=true|false     the source state of that export
```

The statement is an **assertion**, and it is validated before anything
is stamped:

- `MPM_BUILD_COMMIT` must be a full lowercase hex SHA (40 characters, or
  64 for a SHA-256 repository). A malformed value is refused — a typo
  must not become a commit no operator can verify.
- `MPM_BUILD_DIRTY` must be exactly `true`, `false` or `unknown`.
- When the tree **is** an MPM checkout, the assertion must **agree** with
  it. A contradiction means the builder exported one revision and
  stamped another.

Measured, on a Git-free copy:

```
$ ./scripts/build-provenance.sh commit
unknown
$ MPM_BUILD_COMMIT=4111c46dbe30de44e9ed8f7a715fff9e8c3e469e \
  MPM_BUILD_DIRTY=false ./scripts/build-provenance.sh all
version=0.1.0-dev
commit=4111c46dbe30de44e9ed8f7a715fff9e8c3e469e
short_commit=4111c46d
dirty=false
build_type=development
```

And the refusals:

```
$ MPM_BUILD_COMMIT=notasha ./scripts/build-provenance.sh commit
build-provenance.sh: MPM_BUILD_COMMIT must be a full lowercase hex commit SHA
(40 characters, or 64 for a SHA-256 repository); got 'notasha'
$ MPM_BUILD_COMMIT=4111c46...  # in a checkout whose HEAD differs
build-provenance.sh: MPM_BUILD_COMMIT ('4111c46…') contradicts HEAD ('119586d…')
in /home/v/src/mpm; refusing to stamp a revision the tree does not have
```

### 4.5 The proofs

| Proof | Where |
|---|---|
| The resolver's contract | `internal/core/config/dbpath_test.go` |
| The scheduler refuses a non-canonical `--db`, and never creates `/src/db/mpm.db` | `cmd/mpm-scheduler/db_path_resolution_test.go` |
| The critic refuses the same | `cmd/mpm-critic/db_path_resolution_test.go` |
| The CLI refuses a divergent `MPM_DB_PATH`, and `--version` still answers | `cmd/mpm/db_path_resolution_test.go` |
| The critic subprocess is pinned to the resolved workspace **and** database | `internal/scheduler/critic_handler_env_test.go` |
| All five binaries carry the asserted identity from a Git-free build | `release_acceptance/gitfree_build_identity_test.go` |
| The producer assertions are validated | `scripts/tests/test_build_provenance.py` |

### 4.6 A defect the resolver exposed

The critic-audit handler used to pass only `MPM_DB_PATH` to its
subprocess, so the critic resolved **its own** workspace from the
inherited `MPM_WORKSPACE` — the scheduler's *environment* workspace,
not necessarily the one the scheduler opened. A scheduler pointed at
another workspace with `--db` would have had the critic audit a
**different substrate**. The handler now pins both.

---

## 5. F2 and F6 — the `lifecycle_events` schema

### 5.1 The comparison

The live table and the RC's DDL were compared column by column — name,
declared type, NOT NULL flag, default — plus the CHECK constraints, the
indexes and the triggers.

```
  live: 26 columns   RC: 26 columns
  identical column SET: True
  identical column ORDER: True
```

The live table's four CHECK constraints:

```
CHECK (idempotency IN ('strict','best_effort')
CHECK (trust = 'observation')
CHECK (operation IN ('present','absent')
CHECK (result_status IN ('success','error','unknown')
```

The RC's DDL has the same four. The only textual difference is the
quoting of the `meta` default (`'{}'` against `{}`).

**The live table is byte-compatible with the RC's DDL, and the RC binary
accepts it unchanged.**

### 5.2 The five representative schemas

`scripts/rehearsal/30_schemas.sh` opens a **copy** of each shape with
the new binary and asserts that the open succeeds, that nothing
pre-existing is lost, that no row count moves, that the new schema is
present afterwards, and that the result passes `integrity_check`.

| Case | Shape | Result |
|---|---|---|
| **S1** | the live shape | Accepted **unchanged**. No table lost, no row count moved, `integrity_check` ok. |
| **S2** | the pre-incident shape (no `lifecycle_events`) | The schema advances correctly. |
| **S3** | a fresh database | The RC's own bootstrap. |
| **S4** | an empty file (0 bytes) | The full bootstrap. |
| **S5** | an unrelated table named `lifecycle_events` | The unrelated table's **content is preserved** — and the RC's own schema is **not** applied. |

**25 assertions, 0 failures.**

### 5.3 F6 — the `IF NOT EXISTS` hazard

S5 is a latent silent incompatibility. `CREATE TABLE IF NOT EXISTS`
preserves an unrelated table of the same name: the content survived, the
row was intact — and the substrate never got the schema it expected.

```
  columns now:
    id TEXT
    note TEXT
  rows now:
    ('x', 'an unrelated table that happens to share the name')
```

This is **not a repair**. The migration should detect the case and
**refuse**, rather than proceed with a substrate that is missing its
schema. It is recorded as risk R5 in the plan, and the detection is
still pending.

---

## 6. F7 — the backup-mutation incident

The rehearsal's own first implementation took its SQL dump with the
**staged (newer) binary** against the **live** database — and mutated
it. A newer binary opens a database by applying whatever migrations it
believes are pending, so taking a "backup" was an unannounced
migration.

The rehearsal caught it: the driver's own state fingerprint changed
across the backup step.

**The rule that came out of it:** the live database is opened **only**
through the SQLite **online backup API**, in read-only mode; the SQL
dump is taken from the **verified backup copy**; and the staged binary's
`health_check` runs against a **scratch copy**.

The rehearsal driver now does exactly that, and the scratch spaces are
canonical workspaces — the database at `<scratch>/src/db/mpm.db` — which
is both what the substrate supports and what an operator would use.

---

## 7. The rehearsal results

Reproduced in-repo, from the working tree, at revision `8f9839bf`:

```
suite=simulate    passed=0  failed=0 (builder)
suite=obligations passed=56 failed=0
suite=negative    passed=54 failed=0
suite=schemas     passed=25 failed=0

REHEARSAL PASSED — 135 assertions, 0 failures
```

### 7.1 What the obligations prove

- The migration completes with the **state fingerprint unchanged**.
- The bundle carries **no `.git`**, and the staged binary reports the
  staged revision.
- The runtime executes `git` **zero times** — the strace recorded 0
  `execve` calls to git.
- The operator's mode, persona and drill definitions survive.
- The OpenCode registration and the systemd unit are **not** rewritten.
- The services start in isolation; the MCP server answers `initialize`
  and lists its tools.
- The identity format is `0.1.0-dev+g<short-commit>[.dirty].t<epoch>`,
  and the `.dirty` component appears **exactly** when the tree was dirty.

### 7.2 What the negative suite proves

- A bundle with a failed binary is rejected **before** promotion.
- A failed promotion rolls back to the previous release.
- A bundle without the OpenCode payload is refused.
- A tampered bundle is refused.
- The harness's own contents are fingerprinted before the first test and
  checked after every one.

### 7.3 The release gate

At the head of this work, on `feat/runtime-migration-design`:

| Gate | Result |
|---|---|
| `make test` (the full suite: root + internal/core + scheduler + critic + tools + scripts + agent_installation) | **green** |
| `make rehearsal REHEARSAL_REV=HEAD` | **135 assertions, 0 failures** |
| `go test -race` on every changed package (`internal/core`, `internal/core/config`, `internal/scheduler`, `cmd/mpm`, `cmd/mpm-scheduler`, `cmd/mpm-critic`) | **green** |
| `mpm-lint --gate` | **PASS** on every class |
| `scripts/tests/` (559 tests) and `agent_installation/tests/` (456 tests) | **green** |

### 7.4 The live installation, afterwards

```
$ systemctl --user show mpm-scheduler -p NRestarts -p MainPID
MainPID=2804
NRestarts=0
$ ~/.mpm/bin/mpm --version | head -1
MPM 0.1.0-dev+g3df72315.t1792851233
$ ls -d ~/.mpm/.git
/home/v/.mpm/.git
```

**Unchanged**: same pid, no restarts, the same installed revision, `.git`
intact, the host registrations untouched, the live database never opened
for writing.

---

## 8. What is still open

| Item | Why it is still open |
|---|---|
| The `lifecycle_events` collision detection | The hazard is proven (S5); the refusal is not implemented. |
| The `install.sh` bundle phase and the pointer swap | PR 3 of the sequence. |
| The transactional promotion retargeted at `$BUNDLE/bin` | PR 1 of the sequence. |
| `agent_installation/scripts/stage_runtime_package.sh`: the bundle root and the data root as separate helpers | PR 1 of the sequence. |
| The `uninstall.sh` runtime-only path | PR 3 of the sequence. |
| `MPM_EVENT_INGEST`'s default | Session C's call, on #142. |
| The telemetry store path | `mpm-telemetry observe` (#139) needs `<workspace>/telemetry.db` against `<workspace>/src/db/telemetry.db` settled. |
