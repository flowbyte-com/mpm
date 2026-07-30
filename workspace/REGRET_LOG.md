# REGRET LOG

> Append-only record of things the agent considered but didn't add, and things the agent added that turned out not to matter. The empirical record of feature-creep pressure, and the source of truth for what to delete next.

**Created:** 2026-06-30 (per README §7.1).
**This file:** initial scaffolding committed on 2026-07-30 to satisfy the README reference.

## Format

Each entry is one Markdown heading followed by a 2-5 line summary:

```markdown
### YYYY-MM-DD — <short title>
<status>: <what was considered / added>.
<why it was rejected / why it failed to matter>.
<what to do instead>.
```

Status prefix is one of:

- `CONSIDERED-REJECTED` — feature considered, declined. Records *why not*, so the same proposal can be deflected faster next time.
- `SHIPPED-DEAD` — feature shipped, did not pull its weight. Records *the deletion path*, so a future agent knows it has permission to remove.
- `KEPT-AS-EXPECTED` — the rare case where the regret log has nothing to say. Use sparingly; the file's value comes from negative entries.

## Why this file exists

Default to no. The threshold for adding is high. The threshold for removing is the same. This log is the audit trail that keeps both honest.

## Entries

*(none yet — first appends land here)*
