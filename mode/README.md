# Modes

Modes define *how* the AI should approach a task — its mindset, rules, and style.

## Creating a Mode

Duplicate `_template.json` and name it after your mode (e.g., `strategist.json`).

## Fields

| Field | Required | Description |
|-------|----------|-------------|
| `sym_id` | Yes | Unique ID, e.g. `<name>` |
| `title` | Yes | Human-readable title |
| `name` | Yes | Short name for display |
| `subtitle` | No | One-line descriptor |
| `vibe` | Yes | 1-2 sentence character description |
| `purpose` | Yes | What this mode is for |
| `voice` | No | How it speaks |
| `behavioral_rules` | Yes | Array of rules to follow |
| `best_practices` | No | Array of tips |
| `anti_patterns` | No | What to avoid |
| `key_quotes` | No | Relevant quotes (shown in preview) |
| `exit_criteria` | No | When NOT to use this mode |
| `reference` | No | Path to a reference doc (e.g. `reference/The_Prince.txt`) |
| `created_by` | No | Attribution |
| `created_date` | No | Date created |

## Activate a Mode

```bash
mpm mode set <name>
mpm mode add <name>    # add to active modes (multi-mode supported)
```

## Notes

- Modes are auto-compiled when added/removed via CLI
- You can combine multiple modes: `mpm mode add research && mpm mode add writing`
- The `reference` field links to reference docs for RAG context
