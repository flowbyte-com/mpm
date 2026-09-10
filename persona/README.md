# Creating a New Persona

Personas are `.md` files in this directory with YAML frontmatter. No database,
no compile step. The router hot-reloads on directory mtime change.

> This guide file itself is inert: it has no `patterns`, so the auto-router
> never selects it, and it has no `name`/`title`, so `mpm persona list` skips it.

## Minimal template

```markdown
---
name: my-persona
title: Short Role Name
vibe: adjective, adjective, adjective
voice: how, it, sounds
patterns: "review, critique, examine"
domain_out: "praise, casual chat"
voice_guards: "One-line output constraint. What this persona must never do."
---

# My Persona

One paragraph: the identity in one breath.

## Operating Rules

1. **Concrete, checkable rule.** Not a vibe adjective — operationalize it.
2. ...

## Output Format

The exact scaffolding this persona always/never emits.

## Before / After

**Generic (default persona):** "..."

**My persona:** "..."

## Hand-off

When to switch away, via `mpm ops stance assume - <persona> "<rationale>"`.
```

## What each frontmatter field does

| Field | Read by | Effect |
|---|---|---|
| `name` | router, `PersonaManager` | Identity. Lowercase `[a-z0-9_-]+`. Required (or `title`). Filename stem is the fallback — prefer explicit `name`. |
| `title`, `vibe`, `voice` (+ `creature`, `emoji`, `description`) | nobody at runtime | Injected for LLM context only. No routing effect. Keep them honest — the body must operationalize these adjectives, not repeat them. |
| `patterns` | router only | **Routing signal.** Each comma-separated entry is compiled as a case-insensitive regex (`(?i)` + raw text). **+2 per match.** Personas are **max-pool, single-select**: the highest scorer wins, ties break toward the earliest-loaded file (alphabetical), and **there is no implicit default fallback** — no match means no persona injects. |
| `domain_out` | router only | **Refusal signal.** Literal prose (word boundaries + `regexp.QuoteMeta`), **−1 per match**, logged as `penalties_applied`. No regex syntax (the linter rejects it). This is how a specialist yields: e.g. critic refuses `praise, support, agree` so encouragement prompts don't get interrogated. |
| `voice_guards` | nobody at runtime | Never compiled, never scored — pure LLM-context output constraints. One line is enough; the body carries the detail. |

The markdown **body is not a routing signal** (frontmatter `patterns` is the
only routing input). But the body **is injected**: on a match, `renderRoute`
(`cmd/mpm/route_render.go`) pastes the *whole file* into a `<system-reminder>`
block. The body must contain checkable behavior:

- 3–5 concrete rules (e.g. "state an alternative hypothesis before accepting
  any claim" beats "be skeptical").
- Output formatting: what scaffolding this persona always emits (objection
  list? timeline → causal chain → conclusion?) and what it suppresses.
- A before/after example pair so the difference is unambiguous to the next
  human reader, not just the live model.
- Hand-off notes **only** to real targets via `mpm ops stance assume`
  (`-` keeps the current mode). Personas don't implement fixes — say which
  persona executes when the investigation ends.

## Rules the linter enforces (`mpm ops lint`)

1. Frontmatter must parse as YAML (footguns: `\b` in double quotes becomes
   backspace; `\'` breaks single-quoted strings).
2. Every `patterns` entry must compile as `(?i)<entry>`.
3. Every `domain_out` entry must compile both QuoteMeta-wrapped *and* raw —
   plain prose only.

Not checked: required fields, voice content, length. Keep the file tight: mode
+ persona bodies share a 9,500-char injection cap, and on overflow the
**persona is dropped first** — the highest-leverage lines are the rules and
the output format, not the examples.

## Overlap checklist (personas are winner-takes-all, so overlap matters)

- If two personas can match one prompt (e.g. `test` hits critic while
  `timeline`/`logic` hit forensic), the higher net score wins — add a
  `domain_out` entry to the one that should yield, and verify with a dry run.
- If no persona matches, nothing injects (the agent keeps whatever persona is
  in `active.json`, typically `default`). Don't rely on routing to install a
  fallback.
- Keep `name` lowercase, double-quote `patterns`/`domain_out`/`voice_guards`,
  match the existing section layout (`Operating Rules`, `Output Format`,
  `Before / After`, `Hand-off`).

## Activating and verifying

```bash
mpm ops lint --dir ./persona --show-clean   # must pass before anything else
mpm persona set my-persona                  # persist to active.json
mpm ops stance assume - my-persona "reason" # one-turn hot-swap (requires auto)
```
