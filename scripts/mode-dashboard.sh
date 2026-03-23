#!/bin/bash
#
# 🛠️ MPM Mode Dashboard v4.0 - Operational Mode Status System
# Displays active mode stack, system health, and quick actions
#
# Usage: ./mode-dashboard.sh [--compact] [--json]

set -euo pipefail

# -----------------------------------------------------------------------------
# Configuration
# -----------------------------------------------------------------------------

readonly SCRIPT_VERSION="4.0"
readonly CONFIG_DIRS=(
    "${MPM_WORKSPACE:-}"
    "${OPENCLAW_WORKSPACE:-}"
    "${PICOLCLAW_WORKSPACE:-}"
    "$HOME/.mpm"
    "$HOME/.openclaw/workspace"
    "${HOME}/.picoclaw/workspace"
)

# -----------------------------------------------------------------------------
# Colors & Formatting
# -----------------------------------------------------------------------------

declare -A COLORS=(
    [reset]='\033[0m'
    [bold]='\033[1m'
    [dim]='\033[2m'
    [italic]='\033[3m'
    [underline]='\033[4m'
    [red]='\033[31m'
    [green]='\033[32m'
    [yellow]='\033[33m'
    [blue]='\033[34m'
    [magenta]='\033[35m'
    [cyan]='\033[36m'
    [orange]='\033[38;5;208m'
    [white]='\033[37m'
)

DIM="${COLORS[dim]}"
RESET="${COLORS[reset]}"
BOLD="${COLORS[bold]}"

# UI Elements
BOX_TOP="╔══════════════════════════════════════════════════════════════════════╗"
BOX_MID="╠══════════════════════════════════════════════════════════════════════╣"
BOX_BOT="╚══════════════════════════════════════════════════════════════════════╝"
BOX_L="║"

divider() { echo -e "${COLORS[dim]}──────────────────────────────────────────────────────────────────────${RESET}"; }
section() { echo -e "\n${BOLD}${COLORS[orange]}$1${RESET}"; divider; }
subsection() { echo -e "\n${BOLD}$1${RESET}"; }

# -----------------------------------------------------------------------------
# Path Detection
# -----------------------------------------------------------------------------

detect_workspace() {
    local ws=""
    
    for dir in "${CONFIG_DIRS[@]}"; do
        [[ -z "$dir" ]] && continue
        if [[ -d "$dir/mpm/mode" ]] || [[ -d "$dir/MPM/mode" ]]; then
            ws="$dir"
            break
        fi
    done
    
    # Fallback: relative to script
    if [[ -z "$ws" ]]; then
        local script_dir
        script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
        local parent
        parent="$(dirname "$script_dir")"
        [[ -d "$parent/mode" ]] && ws="$parent"
    fi
    
    echo "$ws"
}

# -----------------------------------------------------------------------------
# Helper Functions
# -----------------------------------------------------------------------------

format_bytes() {
    local bytes="$1"
    [[ -z "$bytes" || "$bytes" == "N/A" ]] && echo "N/A" && return
    
    if command -v numfmt &>/dev/null; then
        numfmt --to=iec-i --suffix=B "$bytes" 2>/dev/null || echo "${bytes}B"
    else
        if (( bytes > 1024*1024*1024 )); then
            echo "$(echo "scale=2; $bytes/1024/1024/1024" | bc 2>/dev/null || echo "$((bytes/1024/1024/1024))") GiB"
        elif (( bytes > 1024*1024 )); then
            echo "$(echo "scale=2; $bytes/1024/1024" | bc 2>/dev/null || echo "$((bytes/1024/1024))") MiB"
        elif (( bytes > 1024 )); then
            echo "$((bytes/1024)) KiB"
        else
            echo "${bytes} B"
        fi
    fi
}

progress_bar() {
    local current="$1"
    local total="${2:-100}"
    local width="${3:-30}"
    local label="${4:-}"
    
    [[ -z "$total" || "$total" -eq 0 ]] && { echo "$label"; return; }
    
    local percent=$((current * 100 / total))
    local filled=$((width * current / total))
    local empty=$((width - filled))
    
    local bar=""
    for ((i=0; i<filled; i++)); do bar+="█"; done
    for ((i=0; i<empty; i++)); do bar+="░"; done
    
    printf "%s [%s] %d%% (%d/%d)\n" "$label" "$bar" "$percent" "$current" "$total"
}

stack_indicator() {
    local depth="$1"
    local max="${2:-5}"
    local output=""
    
    for ((i=1; i<=max; i++)); do
        if (( i <= depth )); then
            output+="${COLORS[green]}▆${RESET}"
        else
            output+="${COLORS[dim]}▱${RESET}"
        fi
    done
    
    echo "$output"
}

