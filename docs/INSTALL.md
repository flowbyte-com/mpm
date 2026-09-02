# INSTALL.md

> **Audience:** both humans and agents. Prose explains *why*. Commands are the
> *what*. Both finish the section with the same understanding.
>
> **TL;DR:** `./scripts/install.sh` — done. No sudo required.

Install the MPM cognitive substrate in your user context. Result: a
production-grade agent stack with everything in `$HOME`, isolated from
other users on the host, secured at 0700/0600 by the binary's startup
gate.

For background and design rationale, see [README.md](README.md).

---

## Choose your install path

| Path | Sudo? | Data root | Use when |
|------|-------|-----------|----------|
| **`./scripts/install.sh`** (recommended) | no | `$HOME/.mpm` | Default. Multi-tenant hosts, personal machines, anything where state should be isolated to the operator. Linger enables the scheduler to survive logout. |
| System mode (`sudo ./scripts/install.sh --system`) | yes | `/var/lib/mpm` | Dedicated headless VMs where shared system state is intentional. Uses a system systemd unit (no linger needed). |

**Pick the first unless you specifically need shared system state.**

---

## Prerequisites

| Requirement | Verify with | Pass criterion |
|---|---|---|
| Linux with systemd (any user) | `systemctl --version` | systemd ≥ 240 |
| Go 1.26+ | `go version` | version ≥ go1.26 |
| Node.js (host tooling only) | `node -v` | version ≥ v22 |
| LLM API key | configured in host config | auth block present |

`sudo` is **not required** for the default install. The system-mode
install (`--system`) does require root; use it only when you specifically
need a system-wide service.

---

## 1. Recommended install: `./scripts/install.sh`

> **Why this is the default:** a single command, no sudo, runs entirely in
> your user context. The script detects existing legacy system services and
> offers to tear them down before installing the new user-space one. Handles
> idempotent re-runs, MCP registration, lingering enable, and post-install
> validation. The script logs each phase and fails loudly on any error.

### 1a. Clone and install

```bash
git clone https://github.com/flowbyte-com/mpm ~/projects/mpm
cd ~/projects/mpm
./scripts/install.sh
```

What the script does, in order:
1. **Preflight** — checks Go, systemd, project layout. Detects legacy
   `/etc/systemd/system/mpm-scheduler.service` and offers to disable it
   (avoids split-brain dual-scheduler scenario for upgrading testers).
   Detects legacy data at `/var/lib/mpm/mpm.db` and warns about migration.
2. **Build** — `make build` produces all five binaries
3. **Binaries** — installs `mpm-scheduler`, `mpm-critic`, `mpm-mcp`, `mpm-telemetry` to
   `$HOME/.local/bin/`. Installs `mpm.real` and a workspace wrapper at
   `$HOME/.local/bin/mpm`
4. **Data directory** — creates `$HOME/.mpm/{src/db,backups/critic-pre}`.
   Runtime perms are tightened to 0700/0600 by the binary's startup gate
5. **Systemd service (user)** — installs
   `$HOME/.config/systemd/user/mpm-scheduler.service`, enables lingering
   via `loginctl enable-linger`, enables and starts the service
6. **Host integration** — if OpenClaw is detected, registers `mpm` MCP server
   with `MPM_WORKSPACE=$HOME/.mpm` and restarts the gateway
7. **Validation** — verifies service active (user scope), CLI health check
   passes, MCP points at correct workspace

### 1b. Validate

```bash
./scripts/install.sh --validate       # same checks the install script runs
```

Or manually:

```bash
systemctl --user status mpm-scheduler                    # expect: active
~/.mpm/bin/mpm call mpm_system --payload '{"action":"health_check","params":{}}'        # expect: "ok":true
journalctl --user -u mpm-scheduler -n 20 --no-pager     # expect: "scheduler running"
```

### 1c. Bootstrap cognitive state

```bash
~/.mpm/bin/mpm ops init directives          # seed prime directives (idempotent)
~/.mpm/bin/mpm status                      # verify DB reachable
~/.mpm/bin/mpm call mpm_context --payload '{"action":"read_wake_context","params":{}}'        # first agent tool call
```

