---
name: mpm
description: MPM (Memory Persistence Module) — wake context, long-term memory, lessons, topics, decisions, theories, references. Use when the user is working on MPM, OpenClaw, or any long-running project where prior decisions, evidence, and lessons matter.
---

# MPM Workflow

You have access to a suite of MPM tools (prefixed `mpm_`). Follow this workflow every session.

## On Session Start (DO THIS FIRST)

1. **Wake context is your anchor.** When you need to know what the user was doing before you arrived, call `mpm_read_wake_context` immediately. If it's empty, tell the user you are ready for context — do not invent prior state. This is the single most important instruction in this skill; it prevents hallucinated context on the first turn.
2. **Read directives.** Call `mpm_read_directives` to learn the user's prime directives (what you must and must not do).

## Before Answering

Before answering any question about prior work, decisions, dates, people, preferences, or todos, call `mpm_query_long_term_memory` with a natural-language query.

## After Non-Trivial Actions

- After learning something reusable: `mpm_save_lesson` (type: `insight` / `practice` / `warning`).
- After any non-obvious choice: `mpm_record_decision` (context, choice, rationale — rationale is the most important field).
- After any durable fact: `mpm_save_to_memory`.

## Hypothesize Before Fixing

Before non-obvious fixes:
1. Call `mpm_propose_theory` with an explicit, executable validation criterion.
2. Run the test.
3. Call `mpm_resolve_theory` with `proven` or `disproven` + the conclusion.

## Timeouts

If a tool call times out, the MCP server has a 15s default. Pass `timeout_override_ms` (e.g. 60000) for slow operations: `mpm_add_reference`, `mpm_propose_theory`, `mpm_resolve_theory`. The server caps any override at 5 minutes.
