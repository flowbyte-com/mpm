---
name: mpm
description: FlowByte MPM v3.2. Manage workspace identity (Personas) and behavior (Modes) via SymAI Logic Pipes. Use for high-density context management.
---

# mpm Logic Pipes

## 🚀 Quick Start

```bash
mpm watch          # Auto-sync persona/mode changes (run in background)
mpm status         # Dashboard overview
```

## 🎭 Persona (~p)
- **Selection:** `~p.[name]!act`
- **Purpose:** Identity/Voice override without touching core files.

## 🛠️ Mode (~m)
- **Stacking:** `~m.[name]&[name]!act`
- **Purpose:** Layering task-specific behaviors (e.g., Programming + Debugging).

## 🧠 Memory (~k)
- **Purification:** `~k.cl` (Strip bloat, protect opcodes)
- **Consolidation:** `~k.con` (Sync sessions to fact-matrix)

## 🚦 System State
Always check `active.json` or run `mpm status` to synchronize with current Bytecode state before responding.

## ⚡ Live Sync

Run `mpm watch` in a terminal to auto-sync changes on save:

```bash
mpm watch   # Background watcher for persona + mode + memory
```

- **Persona:** `~/.openclaw/workspace/MPM/persona/*.persona` → auto-compile
- **Mode:** `~/.openclaw/workspace/MPM/mode/*.mode` → auto-compile  
- **Memory:** `~/.openclaw/workspace/memory/*.md` → auto-sync to DB

---

# Credits

**MPM v3.2** — FlowByte Logic Pipes
- Concept & Architecture: The Great 808
- SQLite-backed persona/mode storage for ~97% token reduction vs raw markdown
- SymAI opcode integration (~p, ~m, ~k prefixes)
- Auto-sync via inotifywait