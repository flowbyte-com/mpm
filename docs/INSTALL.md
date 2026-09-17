# INSTALL.md

> **Audience:** both humans and agents. Prose explains *why*. Commands are the
> *what*. Both finish the section with the same understanding.
>
> **TL;DR:** `./mpm/install.sh` — done. No sudo required.

Install the MPM cognitive substrate in your user context. Result: a
production-grade agent stack with everything in `$HOME`, isolated from
other users on the host, secured at 0700/0600 by the installer at
provisioning time and re-tightened by the binary's startup gate on every
boot (defence in depth).

For background and design rationale, see [README.md](../README.md).

---

## Prerequisites

| Requirement | Verify with | Pass criterion |
|---|---|---|
| Linux with systemd (any user) | `systemctl --version` | systemd ≥ 240 |
| Go 1.26+ | `go version` | version ≥ go1.26 |
| Node.js (host tooling only) | `node -v` | version ≥ v22 |
| LLM API key | configured in host config | auth block present |

`sudo` is **not required**. The installer runs entirely in your user
context — no `/var/lib/mpm`, no `/etc/systemd/system` writes, no system
state mutations outside `$HOME`. The legacy `--system` install path
has been removed.

---

## 1. Recommended install: `./mpm/install.sh`

> **Why this is the default:** a single command, no sudo, runs entirely in
> your user context. The script detects existing legacy system services and
> offers to tear them down before installing the new user-space one. Handles
> idempotent re-runs, MCP registration, lingering enable, and post-install
> validation. The script logs each phase and fails loudly on any error.

### 1a. Clone and install

```bash
git clone https://github.com/flowbyte-com/mpm ~/projects/mpm
cd ~/projects/mpm
./mpm/install.sh
```

What the script does, in order:
1. **Preflight** — checks Go, systemd, project layout. Detects legacy
   `/etc/systemd/system/mpm-scheduler.service` and offers to disable it
   (avoids split-brain dual-scheduler scenario for upgrading testers).
   Detects legacy data at `/var/lib/mpm/mpm.db` and warns about migration.
2. **Build** — `make build` produces all five binaries
3. **Binaries** — installs `mpm-scheduler`, `mpm-critic`, `mpm-mcp`, `mpm-telemetry` to
   `$HOME/.mpm/bin/`. Installs `mpm.real` and a workspace wrapper at
   `$HOME/.mpm/bin/mpm`
4. **Data directory** — creates `$HOME/.mpm/{src/db,backups/critic-pre}` at
   mode 0700 (installer-enforced; a pre-existing permissive directory is
   hardened on re-install). The binary's startup gate additionally
   tightens the data root to 0700 and files inside `src/db/` and
   `backups/` to 0600 — defence in depth.
5. **Systemd service (user)** — installs
   `$HOME/.config/systemd/user/mpm-scheduler.service`, enables lingering
   via `loginctl enable-linger`, enables and starts the service
6. **Host integration** — if OpenClaw is detected, registers `mpm` MCP server
   with `MPM_WORKSPACE=$HOME/.mpm` and restarts the gateway
7. **Validation** — verifies service active (user scope), CLI health check
   passes, MCP points at correct workspace

### 1b. Validate

```bash
./mpm/install.sh --validate       # same checks the install script runs
```

Or manually:

```bash
systemctl --user status mpm-scheduler                    # expect: active
~/.mpm/bin/mpm call mpm_system --payload '{"action":"health_check","params":{}}'        # expect: "ok":true
journalctl --user -u mpm-scheduler -n 20 --no-pager     # expect: "scheduler running"
```

### 1c. Configure the LLM provider

After installation, run:

```bash
~/.mpm/bin/mpm config
```

The wizard prompts for the default LLM provider/model and saves to
`profiles["default"]` in `mpm_config.json`. The wizard is state-aware:
it reads existing configuration first, preserves non-empty fields, and
never overwrites components, capabilities, embedding configuration,
or the synthesis flag.

For non-interactive configuration (CI, scripts, agent-managed hosts):

```bash
mpm config profile add default --provider openai --model gpt-4o --base-url https://api.openai.com/v1
mpm config profile set default api_key "$OPENAI_API_KEY"
mpm config component set memory default
```

Embedding and specialised models can be configured later — see
[CONFIGURATION.md](CONFIGURATION.md) for the full reference.

API keys are stored in `mpm_config.json` (file mode 0600). `mpm config show`
redacts them; `mpm config get api_key` returns the full key for the
operator's own use. Environment variables (`MINIMAX_API_KEY`,
`OPENAI_API_KEY`, `OPENROUTER_API_KEY`) work as a non-persisted
alternative.

### 1d. Bootstrap cognitive state

