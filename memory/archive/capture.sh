#!/bin/bash

# Memory Capture - Extract important info from chat into categorized MD files
# Usage: ./capture.sh [--subject|--project|--idea|--lesson|--decision|--preference|--contact|--tool] "content"
#
# Created by: v & Great_808@flowbte.com
# First release: 2026-03-20

WORKSPACE="$HOME/.openclaw/workspace"
MPM_MEMORY="$WORKSPACE/MPM/memory"

# Categories
CATEGORIES=("subject" "project" "idea" "lesson" "decision" "preference" "contact" "tool")

# Ensure directory exists
mkdir -p "$MPM_MEMORY"

show_help() {
    cat << 'EOF'
📝 Memory Capture - Categorize important information

USAGE:
    ./capture.sh --<category> "content to save"

CATEGORIES:
    --subject     Topic-based knowledge (e.g., "SQLite for agent state")
    --project     Project-specific info (e.g., "Flowbyte WordPress site")
    --idea        Concepts/insights (e.g., "DB-first architecture")
    --lesson      Learnings/mistakes (e.g., "Always test before deploy")
    --decision    Important choices (e.g., "Use local backups")
    --preference  User preferences (e.g., " prefers concise replies")
    --contact     People mentioned (e.g., "v - developer, timezone UTC")
    --tool        Tool/setup info (e.g., "OpenRouter API key configured")

COMMANDS:
    --refresh     Full MPM sync (detects deleted files, removes from DB)
    --sync        Sync memory to DB (quiet mode for auto-capture)
    --compact     Review and merge related entries
    --list        Show all categorized memory

EXAMPLES:
    ./capture.sh --project "Flowbyte WordPress - Hostinger, admin user 808"
    ./capture.sh --lesson "Test REST API writes before assuming access"
    ./capture.sh --idea "DB-first: 97% token savings vs MD files"

OUTPUT:
    Creates/updates: MPM/memory/<category>.md
    Format: Markdown with timestamp and categorized content

AUTO-SYNC:
    Every capture auto-syncs to memory.db (SQLite) - lightweight, instant.
EOF
}

list_memory() {
    echo "📚 Categorized Memory"
    echo "===================="
    echo ""
    
    for cat in "${CATEGORIES[@]}"; do
        file="$MPM_MEMORY/${cat}.md"
        if [ -f "$file" ]; then
            # Count entries (lines starting with "**")
            count=$(grep -c "^\*\*" "$file" 2>/dev/null)
            [ "$count" -eq 0 ] && count=0
            printf "%-12s %s entries\n" "$cat" "$count"
        else
            printf "%-12s 0 entries\n" "$cat"
        fi
    done
    echo ""
}

compact_memory() {
    echo "🧹 Compacting memory files..."
    echo ""
    
    for cat in "${CATEGORIES[@]}"; do
        file="$MPM_MEMORY/${cat}.md"
        if [ -f "$file" ]; then
            lines=$(wc -l < "$file")
            if [ "$lines" -gt 50 ]; then
                echo "⚠️  $cat.md has $lines lines - consider archiving"
            fi
        fi
    done
    
    echo ""
    echo "✅ Review complete. Manually merge related entries if needed."
}

