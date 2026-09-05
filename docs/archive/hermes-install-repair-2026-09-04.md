# Hermes Installation Repair — 2026-09-04

Drift discovered during the 2026-09-04 forensic audit of host-adapter
installations. Repairs are documented here so the install path can be
re-derived if the host config is reset.

## Discovery mechanism (verified)

Hermes wires MPM through TWO surfaces, both verified against the
canonical documentation:

1. **MCP stdio server (`mpm-mcp`)** — listed in `~/.hermes/config.yaml`
   under `mcp_servers`. Hermes spawns the binary as a child process and
   speaks JSON-RPC over stdio. The `MPM_WORKSPACE` env var tells
   `mpm-mcp` where to find the pidfile and resolve the database.

2. **Hermes MPM skill** — `~/.hermes/skills/mpm/SKILL.md`, auto-loaded
   by Hermes when the task involves MPM. Provides operational guidance
   for the wake/recall/save discipline and known pitfalls.

The project-level `.hermes.md` (or `HERMES.md`) is a separate, optional
surface — Hermes walks up from cwd looking for it. The MPM installer
(`scripts/install_hermes_instructions.py`) targets project scope; it
is NOT a host-level config.

Both surfaces are documented in `agent_installation/hermes-mpm/SKILL.md`.

## Drift found on this host

| File | Issue |
|---|---|
| `~/.hermes/config.yaml` (mcp_servers.mpm.env.MPM_WORKSPACE) | Set to `/home/v/.openclaw/workspace/projects/mpm` (legacy project-source path). README canonical is `$HOME/.mpm` (install root). Functionally equivalent (same DB inode via symlink) but inconsistent with the documented install layout. The README at `agent_installation/hermes-mpm/SKILL.md:46` already flagged this. |

## Repair

```bash
# Backup before edit
TS=$(date -u +%Y%m%dT%H%M%SZ)
cp ~/.hermes/config.yaml ~/.hermes/config.yaml.bak-${TS}

# Update mcp_servers.mpm.env.MPM_WORKSPACE to the canonical install root
# Before: MPM_WORKSPACE: /home/v/.openclaw/workspace/projects/mpm
# After:  MPM_WORKSPACE: /home/v/.mpm
```

The path must point at the canonical install root (`~/.mpm`), which is
the symlink-tree install layout. The DB resolves through that path to
the same inode as the project source via the existing symlink at
`~/.mpm/src/db/mpm.db`.

## Verification

After the repair:

```bash
# MPM_WORKSPACE points at the canonical install root
grep -A 1 'MPM_WORKSPACE' ~/.hermes/config.yaml
# expected: MPM_WORKSPACE: /home/v/.mpm

# DB inode resolves
stat -c '%i' ~/.mpm/src/db/mpm.db
# expected: matches /home/v/.openclaw/workspace/projects/mpm/src/db/mpm.db

# MCP server binary exists at the documented absolute path
test -x ~/.mpm/bin/mpm-mcp && echo OK
```

End-to-end via Hermes:

```
mcp__mpm__mpm_system action=health_check
# expected: {"ok":true,...}
```

The legacy plugin loader referenced in `agent_installation/hermes-mpm/SKILL.md:111`
(`mpm_hermes_loader.py`) was already absent from the filesystem at
audit time — no separate cleanup required.

## What was NOT changed

- `scripts/install.sh` — no Hermes adapter install function was added.
  Hermes is already wired (MCP + skill); no new install mechanism is
  needed. Adding speculative install.sh functions is out of scope.
- `agent_installation/hermes-mpm/SKILL.md` — the install procedure
  documentation was already accurate; the drift was in the host-side
  config, not the docs.
- The project-level `.hermes.md` was not created — the installer is
  project-scoped, not host-scoped. Per-project decision.

## Provenance

Discovered during the 2026-09-04 forensic audit of agent-instruction
and host-adapter installations. Verified by direct read of
`~/.hermes/config.yaml` and `agent_installation/hermes-mpm/SKILL.md`.
Repair executed in this session; see commit message of the same date
for the runbook commit.
