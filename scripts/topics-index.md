# Memory Topics Index

**Purpose:** Split long-term memory into topic-based files to prevent MEMORY.md bloat.

## Topic Location

MPM/memory/topics/

## Recommended Topics

| File | Purpose | Example Content |
|------|---------|-----------------|
| `identity.md` | Agent identity, version, creation | "I am 808, created 2026-03-18, 5th iteration" |
| `projects.md` | Active projects, status, next steps | "Website: flowbyte, status: in progress, next: homepage" |
| `lessons.md` | Lessons learned, mistakes, insights | "Context bloat kills - keep files compact" |
| `configs.md` | API keys, credentials, paths, URLs | "WordPress admin: 808, REST API user: 808" |
| `people.md` | Human info, collaborators, preferences | "Human: v, timezone: Europe/London" |
| `tools.md` | Available tools, capabilities, permissions | "Node, npm, Python3, Git available" |
| `decisions.md` | Important decisions, direction changes | "Chose pCloud over timeshift for /home backup" |
| `references.md` | Quick references, commands, URLs | "Workspace: ~/.openclaw/workspace" |

## Workflow

### 1. Create Topic Structure
```bash
./topic-split.sh --create-topics
```

Creates `memory-topics/` with template files for each topic.

### 2. Migrate Content
```bash
# Manual (recommended)
# 1. Open MEMORY.md
# 2. Cut sections into appropriate topic files
# 3. Keep MEMORY.md as brief index

# Or review what would move
./topic-split.sh --migrate
```

### 3. Update MEMORY.md to Index
After migration, MEMORY.md should be a brief index:

```markdown
# MEMORY.md - Long-Term Memory Index

**Last updated:** 2026-03-20

## Quick Summary
- Agent: 808 (see `memory-topics/identity.md`)
- Human: v (see `memory-topics/people.md`)
- Active: 1 project (see `memory-topics/projects.md`)

## Topic Files
- `memory-topics/identity.md` - Who I am
- `memory-topics/projects.md` - What I'm working on
- `memory-topics/lessons.md` - What I've learned
- `memory-topics/configs.md` - Configs, keys, paths
- `memory-topics/people.md` - Human & collaborators
- `memory-topics/tools.md` - Capabilities & permissions
- `memory-topics/decisions.md` - Key decisions
- `memory-topics/references.md` - Quick references

## Daily Memory
- `memory/YYYY-MM-DD.md` - Session logs (archived weekly)
```

## Benefits

**Before (single MEMORY.md):**
- Grows indefinitely
- Hard to find specific info
- Mixed concerns (identity + projects + configs)
- Becomes unwieldy at 10KB+

**After (topic-based):**
- Each file stays focused
- Easy to find what you need
- Can archive entire topics if irrelevant
- MEMORY.md stays small as index
- Scales indefinitely

## Migration Strategy

1. **Create topics** - Run `--create-topics`
2. **Review MEMORY.md** - Identify sections per topic
3. **Migrate manually** - Cut/paste into topic files
4. **Update MEMORY.md** - Convert to index format
5. **Future** - Write directly to topic files, not MEMORY.md

---

*Memory should be organized like a library, not a junk drawer.* 📚