sync_to_db() {
    local quiet="${1:-false}"
    [ "$quiet" != "true" ] && echo "🔄 Syncing categorized memory to DB..."
    [ "$quiet" != "true" ] && echo ""
    
    local db_file="$MPM_MEMORY/memory.db"
    
    # Initialize DB
    sqlite3 "$db_file" <<'SCHEMA' 2>/dev/null
CREATE TABLE IF NOT EXISTS memory_entries (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    category TEXT,
    content TEXT,
    timestamp TEXT,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS memory_meta (
    category TEXT PRIMARY KEY,
    entry_count INTEGER,
    last_synced TIMESTAMP
);
SCHEMA
    
    # Clear and rebuild from files (files are source of truth)
    sqlite3 "$db_file" "DELETE FROM memory_entries;" 2>/dev/null
    sqlite3 "$db_file" "DELETE FROM memory_meta;" 2>/dev/null
    
    for cat in "${CATEGORIES[@]}"; do
        file="$MPM_MEMORY/${cat}.md"
        if [ -f "$file" ]; then
            # Extract entries and insert into DB
            grep "^\*\*" "$file" | while IFS= read -r line; do
                content=$(echo "$line" | sed 's/^\*\*[0-9-]* [0-9:]*\*\* //')
                timestamp=$(echo "$line" | sed 's/^\*\*\([0-9-]* [0-9:]*\)\*\*.*/\1/')
                
                sqlite3 "$db_file" "INSERT INTO memory_entries (category, content, timestamp) VALUES ('$cat', '$(echo "$content" | sed "s/'/''/g")', '$timestamp');"
            done
            
            count=$(grep -c "^\*\*" "$file" 2>/dev/null)
            count=${count:-0}
            count=$(echo "$count" | tr -d '[:space:]' | head -1)
            sqlite3 "$db_file" "INSERT INTO memory_meta (category, entry_count, last_synced) VALUES ('$cat', $count, CURRENT_TIMESTAMP);"
            
            [ "$quiet" != "true" ] && echo "✅ Synced $cat ($count entries)"
        else
            # File missing - remove from DB
            sqlite3 "$db_file" "DELETE FROM memory_meta WHERE category='$cat';" 2>/dev/null
            [ "$quiet" != "true" ] && echo "🗑️  Removed $cat (file missing)"
        fi
    done
    
    [ "$quiet" != "true" ] && echo ""
    [ "$quiet" != "true" ] && echo "✅ DB sync complete: $db_file"
}

