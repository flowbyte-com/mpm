---
name: mpm
description: FlowByte MPM v3.2. Manage workspace identity (Personas) and behavior (Modes) via SymAI Logic Pipes. Use for high-density context management.
---

# MPM Logic Pipes

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
