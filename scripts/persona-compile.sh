#!/bin/bash

# ⚡ MPM Persona Compiler
# Convert MD persona files to SQLite DB (Token-Dense Architecture)

WORKSPACE="$HOME/.openclaw/workspace"
PERSONAS_DIR="$WORKSPACE/MPM/persona"
DB_FILE="$PERSONAS_DIR/personas.db"

# Ensure MPM persona directory exists
mkdir -p "$PERSONAS_DIR"

# Initialize database schema
init_db() {
    sqlite3 "$DB_FILE" <<'SCHEMA' 2>/dev/null
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

# Escape single quotes for SQL
escape_sql() {
    echo "$1" | sed "s/'/''/g"
}

# Compile a persona MD file to DB
compile_persona() {
    local name="$1"
    local md_file="$PERSONAS_DIR/${name}.persona"

    if [ ! -f "$md_file" ]; then
        echo "❌ Persona not found: $name"
        return 1
    fi

    # --- SMART SCRAPER (Fallback Logic) ---
    local sym_id=$(grep -m 1 -E "^##? sym_id:" "$md_file" | sed 's/^##* *//' | cut -d':' -f2- | tr -d ' ')
    sym_id=${sym_id:-"~p.$name"}

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

    sqlite3 "$DB_FILE" "INSERT OR REPLACE INTO personas (name, sym_id, identity_name, identity_title, identity_vibe, identity_emoji, voice_tone, behavioral_rules, context, updated_at) VALUES ('$name', '$sym_id', '$id_name', '$title', '$id_vibe', '$id_emoji', '$voice', '$rules', '$context', CURRENT_TIMESTAMP);"
    sqlite3 "$DB_FILE" "INSERT OR REPLACE INTO persona_files (name, file_path, file_hash, last_synced, status) VALUES ('$name', '$md_file', '$file_hash', CURRENT_TIMESTAMP, 'active');"
}

# Sync filesystem to DB
sync_personas() {
    local quiet="$1"
    [ "$quiet" != "quiet" ] && echo "🔄 Syncing personas..."

    init_db

    # 1. Compile new/modified files
    for md_file in "$PERSONAS_DIR"/*.persona; do
        [ -e "$md_file" ] || continue
        name=$(basename "$md_file" .persona)

        local db_hash=$(sqlite3 "$DB_FILE" "SELECT file_hash FROM persona_files WHERE name='$name';" 2>/dev/null)
        local file_hash=$(md5sum "$md_file" | cut -d' ' -f1)

        if [ "$db_hash" != "$file_hash" ]; then
            [ "$quiet" != "quiet" ] && echo "🎭 Compiling Persona: $name"
            compile_persona "$name"
        fi
    done

    # 2. Remove deleted files from DB (Ghost Purge)
    sqlite3 "$DB_FILE" "SELECT name FROM persona_files;" 2>/dev/null | while read -r db_name; do
        if [ ! -f "$PERSONAS_DIR/${db_name}.persona" ]; then
            [ "$quiet" != "quiet" ] && echo "🗑️ Removing Ghost Persona: $db_name"
            sqlite3 "$DB_FILE" "DELETE FROM personas WHERE name='$db_name';"
            sqlite3 "$DB_FILE" "DELETE FROM persona_files WHERE name='$db_name';"
        fi
    done
}

# Main command handler
case "${1:-status}" in
    compile) init_db; compile_persona "$2" ;;
    sync)    init_db; sync_personas ;;
    list)
        printf "%-12s │ %-20s │ %-21s │ %s\n" "sym_id" "emoji" "identity_name" "identity_title"
        printf "%-12s-%-20s-%-21s-%s\n" "------------" "--------------------" "---------------------" "------------------"
        sqlite3 -batch "$DB_FILE" "SELECT sym_id, identity_emoji, identity_name, identity_title FROM personas;" 2>/dev/null | while IFS='|' read -r sid emoji name title; do
            printf "%-12s │ %-20s │ %-21s │ %s\n" "$sid" "$emoji" "$name" "$title"
        done
        ;;
    active)  sqlite3 -header -column "$DB_FILE" "SELECT name FROM personas WHERE active=1;" 2>/dev/null ;;
    unload)  sqlite3 "$DB_FILE" "DELETE FROM personas WHERE name='$2'; DELETE FROM persona_files WHERE name='$2';" ;;
    *)       echo "Usage: $0 [compile|sync|list|active|unload]"; exit 1 ;;
esac
