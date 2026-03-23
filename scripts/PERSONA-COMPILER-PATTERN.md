# Persona Compiler Pattern - MD ↔ SQLite Sync

**Problem:** Persona MD files are token-hungry when loaded into context. Large personas (2-5KB each) burn context window even when just checking what's available.

**Solution:** Compile persona MD files to SQLite DB on load. Query DB for metadata (tiny tokens), load full MD only when activating.

## Architecture

```
personas/
├── *.persona              # Human-editable MD source files
│   ├── default.persona    # (2-5KB, versionable, Git-friendly)
│   ├── corporate.persona
│   └── pirate.persona
└── loaded/
    ├── personas.db        # Compiled SQLite (compact, queryable)
    └── PERSONA-COMPILER-PATTERN.md

Workflow:
1. Edit MD file in personas/
2. Run: ./persona-compile.sh sync
3. MD → DB (compiled, token-efficient)
4. Query DB for metadata (tiny)
5. Load full MD only when activating
```

## Sync Lifecycle

### Phase 1: File Created/Edited
```
personas/mypersona.persona (MD file)
↓
Human edits in text editor
↓
Git tracks changes (versionable)
```

### Phase 2: Compile to DB
```bash
./persona-compile.sh sync
```
- Reads all `*.persona` files
- Computes MD5 hash for each
- Extracts structured fields
- Inserts into SQLite
- Stores hash for change detection

### Phase 3: Query (Token-Efficient)
```bash
./persona-query.sh identity corporate
# Returns: "Assistant v2.0" (3 tokens)
# vs loading full 3KB MD file (~500 tokens)
```

### Phase 4: Activate (Load Full)
```bash
./persona-manager.sh activate corporate
# Reads full MD file
# Adopts voice/tone
# DB tracks loaded_at timestamp
```

### Phase 5: File Deleted
```
Human deletes: personas/old.persona
↓
Run: ./persona-compile.sh sync
↓
DB detects missing file
↓
Auto-removes from DB
```

## Database Schema

```sql
-- Persona content (compiled from MD)
CREATE TABLE personas (
    name TEXT PRIMARY KEY,
    identity_name TEXT,
    identity_title TEXT,
    identity_vibe TEXT,
    identity_emoji TEXT,
    voice_tone TEXT,
    behavioral_rules TEXT,
    context TEXT,
    activation TEXT,
    loaded_at TIMESTAMP,
    updated_at TIMESTAMP
);

-- File tracking (sync state)
CREATE TABLE persona_files (
    name TEXT PRIMARY KEY,
    file_path TEXT,
    file_hash TEXT,      -- MD5 for change detection
    last_synced TIMESTAMP,
    status TEXT          -- 'active', 'pending', 'removed'
);
```

## Token Savings

| Operation | MD Load | DB Query | Savings |
|-----------|---------|----------|---------|
| List personas | 10KB (~1500 tokens) | 100 bytes (~50 tokens) | **97%** |
| Check identity | 3KB (~500 tokens) | 20 bytes (~10 tokens) | **98%** |
| Get vibe | 3KB (~500 tokens) | 30 bytes (~15 tokens) | **97%** |
| Activate | 3KB (~500 tokens) | 3KB (~500 tokens) | 0% (need full) |

**Typical workflow:**
- List → Query DB (tiny)
- Preview → Query DB (tiny)
- Activate → Load MD (full, but only when needed)

## Benefits

### MD Files (Source)
✅ **Human-editable** - Open in any text editor  
✅ **Versionable** - Git tracks changes beautifully  
✅ **Shareable** - Copy/paste, email, upload  
✅ **Readable** - No special tools needed  
✅ **Flexible** - Easy to restructure  

### SQLite DB (Compiled)
✅ **Compact** - Binary format, minimal overhead  
✅ **Queryable** - SQL pulls only what you need  
✅ **Fast** - Instant lookups, no parsing  
✅ **Structured** - Clear schema, typed fields  
✅ **Indexable** - Add indexes for speed  

### Hybrid Approach
✅ **Best of both** - Human-friendly + machine-efficient  
✅ **Lazy loading** - Only load full MD when activating  
✅ **Auto-sync** - Filesystem ↔ DB stays consistent  
✅ **Safe** - MD is source of truth, DB is cache  

## Commands

### Compile
```bash
# Compile single persona
./persona-compile.sh compile <name>

# Sync all (add new, remove missing, update changed)
./persona-compile.sh sync
```

### Query
```bash
# Query specific field
./persona-query.sh identity <name>
./persona-query.sh vibe <name>
./persona-query.sh rules <name>

# Show all fields
./persona-query.sh all <name>
```

### Manage
```bash
# List loaded personas
./persona-compile.sh list

# Show sync status
./persona-compile.sh status

# Unload from DB (keep MD)
./persona-compile.sh unload <name>
```

## Workflow Example

```bash
# 1. Create new persona
cat > personas/zen.persona <<EOF
# Persona: Zen Mode
## Identity Override
- **Name:** Zen 808
- **Vibe:** Calm, meditative, peaceful
EOF

# 2. Compile to DB
./persona-compile.sh sync
# 📦 Compiling: zen (changed)

# 3. Query metadata (tiny tokens)
./persona-query.sh vibe zen
# Calm, meditative, peaceful

# 4. Activate (load full MD)
./persona-manager.sh activate zen
# Reads full file, adopts voice

# 5. Later, delete persona
rm personas/zen.persona

# 6. Sync removes from DB
./persona-compile.sh sync
# 🗑️  Removing: zen (file deleted)
```

## Future Enhancements

### Auto-Compile on Save
```bash
# Watch persona directory
inotifywait -m personas/ -e modify | while read file; do
    name=$(basename "$file" .persona)
    ./persona-compile.sh compile "$name"
done
```

### Skill Integration
```bash
# OpenClaw skill could:
# 1. Detect context (task type, channel, time)
# 2. Query DB for matching persona
# 3. Suggest: "Switch to zen persona?"
# 4. Human approves → activate
```

### Persona Chains
```sql
-- Link personas (inheritance)
CREATE TABLE persona_chains (
    parent_name TEXT,
    child_name TEXT,
    inherits TEXT  -- 'voice', 'rules', 'all'
);
```

### Version History
```sql
-- Track persona versions
CREATE TABLE persona_versions (
    name TEXT,
    version INTEGER,
    content_hash TEXT,
    created_at TIMESTAMP
);
```

---

*Compile human creativity into machine efficiency.* 🎭⚡
