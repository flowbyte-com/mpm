#!/bin/bash

# Persona Watcher - Auto-compile on file change
# Usage: ./persona-watch.sh [--start|--stop|--status]
# 
# Uses inotifywait to watch for file changes and auto-compile

# ⚡ FlowByte Persona Watcher Core
WORKSPACE="$HOME/.openclaw/workspace"
PERSONAS_DIR="$WORKSPACE/MPM/persona"
SCRIPT_DIR="$WORKSPACE/skills/mpm/scripts"

PID_FILE="$SCRIPT_DIR/.persona-watch.pid"
MODE_FILE="$SCRIPT_DIR/.watcher-mode"

# ... inside the inotify loop ...
"$SCRIPT_DIR/persona-compile.sh" compile "$name"

# Default to verbose
get_mode() {
    if [ -f "$MODE_FILE" ]; then
        cat "$MODE_FILE"
    else
        echo "v"
    fi
}

set_mode() {
    echo "$1" > "$MODE_FILE"
    echo "✅ Watcher mode: $1"
    echo "   v = verbose (notifications on)"
    echo "   s = silent (notifications off)"
}

start_watcher() {
    # Check if already running
    if [ -f "$PID_FILE" ]; then
        OLD_PID=$(cat "$PID_FILE")
        if kill -0 "$OLD_PID" 2>/dev/null; then
            echo "⚠️  Watcher already running (PID: $OLD_PID)"
            return 1
        fi
        rm -f "$PID_FILE"
    fi
    
    # Check for inotifywait
    if ! command -v inotifywait &>/dev/null; then
        echo "❌ inotifywait not found. Install: sudo apt install inotify-tools"
        return 1
    fi
    
    # Start watcher in background
    (
        inotifywait -m -e close_write -e moved_to \
            --format '%w%f' "$PERSONAS_DIR" 2>/dev/null | \
        while read FILE; do
            if [[ "$FILE" == *.persona ]]; then
                NAME=$(basename "$FILE" .persona)
                # Get bytecode alias from DB
                ALIAS=$(sqlite3 "$WORKSPACE/MPM/persona/personas.db" \
                    "SELECT bytecode_alias FROM personas WHERE name='$NAME';" 2>/dev/null)
                ALIAS=${ALIAS:-"~p.$NAME"}
                
                # Check mode (v=verbose, s=silent)
                MODE=$(get_mode)
                
                # Terminal output
                echo -e "⚡ \033[1;36mSymAI Pulse:\033[0m ${ALIAS}!sync($NAME) [Live Update Applied]"
                
                # Desktop notification (only in verbose mode)
                if [ "$MODE" = "v" ] && command -v notify-send &>/dev/null; then
                    notify-send -t 1000 "⚡ SymAI Sync" "${ALIAS}!sync [OK]"
                fi
                
                "$SCRIPT_DIR/persona-compile.sh" compile "$NAME" 2>/dev/null | sed 's/^/   /'
            fi
        done
    ) &
    
    echo $! > "$PID_FILE"
    echo "✅ Persona Watcher started (PID: $(cat $PID_FILE))"
    echo "   Watching: $PERSONAS_DIR"
    echo "   Auto-compile on Ctrl+S..."
}

stop_watcher() {
    if [ -f "$PID_FILE" ]; then
        PID=$(cat "$PID_FILE")
        if kill -0 "$PID" 2>/dev/null; then
            kill "$PID" 2>/dev/null
            echo "✅ Watcher stopped"
        fi
        rm -f "$PID_FILE"
    else
        echo "⚠️  No watcher running"
    fi
}

status_watcher() {
    if [ -f "$PID_FILE" ]; then
        PID=$(cat "$PID_FILE")
        if kill -0 "$PID" 2>/dev/null; then
            echo "✅ Watcher active (PID: $PID)"
            echo "   Watching: $PERSONAS_DIR"
        else
            echo "⚠️  Stale PID file (process dead)"
            rm -f "$PID_FILE"
        fi
    else
        echo "⚪ Watcher not running"
    fi
}

case "${1:-status}" in
    --start|start)
        start_watcher
        ;;
    --stop|stop)
        stop_watcher
        ;;
    --status|status)
        status_watcher
        ;;
    --mode|-m)
        shift
        if [ -z "${1:-}" ]; then
            echo "Current mode: $(get_mode)"
            echo "  v = verbose (notifications on)"
            echo "  s = silent (notifications off)"
        else
            set_mode "$1"
        fi
        ;;
    --silent|silent|~w.s)
        set_mode "s"
        ;;
    --verbose|verbose|~w.v)
        set_mode "v"
        ;;
    *)
        echo "Usage: $0 [start|stop|status|mode]"
        echo ""
        echo "  start    - Begin watching for changes"
        echo "  stop     - Stop watching"
        echo "  status   - Check if watching"
        echo "  mode v   - Verbose (notifications on)"
        echo "  mode s   - Silent (notifications off)"
        echo "  ~w.v     - SymAI: verbose"
        echo "  ~w.s     - SymAI: silent"
        ;;
esac