refresh_all() {
    echo "🔄 MPM Refresh - Full filesystem ↔ DB sync"
    echo "==========================================="
    echo ""
    
    # Check MPM folder exists
    if [ ! -d "$WORKSPACE/MPM" ]; then
        echo "❌ MPM folder missing: $WORKSPACE/MPM"
        echo "   Creating fresh MPM structure..."
        mkdir -p "$WORKSPACE/MPM/memory" "$WORKSPACE/MPM/persona" "$WORKSPACE/MPM/mode"
        echo "✅ Created MPM folders"
    fi
    
    # Refresh memory
    echo "📝 Refreshing memory..."
    if [ ! -d "$MPM_MEMORY" ]; then
        mkdir -p "$MPM_MEMORY"
        echo "   Created MPM/memory/"
    fi
    sync_to_db true
    
    # Refresh persona DB
    echo ""
    echo "🎭 Refreshing personas..."
    local persona_db="$WORKSPACE/MPM/persona/personas.db"
    if [ ! -d "$WORKSPACE/MPM/persona" ]; then
        mkdir -p "$WORKSPACE/MPM/persona"
    fi
    
    # Sync persona files
    shopt -s nullglob
    local persona_files=("$WORKSPACE/MPM/persona"/*.persona)
    shopt -u nullglob
    
    declare -A seen_personas
    for pf in "${persona_files[@]}"; do
        name=$(basename "$pf" .persona)
        seen_personas[$name]=1
    done
    
    # Remove missing from DB
    if [ -f "$persona_db" ]; then
        sqlite3 "$persona_db" "SELECT name FROM persona_files;" 2>/dev/null | while read -r name; do
            if [ -z "${seen_personas[$name]}" ]; then
                echo "   🗑️  Removed persona: $name (file missing)"
                sqlite3 "$persona_db" "DELETE FROM personas WHERE name='$name';" 2>/dev/null
                sqlite3 "$persona_db" "DELETE FROM persona_files WHERE name='$name';" 2>/dev/null
            fi
        done
        echo "   ✅ Persona DB synced"
    fi
    
    # Refresh mode DB
    echo ""
    echo "🛠️ Refreshing modes..."
    local mode_db="$WORKSPACE/MPM/mode/m3.db"
    if [ ! -d "$WORKSPACE/MPM/mode" ]; then
        mkdir -p "$WORKSPACE/MPM/mode"
    fi
    
    # Sync mode files
    shopt -s nullglob
    local mode_files=("$WORKSPACE/MPM/mode"/*.mode)
    shopt -u nullglob
    
    declare -A seen_modes
    for mf in "${mode_files[@]}"; do
        name=$(basename "$mf" .mode)
        seen_modes[$name]=1
    done
    
    # Remove missing from DB
    if [ -f "$mode_db" ]; then
        sqlite3 "$mode_db" "SELECT name FROM mode_files;" 2>/dev/null | while read -r name; do
            if [ -z "${seen_modes[$name]}" ]; then
                echo "   🗑️  Removed mode: $name (file missing)"
                sqlite3 "$mode_db" "DELETE FROM modes WHERE name='$name';" 2>/dev/null
                sqlite3 "$mode_db" "DELETE FROM mode_files WHERE name='$name';" 2>/dev/null
                sqlite3 "$mode_db" "DELETE FROM active_modes WHERE name='$name';" 2>/dev/null
            fi
        done
        echo "   ✅ Mode DB synced"
    fi
    
    echo ""
    echo "✅ MPM refresh complete"
    echo "   Memory: $MPM_MEMORY"
    echo "   Persona: $WORKSPACE/MPM/persona"
    echo "   Mode: $WORKSPACE/MPM/mode"
}

capture() {
    local category="$1"
    local content="$2"
    local timestamp=$(date '+%Y-%m-%d %H:%M')
    local file="$MPM_MEMORY/${category}.md"
    
    # Create file with header if it doesn't exist
    if [ ! -f "$file" ]; then
        cat > "$file" << HEADER
# ${category^}

*$(get_category_description "$category").*

---

*No entries recorded yet.*
HEADER
    fi
    
    # Check if file has the template "No entries" line - remove it
    if grep -q "^\*No entries" "$file"; then
        sed -i '/^\*No entries/d' "$file"
    fi
    
    # Append entry (format: **timestamp** — content)
    echo "**$timestamp** — $content" >> "$file"
    
    echo "✅ Captured to MPM/memory/${category}.md"
    echo "   Category: $category"
    echo "   Content: $content"
    
    # Auto-sync to DB (lightweight operation)
    sync_to_db true &>/dev/null
}

get_category_description() {
    case "$1" in
        subject) echo "Topic knowledge and domain expertise" ;;
        project) echo "Active and past project tracking" ;;
        idea) echo "Concepts, insights, and sparks" ;;
        lesson) echo "Learnings and insights earned through experience" ;;
        decision) echo "Choices made and rationale" ;;
        preference) echo "User preferences and opinions" ;;
        contact) echo "People and relationships" ;;
        tool) echo "Tools, setup, and technical config" ;;
    esac
}

# Main handler
case "${1:-}" in
    --subject)
        capture "subject" "$2"
        ;;
    --project)
        capture "project" "$2"
        ;;
    --idea)
        capture "idea" "$2"
        ;;
    --lesson)
        capture "lesson" "$2"
        ;;
    --decision)
        capture "decision" "$2"
        ;;
    --preference)
        capture "preference" "$2"
        ;;
    --contact)
        capture "contact" "$2"
        ;;
    --tool)
        capture "tool" "$2"
        ;;
    --compact)
        compact_memory
        ;;
    --sync)
        sync_to_db false
        ;;
    --refresh)
        refresh_all
        ;;
    --list)
        list_memory
        ;;
    --help|-h)
        show_help
        ;;
    *)
        echo "Usage: $0 --<category> \"content\""
        echo "Run '$0 --help' for categories and examples"
        exit 1
        ;;
esac
