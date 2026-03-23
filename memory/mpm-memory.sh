#!/bin/bash

# Memory Quick Access - Top entries, search, and maintenance
# Usage: mpm-memory.sh [top|search|dedupe|expire|stats]

WORKSPACE="$HOME/.openclaw/workspace"
DB="$WORKSPACE/MPM/memory/memory.db"

case "${1:-stats}" in
    top)
        echo "🔥 Top Priority Entries"
        echo "======================="
        echo ""
        sqlite3 -header -column "$DB" "
            SELECT category, content, importance, timestamp 
            FROM memory_entries 
            WHERE importance >= 5 
            ORDER BY importance DESC, timestamp DESC 
            LIMIT 10;"
        ;;
    
    search)
        query="${2:-}"
        if [ -z "$query" ]; then
            echo "Usage: mpm-memory.sh search <term>"
            exit 1
        fi
        echo "🔍 Searching for: $query"
        echo "======================="
        sqlite3 -header -column "$DB" "
            SELECT category, content, importance 
            FROM memory_entries 
            WHERE content LIKE '%$query%' 
            ORDER BY importance DESC;"
        ;;
    
    dedupe)
        echo "🧹 Deduplicating entries..."
        # Keep highest importance duplicate
        sqlite3 "$DB" "
            DELETE FROM memory_entries 
            WHERE id NOT IN (
                SELECT MIN(id) 
                FROM memory_entries 
                WHERE content IS NOT NULL AND content != ''
                GROUP BY content
            );"
        echo "✅ Done. Current count:"
        sqlite3 "$DB" "SELECT category, COUNT(*) FROM memory_entries GROUP BY category;"
        ;;
    
    expire)
        days="${2:-30}"
        echo "🗑️  Expiring entries older than $days days..."
        sqlite3 "$DB" "
            DELETE FROM memory_entries 
            WHERE timestamp < datetime('now', '-$days days') 
            AND category != 'session';"
        echo "✅ Expired old entries"
        ;;
    
    stats|*)
        echo "📊 Memory Stats"
        echo "=============="
        echo ""
        echo "By category:"
        sqlite3 -header -column "$DB" "
            SELECT category, COUNT(*) as count, MAX(importance) as max_imp
            FROM memory_entries 
            GROUP BY category 
            ORDER BY count DESC;"
        echo ""
        echo "Recent high-priority:"
        sqlite3 -header -column "$DB" "
            SELECT category, substr(content, 1, 50) as content, importance, timestamp
            FROM memory_entries 
            WHERE importance >= 5 
            ORDER BY timestamp DESC 
            LIMIT 5;"
        ;;
esac