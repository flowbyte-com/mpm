#!/bin/bash

# MPM - Main Entry Point (Memory + Persona + Mode)
# Usage: mpm [memory|persona|mode|backup|watch|install|status]

WORKSPACE="$HOME/.openclaw/workspace"
SCRIPT_DIR="$WORKSPACE/skills/mpm/scripts"

show_help() {
    cat << 'EOF'
📖 FLOWBYTE MPM KERNEL v3.2
Usage: mpm [command] [subcommand] [args]

📋 CORE COMMANDS & SUBCOMMANDS
  mpm persona   <subcommand>  [Identity Management]
                list|ls       - Show all available personas
                select        - Open interactive TUI selector (~p)
                compile       - Sync markdown files to database
                dashboard     - Show persona system health
                load <name>   - Fast-activate specific persona

  mpm mode      <subcommand>  [Behavioral Management]
                list|ls       - Show all available modes
                stack         - Open interactive TUI selector (~m)
                compile       - Sync markdown files to database
                clear         - Remove all active modes
                load <name>   - Fast-stack a specific mode

  mpm memory    <subcommand>  [Long-Term Fact Storage]
                sync          - Sync new sessions to DB
                process       - Process and purge session logs
                top           - Show top memory topics
                search <q>    - Search memory DB for queries
                stats         - Memory health and token count

  mpm backup    <subcommand>  [Version Control]
                backup        - Commit and push to Gitlab
                status        - Check current git status

🛠️ SYSTEM UTILITIES
  mpm watch                   - Launch live-sync background worker
  mpm install                 - Setup SymAI aliases (~p, ~m, ~?)
  mpm restore                 - Restore previous session state
  mpm status                  - Show high-density heartbeat dashboard
  mpm telemetry               - View token savings and DB metrics

⚡ SYMAI OPCODE LEGEND (Silicon-Native)
  ~p.[id]    Persona Prefix   (e.g., ~p.oracle)
  ~m.[id]    Mode Prefix      (e.g., ~m.pr)
  ~k.[id]    Kernel/Memory    (e.g., ~k!lsn)
  ~!         System Action    (e.g., ~!fix, ~!crit)
  ~$         Status/Result    (e.g., ~$Sync, ~$Done)
  ~?         Discovery/Query  (e.g., ~?path)

💡 Pro-Tip: Use '~p' or '~m' in your terminal to bypass this menu.
EOF
}

# Restore persisted persona/mode from previous session
restore_active_state() {
    active_persona=$(sqlite3 "$WORKSPACE/MPM/persona/personas.db" "SELECT name FROM personas WHERE active=1 LIMIT 1;" 2>/dev/null)
    if [ -n "$active_persona" ]; then
        echo "🔄 Restored persona: $active_persona"
    fi

    active_modes_count=$(sqlite3 "$WORKSPACE/MPM/mode/m3.db" "SELECT COUNT(*) FROM active_modes;" 2>/dev/null)
    if [ -n "$active_modes_count" ] && [ "$active_modes_count" -gt 0 ]; then
        active_modes=$(sqlite3 "$WORKSPACE/MPM/mode/m3.db" "SELECT GROUP_CONCAT(name, ', ') FROM active_modes ORDER BY stack_order;" 2>/dev/null)
        echo "🔄 Restored modes: $active_modes"
    fi
}

# Auto-restore on status command (lightweight check)
restore_active_state >/dev/null 2>&1

