#!/bin/bash
#
# 🎭 MPM Persona LS v4.0 - List personas with SymAI bytecode references
# Displays available personas, loaded status, and bytecode shortcuts
#
# Usage: ./persona-ls.sh [--loaded|--db] [--all] [--tree] [--compact] [--bytecode]

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
    [red]='\033[31m'
    [green]='\033[32m'
    [yellow]='\033[33m'
    [blue]='\033[34m'
    [magenta]='\033[35m'
    [cyan]='\033[36m'
    [orange]='\033[38;5;208m'
    [white]='\033[37m'
    [gray]='\033[90m'
)

DIM="${COLORS[dim]}"
RESET="${COLORS[reset]}"
BOLD="${COLORS[bold]}"

divider() { echo -e "${COLORS[dim]}─────────────────────────────────────────────────────────────${RESET}"; }
section() { echo -e "\n${BOLD}${COLORS[magenta]}$1${RESET}"; divider; }

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
    [[ -z "$bytes" || "$bytes" == "0" ]] && echo "0 B" && return
    
    if command -v numfmt &>/dev/null; then
        numfmt --to=iec-i --suffix=B "$bytes" 2>/dev/null || echo "${bytes} B"
    else
        if (( bytes > 1024*1024 )); then
            echo "$((bytes/1024/1024)) MiB"
        elif (( bytes > 1024 )); then
            echo "$((bytes/1024)) KiB"
        else
            echo "${bytes} B"
        fi
    fi
}

is_loaded() {
    local name="$1"
    [[ -f "$DB_FILE" ]] || return 1
    
    local count
    count=$(sqlite3 "$DB_FILE" "SELECT COUNT(*) FROM persona_files WHERE name='$name';" 2>/dev/null || echo "0")
    [[ "$count" -gt 0 ]]
}

get_active_persona() {
    [[ -f "$DB_FILE" ]] || return 1
    sqlite3 "$DB_FILE" "SELECT name FROM active_personas LIMIT 1;" 2>/dev/null || echo ""
}

get_persona_title() {
    local file="$1"
    local title=""
    
    if [[ -f "$file" ]]; then
        title=$(grep -m 1 "^[tT]itle:" "$file" 2>/dev/null | \
                sed 's/^[tT]itle:[[:space:]]*//; s/^[*-][[:space:]]*//; s/[[:space:]]*$//' | \
                tr -d '\r')
    fi
    echo "${title:-N/A}"
}

get_persona_identity() {
    local name="$1"
    local identity="$1"
    
    if [[ -f "$DB_FILE" ]]; then
        local db_identity
        db_identity=$(sqlite3 "$DB_FILE" "SELECT identity_name FROM personas WHERE name='$name';" 2>/dev/null || echo "")
        [[ -n "$db_identity" ]] && identity="$db_identity"
    fi
    
    echo "$identity"
}

# -----------------------------------------------------------------------------
# Display Modes
# -----------------------------------------------------------------------------

