#!/bin/bash
# ⚡ MPM Watcher v3.3 - Live-Sync Persona/Mode + Memory Changes
# Enhanced with comprehensive file validation and error handling

set -o pipefail

# Configuration
readonly MAX_FILENAME_LEN=128
readonly VALID_EXT_PATTERN='^[.](persona|mode)$'
readonly VALID_NAME_PATTERN='^[a-zA-Z0-9_-]+$'
readonly LOCK_WAIT_TIMEOUT=30

# Error codes
readonly E_SUCCESS=0
readonly E_DEPENDENCY=1
readonly E_PERMISSION=2
readonly E_LOCK=3
readonly E_CONFIG=4
readonly E_FATAL=5

# -----------------------------------------------------------------------------
# Error Handling
# -----------------------------------------------------------------------------

log_error() {
    local msg="$1"
    local code="${2:-$E_FATAL}"
    echo -e "\033[1;31m[ERROR $code]\033[0m $msg" >&2
    return "$code"
}

log_warn() {
    echo -e "\033[1;33m[WARN]\033[0m $1" >&2
}

log_info() {
    echo -e "\033[1;34m[INFO]\033[0m $1"
}

safe_exit() {
    local code="${1:-$E_SUCCESS}"
    cleanup
    exit "$code"
}

# -----------------------------------------------------------------------------
# Cleanup
# -----------------------------------------------------------------------------

cleanup() {
    # Kill background processes started by this script
    if [[ -n "${MEMORY_WATCHER_PID:-}" ]] && kill -0 "$MEMORY_WATCHER_PID" 2>/dev/null; then
        kill "$MEMORY_WATCHER_PID" 2>/dev/null || true
        log_info "Memory watcher stopped"
    fi
}

trap cleanup EXIT INT TERM

# -----------------------------------------------------------------------------
# Workspace Detection
# -----------------------------------------------------------------------------

detect_workspace() {
    local workspaces=(
        "${OPENCLAW_WORKSPACE:-}"
        "${MPM_WORKSPACE:-}"
        "$HOME/.openclaw/workspace"
        "$HOME/.picoclaw/workspace"
    )
    
    for ws in "${workspaces[@]}"; do
        [[ -z "$ws" ]] && continue
        if [[ -d "$ws" ]]; then
            echo "$ws"
            return $E_SUCCESS
        fi
    done
    
    return $E_CONFIG
}

# -----------------------------------------------------------------------------
# File Validation
# -----------------------------------------------------------------------------

