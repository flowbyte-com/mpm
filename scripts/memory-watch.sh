#!/bin/bash
# 🧠 MPM Memory Watcher - Auto-sync memory files

WORKSPACE="$HOME/.openclaw/workspace"
MEMORY_DIR="$WORKSPACE/memory"
SCRIPT_DIR="$WORKSPACE/skills/mpm/scripts"

echo "🧠 MPM Memory Watcher Active. Monitoring for memory updates..."

# Watch memory directory for file changes
inotifywait -m -r "$MEMORY_DIR" -e close_write,moved_to,create | while read path action file; do
    if [[ "$file" =~ \.md$ ]]; then
        # Run full memory sync (extract facts + update DB)
        "$SCRIPT_DIR/extract-facts.sh" "$path$file" 2>/dev/null
        "$SCRIPT_DIR/../MPM/memory/sync-sessions.sh" >/dev/null 2>&1
        echo "🧠 Memory Pulse: $file [Synced to DB]"
    fi
done