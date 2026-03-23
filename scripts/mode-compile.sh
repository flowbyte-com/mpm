#!/bin/bash

# ⚡ MPM Mode Compiler
# Convert MD mode files to SQLite DB (supports stacking)

# Detect correct workspace path
if [ -d "$HOME/.openclaw/workspace" ]; then
    WORKSPACE="$HOME/.openclaw/workspace"
elif [ -d "$HOME/.picoclaw/workspace" ]; then
    WORKSPACE="$HOME/.picoclaw/workspace"
else
    WORKSPACE="$HOME/.openclaw/workspace"
fi
MODES_DIR="$WORKSPACE/MPM/mode"
DB_FILE="$MODES_DIR/m3.db"

# Ensure MPM mode directory exists
mkdir -p "$MODES_DIR"

# Source SymAI bytecode generator (if exists)
SYMAI_BC_SCRIPT="$WORKSPACE/MPM/scripts/symai-bytecode.sh"
[ -f "$SYMAI_BC_SCRIPT" ] && source "$SYMAI_BC_SCRIPT"

# Validate identifier format
validate_id() {
    local id="$1"
    if [[ ! "$id" =~ ^[a-zA-Z0-9_-]+$ ]]; then
        echo "❌ Invalid identifier: '$id' - only alphanumeric, underscore, and hyphen allowed" >&2
        return 1
    fi
    return 0
}