`ops init directives` is the *only* command that touches cognitive state during
install. It seeds prime directives (wake protocol, canonical DB path, etc.)
into the database. Re-running is safe — local edits are preserved.

### 1d. Script modes reference

| Mode | Purpose |
|------|---------|
| `(default)` | Full install (user-space) |
| `--check` | Preflight only — verify environment, no changes |
| `--dry-run` | Print intended actions, no changes |
| `--validate` | Post-install validation (read-only) |
| `--uninstall` | Remove installed artifacts. Data at `$HOME/.mpm/` is preserved |
| `--system` | Use legacy `/var/lib/mpm` + system systemd (requires sudo) |
| `--prefix <path>` | Override install prefix (default `$HOME/.local/bin`) |
| `--data-root <path>` | Override data root (default `$HOME/.mpm`) |
| `--user <name>` | Override target user (default: current user) |
| `--yes` | Skip confirmation prompts (auto-disable legacy unit when detected) |

```bash
./scripts/install.sh --check           # safe, no changes
./scripts/install.sh --dry-run         # show what would happen
./scripts/install.sh --uninstall       # remove artifacts (data preserved)
sudo ./scripts/install.sh --system     # legacy /var/lib/mpm + system service
```

---

## 2. Legacy / opt-in: system install (`--system`)

> **Skip this section unless** you specifically need shared system state —
> for example, a dedicated headless VM running MPM under a service
> account. For personal machines, multi-tenant hosts, and most alpha
> use cases, the user-space install (Section 1) is correct.
>
> **Why this is opt-in:** the system install puts MPM state at
> `/var/lib/mpm` with root-owned systemd units. That's the right shape
> for production servers but the wrong shape for personal / multi-tenant
> use where state should be isolated to a single user. The alpha
> baseline defaults to user-space for security reasons.
>
> Operators who specifically need `/var/lib/mpm` (e.g., shared across
> multiple service accounts on the same host) use this path.

### 2a. System install (requires sudo)

```bash
cd ~/projects/mpm
sudo ./scripts/install.sh --system
```

This is identical to the default install but:
- Writes binaries to `/usr/local/bin/` (the ONLY mode that does this)
- Creates `/var/lib/mpm/{src/db,backups/critic-pre}` (instead of `$HOME/.mpm/`)
- Installs the system unit to `/etc/systemd/system/mpm-scheduler.service`
- Uses plain `systemctl` (no `--user`)
- Does NOT call `loginctl enable-linger` (not needed for system services)
- Requires root at every step

You can also pass `MPM_SYSTEM=1` instead of `--system`:

```bash
sudo MPM_SYSTEM=1 ./scripts/install.sh
```

### 2b. Validate

```bash
sudo ./scripts/install.sh --validate       # re-run from any mode
```

Or manually:

```bash
sudo systemctl status mpm-scheduler                       # expect: active
sudo /usr/local/bin/mpm call mpm_system --payload '{"action":"health_check","params":{}}'  # expect: "ok":true
sudo journalctl -u mpm-scheduler -n 20 --no-pager         # expect: "scheduler running"
```

**Migrating from system to user-space** (you've decided the system
install was wrong): uninstall first (`sudo ./scripts/install.sh --uninstall`),
then run the default install (`./scripts/install.sh`) which copies
data over (it does NOT — see Section 4 for the manual migration recipe).
The default install's preflight detects the legacy system unit and
offers to tear it down before proceeding.

### 2c. Override paths

Use a drop-in override:

```bash
systemctl --user edit mpm-scheduler
# writes to ~/.config/systemd/user/mpm-scheduler.service.d/override.conf
```

Or write `~/.config/mpm/mpm.env`:

```bash
MPM_DB_PATH=/custom/path/mpm.db
MPM_BACKUP_DIR=/custom/path/backups
```

---

