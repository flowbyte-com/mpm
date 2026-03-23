#!/bin/bash
#
# 🛠️ MPM Mode LS v4.0 - List modes with SymAI bytecode stack references
# Displays available modes, stacked status, and bytecode shortcuts
#
# Usage: ./mode-ls.sh [--loaded|--db] [--active] [--tree] [--compact] [--bytecode]

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
section() { echo -e "\n${BOLD}${COLORS[orange]}$1${RESET}"; divider; }

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
    count=$(sqlite3 "$DB_FILE" "SELECT COUNT(*) FROM mode_files WHERE name='$name';" 2>/dev/null || echo "0")
    [[ "$count" -gt 0 ]]
}

is_active_in_stack() {
    local name="$1"
    [[ -f "$DB_FILE" ]] || return 1
    
    local count
    count=$(sqlite3 "$DB_FILE" "SELECT COUNT(*) FROM active_modes WHERE name='$name';" 2>/dev/null || echo "0")
    [[ "$count" -gt 0 ]]
}

get_stack_order() {
    local name="$1"
    [[ -f "$DB_FILE" ]] || return 1
    
    sqlite3 "$DB_FILE" "SELECT stack_order FROM active_modes WHERE name='$name';" 2>/dev/null || echo ""
}

get_active_stack() {
    [[ -f "$DB_FILE" ]] || return 1
    sqlite3 "$DB_FILE" "SELECT name FROM active_modes ORDER BY stack_order;" 2>/dev/null | paste -sd "&" - | sed 's/&$//' || echo ""
}

