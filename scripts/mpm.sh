#!/bin/bash

# MPM - Main Entry Point (Memory + Persona + Mode)
# Usage: mpm [memory|persona|mode|backup|watch|install|status]

# Detect correct workspace path (support both .openclaw and .picoclaw)
if [ -d "$HOME/.openclaw/workspace" ]; then
    WORKSPACE="$HOME/.openclaw/workspace"
elif [ -d "$HOME/.picoclaw/workspace" ]; then
    WORKSPACE="$HOME/.picoclaw/workspace"
else
    WORKSPACE="$HOME/.openclaw/workspace"
fi
SCRIPT_DIR="$WORKSPACE/skills/mpm/scripts"

# Show help with proper quicklinks and subcommands
show_help() {
    cat << 'EOF'
📖 FLOWBYTE MPM KERNEL v3.2
Usage: mpm [command] [subcommand] [args]

═══════════════════════════════════════════════════════════════
📋 CORE COMMANDS
═══════════════════════════════════════════════════════════════

  mpm persona   <subcommand>  [Identity Management - ~p]
                ─────────────────────────────────────────────────
                list|ls       - Show all available personas
                select        - Open interactive TUI selector
                compile       - Sync markdown files to database
                dashboard     - Show persona system health
                load <name>   - Fast-activate specific persona
                clear         - Deactivate current persona

                ⚡ QUICKLINKS: ~p           (TUI selector)
                             ~p.<name>    (direct activate)
                             ~p!ls        (list all)
                             ~p!dash      (dashboard)

  mpm mode      <subcommand>  [Behavioral Management - ~m]
                ─────────────────────────────────────────────────
                list|ls       - Show all available modes
                stack         - Open interactive TUI selector
                compile       - Sync markdown files to database
                load <name>   - Fast-activate specific mode
                clear         - Remove all active modes
                activate      - Add mode to stack
                deactivate    - Remove mode from stack

                ⚡ QUICKLINKS: ~m           (TUI selector)
                             ~m.<name>    (direct activate)
                             ~m+<name>    (add to stack)
                             ~m-<name>    (remove from stack)
                             ~m.clr       (clear all)
                             ~m!ls        (list all)
                             ~m!act       (show active)

  mpm memory    <subcommand>  [Long-Term Fact Storage - ~k]
                ─────────────────────────────────────────────────
                sync          - Sync new sessions to DB
                process       - Process and purge session logs
                top           - Show top memory topics
                search <q>    - Search memory DB for queries
                stats         - Memory health and token count

                ⚡ QUICKLINKS: ~k.con       (consolidate)
                             ~k.cl        (cleanup/archive)
                             ~k?          (search/query)

  mpm bytecode                [SymAI Code Generator]
                ─────────────────────────────────────────────────
                persona <name>  - Generate ~p.X!act code
                mode <name>     - Generate ~m.XX!act code
                list            - Show all available codes
                check           - Check SymAI availability

═══════════════════════════════════════════════════════════════
🛠️ SYSTEM UTILITIES
═══════════════════════════════════════════════════════════════

  mpm watch                   - Launch live-sync background worker
  mpm install                 - Setup SymAI aliases (~p, ~m, ~?)
  mpm uninstall               - Remove SymAI aliases
  mpm restore                 - Restore previous session state
  mpm status                  - Show high-density heartbeat dashboard
  mpm deps                    - Check all dependencies

═══════════════════════════════════════════════════════════════
⚡ SYMAI OPCODE LEGEND (Silicon-Native)
═══════════════════════════════════════════════════════════════

  ~p.[id]    Persona Prefix     →  Switch to persona (e.g., ~p.oracle)
  ~m.[id]    Mode Prefix        →  Activate mode (e.g., ~m.pr)
  ~m+[id]    Mode Append        →  Add mode to stack
  ~m-[id]    Mode Remove        →  Remove mode from stack
  ~m.clr     Mode Clear         →  Clear all stacked modes
  ~k.[id]    Kernel/Memory      →  Memory operations (~k.con, ~k.cl)
  ~!         System Action      →  Immediate action (~!fix, ~!crit)
  ~$         Status/Result      →  Result indicator (~$Sync, ~$Done)
  ~?         Discovery/Query    →  Query state (~?path, ~m?)

═══════════════════════════════════════════════════════════════
💡 PRO-TIPS
═══════════════════════════════════════════════════════════════

  • Use 'mpm watch' in a terminal to auto-sync file changes
  • Press 'mpm <command>' without args for interactive mode
  • Active state is persisted in: $WORKSPACE/MPM/active.json

╚═══════════════════════════════════════════════════════════════╝
EOF
}

