#!/bin/bash

# ⚡ FlowByte Persona Manager
# Handles activation, creation, and DB reads for identities

WORKSPACE="$HOME/.openclaw/workspace"
PERSONAS_DIR="$WORKSPACE/MPM/persona"
DB_FILE="$PERSONAS_DIR/personas.db"
SCRIPT_DIR="$WORKSPACE/skills/mpm/scripts"

# Ensure DB exists
if [ ! -f "$DB_FILE" ]; then
    "$SCRIPT_DIR/persona-compile.sh" sync quiet >/dev/null 2>&1
fi

case "${1:-list}" in
    list)
        echo -e "\033[1;36m🎭 FlowByte Identity Matrix\033[0m"
        echo "========================================"
        sqlite3 -column -header "$DB_FILE" "SELECT sym_id as Opcode, identity_emoji as '#', identity_name as Name, identity_vibe as Vibe FROM personas;" 2>/dev/null
        count=$(sqlite3 "$DB_FILE" "SELECT COUNT(*) FROM personas;" 2>/dev/null)
        echo "----------------------------------------"
        echo "Total: ${count:-0} personas available."
        ;;

    activate|load)
        if [ -z "$2" ]; then echo "Usage: mpm persona activate <name>"; exit 1; fi
        PERSONA_NAME="$2"

        # 1. Update DB State
        sqlite3 "$DB_FILE" "UPDATE personas SET active = 0;"
        sqlite3 "$DB_FILE" "UPDATE personas SET active = 1, loaded_at = CURRENT_TIMESTAMP WHERE name = '$PERSONA_NAME';"

        # 2. Update active.json (The Gateway Bridge)
        # We preserve the current mode stack during persona swaps
        MODES=$(grep -o '"modes":\[[^]]*\]' "$WORKSPACE/MPM/active.json" 2>/dev/null | cut -d':' -f2-)
        echo "{\"persona\":\"$PERSONA_NAME\",\"modes\":${MODES:-[]},\"updated\":\"$(date -Iseconds)\"}" > "$WORKSPACE/MPM/active.json"

        # 3. Pulse the Terminal
        SYM_ID=$(sqlite3 "$DB_FILE" "SELECT sym_id FROM personas WHERE name='$PERSONA_NAME';" 2>/dev/null)
        echo -e "⚡ \033[1;36mSymAI Pulse:\033[0m ${SYM_ID:-~p.$PERSONA_NAME}!act"
        ;;

    *)
        # Passthrough to help or list
        echo "Usage: mpm persona [list|activate|create|preview] [name]"
        ;;
esac
