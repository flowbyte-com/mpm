# INSTALL.md

> **Audience:** both humans and agents. Prose explains *why*. Commands are the
> *what*. Both finish the section with the same understanding.
>
> **TL;DR:** `sudo ./scripts/install.sh` — done.

Install the MPM cognitive substrate and wire it to your host. Result: a
production-grade agent stack that survives reboots and encrypted home directories.

For background and design rationale, see [README.md](README.md).

---

## Choose your install path

| Path | Sudo? | Survives encrypted home? | Use when |
|------|-------|--------------------------|----------|
| **`sudo ./scripts/install.sh`** (recommended) | yes | yes | Default. Hosts with eCryptfs/LUKS, production use, anything that needs to outlive your login session |
| Legacy user-level (`make service-scheduler`) | no | **no** | No-sudo environments, throwaway containers. Fails silently on encrypted home directories. |

**Pick the first unless you have a specific reason not to.**

---

## Prerequisites

| Requirement | Verify with | Pass criterion |
|---|---|---|
| Linux with systemd | `systemctl --version` | systemd ≥ 240 |
| Go 1.26+ | `go version` | version ≥ go1.26 |
| Node.js (host tooling only) | `node -v` | version ≥ v22 |
| LLM API key | configured in host config | auth block present |
| sudo / root access | `sudo -n true` | exits 0 |

The `enable-linger` step is **no longer required** — the recommended install
uses a system-level service that boots with the machine, not with your login.

---

## 1. Recommended install: `sudo ./scripts/install.sh`

> **Why this is the default:** a single command that does the right thing on
> every host. Handles encryption detection, idempotent re-runs, MCP registration,
> and post-install validation. The script logs each phase and fails loudly on
> any error.

### 1a. Clone and install

```bash
git clone https://github.com/flowbyte-com/mpm ~/projects/mpm
cd ~/projects/mpm
sudo ./scripts/install.sh
```

What the script does, in order:
1. **Preflight** — detects home encryption (warns if eCryptfs/LUKS), checks
   sudo, Go, systemd, project layout
2. **Build** — `make build` produces all four binaries
3. **Binaries** — installs `mpm-scheduler`, `mpm-critic`, `mpm-mcp` to
   `/usr/local/bin/`. Installs `mpm.real` and a workspace wrapper at
   `/usr/local/bin/mpm`
4. **Data directory** — creates `/var/lib/mpm/{src/db,backups/critic-pre}`,
   owned by the invoking user
5. **Systemd service** — installs `/etc/systemd/system/mpm-scheduler.service`,
   enables and starts it
6. **Host integration** — if OpenClaw is detected, registers `mpm` MCP server
   with `MPM_WORKSPACE=/var/lib/mpm` and restarts the gateway
7. **Validation** — verifies service active, CLI health check passes, MCP
   points at correct workspace

### 1b. Validate

```bash
sudo ./scripts/install.sh --validate     # same checks the install script runs
```

Or manually:

```bash
systemctl status mpm-scheduler                       # expect: active
/usr/local/bin/mpm call health_check --payload '{}'  # expect: "ok":true
journalctl -u mpm-scheduler -n 20 --no-pager         # expect: "scheduler running"
```

### 1c. Bootstrap cognitive state

```bash
/usr/local/bin/mpm ops init directives    # seed prime directives (idempotent)
/usr/local/bin/mpm status                # verify DB reachable
/usr/local/bin/mpm call read_wake_context   # first agent tool call
```

`ops init directives` is the *only* command that touches cognitive state during
install. It seeds prime directives (wake protocol, canonical DB path, etc.)
into the database. Re-running is safe — local edits are preserved.

### 1d. Script modes reference

| Mode | Purpose |
|------|---------|
| `(default)` | Full install |
| `--check` | Preflight only — verify environment, no changes |
| `--dry-run` | Print intended actions, no changes |
| `--validate` | Post-install validation (read-only) |
| `--uninstall` | Remove installed artifacts. Data at `/var/lib/mpm/` is preserved |
| `--prefix <path>` | Override install prefix (default `/usr/local`) |
| `--data-root <path>` | Override data root (default `/var/lib/mpm`) |

```bash
sudo ./scripts/install.sh --check       # safe, no changes
sudo ./scripts/install.sh --dry-run     # show what would happen
sudo ./scripts/install.sh --uninstall   # remove artifacts (data preserved)
```

---

## 2. Legacy / opt-in: user-level install

> **Skip this section unless** you cannot use sudo (shared host, restricted
> container) and you are NOT on an encrypted home directory.
>
> **Why this is opt-in:** systemd user services start at session/login via
> `loginctl enable-linger`. On hosts with encrypted home (eCryptfs/LUKS),
> `user@UID.service` starts at boot but cannot read binaries in
> `/home/$USER/` until login + decrypt. The mpm-scheduler unit silently
> fails to start and `Restart=on-failure` does not recover (the failure
> is during initial `ExecStart`, not runtime).
>
> See lesson `071911bc` in MPM for the full postmortem.

### 2a. User-level install (no sudo)

```bash
cd ~/projects/mpm
make build                              # produces bin/mpm, bin/mpm-mcp, bin/mpm-scheduler, bin/mpm-critic
sudo make install                       # binaries to /usr/local/bin (sudo needed for system paths)
sudo loginctl enable-linger $USER       # required for user-level autostart
make service-scheduler                  # copies unit to ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now mpm-scheduler
```

### 2b. Validate

```bash
systemctl --user is-active mpm-scheduler    # expect: active
loginctl show-user $USER --property=Linger  # expect: Linger=yes
```

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

Once the daemon is running, recurring tasks are managed via `mpm tasks`:

```bash
mpm tasks upsert <id> <name> <cron_expr> <directive_id> [status]
mpm tasks list
mpm tasks delete <id>    # or pass 'rm'; prefer status='paused' for soft-stop
```

**Example: nightly epistemic compaction at 03:00 UTC.** The directive_id
must already exist in `memories` where `collection='directives'` — the
handler runs a fail-fast index lookup before writing, so a typo is caught
at upsert time, not at 3 AM as a silent wake drop.

```bash
# Create the directive first (one-time)
mpm call save_to_memory --payload '{
  "fact": "Compact last week's memories tagged \"scratchpad\" into a durable lesson. Shred the originals.",
  "collection": "directives",
  "tags": ["prime_directive", "epistemic-compaction", "2026-07-23"]
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
  --env MPM_WORKSPACE=/var/lib/mpm \
  --env MPM_ACTIVE_MODE=programming \
  --env MPM_ACTIVE_PERSONA=correspondent

openclaw gateway restart                  # gateway caches MCP servers at startup
```

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

## See also

- [README.md](README.md) — cognitive model, design, full reference
- [scripts/install.sh](scripts/install.sh) — the install script (read the source)
- [scripts/](scripts/) — utility scripts (smoke tests, completion, etc.)
- [contrib/systemd/](contrib/systemd/) — unit file templates (system + user)
- [OpenClaw docs](https://docs.openclaw.ai) — platform reference
- [Hermes Agent docs](https://hermes-agent.nousresearch.com/docs) — runtime reference