show_default() {
    section "🎭 Available Personas"
    
    local active
    active=$(get_active_persona)
    active="${active:-}"
    
    # Header
    printf "  ${BOLD}%-3s %-16s %-24s %-12s${RESET}\n" "" "NAME" "TITLE" "STATUS"
    divider
    
    shopt -s nullglob
    local files=("$PERSONAS_DIR"/*.persona)
    shopt -u nullglob
    
    [[ ${#files[@]} -eq 0 ]] && { echo "No personas found in $PERSONAS_DIR"; return; }
    
    for file in "${files[@]}"; do
        local name
        name=$(basename "$file" .persona)
        
        # Skip invalid names
        [[ "$name" =~ ^[a-zA-Z0-9_-]+$ ]] || continue
        
        local title
        title=$(get_persona_title "$file")
        title="${title:0:22}"
        
        local loaded="${COLORS[red]}✗${RESET}"
        local active_marker="  "
        
        if is_loaded "$name"; then
            loaded="${COLORS[green]}✓${RESET}"
        fi
        
        if [[ "$name" == "$active" ]]; then
            active_marker="${COLORS[green]}●${RESET} "
        else
            active_marker="  "
        fi
        
        printf "  %s${COLORS[cyan]}%-16s${RESET} %-25s %s\n" \
            "$active_marker" "$name" "$title" "$loaded"
    done
    
    local count=${#files[@]}
    echo ""
    echo -e "  Total: ${COLORS[cyan]}${count}${RESET} personas | ${COLORS[green]}●${RESET} = active | ${COLORS[green]}✓${RESET} = in DB | ${COLORS[red]}✗${RESET} = file only"
}

show_loaded() {
    section "🎭 Loaded Personas (Database)"
    
    if [[ ! -f "$DB_FILE" ]]; then
        echo -e "  ${COLORS[yellow]}⚠ No database found${RESET}"
        echo "  Run: ./persona-compile.sh to initialize"
        return 1
    fi
    
    local active
    active=$(get_active_persona)
    active="${active:-}"
    
    # Header
    printf "  ${BOLD}%-4s %-12s %-28s %-18s${RESET}\n" "" "NAME" "IDENTITY" "TITLE"
    divider
    
    sqlite3 "$DB_FILE" "SELECT name, COALESCE(identity_name, name), COALESCE(identity_title, 'N/A') FROM personas ORDER BY name;" 2>/dev/null | while IFS='|' read -r name identity title; do
        identity="${identity:-$name}"
        title="${title:-N/A}"
        
        local active_marker="  "
        if [[ "$name" == "$active" ]]; then
            active_marker="${COLORS[green]}●${RESET} "
        fi
        
        printf "  %s${COLORS[cyan]}%-12s${RESET} %-29s %-18s\n" \
            "$active_marker" "$name" "${identity:0:26}" "${title:0:16}"
    done
    
    local count db_size
    count=$(sqlite3 "$DB_FILE" "SELECT COUNT(*) FROM personas;" 2>/dev/null || echo "0")
    db_size=$(stat -f%z "$DB_FILE" 2>/dev/null || stat -c%s "$DB_FILE" 2>/dev/null || echo "0")
    
    echo ""
    echo -e "  Database: ${COLORS[dim]}${DB_FILE}${RESET}"
    echo -e "  Size: $(format_bytes "$db_size") | Entries: ${count}"
    echo ""
    echo -e "  ${DIM}Run: mpm persona compile to refresh${RESET}"
}

show_tree() {
    section "📁 Persona Structure"
    echo ""
    echo -e "  ${BOLD}${PERSONAS_DIR}${RESET}"
    
    shopt -s nullglob
    local files=("$PERSONAS_DIR"/*.persona)
    shopt -u nullglob
    
    local count=${#files[@]}
    local idx=0
    
    for file in "${files[@]}"; do
        ((idx++))
        local prefix
        [[ $idx -eq $count ]] && prefix="└── " || prefix="├── "
        
        local name size loaded_icon
        name=$(basename "$file")
        size=$(stat -f%z "$file" 2>/dev/null || stat -c%s "$file" 2>/dev/null || echo "0")
        
        if is_loaded "${name%.persona}"; then
            loaded_icon="${COLORS[green]}✓${RESET}"
        else
            loaded_icon="${COLORS[red]}✗${RESET}"
        fi
        
        printf "  %s%-22s %s (%s)\n" "$prefix" "$name" "$(format_bytes "$size")" "$loaded_icon"
    done
    
    # Database file
    echo "  └── loaded/"
    if [[ -f "$DB_FILE" ]]; then
        local db_size count
        db_size=$(stat -f%z "$DB_FILE" 2>/dev/null || stat -c%s "$DB_FILE" 2>/dev/null || echo "0")
        count=$(sqlite3 "$DB_FILE" "SELECT COUNT(*) FROM personas;" 2>/dev/null || echo "0")
        echo -e "      ├── personas.db (${COLORS[cyan]}$(format_bytes "$db_size")${RESET}, ${COLORS[cyan]}${count}${RESET} personas)"
    else
        echo -e "      ├── personas.db ${COLORS[red]}(not created)${RESET}"
    fi
    echo -e "      └── ${DIM}PERSONA-COMPILER-PATTERN.md${RESET}"
}

show_bytecode() {
    section "🔣 SymAI Bytecode Reference"
    
    local active
    active=$(get_active_persona)
    active="${active:-baseline}"
    
    echo -e "  ${COLORS[green]}${BOLD}Currently Active: ~p.${active}${RESET}"
    echo ""
    
    shopt -s nullglob
    local files=("$PERSONAS_DIR"/*.persona)
    shopt -u nullglob
    
    # Group by category if possible
    local personas=""
    for file in "${files[@]}"; do
        local name
        name=$(basename "$file" .persona)
        [[ "$name" =~ ^[a-zA-Z0-9_-]+$ ]] || continue
        personas+="$name "
    done
    
    [[ -z "$personas" ]] && { echo "No personas found"; return; }
    
    # Format as bytecode shortcuts
    echo -e "  ${BOLD}Load Shortcuts:${RESET}"
    echo ""
    local count=0
    for name in $personas; do
        ((count++))
        local symbol="${COLORS[cyan]}~p.${name}${RESET}"
        local title
        title=$(get_persona_title "$file")
        title="${title:0:32}"
        
        if [[ "$name" == "$active" ]]; then
            printf "    %-18s %-35s [${COLORS[green]}ACTIVE${RESET}]\n" "$symbol" "$title"
        else
            printf "    %-18s %s\n" "$symbol" "$title"
        fi
        
        # New line every 4 for readability
        [[ $((count % 4)) -eq 0 ]] && echo ""
    done
    
    echo ""
    echo ""
    echo -e "  ${BOLD}Universal Commands:${RESET}"
    echo ""
    echo -e "    ${COLORS[yellow]}~p${RESET}              Open interactive persona selector"
    echo -e "    ${COLORS[yellow]}~p!status${RESET}       Show current persona status"
    echo -e "    ${COLORS[yellow]}~p!list${RESET}         This list"
    echo -e "    ${COLORS[yellow]}~p!next${RESET}         Cycle to next persona"
    echo ""
    echo -e "  ${BOLD}Examples:${RESET}"
    echo ""
    echo -e "    ${DIM}~p.coder           Load 'coder' persona${RESET}"
    echo -e "    ${DIM}~p.doc             Switch to documentation mode${RESET}"
    echo -e "    ${DIM}~p.architect       Load architect perspective${RESET}"
}

show_quicklinks() {
    section "⚡ Quick Actions"
    
    echo -e "  ${BOLD}Persona Switching:${RESET}"
    echo ""
    echo -e "    ${COLORS[green]}~p${RESET}                    Interactive selector"
    echo -e "    ${COLORS[green]}~p.<name>${RESET}             Direct load (e.g., ~p.coder)"
    echo -e "    ${COLORS[yellow]}~p!status${RESET}             Show current active persona"
    echo ""
    echo -e "  ${BOLD}Management:${RESET}"
    echo ""
    echo -e "    ${COLORS[blue]}./persona-compile.sh${RESET}      Compile to database"
    echo -e "    ${COLORS[blue]}./persona-dashboard.sh${RESET}   Full dashboard"
    echo -e "    ${COLORS[cyan]}nano <name>.persona${RESET}      Edit persona file"
    echo ""
    echo -e "  ${BOLD}Database Shortcuts:${RESET}"
    echo ""
    echo -e "    ${COLORS[blue]}~p!ls${RESET}                 This listing (--loaded)"
    echo -e "    ${COLORS[blue]}~p!tree${RESET}               Tree view (--tree)"
    echo -e "    ${COLORS[blue]}~p!bc${RESET}                 Show bytecode shortcuts"
}

show_compact() {
    shopt -s nullglob
    local files=("$PERSONAS_DIR"/*.persona)
    shopt -u nullglob
    
    local total=${#files[@]}
    local loaded=0
    local active
    active=$(get_active_persona)
    active="${active:-none}"
    
    for file in "${files[@]}"; do
        local name
        name=$(basename "$file" .persona)
        is_loaded "$name" && ((loaded++))
    done
    
    echo -e "${COLORS[magenta]}[Persona]${RESET} ~p.${active} | ${loaded}/${total} loaded | ${PERSONAS_DIR}"
}

show_all() {
    show_default
    echo ""
    show_loaded
    echo ""
    show_quicklinks
}

# -----------------------------------------------------------------------------
# Help
# -----------------------------------------------------------------------------

show_help() {
    cat <<'EOF'
🎭 MPM Persona LS v4.0

List available personas with SymAI bytecode references.

USAGE:
  ./persona-ls.sh [OPTIONS]

OPTIONS:
  --loaded, -L, ~p!ls  Show database-loaded personas only
  --tree, -t, ~p.t     Tree view with file structure
  --bytecode, -b       Show SymAI bytecode shortcuts (~p.*)
  --all, -a            Show all views combined
  --compact, -c        Single-line status (for prompts)
  --help, -h           Show this help

BYTECODE COMMANDS:
  ~p                   Interactive persona selector
  ~p.<name>            Load specific persona (e.g., ~p.coder)
  ~p!ls                Same as --loaded flag
  ~p!tree              Same as --tree flag
  ~p!bc                Show bytecode reference
  ~p!list              Show available personas
  ~p!status            Show active persona

EXAMPLES:
  ./persona-ls.sh           # Default list view
  ./persona-ls.sh --loaded  # Database-loaded only
  ./persona-ls.sh -b        # Bytecode shortcuts
  ./persona-ls.sh -c | head # Compact for PS1 prompt

RETURN CODES:
  0  Success
  1  Database error or not found
  2  Workspace not found

ENVIRONMENT:
  MPM_WORKSPACE        Path to MPM workspace (highest priority)
  OPENCLAW_WORKSPACE   Alternative workspace path

See also:
  ./persona-dashboard.sh  # Full dashboard with stats
  ./persona-compile.sh    # Compile personas to DB
EOF
}

# -----------------------------------------------------------------------------
# Main
# -----------------------------------------------------------------------------

main() {
    local mode="default"
    local show_quicklinks=true
    
    # Check for bytecode-style args first
    for arg in "$@"; do
        case "$arg" in
            ~p!ls|~p!list)     mode="loaded" ;;
            ~p.t|~p.tree)       mode="tree" ;;
            ~p!bc|~p!bytecode)  mode="bytecode" ;;
            ~p!status)          mode="status" ;;
        esac
    done
    
    # Process standard flags
    for arg in "$@"; do
        case "$arg" in
            --loaded|-L)        mode="loaded" ;;
            --all|-a)           mode="all" ;;
            --tree|-t)          mode="tree" ;;
            --bytecode|-b)      mode="bytecode" ;;
            --compact|-c)       mode="compact" ;;
            --status|-s)        mode="status" ;;
            --no-quicklinks|-n) show_quicklinks=false ;;
            --help|-h)          show_help; exit 0 ;;
            --version|-V)       echo "v$SCRIPT_VERSION"; exit 0 ;;
            ~p!ls|~p!list|~p.t|~p.tree|~p!bc|~p!bytecode|~p!status)
                # Already handled above
                ;;
            *)
                # Ignore unknown args
                ;;
        esac
    done
    
    # Detect workspace
    WORKSPACE=$(detect_workspace)
    [[ -z "$WORKSPACE" ]] && { echo "Error: Workspace not found" >&2; exit 2; }
    
    # Setup paths (support both mpm and MPM)
    if [[ -d "$WORKSPACE/mpm/persona" ]]; then
        PERSONAS_DIR="$WORKSPACE/mpm/persona"
    else
        PERSONAS_DIR="$WORKSPACE/MPM/persona"
    fi
    DB_FILE="$PERSONAS_DIR/personas.db"
    
    # Ensure directory exists
    [[ -d "$PERSONAS_DIR" ]] || { echo "Error: Persona directory not found: $PERSONAS_DIR" >&2; exit 2; }
    
    # Execute mode
    case "$mode" in
        default)
            show_default
            $show_quicklinks && echo "" && show_quicklinks
            ;;
        loaded|status)
            show_loaded
            $show_quicklinks && echo "" && show_quicklinks
            ;;
        tree)
            show_tree
            $show_quicklinks && echo "" && show_quicklinks
            ;;
        bytecode)
            show_bytecode
            $show_quicklinks && echo "" && show_quicklinks
            ;;
        compact)
            show_compact
            ;;
        all)
            show_all
            ;;
        *)
            show_default
            ;;
    esac
}

cd "$(dirname "${BASH_SOURCE[0]}")" || exit 1
main "$@"