> **Lazy-Start Architecture (encrypted `/home`).** When `/home` is eCryptfs-encrypted,
> the scheduler daemon is **designed to stay dead at boot**. The lockfile lives inside
> the encrypted tree (`~/.mpm/scheduler.lock`); starting the daemon before `/home`
> is decrypted would either fail (inaccessible path) or risk writing to the wrong
> location. The architecture treats *boot + locked home* as the SAFE state and expects
> the agent's first wake context (`AGENTS.md` Session Startup step 2) to spin the
> daemon up *after* decryption is complete. This isolates the daemon's first write
> to a moment when the substrate is verifiably writable. **It is a security feature,
> not a bug.** Lesson `24be03ec71a5981f` codifies the rationale.
>
> Operators on systems without an agent wake path (cron-driven unattended tasks,
> headless deployments) can opt out by adding the drop-in documented in the
> Troubleshooting row below.

### 2.1. Agentic Cron (recurring tasks, optional)

> **Skip this subsection if:** self-scheduled one-off wakes via `schedule_wake`
> are enough. The Agentic Cron adds a registry of recurring tasks the daemon
> polls on its 60s tick — nightly compactions, weekly security audits, etc.
> Single-agent workflows that don't need a fixed cadence can skip this.

A fresh MPM install (since the 2026-09-01 productization pass) arrives
with one canonical scheduled task pre-configured:

| Task ID | Cron (UTC) | Directive | What it does |
|---|---|---|---|
| `epistemic-compaction` | `0 3 * * *` (daily 03:00) | `mpm-seed-epistemic-compaction-policy` | Wakes the agent. The agent reads the directive and invokes `mpm_system.compact` with `force=false`, `max_batches=5`. |

This is the **agent-owned** reflex to `epistemic_pressure.exceeded=true`.
The scheduler only delivers the wake — it does not invoke compaction
itself. The substrate measures pressure, the agent runs the reflex, the
scheduler transports the wake. The boundary is preserved end-to-end.

**Why daily 03:00 UTC.** The 19-day empirical gap (2026-08-13 →
2026-09-01) where no compact invocation occurred produced no measurable
substrate harm: 99% compaction ratio held, query latencies stayed
nominal, FTS5 indexes were unaffected. The reflex does not need to be
aggressive. Daily 03:00 UTC is the documented cadence and the seed
default. Operators with different ingest profiles can re-upsert to a
custom cadence (see "Custom cadence" below).

**Custom cadence by ingest profile.** The seed default is the
conservative middle. Re-upsert the task with a different cron if the
default doesn't fit your workload:

| Profile | Suggested cron | Reasoning |
|---|---|---|
| High ingest (≥500 raw/day) | `0 */4 * * *` (every 4h) | Pressure may exceed threshold between daily cycles; shorter cycles keep `raw_count` near the threshold without crossing it. |
| Normal / default | `0 3 * * *` (daily 03:00 UTC) | Low-noise window for a background LLM reflection pass. Matches the seed default. |
| Low ingest (<50 raw/week) | `0 3 * * 0` (weekly Sunday) | Threshold rarely crossed; weekly is enough. |

Inspect or change the current schedule:

```bash
mpm tasks list                          # see all tasks and their cron
mpm tasks upsert epistemic-compaction \
  "Nightly epistemic compaction" \
  "0 */4 * * *" \
  mpm-seed-epistemic-compaction-policy active
```

**Idempotency and operator customization.** Re-running the seed
(`mpm ops init tasks`, or restarting any `mpm` binary — boot calls
the same apply loop) NEVER overwrites a customized row. If the
operator paused the task or changed the cron, the seed reports
`Skipped` and leaves the row verbatim. The seed is a starting
configuration, not a sync target. To upgrade a canonical row, the
operator must `mpm tasks upsert` it explicitly with the new
parameters.

Once the daemon is running, recurring tasks are managed via `mpm tasks`:

```bash
mpm tasks upsert <id> <name> <cron_expr> <directive_id> [status]
mpm tasks list
mpm tasks delete <id>    # or pass 'rm'; prefer status='paused' for soft-stop
```

**Compact contract (what the directive tells the agent to do).** The
`mpm_system.compact` call has explicit force/threshold/batch semantics:

