#!/bin/bash
#
# SymAI Bytecode Generator for MPM
# Generates token-dense SymAI codes for personas and modes
# Falls back to simple codes if SymAI doesn't exist
#
# Usage: symai-bytecode.sh [persona|mode|list|check] [args...]
#

# Detect workspace path using same logic as MPM
if [[ -n "$MPM_WORKSPACE" ]]; then
    # Already set by calling script
    WORKSPACE="$MPM_WORKSPACE"
    CLAW_NAME="${MPM_CLAW_NAME:-picoclaw}"
elif [ -d "$HOME/.picoclaw/workspace" ]; then
    WORKSPACE="$HOME/.picoclaw/workspace"
    CLAW_NAME="picoclaw"
elif [ -d "$HOME/.openclaw/workspace" ]; then
    WORKSPACE="$HOME/.openclaw/workspace"
    CLAW_NAME="openclaw"
else
    WORKSPACE="$HOME/.picoclaw/workspace"
    CLAW_NAME="picoclaw"
fi

SYMAI_DIR="$WORKSPACE/symai"
SYMAI_MAPPING="$SYMAI_DIR/mapping.json"

# Check if SymAI exists and is usable
check_symai() {
    if [ -f "$SYMAI_MAPPING" ] && [ -r "$SYMAI_MAPPING" ]; then
        return 0
    fi
    return 1
}

# Generate persona bytecode
# Usage: generate_persona_bytecode <name> [short_name]
generate_persona_bytecode() {
    local name="$1"
    local short_name="${2:-${name:0:1}}"
    
    # If SymAI exists, try to get mapping
    if check_symai && command -v python3 >/dev/null 2>&1; then
        local code
        code=$(python3 <<PYEOF
import json
import sys
try:
    with open('$SYMAI_MAPPING') as f:
        mapping = json.load(f)
    # Check for direct activation mapping
    lookup = 'activate $name'
    if lookup in mapping:
        print(mapping[lookup])
    else:
        # Default: first letter shortcode
        print('~p.${short_name}!act')
except Exception:
    # Fallback on any error
    print('~p.${short_name}!act')
PYEOF
)
        if [ -n "$code" ]; then
            echo "$code"
            return 0
        fi
    fi
    
    # Fallback: simple SymAI-style code using first letter
    echo "~p.${short_name}!act"
    return 0
}

# Generate mode bytecode  
# Usage: generate_mode_bytecode <name> [short_name]
generate_mode_bytecode() {
    local name="$1"
    # Common abbreviations for well-known modes
    case "$name" in
        programming) short_name="pr" ;;
        debugging)   short_name="db" ;;
        design)      short_name="ds" ;;
        writing)     short_name="wr" ;;
        research)    short_name="rs" ;;
        creative)    short_name="cr" ;;
        analytical)  short_name="an" ;;
        *)           short_name="${name:0:2}" ;;
    esac
    
    # If SymAI exists, try to find a mapping
    if check_symai && command -v python3 >/dev/null 2>&1; then
        local code
        code=$(python3 <<PYEOF
import json
import sys
try:
    with open('$SYMAI_MAPPING') as f:
        mapping = json.load(f)
    # Check for mode mappings
    lookup = '$name mode'
    if lookup in mapping:
        print(mapping[lookup])
    else:
        # Default: two-letter shortcode
        print('~m.${short_name}!act')
except Exception:
    print('~m.${short_name}!act')
PYEOF
)
        if [ -n "$code" ]; then
            echo "$code"
            return 0
        fi
    fi
    
    # Fallback
    echo "~m.${short_name}!act"
    return 0
}

# Generate list command bytecodes
generate_list_bytecodes() {
    if check_symai; then
        cat <<EOF
~p.ls     → List personas  
~m.ls     → List modes
~p.act    → Show active persona
~m.act    → Show stacked modes
EOF
    else
        cat <<EOF
~p.list   → List personas
~m.list   → List modes
~p.act    → Show active persona
~m.act    → Show stacked modes
EOF
    fi
}

# Main command handler
case "${1:-}" in
    persona|p)
        shift
        generate_persona_bytecode "$@"
        ;;
    mode|m)
        shift
        generate_mode_bytecode "$@"
        ;;
    list)
        generate_list_bytecodes
        ;;
    check)
        if check_symai; then
            echo "✅ SymAI found at $SYMAI_DIR"
            echo "   Mapping: $SYMAI_MAPPING"
            exit 0
        else
            echo "⚠️  SymAI not found - using fallback codes"
            exit 1
        fi
        ;;
    *)
        cat <<'EOF'
Usage: $(basename $0) [persona|mode|list|check] [args...]

Commands:
  persona <name> [short]  - Generate persona bytecode (~p.X!act)
  mode <name> [short]     - Generate mode bytecode (~m.XX!act)
  list                    - Show list command bytecodes
  check                   - Check SymAI availability

Examples:
  $0 persona oracle       → ~p.o!act or ~p.oracle!act
  $0 persona harley       → ~p.h!act
  $0 mode programming     → ~m.pr!act (uses abbreviation)
  $0 mode debugging       → ~m.db!act
  $0 list                 - Show all codes
EOF
        exit 1
        ;;
esac
