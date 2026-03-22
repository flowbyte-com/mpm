#!/bin/bash

# Memory Medic - Automated Cleanup Script
# Usage: ./medic.sh [--consolidate] [--archive] [--cleanup] [--dry-run]

WORKSPACE="$HOME/.openclaw/workspace"
MPM_MEMORY_DIR="$WORKSPACE/MPM/memory"
MEMORY_DIR="$MPM_MEMORY_DIR/sessions"
ARCHIVE_DIR="$MPM_MEMORY_DIR/archive"

# Ensure MPM memory directory exists
mkdir -p "$MPM_MEMORY_DIR"
DRY_RUN=false
CONSOLIDATE=false
ARCHIVE=false
CLEANUP=false

# Parse arguments (supports SymAI-style ~k.dry)
while [[ $# -gt 0 ]]; do
    case "$1" in
        --dry-run|~k.dry) DRY_RUN=true; shift ;;
        --consolidate|~k.con) CONSOLIDATE=true; shift ;;
        --archive|~k.cl) ARCHIVE=true; shift ;;
        --cleanup|~k.cle) CLEANUP=true; shift ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

# Calculate projected savings for dry-run
if [ "$DRY_RUN" = true ]; then
    echo "🔮 DRY RUN MODE - Projecting Token Savings..."
    echo "=============================================="
    
    # Count files that would be archived
    ARCHIVE_COUNT=$(find "$MEMORY_DIR" -maxdepth 1 -name "*.md" -type f -mtime +3 2>/dev/null | wc -l)
    ARCHIVE_SIZE=$(find "$MEMORY_DIR" -maxdepth 1 -name "*.md" -type f -mtime +3 2>/dev/null -exec du -ch {} + 2>/dev/null | tail -1 | cut -f1)
    
    # Extract numeric size
    ARCHIVE_NUM=$(echo "$ARCHIVE_SIZE" | sed 's/[^0-9]//g')
    if [ -n "$ARCHIVE_NUM" ] && [ "$ARCHIVE_NUM" -gt 0 ] 2>/dev/null; then
        EST_TOKENS=$((ARCHIVE_NUM * 250))
    else
        EST_TOKENS=0
    fi
    
    echo ""
    echo "📊 Projected Impact:"
    echo "   Files to archive: $ARCHIVE_COUNT"
    echo "   Size to compress: $ARCHIVE_SIZE"
    echo "   Est. token savings: ~$EST_TOKENS tokens"
    echo "   With SymAI encoding: ~$((EST_TOKENS / 2)) tokens (50% reduction)"
    echo ""
    echo "💡 Run without --dry-run to execute"
    echo ""
    exit 0
fi

echo "🏥 Memory Medic - Starting Diagnosis..."
echo "========================================"

# Check memory bloat (SymAI-style output)
echo -e "\n📊 Memory:"
if [ -d "$MEMORY_DIR" ]; then
    MEM_SIZE=$(du -sh "$MEMORY_DIR" | cut -f1)
    echo -e "   ~k?$MEM_SIZE (Scan result)"
    ls -lh "$MEMORY_DIR"/*.md 2>/dev/null | sort -k5 -hr | head -10 | while read line; do
        SIZE=$(echo "$line" | awk '{print $5}')
        FILE=$(echo "$line" | awk '{print $9}' | xargs basename)
        echo "   📄 $FILE ($SIZE)"
    done
else
    echo -e "   ~k!err (Not Found)"
fi

# Check workspace size
echo -e "\n📁 Workspace:"
du -sh "$WORKSPACE"/* 2>/dev/null | sort -hr | head -15 | while read line; do
    SIZE=$(echo "$line" | cut -f1)
    DIR=$(echo "$line" | cut -f2 | xargs basename)
    echo "   $DIR: $SIZE"
done

# Verify core files (SymAI-style)
echo -e "\n🔍 Core Identity:"
AGENT_FILES=("SOUL.md" "AGENTS.md" "USER.md" "TOOLS.md" "MEMORY.md" "IDENTITY.md" "HEARTBEAT.md")
MISSING_FILES=()
CORE_OK=true
for file in "${AGENT_FILES[@]}"; do
    if [ -f "$WORKSPACE/$file" ]; then
        SIZE=$(stat -f%z "$WORKSPACE/$file" 2>/dev/null || stat -c%s "$WORKSPACE/$file" 2>/dev/null)
        echo "   ✅ $file ($SIZE bytes)"
    else
        echo -e "   🔍 $file: ~!miss (Missing)"
        MISSING_FILES+=("$file")
        CORE_OK=false
    fi
done

if [ "$CORE_OK" = true ]; then
    echo -e "   ~ok (All systems nominal)"
fi

# Run consolidation if requested
if [ "$CONSOLIDATE" = true ]; then
    echo -e "\n🔄 Consolidating memory files..."
    if [ "$DRY_RUN" = true ]; then
        echo "[DRY RUN] Would consolidate memory files into MEMORY.md"
    else
        # Create archive directory
        mkdir -p "$ARCHIVE_DIR"
        
        # Move old files to archive (keep last 2 days)
        find "$MEMORY_DIR" -name "*.md" -type f -mtime +2 -exec mv {} "$ARCHIVE_DIR/" \; 2>/dev/null
        echo "✅ Archived files older than 2 days"
    fi
fi

# Run archive if requested
if [ "$ARCHIVE" = true ]; then
    echo -e "\n📦 Archiving old memory files..."
    if [ "$DRY_RUN" = true ]; then
        echo "[DRY RUN] Would archive all but recent memory files"
    else
        mkdir -p "$ARCHIVE_DIR"
        # Keep only files from last 3 days
        find "$MEMORY_DIR" -maxdepth 1 -name "*.md" -type f -mtime +3 -exec mv {} "$ARCHIVE_DIR/" \; 2>/dev/null
        echo "✅ Archived files older than 3 days"
    fi
fi

# Run cleanup if requested
if [ "$CLEANUP" = true ]; then
    echo -e "\n🧹 Cleaning up workspace..."
    if [ "$DRY_RUN" = true ]; then
        echo "[DRY RUN] Would remove temp files and old backups"
    else
        # Remove temp files
        rm -f "$WORKSPACE"/*.tmp
        rm -f "$WORKSPACE"/*.log
        rm -f "$WORKSPACE"/*OLD*
        rm -rf "$WORKSPACE"/workspace-old 2>/dev/null
        echo "✅ Removed temp files and old backups"
    fi
fi

# Recovery suggestions
if [ ${#MISSING_FILES[@]} -gt 0 ]; then
    echo -e "\n⚠️  Recovery needed for missing files:"
    echo "Run: cd $WORKSPACE && git checkout HEAD -- ${MISSING_FILES[*]}"
fi

echo -e "\n💊 Diagnosis complete!"
echo "========================================"
echo ""
echo "Quick fixes:"
echo "  ./medic.sh --archive    # Archive old memory files"
echo "  ./medic.sh --cleanup    # Remove temp files and old backups"
echo "  ./medic.sh --consolidate # Archive files older than 2 days"
echo "  ./medic.sh --dry-run    # Preview changes"
echo ""
echo "Purification:"
echo "  ./purify.sh <file>      # Strip chat bloat from a memory file"
echo "  ./purify.sh <file> --inplace  # Purify in-place (creates .bak)"
