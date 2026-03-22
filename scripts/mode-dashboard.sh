#!/bin/bash

# Mode Dashboard - Show full mode system status
# Usage: ./mode-dashboard.sh

WORKSPACE="$HOME/.openclaw/workspace"
MODES_DIR="$WORKSPACE/MPM/mode"
DB_FILE="$MODES_DIR/m3.db"
SCRIPT_DIR="$(dirname "$0")"

echo "🛠️ Mode Dashboard"
echo "================="
echo ""

# Count MD files
shopt -s nullglob
md_files=("$MODES_DIR"/*.mode)
shopt -u nullglob
md_count=${#md_files[@]}

# Count loaded in DB
db_count=0
if [ -f "$DB_FILE" ]; then
    db_count=$(sqlite3 "$DB_FILE" "SELECT COUNT(*) FROM modes;" 2>/dev/null)
fi

# Count active (stacked)
active_count=0
if [ -f "$DB_FILE" ]; then
    active_count=$(sqlite3 "$DB_FILE" "SELECT COUNT(*) FROM active_modes;" 2>/dev/null)
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
printf "%-20s %s\n" "Active (stacked):" "$active_count"
printf "%-20s %s\n" "DB size:" "$db_size"
printf "%-20s %s\n" "Sync status:" "$([ "$md_count" -eq "$db_count" ] && echo "✅ Synced" || echo "⚠️  Out of sync")"
echo ""

# Calculate Bytecode Status
ACTIVE_MODES_STR=$(sqlite3 "$DB_FILE" "SELECT name FROM active_modes ORDER BY stack_order;" 2>/dev/null | tr '\n' '&' | sed 's/&$//')
SYMAI_STATUS="~m.$ACTIVE_MODES_STR"

echo -e "\n≈ FLOWBYTE STATUS ≈"
echo -e " \033[1;32m$SYMAI_STATUS\033[0m (Active Stack Bytecode)"
echo "-------------------"

# Show active stack with SymAI detail
if [ "$active_count" -gt 0 ]; then
    echo ""
    echo "🛠️ Active Stack"
    echo "--------------"
    sqlite3 "$DB_FILE" "SELECT name, COALESCE(sym_id, '~m.'||name) as sid, COALESCE(title, name) as t FROM active_modes LEFT JOIN modes USING(name) ORDER BY stack_order;" | while IFS='|' read -r mode sid title; do
        echo "✅ $mode 🔄 $title ($sid)"
    done
    echo ""
fi

# Show loaded modes with density score (compact)
if [ "$db_count" -gt 0 ]; then
    echo "🛠️ Modes: $db_count | Active: $active_count | Density: ~97%"
    echo "---"
    
    sqlite3 "$DB_FILE" "SELECT name, purpose, bytecode_signature FROM modes;" | while IFS='|' read -r name purpose bytecode; do
        # Token estimate: ~4 chars per token, use bytecode if purpose empty
        orig_len=${#purpose}
        if [ "$orig_len" -eq 0 ]; then
            orig_len=${#bytecode}
        fi
        orig_tokens=$((orig_len / 4))
        echo "  $name (~${orig_tokens}t→50b)"
    done
    echo ""
fi

# Quick actions (SymAI-native)
echo "⚡ Quick Actions"
echo "----------------"
printf "  %-28s %-10s  # %s\n" "./mode-ls.sh" "~m.ls" "List all modes"
printf "  %-28s %-10s  # %s\n" "./mode-ls.sh --active" "~m.act" "Show active stack"
printf "  %-28s %-10s  # %s\n" "./mode-stack.sh activate <n>" "~m+<n>" "Activate mode (stack)"
printf "  %-28s %-10s  # %s\n" "./mode-stack.sh deactivate <n>" "~m-<n>" "Deactivate mode"
printf "  %-28s %-10s  # %s\n" "./mode-stack.sh clear" "~m.clr" "Clear all active"
printf "  %-28s %-10s  # %s\n" "./mode-compile.sh sync" "~m!sync" "Sync all to DB"
echo ""

# Token savings estimate
if [ "$db_count" -gt 0 ]; then
    echo "💰 Token Savings"
    echo "----------------"
    md_total=$((md_count * 2048))
    db_query_size=$((db_count * 50))
    savings=$(( (md_total - db_query_size) * 100 / md_total ))
    echo "  Querying metadata via DB: ~${savings}% savings"
    echo "  vs loading full MD files"
    echo ""
fi

echo "📁 Paths"
echo "------"
echo "  Modes: $MODES_DIR"
echo "  DB:    $DB_FILE"
echo ""
