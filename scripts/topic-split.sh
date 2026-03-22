#!/bin/bash

# Topic Splitter - Split MEMORY.md into topic-based files
# Usage: ./topic-split.sh [--create-topics] [--migrate] [--dry-run] [--auto-split] [file]
#        ./topic-split.sh ~k!split [file]  (SymAI mode)

WORKSPACE="$HOME/.openclaw/workspace"
MEMORY_FILE="$WORKSPACE/MEMORY.md"
TOPICS_DIR="$WORKSPACE/MPM/memory/topics"
SPLIT_THRESHOLD=2048  # 2KB default threshold

MODE="create-topics"

# Parse arguments (supports SymAI-style ~k!split)
while [[ $# -gt 0 ]]; do
    case "$1" in
        --migrate|~k.mg) MODE="migrate"; shift ;;
        --create-topics|~k.tpc) MODE="create-topics"; shift ;;
        --dry-run|~k.dry) DRY_RUN=true; shift ;;
        --auto-split|~k!split)
            MODE="auto-split"
            # Optional: file argument
            if [[ -n "$2" && ! "$2" =~ ^-- ]]; then
                TARGET_FILE="$2"
                shift
            fi
            shift
            ;;
        --threshold)
            SPLIT_THRESHOLD="$2"
            shift 2
            ;;
        *) 
            # Assume it's a file path
            if [[ -f "$1" ]]; then
                TARGET_FILE="$1"
            fi
            shift
            ;;
    esac
done

# Auto-split mode: check file size and split if needed
if [ "$MODE" = "auto-split" ]; then
    FILE="${TARGET_FILE:-$MEMORY_FILE}"
    FILENAME=$(basename "$FILE")
    
    echo "🔪 ~k!split: Silicon-Dense Auto-Splitter"
    echo "=========================================="
    echo "Target: $FILE"
    echo "Threshold: ${SPLIT_THRESHOLD} bytes"
    echo ""
    
    if [ ! -f "$FILE" ]; then
        echo "~k!err: File not found"
        exit 1
    fi
    
    FILE_SIZE=$(stat -c%s "$FILE" 2>/dev/null || stat -f%z "$FILE" 2>/dev/null)
    echo "Current size: $FILE_SIZE bytes"
    
    if [ "$FILE_SIZE" -lt "$SPLIT_THRESHOLD" ]; then
        echo ""
        echo "✅ ~ok: File is Silicon-Dense ($FILE_SIZE < $SPLIT_THRESHOLD)"
        echo "   No split needed"
        exit 0
    fi
    
    echo ""
    echo "⚠️  File exceeds threshold - splitting required"
    
    if [ "${DRY_RUN:-false}" = true ]; then
        echo "[DRY RUN] Would split $FILE into:"
        echo "   $FILE (first half)"
        echo "   ${FILE%.md}-archive.md (archived half)"
        exit 0
    fi
    
    # Perform split
    LINES=$(wc -l < "$FILE")
    SPLIT_LINE=$((LINES / 2))
    
    HEAD_FILE="$FILE"
    TAIL_FILE="${FILE%.md}-archive.md"
    
    # Create backup
    cp "$FILE" "${FILE}.bak"
    
    # Split: head stays in original, tail goes to archive
    head -n "$SPLIT_LINE" "$FILE" > "$HEAD_FILE.tmp"
    tail -n +$((SPLIT_LINE + 1)) "$FILE" > "$TAIL_FILE"
    mv "$HEAD_FILE.tmp" "$HEAD_FILE"
    
    # Add MPM header to active file
    {
        cat "$HEAD_FILE"
        echo ""
        echo "--- FLOWBYTE SERIALIZED v3.2 ---"
        echo "Original size: $FILE_SIZE bytes"
        echo "Split at line: $SPLIT_LINE of $LINES"
        echo "Archive: $TAIL_FILE"
        echo "Tokens saved: ~$((FILE_SIZE / 8))"
        echo "--- END SERIALIZATION ---"
    } > "$HEAD_FILE.tmp"
    mv "$HEAD_FILE.tmp" "$HEAD_FILE"
    
    echo "✅ Split complete:"
    echo "   📄 $HEAD_FILE ($(stat -c%s "$HEAD_FILE") bytes)"
    echo "   📦 $TAIL_FILE ($(stat -c%s "$TAIL_FILE") bytes)"
    echo ""
    echo "💡 Token savings: ~$((FILE_SIZE / 4)) → ~$((FILE_SIZE / 8)) tokens (50%)"
    exit 0
