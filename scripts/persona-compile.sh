#!/bin/bash

# ⚡ MPM Persona Compiler
# Convert MD persona files to SQLite DB (Token-Dense Architecture)

# Detect correct workspace path
if [ -d "$HOME/.openclaw/workspace" ]; then
    WORKSPACE="$HOME/.openclaw/workspace"
elif [ -d "$HOME/.picoclaw/workspace" ]; then
    WORKSPACE="$HOME/.picoclaw/workspace"
else
    WORKSPACE="$HOME/.openclaw/workspace"
fi
PERSONAS_DIR="$WORKSPACE/MPM/persona"
DB_FILE="$PERSONAS_DIR/personas.db"

# Ensure MPM persona directory exists
mkdir -p "$PERSONAS_DIR"

# Source SymAI bytecode generator (if exists)
SYMAI_BC_SCRIPT="$WORKSPACE/MPM/scripts/symai-bytecode.sh"
[ -f "$SYMAI_BC_SCRIPT" ] && source "$SYMAI_BC_SCRIPT"

# Generate bytecode with SymAI fallback
generate_sym_id() {
    local name="$1"
    if type generate_persona_bytecode > /dev/null 2>&1; then
        generate_persona_bytecode "$name" "$name"
    else
        echo "~p.${name:0:1}!act"  # Fallback: ~p.o!act for oracle
    fi
}

# Validate identifier format (alphanumeric + underscore/hyphen only)
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
CREATE TABLE IF NOT EXISTS personas (
    name TEXT PRIMARY KEY,
    sym_id TEXT,
    identity_name TEXT,
    identity_title TEXT,
    identity_vibe TEXT,
    identity_emoji TEXT,
    voice_tone TEXT,
    behavioral_rules TEXT,
    context TEXT,
    activation TEXT,
    active INTEGER DEFAULT 0,
    loaded_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS persona_files (
    name TEXT PRIMARY KEY,
    file_path TEXT,
    file_hash TEXT,
    last_synced TIMESTAMP,
    status TEXT
);
SCHEMA
}

# Escape single quotes for SQL (basic sanitization)
escape_sql() {
    local input="$1"
    # Replace single quotes with doubled single quotes
    printf '%s' "$input" | sed "s/'/''/g"
}

# Compile a persona MD file to DB
compile_persona() {
    local name="$1"
    local md_file="$PERSONAS_DIR/${name}.persona"

    # Validate input
    validate_id "$name" || return 1

    if [ ! -f "$md_file" ]; then
        echo "❌ Persona not found: $name"
        return 1
    fi

    # --- SMART SCRAPER (Fallback Logic) ---
    # Use SymAI bytecode generator if available, else fallback to filename
    local sym_id
    if type generate_persona_bytecode > /dev/null 2>&1; then
        sym_id=$(generate_persona_bytecode "$name" "$name")
    else
        # Manual fallback: first letter for quick activation
        sym_id="~p.${name:0:1}!act"
    fi
    
    # Also check if MD file has explicit sym_id
    local md_sym_id=$(grep -m 1 -E "^[#]+? sym_id:|^sym_id:" "$md_file" | sed 's/.*://' | tr -d ' ')
    if [ -n "$md_sym_id" ]; then
        sym_id="$md_sym_id"
    fi

    local title=$(grep -m 1 -E "^title:" "$md_file" | cut -d':' -f2- | sed 's/^ *//')
    title=${title:-"${name^}"}

    # Extract identity metadata (supports Legacy formatting and New formatting)
    local id_name=$(grep -E '^\- ?\*\*Name:\*\*' "$md_file" | head -1 | sed -E 's/^\- ?\*\*[A-Za-z]+:\*\* ?//')
    id_name=${id_name:-"$title"}

    local id_vibe=$(grep -E '^\- ?\*\*Vibe:\*\*' "$md_file" | head -1 | sed -E 's/^\- ?\*\*[A-Za-z]+:\*\* ?//')
    id_vibe=${id_vibe:-"Standard System"}

    local id_emoji=$(grep -E '^\- ?\*\*Emoji:\*\*' "$md_file" | head -1 | sed -E 's/^\- ?\*\*[A-Za-z]+:\*\* ?//')
    id_emoji=${id_emoji:-"👤"}

    # --- HIGH-DENSITY TEXT EXTRACTION ---
    # The 'sed' command collapses multiple spaces/newlines to save LLM tokens
    local voice=$(sed -n '/^## Voice/,/^## /p; /^\-? \*\*Voice:\*\*/p' "$md_file" | grep -v '^##' | grep -v '^\-? \*\*Voice:\*\*' | tr '\n' ' ' | sed 's/  */ /g')
    local rules=$(sed -n '/^## Behavioral Rules$/,/^## /p' "$md_file" | grep -v '^##' | tr '\n' ' ' | sed 's/  */ /g')
    local context=$(sed -n '/^## Context$/,/^## /p' "$md_file" | grep -v '^##' | tr '\n' ' ' | sed 's/  */ /g')

    # Escape for SQL execution
    id_name=$(escape_sql "$id_name")
    title=$(escape_sql "$title")
    id_vibe=$(escape_sql "$id_vibe")
    id_emoji=$(escape_sql "$id_emoji")
    voice=$(escape_sql "$voice")
    rules=$(escape_sql "$rules")
    context=$(escape_sql "$context")

    local file_hash=$(md5sum "$md_file" | cut -d' ' -f1)

    # Use parameterized approach with here-document for safety
    sqlite3 "$DB_FILE" <<EOF
INSERT OR REPLACE INTO personas (name, sym_id, identity_name, identity_title, identity_vibe, identity_emoji, voice_tone, behavioral_rules, context, updated_at) 
VALUES ('$name', '$(escape_sql "$sym_id")', '$id_name', '$title', '$id_vibe', '$id_emoji', '$voice', '$rules', '$context', CURRENT_TIMESTAMP);
EOF

    sqlite3 "$DB_FILE" <<EOF
INSERT OR REPLACE INTO persona_files (name, file_path, file_hash, last_synced, status) 
VALUES ('$name', '$(escape_sql "$md_file")', '$file_hash', CURRENT_TIMESTAMP, 'active');
EOF
}

