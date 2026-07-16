# INSTALL.md — From Zero to a Cognitive Agent

Get a working **OpenClaw + MPM** stack running on a fresh Linux box in under
30 minutes. MPM is the cognitive substrate (memory, decisions, theories,
scheduled cognition); OpenClaw is the agent host (conversation loop, channels,
workspace). This guide wires them together.

For the MPM-specific install story (CLI vs daemon mode, systemd setup,
init directives, MCP host config), see [README §5 Quick Start](README.md#5-quick-start).
This document is the higher-level sequencing guide.

---

## Prerequisites

| Requirement | Version | Why |
|---|---|---|
| Linux | x86_64, systemd with user-instance support | systemd --user for the scheduler |
| Node.js | 22.22.3+ / 24.15+ / 25.9+ | OpenClaw runtime (Node 24 recommended) |
| Go | 1.26+ | Building MPM from source |
| LLM API key | Anthropic, OpenAI, Google, custom endpoint, etc. | Drives the agent loop |

Verify with:

```bash
node --version    # expect v22.22.3+ / v24+ / v25+
go version        # expect go1.26+
systemctl --user status    # expect "active"; if not, see §4 below
```

---

## 1. Install OpenClaw

OpenClaw is the agent host. See the canonical
[OpenClaw Getting Started](https://docs.openclaw.ai/start/getting-started)
for the full guide. The quick path:

```bash
curl -fsSL https://openclaw.ai/install.sh | bash
openclaw onboard --install-daemon
```

The onboard wizard:

- Creates `~/.openclaw/workspace/` (your agent's working directory)
- Prompts for a model provider + API key (or accepts a custom endpoint)
- Optionally installs the Gateway as a systemd daemon (recommended)
- Sets up `~/.openclaw/openclaw.json` (main config) + `~/.openclaw/config/`

Verify the Gateway is alive:

```bash
openclaw gateway status    # expect "listening on port 18789"
```

If you need to add/modify model providers later:

```bash
openclaw configure
```

---

## 2. Install MPM (the cognitive substrate)

MPM gives your agent persistent memory, a decision ledger, theory tracking,
and scheduled cognition. It exposes its tool surface to OpenClaw via MCP.

```bash
git clone https://github.com/yourorg/mpm ~/projects/mpm
cd ~/projects/mpm
make build    # produces bin/mpm, bin/mpm-mcp, bin/mpm-scheduler, bin/mpm-critic
```

For the **full MPM install story** — including the CLI-only fast path, the
daemon setup, the systemd unit, the baseline cognitive directives, and
troubleshooting — see [README §5 Quick Start](README.md#5-quick-start).

The canonical daemon install (from §5.2):

```bash
# Allow systemd --user to run without an active session
sudo loginctl enable-linger $USER

# Install the scheduler as a systemd user service
make service-scheduler
systemctl --user enable --now mpm-scheduler
systemctl --user status mpm-scheduler    # verify

# Seed the baseline cognitive directives (the cognitive bootstrap)
mpm ops init directives
```

---

## 3. Wire MPM into OpenClaw

OpenClaw spawns MCP servers as stdio children. Register `mpm-mcp` via the
canonical OpenClaw CLI:

```bash
openclaw mcp add mpm \
  --command "$HOME/projects/mpm/bin/mpm-mcp" \
  --cwd "$HOME/projects/mpm" \
  --env MPM_WORKSPACE="$HOME/projects/mpm" \
  --env MPM_ACTIVE_MODE=programming \
  --env MPM_ACTIVE_PERSONA=correspondent
```

Verify the wiring:

```bash
openclaw mcp doctor mpm --probe    # expect green; lists exposed tools
```

Restart the Gateway so it picks up the new MCP server:

```bash
openclaw gateway restart
```

> **Where does this get written?** OpenClaw saves the server definition
> under `mcp.servers.mpm` in `~/.openclaw/openclaw.json`. You can edit it
> directly if you prefer; the CLI just wraps the JSON.

> **Alternative paths.** If you cloned MPM somewhere other than
> `~/projects/mpm`, change the `--command` and `--cwd` paths above.
> Non-standard layouts are fine — OpenClaw doesn't care where the binary
> lives, only that it can spawn it.

---

## 4. Verify the stack

Run all three checks. If any fails, see [Troubleshooting](#troubleshooting).

```bash
# OpenClaw
openclaw gateway status                  # gateway listening
openclaw mcp doctor mpm --probe          # MCP server reachable, tools exposed

# MPM
mpm status                               # DB reachable, counts populated
systemctl --user status mpm-scheduler    # scheduler running
mpm ops stats                            # memory/decision/theory counts
```

Then chat with your agent. The MPM cognitive machinery (wake context, audit
clusters, session-end triage) is now wired in. The agent can:

- `save_to_memory`, `query_long_term_memory` for persistent cognition
- `record_decision`, `propose_theory`, `challenge_memory` for belief tracking
- `schedule_wake` for deferred work that surfaces on next contact
- `commit_milestone`, `record_decision`, `save_lesson` for the agent loop

---

## 5. Where to go next

| Doc | What it covers |
|---|---|
| [README.md](README.md) | The cognitive model, design principles, full reference |
| [README §5 Quick Start](README.md#5-quick-start) | Detailed MPM install paths (CLI-only / daemon / MCP host) |
| [docs/audit-2026-07-16.md](docs/audit-2026-07-16.md) | Recent security review |
| [OpenClaw docs](https://docs.openclaw.ai) | Channels, configuration, tools, troubleshooting |

If you want to add channels (Telegram, Signal, Discord, …) so your agent
can talk to you outside the Control UI, start with the
[OpenClaw Channels overview](https://docs.openclaw.ai/channels).

---

## Troubleshooting

**`systemctl --user status` says "Failed to connect to bus"**
The systemd user instance isn't running because you have no active session.
Run `sudo loginctl enable-linger $USER` (one-time per user) and try again.

**`make service-scheduler` succeeds but `enable --now` errors with "Unit not found"**
`make service-scheduler` already runs `daemon-reload`, but if you installed
the unit by hand, run `systemctl --user daemon-reload` first.

**`mpm-scheduler` service starts but logs `DB not found` or `permission denied`**
The default unit assumes `~/projects/mpm`. If your layout is different,
either:

- Edit the unit: `systemctl --user edit mpm-scheduler` (writes a drop-in at
  `~/.config/systemd/user/mpm-scheduler.service.d/override.conf`)
- Or set env vars in `~/.config/mpm/mpm.env` (the unit sources it via
  `EnvironmentFile=-`):
  ```bash
  echo 'MPM_DB_PATH=/your/path/mpm.db' > ~/.config/mpm/mpm.env
  echo 'MPM_BACKUP_DIR=/your/backup/path' >> ~/.config/mpm/mpm.env
  ```
Then `systemctl --user restart mpm-scheduler`.

**`openclaw mcp doctor mpm --probe` errors with "spawn ENOENT"**
The path in `--command` doesn't exist or isn't executable. Verify with
`ls -l "$HOME/projects/mpm/bin/mpm-mcp"` — should be `-rwxr-xr-x`. If you
moved the binary, re-register with the right path.

**Agent doesn't see MPM tools in chat**
The Gateway loads MCP servers at startup. After registering, restart it:
`openclaw gateway restart`. Then check the Control UI `/settings/mcp` page
— `mpm` should be listed and enabled.

**`mpm ops init directives` errors with "no such table: directives"**
The DB hasn't been initialized. Run any `mpm` command first
(e.g. `mpm status`) — the schema is created on first connection.

---

## See also

- [README.md](README.md) — start here for the cognitive model
- [OpenClaw docs](https://docs.openclaw.ai) — full platform reference
- [docs/audit-2026-07-16.md](docs/audit-2026-07-16.md) — security review