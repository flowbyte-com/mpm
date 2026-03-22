# MPM Memory Capture 📝

**Categorized, compact memory extraction from conversations.**

## Categories

8 smart categories for organizing important information:

| Category | Purpose | Example |
|----------|---------|---------|
| **subject** | Topic-based knowledge | "SQLite for agent state management" |
| **project** | Project-specific info | "Flowbyte WordPress - Hostinger hosting" |
| **idea** | Concepts/insights | "DB-first: 97% token savings" |
| **lesson** | Learnings/mistakes | "Test API writes before assuming access" |
| **decision** | Important choices | "Use local backups weekly" |
| **preference** | User preferences | "v prefers concise replies" |
| **contact** | People mentioned | "v - developer, Europe/London timezone" |
| **tool** | Tool/setup info | "OpenRouter API key: sk-or-v1-..." |

## Usage

```bash
# Capture important info (auto-syncs to DB)
./capture.sh --project "Flowbyte WordPress site - admin user 808"
./capture.sh --lesson "Always test REST API writes on Hostinger"
./capture.sh --idea "DB-first architecture for token efficiency"

# Review categorized memory
./capture.sh --list

# Manual DB sync (if needed)
./capture.sh --sync

# Compact/merge related entries
./capture.sh --compact
```

**Auto-sync:** Every capture automatically syncs to `memory.db` - lightweight, instant.

## Output Structure

```
MPM/memory/
├── subject.md      # Topic knowledge
├── project.md      # Project info
├── idea.md         # Concepts/insights
├── lesson.md       # Learnings
├── decision.md     # Decisions made
├── preference.md   # User preferences
├── contact.md      # People
└── tool.md         # Tools/setup
```

## Format

Each file:
```markdown
# Category Memory

**Created:** 2026-03-20 08:00
**Updated:** 2026-03-20 08:06

## Entries

- **2026-03-20 08:06:** Content here
- **2026-03-20 08:00:** Earlier entry
```

## Workflow

1. **During chat** - Agent identifies important info
2. **Categorize** - Pick the right category (project/lesson/idea etc.)
3. **Capture** - Run `./capture.sh --category "content"`
4. **Compact** - Periodically merge related entries
5. **Archive** - Move old entries to archive when file grows

## Integration with MPM

```bash
# Full MPM workflow
mpm memory              # Run diagnosis
./capture.sh --list     # Review categorized memory
./capture.sh --compact  # Merge related entries
mpm memory consolidate  # Archive old, keep lean
```

## Why Categorize?

- **Faster retrieval** - Know exactly where info lives
- **Compact files** - Each category stays focused
- **Better context** - Agent loads only relevant categories
- **Clean MEMORY.md** - Core memory stays minimal

---

*Capture what matters. Categorize for speed. Compact for context.*