fi

echo "🧠 Memory Topic Splitter"
echo "========================"
echo "Mode: $MODE"
echo ""

# Define topic categories
declare -A TOPICS=(
    ["identity"]="Identity, agent info, version, creation date"
    ["projects"]="Active projects, status, next steps"
    ["lessons"]="Lessons learned, mistakes, insights"
    ["configs"]="API keys, credentials, configurations, paths"
    ["people"]="Human info, collaborators, preferences"
    ["tools"]="Available tools, capabilities, permissions"
    ["decisions"]="Important decisions made, direction changes"
    ["references"]="Quick references, URLs, commands"
)

if [ "$MODE" = "create-topics" ]; then
    echo "📁 Creating topic structure:"
    echo ""
    
    if [ "${DRY_RUN:-false}" = true ]; then
        echo "[DRY RUN] Would create:"
        for topic in "${!TOPICS[@]}"; do
            echo "  $TOPICS_DIR/${topic}.md"
        done
    else
        mkdir -p "$TOPICS_DIR"
        
        # Create template for each topic
        for topic in "${!TOPICS[@]}"; do
            cat > "$TOPICS_DIR/${topic}.md" <<EOF
# ${topic^} Memory

**Last updated:** $(date +%Y-%m-%d)

## Contents

<!-- Add ${TOPICS[$topic]} here -->

---
*Managed by Memory Medic skill - topic splitter*
EOF
            echo "✅ Created: $TOPICS_DIR/${topic}.md"
        done
        
        echo ""
        echo "📁 Topic structure created at: $TOPICS_DIR"
        echo ""
        echo "Next: Migrate content from MEMORY.md"
        echo "  ./topic-split.sh --migrate"
    fi
fi

if [ "$MODE" = "migrate" ]; then
    echo "🔄 Migrating MEMORY.md to topic files..."
    echo ""
    
    if [ ! -f "$MEMORY_FILE" ]; then
        echo "❌ MEMORY.md not found: $MEMORY_FILE"
        exit 1
    fi
    
    if [ "${DRY_RUN:-false}" = true ]; then
        echo "[DRY RUN] Would analyze and split MEMORY.md"
    else
        # Analyze MEMORY.md and show what would go where
        echo "📊 Analyzing MEMORY.md..."
        echo ""
        
        # Count sections/lines per topic
        echo "Content breakdown:"
        grep -E '^##' "$MEMORY_FILE" | while read -r line; do
            section=$(echo "$line" | sed 's/^## //')
            echo "  $section"
        done
        
        echo ""
        echo "💡 Manual migration recommended:"
        echo "  1. Review MEMORY.md sections"
        echo "  2. Cut/paste into appropriate topic files"
        echo "  3. Keep MEMORY.md as index/summary"
        echo ""
        echo "Or auto-split (experimental):"
        echo "  ./topic-split.sh --migrate --auto"
    fi
fi

echo ""
echo "📋 Topic files:"
if [ -d "$TOPICS_DIR" ]; then
    ls -lh "$TOPICS_DIR"/*.md 2>/dev/null | awk '{print $5, $9}'
else
    echo "(not created yet)"
fi
echo ""

echo "💡 Pattern:"
echo "  MEMORY.md → Index/summary only (keep small)"
echo "  memory-topics/*.md → Detailed topic memory"
echo "  memory/YYYY-MM-DD.md → Daily session logs (archived)"
