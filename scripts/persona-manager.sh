#!/bin/bash

# ⚡ FlowByte Persona Manager
# Handles activation, creation, and DB reads for identities

# Detect correct workspace path
if [ -d "$HOME/.openclaw/workspace" ]; then
    WORKSPACE="$HOME/.openclaw/workspace"
elif [ -d "$HOME/.picoclaw/workspace" ]; then
    WORKSPACE="$HOME/.picoclaw/workspace"
else
    WORKSPACE="$HOME/.openclaw/workspace"
fi

PERSONAS_DIR="$WORKSPACE/MPM/persona"
DB_FILE="$PERSONAS_DIR/personas.db"
SCRIPT_DIR="$WORKSPACE/skills/mpm/scripts"

# Validate identifier
validate_id() {
    local id="$1"
    if [[ ! "$id" =~ ^[a-zA-Z0-9_-]+$ ]]; then
        echo "❌ Invalid persona name: '$id' - only alphanumeric, underscore, and hyphen allowed"
        return 1
    fi
    return 0
}

# Escape SQL string
escape_sql() {
    printf '%s' "$1" | sed "s/'/''/g"
}

# Ensure DB exists
if [ ! -f "$DB_FILE" ] && [ -f "$SCRIPT_DIR/persona-compile.sh" ]; then
    "$SCRIPT_DIR/persona-compile.sh" sync quiet >/dev/null 2>&1
fi

case "${1:-list}" in
    list|ls)
        echo -e "\033[1;36m🎭 FlowByte Identity Matrix\033[0m"
        echo "═══════════════════════════════════════════════════════════════"
        
        if [ -f "$DB_FILE" ]; then
            sqlite3 -column -header "$DB_FILE" "SELECT sym_id as Opcode, identity_emoji as '#', identity_name as Name, identity_vibe as Vibe FROM personas;" 2>/dev/null || echo "No personas loaded."
            local count
            count=$(sqlite3 "$DB_FILE" "SELECT COUNT(*) FROM personas;" 2>/dev/null)
            echo "───────────────────────────────"
            echo "Total: ${count:-0} personas available."
        else
            echo "Database not initialized. Run: mpm persona compile"
        fi
        ;;

    activate|load|~p)
        if [ -z "$2" ]; then 
            echo "Usage: mpm persona activate <name>"
            exit 1
        fi
        
        local PERSONA_NAME="$2"
        validate_id "$PERSONA_NAME" || exit 1
        
        if [ ! -f "$DB_FILE" ]; then
            echo "❌ Database not found. Run: mpm persona compile"
            exit 1
        fi

        # 1. Update DB State
        sqlite3 "$DB_FILE" "UPDATE personas SET active = 0;"
        sqlite3 "$DB_FILE" "UPDATE personas SET active = 1, loaded_at = CURRENT_TIMESTAMP WHERE name = '$PERSONA_NAME';"
        
        # 2. Update active.json (preserve modes)
        local active_json="$WORKSPACE/MPM/active.json"
        local MODES="[]"
        if [ -f "$active_json" ]; then
            MODES=$(grep -o '"modes":\[[^]]*\]' "$active_json" 2>/dev/null | cut -d':' -f2-)
            [ -z "$MODES" ] && MODES="[]"
        fi
        
        mkdir -p "$WORKSPACE/MPM"
        printf '{"persona":"%s","modes":%s,"updated":"%s"}\n' \
            "$PERSONA_NAME" "$MODES" "$(date -Iseconds)" > "$active_json"

        # 3. Pulse the Terminal
        local SYM_ID
        SYM_ID=$(sqlite3 "$DB_FILE" "SELECT sym_id FROM personas WHERE name='$PERSONA_NAME';" 2>/dev/null)
        echo -e "⚡ \033[1;36mSymAI Pulse:\033[0m ${SYM_ID:-~p.$PERSONA_NAME}!act"
        echo ""
        echo "✅ Persona '$PERSONA_NAME' is now active."
        ;;

    create)
        if [ -z "$2" ]; then
            cat <<'EOF'
Usage: mpm persona create <name>

Creates a new persona template file.
EOF
            exit 1
        fi
        
        validate_id "$2" || exit 1
        local name="$2"
        local file="$PERSONAS_DIR/${name}.persona"
        
        if [ -f "$file" ]; then
            echo "⚠️ Persona already exists: $file"
            exit 1
        fi

        cat > "$file" <<'PERSONA_TEMPLATE'
# Persona: NAME
## Identity
- **Name:** [Override name]
- **Title:** [Override title]  
- **Vibe:** [Override personality]
- **Emoji:** [Override emoji]

## Voice & Tone
[Description]

## Behavioral Rules
- [Rule 1]
- [Rule 2]

## Context
[When to use]

## Activation
[How to activate]
PERSONA_TEMPLATE

        sed -i "s/NAME/${name^}/g" "$file"
        echo "✅ Created persona template: $file"
        echo "   Edit with: nano $file"
        ;;

    help|--help|-h)
        cat <<'EOF'
⚡ Persona Manager (Identity Management)

USAGE:
  mpm persona [list|activate|create] [name]

COMMANDS:
  list|ls              Show all available personas
  activate <name>      Activate persona (shows content)
  create <name>        Create new persona template

QUICKLINKS:
  ~p                   Open interactive persona selector
  ~p.<name>            Direct activate persona
  ~p.clear             Clear to base (default)

EXAMPLES:
  mpm persona activate oracle
  ~p.oracle
  ~p.clear
EOF
        ;;

    *)
        echo "Usage: mpm persona [list|activate|create] [name]"
        echo "Run 'mpm persona help' for full documentation."
        ;;
esac