- `force=false` (default) — RELIEVE pressure. The drain stops as soon
  as `raw_count <= threshold`. Eligible raw memories may remain after
  the call. This is the safe reflex.
- `force=true` — DRAIN everything. The threshold gate is bypassed;
  the drain continues until the substrate is empty or the per-
  invocation safety cap is hit. Use only for explicit operator or
  agent action, not from a scheduled wake.
- `max_batches` (default 20, hard cap 100) — per-invocation safety
  cap on LLM calls. 20 × 50 = 1000 raw memories per invocation. The
  seed-directive default is 5, which keeps the per-wake LLM cost
  bounded for the common case.

Inspect the response's `stop_reason` and `raw_remaining`:
- `no_work` — no eligible raw memories (below threshold or all consumed).
- `completed` — eligible rows consumed; `raw_remaining` may be > 0 if
  ineligible rows remain (the seed default).
- `threshold_reached` — pressure relieved; eligible rows may remain
  below the gate.
- `max_batches_reached` — safety cap hit; the next scheduled cycle
  resumes the work.
- `failure` — mid-drain batch failure; earlier batches are durable;
  the next cycle resumes the remaining work.

`success=true` does NOT mean `raw_remaining == 0`. `success=false`
is set ONLY on a mid-drain batch failure.

**Example: nightly epistemic compaction at 03:00 UTC (manual override).** The directive_id

must already exist in `memories` where `collection='directives'` — the
handler runs a fail-fast index lookup before writing, so a typo is caught
at upsert time, not at 3 AM as a silent wake drop.

```bash
# Create the directive first (one-time)
mpm call mpm_memory --payload '{
  "action": "save",
  "params": {
    "fact": "Compact last week'"'"'s memories tagged \"scratchpad\" into a durable lesson. Shred the originals.",
    "collection": "directives",
    "tags": ["prime_directive", "epistemic-compaction", "2026-07-23"]
  }
}'    # note the returned id, e.g. abc123...

# Then schedule the task
mpm tasks upsert epistemic-compaction \
  "Nightly epistemic compaction" \
  "0 3 * * *" \
  abc123... active
```

The daemon's tick loop polls `scheduled_tasks WHERE status='active' AND
next_run_at <= ?` and, for each due task, injects a standard `scheduled_wakes`
row in the same transaction as the `next_run_at` rollover. A daemon crash
between injection and rollover cannot double-fire. The agent sees the
injected wake on its next MCP call and reads the directive.

**Three MCP tools** mirror the CLI for agent-driven automation:
`upsert_scheduled_task`, `list_scheduled_tasks`, `delete_scheduled_task`.
Architecture and edge cases (re-upsert semantics, poison-pill handling,
why pre-compute `next_run_at`) documented in [README §9.3](README.md#agentic-cron-recurring-tasks).

## 3. Wire to your host

> **Pick one.** OpenClaw and Hermes are the two supported hosts as of
> 2026-07-18. The recommended install (Section 1) handles OpenClaw
> automatically. Re-run `sudo ./scripts/install.sh` after switching hosts.

### 3a. OpenClaw (auto-wired by install script)

The install script detects OpenClaw and registers the MCP server automatically.
If you skipped that step or need to re-register manually:

```bash
openclaw mcp add mpm \
  --command /usr/local/bin/mpm-mcp \
  --env MPM_WORKSPACE=/var/lib/mpm

