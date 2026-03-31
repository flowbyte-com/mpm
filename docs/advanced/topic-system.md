# Topic System Guide

> 📚 **Knowledge curation lifecycle.** Topics bridge the gap between raw sessions and hardened memories.

## Overview

The Topic System organizes related memories and sessions into semantic groupings. Topics act as an **intermediate layer** between raw session data and curated long-term memories.

### The Problem It Solves

- **Sessions** are temporary, noisy, and grow unbounded
- **Memories** should be refined, distilled knowledge
- **Topics** provide a curation pipeline: raw → refined → permanent

## Architecture

### Memory Flow Diagram

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                         TOPIC SYSTEM LIFECYCLE                              │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│   ┌──────────────┐         ┌──────────────┐         ┌──────────────┐       │
│   │   SESSION    │────────▶│    TOPIC     │────────▶│    MEMORY    │       │
│   │  (raw data)  │ DISTILL  │ (curated)    │ HARDEN  │ (permanent)  │       │
│   └──────────────┘         └──────────────┘         └──────────────┘       │
│         │                        │                        │                │
│         ▼                        ▼                        ▼                │
│   ┌──────────────┐         ┌──────────────┐         ┌──────────────┐        │
│   │ • content    │         │ • topic      │         │ • content    │        │
│   │ • timestamp  │         │ • description│         │ • embedding  │        │
│   │ • summary    │         │ • source_*   │         │ • tags       │        │
│   └──────────────┘         └──────────────┘         └──────────────┘        │
│                                                                             │
│   RETRIEVAL LOOP: Sessions → Topics → Memory → References                   │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

### Lifecycle Stages

| Stage | Trigger | Result |
|-------|---------|--------|
| **Distillation** | Session hits token limit or concept shift | Session summarized → Topic created |
| **Hardening** | Topic verified 3+ successful references | Topic promoted → Memory stored |
| **Retrieval** | Query matches topic | Multi-stage search returns results |

### Provenance Tracking

Every topic retains its origin:

| Field | Description |
|-------|-------------|
| `source_type` | Where the topic came from: `memory`, `session`, or `reference` |
| `source_id` | The original database ID |

**Example:**
```
Topic: "Docker Best Practices"
  source_type: session
  source_id: sess_abc123
  
→ Traced back to original session
```

## Schema

### Topics Table

| Column | Type | Description |
|--------|------|-------------|
| `id` | TEXT | Unique identifier |
| `topic` | TEXT | Topic title |
| `description` | TEXT | Topic description |
| `source_type` | TEXT | Source: memory, session, reference |
| `source_id` | TEXT | Original source ID |
| `tags` | JSON | Array of tags |
| `created_at` | DATETIME | Creation timestamp |
| `updated_at` | DATETIME | Last update timestamp |
| `is_active` | BOOLEAN | Soft delete flag (default: true) |

### Topic Memberships Table

| Column | Type | Description |
|--------|------|-------------|
| `topic_id` | TEXT | Foreign key to topics |
| `memory_id` | TEXT | Foreign key to memories |
| `added_at` | DATETIME | When the memory was added |

---

## Commands

### List Topics
```bash
mpm topic list [--limit <n>]
```
Lists all topics with member counts.

### Show Topic Details
```bash
mpm topic show <id>
```
Displays topic details with all members.

### Create Topic
```bash
mpm topic add
```
Interactive creation. Prompts for:
- Source type (memory, session, or reference)
- Source ID
- Title
- Description

### Delete Topic
```bash
mpm topic delete <id>
```
Prompts for confirmation. Uses **soft delete** (sets `is_active = false`).

### Shred Topic (Hard Delete)
```bash
mpm topic shred <id>
```
True irreversible erasure with `DELETE` + `VACUUM`. Also removes all `topic_memberships`.

### List Topic Members
```bash
mpm topic members <id>
```
Shows all memories linked to this topic.

### Search Topics
```bash
mpm topic search <query> [--limit <n>]
```
FTS5 search across topic titles and descriptions.

---

## How Topics Are Used

### 1. Session Distillation

When a session grows too large or shifts concepts:

```
Session (1000+ tokens, concept shift detected)
    ↓
AI summarizes key points
    ↓
New Topic created with provenance
```

### 2. Topic Hardening

When a topic is repeatedly referenced:

```
Topic ("Docker Best Practices")
    ↓
3+ successful retrievals
    ↓
Topic promoted to Memory
    ↓
Retains provenance (topic → memory)
```

### 3. Retrieval Loop

When answering a query:

```
Query: "How do I debug Docker containers?"
    ↓
1. Search Sessions (most recent)
    ↓
2. Search Topics (curated knowledge)
    ↓
3. Search Memory (permanent knowledge)
    ↓
4. Search References (documents)
    ↓
Return best matches with source attribution
```

---

## Integration

| Component | Integration Point |
|-----------|-------------------|
| **Sessions** | Topics created from sessions retain `source_type: session` |
| **Memory** | Topics can be hardened into memories |
| **References** | Documents can spawn topics |
| **FTS5** | Topics have searchable FTS5 virtual table |
| **Shred Protocol** | Hard delete cascades to `topic_memberships` |

---

## Best Practices

### When to Create Topics

| Signal | Action |
|--------|--------|
| Session exceeds token limit | Auto-distill to topic |
| Clear concept boundary in conversation | Manual topic creation |
| Repeated theme across sessions | Create umbrella topic |
| Important reference material | Topic from reference |

### Topic Naming

| Good | Bad |
|------|-----|
| "Docker Container Debugging" | "stuff about docker" |
| "Q1 2026 Project Decisions" | "meeting notes" |
| "API Error Patterns" | "errors" |

### Maintaining Topics

```bash
# Review topics quarterly
mpm topic list

# Check for stale topics (no member memories)
mpm topic members <id>

# Consolidate related topics
mpm topic add  # Create umbrella topic
# Then manually move memories between topics
```

---

## Troubleshooting

| Issue | Solution |
|-------|----------|
| Topic has no members | Add memories manually: `mpm memory add --topic <id>` |
| Topic not appearing in search | Check `is_active` flag; use `mpm topic search` |
| Topic won't shred | Verify no foreign key locks |
| Lost provenance | Provenance stored in `source_type` and `source_id` columns |

---

**Last Updated:** 2026-03-29  
**MPM Version:** 6.0.0
