#!/bin/bash

# Persona LS - List personas in terminal-friendly format
# Usage: ./persona-ls.sh [--loaded] [--all] [--tree]

WORKSPACE="$HOME/.openclaw/workspace"
PERSONAS_DIR="$WORKSPACE/MPM/persona"
DB_FILE="$WORKSPACE/MPM/persona/personas.db"

MODE="default"

while [[ $# -gt 0 ]]; do
    case $1 in
        --loaded) MODE="loaded"; shift ;;
        --all) MODE="all"; shift ;;
        --tree) MODE="tree"; shift ;;
        *) shift ;;
    esac
done

if [ "$MODE" = "loaded" ]; then
    echo "🎭 Loaded Personas (in DB)"
    echo "========================="
    echo ""
    
    if [ -f "$DB_FILE" ]; then
        printf "%-10s %-1s %-28s %-1s %-18s\n" "NAME" "│" "IDENTITY" "│" "TITLE"
        printf "%-10s %-1s %-28s %-1s %-18s\n" "----" "│" "--------" "│" "-----"
        
        sqlite3 "$DB_FILE" "SELECT name, identity_name, identity_title FROM personas;" | while IFS='|' read -r name identity title; do
            printf "%-10s │ %-28s │ %-18s\n" "$name" "$identity" "$title"
        done
    else
        echo "(no personas loaded)"
    fi
    echo ""
    
elif [ "$MODE" = "tree" ]; then
    echo "📁 Persona Structure"
    echo "===================="
    echo ""
    echo "personas/"
    
    shopt -s nullglob
    for file in "$PERSONAS_DIR"/*.persona; do
        name=$(basename "$file" .persona)
        size=$(stat -c%s "$file" 2>/dev/null || stat -f%z "$file" 2>/dev/null)
        loaded="❌"
        
        if [ -f "$DB_FILE" ]; then
            in_db=$(sqlite3 "$DB_FILE" "SELECT COUNT(*) FROM persona_files WHERE name='$name';")
            [ "$in_db" -gt 0 ] && loaded="✅"
        fi
        
        printf "├── %-20s (%s, %s)\n" "$name.persona" "${size}B" "$loaded"
    done
    shopt -u nullglob
    
    echo "└── loaded/"
    if [ -f "$DB_FILE" ]; then
        db_size=$(stat -c%s "$DB_FILE" 2>/dev/null || stat -f%z "$DB_FILE" 2>/dev/null)
        count=$(sqlite3 "$DB_FILE" "SELECT COUNT(*) FROM personas;")
        printf "    ├── personas.db (%s, %s personas)\n" "${db_size}B" "$count"
    fi
    echo "    └── PERSONA-COMPILER-PATTERN.md"
    echo ""
    
else
    # Default: simple list
    echo "🎭 Available Personas"
    echo "===================="
    echo ""
    
    shopt -s nullglob
    for file in "$PERSONAS_DIR"/*.persona; do
        name=$(basename "$file" .persona)
        title=$(grep -m 1 "^title:" "$file" | cut -d':' -f2- | tr -d '*\r-' | xargs)
        title="${title:-${name^}}"
        
        loaded="❌"
        if [ -f "$DB_FILE" ]; then
            in_db=$(sqlite3 "$DB_FILE" "SELECT COUNT(*) FROM persona_files WHERE name='$name';")
            [ "$in_db" -gt 0 ] && loaded="✅"
        fi
        
        printf "%s %-15s %s\n" "$loaded" "$name" "$title"
        [ -n "$vibe" ] && echo "                  $vibe"
    done
    shopt -u nullglob
    
    echo ""
    count=$(ls "$PERSONAS_DIR"/*.persona 2>/dev/null | wc -l)
    echo "Total: $count personas"
    echo ""
    echo "Commands:"
    echo "  ./persona-ls.sh          # Simple list"
    echo "  ./persona-ls.sh --loaded # Show loaded in DB"
    echo "  ./persona-ls.sh --tree   # Show tree structure"
    echo ""
fi
