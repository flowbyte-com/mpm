#!/bin/bash
#
# MPM Setup v3.4 - Initialize and test the new memory system
# Usage: ./setup-v3.4.sh [--init|--test|--cron]

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORKSPACE="${MPM_WORKSPACE:-$HOME/.picoclaw/workspace}"
DB="$WORKSPACE/mpm/memory/memory.db"

echo "╔════════════════════════════════════════════════════════════════╗"
echo "║     MPM Memory System v3.4 - Setup & Initialization             ║"
echo "╚════════════════════════════════════════════════════════════════╝"
echo ""

case "${1:---init}" in
    --init)
        echo "Step 1: Initializing database..."
        cd "$SCRIPT_DIR"
        ./auto-session-worker.sh --process-all
        echo ""
        
        echo "Step 2: Running status check..."
        ./auto-session-worker.sh --status
        ;;
    
    --test)
        echo "Running query tests..."
        echo ""
        
        echo "Test 1: Stats"
        ./query-memories.sh stats
        echo ""
        
        echo "Test 2: Recent activity"
        ./query-memories.sh recent --hours=72
        echo ""
        
        echo "Test 3: Search 'MPM'"
        ./query-memories.sh search "MPM" --limit=5
        echo ""
        
        echo "Test 4: Active threads"
        ./query-memories.sh thread
        echo ""
        
        echo "Test 5: Decisions"
        ./query-memories.sh decisions --limit=5
        ;;
    
    --cron)
        echo "Setting up cron job..."
        
        # Add to crontab
        (crontab -l 2>/dev/null | grep -v "mpm/auto-session-worker"; echo "# MPM Auto Session Worker - every 5 minutes") | crontab -
        (crontab -l 2>/dev/null; echo "*/5 * * * * cd $SCRIPT_DIR && ./auto-session-worker.sh --process-all >> $WORKSPACE/mpm/memory/sync.log 2>&1") | crontab -
        
        echo "✓ Cron job added (runs every 5 minutes)"
        echo "  Log: $WORKSPACE/mpm/memory/sync.log"
        ;;
    
    --watch)
        echo "Starting watch mode..."
        ./auto-session-worker.sh --watch
        ;;
    
    *)
        echo "Usage: $0 [--init|--test|--cron|--watch]"
        echo ""
        echo "  --init   Initialize database and process existing sessions (default)"
        echo "  --test   Run query tests"
        echo "  --cron   Add cron job for automatic syncing"
        echo "  --watch  Start watch mode (foreground daemon)"
        ;;
esac
chmod +x "$SCRIPT_DIR"/*.sh "$SCRIPT_DIR"/analyze-session-v3.4.py