validate_filename() {
    local filename="$1"
    local max_len="${2:-$MAX_FILENAME_LEN}"
    
    # Check empty
    [[ -z "$filename" ]] && { log_error "Empty filename provided"; return 1; }
    
    # Check length
    local len=${#filename}
    (( len > max_len )) && { log_warn "Filename too long ($len > $max_len): $filename"; return 1; }
    
    # Check for path traversal
    [[ "$filename" == *".."* ]] && { log_warn "Path traversal attempt: $filename"; return 1; }
    [[ "$filename" == *"/"* ]] && { log_warn "Path separator in filename: $filename"; return 1; }
    
    return 0
}

validate_extension() {
    local filename="$1"
    local ext="${filename##*.}"
    local base="${filename%.*}"
    
    # Check if has extension
    [[ "$filename" == "$base" ]] && { log_warn "No extension found: $filename"; return 1; }
    
    # Check extension pattern
    [[ ".$ext" =~ $VALID_EXT_PATTERN ]] && return 0
    
    log_warn "Invalid extension: $ext"
    return 1
}

validate_identifier() {
    local id="$1"
    [[ "$id" =~ $VALID_NAME_PATTERN ]] && return 0
    log_warn "Invalid identifier (must match: $VALID_NAME_PATTERN): $id"
    return 1
}

validate_file_accessible() {
    local filepath="$1"
    
    [[ -e "$filepath" ]] || { log_warn "File does not exist: $filepath"; return 1; }
    [[ -f "$filepath" ]] || { log_warn "Not a regular file: $filepath"; return 1; }
    [[ -r "$filepath" ]] || { log_warn "Cannot read file: $filepath"; return 1; }
    
    # Check file size (max 10MB)
    local size
    size=$(stat -f%z "$filepath" 2>/dev/null || stat -c%s "$filepath" 2>/dev/null || echo "0")
    [[ -n "$size" && "$size" =~ ^[0-9]+$ ]] || size=0
    
    local max_size=$((10 * 1024 * 1024))
    (( size > max_size )) && { log_warn "File too large (${size} bytes): $filepath"; return 1; }
    
    return 0
}

# -----------------------------------------------------------------------------
# Process File
# -----------------------------------------------------------------------------

process_persona_file() {
    local filepath="$1"
    local filename="$2"
    local name="$3"
    
    if [[ ! -f "$SCRIPT_DIR/persona-compile.sh" ]]; then
        log_error "Compiler not found: $SCRIPT_DIR/persona-compile.sh"
        return 1
    fi
    
    if ! validate_file_accessible "$filepath"; then
        return 1
    fi
    
    # Attempt compilation with retry
    local attempts=0
    local max_attempts=3
    while (( attempts < max_attempts )); do
        if "$SCRIPT_DIR/persona-compile.sh" compile "$name" 2>/dev/null; then
            if command -v notify-send &>/dev/null; then
                notify-send -t 1000 "⚡ SymAI Pulse" "~p.$name!sync [OK]" 2>/dev/null || true
            fi
            echo -e "🎭 \033[0;36m~p.$name!sync\033[0m [$(date '+%H:%M:%S')] [Live Update Applied]"
            return 0
        fi
        (( attempts++ ))
        (( attempts < max_attempts )) && sleep 0.5
    done
    
    log_error "Failed to compile persona after $max_attempts attempts: $name"
    return 1
}

process_mode_file() {
    local filepath="$1"
    local filename="$2"
    local name="$3"
    
    if [[ ! -f "$SCRIPT_DIR/mode-compile.sh" ]]; then
        log_error "Compiler not found: $SCRIPT_DIR/mode-compile.sh"
        return 1
    fi
    
    if ! validate_file_accessible "$filepath"; then
        return 1
    fi
    
    # Attempt compilation with retry
    local attempts=0
    local max_attempts=3
    while (( attempts < max_attempts )); do
        if "$SCRIPT_DIR/mode-compile.sh" compile "$name" 2>/dev/null; then
            if command -v notify-send &>/dev/null; then
                notify-send -t 1000 "🛠️ SymAI Pulse" "~m.$name!sync [OK]" 2>/dev/null || true
            fi
            echo -e "🛠️ \033[0;36m~m.$name!sync\033[0m [$(date '+%H:%M:%S')] [Behavior Updated]"
            return 0
        fi
        (( attempts++ ))
        (( attempts < max_attempts )) && sleep 0.5
    done
    
    log_error "Failed to compile mode after $max_attempts attempts: $name"
    return 1
}

# -----------------------------------------------------------------------------
# Main
# -----------------------------------------------------------------------------

main() {
    echo -e "\033[1;36m╔═══════════════════════════════════════════════════════════════╗\033[0m"
    echo -e "\033[1;36m║  📡 MPM Watcher v3.3 - Live Sync                               ║\033[0m"
    echo -e "\033[1;36m╚═══════════════════════════════════════════════════════════════╝\033[0m"
    echo ""
    
    # Check dependencies
    if ! command -v inotifywait &>/dev/null; then
        log_error "Missing dependency: inotifywait" "$E_DEPENDENCY"
        echo "   Install: sudo apt install inotify-tools"
        exit $E_DEPENDENCY
    fi
    
    # Detect workspace
    WORKSPACE=$(detect_workspace)
    if [[ $? -ne $E_SUCCESS ]] || [[ -z "$WORKSPACE" ]]; then
        log_error "Could not detect workspace directory" "$E_CONFIG"
        exit $E_CONFIG
    fi
    
    readonly WORKSPACE
    readonly PERSONAS_DIR="$WORKSPACE/mpm/persona"
    readonly MODES_DIR="$WORKSPACE/mpm/mode"
    readonly MEMORY_DIR="$WORKSPACE/memory"
    readonly SCRIPT_DIR="$WORKSPACE/mpm/scripts"
    
    log_info "Workspace: $WORKSPACE"
    
    # Ensure directories exist
    for dir in "$PERSONAS_DIR" "$MODES_DIR" "$MEMORY_DIR"; do
        if [[ ! -d "$dir" ]]; then
            log_info "Creating directory: $dir"
            mkdir -p "$dir" || {
                log_error "Failed to create directory: $dir" "$E_PERMISSION"
                exit $E_PERMISSION
            }
        fi
    done
    
    # Validate script directories
    if [[ ! -d "$SCRIPT_DIR" ]]; then
        log_error "Script directory not found: $SCRIPT_DIR" "$E_CONFIG"
        exit $E_CONFIG
    fi
    
    echo ""
    echo "🔍 Monitoring directories:"
    echo "   Personas: $PERSONAS_DIR"
    echo "   Modes:    $MODES_DIR"
    echo "   Memory:   $MEMORY_DIR"
    echo ""
    echo "Press Ctrl+C to stop watcher"
    echo "─────────────────────────────────────────────────────────────"
    
    # Initialize databases on start
    local init_errors=0
    
    if [[ -f "$SCRIPT_DIR/persona-compile.sh" ]]; then
        if ! "$SCRIPT_DIR/persona-compile.sh" sync quiet &>/dev/null; then
            log_warn "Initial persona compilation failed"
            ((init_errors++))
        fi
    fi
    
    if [[ -f "$SCRIPT_DIR/mode-compile.sh" ]]; then
        if ! "$SCRIPT_DIR/mode-compile.sh" sync quiet &>/dev/null; then
            log_warn "Initial mode compilation failed"
            ((init_errors++))
        fi
    fi
    
    # Start memory watcher in background
    if [[ -f "$SCRIPT_DIR/memory-watch.sh" ]]; then
        "$SCRIPT_DIR/memory-watch.sh" &
        MEMORY_WATCHER_PID=$!
        if kill -0 "$MEMORY_WATCHER_PID" 2>/dev/null; then
            log_info "Memory watcher started (PID: $MEMORY_WATCHER_PID)"
        else
            log_warn "Failed to start memory watcher"
        fi
    fi
    
    # Stats
    local processed_count=0
    local error_count=0
    
    # Main watch loop
    inotifywait -m -r "$PERSONAS_DIR" "$MODES_DIR" -e close_write \
        --format '%w%f %f %e' 2>/dev/null | while read -r filepath file event; do
        
        # Skip if this is just a directory (not a file)
        [[ -d "$filepath" ]] && continue
        
        # Validate filename
        if ! validate_filename "$file"; then
            ((error_count++))
            continue
        fi
        
        # Validate extension and extract name
        local ext="${file##*.}"
        local name="${file%.*}"
        
        if ! validate_extension "$file"; then
            ((error_count++))
            continue
        fi
        
        # Validate identifier
        if ! validate_identifier "$name"; then
            ((error_count++))
            continue
        fi
        
        # Process based on extension
        case ".$ext" in
            .persona)
                if process_persona_file "$filepath" "$file" "$name"; then
                    ((processed_count++))
                else
                    ((error_count++))
                fi
                ;;
            .mode)
                if process_mode_file "$filepath" "$file" "$name"; then
                    ((processed_count++))
                else
                    ((error_count++))
                fi
                ;;
            *)
                log_warn "Unhandled extension: $ext"
                ((error_count++))
                ;;
        esac
        
        # Log stats periodically
        if (( processed_count % 10 == 0 && processed_count > 0 )); then
            log_info "Stats: $processed_count processed, $error_count errors"
        fi
    done
}

# Run main
main "$@"
