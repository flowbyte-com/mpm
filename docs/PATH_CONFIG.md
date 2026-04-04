# Path Configuration

MPM resolves all paths dynamically — no hardcoded absolute paths. This enables portable installations.

## Priority Order

| Priority | Source | Example |
|----------|--------|---------|
| 1 | CLI flag | `--workspace=/custom/path` |
| 2 | Environment variable | `MPM_WORKSPACE=/custom/path` |
| 3 | Executable-relative | binary at `flowbyte/mpm/bin/mpm` |
| 4 | Current directory | `os.Getwd()` |

## Key Paths

These are all derived from `GetMPMDir()` → `GetWorkspace()`:

| Path | Default |
|------|---------|
| **Workspace root** | `$HOME/.openclaw/workspace` |
| **MPM binary dir** | `$HOME/.openclaw/workspace/flowbyte/mpm/bin/` |
| **Database** | `$HOME/.openclaw/workspace/flowbyte/mpm/src/db/mpm.db` |
| **JSONL mirror** | `$HOME/.openclaw/workspace/flowbyte/mpm/src/db/mirror.jsonl` |
| **Mode configs** | `$HOME/.openclaw/workspace/flowbyte/mpm/mode/` |
| **Persona configs** | `$HOME/.openclaw/workspace/flowbyte/mpm/persona/` |
| **Watch: memory** | `$HOME/.openclaw/workspace/memory/` |
| **Watch: sessions** | `$HOME/.openclaw/agents/main/sessions/` |
| **Socket** | `/run/user/uid/mpm.sock` |

## How It Works

`GetWorkspace()` in `internal/config/config.go` walks upward from the executable's directory:

```
Executable: .../flowbyte/mpm/bin/mpm
  execDir:  .../flowbyte/mpm/bin
  parent:   .../flowbyte/mpm     ← base = "mpm"
  → workspace = filepath.Dir(parent) = .../flowbyte
```

Special cases exist for `bin/`, `projects/`, `workspace/` directory layouts.

`GetMPMDir()` returns `filepath.Join(workspace, "mpm")` which gives the proper `.../flowbyte/mpm` path.

## Customization

### Via environment:

```bash
export MPM_WORKSPACE=/opt/mpm
```

### Via config file:

```json
// $HOME/.openclaw/workspace/flowbyte/mpm/mpm_config.json
{
  "memory_dir": "/custom/memory",
  "sessions_dir": "/custom/sessions",
  "reference_dir": "/custom/reference"
}
```

### Check current paths:

```bash
mpm doctor
```

---

**Last Updated:** 2026-04-03