# Initialize database schema
init_db() {
    sqlite3 "$DB_FILE" <<'SCHEMA'
CREATE TABLE IF NOT EXISTS modes (
    name TEXT PRIMARY KEY,
    sym_id TEXT,
    title TEXT,
    purpose TEXT,
    patterns TEXT,
    checklist TEXT,
    best_practices TEXT,
    anti_patterns TEXT,
    tools TEXT,
    exit_criteria TEXT,
    bytecode_signature TEXT,
    compiled_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS mode_files (
    name TEXT PRIMARY KEY,
    file_path TEXT,
    file_hash TEXT,
    last_synced TIMESTAMP,
    status TEXT
);

CREATE TABLE IF NOT EXISTS active_modes (
    name TEXT PRIMARY KEY,
    activated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    stack_order INTEGER UNIQUE
);
SCHEMA
}

# Generate SymAI bytecode signature from mode content
generate_signature() {
    local name="$1"
    local md_file="$MODES_DIR/${name}.mode"

    [ -f "$md_file" ] || return 0

    local purpose=$(sed -n '/^## Purpose$/,/^## /p' "$md_file" | grep -v '^##' | tr '\n' ' ' | cut -c1-60 | xargs)
    local checklist=$(sed -n '/^## Checklist$/,/^## /p' "$md_file" | grep -v '^##' | head -3 | tr '\n' ' ' | xargs)

    local sig="~$name:"

    if [ -n "$purpose" ]; then
        local short_purpose=$(echo "$purpose" | tr ' ' '_')
        sig+="~\$$short_purpose;"
    fi

    if [ -n "$checklist" ]; then
        local items=$(echo "$checklist" | cut -c1-40 | tr ' ' '_')
        sig+="~?$items"
    fi

    echo "$sig" | cut -c1-150
}

# Escape single quotes for SQL
escape_sql() {
    printf '%s' "$1" | sed "s/'/''/g"
}

# Compile a mode MD file to DB
compile_mode() {
    local name="$1"
    local md_file="$MODES_DIR/${name}.mode"

    # Validate input
    validate_id "$name" || return 1

    if [ ! -f "$md_file" ]; then
        echo "❌ Mode not found: $name"
        return 1
    fi

    # Extract sym_id and title from MD headers (The Smart Scraper)
    # Priority: MD explicit sym_id > SymAI bytecode > default ~m.name
    local md_sym_id=$(grep -m 1 -E "^[#]+? sym_id:|^sym_id:" "$md_file" | sed 's/.*://' | tr -d ' ')
    local sym_id
    
    if [ -n "$md_sym_id" ]; then
        sym_id="$md_sym_id"
    elif type generate_mode_bytecode \u003e /dev/null 2\u003e\u00261; then
        # Try SymAI bytecode generator
        sym_id=$(generate_mode_bytecode "$name" "${name:0:2}")
    else
        # Fallback: abbreviate to 2 chars
        sym_id="~m.${name:0:2}!act"
    fi

    local title=$(grep -m 1 -E "^title:" "$md_file" | cut -d':' -f2- | sed 's/^ *//')
    title=${title:-"${name^}"}

    # Extract fields from MD
    local purpose=$(sed -n '/^## Purpose$/,/^## /p; /^## Identity$/,/^## /p' "$md_file" | grep -v '^##' | tr '\n' ' ' | sed 's/  */ /g')
    local patterns=$(sed -n '/^## Behavioral Patterns$/,/^## /p; /^## Voice.*Tone$/,/^## /p' "$md_file" | grep -v '^##' | tr '\n' ' ' | sed 's/  */ /g')
    local checklist=$(sed -n '/^## Checklist$/,/^## /p; /^## 🛠️ Opcodes$/,/^## /p; /^## 🔍 Opcodes$/,/^## /p; /^## 🎨 Opcodes$/,/^## /p' "$md_file" | grep -v '^##' | tr '\n' ' ' | sed 's/  */ /g')
    local best_practices=$(sed -n '/^## Best Practices$/,/^## /p; /^## 📝 Rules$/,/^## /p' "$md_file" | grep -v '^##' | tr '\n' ' ' | sed 's/  */ /g')
    local anti_patterns=$(sed -n '/^## Anti-Patterns/,/^## /p' "$md_file" | grep -v '^##' | tr '\n' ' ' | sed 's/  */ /g')
    local tools=$(sed -n '/^## Tools/,/^## /p' "$md_file" | grep -v '^##' | tr '\n' ' ' | sed 's/  */ /g')
    local exit_criteria=$(sed -n '/^## Exit Criteria$/,/^## \|EOF$/p' "$md_file" | grep -v '^##' | tr '\n' ' ' | sed 's/  */ /g')

    # Escape for SQL
    local esc_title=$(escape_sql "$title")
    local esc_purpose=$(escape_sql "$purpose")
    local esc_patterns=$(escape_sql "$patterns")
    local esc_checklist=$(escape_sql "$checklist")
    local esc_best_practices=$(escape_sql "$best_practices")
    local esc_anti_patterns=$(escape_sql "$anti_patterns")
    local esc_tools=$(escape_sql "$tools")
    local esc_exit_criteria=$(escape_sql "$exit_criteria")

    local file_hash=$(md5sum "$md_file" | cut -d' ' -f1)
    local bytecode_sig=$(generate_signature "$name" | sed "s/'/''/g")

    # Safe parameterized-like approach
    sqlite3 "$DB_FILE" <<EOF
INSERT OR REPLACE INTO modes (name, sym_id, title, purpose, patterns, checklist, best_practices, anti_patterns, tools, exit_criteria, bytecode_signature, updated_at) 
VALUES ('$name', '$(escape_sql "$sym_id")', '$esc_title', '$esc_purpose', '$esc_patterns', '$esc_checklist', '$esc_best_practices', '$esc_anti_patterns', '$esc_tools', '$esc_exit_criteria', '$bytecode_sig', CURRENT_TIMESTAMP);
EOF

    sqlite3 "$DB_FILE" <<EOF
INSERT OR REPLACE INTO mode_files (name, file_path, file_hash, last_synced, status) 
VALUES ('$name', '$(escape_sql "$md_file")', '$file_hash', CURRENT_TIMESTAMP, 'active');
EOF
}

# Sync filesystem to DB
sync_modes() {
    local quiet="$1"
    [ "$quiet" != "quiet" ] && echo "🔄 Syncing modes..."

    init_db

    # 1. Compile new/modified files
    for md_file in "$MODES_DIR"/*.mode; do
        [ -e "$md_file" ] || continue
        local name=$(basename "$md_file" .mode)

        validate_id "$name" 2>/dev/null || continue

        local db_hash=$(sqlite3 "$DB_FILE" "SELECT file_hash FROM mode_files WHERE name='$name';" 2>/dev/null)
        local file_hash=$(md5sum "$md_file" | cut -d' ' -f1)

        if [ "$db_hash" != "$file_hash" ]; then
            [ "$quiet" != "quiet" ] && echo "📦 Compiling: $name"
            compile_mode "$name"
        fi
    done

    # 2. Remove deleted files from DB - SAFE version
    local db_names
    db_names=$(sqlite3 "$DB_FILE" "SELECT name FROM mode_files;" 2>/dev/null)
    
    while IFS= read -r db_name; do
        [ -n "$db_name" ] || continue
        
        if ! validate_id "$db_name" 2>/dev/null; then
            echo "⚠️ Skipping invalid database entry: '$db_name'" >&2
            continue
        fi
        
        if [ ! -f "$MODES_DIR/${db_name}.mode" ]; then
            [ "$quiet" != "quiet" ] && echo "🗑️ Removing Ghost Mode: $db_name"
            sqlite3 "$DB_FILE" "DELETE FROM modes WHERE name='$db_name';" 2>/dev/null
            sqlite3 "$DB_FILE" "DELETE FROM mode_files WHERE name='$db_name';" 2>/dev/null
            sqlite3 "$DB_FILE" "DELETE FROM active_modes WHERE name='$db_name';" 2>/dev/null
        fi
    done <<< "$db_names"
}

# Main command handler
case "${1:-sync}" in
    compile) 
        init_db
        if [ -z "$2" ]; then
            echo "Usage: $0 compile <name>"
            exit 1
        fi
        compile_mode "$2" 
        ;;
    sync|refresh)    
        init_db 
        sync_modes 
        ;;
    list|ls)    
        init_db
        sqlite3 -header -column "$DB_FILE" "SELECT sym_id, title, updated_at FROM modes;" 2>/dev/null || echo "No modes loaded"
        ;;
    active|~m.act)  
        init_db
        sqlite3 -header -column "$DB_FILE" "SELECT name, stack_order FROM active_modes ORDER BY stack_order;" 2>/dev/null || echo "No active modes"
        ;;
    unload)  
        if [ -z "$2" ]; then
            echo "Usage: $0 unload <name>"
            exit 1
        fi
        validate_id "$2" || exit 1
        sqlite3 "$DB_FILE" "DELETE FROM modes WHERE name='$2';" 2>/dev/null
        sqlite3 "$DB_FILE" "DELETE FROM mode_files WHERE name='$2';" 2>/dev/null
        sqlite3 "$DB_FILE" "DELETE FROM active_modes WHERE name='$2';" 2>/dev/null
        echo "✅ Unloaded mode: $2"
        ;;
    *)
        cat <<'EOF'
Usage: $0 [compile|sync|list|active|unload] [name]

Subcommands:
  compile <name>  - Compile a single mode to DB
  sync|refresh     - Sync all modes (MD files → DB)
  list|ls          - List all modes
  active           - Show currently active/stacked modes
  unload <name>   - Remove mode from DB
EOF
        exit 1
        ;;
esac