# Restore persisted persona/mode from previous session
restore_active_state() {
    local persona_db="$WORKSPACE/MPM/persona/personas.db"
    local mode_db="$WORKSPACE/MPM/mode/m3.db"
    
    [ -f "$persona_db" ] || return 0
    [ -f "$mode_db" ] || return 0
    
    active_persona=$(sqlite3 "$persona_db" "SELECT name FROM personas WHERE active=1 LIMIT 1;" 2>/dev/null)
    if [ -n "$active_persona" ]; then
        echo "🔄 Restored persona: $active_persona"
    fi

    active_modes_count=$(sqlite3 "$mode_db" "SELECT COUNT(*) FROM active_modes;" 2>/dev/null)
    if [ -n "$active_modes_count" ] && [ "$active_modes_count" -gt 0 ]; then
        active_modes=$(sqlite3 "$mode_db" "SELECT GROUP_CONCAT(name, ', ') FROM active_modes ORDER BY stack_order;" 2>/dev/null)
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
    
    uninstall)
        "$SCRIPT_DIR/install.sh" --uninstall
        ;;
    
    deps|dependencies)
        echo "📦 MPM Dependencies:"
        for cmd in fzf sqlite3 git whiptail inotifywait; do
            if command -v $cmd &>/dev/null; then
                echo "  ✅ $cmd"
            else
                echo "  ❌ $cmd - Missing!"
            fi
        done
        echo ""
        echo "Install missing: sudo apt install fzf sqlite3 inotify-tools whiptail"
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
                    "$SCRIPT_DIR/persona-activate.sh" activate "$2"
                else
                    "$SCRIPT_DIR/persona-select.sh" --load
                fi
                ;;
            clear|deactivate)
                echo "🎭 Clearing persona..."
                sqlite3 "$WORKSPACE/MPM/persona/personas.db" "UPDATE personas SET active=0;" 2>/dev/null
                if [ -f "$WORKSPACE/MPM/persona/default.persona" ]; then
                    cat "$WORKSPACE/MPM/persona/default.persona"
                fi
                ;;
            select|active) "$SCRIPT_DIR/persona-select.sh" --current ;;
            dashboard) "$SCRIPT_DIR/persona-dashboard.sh" ;;
            tui) "$SCRIPT_DIR/persona-tui.sh" ;;
            *) "$SCRIPT_DIR/persona-activate.sh" "$@" ;;
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
            stack|select) "$SCRIPT_DIR/mode-select.sh" --add ;;
            clear|deactivate|clr) "$SCRIPT_DIR/mode-stack.sh" clear ;;
            remove|deactivate) shift; "$SCRIPT_DIR/mode-stack.sh" deactivate "$1" ;;
            create) shift; "$SCRIPT_DIR/mode-stack.sh" create "$1" ;;
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

    status|diagnose|~$)
        echo -e "\033[1;32m╔═══════════════════════════════════════════════════════════════╗\033[0m"
        echo -e "\033[1;32m║  ≈ FLOWBYTE OS HEARTBEAT v3.2 ≈                               ║\033[0m"
        echo -e "\033[1;32m╚═══════════════════════════════════════════════════════════════╝\033[0m"
        
        local persona_db="$WORKSPACE/MPM/persona/personas.db"
        local mode_db="$WORKSPACE/MPM/mode/m3.db"
        
        ACTIVE_PERSONA=$(sqlite3 "$persona_db" "SELECT name FROM personas WHERE active=1;" 2>/dev/null)
        ACTIVE_MODES=$(sqlite3 "$mode_db" "SELECT name FROM active_modes ORDER BY stack_order;" 2>/dev/null | tr '\n' '&' | sed 's/&$/''/')
        
        echo -e "\n┌─ CURRENT STATE"
        echo -e "├─ Persona: \033[0;36m~p.${ACTIVE_PERSONA:-default}\033[0m"
        echo -e "├─ Modes:   \033[0;36m~m.${ACTIVE_MODES:-base}\033[0m"
        echo -e "├─ Density: \033[1;32m94% Token-Dense Architecture\033[0m"
        
        # Show DB stats
        local persona_count=0 mode_count=0 active_mode_count=0
        [ -f "$persona_db" ] && persona_count=$(sqlite3 "$persona_db" "SELECT COUNT(*) FROM personas;" 2>/dev/null)
        [ -f "$mode_db" ] && mode_count=$(sqlite3 "$mode_db" "SELECT COUNT(*) FROM modes;" 2>/dev/null)
        [ -f "$mode_db" ] && active_mode_count=$(sqlite3 "$mode_db" "SELECT COUNT(*) FROM active_modes;" 2>/dev/null)
        
        echo -e "├─ Loaded:  $persona_count personas, $mode_count modes ($active_mode_count active)"
        
        echo -e "\n└─ ⚡ Quick Commands:"
        echo -e "   ~p | ~m | mpm watch | mpm persona | mpm mode"
        
        # Save to active.json
        mkdir -p "$WORKSPACE/MPM"
        echo "{\"persona\":\"${ACTIVE_PERSONA:-default}\",\"modes\":[$(echo "${ACTIVE_MODES:-}" | tr '&' ',' | sed 's/,$//')],\"updated\":\"$(date -Iseconds)\"}" > "$WORKSPACE/MPM/active.json"
        ;;

    ~p)
        # Direct ~p invocation
        "$SCRIPT_DIR/persona-select.sh"
        ;;

    ~m)
        # Direct ~m invocation
        "$SCRIPT_DIR/mode-select.sh"
        ;;

    *)
        echo "Usage: mpm [memory|persona|mode|watch|install|backup|telemetry|status]"
        echo "Run 'mpm --help' for the full Opcode Legend."
        exit 1
        ;;
esac


# --- SymAI Integration ---
# Add new handler blocks before the final case statement
# Insert these before the default case (*) in mpm.sh

    bytecode|bc)
        shift
        case "${1:-list}" in
            persona|p)
                shift
                "$WORKSPACE/MPM/scripts/symai-bytecode.sh" persona "$@"
                ;;
            mode|m)
                shift
                "$WORKSPACE/MPM/scripts/symai-bytecode.sh" mode "$@"
                ;;
            list)
                "$WORKSPACE/MPM/scripts/symai-bytecode.sh" list
                ;;
            check)
                "$WORKSPACE/MPM/scripts/symai-bytecode.sh" check
                ;;
            *)
                echo "Usage: mpm bytecode [persona|mode|list|check] [args]"
                echo ""
                echo "Examples:"
                echo "  mpm bytecode persona oracle   → ~p.o!act"
                echo "  mpm bytecode mode programming → ~m.pr!act"
                echo "  mpm bytecode check            - Check SymAI status"
                ;;
        esac
        ;;
