#!/bin/bash

# Persona Dashboard - Show full persona system status
# Usage: ./persona-dashboard.sh

WORKSPACE="$HOME/.openclaw/workspace"
PERSONAS_DIR="$WORKSPACE/MPM/persona"
DB_FILE="$PERSONAS_DIR/personas.db"
SCRIPT_DIR="$(dirname "$0")"

echo "🎭 Persona Dashboard"
echo "===================="

# Show Active Frequency (SymAI bytecode)
ACTIVE_PERSONA=$(sqlite3 "$DB_FILE" "SELECT name FROM personas WHERE active=1 LIMIT 1;" 2>/dev/null)
if [ -n "$ACTIVE_PERSONA" ]; then
    ACTIVE_ALIAS=$(sqlite3 "$DB_FILE" "SELECT bytecode_alias FROM personas WHERE name='$ACTIVE_PERSONA';" 2>/dev/null)
    ACTIVE_ALIAS=${ACTIVE_ALIAS:-"~p.${ACTIVE_PERSONA:0:1}"}
    echo -e "📡 Current Frequency: \033[1;36m${ACTIVE_ALIAS}\033[0m"
fi
echo ""

# Count MD files
shopt -s nullglob
md_files=("$PERSONAS_DIR"/*.persona)
shopt -u nullglob
md_count=${#md_files[@]}

# Count loaded in DB
db_count=0
if [ -f "$DB_FILE" ]; then
    db_count=$(sqlite3 "$DB_FILE" "SELECT COUNT(*) FROM personas;" 2>/dev/null)
fi

# DB size
db_size="N/A"
if [ -f "$DB_FILE" ]; then
    db_size=$(stat -c%s "$DB_FILE" 2>/dev/null || stat -f%z "$DB_FILE" 2>/dev/null)
    db_size="${db_size}B"
fi

echo "📊 Status"
echo "--------"
printf "%-20s %s\n" "MD files:" "$md_count"
printf "%-20s %s\n" "Loaded in DB:" "$db_count"
printf "%-20s %s\n" "DB size:" "$db_size"
printf "%-20s %s\n" "Sync status:" "$([ "$md_count" -eq "$db_count" ] && echo "✅ Synced" || echo "⚠️  Out of sync")"
echo ""

# Show loaded personas
if [ "$db_count" -gt 0 ]; then
    echo "🎭 Loaded Personas"
    echo "------------------"
    printf "%-4s %-12s %-20s %-25s\n" "EMOJI" "NAME" "IDENTITY" "TITLE"
    printf "%-4s %-12s %-20s %-25s\n" "----" "----" "--------" "-----"
    
    sqlite3 "$DB_FILE" "SELECT identity_emoji, name, identity_name, identity_title FROM personas;" | while IFS='|' read -r emoji name identity title; do
        emoji="${emoji:-✅}"
        printf "%-4s %-12s %-20s %-25s\n" "$emoji" "$name" "$identity" "$title"
    done
    echo ""
fi

# Quick actions
echo "⚡ Quick Actions"
echo "--------------"
echo "  ./persona-ls.sh              # List all personas"
echo "  ./persona-ls.sh --loaded     # Show loaded in DB"
echo "  ./persona-ls.sh --tree       # Show tree structure"
echo "  ./persona-activate.sh <name> # Activate persona"
echo "  ./persona-compile.sh sync    # Sync all to DB"
echo "  ./persona-query.sh vibe <n>  # Query field"
echo ""

# Token savings estimate
if [ "$db_count" -gt 0 ]; then
    echo "💰 Token Savings"
    echo "----------------"
    # Estimate: MD files ~1KB each, DB query ~50 bytes
    md_total=$((md_count * 1024))
    db_query_size=$((db_count * 50))
    savings=$(( (md_total - db_query_size) * 100 / md_total ))
    echo "  Querying metadata via DB: ~${savings}% savings"
    echo "  vs loading full MD files"
    echo ""
fi

echo "📁 Paths"
echo "------"
echo "  Personas: $PERSONAS_DIR"
echo "  DB:       $DB_FILE"
echo ""
