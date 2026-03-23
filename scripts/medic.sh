#!/bin/bash
#
# 🏥 MPM Medic v3.3 - Memory Cleanup & Health Diagnostic Tool
# Automated cleanup with smart archiving and health checks
#
# Usage: ./medic.sh [--consolidate] [--archive] [--cleanup] [--dry-run] [--health]

set -euo pipefail

# -----------------------------------------------------------------------------
# Configuration
# -----------------------------------------------------------------------------

readonly SCRIPT_VERSION="3.3"
readonly CONFIG_DIRS=(
    "${OPENCLAW_WORKSPACE:-}"
    "${PICOLCLAW_WORKSPACE:-}"
    "${MPM_WORKSPACE:-}"
    "$HOME/.openclaw/workspace"
    "$HOME/.picoclaw/workspace"
)

readonly ARCHIVE_AGE_CONSOLIDATE=2  # days
readonly ARCHIVE_AGE_ARCHIVE=3      # days
readonly TEMP_PATTERNS=("*.tmp" "*.log.bak" "*~" "*.swp" ".DS_Store")
readonly SAFE_EXTENSIONS=("md" "txt" "json" "yaml" "yml")

# Stats
PROCESS_STATS=("files_checked:0" "files_archived:0" "files_removed:0" "size_freed:0")

# -----------------------------------------------------------------------------
# Colors & Formatting
# -----------------------------------------------------------------------------

declare -A COLORS=(
    [reset]='\033[0m'
    [bold]='\033[1m'
    [dim]='\033[2m'
    [red]='\033[31m'
    [green]='\033[32m'
    [yellow]='\033[33m'
    [blue]='\033[34m'
    [cyan]='\033[36m'
    [white]='\033[37m'
)

color() {
    local code="${COLORS[$1]:-$1}"
    echo -en "$code"
}

# -----------------------------------------------------------------------------
# Logging
# -----------------------------------------------------------------------------

log_info()    { echo -e "${COLORS[blue]}[INFO]${COLORS[reset]} $1"; }
log_success() { echo -e "${COLORS[green]}[OK]${COLORS[reset]} $1"; }
log_warn()    { echo -e "${COLORS[yellow]}[WARN]${COLORS[reset]} $1"; }
log_error()   { echo -e "${COLORS[red]}[ERROR]${COLORS[reset]} $1" >&2; }
log_section() { echo -e "\n${COLORS[bold]}${COLORS[cyan]}$1${COLORS[reset]}"; }

# -----------------------------------------------------------------------------
# Path Detection
# -----------------------------------------------------------------------------

detect_workspace() {
    local ws=""
    
    for dir in "${CONFIG_DIRS[@]}"; do
        [[ -z "$dir" ]] && continue
        if [[ -d "$dir" ]]; then
            ws="$dir"
            break
        fi
    done
    
    # Fallback: try to find workspace relative to script
    if [[ -z "$ws" ]]; then
        local script_dir
        script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
        # Look for workspace 2-3 levels up
        local parent
        parent="$(dirname "$script_dir")"
        if [[ -d "$parent/memory" ]]; then
            ws="$parent"
        else
            parent="$(dirname "$parent")"
            [[ -d "$parent/memory" ]] && ws="$parent"
        fi
    fi
    
    [[ -n "$ws" ]] && echo "$ws"
}

