# Mode Commands

> 🎛️ **Behavioral toggles.** Modes define *how* the assistant behaves — situational adjustments that stack.

## Overview

Modes are **lightweight behavioral toggles** that modify how the assistant operates. Unlike personas (which define *who* the assistant is), modes define *how* it behaves in specific situations.

### Mode vs Persona

| Aspect | Mode | Persona |
|--------|------|---------|
| Defines | **Behavior** — how the assistant acts | **Identity** — who the assistant is |
| Scope | Situational adjustments | Global personality traits |
| Examples | `debug`, `concise`, `technical` | `developer`, `support`, `researcher` |
| Stackable | Multiple can stack simultaneously | One active at a time |
| Persistence | Session-based (until cleared) | Long-term |
| Priority | Modes override persona traits | Persona provides foundation |

---

## CLI Commands

### List Modes
```bash
mpm mode list
```

Shows all configured modes.

**Example output:**
```
$ mpm mode list

┌─────────────────────────────────────────────────────────────┐
│ Available Modes                                            │
├─────────────────────────────────────────────────────────────┤
│ ○ debug      | Verbose output, show internal state         │
│ ○ concise    | Short responses, less explanation           │
│ ○ detailed   | Thorough responses, more examples           │
│ ○ technical  | Technical terminology, precise language    │
│ ○ casual     | Friendly, conversational tone                │
└─────────────────────────────────────────────────────────────┘
```

### Create Mode
```bash
mpm mode create <name>
```

Creates a new mode. Interactive prompt for configuration.

### Add Mode to Stack
```bash
mpm mode add <name>
```

Activates a mode (adds to the current stack of active modes).

**Example:**
```bash
$ mpm mode add debug
Added: debug

$ mpm mode add technical
Added: technical

$ mpm mode active
Active modes: [debug, technical]
```

### Quick Add
```bash
mpm m add <name>
```

Quick mode add from command line.

**Examples:**
```bash
mpm m add debug       # Add debug mode
mpm m add concise     # Add concise mode
mpm m add technical   # Add technical mode
```

### Clear All Modes
```bash
mpm m clear
```

Removes all active modes, returning to base persona behavior.

### Get Active Modes
```bash
mpm mode active
```

Shows currently active modes.

### Delete Mode
```bash
mpm mode delete <name>
```

Removes a mode configuration entirely.

---

## Mode Properties

| Property | Type | Description |
|----------|------|-------------|
| `name` | TEXT | Unique identifier |
| `description` | TEXT | What the mode does |
| `created` | DATETIME | Creation timestamp |
| `enabled` | BOOLEAN | Whether mode is in active stack |

---

## Common Mode Use Cases

| Mode | Purpose | Effect |
|------|---------|--------|
| `debug` | Debugging issues | Verbose output, show internal state, reveal reasoning |
| `concise` | Quick answers | Short responses, minimal explanation |
| `detailed` | Learning something new | Thorough responses, many examples |
| `technical` | Technical discussions | Precise terminology, assumes domain knowledge |
| `casual` | Relaxed conversation | Friendly tone, informal language |
| `sandbox` | Testing | Allows destructive operations, overrides safeguards |

### When to Use Each Mode

| Situation | Mode | Why |
|-----------|------|-----|
| Fixing a bug | `debug` | See internal state, trace execution |
| Quick question | `concise` | Don't need full explanation |
| Learning a topic | `detailed` | Get comprehensive understanding |
| Code review | `technical` | Precise technical discussion |
| Friendly chat | `casual` | Match conversational vibe |
| Testing new code | `sandbox` | Bypass confirmation prompts |

---

## Stacking Modes

Multiple modes can be active simultaneously. Effects **compound**.

### Stacking Examples

| Stack | Effect |
|-------|--------|
| `debug` | Verbose internal state |
| `concise` | Short responses |
| `debug + concise` | Short responses with verbose internal state |
| `debug + technical` | Technical verbose output |
| `detailed + technical` | Thorough technical responses |
| `debug + concise + technical` | Short technical responses with debug info |