openclaw gateway restart                  # gateway caches MCP servers at startup
```

> **Note:** `MPM_ACTIVE_MODE` and `MPM_ACTIVE_PERSONA` are intentionally
> not set here. MPM resolves both from env at request time via
> `internal/core/mpmcli.ActiveContextFromEnv()` and applies its own
> `default`/`default` contract when unset. Hardcoding framework-specific
> defaults at MCP-wiring time was a pre-2026-08-29 drift that leaked
> old mode taxonomy into every registration. To pin a non-default mode
> for this OpenClaw session, set the env vars on the gateway process
> itself, not on the MCP server entry.

> **Important:** `openclaw mcp add` is a silent no-op if `mpm` is already
> registered. To update an existing registration, use `set`:
> ```bash
> openclaw mcp set mpm '{"command":"/usr/local/bin/mpm-mcp","env":{"MPM_WORKSPACE":"/var/lib/mpm"}}'
> ```

**Validate:**

```bash
openclaw mcp show mpm | grep MPM_WORKSPACE   # expect: "/var/lib/mpm"
openclaw mcp doctor mpm --probe              # expect: probe passes
```

### 3b. Hermes

```bash
hermes mcp add mpm \
  --command /usr/local/bin/mpm-mcp \
  --env MPM_WORKSPACE=/var/lib/mpm \
  --connect-timeout 15
```

Edit `~/.hermes/config.yaml`:

```yaml
mcp_servers:
  mpm:
    command: /usr/local/bin/mpm-mcp
    env:
      MPM_WORKSPACE: /var/lib/mpm
    timeout: 60
    connect_timeout: 30
    enabled: true
```

**Validate:**

```bash
hermes mcp test mpm                    # expect: Connected, 61 tools discovered
hermes mcp list | grep mpm             # expect: mpm ... ✓ enabled
```

---

## 4. Uninstall

```bash
sudo ./scripts/install.sh --uninstall
```

Removes: binaries, wrapper, systemd unit. **Preserves:** `/var/lib/mpm/`
data, source code at `~/projects/mpm`, legacy user unit (if any).

To remove data too:

```bash
sudo rm -rf /var/lib/mpm
```

---

## 5. Path cheat sheet

| Path | Owner | Purpose |
|------|-------|---------|
| `/usr/local/bin/mpm` | root | Wrapper script (sets MPM_WORKSPACE, exec's mpm.real) |
| `/usr/local/bin/mpm.real` | root | The actual mpm CLI binary |
| `/usr/local/bin/mpm-mcp` | root | MCP server stdio binary |
| `/usr/local/bin/mpm-scheduler` | root | Scheduler daemon |
| `/usr/local/bin/mpm-critic` | root | Memory critic binary |
| `/etc/systemd/system/mpm-scheduler.service` | root | System service unit |
| `/var/lib/mpm/src/db/mpm.db` | $USER | SQLite database |
| `/var/lib/mpm/backups/critic-pre/` | $USER | Pre-critic DB snapshots |
| `/var/lib/mpm/scheduler.lock` | $USER | flock singleton lock |
| `~/projects/mpm/` | $USER | Source code (NOT runtime data) |

The split between `/usr/local/bin/` (binaries), `/var/lib/mpm/` (runtime data),
and `~/projects/mpm/` (source code) is intentional. Source code can be wiped
without losing agent state; runtime data persists across `git pull`.

---

## 6. Troubleshooting

| Symptom | Diagnose with | Fix |
|---------|---------------|-----|
| Install fails: "Go not found" | `go version` | Install Go 1.26+ or add to PATH |
| Install fails: "systemd required" | `systemctl --version` | Install systemd (most distros have it) |
| `systemctl --user` fails with "Failed to connect to bus" | `loginctl show-user $USER --property=Linger` | `sudo loginctl enable-linger $USER` (set `Linger=yes`) |
| Service won't start: "permission denied" on `/var/lib/mpm/` | `ls -la /var/lib/mpm/` | `sudo chown -R $USER:$USER /var/lib/mpm` |
| Service won't start after reboot on encrypted home | `findmnt /home` | Use `sudo ./scripts/install.sh` (system service) instead of `make service-scheduler` (user service) |
| `mpm-scheduler`: DB not found in logs | `systemctl --user show mpm-scheduler -p Environment` | Set `MPM_DB_PATH` in `~/.config/mpm/mpm.env`, or `systemctl --user edit mpm-scheduler` |
| `mpm-scheduler` stays `inactive` after reboot on encrypted `/home` (this is expected) | `systemctl --user is-active mpm-scheduler` returns `inactive`; `journalctl --user -u mpm-scheduler` shows no entries since boot | This is **expected behaviour** under the Lazy-Start Architecture — see INSTALL §2. The daemon is designed to stay dead at boot when `/home` is encrypted (the lockfile inside the encrypted tree would be inaccessible otherwise). On the next agent wake, `AGENTS.md` Session Startup step 2 detects the dead daemon and starts it post-decryption. If your workload runs unattended with no agent wake path (cron / system timers only), opt out by adding a drop-in: `systemctl --user edit mpm-scheduler` → under `[Service]` add `ExecStartPre=/bin/bash -c 'until mountpoint -q $HOME; do sleep 1; done'` to delay-start until the mount is up. |
| CLI fails: "no such file: mpm.real" | `ls -la /usr/local/bin/mpm*` | Re-run `sudo ./scripts/install.sh` to restore the wrapper |
| CLI reads from wrong DB (e.g. `~/projects/mpm/src/db/mpm.db`) | `which mpm`; `head -1 /usr/local/bin/mpm` | `/usr/local/bin/mpm` must be a wrapper (`#!/bin/sh`), not the raw binary. Re-run install. |
| Spawn ENOENT when host tries to launch mpm-mcp | `ls -l $HOME/projects/mpm/bin/mpm-mcp` | If missing: `make build`. If not executable: `chmod +x`. Then re-register with correct path. |
| MCP tools return data, but writes don't persist | `openclaw mcp show mpm` | Check `MPM_WORKSPACE` matches the canonical path; restart gateway |
| `openclaw mcp add mpm` is a silent no-op | `openclaw mcp list` | Server already exists — use `openclaw mcp set mpm '<json>'` instead |
| OpenClaw: agent doesn't see MPM tools in chat | `openclaw mcp list \| grep mpm` | `openclaw gateway restart` (Gateway caches MCP servers at startup) |
| Hermes: agent doesn't see MPM tools in chat | `hermes mcp list \| grep mpm` | Confirm `mcp` toolset in `~/.hermes/config.yaml:toolsets`. Restart session (`/reset`) — config changes don't apply mid-conversation. |
| `mpm ops init directives` errors: "no such table: directives" | `mpm status` | Schema not initialized. Run `mpm status` first to init, then re-run `mpm ops init directives`. |
| MCP tools load but `read_wake_context` returns empty | `mpm ops stats` | DB may be empty. Confirm `MPM_WORKSPACE` matches the canonical db path prime directive. |
| Legacy user unit conflicts with system unit | `systemctl --user status mpm-scheduler`; `systemctl status mpm-scheduler` | Disable the legacy one: `systemctl --user disable --now mpm-scheduler; rm ~/.config/systemd/user/mpm-scheduler.service` |

