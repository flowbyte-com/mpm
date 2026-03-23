#!/bin/bash
#
# 📖 MPM Help System v4.0 - Comprehensive command reference
# Multi-Persona Manager (MPM) documentation and quick-reference
#
# Usage: ./mpm-help.sh [SECTION] [--search=<term>] [--examples]

set -euo pipefail

# -----------------------------------------------------------------------------
# Configuration
# -----------------------------------------------------------------------------

readonly SCRIPT_VERSION="4.0"
readonly MPM_VERSION="4.0.0"

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
    [bg_green]='\033[42m'
    [bg_blue]='\033[44m'
)

DIM="${COLORS[dim]}"
RESET="${COLORS[reset]}"
BOLD="${COLORS[bold]}"
GREEN="${COLORS[green]}"
YELLOW="${COLORS[yellow]}"
CYAN="${COLORS[cyan]}"
ORANGE="${COLORS[orange]}"
BLUE="${COLORS[blue]}"
MAGNENTA="${COLORS[magenta]}"
GRAY="${COLORS[gray]}"

# -----------------------------------------------------------------------------
# Layout Helpers
# -----------------------------------------------------------------------------

divider() {
    local char="${1:-─}"
    local width="${2:-60}"
    printf "%${width}s\n" "" | tr " " "$char"
}

