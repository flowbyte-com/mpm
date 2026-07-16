# INSTALL.md

Install the MPM cognitive substrate and wire it to OpenClaw. Result: a
persistent reasoning agent stack.

For background and alternative install paths (CLI-only, no daemon), see
[README.md](README.md) and [README §5 Quick Start](README.md#5-quick-start).

## Prerequisites

| Requirement | Verify with |
|---|---|
| Linux x86_64 with systemd user instance | `systemctl --user status` shows "active" |
| Node.js 22.22.3+ / 24+ / 25+ | `node --version` |
| Go 1.26+ | `go version` |
| OpenClaw installed and onboarded | `openclaw gateway status` shows "listening" |
| LLM API key | Configured in `~/.openclaw/openclaw.json` |

```bash
sudo loginctl enable-linger $USER    # one-time, required for systemd --user
```

## 1. Install MPM

```bash
git clone https://github.com/yourorg/mpm ~/projects/mpm
cd ~/projects/mpm
make build    # produces bin/mpm, bin/mpm-mcp, bin/mpm-scheduler, bin/mpm-critic
```

```bash
make service-scheduler                      # installs the systemd user unit
systemctl --user enable --now mpm-scheduler
systemctl --user status mpm-scheduler       # expect "active (running)"
```

If your MPM clone lives somewhere other than `~/projects/mpm`, override via
`systemctl --user edit mpm-scheduler` or `~/.config/mpm/mpm.env`.

## 2. Wire to OpenClaw

```bash
openclaw mcp add mpm \
  --command "$HOME/projects/mpm/bin/mpm-mcp" \
  --cwd "$HOME/projects/mpm" \
  --env MPM_WORKSPACE="$HOME/projects/mpm" \
  --env MPM_ACTIVE_MODE=programming \
  --env MPM_ACTIVE_PERSONA=correspondent

openclaw mcp doctor mpm --probe    # verify MCP server reachable
openclaw gateway restart           # Gateway caches MCP servers at startup
```

## 3. Bootstrap

```bash
mpm ops init directives    # seed the cognitive bootstrap (idempotent)
mpm status                  # confirm: DB reachable, counts populated
```

## Troubleshooting

| Symptom | Fix |
|---|---|
| `Failed to connect to bus` from `systemctl --user` | `sudo loginctl enable-linger $USER` |
| `mpm-scheduler`: `DB not found` in logs | Set `MPM_DB_PATH` in `~/.config/mpm/mpm.env`, or `systemctl --user edit mpm-scheduler` |
| `mcp doctor mpm`: `spawn ENOENT` | `ls -l $HOME/projects/mpm/bin/mpm-mcp` — must be executable. Re-register with correct path. |
| Agent doesn't see MPM tools | `openclaw gateway restart` (Gateway caches MCP servers at startup) |
| `directives` table missing | Run `mpm status` first to init schema, then `mpm ops init directives` |

## See also

- [README.md](README.md) — cognitive model, design, full reference
- [README §5 Quick Start](README.md#5-quick-start) — install paths (CLI-only, daemon, custom)
- [OpenClaw docs](https://docs.openclaw.ai) — platform reference
- [OpenClaw Agent Workspace](https://docs.openclaw.ai/concepts/agent-workspace) — customize your agent's identity, persona, memory files