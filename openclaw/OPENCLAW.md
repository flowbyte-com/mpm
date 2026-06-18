# MPM + OpenClaw Integration

MPM (Memory Persistence Module) integrates with OpenClaw as a plugin, exposing the full Go backend capability set as native function-calling tools. The plugin calls the Go binary directly via `child_process` — no MCP intermediary.

---

## What Gets Installed

| File | Location |
|------|----------|
| Plugin source | `/home/v/workspace/projects/mpm/openclaw/mpm-plugin/src/` |
| Bundled entry | `/home/v/workspace/projects/mpm/openclaw/mpm-plugin/dist/index.js` |
| Plugin config | `/home/v/workspace/projects/mpm/openclaw/mpm-plugin/openclaw.plugin.json` |

---

## Prerequisites

- MPM binary built with FTS5 support (MPM v2.0+)
- OpenClaw Gateway `>= 2026.3.24-beta.2`
- Node.js (runtime for OpenClaw)

---

## Step 1 — Build MPM

```bash
cd /home/v/workspace/projects/mpm
PATH=/usr/local/go/bin:$PATH make build
# Produces: bin/mpm
```

Keep the system binary in sync:
```bash
cp /home/v/workspace/projects/mpm/bin/mpm /usr/local/bin/mpm
```

---

## Step 2 — Install the Plugin

The plugin lives at `/home/v/workspace/projects/mpm/openclaw/mpm-plugin/`. It is already configured; no manual creation needed.

### `openclaw.plugin.json` (already configured — 21 tools registered)

```json
{
  "id": "mpm",
  "name": "MPM (Memory Persistence Module)",
  "description": "Long-term memory tool for 808 — full SQLite-backed persistence layer with lessons, topics, references, and directives.",
  "version": "2.0.0",
  "contracts": {
    "tools": [
      "add_evidence",
      "add_reference",
      "challenge_memory",
      "create_topic",
      "link_topic",
      "list_evidence",
      "list_lessons",
      "list_references",
      "proactive_recall_hint",
      "propose_theory",
      "query_confidence_history",
      "query_long_term_memory",
      "read_directives",
      "read_wake_context",
      "record_decision",
      "resolve_theory",
      "save_lesson",
      "save_to_memory",
      "search_lessons",
      "search_references",
      "search_topics"
    ]
  },
  "activation": {
    "onStartup": true
  }
}
```

---

## Step 3 — Configure `openclaw.json`

Add to `plugins.entries` and `plugins.load.paths`:

```json
{
  "plugins": {
    "entries": {
      "mpm": {
        "enabled": true,
        "config": {
          "binary": "/home/v/workspace/projects/mpm/bin/mpm",
          "workspace": "/home/v/workspace/projects/mpm"
        }
      }
    },
    "load": {
      "paths": [
        "/home/v/workspace/projects/mpm/openclaw/mpm-plugin"
      ]
    }
  },
  "tools": {
    "profile": "coding"
  }
}
```

**Note:** Do **not** use an explicit `tools.allow` array — it blocks dynamic plugin tool hydration and crashes subagents. Let the Gateway inject tools dynamically via the profile.

---

## Step 4 — Verify

### Check plugin is loaded

```bash
openclaw plugins list
# Should show: mpm | enabled | openclaw | .../mpm-plugin/dist/index.js
```

### Inspect runtime tools

```bash
openclaw plugins inspect mpm --runtime --json | python3 -c "
import sys, json
d = json.load(sys.stdin)
p = d['plugin']
print('Tool names:', p.get('toolNames', []))
print('Status:', p.get('status'))
print('Activated:', p.get('activated'))
"
```

Expected output — all 21 tools:
```
Tool names: ['query_long_term_memory', 'save_to_memory', 'save_lesson', 'search_lessons',
             'list_lessons', 'create_topic', 'search_topics', 'link_topic',
             'add_reference', 'search_references', 'list_references',
             'read_wake_context', 'read_directives', 'record_decision',
             'proactive_recall_hint', 'propose_theory', 'resolve_theory',
             'challenge_memory', 'add_evidence', 'list_evidence', 'query_confidence_history']
Status: loaded
Activated: True
```

