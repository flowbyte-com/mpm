# Persona Commands

> 🎭 **Behavioral templates.** Personas define *who* the assistant is — personality, tone, and decision-making style.

## Overview

Personas are behavioral templates that shape how the AI assistant responds. Unlike modes (which adjust *how* the assistant behaves), personas define *who* the assistant is.

### Persona vs Mode

| Aspect | Persona | Mode |
|--------|---------|------|
| Defines | **Identity** — who the assistant is | **Behavior** — how the assistant acts |
| Scope | Global personality traits | Specific situational adjustments |
| Examples | `developer`, `support`, `researcher` | `debug`, `concise`, `technical` |
| Stackable | One active at a time | Multiple can stack |
| Persistence | Long-term | Session-based |

### How Personas Work

When you set a persona:
1. Assistant loads persona traits from JSON config
2. Traits influence response tone, vocabulary, and structure
3. Persona is recorded in session metadata

---

## CLI Commands

### List Personas
```bash
mpm persona list
```

Shows all configured personas.

**Example output:**
```
$ mpm persona list

┌─────────────────────────────────────────────────────────────┐
│ Available Personas                                         │
├─────────────────────────────────────────────────────────────┤
│ ● default     | The standard assistant                     │
│ ○ developer   | Technical, code-focused responses          │
│ ○ support     | Helpful, patient, clear explanations       │
│ ○ researcher  | Thorough, citation-friendly                │
└─────────────────────────────────────────────────────────────┘

● = active  ○ = available
```

### Create Persona
```bash
mpm persona create <name>
```

Creates a new persona. Opens interactive prompt for configuration.

**Options:**
- `name` — Unique identifier
- `description` — Brief description
- `traits` — Comma-separated personality traits

### Set Active Persona
```bash
mpm persona set <name>
```

Sets the active persona. The assistant adopts these traits immediately.

### Quick Switch
```bash
mpm p <name>
```

Direct persona switch from command line.

**Examples:**
```bash
mpm p default      # Switch to default persona
mpm p developer    # Switch to developer persona
mpm p support      # Switch to support persona
```

### Get Active Persona
```bash
mpm persona active
```

Shows the currently active persona.

**Example output:**
```
$ mpm persona active

Active: developer
Traits: technical, code-focused, concise
```

### Delete Persona
```bash
mpm persona delete <name>
```

Removes a persona. Prompts for confirmation.

> ⚠️ Cannot delete the active persona. Switch away first.

---

## Persona Properties

| Property | Type | Description |
|----------|------|-------------|
| `name` | TEXT | Unique identifier |
| `description` | TEXT | Brief description of the persona |
| `traits` | JSON | Array of personality traits |
| `created` | DATETIME | Creation timestamp |
| `is_active` | BOOLEAN | Current active flag |

### Example Persona JSON

```json
{
  "name": "developer",
  "description": "Technical, code-focused responses",
  "traits": [
    "technical",
    "code-focused",
    "concise",
    "uses-code-blocks",
    "prefers-terminal"
  ],
  "created": "2026-03-28T10:00:00Z",
  "is_active": true
}
```

---

## Persona Templates

### Default Persona
```json
{
  "name": "default",
  "description": "The standard assistant",
  "traits": ["helpful", "friendly", "general-purpose"]
}
```

### Developer Persona
```json
{
  "name": "developer",
  "description": "Technical, code-focused responses",
  "traits": [
    "technical",
    "code-focused",
    "concise",
    "uses-code-blocks",
    "prefers-terminal",
    "explains-tradeoffs"
  ]
}
```

### Support Persona
```json
{
  "name": "support",
  "description": "Patient, clear customer support",
  "traits": [
    "patient",
    "clear",
    "step-by-step",
    "uses-examples",
    "confirms-understanding"
  ]
}
```

### Researcher Persona
```json
{
  "name": "researcher",
  "description": "Thorough, citation-friendly",
  "traits": [
    "thorough",
    "citation-friendly",
    "balances-perspectives",
    "identifies-uncertainties",
    "links-sources"
  ]
}
```

---

## Persona-Mode Interaction

Personas and modes work together:

```
┌─────────────────────────────────────────────────────────────┐
│              LAYERED BEHAVIOR EXAMPLE                       │
├─────────────────────────────────────────────────────────────┤
│                                                             │
│  Layer 1: PERSONA (base identity)                            │
│  └─ developer: technical, code-focused, concise             │
│                                                             │
│  Layer 2: MODE STACK (situational adjustments)              │
│  └─ +debug: verbose internal state                          │
│  └─ +technical: even more technical                         │
│                                                             │
│  Result: Code-focused, technical, concise responses         │
│          with verbose debug information when needed         │
│                                                             │
└─────────────────────────────────────────────────────────────┘
```

### Conflict Resolution

| Persona says | Mode says | Resolution |
|--------------|-----------|------------|
| Be friendly | Be concise | Mode wins (situational) |
| Use simple words | Use technical terms | Mode wins (mode is intentional override) |
| Long responses | Short responses | Mode stack order: last mode wins |

---

## File Structure

Personas are stored as JSON files:

```
$HOME/.openclaw/workspace/persona/
├── default/
│   └── persona.json
├── developer/
│   └── persona.json
└── support/
    └── persona.json
```

### Creating Personas Manually

Create the directory and JSON file:

```bash
mkdir -p ~/.openclaw/workspace/persona/my_persona
```

```json
{
  "name": "my_persona",
  "description": "Custom persona description",
  "traits": ["trait1", "trait2"],
  "created": "2026-03-29T00:00:00Z"
}
```

---

## Integration

| Component | Integration |
|-----------|------------|
| **Modes** | Stack on top of persona traits |
| **Sessions** | Persona recorded in session metadata |
| **Memory** | Persona context stored with memories |
| **Status** | `mpm status` shows active persona |

---

## Examples

### Create and Use a Persona
```bash
# Create new persona
mpm persona create researcher

# Configure traits via file
# Edit: ~/.openclaw/workspace/persona/researcher/persona.json

# Activate
mpm persona set researcher
```

### Quick Persona Switch
```bash
# Check current
mpm persona active

# Quick switch
~p.developer

# Switch back
mpm p default
```

### View All Personas
```bash
mpm persona list
```

---

## Troubleshooting

| Issue | Solution |
|-------|----------|
| `Persona not found` | Create with `mpm persona create <name>` or check name |
| `Cannot delete active persona` | Switch to another first: `mpm persona set default` |
| `Persona has no effect` | Check session is using persona; restart if needed |
| `Traits not loading` | Validate JSON syntax; check file permissions |

---

**Last Updated:** 2026-03-29  
**MPM Version:** 6.0.0