```bash
~/.mpm/bin/mpm ops init directives          # seed prime directives (idempotent)
~/.mpm/bin/mpm status                      # verify DB reachable
~/.mpm/bin/mpm call mpm_context --payload '{"action":"read_wake_context","params":{}}'        # first agent tool call
```

`ops init directives` is the *only* command that touches cognitive state during
install. It seeds prime directives (wake protocol, canonical DB path, etc.)
into the database. Re-running is safe — local edits are preserved.

### 1e. Script modes reference

| Mode | Purpose |
|------|---------|
| `(default)` | Full install (user-space) |
| `--check` | Preflight only — verify environment, no changes |
| `--dry-run` | Print intended actions, no changes |
| `--validate` | Post-install validation (read-only) |
| `--uninstall` | Remove installed artifacts. Data at `$HOME/.mpm/` is preserved |
| `--prefix <path>` | Override install prefix (default `$HOME/.mpm`) |
| `--data-root <path>` | Override data root (default `$HOME/.mpm`) |
| `--user <name>` | Override target user (default: current user) |
| `--yes` | Skip confirmation prompts |

```bash
./mpm/install.sh --check           # safe, no changes
./mpm/install.sh --dry-run         # show what would happen
./mpm/install.sh --uninstall       # remove artifacts (data preserved)
```

> The `--system` flag and `MPM_SYSTEM=1` env form have been removed. MPM
> is user-space only as of this release. A legacy `/etc/systemd/system/
> mpm-scheduler.service` left over from a previous install must be
> disabled manually with `sudo`; the installer will not touch it.

---

> **eCryptfs encrypted-home autostart workaround.** When `/home` is eCryptfs-encrypted
> AND `loginctl enable-linger` is set, the user manager boots at ~08:01 (before
> PAM unwraps eCryptfs on the first login at ~08:17). Unit files inside the
> encrypted tree are invisible at that point — `default.target` is reached before
> `Wants=mpm-scheduler.service` can be evaluated, so `Restart=` does not help.
> The fix is a `~/.config/autostart/mpm-post-decrypt.desktop` entry that runs
> `systemctl --user daemon-reload && systemctl --user start mpm-scheduler.service`
> on every graphical login (post-decrypt). `mpm/install.sh` detects this case
> via `mount` + `findmnt` + the `/home/.ecryptfs/$USER` marker and writes the
> autostart entry automatically; `mpm/uninstall.sh` removes it.
> The `.desktop` `Comment=` line carries the string `071911bc`. It is not a
> lesson ID: it resolves to no row in any substrate table and to no git object in
> this repository, and its origin is unknown. The autostart mechanism was
> introduced by commit `14ac32b`. Separately, the substrate memory that frames the
> daemon-stays-dead-at-boot behaviour as a designed "Lazy-Start Architecture" is
> `463fb2c8014fc1f1`, which carries forward the reasoning from an earlier,
> no-longer-retrievable reference `24be03ec71a5981f` — that reference is not
> independently queryable, so query the memory ID, not the reference.
> `463fb2c8014fc1f1` is currently flagged for manual review — the challenge
> queue's `unresolved state collision (cosine=0.88)` is a stale-detection flag,
> not a substantive dispute of its content. **Both are cited because the substrate
> does not articulate how the two mechanisms relate; the relationship
> between the wake-layer design and the autostart-layer workaround is
> unresolved in the cited material.**
>
> Operators on systems without an agent wake path (cron-driven unattended tasks,
> headless deployments) can opt out by removing the autostart entry and instead
> adding `ExecStartPre=/bin/bash -c 'until mountpoint -q $HOME; do sleep 1; done'`
> to `mpm-scheduler.service` via `systemctl --user edit mpm-scheduler`.

