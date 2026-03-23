#!/bin/bash
#
# 🎭 MPM Persona Dashboard v4.0 - Visual System Status
# Displays persona database status, active frequency, and quick actions
#
# Usage: ./persona-dashboard.sh [--compact] [--json]

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
    "$HOME/.picoclaw/workspace"
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
    [white]='\033[37m'
    [bg_black]='\033[40m'
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
section() { echo -e "\n${BOLD}${COLORS[cyan]}$1${RESET}"; divider; }
subsection() { echo -e "\n${BOLD}$1${RESET}"; }

# -----------------------------------------------------------------------------
# Path Detection
# -----------------------------------------------------------------------------

detect_workspace() {
    local ws=""
    
    for dir in "${CONFIG_DIRS[@]}"; do
        [[ -z "$dir" ]] && continue
        if [[ -d "$dir/mpm/persona" ]] || [[ -d "$dir/MPM/persona" ]]; then
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
        [[ -d "$parent/persona" ]] && ws="$parent"
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
    local total="$2"
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
    PERSONA_MD_FILES=("$PERSONAS_DIR"/*.persona)
    shopt -u nullglob
    MD_COUNT=${#PERSONA_MD_FILES[@]}
    
    # Database stats
    DB_COUNT=0
    ACTIVE_PERSONA=""
    DB_SIZE="N/A"
    
    if [[ -f "$DB_FILE" ]]; then
        DB_COUNT=$(sqlite3 "$DB_FILE" "SELECT COUNT(*) FROM personas;" 2>/dev/null || echo "0")
        ACTIVE_PERSONA=$(sqlite3 "$DB_FILE" "SELECT name FROM personas WHERE active=1 LIMIT 1;" 2>/dev/null || echo "")
        
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
    echo -e "${COLORS[bold]}${COLORS[magenta]}${BOX_TOP}${RESET}"
    echo -e "${COLORS[bold]}${COLORS[magenta]}${BOX_L}${RESET}  ${COLORS[bold]}🎭 Persona Dashboard${RESET}                                    v${SCRIPT_VERSION}  ${COLORS[bold]}${COLORS[magenta]}${BOX_L}${RESET}"
    echo -e "${COLORS[bold]}${COLORS[magenta]}${BOX_MID}${RESET}"
    
    # Active frequency (centered)
    if [[ -n "$ACTIVE_PERSONA" ]]; then
        printf "${COLORS[bold]}${COLORS[magenta]}${BOX_L}${RESET}  ${COLORS[cyan]}📡 Active: ~p.${ACTIVE_PERSONA}${RESET}%*s${COLORS[bold]}${COLORS[magenta]}${BOX_L}${RESET}\n" $((50 - ${#ACTIVE_PERSONA})) ""
    else
        echo -e "${COLORS[bold]}${COLORS[magenta]}${BOX_L}${RESET}  ${COLORS[yellow]}⚠ No active persona${RESET}                                  ${COLORS[bold]}${COLORS[magenta]}${BOX_L}${RESET}"
    fi
    echo -e "${COLORS[bold]}${COLORS[magenta]}${BOX_BOT}${RESET}"
}

show_status_cards() {
    section "📊 System Status"
    
    # Three-column layout
    local card1="${COLORS[green]}${BOX_TOP}${RESET}"
    local card2="${COLORS[blue]}${BOX_TOP}${RESET}"
    local card3="${COLORS[yellow]}${BOX_TOP}${RESET}"
    
    echo -e "  ${COLORS[green]}┌ Persona Files ┐${RESET}      ${COLORS[blue]}┌ Database ┐${RESET}      ${COLORS[yellow]}┌ Sync Status ┐${RESET}"
    echo -e "  ${COLORS[green]}│${RESET}  ${BOLD}${MD_COUNT}${RESET} MD files ${COLORS[green]}│${RESET}      ${COLORS[blue]}│${RESET}  ${BOLD}${DB_COUNT}${RESET} loaded  ${COLORS[blue]}│${RESET}      ${COLORS[yellow]}│${RESET} $(status_icon "$SYNC_STATUS") ${SYNC_STATUS^^} ${COLORS[yellow]}│${RESET}"
    echo -e "  ${COLORS[green]}└───────────────┘${RESET}      ${COLORS[blue]}└──────────┘${RESET}      ${COLORS[yellow]}└─────────────┘${RESET}"
    
    # Sync progress bar
    if [[ "$MD_COUNT" -gt 0 ]]; then
        progress_bar "$DB_COUNT" "$MD_COUNT" 40 "     Sync Progress"
    fi
}

show_persona_list() {
    [[ "$DB_COUNT" -eq 0 ]] && return
    
    section "🎭 Active Personas"
    
    # Table header
    printf "  ${BOLD}%-4s %-12s %-22s %-25s %-8s${RESET}\n" "ICON" "NAME" "IDENTITY" "TITLE" "STATUS"
    divider
    
    # Fetch and display
    local query="SELECT identity_emoji, name, identity_name, identity_title, active FROM personas ORDER BY name;"
    sqlite3 "$DB_FILE" "$query" 2>/dev/null | while IFS='|' read -r emoji name identity title active; do
        emoji="${emoji:-🔹}"
        identity="${identity:-Unknown}"
        title="${title:-N/A}"
        
        # Truncate long fields
        identity="${identity:0:20}"
        title="${title:0:24}"
        
        if [[ "$active" == "1" ]]; then
            status="${COLORS[green]}ACTIVE${RESET}"
        else
            status="${COLORS[dim]}ready${RESET}"
        fi
        
        printf "  %-4s ${COLORS[cyan]}%-12s${RESET} %-22s %-25s %b\n" \
            "$emoji" "$name" "$identity" "$title" "$status"
    done
}

show_quicklinks() {
    section "⚡ Quick Links"
    
    echo -e "  ${BOLD}Persona Operations:${RESET}"
    echo ""
    echo -e "    ${COLORS[green]}~p${RESET}                        Open interactive persona selector"
    echo -e "    ${COLORS[green]}~p.<name>${RESET}                Quick activate: ~p.coder, ~p.writer"
    echo -e "    ${COLORS[blue]}./persona-ls.sh${RESET}          List all available personas"
    echo -e "    ${COLORS[blue]}./mpm.sh -p <name>${RESET}       Load persona (legacy)"
    echo ""
    echo -e "  ${BOLD}Management:${RESET}"
    echo ""
    echo -e "    ${COLORS[blue]}./persona-compile.sh${RESET}    Compile all .persona files to DB"
    echo -e "    ${COLORS[blue]}./persona-compile.sh sync${RESET} Force re-sync of all files"
    echo -e "    ${COLORS[yellow]}nano <name>.persona${RESET}      Edit persona definition"
    echo -e "    ${COLORS[yellow]}code <name>.persona${RESET}      Edit persona in VS Code"
    echo ""
    echo -e "  ${BOLD}System:${RESET}"
    echo ""
    echo -e "    ${COLORS[blue]}./persona-dashboard.sh${RESET}   Refresh this dashboard"
    echo -e "    ${COLORS[blue]}./medic.sh --health${RESET}      Run system health check"
    echo -e "    ${COLORS[blue]}./mpm-watch.sh${RESET}           Monitor changes"
}

show_stats() {
    section "💰 Efficiency Metrics"
    
    if [[ "$DB_COUNT" -gt 0 ]]; then
        local md_total=$((MD_COUNT * 1024))
        local db_query_size=$((DB_COUNT * 50))
        local savings_pct=0
        [[ "$md_total" -gt 0 ]] && savings_pct=$(( (md_total - db_query_size) * 100 / md_total ))
        
        echo ""
        echo -e "  ${COLORS[green]}${BOX_TOP}${RESET}"
        printf "  ${COLORS[green]}${BOX_L}${RESET}  Token Efficiency: ${BOLD}%d%%${RESET}%*s${COLORS[green]}${BOX_L}${RESET}\n" "$savings_pct" $((48)) ""
        echo -e "  ${COLORS[green]}${BOX_L}${RESET}  DB query vs loading full MD files                   ${COLORS[green]}${BOX_L}${RESET}"
        echo -e "  ${COLORS[green]}${BOX_BOT}${RESET}"
        echo ""
        echo -e "  ${DIM}Persona files: ~${md_total} bytes${RESET}"
        echo -e "  ${DIM}DB queries:     ~${db_query_size} bytes${RESET}"
        echo -e "  ${DIM}DB size:        ${DB_SIZE}${RESET}"
    else
        echo -e "  ${COLORS[yellow]}Database not initialized. Run ./persona-compile.sh${RESET}"
    fi
}

show_paths() {
    section "📁 System Paths"
    
    echo -e "  ${COLORS[dim]}Workspace:${RESET}    $WORKSPACE"
    echo -e "  ${COLORS[dim]}Personas:${RESET}     $PERSONAS_DIR"
    echo -e "  ${COLORS[dim]}Database:${RESET}     $DB_FILE"
    echo -e "  ${COLORS[dim]}Scripts:${RESET}      $(dirname "$0")"
}

show_compact_view() {
    # Single-line status for embedding in prompts
    local status=""
    [[ -n "$ACTIVE_PERSONA" ]] && status="~p.${ACTIVE_PERSONA}"
    
    echo -e "${COLORS[magenta]}[Persona]${RESET} ${status:-none} | ${DB_COUNT}/${MD_COUNT} personas | ${SYNC_STATUS}"
}

show_json() {
    # JSON output for programmatic use
    cat <<EOF
{
  "version": "$SCRIPT_VERSION",
  "active": "${ACTIVE_PERSONA:-null}",
  "stats": {
    "md_files": $MD_COUNT,
    "db_records": $DB_COUNT,
    "db_size": "$DB_SIZE",
    "sync_status": "$SYNC_STATUS",
    "sync_percent": $SYNC_PCT
  },
  "paths": {
    "workspace": "$WORKSPACE",
    "personas": "$PERSONAS_DIR",
    "database": "$DB_FILE"
  },
  "personas": [
EOF
    
    if [[ "$DB_COUNT" -gt 0 ]]; then
        local first=true
        sqlite3 "$DB_FILE" "SELECT name, identity_name, identity_title, identity_emoji, active FROM personas ORDER BY name;" 2>/dev/null | while IFS='|' read -r name identity title emoji active; do
            [[ "$first" == true ]] || echo ","
            first=false
            cat <<EOF
    {
      "name": "$name",
      "identity": "${identity:-null}",
      "title": "${title:-null}",
      "emoji": "${emoji:-🔹}",
      "active": $([[ "$active" == "1" ]] && echo "true" || echo "false")
    }
EOF
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
🎭 MPM Persona Dashboard v4.0

Displays persona system status, active frequency, and provides quick actions.

USAGE:
  ./persona-dashboard.sh [OPTIONS]

OPTIONS:
  --compact, -c       Show single-line status (for embedding)
  --json, -j          Output as JSON
  --quiet, -q         Suppress header/footer
  --version, -V       Show version
  --help, -h          Show this help

EXAMPLES:
  ./persona-dashboard.sh           # Full interactive dashboard
  ./persona-dashboard.sh -c        # Compact: [Persona] ~p.coder | 5/6 | synced
  ./persona-dashboard.sh -j | jq  # JSON output for scripting

RETURN CODES:
  0  Success
  1  Database error
  2  Workspace not found

ENVIRONMENT:
  MPM_WORKSPACE      Path to MPM workspace

SHORTCUTS:
  ~p                 Interactive persona selector
  ~p.<name>          Direct activation
  ~k                 Interactive command selector
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
    
    # Setup paths (support both MPM and mpm)
    if [[ -d "$WORKSPACE/mpm/persona" ]]; then
        PERSONAS_DIR="$WORKSPACE/mpm/persona"
        PERSONAS_NAME="mpm"
    else
        PERSONAS_DIR="$WORKSPACE/MPM/persona"
        PERSONAS_NAME="MPM"
    fi
    DB_FILE="$PERSONAS_DIR/personas.db"
    
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
            show_persona_list
            $quiet || show_quicklinks
            show_stats
            $quiet || show_paths
            $quiet && echo "OK: $DB_COUNT/$MD_COUNT personas, $SYNC_STATUS"
            ;;
    esac
}

cd "$(dirname "${BASH_SOURCE[0]}")" || exit 1
main "$@"