---

## `_epistemic_snapshot` configuration

Default values (override via `system_config`):

| Key | Default | Meaning |
|---|---|---|
| `epistemic_snapshot.max_observation_window_ms` | `60000` | Drop tool calls older than this from `provenance` |
| `goal_snapshot` overflow cap | `200` chars | Reject (don't truncate) on overflow |
| `provenance.uri` overflow cap | `2048` chars | Reject (don't truncate) on overflow |

Override example:

```bash
mpm ops config set epistemic_snapshot.max_observation_window_ms 90000
```

**Backfill for legacy databases** (alpha testers importing databases
that pre-date this release):

```bash
mpm ops maintain backfill-snapshots
```

Derives `creator` (from existing `created_by` field) and `validation`
(from the evidence table) for every existing memory. Other sub-blocks
(`execution`, `provenance`, `context`) are unrecoverable for memories
saved before the resolver existed — those rows carry partial snapshots.

---

## See also

- [README.md](README.md) — cognitive model, design, full reference
- [scripts/install.sh](scripts/install.sh) — the install script (read the source)
- [scripts/](scripts/) — utility scripts (smoke tests, completion, etc.)
- [contrib/systemd/](contrib/systemd/) — unit file templates (system + user)
- [OpenClaw docs](https://docs.openclaw.ai) — platform reference
- [Hermes Agent docs](https://hermes-agent.nousresearch.com/docs) — runtime reference
