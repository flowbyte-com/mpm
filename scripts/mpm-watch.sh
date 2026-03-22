#!/bin/bash
# ⚡ MPM Watcher - Live-Sync Persona/Mode Changes

PERSONAS_DIR="$HOME/.openclaw/workspace/MPM/persona"
MODES_DIR="$HOME/.openclaw/workspace/MPM/mode"
SCRIPT_DIR="$HOME/.openclaw/workspace/skills/mpm/scripts"

echo "📡 MPM Watcher Active. Monitoring for Silicon-Native updates..."

# Watch both directories for file closures (saves)
inotifywait -m -r "$PERSONAS_DIR" "$MODES_DIR" -e close_write | while read path action file; do
 if [[ "$file" =~ \.persona$ ]]; then
 name=$(basename "$file" .persona)
"$SCRIPT_DIR/persona-compile.sh" compile "$name"
 notify-send -t 1000 "⚡ SymAI Pulse" "~p.$name!sync [OK]"
 echo "⚡ SymAI Pulse: ~p.$name!sync [Live Update Applied]"
 
 elif [[ "$file" =~ \.mode$ ]]; then
 name=$(basename "$file" .mode)
"$SCRIPT_DIR/mode-compile.sh" compile "$name"
 notify-send -t 1000 "🛠️ SymAI Pulse" "~m.$name!sync [OK]"
 echo "⚡ SymAI Pulse: ~m.$name!sync [Behavior Updated]"
 fi
done