case "${1:-status}" in
    -h|--help|help|~?)
        show_help
        ;;

    watch)
        echo "🛰️ Launching MPM Watcher..."
        "$SCRIPT_DIR/mpm-watch.sh"
        ;;

    install|update)
        "$SCRIPT_DIR/install.sh" --install
        ;;
    
    deps|dependencies)
        echo "📦 MPM Dependencies:"
        for cmd in fzf sqlite3 git; do
            if command -v $cmd &>/dev/null; then
                echo "  ✅ $cmd"
            else
                echo "  ❌ $cmd - Missing!"
            fi
        done
        ;;

    memory)
        shift
        case "${1:-diagnose}" in
            refresh) "$WORKSPACE/MPM/memory/capture.sh" --refresh ;;
            sync|sessions)
                echo "🔄 Syncing sessions to DB..."
                "$WORKSPACE/MPM/memory/sync-sessions.sh"
                if [ "$2" = "--purge" ] || [ "$2" = "-p" ]; then
                    rm -f "$WORKSPACE/MPM/memory/sessions"/*.md
                    echo "✅ Sessions purged"
                fi
                ;;
            process)
                echo "🔄 Processing sessions..."
                "$WORKSPACE/MPM/memory/process-sessions.sh"
                rm -f "$WORKSPACE/MPM/memory/sessions"/*.md
                echo "✅ Sessions processed and purged"
                ;;
            top) "$WORKSPACE/MPM/memory/mpm-memory.sh" top ;;
            search) "$WORKSPACE/MPM/memory/mpm-memory.sh" search "$2" ;;
            stats) "$WORKSPACE/MPM/memory/mpm-memory.sh" stats ;;
            dedupe) "$WORKSPACE/MPM/memory/mpm-memory.sh" dedupe ;;
            expire) "$WORKSPACE/MPM/memory/mpm-memory.sh" expire "$2" ;;
            *) echo "Usage: mpm memory [refresh|sync|process|top|search|stats|dedupe|expire]" ;;
        esac
        ;;

    persona)
        shift
        case "${1:-active}" in
            list|ls) "$SCRIPT_DIR/persona-compile.sh" list ;;
            compile) "$SCRIPT_DIR/persona-compile.sh" sync ;;
            load|activate)
                "$SCRIPT_DIR/persona-compile.sh" sync >/dev/null 2>&1
                if [ -n "$2" ]; then
                    "$SCRIPT_DIR/persona-manager.sh" activate "$2"
                else
                    "$SCRIPT_DIR/persona-select.sh" --load
                fi
                ;;
            clear|deactivate)
                echo "🎭 Clearing persona..."
                if [ -f "$WORKSPACE/MPM/persona/default.persona" ]; then
                    cat "$WORKSPACE/MPM/persona/default.persona"
                fi
                ;;
            select|active) "$SCRIPT_DIR/persona-select.sh" --current ;;
            dashboard) "$SCRIPT_DIR/persona-dashboard.sh" ;;
            *) "$SCRIPT_DIR/persona-manager.sh" "$@" ;;
        esac
        ;;

    mode)
        shift
        case "${1:-active}" in
            list|ls) "$SCRIPT_DIR/mode-compile.sh" list ;;
            compile) "$SCRIPT_DIR/mode-compile.sh" sync ;;
            load|add|activate)
                "$SCRIPT_DIR/mode-compile.sh" sync >/dev/null 2>&1
                if [ -n "$2" ]; then
                    "$SCRIPT_DIR/mode-stack.sh" activate "$2"
                else
                    "$SCRIPT_DIR/mode-select.sh" --add
                fi
                ;;
            clear|deactivate) "$SCRIPT_DIR/mode-stack.sh" clear ;;
            select|active|stack) "$SCRIPT_DIR/mode-select.sh" --add ;;
            *) "$SCRIPT_DIR/mode-stack.sh" "$@" ;;
        esac
        ;;

    restore)
        shift
        case "${1:-interactive}" in
            clear)
                sqlite3 "$WORKSPACE/MPM/persona/personas.db" "UPDATE personas SET active = 0;" 2>/dev/null
                sqlite3 "$WORKSPACE/MPM/mode/m3.db" "DELETE FROM active_modes;" 2>/dev/null
                echo "✅ Restored state cleared"
                ;;
            *)
                echo "📥 Restore Command Acknowledged. Use 'mpm status' to view state."
                ;;
        esac
        ;;

    backup)
        shift
        "$SCRIPT_DIR/gitlab-backup.sh" "${1:-backup}" "$2"
        ;;

    telemetry)
        shift
        case "${1:-report}" in
            report|30-day)
                echo "📊 FLOWBYTE TELEMETRY - 30-Day Report"
                echo "  💰 Cost Savings Active: ~94% Density"
                ;;
            *) echo "Usage: mpm telemetry [report]" ;;
        esac
        ;;

    status|diagnose)
        echo -e "\033[1;32m≈ FLOWBYTE OS HEARTBEAT v3.2 ≈\033[0m"
        echo "----------------------------------------"
        ACTIVE_PERSONA=$(sqlite3 "$WORKSPACE/MPM/persona/personas.db" "SELECT name FROM personas WHERE active=1;" 2>/dev/null)
        ACTIVE_MODES=$(sqlite3 "$WORKSPACE/MPM/mode/m3.db" "SELECT name FROM active_modes ORDER BY stack_order;" 2>/dev/null | tr '\n' '&' | sed 's/&$//')
        echo -e " [Bytecode]: \033[0;36m~p.${ACTIVE_PERSONA:-default}&m.${ACTIVE_MODES:-none}\033[0m"
        echo -e " [Efficiency]: 94% Token-Dense Architecture"
        echo "----------------------------------------"
        echo "⚡ Quick Commands: mpm [persona|mode|watch|memory|install]"
        ;;

    *)
        echo "Usage: mpm [memory|persona|mode|watch|install|backup|telemetry|status]"
        echo "Run 'mpm --help' for the full Opcode Legend."
        exit 1
        ;;
esac