### Restart Gateway

Config changes to plugin tool exposure require a Gateway restart:
```bash
openclaw gateway restart
```

---

## Tool Reference (21 tools total)

### Core Memory (3 tools)

**`query_long_term_memory`**
```
query: "any natural language search"
limit: 5  (optional, default 5)
```

**`save_to_memory`**
```
fact: "The fact, lesson, or decision to persist"
tags: ["tag1", "tag2"]  (optional)
weight: 0.5  (optional, 0.0-1.0)
ttl: "24h"  (optional, '0' for permanent, or a Go duration like '72h')
collection: "memories"  (optional, default 'memories')
```

**`challenge_memory`** — Challenge an existing memory with contradictory evidence. This weakens the memory, creates a pending theory, and logs a decision.
```
memoryId: "memory ID to challenge"
evidence: "Be specific about what changed and why the memory is no longer accurate"
```

### Lessons (3 tools)

**`save_lesson`** — Store a learned insight, warning, or practice
```
fact: "What was learned or observed"
type: "warning" | "practice" | "insight"  (default: insight)
tags: ["tag1", "tag2"]  (optional)
```

**`search_lessons`** — Find lessons by natural language query
```
query: "search string"
```

**`list_lessons`** — List all stored lessons
```
type: "warning" | "practice" | "insight"  (optional filter)
```

### Topics (3 tools)

**`create_topic`** — Create a named memory cluster
```
name: "Topic name"
description: "Optional description"
```

**`search_topics`** — Find topics by name or description
```
query: "search string"
limit: 20  (optional)
```

**`link_topic`** — Link an existing memory to an existing topic
```
memory_id: "memory ID"
topic_id: "topic ID"
```

### References (3 tools)

**`add_reference`** — Ingest a document (PDF, EPUB, MD, TXT, HTML)
```
filepath: "/absolute/path/to/document"  (required)
title: "Optional title"  (optional)
```

**`search_references`** — Full-text search within ingested documents
```
query: "search string"
limit: 5  (optional, default 5)
```

**`list_references`** — List all ingested documents
```
limit: 50  (optional, default 50)
offset: 0  (optional)
```

### System (2 tools)

**`read_wake_context`** — Session bootstrap: mode, persona, recent topics, recent memories. Call immediately on session start.
```
{}  (no parameters)
```

**`read_directives`** — Retrieve prime directives (operating principles)
```
{}  (no parameters)
```

### Epistemology Engine (3 tools)

**`propose_theory`** — Log a hypothesis before writing a fix. Forces confrontation with whether the assumption is actually testable.
```
hypothesis: "What you think is happening"
validationCriteria: "A specific, executable test that would prove or disprove it"
tags: ["tag1", "tag2"]  (optional)
```

**`resolve_theory`** — Close the loop after running the test. Mark proven/disproven, then save as permanent memory if confirmed.
```
theoryId: "theory ID to resolve"
conclusion: "What was concluded after running the validation criteria"
newStatus: "proven" | "disproven"
```

**`record_decision`** — Record an architectural choice with rationale. Call BEFORE or AFTER the decision — not just after.
```
context: "The situation or problem that required a decision"
choice: "What was decided"
rationale: "Why this path over alternatives"
outcome: "Optional: what actually happened"  (optional)
tags: ["tag1", "tag2"]  (optional)
weight: 0.5  (optional)
```

### Cognitive Proactive Recall (1 tool)

**`proactive_recall_hint`** — Checks recent conversation for semantic overlap with decisions and theories. Call after each user message.
```
conversation_text: "Recent conversation context"
max_hints: 3  (optional, default 3)
min_score: -3.0  (optional, lower = stronger match)
```

### Evidence & Confidence (3 tools)

**`add_evidence`** — Record evidence supporting or challenging an artifact. The system recomputes confidence automatically.
```
artifact_id: "Target artifact ID"
artifact_type: "memory" | "theory" | "decision" | "lesson"
type: "observation" | "test" | "reproduction" | "challenge" | "decision_outcome" | "external_reference"
source_group: "Where this originated (e.g., 'user_prompt', 'unit_test_suite', 'terminal_stderr')"
created_by: "Agent identity (e.g., 'openclaw-agent')"
strength: -1.0 to 1.0  (optional, uses calibrated default if omitted)
notes: "Brief human-readable summary"  (optional)
```
> **Critical:** Use `type: "challenge"` when evidence contradicts the artifact.

