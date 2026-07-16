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

The **Baseline Cognitive Bootstrap** is MPM's internal "operating manual" — a
set of prime directives that close MPM's cognitive loops:

- Read `wake_context` on every session start (no amnesia).
- Triage `list_active_clusters` at session end (vacation-proof audit survival).
- Triage wake notifications deterministically (don't surprise the user; don't
  silently drop).
- Use `mpm call save_to_memory` / `query_long_term_memory` for persistent
  cognition rather than re-deriving from scratch.

Without these directives, MPM's advanced machinery (audit-cluster detection,
wake-context surfacing, theory-driven repair) has no behavioral hooks and
silently no-ops. The command is **idempotent** — safe to re-run; existing
directives are detected by stable ID and skipped. Local edits to a seeded
directive are preserved; the run report flags drift so you can reconcile
manually.

These directives live in MPM's SQLite database (`directives` table),
distinct from the OpenClaw workspace files (`AGENTS.md`, `SOUL.md`, etc.)
covered in §4. Both need to be set up for the full closed loop. See
`internal/core/seed/directives.go` for the canonical seed registry.

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

## 4. Customize your agent's core files

`openclaw onboard` seeded the workspace with default files. They make the
agent _functional_ — not _yours_. The files below are what turn a generic
assistant into one that knows your name, your preferences, your domain, and
how you like to work.

All of these live in your workspace (`~/.openclaw/workspace/` by default).
See the canonical [OpenClaw Agent Workspace](https://docs.openclaw.ai/concepts/agent-workspace)
reference for the full file map.

| File | What it shapes | When to edit |
|---|---|---|
| `IDENTITY.md` | Name, sigil, color, emoji — who the agent _is_ | During bootstrap; revisit when the persona evolves |
| `SOUL.md` | Persona, tone, boundaries, vibe — how the agent _behaves_ | During bootstrap; refine as patterns emerge |
| `AGENTS.md` | Operating instructions, memory workflow, red lines | As the workflow stabilizes — this is the agent's operating manual |
| `USER.md` | Who you are, how to address you, your preferences | Once you know what the agent should remember about you |
| `TOOLS.md` | Local tool conventions (cameras, SSH hosts, TTS voices, etc.) | When you start using tools the agent doesn't know about |
| `HEARTBEAT.md` | Periodic check tasks (or empty to skip heartbeats) | When you want scheduled checks |
| `MEMORY.md` | Curated long-term memory — durable facts, preferences, decisions | Continuously, as the agent learns things worth keeping |
| `memory/YYYY-MM-DD.md` | Daily logs of what happened | Daily, raw session notes |

**Start simple. Iterate as the agent does work.** You don't need to fill these
in upfront — they co-evolve with the agent over sessions. A useful pattern:

1. After bootstrap, run the agent for a week on default files.
2. Notice where the agent is _generic_ (no opinions, no preferences, no
   knowledge of you).
3. Edit the relevant file to capture the gap.
4. Repeat.

**SOUL.md is the highest-leverage file.** It's loaded every session and sets
the agent's tone. See the [SOUL.md personality guide](https://docs.openclaw.ai/concepts/soul)
for what good ones look like. Defaults are generic-friendly; an opinionated
SOUL.md (with stance, no preamble, sharp entrances) is the difference between
a chatbot and an assistant that feels like a colleague.

**MEMORY.md vs MPM's long-term memory.** Both serve memory but at different
layers:

- **`MEMORY.md`** is the workspace file — small, curated, loaded every main
  session. Identity, bootstrap, durable preferences. Should stay under a
  few hundred lines.
- **MPM** (`mpm call save_to_memory` / `query_long_term_memory`) is the
  cognitive substrate — structured, queryable, evidence-tracked. Beliefs,
  decisions, theories, lessons. Scales to thousands of rows.

Use `MEMORY.md` for "who am I and what do I know about my user". Use MPM for
"what does the agent believe, why, and with what evidence". They complement
each other — neither replaces the other.

**There's also a third layer: MPM's prime directives** — seeded by
`mpm ops init directives` (see §2). These live in MPM's database
(`directives` table) and surface via `mpm call read_directives` on every
wake. They're MPM-internal "operating manual" — the cognitive hooks that
make wake_context, cluster triage, and wake notification handling actually
fire. The full agent stack has all three layers: workspace files (who the
agent is) + MPM directives (how MPM's machinery hooks in) + MPM memories
(what the agent believes).

---

## 5. Verify the stack

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

## 6. Where to go next

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

---

## Doc maintenance

This section is for future-me (or anyone maintaining the install story). New
installers can skip it; it's about the doc architecture, not the install
sequence.

**The install story lives in two places, by design:**

- **`INSTALL.md`** (this file, ~300 lines) — *action doc*. Sequence for
  installers, troubleshooting for fresh-install friction. Optimized for
  "type these commands and you're running."
- **`README §5 Quick Start`** (~80 lines) — *reference doc*. Install options,
  trade-offs, full surface. Optimized for "I want to understand all the
  install paths and choose deliberately."

The 5-command daemon install block (§2 here, §5.2 in README) is duplicated
intentionally — minimum viable command surface. Don't try to deduplicate;
the small duplication is cheaper than the cross-reference complexity.

**When fixing install friction, atomic updates are mandatory:**

1. Capture the finding in `docs/superpowers/install-friction-2026-07-16.md`
   (gitignored — local working notes). Include the symptom, the diagnosis,
   the fix, and the commit SHA that resolved it.
2. Update **both** `INSTALL.md` and `README §5` in the same commit. Don't
   land a fix in one and forget the other — readers use both paths.
3. Update `contrib/systemd/mpm-scheduler.service` if the systemd surface
   changes (paths, env vars, hardening flags).
4. Update the Makefile (`service-scheduler` / `service` / `uninstall-service`
   targets) if install/deploy semantics change.
5. The friction log entry references the commit SHA, so a future `git log
   --grep` finds the resolution.

**When adding a new install path or option:**

- Add to both docs (the action sequence in INSTALL.md; the reference entry
  in README §5).
- The action doc shows the canonical path. The reference doc shows all
  paths with trade-offs. Different jobs, same source of truth.

**Heath Robinson check:** if a fix needs to scatter across more than two
docs, that's a signal the architecture has drifted. Pause, reconsider
whether the new content belongs in one of the existing docs or in a new
specialized doc (e.g., `docs/INSTALL_PROXY.md` for a deployment variant).
Don't pile into INSTALL.md.