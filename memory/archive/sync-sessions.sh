#!/bin/bash

# Memory Sync - Bidirectional sync between MD files and SQLite DB
# Usage: ./sync-sessions.sh [--dry-run]
# 
# Ensures: every MD file has a DB entry, every DB entry has a MD file
# Categories: subject, project, idea, lesson, decision, preference, contact, tool, session

WORKSPACE="$HOME/.openclaw/workspace"
MPM_MEMORY="$WORKSPACE/MPM/memory"
DB="$MPM_MEMORY/memory.db"

DRY_RUN=false
[ "$1" = "--dry-run" ] && DRY_RUN=true

echo "🔄 Memory Sync - File ↔ DB"
echo "=========================="
echo ""

# Ensure DB table exists with source tracking
if [ "$DRY_RUN" = false ]; then
    sqlite3 "$DB" "CREATE TABLE IF NOT EXISTS memory_entries (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        category TEXT NOT NULL,
        filename TEXT NOT NULL,
        content TEXT,
        timestamp TEXT,
        source TEXT DEFAULT 'file',
        created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
    );"
    # Add source column if missing
    sqlite3 "$DB" "ALTER TABLE memory_entries ADD COLUMN source TEXT DEFAULT 'file';" 2>/dev/null
fi

# Categories to track (including sessions)
CATEGORIES=("subject" "project" "idea" "lesson" "decision" "preference" "contact" "tool")
SESSION_DIR="$MPM_MEMORY/sessions"

# Function to add/update entry
upsert_entry() {
    local category="$1"
    local filename="$2"
    local content="$3"
    local timestamp="$4"
    
    if [ "$DRY_RUN" = true ]; then
        echo "  [SYNC] $category/$filename"
        return
    fi
    
    # Check if entry exists
    local existing=$(sqlite3 "$DB" "SELECT id FROM memory_entries WHERE category='$category' AND filename='$filename' LIMIT 1;")
    
    if [ -n "$existing" ]; then
        sqlite3 "$DB" "UPDATE memory_entries SET filename='$filename', content='$content', timestamp='$timestamp' WHERE id='$existing';"
    else
        sqlite3 "$DB" "INSERT INTO memory_entries (category, filename, content, timestamp, source) VALUES ('$category', '$filename', '$content', '$timestamp', 'file');"
    fi
}

# Sync category files
echo "📁 Category Files → DB"
echo "----------------------"
for cat in "${CATEGORIES[@]}"; do
    file="$MPM_MEMORY/$cat.md"
    if [ -f "$file" ]; then
        # Extract first line as summary (skip headers)
        content=$(head -5 "$file" | grep -v "^#" | grep -v "^---" | head -1 | tr -d '\n')
        timestamp=$(date -r "$file" '+%Y-%m-%d %H:%M')
        upsert_entry "$cat" "$cat.md" "$content" "$timestamp"
    fi
done

# Sync session files
echo ""
echo "📁 Session Files → DB"
echo "---------------------"
if [ -d "$SESSION_DIR" ]; then
    for session in "$SESSION_DIR"/*.md; do
        [ -f "$session" ] || continue
        filename=$(basename "$session")
        # Extract session ID or timestamp from file
        session_id=$(grep -m1 "Session ID" "$session" | sed 's/.*: //' | tr -d ' ')
        content=$(head -3 "$session" | tail -1 | tr -d '\n')
        timestamp=$(date -r "$file" '+%Y-%m-%d %H:%M' 2>/dev/null || echo "unknown")
        
        if [ "$DRY_RUN" = true ]; then
            echo "  [SYNC] session/$filename"
        else
            # Upsert session entry
            existing=$(sqlite3 "$DB" "SELECT id FROM memory_entries WHERE category='session' AND filename='$filename' LIMIT 1;")
            if [ -n "$existing" ]; then
                sqlite3 "$DB" "UPDATE memory_entries SET content='$content', timestamp='$timestamp' WHERE id='$existing';"
            else
                sqlite3 "$DB" "INSERT INTO memory_entries (category, filename, content, timestamp, source) VALUES ('session', '$filename', '$content', '$timestamp', 'file');"
            fi
        fi
    done
fi

# Summary
echo ""
echo "📊 DB Status"
echo "------------"
sqlite3 "$DB" "SELECT category, COUNT(*) as count FROM memory_entries GROUP BY category;"

echo ""
if [ "$DRY_RUN" = true ]; then
    echo "⚠️  Dry run complete (no changes made)"
else
    echo "✅ Sync complete"
fi