# INSTALL.md

Install the MPM cognitive substrate and wire it to OpenClaw. Result: a
persistent reasoning agent stack.

For background and alternative install paths (CLI-only, no daemon), see
[README.md](README.md) and [README §5 Quick Start](README.md#5-quick-start).

## Prerequisites

| Requirement | Verify with | Pass criterion |
|---|---|---|
| Linux systemd user instance | `systemctl --user status` | exits 0 (active) |
| Node.js 22.22.3+ / 24+ / 25+ | `node -v` | version ≥ v22.22.3 |
| Go 1.26+ | `go version` | version ≥ go1.26 |
| OpenClaw installed and onboarded | `openclaw gateway status` | "listening on port 18789" |
| LLM API key | configured in `~/.openclaw/openclaw.json` | auth block present |

```bash
sudo loginctl enable-linger $USER    # required for systemctl --user
```

## 1. Install MPM

```bash
# Clone and build
git clone https://github.com/yourorg/mpm ~/projects/mpm
cd ~/projects/mpm
make build

# Deploy systemd unit
make service-scheduler
systemctl --user enable --now mpm-scheduler
```

**Validate:**

```bash
systemctl --user is-active mpm-scheduler    # expect: active
ls -l ~/projects/mpm/bin/mpm-scheduler      # expect: -rwxr-xr-x
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

openclaw gateway restart           # Gateway caches MCP servers at startup
```

**Validate:**

```bash
openclaw mcp doctor mpm --probe    # expect: probe passes; tools listed
```

## 3. Bootstrap

```bash
mpm ops init directives    # seed the cognitive bootstrap (idempotent)
```

**Validate:**

```bash
mpm status                  # expect: DB reachable
mpm ops stats               # expect: counts populated (memory, decision, theory, lesson)
```

## Troubleshooting

| Symptom | Diagnose with | Fix |
|---|---|---|
| `systemctl --user` fails with "Failed to connect to bus" | `loginctl show-user $USER --property=Linger` | `sudo loginctl enable-linger $USER` (set `Linger=yes`) |
| `mpm-scheduler`: DB not found in logs | `systemctl --user show mpm-scheduler -p Environment` | Set `MPM_DB_PATH` in `~/.config/mpm/mpm.env`, or `systemctl --user edit mpm-scheduler` |
| `mcp doctor mpm`: spawn ENOENT | `ls -l $HOME/projects/mpm/bin/mpm-mcp` | If missing: `make build`. If not executable: `chmod +x`. Then re-register with correct path. |
| Agent doesn't see MPM tools in chat | `openclaw mcp list \| grep mpm` | `openclaw gateway restart` (Gateway caches MCP servers at startup) |
| `mpm ops init directives` errors: "no such table: directives" | `mpm status` | Schema not initialized. Run `mpm status` first to init, then re-run `mpm ops init directives`. |

## See also

- [README.md](README.md) — cognitive model, design, full reference
- [README §5 Quick Start](README.md#5-quick-start) — install paths (CLI-only, daemon, custom)
- [README §5.4](README.md#54-customize-your-agents-core-files) — workspace file reference
- [OpenClaw docs](https://docs.openclaw.ai) — platform reference
- [OpenClaw Agent Workspace](https://docs.openclaw.ai/concepts/agent-workspace) — customize your agent's identity, persona, memory files