#!/bin/bash

# Mode LS - List modes in terminal-friendly format
# Usage: ./mode-ls.sh [--loaded] [--active] [--tree]
#        ./mode-ls.sh ~m? [mode]     # SymAI: show all
#        ./mode-ls.sh ~m?+           # SymAI: show active stack
#        ./mode-ls.sh ~m?t            # SymAI: show tree

WORKSPACE="$HOME/.openclaw/workspace"
MODES_DIR="$WORKSPACE/MPM/mode"
DB_FILE="$WORKSPACE/MPM/mode/m3.db"

MODE="default"

# Parse arguments (supports SymAI-style ~m? commands)
while [[ $# -gt 0 ]]; do
    case "$1" in
        --loaded|~m?l) MODE="loaded"; shift ;;
        --active|~m?+|~m.act) MODE="active"; shift ;;
        --tree|~m?t) MODE="tree"; shift ;;
        ~m?) MODE="default"; shift ;;
        ~m) MODE="default"; shift ;;
        *) shift ;;
    esac
done

if [ "$MODE" = "loaded" ]; then
    echo "🛠️ Loaded Modes (in DB)"
    echo "======================="
    echo ""
    
    if [ -f "$DB_FILE" ]; then
        printf "%-15s %-50s\n" "NAME" "PURPOSE"
        printf "%-15s %-50s\n" "----" "-------"
        
        sqlite3 "$DB_FILE" "SELECT name, purpose FROM modes;" | while IFS='|' read -r name purpose; do
            printf "%-15s %-50s\n" "$name" "$purpose"
        done
    else
        echo "(no modes loaded)"
    fi
    echo ""
    
elif [ "$MODE" = "active" ]; then
    echo "🛠️ Active Modes"
    echo "========================="
    echo ""
    
    if [ -f "$DB_FILE" ]; then
        count=$(sqlite3 "$DB_FILE" "SELECT COUNT(*) FROM active_modes;" 2>/dev/null)
        if [ -n "$count" ] && [ "$count" -gt 0 ] 2>/dev/null; then
            echo "Active stack:"
            sqlite3 "$DB_FILE" "SELECT name, stack_order FROM active_modes ORDER BY stack_order;" 2>/dev/null | while IFS='|' read -r name order; do
                printf "  %d. %s\n" "$((order + 1))" "$name"
            done
            echo ""
        else
            echo "(no active modes)"
        fi
    else
        echo "(no modes loaded)"
    fi
    echo ""
    
elif [ "$MODE" = "tree" ]; then
    echo "📁 Mode Structure"
    echo "================="
    echo ""
    echo "modes/"
    
    shopt -s nullglob
    for file in "$MODES_DIR"/*.mode; do
        name=$(basename "$file" .mode)
        size=$(stat -c%s "$file" 2>/dev/null || stat -f%z "$file" 2>/dev/null)
        loaded="❌"
        
        if [ -f "$DB_FILE" ]; then
            in_db=$(sqlite3 "$DB_FILE" "SELECT COUNT(*) FROM mode_files WHERE name='$name';" 2>/dev/null)
            if [ -n "$in_db" ] && [ "$in_db" -gt 0 ] 2>/dev/null; then
                loaded="✅"
            fi
        fi
        
        active="❌"
        if [ -f "$DB_FILE" ]; then
            is_active=$(sqlite3 "$DB_FILE" "SELECT COUNT(*) FROM active_modes WHERE name='$name';" 2>/dev/null)
            if [ -n "$is_active" ] && [ "$is_active" -gt 0 ] 2>/dev/null; then
                active="🟢"
            fi
        fi
        
        printf "├── %-20s (%s, %s %s)\n" "$name.mode" "${size}B" "$loaded" "$active"
    done
    shopt -u nullglob
    
    echo "└── loaded/"
    if [ -f "$DB_FILE" ]; then
        db_size=$(stat -c%s "$DB_FILE" 2>/dev/null || stat -f%z "$DB_FILE" 2>/dev/null)
        count=$(sqlite3 "$DB_FILE" "SELECT COUNT(*) FROM modes;" 2>/dev/null)
        active_count=$(sqlite3 "$DB_FILE" "SELECT COUNT(*) FROM active_modes;" 2>/dev/null)
        printf "    ├── modes.db (%s, %s modes, %s active)\n" "${db_size}B" "${count:-0}" "${active_count:-0}"
    fi
    echo "    └── MODE-PATTERN.md"
    echo ""
    
else
    # Default: simple list
    echo "🛠️ Available Modes"
    echo "=================="
    echo ""
    
    shopt -s nullglob
    for file in "$MODES_DIR"/*.mode; do
        name=$(basename "$file" .mode)
        purpose=$(head -20 "$file" | grep -A2 '^## Purpose' | tail -1 | sed 's/^## Purpose //')
        
        loaded="❌"
        if [ -f "$DB_FILE" ]; then
            in_db=$(sqlite3 "$DB_FILE" "SELECT COUNT(*) FROM mode_files WHERE name='$name';" 2>/dev/null)
            if [ -n "$in_db" ] && [ "$in_db" -gt 0 ] 2>/dev/null; then
                loaded="✅"
            fi
        fi
        
        active="❌"
        if [ -f "$DB_FILE" ]; then
            is_active=$(sqlite3 "$DB_FILE" "SELECT COUNT(*) FROM active_modes WHERE name='$name';" 2>/dev/null)
            if [ -n "$is_active" ] && [ "$is_active" -gt 0 ] 2>/dev/null; then
                active="🟢"
            fi
        fi
        
        printf "%s %s %-15s %s\n" "$loaded" "$active" "$name" "$purpose"
    done
    shopt -u nullglob
    
    echo ""
    count=$(ls "$MODES_DIR"/*.mode 2>/dev/null | wc -l)
    echo "Total: $count modes"
    echo ""
    echo "Commands (SymAI-native):"
    echo "  ./mode-ls.sh              ~m?    # Show all available"
    echo "  ./mode-ls.sh --loaded     ~m?l   # Show loaded in DB"
    echo "  ./mode-ls.sh --active     ~m?+   # Show active stack"
    echo "  ./mode-ls.sh --tree       ~m?t   # Show architecture"
    echo ""
fi
