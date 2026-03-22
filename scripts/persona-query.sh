#!/bin/bash

# Persona Query - Query loaded personas from DB
# Usage: ./persona-query.sh [field] [persona-name]
#        ./persona-query.sh ~p?<persona>.<field>

WORKSPACE="$HOME/.openclaw/workspace"
DB_FILE="$WORKSPACE/MPM/persona/personas.db"

# Support SymAI opcode: ~p?oracle.vibe
case "${1:-}" in
    ~p?*)
        # Parse ~p?oracle.vibe
        INPUT="${1#*~p?}"  # Remove ~p? prefix
        NAME="${INPUT%.*}"  # Everything before the dot
        FIELD="${INPUT#*.}"  # Everything after the dot
        ;;
    *)
        FIELD="$1"
        NAME="$2"
        ;;
esac

if [ ! -f "$DB_FILE" ]; then
    echo "❌ No persona DB found"
    echo "   Run: ./persona-compile.sh sync"
    exit 1
fi

# Resolve name to bytecode_alias if needed
if [ -n "$NAME" ]; then
    RESOLVED=$(sqlite3 "$DB_FILE" "SELECT name FROM personas WHERE name='$NAME' OR bytecode_alias='~p.${NAME:0:1}' LIMIT 1;" 2>/dev/null)
    if [ -n "$RESOLVED" ]; then
        NAME="$RESOLVED"
    fi
fi

if [ -z "$FIELD" ] || [ -z "$NAME" ]; then
    echo "Usage: $0 <field> <persona-name>"
    echo "       $0 ~p?<persona>.<field>     # SymAI shortcut"
    echo ""
    echo "SymAI Examples:"
    echo "  ~p?oracle.vibe    → Query oracle's vibe"
    echo "  ~p?harley.title   → Query harley's title"
    echo "  ~p?default.rules  → Query default's rules"
    echo ""
    echo "Fields:"
    echo "  name          - Persona name"
    echo "  identity      - Identity name"
    echo "  title         - Identity title"
    echo "  vibe          - Personality vibe"
    echo "  emoji         - Allowed emoji"
    echo "  voice         - Voice & tone description"
    echo "  rules         - Behavioral rules"
    echo "  context       - When to use"
    echo "  activation    - How to activate"
    echo "  all           - Show all fields"
    exit 1
fi

case "$FIELD" in
    name)
        sqlite3 "$DB_FILE" "SELECT name FROM personas WHERE name='$NAME';"
        ;;
    identity)
        sqlite3 "$DB_FILE" "SELECT identity_name FROM personas WHERE name='$NAME';"
        ;;
    title)
        sqlite3 "$DB_FILE" "SELECT identity_title FROM personas WHERE name='$NAME';"
        ;;
    vibe)
        sqlite3 "$DB_FILE" "SELECT identity_vibe FROM personas WHERE name='$NAME';"
        ;;
    emoji)
        sqlite3 "$DB_FILE" "SELECT identity_emoji FROM personas WHERE name='$NAME';"
        ;;
    voice)
        sqlite3 "$DB_FILE" "SELECT voice_tone FROM personas WHERE name='$NAME';"
        ;;
    rules)
        sqlite3 "$DB_FILE" "SELECT behavioral_rules FROM personas WHERE name='$NAME';"
        ;;
    context)
        sqlite3 "$DB_FILE" "SELECT context FROM personas WHERE name='$NAME';"
        ;;
    activation)
        sqlite3 "$DB_FILE" "SELECT activation FROM personas WHERE name='$NAME';"
        ;;
    all)
        sqlite3 -column -header "$DB_FILE" "SELECT * FROM personas WHERE name='$NAME';"
        ;;
    *)
        echo "❌ Unknown field: $FIELD"
        exit 1
        ;;
esac
