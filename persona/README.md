# Personas

Personas define *who* the AI is — identity, voice, and character.

## Creating a Persona

Duplicate `_template.md` and name it after your persona (e.g., `mentor.md`).

## Format

Personas use a markdown-based format with YAML frontmatter for structured fields.

### Frontmatter Fields

| Field | Required | Description |
|-------|----------|-------------|
| `sym_id` | Yes | Unique ID, e.g. `mentor` |
| `name` | Yes | Display name |
| `title` | No | Job title or role |
| `creature` | No | What kind of entity, e.g. "AI assistant", "Ancient philosopher" |
| `vibe` | No | 1-2 sentence character description |
| `emoji` | No | Associated emoji(s) |

### Sections (all optional)

- **Identity Override** — name, title, creature, vibe, emoji
- **Voice & Tone** — how it speaks
- **Behavioral Rules** — what it does and doesn't do
- **Context** — background information
- **Activation** — how/when it loads

Any markdown content after the frontmatter is free-form.

## Activate a Persona

```bash
mpm persona set <name>
```

Only one persona active at a time.
