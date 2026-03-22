#!/bin/bash

# Memory Consolidator - Review and merge memory files into MEMORY.md
# Usage: ./consolidate.sh [--review] [--merge] [--archive]

WORKSPACE="$HOME/.openclaw/workspace"
MEMORY_DIR="$WORKSPACE/MPM/memory/sessions"
MPM_MEMORY_DIR="$WORKSPACE/MPM/memory"
MEMORY_FILE="$WORKSPACE/MEMORY.md"
ARCHIVE_DIR="$MPM_MEMORY_DIR/archive"       # <-- Keep archives inside MPM

# Function to extract summary based on SymAI availability
get_summary() {
    local file=$1
    local TAGGER="$WORKSPACE/symai/tagger.py"
    local EXTRACT="$WORKSPACE/skills/mpm/scripts/extract-facts.sh"

    if [ -f "$TAGGER" ] && [ -f "$EXTRACT" ]; then
        # --- SymAI HIGH-PERFORMANCE PATH ---
        # Uses your extract-facts.sh to purify, then SymAI to compress
        bash "$EXTRACT" "$file" | python3 "$TAGGER" | head -5
    elif [ -f "$EXTRACT" ]; then
        # --- MPM PURIFIED PATH (No SymAI) ---
        # Still uses your extract-facts.sh for a clean, non-bytecode view
        echo -e "\033[0;33m[Fact Extraction Mode]\033[0m"
        bash "$EXTRACT" "$file" | head -8 | sed 's/^/  /'
    else
        # --- LEGACY FALLBACK ---
        # Standard grep if extract-facts.sh is missing
        grep -E '^[-*#]|^[0-9]+\.' "$file" | head -8 | sed 's/^/  /'
    fi
}

# Ensure MPM memory directory exists
mkdir -p "$MPM_MEMORY_DIR" "$ARCHIVE_DIR"

MODE="review"

while [[ $# -gt 0 ]]; do
    case $1 in
        --merge) MODE="merge"; shift ;;
        --archive) MODE="archive"; shift ;;
        --review) MODE="review"; shift ;;
        *) shift ;;
    esac
done

echo "🧠 Memory Consolidator"
echo "======================"
echo "Mode: $MODE"
echo ""

# List memory files
echo "📁 Memory files:"
ls -lh "$MEMORY_DIR"/*.md 2>/dev/null | awk '{print $5, $9}' | sort -hr
echo ""

# Show file count and total size
FILE_COUNT=$(ls "$MEMORY_DIR"/*.md 2>/dev/null | wc -l)
TOTAL_SIZE=$(du -sh "$MEMORY_DIR" | cut -f1)
echo "Total: $FILE_COUNT files, $TOTAL_SIZE"
echo ""

if [ "$MODE" = "review" ]; then
    echo "📋 MPM Bytecode Preview (Recent Sessions):"
    echo "==============================================="
    
    # SymAI Tagger path
    TAGGER="$WORKSPACE/symai/tagger.py"
    
    for file in $(ls -t "$MEMORY_DIR"/*.md 2>/dev/null | head -5); do
        echo -e "\n📄 \033[1m$(basename "$file")\033[0m:"
        
        # Use the modular summary function
        get_summary "$file"
        echo " [...]"
    done
    
    echo -e "\n💡 \033[1mMPM Action Required:\033[0m"
    echo " ~k!mg -> Merge these facts to MEMORY.md"
    echo " ~k!cl -> Archive processed sessions"
    echo ""
fi

if [ "$MODE" = "archive" ]; then
    echo "📦 Compiling and Archiving old memory files..."
    mkdir -p "$ARCHIVE_DIR"
    TAGGER="$WORKSPACE/symai/tagger.py"
    EXTRACT="$WORKSPACE/skills/mpm/scripts/extract-facts.sh"
    
    # Find files older than 3 days
    for file in $(find "$MEMORY_DIR" -maxdepth 1 -name "*.md" -type f -mtime +3); do
        echo -n " ⚡ Compiling $(basename "$file")..."
        
# 1. Capture the summary (whether SymAI or Standard)
        SUMMARY=$(get_summary "$file")

        # 2. Prepend the 'Silicon-Native' header so the file carries its DNA into the archive
        # We use a temp file to safely overwrite the original
        TEMP_FILE=$(mktemp)
        echo -e "--- FLOWBYTE SERIALIZED v3.2 ---\n$SUMMARY\n-------------------------------\n" > "$TEMP_FILE"
        cat "$file" >> "$TEMP_FILE"
        mv "$TEMP_FILE" "$file"

        echo -e " \033[0;32mDONE\033[0m"
        
        # 3. Move to archive
        mv "$file" "$ARCHIVE_DIR/"
    done
    
    echo "✅ Archive complete. All files now have 'Silicon-Native' headers."
    echo ""
fi

if [ "$MODE" = "merge" ]; then
    echo "⚠️  Merge mode not yet implemented"
    echo "Manual process recommended:"
    echo "  1. Read each memory file"
    echo "  2. Extract key facts"
    echo "  3. Update MEMORY.md manually"
    echo "  4. Run: ./consolidate.sh --archive"
    echo ""
fi
