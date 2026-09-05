# OpenCode Installation Repair — 2026-09-04

Drift discovered during the 2026-09-04 forensic audit of host-adapter
installations. Repairs are documented here so the install path can be
re-derived if the host config is reset.

## Discovery mechanism (verified)

OpenCode discovers two things at boot:

1. **Plugins** — listed in `~/.config/opencode/opencode.jsonc` under the
   `plugin` field as `file://` URLs pointing at JS modules to load.
   The plugin's main entry is read from the `main` field in its
   `package.json`. For `opencode-mpm`, this is `dist/index.js`.

2. **Persistent instructions** — read from `AGENTS.md` in the project
   root (project scope) or `~/.config/opencode/AGENTS.md` (user scope).
   The opencode-mpm installer (`scripts/install_agents_instructions.py`)
   targets `~/.config/opencode/AGENTS.md` for `--scope user`.

Both paths are documented in `agent_installation/opencode-mpm/README.md`
and are the canonical install layout. The `agent_plugins/` directory
name referenced in some configs is the **old** name — the directory
was renamed to `agent_installation/` (see
`agent_installation/README.md:28`).

## Drift found on this host

| File | Issue |
|---|---|
| `~/.config/opencode/opencode.jsonc` | `plugin` array referenced `~/.mpm/agent_plugins/opencode-mpm/dist/index.js` — the directory does not exist (renamed to `agent_installation/`) |
| `~/.config/opencode/AGENTS.md` | Did not exist; the managed MPM behavioral block was never installed |

## Repair steps

### Plugin path correction

```bash
# Backup before edit
TS=$(date -u +%Y%m%dT%H%M%SZ)
cp ~/.config/opencode/opencode.jsonc ~/.config/opencode/opencode.jsonc.bak-${TS}

# Update the plugin reference to the canonical path
# Before: file:///home/v/.mpm/agent_plugins/opencode-mpm/dist/index.js
# After:  file:///home/v/.mpm/agent_installation/opencode-mpm/dist/index.js
```

The new path must exist and contain a built `dist/index.js`. Run
`npm run build` inside `~/.mpm/agent_installation/opencode-mpm/`
if the dist is stale.

### AGENTS.md installation

```bash
python3 ~/.mpm/agent_installation/opencode-mpm/scripts/install_agents_instructions.py \
    --scope user \
    --target ~/.config/opencode/AGENTS.md \
    --snippet ~/.mpm/agent_installation/opencode-mpm/templates/AGENTS.md.snippet
```

The installer is idempotent — re-running converges on the canonical
managed block from the snippet.

## Verification

After both repairs:

```bash
# Plugin reference resolves
test -f ~/.mpm/agent_installation/opencode-mpm/dist/index.js && \
  grep -c 'agent_installation/opencode-mpm/dist/index.js' \
    ~/.config/opencode/opencode.jsonc

# AGENTS.md has exactly one managed block
grep -c '<!-- BEGIN MPM-MANAGED SECTION:opencode-instructions -->' \
    ~/.config/opencode/AGENTS.md
# expected: 1

# End-to-end (in an OpenCode session):
#   :tools — expect mpm__mpm_memory, mpm__mpm_handoff, mpm__mpm_scratchpad
```

## What was NOT changed

- `scripts/install.sh` — no OpenCode adapter install function was added.
  Adding one is scope creep relative to the audit's "repair, do not
  redesign" boundary.
- `agent_installation/opencode-mpm/` — no source changes. The plugin
  source was already correct; only the host-side config drift needed
  repair.
- `opencode-mpm/README.md` — the install procedure was already
  accurate; no documentation drift to fix here.

## Provenance

Discovered during the 2026-09-04 forensic audit of agent-instruction
and host-adapter installations. Verified by direct filesystem
inspection of `~/.config/opencode/` and
`~/.mpm/agent_installation/opencode-mpm/`. Repair executed in this
session; see commit message of the same date for the runbook commit.