## 2. Agentic Cron (recurring tasks, optional)

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
why pre-compute `next_run_at`) documented in [README §9.3](../README.md#agentic-cron-recurring-tasks).

## 3. Wire to your host

> **Pick one.** MPM supports five host adapters — Claude Code, OpenCode,
> Pi, Hermes, and OpenClaw — each pinned by a managed-section install of
> the canonical MPM behavioral protocol (`~/.mpm/agent_installation/
> mpm-agent-protocol.md`). See `agent_installation/INSTALL.md` for the
> per-host install/verify/uninstall walkthroughs. The recommended
> install (Section 1) auto-wires the OpenClaw adapter when it detects
> the `openclaw` binary; the other four adapters ship their own
> `./mpm/install.sh` and Python snippet installers. Re-run the host adapter
> installer after switching hosts. No `sudo` is required at any point —
> every command below runs in the user's context.

### 3a. OpenClaw (auto-wired by install script)

The install script detects OpenClaw and registers the MCP server automatically,
pointing at `$HOME/.local/bin/mpm-mcp` (the symlink the installer created)
with `MPM_WORKSPACE=$HOME/.mpm`. If you skipped that step or need to
re-register manually:

```bash
openclaw mcp add mpm \
  --command "$HOME/.local/bin/mpm-mcp" \
  --env MPM_WORKSPACE="$HOME/.mpm"

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
> openclaw mcp set mpm '{"command":"'"$HOME"'/.local/bin/mpm-mcp","env":{"MPM_WORKSPACE":"'"$HOME"'/.mpm"}}'
> ```

**Validate:**

```bash
openclaw mcp show mpm | grep MPM_WORKSPACE   # expect: "$HOME/.mpm"
openclaw mcp doctor mpm --probe              # expect: probe passes
```

### 3b. Hermes

```bash
hermes mcp add mpm \
  --command "$HOME/.local/bin/mpm-mcp" \
  --env MPM_WORKSPACE="$HOME/.mpm" \
  --connect-timeout 15
```

Edit `~/.hermes/config.yaml`:

```yaml
mcp_servers:
  mpm:
    command: /home/<your-username>/.local/bin/mpm-mcp
    env:
      MPM_WORKSPACE: /home/<your-username>/.mpm
    timeout: 60
    connect_timeout: 30
    enabled: true
```

**Validate:**

```bash
hermes mcp test mpm                    # expect: Connected, 3 tools discovered (default initial surface; MPM_EXPOSE_ALL_TOOLS=1 → 22)
hermes mcp list | grep mpm             # expect: mpm ... ✓ enabled
```

---

## 4. Uninstall

```bash
./mpm/install.sh --uninstall
```

Removes: `~/.mpm/bin/` (all five binaries + wrapper), `~/.local/bin/mpm`,
`~/.local/bin/mpm-mcp` symlinks, and `~/.config/systemd/user/mpm-scheduler.service`.
**Preserves:** data directory (`$HOME/.mpm/`) and source code at
`~/projects/mpm`.

To remove data too:

```bash
rm -rf ~/.mpm
```

If a stale legacy unit remains at `/etc/systemd/system/mpm-scheduler.service`
from a previous `--system` install, disable it manually:

```bash
sudo systemctl disable --now mpm-scheduler
sudo rm -f /etc/systemd/system/mpm-scheduler.service
```

---

## 5. Path cheat sheet

`./mpm/install.sh` is the only install path. All paths below are
user-owned; no `/usr/local` or `/var/lib/mpm` exists.

| Path | Owner | Purpose |
|------|-------|---------|
| `~/.local/bin/mpm` | $USER | Symlink → `~/.mpm/bin/mpm`. User-PATH entry; subprocesses resolve this by name. |
| `~/.local/bin/mpm-mcp` | $USER | Symlink → `~/.mpm/bin/mpm-mcp`. User-PATH entry for MCP hosts (Claude Code / OpenClaw / Hermes). |
| `~/.mpm/bin/mpm` | $USER | Wrapper script (sets MPM_WORKSPACE, exec's mpm.real) |
| `~/.mpm/bin/mpm.real` | $USER | The actual mpm CLI binary |
| `~/.mpm/bin/mpm-mcp` | $USER | MCP server stdio binary |
| `~/.mpm/bin/mpm-scheduler` | $USER | Scheduler daemon (invoked by systemd --user only — not on PATH) |
| `~/.mpm/bin/mpm-critic` | $USER | Memory critic binary (invoked by mpm-scheduler only — not on PATH) |
| `~/.mpm/bin/mpm-telemetry` | $USER | Telemetry sidecar (invoked by systemd --user only — not on PATH) |
| `~/.config/systemd/user/mpm-scheduler.service` | $USER | User service unit |
| `~/.mpm/src/db/mpm.db` | $USER | SQLite database |
| `~/.mpm/backups/critic-pre/` | $USER | Pre-critic DB snapshots |
| `~/.mpm/scheduler.lock` | $USER | flock singleton lock |

Source code (e.g. `~/projects/mpm/`) is NOT runtime data. It can be wiped
without losing agent state; runtime data persists across `git pull`.

---

## 6. Troubleshooting

| Symptom | Diagnose with | Fix |
|---------|---------------|-----|
| Install fails: "Go not found" | `go version` | Install Go 1.26+ or add to PATH |
| Install fails: "systemd required" | `systemctl --version` | Install systemd (most distros have it) |
| `systemctl --user` fails with "Failed to connect to bus" | `loginctl show-user $USER --property=Linger` | `loginctl enable-linger $USER` (set `Linger=yes`) |
| Service won't start: "permission denied" on data dir | `ls -la ~/.mpm/` | `chown -R $USER:$USER ~/.mpm` |
| Service won't start after reboot on encrypted home | `findmnt /home` | Use `./mpm/install.sh` (full install flow handles linger + drop-in); or manually `systemctl --user edit mpm-scheduler` to add the post-decrypt delay described below. |
| `mpm-scheduler`: DB not found in logs | `systemctl --user show mpm-scheduler -p Environment` | Set `MPM_DB_PATH` in `~/.config/mpm/mpm.env`, or `systemctl --user edit mpm-scheduler` |
| `mpm-scheduler` stays `inactive` after reboot on encrypted `/home` | `systemctl --user is-active mpm-scheduler` returns `inactive`; `journalctl --user -u mpm-scheduler` shows no entries since boot | The autostart fix should have handled this — `~/.config/autostart/mpm-post-decrypt.desktop` runs `daemon-reload && start mpm-scheduler.service` on every graphical login. Verify the file exists; if missing, re-run `./mpm/install.sh` (it re-detects via `mount` + `findmnt` + `/home/.ecryptfs/$USER` and reinstalls the `.desktop`). If your workload runs unattended with no graphical login (cron / system timers only), opt out by removing the `.desktop` and adding a drop-in: `systemctl --user edit mpm-scheduler` → under `[Service]` add `ExecStartPre=/bin/bash -c 'until mountpoint -q $HOME; do sleep 1; done'` to delay-start until the mount is up. Commit `14ac32b` introduced the detection/wiring. |
| CLI fails: "no such file: mpm.real" | `ls -la ~/.mpm/bin/mpm*` | Re-run `./mpm/install.sh` to restore the wrapper |
| CLI reads from wrong DB (e.g. `~/projects/mpm/src/db/mpm.db`) | `which mpm`; `head -1 $(which mpm)` | The `mpm` binary must be a wrapper (`#!/bin/sh`), not the raw binary. Re-run install. |
| Spawn ENOENT when host tries to launch mpm-mcp | `ls -l ~/.mpm/bin/mpm-mcp` (or `bin/mpm-mcp` in source tree) | If missing: `make build`. If not executable: `chmod +x`. Then re-register with correct path. |
| `mpm` not found on PATH after install | `command -v mpm`; `echo $PATH` | Verify `~/.local/bin` is on PATH: most shells pick it up via `/etc/profile.d/` defaults. If not: `export PATH="$HOME/.local/bin:$PATH"`. Internal daemons (mpm-scheduler, mpm-critic, mpm-telemetry) are NOT on PATH by design — they are invoked by systemd, never directly. |
| MCP tools return data, but writes don't persist | `openclaw mcp show mpm` | Check `MPM_WORKSPACE` matches the canonical path (`$HOME/.mpm`); restart gateway |
| `openclaw mcp add mpm` is a silent no-op | `openclaw mcp list` | Server already exists — use `openclaw mcp set mpm '<json>'` instead |
| OpenClaw: agent doesn't see MPM tools in chat | `openclaw mcp list \| grep mpm` | `openclaw gateway restart` (Gateway caches MCP servers at startup) |
| Hermes: agent doesn't see MPM tools in chat | `hermes mcp list \| grep mpm` | Confirm `mcp` toolset in `~/.hermes/config.yaml:toolsets`. Restart session (`/reset`) — config changes don't apply mid-conversation. |
| `mpm ops init directives` errors: "no such table: directives" | `mpm status` | Schema not initialized. Run `mpm status` first to init, then re-run `mpm ops init directives`. |
| MCP tools load but `read_wake_context` returns empty | `mpm ops stats` | DB may be empty. Confirm `MPM_WORKSPACE` matches the canonical db path prime directive. |
| Stale legacy unit at `/etc/systemd/system/mpm-scheduler.service` | `systemctl status mpm-scheduler` (system) shows `loaded failed` or `active` against the old path | The user installer cannot remove it (no sudo). Disable manually: `sudo systemctl disable --now mpm-scheduler && sudo rm -f /etc/systemd/system/mpm-scheduler.service && sudo systemctl daemon-reload`. The current user-space install is unaffected. |

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

- [README.md](../README.md) — cognitive model, design, full reference
- [install.sh](../mpm/install.sh) — the install script (read the source)
- [scripts/](../scripts/) — utility scripts (smoke tests, completion, etc.)
- [contrib/systemd/](../contrib/systemd/) — unit file templates (user only; the legacy system template has been removed)
- [OpenClaw docs](https://docs.openclaw.ai) — platform reference
- [Hermes Agent docs](https://hermes-agent.nousresearch.com/docs) — runtime reference
