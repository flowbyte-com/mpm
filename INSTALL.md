# INSTALL.md

> **Audience:** both humans and agents. Prose explains *why*. Commands are the
> *what*. Both finish the section with the same understanding. Pick your host
> in §3 — everything else is shared.

Install the MPM cognitive substrate and wire it to your host. Result: a
persistent reasoning agent stack.

For background and alternative install paths (CLI-only, no daemon), see
[README.md](README.md) and [README §5 Quick Start](README.md#5-quick-start).

## Prerequisites

> **Why this section first:** every install path — CLI, daemon, OpenClaw, or
> Hermes — needs the same underlying substrate. Fail here, nothing else matters.

| Requirement | Verify with | Pass criterion |
|---|---|---|
| Linux systemd user instance | `systemctl --user status` | exits 0 (active) |
| Node.js 22.22.3+ / 24+ / 25+ | `node -v` | version ≥ v22.22.3 |
| Go 1.26+ | `go version` | version ≥ go1.26 |
| LLM API key | configured in host config | auth block present |

```bash
sudo loginctl enable-linger $USER    # required for systemctl --user
```

> **Daemon scheduler is optional for self-scheduled reminders.** The
> opportunistic fold (next `mpm call` after a wake's target time) handles
> agent-scheduled wakes without a running daemon. The daemon
> (`mpm-scheduler`) is only required for *system*-kind wakes: pre-flight
> snapshots, critic audits, GC sweeps, broadcasts. If you're a single agent
> waking naturally between user prompts, skip §2b and revisit when you need
> unattended system tasks.

## 1. Install MPM

> **Why clone to `~/projects/mpm`:** install.md assumes this default so
> systemd units, env files, and host configs resolve without overrides. If
> you clone elsewhere, document the divergence in `~/.config/mpm/mpm.env` —
> the prime directive on the canonical db path will not accept silent drift.

```bash
git clone https://github.com/yourorg/mpm ~/projects/mpm
cd ~/projects/mpm
make build                            # produces bin/mpm, bin/mpm-mcp, bin/mpm-scheduler, bin/mpm-critic
```

**Validate (all hosts):**

```bash
ls -l ~/projects/mpm/bin/mpm-mcp     # expect: -rwxr-xr-x
mpm status                           # expect: DB reachable
```

## 2. (Optional) Deploy the daemon scheduler

> **Skip this section if:** you only need self-scheduled reminders via the
> opportunistic fold. The daemon does *not* make self-scheduled wakes more
> reliable — it makes *system*-kind wakes (snapshots, critic, GC, broadcast)
> run unattended regardless of user presence. Single-agent workflows don't
> need it.

```bash
cd ~/projects/mpm
make service-scheduler                # copies unit to ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now mpm-scheduler
```

**Validate:**

```bash
systemctl --user is-active mpm-scheduler    # expect: active
```

Override the default path via `systemctl --user edit mpm-scheduler` or
`~/.config/mpm/mpm.env`.

## 3. Wire to your host

> **Pick one.** OpenClaw and Hermes are the two supported hosts as of
> 2026-07-16. The commands differ; the substrate underneath is identical.
> Run only the block for your host — running both will register MPM twice
> in your host's MCP server list.

### 3a. OpenClaw

> **Why OpenClaw:** the gateway caches MCP servers at startup; config edits
> require `openclaw gateway restart` to take effect.

```bash
openclaw mcp add mpm \
  --command "$HOME/projects/mpm/bin/mpm-mcp" \
  --cwd "$HOME/projects/mpm" \
  --env MPM_WORKSPACE="$HOME/projects/mpm" \
  --env MPM_ACTIVE_MODE=programming \
  --env MPM_ACTIVE_PERSONA=correspondent

openclaw gateway restart              # Gateway caches MCP servers at startup
```

**Validate:**

```bash
openclaw mcp doctor mpm --probe       # expect: probe passes; tools listed
```

### 3b. Hermes

> **Why Hermes:** the agent loop reads MCP servers at session start. After
> editing `~/.hermes/config.yaml`, the changes apply on `/reset` or next
> session — they do *not* apply mid-conversation (prompt caching). Mode and
> persona are routed dynamically by the `route` tool on each prompt, so
> env-var-based `MPM_ACTIVE_MODE` / `MPM_ACTIVE_PERSONA` are NOT used here.
>
> **No workspace files needed.** OpenClaw wires MPM via prose directives in
> workspace files (`AGENTS.md`, `SOUL.md`, `MEMORY.md`). Hermes wires MPM
> via MCP server registration in `~/.hermes/config.yaml` — the agent sees
> 61 MPM tools in its function-call schema at session start and uses them
> natively. Don't write a `SOUL.md` for Hermes asking it to use MPM; the
> config IS the wiring. If you migrate from OpenClaw, your existing
> workspace files become reference material at most — the runtime is
> different.

```bash
hermes mcp add mpm \
  --command "$HOME/projects/mpm/bin/mpm-mcp" \
  --env MPM_WORKSPACE="$HOME/projects/mpm" \
  --connect-timeout 15
```

If your MPM lives at a non-default path, set `MPM_WORKSPACE` to the
absolute path. The prime directive on the canonical db path will reject
drift silently — point at `/home/v/.mpm/src/db/mpm.db` (or your equivalent
canonical path) rather than the project tree.

Edit `~/.hermes/config.yaml` and ensure the `mcp` toolset is enabled:

```yaml
toolsets:
  - hermes-cli
  - mcp

mcp_servers:
  mpm:
    command: /home/v/.mpm/bin/mpm-mcp     # or wherever bin/mpm-mcp lives
    args: []
    env:
      MPM_WORKSPACE: /home/v/.mpm         # canonical db path
    timeout: 60
    connect_timeout: 30
    enabled: true
```

**Validate:**

```bash
hermes mcp test mpm                    # expect: Connected, 61 tools discovered
hermes mcp list | grep mpm             # expect: mpm ... ✓ enabled
```

Then start a new Hermes session. The 61 MPM tools load into the agent's
function-call schema. First tool call should be `read_wake_context` per the
mpm wake protocol.

## 4. Bootstrap

> **Why this is universal:** prime directives are seeded into the
> directives table by `mpm ops init directives`. Both hosts surface them
> via `read_directives` on every wake. Idempotent — safe to re-run; local
> edits are preserved.

```bash
mpm ops init directives
```

**Validate:**

```bash
mpm status                             # expect: DB reachable
mpm ops stats                          # expect: counts populated
```

## 5. Validate end-to-end

> **One host, one chat, one read.** This is the closest thing to a smoke
> test that works across both hosts.

Open a chat with your agent (OpenClaw chat, Hermes chat, or `mpm wake` from
the CLI). The agent's first tool call should be `read_wake_context`. It
should return mode, persona, recent memories, prime directives, and (if
present) an unread handoff. If it does, MPM is wired correctly.

If `read_wake_context` returns an empty wake context, run `mpm ops stats`
to confirm the database is reachable. If the database is unreachable,
check `MPM_WORKSPACE` and the canonical db path prime directive.

## Troubleshooting

> **Diagnose first, fix second.** Most failures are path or env issues —
> not MPM itself.

| Symptom | Diagnose with | Fix |
|---|---|---|
| `systemctl --user` fails with "Failed to connect to bus" | `loginctl show-user $USER --property=Linger` | `sudo loginctl enable-linger $USER` (set `Linger=yes`) |
| `mpm-scheduler`: DB not found in logs | `systemctl --user show mpm-scheduler -p Environment` | Set `MPM_DB_PATH` in `~/.config/mpm/mpm.env`, or `systemctl --user edit mpm-scheduler` |
| Spawn ENOENT when host tries to launch mpm-mcp | `ls -l $HOME/projects/mpm/bin/mpm-mcp` | If missing: `make build`. If not executable: `chmod +x`. Then re-register with correct path. |
| OpenClaw: agent doesn't see MPM tools in chat | `openclaw mcp list \| grep mpm` | `openclaw gateway restart` (Gateway caches MCP servers at startup) |
| Hermes: agent doesn't see MPM tools in chat | `hermes mcp list \| grep mpm` | Confirm `mcp` toolset in `~/.hermes/config.yaml:toolsets`. Restart session (`/reset`) — config changes don't apply mid-conversation. |
| `mpm ops init directives` errors: "no such table: directives" | `mpm status` | Schema not initialized. Run `mpm status` first to init, then re-run `mpm ops init directives`. |
| MPM tools load but `read_wake_context` returns empty | `mpm ops stats` | DB may be empty. Confirm `MPM_WORKSPACE` matches the canonical db path prime directive. |

## See also

- [README.md](README.md) — cognitive model, design, full reference
- [README §5 Quick Start](README.md#5-quick-start) — install paths (CLI-only, daemon, custom)
- [README §5.4](README.md#54-customize-your-agents-core-files) — workspace file reference (OpenClaw-specific; Hermes has no equivalent workspace layer)
- [OpenClaw docs](https://docs.openclaw.ai) — platform reference
- [Hermes Agent docs](https://hermes-agent.nousresearch.com/docs) — runtime reference
