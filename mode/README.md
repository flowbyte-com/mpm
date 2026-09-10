# Creating a New Mode

Modes are `.md` files in this directory with YAML frontmatter. No database,
no compile step. The router hot-reloads on directory mtime change.

> This guide file itself is inert: it has no `patterns`, so the auto-router
> never selects it, and it has no `name`/`title`, so `mpm mode list` skips it.

## Minimal template

```markdown
---
name: my-mode
patterns: "keyword one, keyword two, phrase three"
domain_out: "unrelated thing, other specialty"
retrieval_limit: 7
retrieval_threshold: -1.5
---

# My Mode

One paragraph: what operating context this mode covers.

## Operating Rules

1. **Concrete rule.** ...
2. ...

## Output Format

How responses in this mode are shaped.

## Before / After

**Default mode:** "..."

**My mode:** "..."

## Retrieval Tuning Rationale (maintainers)

Why these numbers and when to change them.
```

## What each frontmatter field does

| Field | Read by | Effect |
|---|---|---|
| `name` | router, `ModeManager` | Identity. Lowercase `[a-z0-9_-]+`. Required (or `title`). Filename stem is the fallback — prefer explicit `name`. |
| `patterns` | router only | **Routing signal.** Each comma-separated entry is compiled as a case-insensitive regex (`(?i)` + raw text, so regex syntax works and spaces match literally). **+2 score per match.** A mode activates when its net score reaches >= 1. Modes are multi-select: several can fire on one prompt. |
| `domain_out` | router only | **Refusal signal.** Each entry is treated as *literal prose* (wrapped in word boundaries + `regexp.QuoteMeta`). **−1 per match.** Do NOT put regex syntax here — the linter rejects it. Use prompt-vocabulary phrases that mean "a different specialist should win". |
| `voice_guards` | nobody at runtime | Stored, never compiled, never scored. Injected for LLM context only. Optional on modes (personas use it more). |
| `retrieval_limit` | `mpm hint` path | Max decision/theory rows returned. First active mode wins. Default 5 when <= 0. Current trio: debugging 3, default 7, architect 10. |
| `retrieval_threshold` | `mpm hint` path | BM25 gate on decisions/theories (`keywords.go`). **Raw BM25 is negative with more-negative = better; only rows scoring *below* the threshold survive.** So more-negative = *stricter*, less-negative = *looser*. −2.5 admits a strict subset of −1.5. (The `hybrid_search.go` "lower = more results" comment describes a different score scale and does not govern this number.) Default −1.0 when 0. |
| `title`, `version`, `status`, `purpose`, `description`, `checklist`, `tools` | `ModeManager` (parsed, mostly display) | `version` must start with a digit if present. `purpose`/`description` surface in some render paths. None affect routing. |

The markdown **body is not a routing signal** (body-word inference was removed;
frontmatter `patterns` is the only routing input). But the body **is injected**:
on a match, `renderRoute` (`cmd/mpm/route_render.go`) pastes the *whole file*
into a `<system-reminder>` block. Write operating rules, output format, and a
before/after pair — that body is what actually changes agent behavior.

## Rules the linter enforces (`mpm ops lint`)

1. Frontmatter must parse as YAML (watch the footguns: `\b` in double-quoted
   strings becomes backspace; `\'` breaks single-quoted strings).
2. Every `patterns` entry must compile as `(?i)<entry>`.
3. Every `domain_out` entry must compile both QuoteMeta-wrapped *and* raw —
   i.e. keep it plain prose, no brackets or regex.

The linter does NOT check required fields, thresholds, or length. Keep the
whole file tight: mode + persona bodies share a 9,500-char injection cap, and
on overflow the **persona is dropped first**.

## Overlap checklist (modes are multi-select, so overlap is allowed, not free)

- If your patterns also hit another mode's `domain_out`, say which should win
  and why (e.g. debugging's `domain_out` refuses `design, research, plan` so
  those route to architect even when `bug`/`fix` also match).
- If nothing matches a prompt, *no* mode injects (there is no guaranteed
  no-match fallback at route time; the agent falls back to whatever is in
  `active.json`). Document whether your mode is ever expected to be that
  standing default — only `default` plays that role.
- Keep `name` lowercase, double-quote `patterns`/`domain_out`, match the
  existing section layout (`Operating Rules`, `Output Format`, `Before / After`,
  `Retrieval Tuning Rationale`).

## Activating

```bash
mpm ops lint --dir ./mode --show-clean   # must pass before anything else
mpm mode add my-mode                     # persist to active.json
mpm ops stance assume my-mode default "reason for the switch"  # one-turn hot-swap (requires auto)
```
