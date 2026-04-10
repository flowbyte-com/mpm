# Installing MPM

MPM (Memory-Persona-Mode Manager) — SQLite-native agent state management for OpenClaw agents.

## Prerequisites

- **Go 1.18+** — For building from source
- **Unix-like OS** — Linux or macOS (fsnotify required)
- **~50MB disk space** — For binary + database

## Step 1: Build

The recommended location for MPM is your home directory:

```bash
git clone https://github.com/flowbyte-com/mpm.git ~/mpm
cd ~/mpm
make build
```

This creates `~/mpm/bin/mpm` — the main binary.

## Step 2: Install System-Wide (Optional)

```bash
sudo make install        # Installs to /usr/local/bin/mpm
# Or custom prefix:
make install PREFIX=$HOME/.local
```

## Step 3: Add to PATH

Add to `~/.bashrc` or `~/.zshrc`:

```bash
export PATH="$HOME/mpm/bin:$PATH"
export MPM_WORKSPACE="$HOME/mpm"
```

> **Note:** `MPM_WORKSPACE=$HOME/mpm` keeps all state (memory/sessions dirs, socket) inside the `~/mpm` tree. Without it, MPM auto-detects from the binary path but watch dirs fall back to `$HOME/memory/` and `$HOME/sessions/`.

Reload:
```bash
source ~/.bashrc
```

Verify:
```bash
mpm status
```

## Step 4: Configure OpenClaw (Optional)

MPM is designed as the memory layer for OpenClaw agents. If you use OpenClaw:

```bash
openclaw skills enable mpm
```

Or add to your OpenClaw config (`~/.openclaw/openclaw.json`):

```json
{
  "skills": {
    "entries": {
      "mpm": { "enabled": true }
    }
  }
}
```

## Step 5: Configure Watch Sources (Optional)

Edit `mpm_config.json` in the MPM directory:

```json
{
  "memory_dirs": [],
  "sessions_dirs": [],
  "external_dbs": [
    {
      "path": "~/.openclaw/memory/main.sqlite",
      "label": "openclaw",
      "interval_seconds": 30
    }
  ],
  "synth": {
    "model": "MiniMax-M2.7",
    "api_key": "",
    "base_url": ""
  }
}
```

| Field | What It Does |
|-------|--------------|
| `memory_dirs` | Watch directories for `.md` memory files (auto-ingest) |
| `sessions_dirs` | Watch directories for `.jsonl` session files |
| `external_dbs` | Poll external SQLite DBs for memories to ingest |
| `synth` | LLM settings for session synthesis |

Leave arrays empty to use MPM's internal defaults. See [docs/WATCH.md](docs/WATCH.md) for details.

## Step 6: Start

```bash
mpm start              # Start daemon + watch daemon
mpm status             # Verify it's running
```

## Directory Layout

```
~/mpm/
├── bin/mpm                      # Compiled binary
├── src/db/
│   ├── mpm.db                   # SQLite database
│   └── mirror.jsonl             # Audit log
├── mode/                        # Mode configurations (JSON)
├── persona/                     # Persona configurations (JSON)
├── docs/                        # Documentation
└── mpm_config.json             # Configuration
```

## Configuration

### MPM_WORKSPACE

Controls where MPM stores data and looks for configs.

```bash
export MPM_WORKSPACE=/path/to/workspace
```

Priority: `MPM_WORKSPACE` env var → executable-relative → CWD fallback

### mpm_config.json

Located at `$MPM_WORKSPACE/mpm_config.json`. Controls:

- Watch directories (memory, sessions)
- External databases to poll
- LLM/synthesis settings

Changes take effect immediately — no restart needed.

See [docs/PATH_CONFIG.md](docs/PATH_CONFIG.md) for path resolution details.

## Troubleshooting

### "Daemon not running"

```bash
mpm start
```

### Check paths

```bash
mpm doctor
```

### Verbose watch

```bash
mpm watch --v
```

### Build errors

```bash
go version    # Must be 1.18+
go build -o bin/mpm ./cmd/mpm   # Direct build to see errors
```

## Upgrading

```bash
cd ~/mpm
git pull
make clean build
mpm restart
```

## Keeping in Sync

If you develop on MPM (clone at dev path) and install at `~/mpm/`:

```bash
# From dev clone — sync source to install
rsync -av --exclude='.git' --exclude='bin/mpm' --exclude='mpm-agent/mpm_agent' --exclude='*.db-shm' --exclude='*.db-wal' . ~/mpm/
make -C ~/mpm build && cp ~/mpm/bin/mpm ~/mpm/bin/mpm
```

> **Important:** Never run `git` commands from the `~/mpm/` install directory — it's a synced copy, not a git repo. Do all development in your dev clone.

## Uninstall

```bash
rm -rf ~/mpm   # Remove data and binary
```

(Remove the PATH and MPM_WORKSPACE lines from your shell config too.)

## For AI Agents

MPM is designed to be used by AI agents. Add this to your agent's system prompt or memory:

```markdown
# MPM - Memory-Persona-Mode Manager

MPM is my persistent memory layer at ~/mpm/

## Key Commands
- mpm start / stop / restart — Daemon lifecycle
- mpm memory add "<fact>" — Store a memory
- mpm recall <query> — Search memories (FTS5)
- mpm mode — Switch behavioral mode (interactive)
- mpm persona — Switch persona (interactive)
- mpm lesson add "<wisdom>" --type warning|practice|insight — Add lesson

## Modes Available
ask, creative, debug, default, design, direct, grow, plan, research, ship

## Personas Available
alice, caterpillar, cheshire, default, hatter, oracle, pirate, queen, whiterabbit

## Tip
I can ask MPM questions directly: "Search mpm memories for anything about Go generics"
```