validate_workspace() {
    local ws="$1"
    local required_dirs=("memory")
    local missing=()
    
    [[ ! -d "$ws" ]] && { log_error "Workspace not a directory: $ws"; return 1; }
    [[ ! -r "$ws" ]] && { log_error "Cannot read workspace: $ws"; return 1; }
    
    for dir in "${required_dirs[@]}"; do
        [[ ! -d "$ws/$dir" ]] && missing+=("$dir")
    done
    
    if [[ ${#missing[@]} -gt 0 ]]; then
        log_warn "Missing directories in workspace: ${missing[*]}"
        log_info "Creating missing directories..."
        for dir in "${missing[@]}"; do
            mkdir -p "$ws/$dir" || { log_error "Failed to create: $ws/$dir"; return 1; }
        done
    fi
    
    return 0
}

# -----------------------------------------------------------------------------
# File Safety
# -----------------------------------------------------------------------------

is_safe_to_archive() {
    local file="$1"
    local ext="${file##*.}"
    local size
    
    # Check if file exists and is readable
    [[ -f "$file" ]] || return 1
    [[ -r "$file" ]] || { log_warn "Cannot read: $file"; return 1; }
    
    # Check extension
    local safe=false
    for se in "${SAFE_EXTENSIONS[@]}"; do
        [[ "$ext" == "$se" ]] && { safe=true; break; }
    done
    
    $safe || { log_warn "Skipping unknown extension ($ext): $file"; return 1; }
    
    # Check file size (max 50MB for archiving)
    size=$(stat -f%z "$file" 2>/dev/null || stat -c%s "$file" 2>/dev/null || echo "0")
    [[ "$size" =~ ^[0-9]+$ ]] || size=0
    
    local max_size=$((50 * 1024 * 1024))
    (( size > max_size )) && { log_warn "File too large (${size} bytes): $file"; return 1; }
    
    return 0
}

is_safe_to_remove() {
    local file="$1"
    local basename
    basename=$(basename "$file")
    
    # Check patterns
    for pattern in "${TEMP_PATTERNS[@]}"; do
        [[ "$basename" == $pattern ]] && return 0
    done
    
    # Check extension
    local ext="${file##*.}"
    [[ "$ext" == "tmp" ]] && return 0
    [[ "$ext" == "bak" ]] && return 0
    [[ "$ext" == "swp" ]] && return 0
    [[ "$ext" == "log" ]] && return 0
    
    return 1
}

# -----------------------------------------------------------------------------
# Operations
# -----------------------------------------------------------------------------

get_files_older_than() {
    local dir="$1"
    local days="$2"
    
    find "$dir" -maxdepth 1 -type f -mtime +"$days" 2>/dev/null || true
}

archive_files() {
    local source_dir="$1"
    local dest_dir="$2"
    local days="$3"
    local count=0
    local total_size=0
    
    mkdir -p "$dest_dir" || { log_error "Cannot create archive dir: $dest_dir"; return 1; }
    
    while IFS= read -r -d '' file; do
        [[ -z "$file" ]] && continue
        
        is_safe_to_archive "$file" || continue
        
        local size
        size=$(stat -f%z "$file" 2>/dev/null || stat -c%s "$file" 2>/dev/null || echo "0")
        
        if $DRY_RUN; then
            log_info "[DRY-RUN] Would archive: $(basename "$file")"
        else
            cp "$file" "$dest_dir/" && rm "$file"
            log_success "Archived: $(basename "$file")"
        fi
        
        ((count++))
        total_size=$((total_size + size))
    done < <(find "$source_dir" -maxdepth 1 -type f -mtime +"$days" -print0 2>/dev/null || true)
    
    PROCESS_STATS[files_archived]=$((PROCESS_STATS[files_archived] + count))
    PROCESS_STATS[size_freed]=$((PROCESS_STATS[size_freed] + total_size))
    
    echo "$count"
}

cleanup_temp_files() {
    local dir="$1"
    local count=0
    local total_size=0
    
    for pattern in "${TEMP_PATTERNS[@]}"; do
        while IFS= read -r -d '' file; do
            [[ -z "$file" ]] && continue
            is_safe_to_remove "$file" || continue
            
            local size
            size=$(stat -f%z "$file" 2>/dev/null || stat -c%s "$file" 2>/dev/null || echo "0")
            
            if $DRY_RUN; then
                log_info "[DRY-RUN] Would remove: $(basename "$file")"
            else
                rm "$file"
                log_success "Removed: $(basename "$file")"
            fi
            
            ((count++))
            total_size=$((total_size + size))
        done < <(find "$dir" -name "$pattern" -type f -print0 2>/dev/null || true)
    done
    
    PROCESS_STATS[files_removed]=$((PROCESS_STATS[files_removed] + count))
    PROCESS_STATS[size_freed]=$((PROCESS_STATS[size_freed] + total_size))
    
    echo "$count"
}

# -----------------------------------------------------------------------------
# Health Check
# -----------------------------------------------------------------------------

run_health_check() {
    log_section "🔍 Health Check"
    local issues=0
    
    # Check core files
    log_info "Checking core identity files..."
    local core_files=("SOUL.md" "AGENTS.md" "USER.md")
    for file in "${core_files[@]}"; do
        if [[ -f "$WORKSPACE/$file" ]]; then
            local size
            size=$(stat -f%z "$WORKSPACE/$file" 2>/dev/null || stat -c%s "$WORKSPACE/$file" 2>/dev/null || echo "?")
            log_success "$file (${size}b)"
        else
            log_warn "Missing core file: $file"
            ((issues++))
        fi
    done
    
    # Check memory directory
    log_info "Checking memory directory..."
    local mem_count
    mem_count=$(find "$MEMORY_DIR" -name "*.md" -type f 2>/dev/null | wc -l | tr -d '[:space:]')
    log_info "Memory files: $mem_count"
    
    # Check archive directory
    local arch_count
    arch_count=$(find "$ARCHIVE_DIR" -type f 2>/dev/null | wc -l | tr -d '[:space:]')
    log_info "Archived files: $arch_count"
    
    # Disk usage
    log_info "Disk usage summary:"
    du -sh "$WORKSPACE"/* 2>/dev/null | sort -hr | head -10 | while read size dir; do
        dir=$(basename "$dir")
        echo "   ${COLORS[dim]}$dir:${COLORS[reset]} $size"
    done
    
    if [[ $issues -eq 0 ]]; then
        log_success "All health checks passed"
        return 0
    else
        log_warn "Found $issues potential issues"
        return 1
    fi
}

# -----------------------------------------------------------------------------
# Dry Run Preview
# -----------------------------------------------------------------------------

show_dry_run_preview() {
    log_section "🔮 Dry Run Preview"
    
    # Consolidate preview
    if $CONSOLIDATE; then
        log_info "Files to consolidate (> $ARCHIVE_AGE_CONSOLIDATE days old):"
        local count
        count=$(find "$MEMORY_DIR" -maxdepth 1 -type f -mtime +$ARCHIVE_AGE_CONSOLIDATE 2>/dev/null | wc -l | tr -d '[:space:]')
        local size
        size=$(find "$MEMORY_DIR" -maxdepth 1 -type f -mtime +$ARCHIVE_AGE_CONSOLIDATE -exec du -ch {} + 2>/dev/null | tail -1 | cut -f1 || echo "0B")
        echo "   Count: $count files"
        echo "   Size: $size"
    fi
    
    # Archive preview
    if $ARCHIVE; then
        log_info "Files to archive (> $ARCHIVE_AGE_ARCHIVE days old):"
        local count
        count=$(find "$MEMORY_DIR" -maxdepth 1 -type f -mtime +$ARCHIVE_AGE_ARCHIVE 2>/dev/null | wc -l | tr -d '[:space:]')
        local size
        size=$(find "$MEMORY_DIR" -maxdepth 1 -type f -mtime +$ARCHIVE_AGE_ARCHIVE -exec du -ch {} + 2>/dev/null | tail -1 | cut -f1 || echo "0B")
        echo "   Count: $count files"
        echo "   Size: $size"
    fi
    
    # Cleanup preview
    if $CLEANUP; then
        log_info "Temp files to remove:"
        local total=0
        for pattern in "${TEMP_PATTERNS[@]}"; do
            local count
            count=$(find "$WORKSPACE" -name "$pattern" -type f 2>/dev/null | wc -l | tr -d '[:space:]')
            echo "   Pattern '$pattern': $count files"
            ((total+=count))
        done
        echo "   Total: $total files"
    fi
    
    log_info "Use --consolidate, --archive, or --cleanup to execute"
}

# -----------------------------------------------------------------------------
# Help Text
# -----------------------------------------------------------------------------

show_help() {
    cat << 'EOF'
╔═══════════════════════════════════════════════════════════════════════════╗
║                         🏥 MPM Medic v3.3                                  ║
║                Memory Cleanup & Health Diagnostic Tool                     ║
╚═══════════════════════════════════════════════════════════════════════════╝

DESCRIPTION:
  Automated cleanup tool for MPM workspace. Archives old session files,
  removes temporary files, and performs health diagnostics.

USAGE:
  ./medic.sh [OPTIONS]

OPTIONS:
  --consolidate, -con      Archive memory files older than 2 days
  --archive, -a            Archive memory files older than 3 days
  --cleanup, -c            Remove temporary and backup files
  --health, -h             Run health check only (no cleanup)
  --dry-run, -n            Preview changes without executing
  --verbose, -v            Show detailed output
  --version                Show version information
  --help                   Show this help message

SHORTCUTS (SymAI style):
  ~k.con, ~k.consolidate   Consolidate files
  ~k.arc, ~k.archive      Archive files
  ~k.cl, ~k.cleanup        Cleanup temp files
  ~k.dry                  Dry run mode

EXAMPLES:
  # Preview what would be archived
  ./medic.sh --dry-run --consolidate

  # Archive old files and clean up temps
  ./medic.sh --archive --cleanup

  # Run full maintenance
  ./medic.sh --consolidate --archive --cleanup

  # Health check only
  ./medic.sh --health

DIRECTORIES:
  Memory:    ${WORKSPACE}/mpm/memory/sessions/
  Archive:   ${WORKSPACE}/mpm/memory/archive/

SAFETY:
  • Only archives .md, .txt, .json, .yaml, .yml files
  • Verifies file integrity before operations
  • Preserves files modified within retention period
  • Shows preview in dry-run mode before execution

EXIT CODES:
  0  Success
  1  General error
  2  Workspace not found
  3  Permission denied
  4  Invalid arguments

For more information, see:
  ./medic.sh --help | less
EOF
}

show_version() {
    echo "MPM Medic v$SCRIPT_VERSION"
    echo "Memory Cleanup & Health Diagnostic Tool"
    echo "Part of the SymAI Persona Management System"
}

# -----------------------------------------------------------------------------
# Main
# -----------------------------------------------------------------------------

main() {
    # Parse arguments
    DRY_RUN=false
    CONSOLIDATE=false
    ARCHIVE=false
    CLEANUP=false
    HEALTH=false
    VERBOSE=false
    
    for arg in "$@"; do
        case "$arg" in
            --dry-run|-n|~k.dry)               DRY_RUN=true ;;
            --consolidate|-con|~k.con)         CONSOLIDATE=true ;;
            --archive|-a|~k.arc)               ARCHIVE=true ;;
            --cleanup|-c|~k.cl)                CLEANUP=true ;;
            --health|~k.health|-h)            HEALTH=true ;;
            --verbose|-v)                     VERBOSE=true ;;
            --version|-V)                     show_version; exit 0 ;;
            --help|--helps|-help|--help)      show_help; exit 0 ;;
            *)                                log_error "Unknown option: $arg"; show_help; exit 4 ;;
        esac
    done
    
    # Detect workspace
    WORKSPACE=$(detect_workspace)
    if [[ -z "$WORKSPACE" ]]; then
        log_error "Could not detect workspace"
        log_info "Set OPENCLAW_WORKSPACE or MPM_WORKSPACE environment variable"
        exit 2
    fi
    
    # Validate workspace
    if ! validate_workspace "$WORKSPACE"; then
        log_error "Workspace validation failed: $WORKSPACE"
        exit 2
    fi
    
    # Set up paths
    readonly WORKSPACE
    readonly MPM_DIR="$WORKSPACE/mpm"
    readonly MEMORY_DIR="$MPM_DIR/memory/sessions"
    readonly ARCHIVE_DIR="$MPM_DIR/memory/archive"
    
    # Ensure directories exist
    mkdir -p "$MEMORY_DIR" "$ARCHIVE_DIR"
    
    # Header
    echo -e "${COLORS[bold]}${COLORS[cyan]}╔═══════════════════════════════════════════════════════════════╗${COLORS[reset]}"
    echo -e "${COLORS[bold]}${COLORS[cyan]}║  🏥 MPM Medic v${SCRIPT_VERSION} - Memory Cleanup & Health           ║${COLORS[reset]}"
    echo -e "${COLORS[bold]}${COLORS[cyan]}╚═══════════════════════════════════════════════════════════════╝${COLORS[reset]}"
    
    log_info "Workspace: $WORKSPACE"
    $VERBOSE && log_info "Memory:    $MEMORY_DIR"
    $VERBOSE && log_info "Archive:   $ARCHIVE_DIR"
    $DRY_RUN && log_section "🔮 DRY RUN MODE - No changes will be made"
    
    # Health check
    run_health_check || true
    
    # Show dry run preview if requested
    if $DRY_RUN; then
        show_dry_run_preview
        exit 0
    fi
    
    # Run operations
    local total_archived=0
    local total_removed=0
    
    if $CONSOLIDATE; then
        log_section "🧹 Consolidating Files (> $ARCHIVE_AGE_CONSOLIDATE days old)"
        total_archived=$(archive_files "$MEMORY_DIR" "$ARCHIVE_DIR" $ARCHIVE_AGE_CONSOLIDATE)
        log_success "Consolidated $total_archived files"
    fi
    
    if $ARCHIVE; then
        log_section "📦 Archiving Files (> $ARCHIVE_AGE_ARCHIVE days old)"
        total_archived=$(archive_files "$MEMORY_DIR" "$ARCHIVE_DIR" $ARCHIVE_AGE_ARCHIVE)
        log_success "Archived $total_archived files"
    fi
    
    if $CLEANUP; then
        log_section "🗑️  Cleaning Up Temp Files"
        total_removed=$(cleanup_temp_files "$WORKSPACE")
        log_success "Removed $total_removed temp files"
    fi
    
    # Summary
    if ! $CONSOLIDATE && ! $ARCHIVE && ! $CLEANUP; then
        log_info "No operations requested. Use --help for options."
        exit 0
    fi
    
    log_section "✅ Summary"
    echo "   Archived: $total_archived files"
    echo "   Removed:  $total_removed files"
    
    if [[ ${PROCESS_STATS[size_freed]} -gt 0 ]]; then
        local size_mb=$((PROCESS_STATS[size_freed] / 1024 / 1024))
        echo "   Space freed: ${size_mb}MB"
    fi
    
    log_success "Cleanup complete!"
}

# Run main
cd "$(dirname "${BASH_SOURCE[0]}")" || exit 1
main "$@"
