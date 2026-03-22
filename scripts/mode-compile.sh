#!/bin/bash

# ⚡ MPM Mode Compiler
# Convert MD mode files to SQLite DB (supports stacking)

WORKSPACE="$HOME/.openclaw/workspace"
MODES_DIR="$WORKSPACE/MPM/mode"
DB_FILE="$MODES_DIR/m3.db"

# Ensure MPM mode directory exists
mkdir -p "$MODES_DIR"

# Initialize database schema
init_db() {
    sqlite3 "$DB_FILE" <<'SCHEMA' 2>/dev/null
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
    stack_order INTEGER
);
SCHEMA
}

# Generate SymAI bytecode signature from mode content
generate_signature() {
    local name="$1"
    local md_file="$MODES_DIR/${name}.mode"

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
    echo "$1" | sed "s/'/''/g"
}

# Compile a mode MD file to DB
compile_mode() {
    local name="$1"
    local md_file="$MODES_DIR/${name}.mode"

    if [ ! -f "$md_file" ]; then
        echo "❌ Mode not found: $name"
        return 1
    fi

    # Extract sym_id and title from MD headers (The Smart Scraper)
    local sym_id=$(grep -m 1 -E "^sym_id:" "$md_file" | cut -d':' -f2- | tr -d ' ')
    sym_id=${sym_id:-"~m.$name"}

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
    purpose=$(escape_sql "$purpose")
    patterns=$(escape_sql "$patterns")
    checklist=$(escape_sql "$checklist")
    best_practices=$(escape_sql "$best_practices")
    anti_patterns=$(escape_sql "$anti_patterns")
    tools=$(escape_sql "$tools")
    exit_criteria=$(escape_sql "$exit_criteria")

    local file_hash=$(md5sum "$md_file" | cut -d' ' -f1)
    local bytecode_sig=$(generate_signature "$name")

    sqlite3 "$DB_FILE" "INSERT OR REPLACE INTO modes (name, sym_id, title, purpose, patterns, checklist, best_practices, anti_patterns, tools, exit_criteria, bytecode_signature, updated_at) VALUES ('$name', '$sym_id', '$title', '$purpose', '$patterns', '$checklist', '$best_practices', '$anti_patterns', '$tools', '$exit_criteria', '$bytecode_sig', CURRENT_TIMESTAMP);"
    sqlite3 "$DB_FILE" "INSERT OR REPLACE INTO mode_files (name, file_path, file_hash, last_synced, status) VALUES ('$name', '$md_file', '$file_hash', CURRENT_TIMESTAMP, 'active');"
}

# Sync filesystem to DB
sync_modes() {
    local quiet="$1"
    [ "$quiet" != "quiet" ] && echo "🔄 Syncing modes..."

    init_db

    # 1. Compile new/modified files
    for md_file in "$MODES_DIR"/*.mode; do
        [ -e "$md_file" ] || continue
        name=$(basename "$md_file" .mode)

        local db_hash=$(sqlite3 "$DB_FILE" "SELECT file_hash FROM mode_files WHERE name='$name';" 2>/dev/null)
        local file_hash=$(md5sum "$md_file" | cut -d' ' -f1)

        if [ "$db_hash" != "$file_hash" ]; then
            [ "$quiet" != "quiet" ] && echo "📦 Compiling: $name"
            compile_mode "$name"
        fi
    done

    # 2. Remove deleted files from DB
    sqlite3 "$DB_FILE" "SELECT name FROM mode_files;" 2>/dev/null | while read -r db_name; do
        if [ ! -f "$MODES_DIR/${db_name}.mode" ]; then
            [ "$quiet" != "quiet" ] && echo "🗑️ Removing Ghost Mode: $db_name"
            sqlite3 "$DB_FILE" "DELETE FROM modes WHERE name='$db_name';"
            sqlite3 "$DB_FILE" "DELETE FROM mode_files WHERE name='$db_name';"
            sqlite3 "$DB_FILE" "DELETE FROM active_modes WHERE name='$db_name';"
        fi
    done
}

# Main command handler
case "${1:-sync}" in
    compile) init_db; compile_mode "$2" ;;
    sync)    init_db; sync_modes ;;
    list)    sqlite3 -header -column "$DB_FILE" "SELECT sym_id, title, updated_at FROM modes;" 2>/dev/null ;;
    active)  sqlite3 -header -column "$DB_FILE" "SELECT name, stack_order FROM active_modes ORDER BY stack_order;" 2>/dev/null ;;
    unload)  sqlite3 "$DB_FILE" "DELETE FROM modes WHERE name='$2'; DELETE FROM mode_files WHERE name='$2'; DELETE FROM active_modes WHERE name='$2';" ;;
    *)       echo "Usage: $0 [compile|sync|list|active|unload]"; exit 1 ;;
esac
