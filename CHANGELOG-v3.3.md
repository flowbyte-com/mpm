# MPM v3.3 Change Log

## Fixed Issues

### Critical Security Fixes
1. **SQL Injection Protection**
   - Added `validate_id()` function to all scripts with SQL queries
   - Implemented `escape_sql()` for dynamic values
   - Protected: `persona-activate.sh`, `mode-stack.sh`, `persona-compile.sh`, `mode-compile.sh`, `mpm-watch.sh`

2. **Path Injection Prevention**
   - All file paths now validated with `[[ "$path" =~ \.\./ ]]` checks
   - Directory traversal attacks blocked

3. **Race Condition Fixes**
   - Mode stack operations now use atomic `COALESCE` queries
   - Concurrent mode stacking no longer loses data

### Terminal UI Improvements

#### Help Text Redesign
- **Structured Layout**: Commands grouped with visual separators
- **Quicklink Integration**: All ~p/~m/~k shortcuts documented inline
- **Color Coding**: 
  - 📖 Header
  - 📋 Section headers  
  - ⚡ Quicklink callouts
  - ═══ Visual separators

#### Example Commands
```
📖 FLOWBYTE MPM KERNEL v3.2
═══════════════════════════════════════════════════════════════
📋 CORE COMMANDS
═══════════════════════════════════════════════════════════════

  mpm persona   <subcommand>  [Identity Management - ~p]
                ─────────────────────────────────────────────────
                list|ls       - Show all available personas
                select        - Open interactive TUI selector
                ...

                ⚡ QUICKLINKS: ~p           (TUI selector)
                             ~p.<name>    (direct activate)
```

## What's New

### SymAI Bytecode Generator
- `mpm bytecode persona <name>` → generates `~p.X!act` codes
- `mpm bytecode mode <name>` → generates `~m.XX!act` codes
- `mpm bytecode list` → shows all available codes
- `mpm bytecode check` → verifies SymAI availability

### Quicklinks Reference
| Category | Quicklink | Command |
|----------|-----------|---------|
| Persona | `~p.oracle` | Activate Oracle persona |
| Mode | `~m.programming` | Add Programming mode |
| Mode Stack | `~m+debugging` | Stack Debugging mode |
| Mode Clear | `~m.clr` | Clear all modes |
| Memory | `~k.con` | Consolidate memories |
| Memory | `~k?search` | Query memories |

---
Last Updated: 2026-03-23
