#!/bin/bash

# Memory Purifier - Strip chat bloat from memory files
# Usage: ./purify.sh <input-file> [--output <output-file>] [--dry-run]

INPUT_FILE=""
OUTPUT_FILE=""
DRY_RUN=false
INPLACE=false

# Parse arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        --output) OUTPUT_FILE="$2"; shift 2 ;;
        --dry-run) DRY_RUN=true; shift ;;
        --inplace) INPLACE=true; shift ;;
        *) 
            if [ -z "$INPUT_FILE" ]; then
                INPUT_FILE="$1"
            fi
            shift
            ;;
    esac
done

if [ -z "$INPUT_FILE" ]; then
    echo "Usage: ./purify.sh <file> [--output <file>] [--dry-run] [--inplace]"
    exit 1
fi

if [ ! -f "$INPUT_FILE" ]; then
    echo "❌ File not found: $INPUT_FILE"
    exit 1
fi

if [ "$INPLACE" = true ] && [ -z "$OUTPUT_FILE" ]; then
    OUTPUT_FILE="${INPUT_FILE}.tmp"
    mv "$INPUT_FILE" "${INPUT_FILE}.bak"
fi

if [ -z "$OUTPUT_FILE" ]; then
    OUTPUT_FILE="/dev/stdout"
fi

echo "🧹 Purifying: $INPUT_FILE"
if [ "$DRY_RUN" = true ]; then
    echo "[DRY RUN - No changes will be made]"
fi

# Purification pipeline
purify() {
sed -E '
        # 🛡️ PROTECTION: If line starts with ~, do nothing and stop processing it
        /^~/b;

        # 1. Remove timestamp headers
        /^\[([A-Za-z]+ )?[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}/d;

        # 2. Remove metadata & JSON blocks
        /^Sender \(untrusted metadata\):/,/^```/d;
        /^```json$/,/^```/d;
        /^# Session:/,/^## /d;
        /^\*\*Session (Key|ID|Source)/d;
        /^## Conversation Summary$/d;
        /^(assistant|user|System):/d;
        /\[\[reply_to/d;

        # 3. Remove conversation fillers & confirmations
        /^(Let me check|Here'\''s what|Want me to|Shall I|Should I|I'\''ll |I can |I will |Want to|Did you mean|Quick clarification|Great news|Ah,|Ah!)/d;
        /^(Done|Created|Fixed|Updated|Added|Removed|Deleted|Completed)![[:space:]]*$/d;

        # 4. Remove thinking/reasoning blocks
        /^<think>/,/^<\/think>/d;

        # 5. Clean tool output blocks (not user code)
        /^```(bash|shell|javascript|json|python)$/,/^```$/{
            /^```(bash|shell|javascript|json|python)$/d;
            /^```$/d;
            /exec|Tool|tool|failed|error/d;
        };

        # 6. Remove system markers & separators
        /^[[:space:]]*[🦞🤖👑✅❌🔍📊🧹📦🔄💊🏥🚀🐣💡📝📁🗑️][[:space:]]*$/d;
        /^Tool: /d;
        /^Tool call: /d;
        /^(NO_REPLY|HEARTBEAT_OK)$/d;
        /^---+$|^===+$|^___+$/d;
        /^Exec failed/d;

        # 7. Strip trailing chat-bloat emojis
        s/[[:space:]]*[🦞🤖👑✅❌🔍📊🧹📦🔄💊🏥🚀🐣💡📝📁🗑️🎯]+$//;
    ' "$1" | \
    # 8. Squash consecutive blank lines (Keep as a final pipe for logic safety)
    sed -E '/^$/N;/^\n$/d'
}

# Run purification
if [ "$DRY_RUN" = true ]; then
    echo -e "\n📊 What would be removed:"
    purify "$INPUT_FILE" | head -50
    echo -e "\n[... truncated]"
else
    if [ "$OUTPUT_FILE" = "/dev/stdout" ]; then
        purify "$INPUT_FILE"
    else
        purify "$INPUT_FILE" > "$OUTPUT_FILE"
        if [ "$INPLACE" = true ]; then
            mv "$OUTPUT_FILE" "$INPUT_FILE"
            rm -f "${INPUT_FILE}.bak"
            echo "✅ In-place purification complete: $INPUT_FILE"
        else
            echo "✅ Purified output: $OUTPUT_FILE"
        fi
    fi
fi