get_mode_purpose() {
    local file="$1"
    local purpose=""
    
    if [[ -f "$file" ]]; then
        purpose=$(head -30 "$file" 2>/dev/null | grep -A2 '^## Purpose' | tail -1 | sed 's/^## Purpose //; s/^[[:space:]]*//' || echo "")
    fi
    echo "${purpose:-N/A}"
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

# -----------------------------------------------------------------------------
# Display Modes
# -----------------------------------------------------------------------------

show_default() {
    section "🛠️ Available Modes"
    
    local stack_str
    stack_str=$(get_active_stack)
    stack_str="${stack_str:-}"
    
    # Header
    printf "  ${BOLD}%-3s %-4s %-16s %-30s %-8s${RESET}\n" "" "LVL" "NAME" "PURPOSE" "STATUS"
    divider
    
    shopt -s nullglob
    local files=("$MODES_DIR"/*.mode)
    shopt -u nullglob
    
    [[ ${#files[@]} -eq 0 ]] && { echo "No modes found in $MODES_DIR"; return; }
    
    for file in "${files[@]}"; do
        local name
        name=$(basename "$file" .mode)
        
        # Skip invalid names
        [[ "$name" =~ ^[a-zA-Z0-9_-]+$ ]] || continue
        
        local purpose
        purpose=$(get_mode_purpose "$file")
        purpose="${purpose:0:28}"
        
        local level="-"
        local in_stack="${COLORS[red]}○${RESET}"
        local loaded="${COLORS[red]}✗${RESET}"
        
        if is_loaded "$name"; then
            loaded="${COLORS[green]}✓${RESET}"
        fi
        
        if is_active_in_stack "$name"; then
            local order
            order=$(get_stack_order "$name")
            level="${order}"
            in_stack="${COLORS[green]}●${RESET}"
        fi
        
        printf "  %s${COLORS[dim]}%-4s${RESET} ${COLORS[cyan]}%-16s${RESET} %-30s %s/%s\n" \
            "$in_stack" "$level" "$name" "$purpose" "$loaded" "$loaded"
    done
    
    local count=${#files[@]}
    local active_count=0
    [[ -f "$DB_FILE" ]] && active_count=$(sqlite3 "$DB_FILE" "SELECT COUNT(*) FROM active_modes;" 2>/dev/null || echo "0")
    
    echo ""
    echo -e "  Total: ${COLORS[cyan]}${count}${RESET} modes | Stack: ${COLORS[green]}${active_count}${RESET}/5 | ${COLORS[green]}●${RESET}=in stack ${COLORS[green]}✓${RESET}=in DB"
}

show_loaded() {
    section "🛠️ Loaded Modes (Database)"
    
    if [[ ! -f "$DB_FILE" ]]; then
        echo -e "  ${COLORS[yellow]}⚠ No database found${RESET}"
        echo "  Run: ./mode-compile.sh to initialize"
        return 1
    fi
    
    # Header
    printf "  ${BOLD}%-16s %-40s %-15s${RESET}\n" "NAME" "PURPOSE" "BYTECODE"
    divider
    
    sqlite3 "$DB_FILE" "SELECT name, COALESCE(purpose, 'N/A'), COALESCE(bytecode_signature, '~m.'||name) FROM modes ORDER BY name;" 2>/dev/null | while IFS='|' read -r name purpose bytecode; do
        purpose="${purpose:0:38}"
        bytecode="${bytecode:-~m.$name}"
        
        printf "  ${COLORS[cyan]}%-16s${RESET} %-40s ${COLORS[dim]}%-15s${RESET}\n" \
            "$name" "$purpose" "$bytecode"
    done
    
    local count db_size
    count=$(sqlite3 "$DB_FILE" "SELECT COUNT(*) FROM modes;" 2>/dev/null || echo "0")
    db_size=$(stat -f%z "$DB_FILE" 2>/dev/null || stat -c%s "$DB_FILE" 2>/dev/null || echo "0")
    
    echo ""
    echo -e "  Database: ${COLORS[dim]}${DB_FILE}${RESET}"
    echo -e "  Size: $(format_bytes "$db_size") | Entries: ${count}"
    echo ""
    echo -e "  ${DIM}Run: mpm mode compile to refresh${RESET}"
}

show_active() {
    section "🛠️ Active Mode Stack"
    
    if [[ ! -f "$DB_FILE" ]]; then
        echo -e "  ${COLORS[yellow]}⚠ No database found${RESET}"
        return 1
    fi
    
    local count
    count=$(sqlite3 "$DB_FILE" "SELECT COUNT(*) FROM active_modes;" 2>/dev/null || echo "0")
    
    if [[ "$count" -eq 0 ]]; then
        echo -e "  ${COLORS[yellow]}⚪ No active modes${RESET}"
        echo ""
        echo -e "  Use ${COLORS[green]}~m${RESET} or ${COLORS[green]}~m+<name>${RESET} to activate modes"
        echo ""
        echo -e "  ${DIM}The system runs in ~m.base (default) mode when no modes are active${RESET}"
        return 0
    fi
    
    # Show stack depth
    echo -e "  Stack Depth: $(stack_indicator "$count") ${count}/5"
    echo ""
    
    # Header
    printf "  ${BOLD}%-5s %-16s %-30s %-15s${RESET}\n" "ORDER" "NAME" "PURPOSE" "SYMBOL"
    divider
    
    sqlite3 "$DB_FILE" "SELECT am.stack_order, am.name, COALESCE(m.purpose, 'N/A'), COALESCE(m.bytecode_signature, '~m.'||am.name) FROM active_modes am LEFT JOIN modes m ON am.name = m.name ORDER BY am.stack_order;" 2>/dev/null | while IFS='|' read -r order name purpose bytecode; do
        purpose="${purpose:0:28}"
        bytecode="${bytecode:-~m.$name}"
        
        local indicator=""
        for ((i=1; i<=order; i++)); do indicator+="▲"; done
        
        printf "  ${COLORS[green]}%-3d${RESET}   ${COLORS[cyan]}%-16s${RESET} %-30s ${COLORS[dim]}%-15s${RESET}\n" \
            "$((order+1))" "$name" "$purpose" "$bytecode"
    done
    
    echo ""
    echo -e "  Active bytecode: ${COLORS[green]}~m.${stack_str}${RESET}"
    echo ""
    echo -e "  Commands: ${COLORS[yellow]}~m.clr${RESET} (clear) | ${COLORS[red]}~m.pop${RESET} (remove last)"
}

show_tree() {
    section "📁 Mode Structure"
    echo ""
    echo -e "  ${BOLD}${MODES_DIR}${RESET}"
    
    shopt -s nullglob
    local files=("$MODES_DIR"/*.mode)
    shopt -u nullglob
    
    local count=${#files[@]}
    local idx=0
    
    for file in "${files[@]}"; do
        ((idx++))
        local prefix
        [[ $idx -eq $count ]] && prefix="└── " || prefix="├── "
        
        local name size loaded active_icon
        name=$(basename "$file")
        local mode_name="${name%.mode}"
        size=$(stat -f%z "$file" 2>/dev/null || stat -c%s "$file" 2>/dev/null || echo "0")
        
        if is_active_in_stack "$mode_name"; then
            active_icon="${COLORS[green]}🟢${RESET}"
        elif is_loaded "$mode_name"; then
            loaded="${COLORS[green]}✓${RESET}"
            active_icon="${COLORS[dim]}○${RESET}"
        else
            loaded="${COLORS[red]}✗${RESET}"
            active_icon="${COLORS[dim]}○${RESET}"
        fi
        
        printf "  %s%-22s %s (%s %s %s)\n" "$prefix" "$name" "$(format_bytes "$size")" "$active_icon" "$loaded"
    done
    
    # Database file
    echo "  └── loaded/"
    if [[ -f "$DB_FILE" ]]; then
        local db_size count active_count
        db_size=$(stat -f%z "$DB_FILE" 2>/dev/null || stat -c%s "$DB_FILE" 2>/dev/null || echo "0")
        count=$(sqlite3 "$DB_FILE" "SELECT COUNT(*) FROM modes;" 2>/dev/null || echo "0")
        active_count=$(sqlite3 "$DB_FILE" "SELECT COUNT(*) FROM active_modes;" 2>/dev/null || echo "0")
        echo -e "      ├── m3.db (${COLORS[cyan]}$(format_bytes "$db_size")${RESET}, ${COLORS[cyan]}${count}${RESET} modes, ${COLORS[green]}${active_count}${RESET} active)"
    else
        echo -e "      ├── m3.db ${COLORS[red]}(not created)${RESET}"
    fi
    echo -e "      └── ${DIM}MODE-PATTERN.md${RESET}"
}

show_bytecode() {
    section "🔣 SymAI Mode Bytecode Reference"
    
    local stack_str
    stack_str=$(get_active_stack)
    
    if [[ -n "$stack_str" ]]; then
        echo -e "  ${COLORS[green]}${BOLD}Currently Active: ~m.${stack_str}${RESET}"
    else
        echo -e "  ${COLORS[yellow]}${BOLD}Currently Active: ~m.base${RESET} (default)"
    fi
    echo ""
    
    shopt -s nullglob
    local files=("$MODES_DIR"/*.mode)
    shopt -u nullglob
    
    # Build list
    local modes=""
    for file in "${files[@]}"; do
        local name
        name=$(basename "$file" .mode)
        [[ "$name" =~ ^[a-zA-Z0-9_-]+$ ]] || continue
        modes+="$name "
    done
    
    [[ -z "$modes" ]] && { echo "No modes found"; return; }
    
    # Load Shortcuts
    echo -e "  ${BOLD}Load Shortcuts (${COLORS[dim]}~m.<name>${RESET}):${RESET}"
    echo ""
    
    local count=0
    for name in $modes; do
        ((count++))
        local symbol="${COLORS[cyan]}~m.${name}${RESET}"
        local purpose
        purpose=$(get_mode_purpose "$file")
        purpose="${purpose:0:35}"
        
        if is_active_in_stack "$name"; then
            printf "    %-18s %-36s [${COLORS[green]}ACTIVE${RESET}]\n" "$symbol" "$purpose"
        else
            printf "    %-18s %s\n" "$symbol" "$purpose"
        fi
    done
    
    echo ""
    echo ""
    echo -e "  ${BOLD}Stack Operators:${RESET}"
    echo ""
    echo -e "    ${COLORS[yellow]}~m${RESET}              Interactive mode selector"
    echo -e "    ${COLORS[yellow]}~m+<name>${RESET}        Add mode to stack (e.g., ~m+debug)"
    echo -e "    ${COLORS[yellow]}~m-<name>${RESET}        Remove mode from stack"
    echo -e "    ${COLORS[yellow]}~m.clr${RESET}          Clear entire stack"
    echo -e "    ${COLORS[yellow]}~m.pop${RESET}          Remove last mode from stack"
    echo ""
    echo -e "  ${BOLD}Query Commands:${RESET}"
    echo ""
    echo -e "    ${COLORS[blue]}~m?${RESET}, ${COLORS[blue]}~m!list${RESET}       This listing"
    echo -e "    ${COLORS[blue]}~m?l${RESET}             Loaded modes only"
    echo -e "    ${COLORS[blue]}~m?+${RESET}             Active stack view"
    echo -e "    ${COLORS[blue]}~m.act${RESET}           Same as ~m?+"
    echo ""
    echo -e "  ${BOLD}Examples:${RESET}"
    echo ""
    echo -e "    ${DIM}~m.code              Load coding optimization mode${RESET}"
    echo -e "    ${DIM}~m.code ~m+security  Stack code + security modes${RESET}"
    echo -e "    ${DIM}~m.doc ~m+verbose    Documentation with verbose output${RESET}"
    echo -e "    ${DIM}~m.clr ~m.debug      Clear, then load debug mode${RESET}"
}

show_quicklinks() {
    section "⚡ Quick Commands"
    
    echo -e "  ${BOLD}Mode Switching:${RESET}"
    echo ""
    echo -e "    ${COLORS[green]}~m${RESET}                    Interactive selector"
    echo -e "    ${COLORS[green]}~m.<name>${RESET}              Load single mode"
    echo -e "    ${COLORS[green]}~m+<name>${RESET}              Add to stack"
    echo -e "    ${COLORS[yellow]}~m.clr${RESET}                 Clear all modes"
    echo -e "    ${COLORS[yellow]}~m.pop${RESET}                 Remove last"
    echo ""
    echo -e "  ${BOLD}Stack Examples:${RESET}"
    echo ""
    echo -e "    ${DIM}~m.code ~m+security    = code + security${RESET}"
    echo -e "    ${DIM}~m.doc ~m+verbose      = documentation + verbose${RESET}"
    echo ""
    echo -e "  ${BOLD}Management:${RESET}"
    echo ""
    echo -e "    ${COLORS[blue]}./mode-compile.sh${RESET}      Compile modes to DB"
    echo -e "    ${COLORS[blue]}./mode-dashboard.sh${RESET}   Full dashboard"
    echo -e "    ${COLORS[cyan]}nano <name>.mode${RESET}       Edit mode file"
}

show_compact() {
    shopt -s nullglob
    local files=("$MODES_DIR"/*.mode)
    shopt -u nullglob
    
    local total=${#files[@]}
    local loaded=0
    local active=0
    local stack_info=""
    
    for file in "${files[@]}"; do
        local name
        name=$(basename "$file" .mode)
        is_loaded "$name" && ((loaded++))
        is_active_in_stack "$name" && ((active++))
    done
    
    local stack_str
    stack_str=$(get_active_stack)
    stack_str="${stack_str:-base}"
    
    [[ $active -gt 0 ]] && stack_info="[${active} modes]"
    
    echo -e "${COLORS[orange]}[Mode]${RESET} ~m.${stack_str} ${stack_info} | ${loaded}/${total} loaded | ${MODES_DIR}"
}

show_all() {
    show_default
    echo ""
    show_active
    echo ""
    show_quicklinks
}

# -----------------------------------------------------------------------------
# Help
# -----------------------------------------------------------------------------

show_help() {
    cat <<'EOF'
🛠️ MPM Mode LS v4.0

List available modes with SymAI bytecode stack references.

USAGE:
  ./mode-ls.sh [OPTIONS]

OPTIONS:
  --loaded, -L, ~m?l      Show database-loaded modes only
  --active, -a, ~m?+       Show currently active stack
  --tree, -t, ~m?t         Tree view with file structure
  --bytecode, -b           Show SymAI bytecode shortcuts
  --all                    Show all views combined
  --compact, -c            Single-line status (for prompts)
  --help, -h               Show this help

BYTECODE COMMANDS:
  ~m                       Interactive mode selector
  ~m.<name>                Load single mode
  ~m+<name>                Add mode to stack
  ~m-<name>                Remove mode from stack
  ~m.clr                   Clear entire stack
  ~m.pop                   Remove last mode
  ~m?                      This listing
  ~m?l                     Loaded modes (same as --loaded)
  ~m?+                     Active stack (same as --active)

MODE STACKING:
  Modes stack! You can activate multiple modes simultaneously:
    ~m.code ~m+security    = coding mode + security audit
    ~m.doc ~m+verbose      = documentation + verbose logging
  Stack depth: up to 5 modes

EXAMPLES:
  ./mode-ls.sh              # Default list view
  ./mode-ls.sh --active     # Show active stack
  ./mode-ls.sh -b           # Bytecode shortcuts
  ./mode-ls.sh -c | head    # Compact for PS1

RETURN CODES:
  0  Success
  1  Database error
  2  Workspace not found

ENVIRONMENT:
  MPM_WORKSPACE            Path to MPM workspace

See also:
  ./mode-dashboard.sh      # Full dashboard with stats
  ./mode-compile.sh        # Compile modes to DB
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
            ~m?l)
                mode="loaded"
                ;;
            ~m?+|~m.act)
                mode="active"
                ;;
            ~m?t)
                mode="tree"
                ;;
            ~m?|~m!list)
                mode="default"
                ;;
        esac
    done
    
    # Process standard flags
    for arg in "$@"; do
        case "$arg" in
            --loaded|-L)        mode="loaded" ;;
            --active|-a)        mode="active" ;;
            --tree|-t)          mode="tree" ;;
            --bytecode|-b)      mode="bytecode" ;;
            --compact|-c)       mode="compact" ;;
            --all)              mode="all" ;;
            --no-quicklinks|-n) show_quicklinks=false ;;
            --help|-h)          show_help; exit 0 ;;
            --version|-V)       echo "v$SCRIPT_VERSION"; exit 0 ;;
            ~m?l|~m?+|~m.act|~m?t|~m?|~m!list)
                # Already handled above
                ;;
            *)
                # Ignore unknown
                ;;
        esac
    done
    
    # Detect workspace
    WORKSPACE=$(detect_workspace)
    [[ -z "$WORKSPACE" ]] && { echo "Error: Workspace not found" >&2; exit 2; }
    
    # Setup paths
    if [[ -d "$WORKSPACE/mpm/mode" ]]; then
        MODES_DIR="$WORKSPACE/mpm/mode"
    else
        MODES_DIR="$WORKSPACE/MPM/mode"
    fi
    DB_FILE="$MODES_DIR/m3.db"
    
    # Ensure directory exists
    [[ -d "$MODES_DIR" ]] || { echo "Error: Mode directory not found: $MODES_DIR" >&2; exit 2; }
    
    # Execute mode
    case "$mode" in
        default)
            show_default
            $show_quicklinks && echo "" && show_quicklinks
            ;;
        loaded)
            show_loaded
            $show_quicklinks && echo "" && show_quicklinks
            ;;
        active)
            show_active
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
