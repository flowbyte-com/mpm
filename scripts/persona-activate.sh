#!/bin/bash

# Persona Activate - Activate a persona (show content for adoption)
# Usage: ./persona-activate.sh <persona-name>

WORKSPACE="$HOME/.openclaw/workspace"
PERSONAS_DIR="$WORKSPACE/MPM/persona"
SCRIPT_DIR="$(dirname "$0")"

# Support SymAI-style opcodes (~p.name)
case "$1" in
    ~p.clear|~p.none|~p.reset)
        # Clear persona - revert to base agent
        sqlite3 "$WORKSPACE/MPM/persona/personas.db" "UPDATE personas SET active=0;" 2>/dev/null
        echo "🎭 Persona cleared"
        echo "≈ FLOWBYTE ≈ ~p. (base)"
        exit 0
        ;;
    ~p.o|~p.oracle)
        NAME="oracle"
        ;;
    ~p.d|~p.default)
        NAME="default"
        ;;
    ~p.h|~p.harley)
        NAME="harley"
        ;;
    ~p.c|~p.corporate)
        NAME="corporate"
        ;;
    ~p.p|~p.pirate)
        NAME="pirate"
        ;;
    ~p.b|~p.bane)
        NAME="bane"
        ;;
    ~p.r|~p.dredd)
        NAME="dredd"
        ;;
    ~p.*)
        # Extract name after ~p.
        NAME="${1#*~p.}"
        ;;
    *)
        NAME="$1"
        ;;
esac

if [ -z "$NAME" ]; then
    echo "Usage: $0 <persona-name>"
    echo "       $0 ~p.<shortcut>"
    echo ""
    echo "SymAI shortcuts:"
    echo "  ~p.o     → oracle"
    echo "  ~p.d     → default"
    echo "  ~p.h     → harley"
    echo "  ~p.c     → corporate"
    echo "  ~p.p     → pirate"
    echo ""
    echo "Available personas:"
    "$SCRIPT_DIR/persona-ls.sh" 2>/dev/null | grep -E '^[✅❌]' | sed 's/^[✅❓] //' | awk '{print "  " $1}' | head -10
    exit 1
fi

FILE="$PERSONAS_DIR/${NAME}.persona"

if [ ! -f "$FILE" ]; then
    echo "❌ Persona not found: $NAME"
    exit 1
fi

# Let the robust Manager handle the DB, active.json, and the Terminal Pulse!
"$SCRIPT_DIR/persona-manager.sh" activate "$NAME"

# Show Vibe Check (first 3 lines = core directive)
echo "🎭 Current Frequency: ~p.$NAME"
echo "---------------------------"
head -n 3 "$FILE" | sed 's/^/ /'
echo ""
echo "📄 Full Content:"
echo ""
cat "$FILE"
echo ""
echo "==========================="
echo "✅ Persona loaded!"
echo ""
echo "Adopt the voice & tone above for this session."
echo ""
echo "≈ FLOWBYTE ≈ ~p.$NAME"
echo ""
echo "SymAI shortcuts:"
echo "  ~p.o     → oracle"
echo "  ~p.d     → default"
echo "  ~p.h     → harley"
echo "  ~p.c     → corporate"
echo "  ~p.p     → pirate"
echo "  Return:  ~p.d"
