#!/bin/bash
# 🧠 MPM Memory Watcher - Auto-sync memory files

MEMORY_DIR="$HOME/.openclaw/workspace/memory"
SCRIPT_DIR="$HOME/.openclaw/workspace/skills/mpm/scripts"

echo "🧠 MPM Memory Watcher Active. Monitoring for memory updates..."

# Watch memory directory for file changes
inotifywait -m -r "$MEMORY_DIR" -e close_write,moved_to,create | while read path action file; do
    if [[ "$file" =~ \.md$ ]]; then
        # Run memory sync (extract facts + update DB)
        "$SCRIPT_DIR/extract-facts.sh" "$path$file" 2>/dev/null
        echo "🧠 Memory Pulse: $file [Synced to DB]"
    fi
done