header() {
    local title="$1"
    local width=60
    local padding=$(((width - ${#title}) / 2))
    echo ""
    echo -e "${GRAY}$(divider "═" $width)${RESET}"
    printf "%*s${BOLD}${ORANGE}%s${RESET}%*s\n" $padding "" "$title" $padding ""
    echo -e "${GRAY}$(divider "═" $width)${RESET}"
}

section() {
    echo ""
    echo -e "${BOLD}${CYAN}▸ $1${RESET}"
    echo -e "${DIM}$(divider "─")${RESET}"
}

subsection() {
    echo -e "  ${BOLD}$1${RESET}"
}

cmd() {
    local name="$1"
    local desc="$2"
    local example="${3:-}"
    
    printf "    ${GREEN}%-20s${RESET} %s\n" "$name" "$desc"
    [[ -n "$example" ]] && printf "    ${DIM}%-20s Example: %s${RESET}\n" "" "$example"
}

bytecode() {
    local symbol="$1"
    local desc="$2"
    local stack="${3:-}"
    
    if [[ -n "$stack" ]]; then
        printf "    ${YELLOW}%-16s${RESET} %-35s ${DIM}[%s]${RESET}\n" "$symbol" "$desc" "$stack"
    else
        printf "    ${YELLOW}%-16s${RESET} %s\n" "$symbol" "$desc"
    fi
}

shortcut() {
    local keys="$1"
    local action="$2"
    printf "    ${CYAN}%-20s${RESET} %s\n" "$keys" "$action"
}

# -----------------------------------------------------------------------------
# Main Help Sections
# -----------------------------------------------------------------------------

show_banner() {
    cat <<'EOF'
    ███╗   ███╗██████╗ ███╗   ███╗
    ████╗ ████║██╔══██╗████╗ ████║
    ██╔████╔██║██████╔╝██╔████╔██║
    ██║╚██╔╝██║██╔═══╝ ██║╚██╔╝██║
    ██║ ╚═╝ ██║██║     ██║ ╚═╝ ██║
    ╚═╝     ╚═╝╚═╝     ╚═╝     ╚═╝
EOF
    echo ""
    echo -e "    ${BOLD}Multi-Persona Manager${RESET} ${DIM}v${MPM_VERSION}${RESET}"
    echo -e "    ${GRAY}SymAI Bytecode Stack System${RESET}"
    echo ""
}

show_quickstart() {
    header "🚀 QUICK START"
    
    section "1. Select a Persona"
    echo ""
    echo -e "    ${DIM}View available personas:${RESET}"
    echo -e "      ${GREEN}./persona-ls.sh${RESET}          ${DIM}# or ~p?${RESET}"
    echo ""
    echo -e "    ${DIM}Activate a persona:${RESET}"
    echo -e "      ${YELLOW}~p.developer${RESET}           ${DIM}# Load 'developer' persona${RESET}"
    echo -e "      ${YELLOW}~p${RESET}                     ${DIM}# Interactive selector${RESET}"
    echo ""
    
    section "2. Stack Your Modes"
    echo ""
    echo -e "    ${DIM}View available modes:${RESET}"
    echo -e "      ${GREEN}./mode-ls.sh${RESET}           ${DIM}# or ~m?${RESET}"
    echo ""
    echo -e "    ${DIM}Activate multiple modes:${RESET}"
    echo -e "      ${YELLOW}~m.code${RESET}                ${DIM}# Load 'code' mode${RESET}"
    echo -e "      ${YELLOW}~m +security${RESET}          ${DIM}# Add 'security' to stack${RESET}"
    echo -e "      ${YELLOW}~m.code ~m+security${RESET}    ${DIM}# Combined: code + security${RESET}"
    echo ""
    
    section "3. Check System Health"
    echo ""
    echo -e "    ${DIM}Quick health check:${RESET}"
    echo -e "      ${GREEN}./medic-ls.sh${RESET}          ${DIM}# or ~h?${RESET}"
    echo -e "      ${YELLOW}~h.run${RESET}                ${DIM}# Run self-test${RESET}"
    echo ""
    echo -e "    ${DIM}Full dashboard:${RESET}"
    echo -e "      ${GREEN}./medic-dashboard.sh${RESET}   ${DIM}# System overview${RESET}"
    echo ""
    
    section "4. Monitor Status"
    echo ""
    echo -e "    ${DIM}Your current session:${RESET}"
    echo -e "      ${YELLOW}~p.">persona_name"${RESET}${DIM}..."${RESET}${YELLOW}" ~m.">mode_stack"${RESET}"
    echo ""
    echo -e "    ${GRAY}Pro tip: Use ~? to see this help anytime${RESET}"
}

show_persona_commands() {
    header "👤 PERSONA COMMANDS (~p)"
    
    echo ""
    echo -e "  ${DIM}Personas define who you are - coding style, expertise, tone${RESET}"
    echo ""
    
    section "Core Commands"
    bytecode "~p" "Interactive persona selector" ""
    bytecode "~p.<name>" "Load specific persona" "~p.dev"
    bytecode "~p!list, ~p?" "List all personas" ""
    bytecode "~p!stat" "Show active persona details" ""
    bytecode "~p!switch <n>" "Switch to persona by number" ""
    bytecode "~p.clr" "Clear/reset persona" ""
    
    section "Quick Examples"
    echo ""
    echo -e "    ${DIM}Developer workflow:${RESET}"
    echo -e "      ${YELLOW}~p.developer ~m.code ~m+debug${RESET}"
    echo ""
    echo -e "    ${DIM}Documentation mode:${RESET}"
    echo -e "      ${YELLOW}~p.writer ~m.doc ~m+verbose${RESET}"
    echo ""
    echo -e "    ${DIM}Security audit:${RESET}"
    echo -e "      ${YELLOW}~p.security ~m.code ~m+security ~m+strict${RESET}"
    
    section "Management Scripts"
    cmd "./persona-ls.sh""List available personas" "./persona-ls.sh -a"
    cmd "./persona-dashboard.sh" "Full persona dashboard" "./persona-dashboard.sh"
    cmd "./persona-compile.sh" "Compile personas to DB" "./persona-compile.sh"
}

show_mode_commands() {
    header "🛠️ MODE COMMANDS (~m)"
    
    echo ""
    echo -e "  ${DIM}Modes define how you work - optimization levels, features, behaviors${RESET}"
    echo -e "  ${DIM}Modes STACK! You can combine up to 5 modes simultaneously${RESET}"
    echo ""
    
    section "Stack Operations"
    bytecode "~m" "Interactive mode selector" ""
    bytecode "~m.<name>" "Load single mode (replaces stack)" "~m.code"
    bytecode "~m+<name>" "Add mode to stack (keeps existing)" "~m+security"
    bytecode "~m-<name>" "Remove mode from stack" "~m-verbose"
    bytecode "~m.clr" "Clear entire stack" ""
    bytecode "~m.pop" "Remove last mode from stack" ""
    
    section "Query Commands"
    bytecode "~m?, ~m!list" "List all modes (this view)" ""
    bytecode "~m?l, ~m.loaded" "Show database-loaded modes" ""
    bytecode "~m?+, ~m.act" "Show active stack" ""
    bytecode "~m?t, ~m.tree" "Show file tree view" ""
    
    section "Common Mode Combinations"
    echo ""
    shortcut "~m.code ~m+debug" "Coding with debug output"
    shortcut "~m.code ~m+security" "Code review + security audit"
    shortcut "~m.doc ~m+verbose" "Documentation with full details"
    shortcut "~m.doc ~m+concise" "Documentation, brief style"
    shortcut "~m.base ~m+strict" "Strict validation mode"
    shortcut "~m.analyze ~m+deep" "Deep analysis mode"
    
    section "Management Scripts"
    cmd "./mode-ls.sh" "List available modes" "./mode-ls.sh --bytecode"
    cmd "./mode-dashboard.sh" "Full mode dashboard" "./mode-dashboard.sh"
    cmd "./mode-compile.sh" "Compile modes to DB" "./mode-compile.sh"
}

show_medic_commands() {
    header "🔍 MEDIC COMMANDS (~h)"
    
    echo ""
    echo -e "  ${DIM}Medic monitors system health - files, symlinks, integrity${RESET}"
    echo ""
    
    section "Health Check Commands"
    bytecode "~h?, ~h!list" "List medic components" ""
    bytecode "~h.run" "Run full health diagnostic" ""
    bytecode "~h.quick" "Quick health check" ""
    bytecode "~h.report" "Generate health report" ""
    bytecode "~h.fix" "Auto-fix common issues" ""
    
    section "Component Checks"
    bytecode "~h.links" "Check symlink integrity" ""
    bytecode "~h.files" "Verify file structure" ""
    bytecode "~h.db" "Check database health" ""
    bytecode "~h.patterns" "Validate pattern files" ""
    
    section "Management Scripts"
    cmd "./medic-ls.sh" "List medic components" "./medic-ls.sh --health"
    cmd "./medic-dashboard.sh" "Full health dashboard" "./medic-dashboard.sh"
    cmd "./medic.sh --health" "Run health checks" "./medic.sh --health --fix"
}

show_bytecode_reference() {
    header "🔣 SYMANTIC BYTECODE REFERENCE"
    
    echo ""
    echo -e "  ${DIM}SymAI uses compact bytecode syntax for rapid context switching${RESET}"
    echo ""
    
    section "Notation Guide"
    echo ""
    echo -e "    ${BOLD}~p${RESET}    = Persona prefix"
    echo -e "    ${BOLD}~m${RESET}    = Mode prefix"
    echo -e "    ${BOLD}~h${RESET}    = Health/Medic prefix"
    echo -e "    ${BOLD}.${RESET}     = Select/activate"
    echo -e "    ${BOLD}+${RESET}     = Add to stack"
    echo -e "    ${BOLD}-${RESET}     = Remove from stack"
    echo -e "    ${BOLD}!${RESET}     = Action verb"
    echo -e "    ${BOLD}?${RESET}     = Query/inspect"
    
    section "Complete Syntax"
    echo ""
    echo -e "    ${DIM}A typical session might look like:${RESET}"
    echo ""
    echo -e "    ${YELLOW}~p.developer ~m.code ~m+security ~h.run${RESET}"
    echo ""
    echo -e "    ${DIM}Translation:${RESET}"
    echo -e "      • Load 'developer' persona"
    echo -e "      • Activate 'code' mode"
    echo -e "      • Add 'security' mode to stack"
    echo -e "      • Run health check"
    echo ""
    echo -e "    ${DIM}Combined bytecode representation:${RESET}"
    echo -e "    ${CYAN}~p.developer ~m.code&security ~h.run${RESET}"
    
    section "Special Symbols"
    bytecode "." "Dot access (select specific)" "~p.dev"
    bytecode "&" "Stack combination" "~m.code&security"
    bytecode "*" "Wildcard/all" "~m.*"
    bytecode "!" "Execute action" "~h!run"
}

show_scripts_reference() {
    header "📁 SCRIPT REFERENCE"
    
    section "Core Scripts"
    echo ""
    cmd "mpm" "Multi-Persona Manager (CLI entry)" "mpm --help"
    cmd "mpm-ls" "List all MPM components" "mpm-ls --all"
    cmd "mpm-init" "Initialize new persona/mode" "mpm-init persona <name>"
    
    section "Persona Scripts"
    cmd "persona-ls.sh" "List personas" "./persona-ls.sh -a"
    cmd "persona-dashboard.sh" "Persona dashboard" "./persona-dashboard.sh"
    cmd "persona-compile.sh" "Compile to DB" "./persona-compile.sh"
    
    section "Mode Scripts"
    cmd "mode-ls.sh" "List modes" "./mode-ls.sh --bytecode"
    cmd "mode-dashboard.sh" "Mode dashboard" "./mode-dashboard.sh"
    cmd "mode-compile.sh" "Compile to DB" "./mode-compile.sh"
    
    section "Medic Scripts"
    cmd "medic-ls.sh" "List components" "./medic-ls.sh --health"
    cmd "medic-dashboard.sh" "Health dashboard" "./medic-dashboard.sh"
    cmd "medic.sh" "Run medic checks" "./medic.sh --health --fix"
}

show_diagnostics() {
    header "🔧 DIAGNOSTICS & TROUBLESHOOTING"
    
    section "Common Issues"
    echo ""
    echo -e "    ${BOLD}Issue:${RESET} Command not found"
    echo -e "    ${GREEN}Fix:${RESET}   Ensure scripts are executable: ${DIM}chmod +x *.sh${RESET}"
    echo ""
    echo -e "    ${BOLD}Issue:${RESET} Database not found"
    echo -e "    ${GREEN}Fix:${RESET}   Run compile script: ${DIM}./mode-compile.sh${RESET}"
    echo ""
    echo -e "    ${BOLD}Issue:${RESET} Symlinks broken"
    echo -e "    ${GREEN}Fix:${RESET}   Run: ${DIM}./medic.sh --fix${RESET}"
    echo ""
    echo -e "    ${BOLD}Issue:${RESET} Workspace not detected"
    echo -e "    ${GREEN}Fix:${RESET}   Set ${DIM}MPM_WORKSPACE=/path/to/workspace${RESET}"
    
    section "Debug Commands"
    cmd "./medic.sh --verbose" "Verbose health check" ""
    cmd "./medic.sh --diagnose" "Full diagnostic" ""
    cmd "sqlite3 m3.db '.tables'" "Check DB tables" ""
    
    section "Environment Variables"
    echo ""
    echo -e "    ${BOLD}MPM_WORKSPACE${RESET}      Path to workspace"
    echo -e "    ${BOLD}MPM_PERSONA${RESET}      Default persona"
    echo -e "    ${BOLD}MPM_MODE${RESET}          Default mode(s)"
    echo -e "    ${BOLD}MPM_VERBOSE${RESET}      Enable verbose output"
}

show_compact() {
    echo -e "${BOLD}MPM v${MPM_VERSION}${RESET} - Multi-Persona Manager"
    echo ""
    echo -e "${CYAN}Persona:${RESET}  ~p, ~p.<name>, ~p!, ~p?"
    echo -e "${CYAN}Mode:${RESET}     ~m, ~m.<name>, ~m+name, ~m.clr"
    echo -e "${CYAN}Medic:${RESET}    ~h, ~h.run, ~h.fix"
    echo -e "${CYAN}Help:${RESET}     ~?, ./mpm-help.sh"
    echo ""
    echo -e "${DIM}Run ./mpm-help.sh --all for full reference${RESET}"
}

show_all() {
    show_banner
    show_quickstart
    show_persona_commands
    show_mode_commands
    show_medic_commands
    show_bytecode_reference
    show_scripts_reference
    show_diagnostics
    
    echo ""
    echo -e "${GRAY}$(divider "═" 60)${RESET}"
    echo -e "${BOLD}End of MPM Help Reference${RESET}"
    echo -e "${GRAY}$(divider "═" 60)${RESET}"
    echo ""
    echo -e "${GRAY}For more: https://github.com/sipeed/picoclaw${RESET}"
}

show_version() {
    echo "MPM (Multi-Persona Manager) v${MPM_VERSION}"
    echo "Help System v${SCRIPT_VERSION}"
    echo ""
    echo "Copyright (c) 2024-2026 Sipeed"
    echo "License: MIT"
}

show_usage() {
    cat <<EOF
Usage: ./mpm-help.sh [SECTION] [OPTIONS]

SECTIONS:
  --quickstart, -q      Quick start guide
  --persona, -p         Persona commands (~p)
  --mode, -m            Mode commands (~m)
  --medic, -h           Medic/health commands (~h)
  --bytecode, -b        Bytecode syntax reference
  --scripts, -s         Script reference
  --diagnostics, -d      Troubleshooting guide
  --all, -a             Show everything
  --compact, -c         Compact summary
  --version, -V         Show version
  --help                This help

OPTIONS:
  --search=<term>       Search for specific term
  --examples            Show usage examples

QUICK ACCESS:
  ~?                    Show quick help
  ~p?                   Persona help
  ~m?                   Mode help
  ~h?                   Medic help

EXAMPLES:
  ./mpm-help.sh           # Default quickstart
  ./mpm-help.sh -a        # Full reference
  ./mpm-help.sh -c        # Compact for prompts
  ./mpm-help.sh -m -e     # Mode commands + examples
EOF
}

# -----------------------------------------------------------------------------
# Main Entry Point
# -----------------------------------------------------------------------------

main() {
    local section="quickstart"
    local show_examples=false
    local search_term=""
    
    # Check for SymAI-style shorthand
    for arg in "$@"; do
        case "$arg" in
            \~\?|~?) section="quickstart" ;;
            \~p\?|~p?) section="persona" ;;
            \~m\?|~m?) section="mode" ;;
            \~h\?|~h?) section="medic" ;;
        esac
    done
    
    # Process arguments
    for arg in "$@"; do
        case "$arg" in
            --all|-a)           section="all" ;;
            --quickstart|-q)    section="quickstart" ;;
            --persona|-p)       section="persona" ;;
            --mode|-m)          section="mode" ;;
            --medic|-h)         section="medic" ;;
            --bytecode|-b)      section="bytecode" ;;
            --scripts|-s)       section="scripts" ;;
            --diagnostics|-d)   section="diagnostics" ;;
            --compact|-c)       section="compact" ;;
            --version|-V)       show_version; exit 0 ;;
            --help)             show_usage; exit 0 ;;
            --examples|-e)      show_examples=true ;;
            --search=*)         search_term="${arg#*=}" ;;
            \~\?|\~p\?|\~m\?|\~h\?)
                # SymAI shortcuts handled above
                ;;
            *)
                # Try to match section name
                case "$arg" in
                    persona|personas|p) section="persona" ;;
                    mode|modes|m)       section="mode" ;;
                    medic|health|h)    section="medic" ;;
                    bytecode|b)        section="bytecode" ;;
                    scripts|script)    section="scripts" ;;
                    diagnostic*)       section="diagnostics" ;;
                esac
                ;;
        esac
    done
    
    # Display selected section
    case "$section" in
        quickstart)
            show_banner
            show_quickstart
            echo ""
            echo -e "${GRAY}Tip: Use --all for complete reference${RESET}"
            ;;
        persona)    show_banner; show_persona_commands ;;
        mode)       show_banner; show_mode_commands ;;
        medic)      show_banner; show_medic_commands ;;
        bytecode)   show_banner; show_bytecode_reference ;;
        scripts)    show_scripts_reference ;;
        diagnostics) show_diagnostics ;;
        compact)    show_compact ;;
        all)        show_all ;;
        *)
            echo "Unknown section: $section"
            show_usage
            exit 1
            ;;
    esac
    
    # Add examples if requested
    if $show_examples; then
        echo ""
        section "Usage Examples"
        echo ""
        echo -e "    ${DIM}$ cat ~/.bashrc | grep MPM${RESET}"
        echo -e "    ${YELLOW}alias ~p='./persona-selector.sh'${RESET}"
        echo -e "    ${YELLOW}alias ~m='./mode-selector.sh'${RESET}"
        echo -e "    ${YELLOW}alias ~h='./medic-ls.sh'${RESET}"
        echo ""
        echo -e "    ${DIM}$ ~p && ~m.code ~m+debug${RESET}"
        echo -e "    ${YELLOW}[persona: developer] [mode: code&debug]${RESET}"
    fi
    
    # Handle search if provided
    if [[ -n "$search_term" ]]; then
        echo ""
        section "Search Results for: $search_term"
        echo ""
        echo -e "    ${YELLOW}Searching...${RESET}"
        echo -e "    ${DIM}(In a real implementation, this would grep through help content)${RESET}"
    fi
}

cd "$(dirname "${BASH_SOURCE[0]}")" 2>/dev/null || true
main "$@"
