# Memory Pattern - Three-Tier Architecture

**Problem:** Single MEMORY.md inevitably bloats regardless of compaction quality.

**Solution:** Three-tier memory architecture with clear separation of concerns.

## Tier 1: Daily Logs (`memory/YYYY-MM-DD.md`)

**Purpose:** Raw session transcripts, chat logs, immediate context

**Characteristics:**
- Auto-generated from sessions
- Contains full conversation flow
- Includes timestamps, metadata, tool outputs
- Large (5-15KB typical)
- **Temporary** - archived after 2-3 days

**Lifecycle:**
```
Day 0: Created during session
Day 1-3: Available for reference
Day 4+: Archived to memory/archive/
```

**Action:** Extract any lasting facts to Tier 2 or 3, then archive

---

## Tier 2: Topic Memory (`memory-topics/*.md`)

**Purpose:** Long-term detailed memory organized by subject

**Files:**
- `identity.md` - Agent identity, version, creation
- `projects.md` - Active projects, status, next steps
- `lessons.md` - Lessons learned, mistakes, insights
- `configs.md` - API keys, credentials, paths, URLs
- `people.md` - Human info, collaborators, preferences
- `tools.md` - Available tools, capabilities, permissions
- `decisions.md` - Important decisions, direction changes
- `references.md` - Quick references, commands, URLs

**Characteristics:**
- Focused scope (one topic per file)
- Grows organically but stays manageable
- Easy to find specific information
- Can be pruned if topic becomes irrelevant
- **Permanent** - lives indefinitely

**Lifecycle:**
```
Created: When topic emerges
Updated: As new info arrives
Archived: Only if topic becomes obsolete
```

---

## Tier 3: Memory Index (`MEMORY.md`)

**Purpose:** Quick reference index, not detailed storage

**Characteristics:**
- **Small** (<2KB target)
- Summary/overview only
- Points to topic files for details
- Quick stats (agent version, human name, active project count)
- **Permanent** but minimal

**Example:**
```markdown
# MEMORY.md - Long-Term Memory Index

**Last updated:** 2026-03-20

## Quick Summary
- Agent: 808 (see memory-topics/identity.md)
- Human: v (see memory-topics/people.md)
- Active: 1 project (see memory-topics/projects.md)
- Lessons: 5 (see memory-topics/lessons.md)

## Topic Files
- memory-topics/identity.md
- memory-topics/projects.md
- memory-topics/lessons.md
- memory-topics/configs.md
- memory-topics/people.md
- memory-topics/tools.md
- memory-topics/decisions.md
- memory-topics/references.md

## Daily Memory
- memory/ - Session logs (archived weekly)
```

---

## Workflow

### Daily
1. Session creates `memory/YYYY-MM-DD.md`
2. Agent works normally
3. End of day: review if anything worth keeping

### Weekly (Heartbeat task)
1. Review past 7 days of `memory/*.md` files
2. Extract lasting facts → appropriate `memory-topics/*.md`
3. Archive daily files older than 3 days
4. Update MEMORY.md index if needed

### Monthly
1. Review topic files - prune obsolete topics
2. Check MEMORY.md stays small
3. Commit to git

---

## Benefits

| Metric | Single MEMORY.md | Three-Tier Pattern |
|--------|-----------------|-------------------|
| **Scalability** | Poor (grows forever) | Excellent (scales indefinitely) |
| **Findability** | Poor (search through bloat) | Excellent (know where to look) |
| **Maintenance** | Hard (one giant file) | Easy (modular, focused) |
| **Context Load** | High (10KB+ typical) | Low (<2KB index + targeted topics) |
| **Archival** | Risky (might lose important stuff) | Safe (daily logs archived, topics kept) |

---

## Migration Path

### Phase 1: Create Topics
```bash
./topic-split.sh --create-topics
```

### Phase 2: Migrate Content
1. Open MEMORY.md
2. Identify sections by topic
3. Cut/paste into `memory-topics/*.md`
4. Keep only summary in MEMORY.md

### Phase 3: Update Workflow
- New memories → write directly to topic files
- Daily logs → auto-archive after 3 days
- MEMORY.md → index only

---

## Example Migration

**Before (MEMORY.md at 10KB):**
```markdown
# MEMORY.md

## Identity
I am 808, created 2026-03-18, 5th iteration...
[200 more lines of identity stuff]

## Projects
Working on flowbyte website...
[300 lines of project details]

## Lessons Learned
Context bloat kills...
[400 lines of lessons]

## Configs
WordPress admin: 808...
[500 lines of configs]
```

**After (MEMORY.md at 1KB, topics detailed):**
```markdown
# MEMORY.md - Index

- Agent: 808 (→ identity.md)
- Projects: 1 active (→ projects.md)
- Lessons: 5 key insights (→ lessons.md)
- Configs: WordPress, pCloud, MCP (→ configs.md)
```

Each topic file now holds the detail, MEMORY.md stays lean.

---

*Organized memory scales. Bureaucratic memory suffocates.* 🧠📚