**`list_evidence`** — Retrieve the complete chronological ledger of evidence for a specific artifact.
```
artifact_id: "Target artifact ID"
artifact_type: "memory" | "theory" | "decision" | "lesson"
```

**`query_confidence_history`** — Retrieve the historical trajectory of an artifact's confidence score.
```
artifact_id: "Target artifact ID"
artifact_type: "memory" | "theory" | "decision" | "lesson"
limit: 10  (optional)
```

---

## Agent Configuration (`AGENTS.md`)

Add this to the agent's `AGENTS.md` to enforce memory discipline:

```markdown
## MPM — Memory Persistence Module (Mandatory)

MPM is your native SQLite-backed persistence layer. You are directly wired into it via your tool schema. It is **not** optional context — it is your primary brain.

**CRITICAL: Mandatory Recall**
Before answering *any* question about prior work, decisions, dates, people, preferences, or todos, you MUST execute the `query_long_term_memory` tool. NEVER claim you don't know something or guess an answer without querying your memory first.
*Note: Use `memory_get` only when a result points to a specific file.*

**CRITICAL: Mandatory Save**
After any non-trivial action, lesson learned, or architectural decision, you MUST execute the `save_to_memory` tool.
- **Tags:** Help later retrieval — use them heavily.
- **Weight:** 0.5 default, higher for important absolute truths.
- **TTL '24h':** Ephemeral session-scoped facts (e.g. current task context).
- **TTL '0':** Permanent memories (user preferences, project state, architectural decisions).

**CRITICAL: Structured Memory**
For lessons, topics, references, and directives — use the dedicated tools:
- `save_lesson` for insights, warnings, and practices
- `create_topic` / `search_topics` for memory clustering
- `add_reference` / `search_references` for ingested documents
- `read_directives` for operating principles

## Cognitive Immune System (Non-Negotiable)

When a user or external source presents facts that **contradict a stored memory**, you MUST:

1. Call `add_evidence` with `type: "challenge"` targeting the memory ID, using the contradicting source as `source_group`
2. Call `challenge_memory` to formally challenge the artifact
3. Then explain the contradiction

Do NOT silently update the memory. Do NOT apologize and rewrite without the evidence trail. The immune response must fire before the explanation.

**Trigger:** Any time a user says something like "actually it's X not Y", presents a debug output showing different behavior than stored, or provides environment/config evidence contradicting a stored fact.

## Session Startup

On every session start, before responding to the user:

Call `read_wake_context` (OpenClaw plugin tool) or run `mpm wake --json` directly to get mode, persona, topics, and recent memories from the MPM database.
```

---

## Troubleshooting

**Tools don't appear in agent schema / "No callable tools remain" error**

Check your `openclaw.json` config. If you are using an explicit `tools.allow` array, it will block dynamic plugin hydration and crash subagents. Remove the `"allow"` array entirely from your `"tools"` block to let the Gateway dynamically inject loaded plugin tools.

**"No such module: fts5" on memory operations**

Rebuild MPM with FTS5 enabled:
```bash
cd /home/v/workspace/projects/mpm
PATH=/usr/local/go/bin:$PATH make build
```

**DB trigger constraint errors**

Stale debug triggers sometimes accumulate. Check:
```bash
sqlite3 /home/v/workspace/projects/mpm/src/db/mpm.db ".triggers"
```
Drop any `test_ai2` or `dbg_ai2` triggers.

**Plugin registered but child_process calls fail**

The dangerous code scanner may block `child_process`. Use `--dangerously-force-unsafe-install` flag when installing, or configure scanner allowlist for the plugin directory.

**`reference ls` returns "no such table: references"**

This was a pre-existing bug — the SQL queries referenced a table named `"references"` but the actual table is `reference_docs`. Fixed in MPM v2.0 build. Rebuild if still seeing this error.
