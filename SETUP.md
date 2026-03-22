# MPM Quick Setup

**Memory-Persona-Mode** — Token-optimized agent state management.

## Installation

### Option 1: Enable via CLI (recommended)
```bash
openclaw skills enable MPM
```

### Option 2: Manual config edit
Add to `~/.openclaw/openclaw.json`:
```json
"skills": {
  "entries": {
    "MPM": { "enabled": true }
  }
}
```

### Verify it's working
```bash
mpm                    # Show status dashboard
mpm --help             # Show all commands
```

## First Run

```bash
# Initialize MPM structure (if not exists)
mpm memory refresh

# Check status
mpm                    # Full dashboard
mpm persona active     # Current persona
mpm mode active        # Stacked modes
```

## Quick Start

### Memory (8 categories)
```bash
# Capture important info
cd MPM/memory
./capture.sh --lesson "Always test before release"
./capture.sh --project "My awesome project"
./capture.sh --idea "DB-first architecture"

# Review
./capture.sh --list    # Show all categories
./capture.sh --sync    # Force sync to DB
```

### Personas (one at a time)
```bash
mpm persona list       # Available personas
mpm persona load corporate  # Switch persona
mpm persona active     # Show current
mpm persona clear      # Revert to default
```

### Modes (stackable)
```bash
mpm mode list          # Available modes
mpm mode load programming  # Add to stack
mpm mode load debugging    # Stack multiple
mpm mode active        # Show stack
mpm mode clear         # Clear all
```

## File Structure

```
MPM/
├── memory/            # 8 categorized .md files + DB
│   ├── lesson.md
│   ├── project.md
│   ├── idea.md
│   ├── decision.md
│   ├── preference.md
│   ├── contact.md
│   ├── subject.md
│   ├── tool.md
│   └── memory.db
├── persona/           # Persona files + DB
│   ├── *.persona
│   └── personas.db
└── mode/              # Mode files + DB
    ├── *.mode
    └── m3.db
```

## Core Principle

> *"Memory is sacred."*

Treat what you remember with care. Curate, don't hoard.

## Created by

v & Great_808@flowbte.com | 2026-03-20