# Sync filesystem to DB
sync_personas() {
    local quiet="$1"
    [ "$quiet" != "quiet" ] && echo "🔄 Syncing personas..."

    init_db

    # 1. Compile new/modified files
    for md_file in "$PERSONAS_DIR"/*.persona; do
        [ -e "$md_file" ] || continue
        local name=$(basename "$md_file" .persona)

        # Validate name before processing
        validate_id "$name" 2>/dev/null || continue

        local db_hash=$(sqlite3 "$DB_FILE" "SELECT file_hash FROM persona_files WHERE name='$name';" 2>/dev/null)
        local file_hash=$(md5sum "$md_file" | cut -d' ' -f1)

        if [ "$db_hash" != "$file_hash" ]; then
            [ "$quiet" != "quiet" ] && echo "🎭 Compiling Persona: $name"
            compile_persona "$name"
        fi
    done

    # 2. Remove deleted files from DB (Ghost Purge) - SAFE version
    local db_names
    db_names=$(sqlite3 "$DB_FILE" "SELECT name FROM persona_files;" 2>/dev/null)
    
    while IFS= read -r db_name; do
        [ -n "$db_name" ] || continue
        
        # Validate before deletion
        if ! validate_id "$db_name" 2>/dev/null; then
            echo "⚠️ Skipping invalid database entry: '$db_name'" >&2
            continue
        fi
        
        if [ ! -f "$PERSONAS_DIR/${db_name}.persona" ]; then
            [ "$quiet" != "quiet" ] && echo "🗑️ Removing Ghost Persona: $db_name"
            sqlite3 "$DB_FILE" "DELETE FROM personas WHERE name='$db_name';" 2>/dev/null
            sqlite3 "$DB_FILE" "DELETE FROM persona_files WHERE name='$db_name';" 2>/dev/null
        fi
    done <<< "$db_names"
}

# Main command handler
case "${1:-status}" in
    compile) 
        init_db
        if [ -z "$2" ]; then
            echo "Usage: $0 compile <name>"
            exit 1
        fi
        compile_persona "$2" 
        ;;
    sync|refresh)    
        init_db
        sync_personas 
        ;;
    list|ls)
        init_db
        printf "%-12s │ %-20s │ %-21s │ %s\n" "sym_id" "emoji" "identity_name" "identity_title"
        printf "%-12s-%-20s-%-21s-%s\n" "------------" "--------------------" "---------------------" "------------------"
        sqlite3 -batch "$DB_FILE" "SELECT sym_id, identity_emoji, identity_name, identity_title FROM personas;" 2>/dev/null | while IFS='|' read -r sid emoji name title; do
            printf "%-12s │ %-20s │ %-21s │ %s\n" "$sid" "$emoji" "$name" "$title"
        done
        ;;
    active|~p.act)  
        init_db
        sqlite3 -header -column "$DB_FILE" "SELECT name, sym_id, identity_name FROM personas WHERE active=1;" 2>/dev/null || echo "No active persona"
        ;;
    unload)
        if [ -z "$2" ]; then
            echo "Usage: $0 unload <name>"
            exit 1
        fi
        validate_id "$2" || exit 1
        sqlite3 "$DB_FILE" "DELETE FROM personas WHERE name='$2';" 2>/dev/null
        sqlite3 "$DB_FILE" "DELETE FROM persona_files WHERE name='$2';" 2>/dev/null
        echo "✅ Unloaded persona: $2"
        ;;
    *)       
        echo "Usage: $0 [compile|sync|list|active|unload] [name]"
        echo ""
        echo "Subcommands:"
        echo "  compile <name>  - Compile a single persona to DB"
        echo "  sync            - Sync all personas (MD files → DB)"
        echo "  list|ls         - List all personas"
        echo "  active          - Show currently active persona"
        echo "  unload <name>   - Remove persona from DB"
        exit 1 
        ;;
esac