### Conflict Resolution

| Modes | Conflict | Resolution |
|-------|----------|------------|
| `concise` vs `detailed` | Response length | Last-added wins |
| `casual` vs `technical` | Tone | Last-added wins |
| `debug` + anything | Debug info added | Always compounds |

**Rule:** When modes conflict, the **last-added mode** takes precedence for that aspect.

### Visual Stacking Diagram

```
┌─────────────────────────────────────────────────────────────┐
│                    MODE STACKING                             │
├─────────────────────────────────────────────────────────────┤
│                                                             │
│  Base: Persona traits (e.g., "developer")                     │
│       │                                                     │
│       ▼                                                     │
│  ┌─────────┐                                                │
│  │ +debug  │ ──── Add debug mode                           │
│  └────┬────┘                                                │
│       │                                                     │
│       ▼                                                     │
│  ┌─────────────┐                                            │
│  │ +concise    │ ──── Add concise (overrides detailed)      │
│  └──────┬──────┘                                            │
│         │                                                  │
│         ▼                                                  │
│  ┌─────────────────┐                                       │
│  │ Final behavior:  │                                       │
│  │ • From debug:    │                                       │
│  │   - Verbose      │                                       │
│  │   - Show state   │                                       │
│  │ • From concise:  │                                       │
│  │   - Short        │                                       │
│  │   - Minimal      │                                       │
│  └─────────────────┘                                       │
│                                                             │
└─────────────────────────────────────────────────────────────┘
```

---

## Persona-Mode Interaction

Personas provide the foundation; modes adjust behavior:

```
┌─────────────────────────────────────────────────────────────┐
│              LAYERED BEHAVIOR STACK                         │
├─────────────────────────────────────────────────────────────┤
│                                                             │
│  Layer 1: PERSONA (base identity)                            │
│  └─ "developer" → technical, code-focused, concise          │
│                                                             │
│  Layer 2: MODE STACK (situational)                           │
│  └─ +debug → adds verbose output                            │
│  └─ +technical → reinforces technical tone                  │
│                                                             │
│  Combined: Technical, code-focused, concise responses        │
│            with debug information when enabled             │
│                                                             │
└─────────────────────────────────────────────────────────────┘
```

| Persona | Mode | Result |
|---------|------|--------|
| `developer` | `concise` | Code-focused, short answers |
| `support` | `debug` | Patient, clear, with internal state |
| `researcher` | `detailed` | Thorough, citation-friendly |

---

## File Structure

Modes are stored as JSON files:

```
/home/node/.openclaw/workspace/mode/
├── debug/
│   └── mode.json
├── concise/
│   └── mode.json
└── detailed/
    └── mode.json
```

### Example Mode JSON

```json
{
  "name": "debug",
  "description": "Verbose output, show internal state",
  "traits": [
    "verbose",
    "show-internal-state",
    "reveal-reasoning"
  ],
  "created": "2026-03-28T10:00:00Z",
  "enabled": false
}
```

---

## Examples

### Enable Debug Mode
```bash
mpm mode add debug
```

### Stack Modes
```bash
# Add multiple modes
mpm m add debug
mpm m add technical

# Check active stack
mpm mode active
```

### Use Concise Mode
```bash
# For quick answers
mpm m add concise

# Ask your question

# Clear when done
mpm m clear
```

### Clear All Modes
```bash
mpm m clear
```

Returns to base persona behavior.

---

## Troubleshooting

| Issue | Solution |
|-------|----------|
| Mode not found | Create with `mpm mode create <name>` |
| Mode has no effect | Check it's in active stack (`mpm mode active`) |
| Conflicting modes | Last-added wins; use `mpm m clear` and re-add in order |
| Modes persist | Modes clear with `mpm m clear`; persona persists |
| Too verbose | `mpm m add concise` to counter `debug` |

---

**Last Updated:** 2026-03-29  
**MPM Version:** 6.0.0
