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

| Path | Default (MPM_WORKSPACE=$HOME/mpm) |
|------|---------|
| **Workspace root** | `$HOME/mpm` |
| **MPM binary dir** | `$HOME/mpm/bin/` |
| **Database** | `$HOME/mpm/src/db/mpm.db` |
| **JSONL mirror** | `$HOME/mpm/src/db/mirror.jsonl` |
| **Mode configs** | `$HOME/mpm/mode/` |
| **Persona configs** | `$HOME/mpm/persona/` |
| **Watch: memory** | `$MPM_WORKSPACE/memory/` |
| **Watch: sessions** | `$MPM_WORKSPACE/sessions/` |
| **Socket** | `/run/user/uid/mpm.sock` |

## How It Works

`GetWorkspace()` in `internal/config/config.go` walks upward from the executable's directory:

```
Executable: .../mpm/bin/mpm
  execDir:  .../mpm/bin
  parent:   .../mpm     ← base = "mpm"
  → workspace = filepath.Dir(parent) = ...
```

`GetMPMDir()` returns `filepath.Join(workspace, "mpm")` which gives `.../mpm`.

## Customization

### Via environment:

```bash
export MPM_WORKSPACE=/opt/mpm
```

### Via config file:

```json
// $MPM_WORKSPACE/mpm/mpm_config.json
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