status_icon() {
    local status="$1"
    case "$status" in
        ok|synced|active)  echo -e "${COLORS[green]}●${RESET}" ;;
        warn|out|dirty)    echo -e "${COLORS[yellow]}◐${RESET}" ;;
        error|missing)     echo -e "${COLORS[red]}✗${RESET}" ;;
        *)                 echo -e "${COLORS[dim]}○${RESET}" ;;
    esac
}

# -----------------------------------------------------------------------------
# Data Gathering
# -----------------------------------------------------------------------------

gather_stats() {
    # File counts
    shopt -s nullglob
    MODE_MD_FILES=("$MODES_DIR"/*.mode)
    shopt -u nullglob
    MD_COUNT=${#MODE_MD_FILES[@]}
    
    # Database stats
    DB_COUNT=0
    ACTIVE_COUNT=0
    ACTIVE_MODES_STR=""
    DB_SIZE="N/A"
    
    if [[ -f "$DB_FILE" ]]; then
        DB_COUNT=$(sqlite3 "$DB_FILE" "SELECT COUNT(*) FROM modes;" 2>/dev/null || echo "0")
        ACTIVE_COUNT=$(sqlite3 "$DB_FILE" "SELECT COUNT(*) FROM active_modes;" 2>/dev/null || echo "0")
        ACTIVE_MODES_STR=$(sqlite3 "$DB_FILE" "SELECT name FROM active_modes ORDER BY stack_order;" 2>/dev/null | paste -sd "&" - | sed 's/&$//' || echo "")
        
        local size_bytes
        size_bytes=$(stat -f%z "$DB_FILE" 2>/dev/null || stat -c%s "$DB_FILE" 2>/dev/null || echo "0")
        DB_SIZE=$(format_bytes "$size_bytes")
    fi
    
    # Sync status
    SYNC_STATUS="unknown"
    if [[ "$MD_COUNT" -eq "$DB_COUNT" && "$DB_COUNT" -gt 0 ]]; then
        SYNC_STATUS="synced"
    elif [[ "$DB_COUNT" -gt 0 ]]; then
        SYNC_STATUS="out"
    else
        SYNC_STATUS="missing"
    fi
    
    # Calculate sync percentage
    SYNC_PCT=0
    if [[ "$MD_COUNT" -gt 0 ]]; then
        SYNC_PCT=$((DB_COUNT * 100 / MD_COUNT))
    fi
}

# -----------------------------------------------------------------------------
# Display Functions
# -----------------------------------------------------------------------------

show_header() {
    echo -e "${COLORS[bold]}${COLORS[orange]}${BOX_TOP}${RESET}"
    echo -e "${COLORS[bold]}${COLORS[orange]}${BOX_L}${RESET}  ${COLORS[bold]}🛠️ Mode Dashboard${RESET}                                    v${SCRIPT_VERSION}  ${COLORS[bold]}${COLORS[orange]}${BOX_L}${RESET}"
    echo -e "${COLORS[bold]}${COLORS[orange]}${BOX_MID}${RESET}"
    
    # Bytecode status
    if [[ -n "$ACTIVE_MODES_STR" ]]; then
        local bc_status="~m.${ACTIVE_MODES_STR}"
        printf "${COLORS[bold]}${COLORS[orange]}${BOX_L}${RESET}  ${COLORS[green]}${bc_status}${RESET} %*s${COLORS[bold]}${COLORS[orange]}${BOX_L}${RESET}\n" $((53 - ${#bc_status})) ""
        echo -e "${COLORS[bold]}${COLORS[orange]}${BOX_L}${RESET}  ${COLORS[green]}🟢 Active Stack${RESET}                                    ${COLORS[bold]}${COLORS[orange]}${BOX_L}${RESET}"
    else
        echo -e "${COLORS[bold]}${COLORS[orange]}${BOX_L}${RESET}  ${COLORS[yellow]}~m.base${RESET} (base behavior)                               ${COLORS[bold]}${COLORS[orange]}${BOX_L}${RESET}"
        echo -e "${COLORS[bold]}${COLORS[orange]}${BOX_L}${RESET}  ${COLORS[dim]}⚪ No active modes${RESET}                                ${COLORS[bold]}${COLORS[orange]}${BOX_L}${RESET}"
    fi
    echo -e "${COLORS[bold]}${COLORS[orange]}${BOX_BOT}${RESET}"
}

show_status_cards() {
    section "📊 System Status"
    
    # Four-column layout
    echo -e "  ${COLORS[green]}┌── Modes ──┐${RESET}  ${COLORS[blue]}┌── Loaded ──┐${RESET}  ${COLORS[magenta]}┌─ Active ─┐${RESET}  ${COLORS[yellow]}┌── Sync ──┐${RESET}"
    printf "  ${COLORS[green]}│${RESET} %3d files ${COLORS[green]}│${RESET}  ${COLORS[blue]}│${RESET} %3d in DB ${COLORS[blue]}│${RESET}  ${COLORS[magenta]}│${RESET} %2d stack ${COLORS[magenta]}│${RESET}  ${COLORS[yellow]}│${RESET} $(status_icon "$SYNC_STATUS") %-5s ${COLORS[yellow]}│${RESET}\n" \
        "$MD_COUNT" "$DB_COUNT" "$ACTIVE_COUNT" "${SYNC_STATUS^^}"
    echo -e "  ${COLORS[green]}└───────────┘${RESET}  ${COLORS[blue]}└────────────┘${RESET}  ${COLORS[magenta]}└──────────┘${RESET}  ${COLORS[yellow]}└──────────┘${RESET}"
    
    # Sync progress
    if [[ "$MD_COUNT" -gt 0 ]]; then
        progress_bar "$DB_COUNT" "$MD_COUNT" 40 "     Sync Progress"
    fi
    
    # Stack depth indicator
    if [[ "$ACTIVE_COUNT" -gt 0 ]]; then
        echo ""
        echo -e "  Stack Depth: $(stack_indicator "$ACTIVE_COUNT") ${ACTIVE_COUNT}/5"
    fi
}

show_active_stack() {
    [[ "$ACTIVE_COUNT" -eq 0 ]] && return
    
    section "🛠️ Active Mode Stack"
    
    # Table header
    printf "  ${BOLD}%-4s %-12s %-20s %-10s %-12s${RESET}\n" "LVL" "NAME" "PURPOSE" "SYMBOL" "DENSITY"
    divider
    
    # Fetch active modes with details
    local query="SELECT am.stack_order, am.name, m.purpose, m.bytecode_signature, m.orig_tokens 
                 FROM active_modes am 
                 LEFT JOIN modes m ON am.name = m.name 
                 ORDER BY am.stack_order;"
    
    local count=0
    sqlite3 "$DB_FILE" "$query" 2>/dev/null | while IFS='|' read -r order name purpose sig orig_tokens; do
        ((count++))
        purpose="${purpose:-N/A}"
        purpose="${purpose:0:18}"
        
        local sym_id="~m.${name}"
        [[ -n "$sig" ]] && sym_id="${sig}"
        
        local density="~94%"
        if [[ -n "$orig_tokens" && "$orig_tokens" =~ ^[0-9]+$ ]]; then
            local db_tokens=50
            local calc=$(( (orig_tokens - db_tokens) * 100 / orig_tokens ))
            density="~${calc}%"
        fi
        
        # Level indicator
        local lvl=""
        for ((i=1; i<=order; i++)); do lvl+="▲"; done
        
        printf "  ${COLORS[green]}%-4s${RESET} ${COLORS[cyan]}%-12s${RESET} %-20s ${COLORS[dim]}%-10s${RESET} %s\n" \
            "$lvl" "$name" "$purpose" "$sym_id" "$density"
    done
    
    echo ""
    echo -e "  Use ${COLORS[yellow]}~m.clr${RESET} to clear stack, ${COLORS[green]}~m+<name>${RESET} to add mode"
}

show_mode_library() {
    [[ "$DB_COUNT" -eq 0 ]] && return
    
    section "📚 Mode Library"
    
    # Table header
    printf "  ${BOLD}%-16s %-35s %-15s${RESET}\n" "MODE" "PURPOSE" "COMPRESSION"
    divider
    
    local query="SELECT name, purpose, bytecode_signature, COALESCE(orig_tokens, 0) FROM modes ORDER BY name;"
    
    sqlite3 "$DB_FILE" "$query" 2>/dev/null | while IFS='|' read -r name purpose sig orig_tokens; do
        purpose="${purpose:-N/A}"
        purpose="${purpose:0:33}"
        
        local compression="~94%"
        if [[ "$orig_tokens" =~ ^[0-9]+$ && "$orig_tokens" -gt 0 ]]; then
            local db_tokens=50
            local calc=$(( (orig_tokens - db_tokens) * 100 / orig_tokens ))
            compression="~${calc}%"
        fi
        
        printf "  ${COLORS[cyan]}%-16s${RESET} %-35s ${COLORS[green]}%s${RESET}\n" "$name" "$purpose" "$compression"
    done
    
    echo ""
    echo -e "  Total: ${DB_COUNT} modes loaded | DB size: ${DB_SIZE}"
}

show_quicklinks() {
    section "⚡ Quick Commands"
    
    echo -e "  ${BOLD}Mode Operations:${RESET}"
    echo ""
    echo -e "    ${COLORS[green]}~m${RESET}                      Open interactive mode selector"
    echo -e "    ${COLORS[green]}~m.<name>${RESET}                Direct load: ~m.code, ~m.doc"
    echo -e "    ${COLORS[green]}~m+<name>${RESET}                Add to stack (e.g., ~m+explain)"
    echo -e "    ${COLORS[yellow]}~m.clr${RESET}                   Clear entire mode stack"
    echo -e "    ${COLORS[yellow]}~m.pop${RESET}                   Remove last mode from stack"
    echo -e "    ${COLORS[blue]}./mode-ls.sh${RESET}            List all available modes"
    echo ""
    echo -e "  ${BOLD}Stack Examples:${RESET}"
    echo ""
    echo -e "    ${DIM}~m.code ~m+security     =  code mode + security mode${RESET}"
    echo -e "    ${DIM}~m.doc ~m+verbose      =  documentation + verbose mode${RESET}"
    echo -e "    ${DIM}~m.clr ~m.debug        =  clear then enable debug${RESET}"
    echo ""
    echo -e "  ${BOLD}Management:${RESET}"
    echo ""
    echo -e "    ${COLORS[blue]}./mode-compile.sh${RESET}       Compile all .mode files"
    echo -e "    ${COLORS[blue]}./mode-compile.sh sync${RESET}  Force re-sync"
    echo -e "    ${COLORS[cyan]}nano <name>.mode${RESET}        Edit mode definition"
    echo ""
    echo -e "  ${BOLD}Monitoring:${RESET}"
    echo ""
    echo -e "    ${COLORS[blue]}./mode-dashboard.sh${RESET}     Refresh this dashboard"
    echo -e "    ${COLORS[blue]}./medic.sh --health${RESET}     System health check"
    echo -e "    ${COLORS[blue]}./mpm-watch.sh${RESET}        Watch for file changes"
}

show_efficiency() {
    [[ "$DB_COUNT" -eq 0 ]] && return
    
    section "💰 Efficiency Analysis"
    
    local md_total=$((MD_COUNT * 2048))
    local db_query_size=$((DB_COUNT * 50))
    local savings_pct=0
    local active_savings=0
    
    [[ "$md_total" -gt 0 ]] && savings_pct=$(( (md_total - db_query_size) * 100 / md_total ))
    
    # Calculate active stack savings
    if [[ "$ACTIVE_COUNT" -gt 0 ]]; then
        local active_full=$((ACTIVE_COUNT * 2048))
        local active_db=$((ACTIVE_COUNT * 50))
        active_savings=$(( (active_full - active_db) * 100 / active_full ))
    fi
    
    echo ""
    echo -e "  ${COLORS[green]}${BOX_TOP}${RESET}"
    printf "  ${COLORS[green]}${BOX_L}${RESET}  Token Efficiency: Library ${BOLD}%d%%${RESET}%*s${COLORS[green]}${BOX_L}${RESET}\n" "$savings_pct" $((20)) ""
    if [[ "$ACTIVE_COUNT" -gt 0 ]]; then
        printf "  ${COLORS[green]}${BOX_L}${RESET}  Active Stack:     ${BOLD}%d%%${RESET}%*s${COLORS[green]}${BOX_L}${RESET}\n" "$active_savings" $((25)) ""
    fi
    echo -e "  ${COLORS[green]}${BOX_BOT}${RESET}"
    echo ""
    echo -e "  ${DIM}MD mode files: ~${md_total} bytes${RESET}"
    echo -e "  ${DIM}DB queries:    ~${db_query_size} bytes${RESET}"
    echo -e "  ${DIM}DB file size:   ${DB_SIZE}${RESET}"
}

show_paths() {
    section "📁 System Paths"
    
    echo -e "  ${COLORS[dim]}Workspace:${RESET}    $WORKSPACE"
    echo -e "  ${COLORS[dim]}Modes:${RESET}       $MODES_DIR"
    echo -e "  ${COLORS[dim]}Database:${RESET}     $DB_FILE"
    echo -e "  ${COLORS[dim]}Scripts:${RESET}      $(dirname "$0")"
}

show_compact_view() {
    local status="base"
    local stack_info=""
    
    [[ -n "$ACTIVE_MODES_STR" ]] && status="$ACTIVE_MODES_STR"
    [[ "$ACTIVE_COUNT" -gt 0 ]] && stack_info="[${ACTIVE_COUNT} modes]"
    
    echo -e "${COLORS[orange]}[Mode]${RESET} ~m.${status} ${stack_info} | ${DB_COUNT}/${MD_COUNT} loaded | ${SYNC_STATUS}"
}

show_json() {
    cat <<EOF
{
  "version": "$SCRIPT_VERSION",
  "active_modes": "${ACTIVE_MODES_STR:-base}",
  "stats": {
    "md_files": $MD_COUNT,
    "db_modes": $DB_COUNT,
    "active_count": $ACTIVE_COUNT,
    "db_size": "$DB_SIZE",
    "sync_status": "$SYNC_STATUS",
    "sync_percent": $SYNC_PCT
  },
  "paths": {
    "workspace": "$WORKSPACE",
    "modes": "$MODES_DIR",
    "database": "$DB_FILE"
  },
  "stack": [
EOF
    if [[ "$ACTIVE_COUNT" -gt 0 ]]; then
        local first=true
        sqlite3 "$DB_FILE" "SELECT name, stack_order FROM active_modes ORDER BY stack_order;" 2>/dev/null | while IFS='|' read -r name order; do
            [[ "$first" == true ]] || echo ","
            first=false
            printf "    { \"name\": \"%s\", \"order\": %s }" "$name" "$order"
        done
        echo ""
    fi
    echo "  ]"
    echo "}"
}

# -----------------------------------------------------------------------------
# Help
# -----------------------------------------------------------------------------

show_help() {
    cat <<'EOF'
🛠️ MPM Mode Dashboard v4.0

Displays operational mode stack, system health, and provides quick actions.

USAGE:
  ./mode-dashboard.sh [OPTIONS]

OPTIONS:
  --compact, -c       Show single-line status (for embedding)
  --json, -j          Output as JSON
  --quiet, -q         Suppress header/footer
  --version, -V       Show version
  --help, -h          Show this help

EXAMPLES:
  ./mode-dashboard.sh           # Full visual dashboard
  ./mode-dashboard.sh -c          # Compact: [Mode] ~m.code | [1] | synced
  ./mode-dashboard.sh -j | jq   # JSON output for scripting
  mpm mode dashboard             # Short command alias

BYTECODE COMMANDS:
  ~m                  Interactive mode selector
  ~m.<name>           Load single mode (e.g., ~m.code)
  ~m+<name>           Add to stack (e.g., ~m.code~m+security)
  ~m.clr              Clear entire stack
  ~m.pop              Remove last mode

STACK BEHAVIOR:
  Modes stack! You can active multiple modes simultaneously:
    ~m.code ~m+debug = coding mode with debug output
    ~m.doc ~m+verbose = documentation with verbose details

RETURN CODES:
  0  Success
  1  Database error
  2  Workspace not found

ENVIRONMENT:
  MPM_WORKSPACE       Path to MPM workspace

See also:
  ./persona-dashboard.sh  # View persona status
EOF
}

# -----------------------------------------------------------------------------
# Main
# -----------------------------------------------------------------------------

main() {
    local mode="full"
    local quiet=false
    
    for arg in "$@"; do
        case "$arg" in
            --compact|-c)     mode="compact" ;;
            --json|-j)          mode="json" ;;
            --quiet|-q)         quiet=true ;;
            --version|-V)       echo "v$SCRIPT_VERSION"; exit 0 ;;
            --help|-h)          show_help; exit 0 ;;
            *)                  echo "Unknown: $arg"; show_help; exit 1 ;;
        esac
    done
    
    # Detect workspace
    WORKSPACE=$(detect_workspace)
    [[ -z "$WORKSPACE" ]] && { echo "Error: Workspace not found" >&2; exit 2; }
    
    # Setup paths (support both mpm and MPM)
    if [[ -d "$WORKSPACE/mpm/mode" ]]; then
        MODES_DIR="$WORKSPACE/mpm/mode"
    else
        MODES_DIR="$WORKSPACE/MPM/mode"
    fi
    DB_FILE="$MODES_DIR/m3.db"
    
    # Gather statistics
    gather_stats
    
    # Output based on mode
    case "$mode" in
        compact)
            show_compact_view
            ;;
        json)
            show_json
            ;;
        full)
            $quiet || show_header
            show_status_cards
            show_active_stack
            show_mode_library
            $quiet || show_quicklinks
            show_efficiency
            $quiet || show_paths
            $quiet && echo "OK: ${DB_COUNT} modes, ${ACTIVE_COUNT} active, ${SYNC_STATUS}"
            ;;
    esac
}

cd "$(dirname "${BASH_SOURCE[0]}")" || exit 1
main "$@"
