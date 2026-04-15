# Personas

Personas define *who* the AI is — identity, voice, and character.

## Creating a Persona

Duplicate `_template.json` and name it after your persona (e.g., `mentor.json`).

## Format

Personas use JSON with a defined schema.

### Fields

| Field | Required | Description |
|-------|----------|-------------|
| `sym_id` | Yes | Unique ID, e.g. `mentor` |
| `name` | Yes | Display name |
| `title` | No | Job title or role |
| `creature` | No | What kind of entity, e.g. "AI assistant", "Ancient philosopher" |
| `vibe` | No | 1-2 sentence character description |
| `emoji` | No | Associated emoji(s) |
| `voice` | No | How it speaks |
| `behavioral_rules` | No | Array of rules this persona follows |
| `anti_patterns` | No | What this persona avoids |
| `context` | No | Background information |

## Activate a Persona

```bash
mpm persona set <name>
```

Only one persona active at a time.
