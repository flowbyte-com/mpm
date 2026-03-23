#!/bin/bash
#
# MPM Auto Session Worker v3.4
# Auto-save/compile/sort sessions into DB memory
# Usage: ./auto-session-worker.sh [--process-all|--status|--watch]

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORKSPACE="${MPM_WORKSPACE:-$HOME/.picoclaw/workspace}"
SESSIONS_DIR="$WORKSPACE/sessions"
MEMORY_DIR="$WORKSPACE/mpm/memory"
DB="$MEMORY_DIR/memory.db"
SCHEMA="$MEMORY_DIR/schema-v3.4.sql"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m'

log() { echo -e "${GREEN}[MPM]${NC} $1"; }
log_info() { echo -e "${BLUE}[INFO]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }
log_debug() { [[ "${MPM_DEBUG:-0}" == "1" ]] && echo -e "${CYAN}[DEBUG]${NC} $1" || true; }

# Detect Python
PYTHON="${MPM_PYTHON:-$(which python3 || which python 2>/dev/null || echo '')}"
[[ -z "$PYTHON" ]] && log_error "Python not found" && exit 1

# =====================================================
# Database Setup
# =====================================================
init_db() {
    if [[ ! -f "$DB" ]]; then
        log "Creating database..."
        sqlite3 "$DB" < "$SCHEMA"
        log "✓ Database initialized"
        return 0
    fi
    
    # Check if upgrade needed (check for v3.4 tables)
    local has_v34=$(sqlite3 "$DB" "SELECT name FROM sqlite_master WHERE name='conversation_threads' LIMIT 1;" 2>/dev/null || echo "")
    if [[ -z "$has_v34" ]]; then
        log_warn "Database is older version. Migrating..."
        cp "$DB" "$DB.v3.3.bak"
        sqlite3 "$DB" < "$SCHEMA"
        log "✓ Database migrated (backup: $DB.v3.3.bak)"
    fi
}

# =====================================================
# Session Processing
# =====================================================
get_processed_files() {
    sqlite3 "$DB" "SELECT DISTINCT source_file FROM sessions WHERE processed_at IS NOT NULL;" 2>/dev/null | sort -u || echo ""
}

get_all_session_files() {
    find "$SESSIONS_DIR" -name "*.jsonl" -type f 2>/dev/null | sort
}

calc_relevance_score() {
    local msg_count="$1"
    local has_decision="$2"
    local importance="$3"
    
    # Base from message count (log scale)
    local base=$(echo "scale=4; l($msg_count + 1) / l(100)" | bc -l 2>/dev/null || echo "0.5")
    [[ -z "$base" || "$base" == "" ]] && base=0.5
    
    # Decision bonus
    local bonus=0
    [[ "$has_decision" == "1" ]] && bonus=0.3
    
    # Importance weighting
    local imp_weight=$(echo "scale=4; $importance / 10" | bc 2>/dev/null || echo "0.3")
    [[ -z "$imp_weight" || "$imp_weight" == "" ]] && imp_weight=0.3
    
    # Combine (cap at 1.0)
    local score=$(echo "scale=4; $base + $bonus + ($imp_weight * 0.2)" | bc)
    [[ -z "$score" || "$score" == "" ]] && score=0.5
    
    # Clamp to 0-1
    if (( $(echo "$score > 1" | bc -l) )); then score="1.0"; fi
    if (( $(echo "$score < 0" | bc -l) )); then score="0.0"; fi
    
    echo "$score"
}

process_session_file() {
    local jsonl_file="$1"
    local meta_file="${jsonl_file%.jsonl}.meta.json"
    local filename=$(basename "$jsonl_file")
    
    log_info "Processing: $filename"
    
    # Check if already processed
    local already=$(sqlite3 "$DB" "SELECT id FROM sessions WHERE source_file = '$jsonl_file' LIMIT 1;" 2>/dev/null || echo "")
    if [[ -n "$already" ]]; then
        log_debug "Already processed: $filename"
        return 0
    fi
    
    # Validate files exist (except heartbeat)
    if [[ ! "$filename" =~ heartbeat ]] && [[ ! -f "$meta_file" ]]; then
        log_warn "Missing meta file for $filename, skipping"
        return 1
    fi
    
    # Parse metadata
    local session_key=""
    local summary=""
    local channel=""
    local importance=3
    local start_time=""
    local end_time=""
    local msg_count=0
    
    if [[ -f "$meta_file" ]]; then
        session_key=$(jq -r '.key // empty' "$meta_file" 2>/dev/null)
        summary=$(jq -r '.summary // empty' "$meta_file" 2>/dev/null)
        channel=$(jq -r '.channel // empty' "$meta_file" 2>/dev/null)
        importance=$(jq -r '.importance // 3' "$meta_file" 2>/dev/null)
        start_time=$(jq -r '.created_at // empty' "$meta_file" 2>/dev/null)
        end_time=$(jq -r '.updated_at // empty' "$meta_file" 2>/dev/null)
        msg_count=$(jq -r '.count // 0' "$meta_file" 2>/dev/null)
    fi
    
    # Generate session key if missing
    if [[ -z "$session_key" ]]; then
        session_key="auto_$(basename "$jsonl_file" .jsonl)"
    fi
    
    # Default times
    [[ -z "$start_time" ]] && start_time=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
    [[ -z "$end_time" ]] && end_time=$start_time
    [[ -z "$channel" ]] && channel="unknown"
    
    # Calculate duration
    local duration=0
    if command -v python3 >/dev/null; then
        duration=$(python3 -c "
from datetime import datetime
import sys
try:
    t1 = datetime.fromisoformat('${start_time}'.replace('Z', '+00:00'))
    t2 = datetime.fromisoformat('${end_time}'.replace('Z', '+00:00'))
    print(int((t2 - t1).total_seconds()))
except:
    print(0)
" 2>/dev/null || echo "0")
    fi
    [[ -z "$duration" ]] && duration=0
    
    # Detect thread_id from session_key pattern
    local thread_id=""
    if [[ "$session_key" =~ agent:[^:]+:([^:]+) ]]; then
        thread_id="${BASH_REMATCH[1]}"
    fi
    
    # Default summary if missing
    if [[ -z "$summary" ]]; then
        local first_line=$(head -1 "$jsonl_file" | jq -r '.content // empty' 2>/dev/null | cut -c1-80)
        summary="Auto-extracted: ${first_line}..."
    fi
    
    # Estimate tokens (rough: 4 chars ~ 1 token)
    local file_size=$(stat -f%z "$jsonl_file" 2>/dev/null || stat -c%s "$jsonl_file" 2>/dev/null || echo "0")
    local token_est=$((file_size / 4))
    
    # Check if contains decision patterns
    local has_decision=0
    if grep -qiE "(decided|decision|choice|selected|chose|went with|picked|opted)" "$jsonl_file" 2>/dev/null; then
        has_decision=1
        importance=$((importance + 1))
        [[ $importance -gt 10 ]] && importance=10
    fi
    
    # Calculate relevance score
    local relevance=$(calc_relevance_score "$msg_count" "$has_decision" "$importance")
    
    log_debug "Session: $session_key / Count: $msg_count / Duration: ${duration}s / Rel: $relevance"
    
    # Insert session
    sqlite3 "$DB" <<SQL
INSERT INTO sessions (
    session_key, channel, thread_id, start_time, end_time,
    duration_seconds, summary, importance, relevance_score,
    message_count, token_estimate, source_file, processed_at
) VALUES (
    '$session_key',
    '$(echo "$channel" | sed "s/'/''/g")',
    '$(echo "$thread_id" | sed "s/'/''/g")',
    '$start_time',
    '$end_time',
    $duration,
    '$(echo "$summary" | sed "s/'/''/g")',
    $importance,
    $relevance,
    $msg_count,
    $token_est,
    '$jsonl_file',
    datetime('now')
);
SQL
    
    local session_id=$(sqlite3 "$DB" "SELECT id FROM sessions WHERE session_key = '$session_key' ORDER BY id DESC LIMIT 1;" 2>/dev/null)
    if [[ -z "$session_id" ]]; then
        log_error "Failed to insert session: $session_key"
        return 1
    fi
    
    log "✓ Session $session_id inserted"
    
    # Process messages
    process_messages "$jsonl_file" "$session_id"
    
    # Create thread if needed
    if [[ -n "$thread_id" ]]; then
        ensure_thread "$thread_id" "$session_key"
        local thread_db_id=$(sqlite3 "$DB" "SELECT id FROM conversation_threads WHERE thread_key = '$thread_id' LIMIT 1;" 2>/dev/null || echo "")
        if [[ -n "$thread_db_id" ]]; then
            sqlite3 "$DB" "INSERT OR IGNORE INTO session_threads (session_id, thread_id) VALUES ($session_id, $thread_db_id);"
            sqlite3 "$DB" "UPDATE conversation_threads SET last_activity = datetime('now') WHERE id = $thread_db_id;"
        fi
    fi
    
    # Extract memories (defer to Python)
    (cd "$SCRIPT_DIR" && "$PYTHON" analyze-session-v3.4.py "$session_id" "$jsonl_file" 2>/dev/null) &
    
    return 0
}

process_messages() {
    local jsonl_file="$1"
    local session_id="$2"
    
    local idx=0
    while IFS= read -r line || [[ -n "$line" ]]; do
        [[ -z "$line" ]] && continue
        
        # Extract via sqlite3 (single quotes safe)
        local content=$(echo "$line" | jq -r '.content // empty' 2>/dev/null | sed "s/'/''/g")
        local role=$(echo "$line" | jq -r '.role // "assistant"' 2>/dev/null)
        local timestamp=$(echo "$line" | jq -r '.timestamp // empty' 2>/dev/null)
        local tool_calls=$(echo "$line" | jq -c '.tool_calls // empty' 2>/dev/null | sed "s/'/''/g")
        [[ -z "$content" ]] && continue
        
        # Detect content type
        local content_type="other"
        if [[ "$role" == "user" ]]; then
            if [[ "$content" =~ \? ]]; then
                content_type="question"
            elif [[ "$content" =~ ^[[:space:]]*(fix|debug|solve|implement|create|build|add|update|delete|refactor) ]]; then
                content_type="command"
            else
                content_type="clarification"
            fi
        elif [[ "$role" == "assistant" ]]; then
            if [[ "$content" =~ ^"```" ]]; then
                content_type="code"
            elif [[ "$content" =~ ^"##".*decision ]]; then
                content_type="decision"
            fi
        fi
        
        # Summary for long messages
        local summary=""
        if [[ ${#content} -gt 100 ]]; then
            summary="${content:0:100}..."
        fi
        
        # Insert message
        sqlite3 "$DB" <<SQL 2>/dev/null || true
INSERT INTO messages (
    session_id, msg_index, role, content, summary, timestamp, tool_calls, content_type
) VALUES (
    $session_id,
    $idx,
    '$(echo "$role" | sed "s/'/''/g")',
    '${content}',
    '$(echo "$summary" | sed "s/'/''/g")',
    '$(echo "$timestamp" | sed "s/'/''/g")',
    '${tool_calls}',
    '$content_type'
);
SQL
        ((idx++))
    done < "$jsonl_file"
    
    log_debug "Inserted $idx messages"
}

ensure_thread() {
    local thread_key="$1"
    local session_key="$2"
    
    local exists=$(sqlite3 "$DB" "SELECT 1 FROM conversation_threads WHERE thread_key = '$thread_key' LIMIT 1;" 2>/dev/null || echo "")
    if [[ -z "$exists" ]]; then
        sqlite3 "$DB" <<SQL
INSERT INTO conversation_threads (thread_key, description, status, priority)
VALUES ('$thread_key', 'Auto-created from $session_key', 'active', 3);
SQL
        log_debug "Created thread: $thread_key"
    fi
}

# =====================================================
# Main Operations
# =====================================================
process_all() {
    log "=== Auto Session Worker v3.4 ==="
    echo ""
    
    init_db
    
    local files=$(get_all_session_files)
    local total=$(echo "$files" | grep -c "\.jsonl$" 2>/dev/null || echo "0")
    local processed=0
    local skipped=0
    
    log "Found $total session files"
    
    while IFS= read -r file; do
        [[ -f "$file" ]] || continue
        
        if process_session_file "$file"; then
            ((processed++))
        else
            ((skipped++))
        fi
        
        # Show progress every 10
        if (( processed % 10 == 0 && processed > 0 )); then
            log "Progress: $processed processed, $skipped skipped"
        fi
    done <<< "$files"
    
    # Update sync meta
    sqlite3 "$DB" <<SQL
UPDATE sync_meta SET 
    last_sync = datetime('now'),
    sessions_synced = sessions_synced + $processed,
    version = '3.4'
WHERE id = 1;
SQL
    
    echo ""
    log "=== Summary ==="
    log "Processed: $processed"
    log "Skipped: $skipped"
    echo ""
    
    # Quick stats
    local mem_count=$(sqlite3 "$DB" "SELECT COUNT(*) FROM memory_items;" 2>/dev/null || echo "0")
    local ent_count=$(sqlite3 "$DB" "SELECT COUNT(*) FROM entities;" 2>/dev/null || echo "0")
    log "Total memories in DB: $mem_count"
    log "Total entities: $ent_count"
}

show_status() {
    if [[ ! -f "$DB" ]]; then
        log_warn "Database not initialized"
        log "Run: ./auto-session-worker.sh --process-all"
        exit 1
    fi
    
    echo ""
    echo "╔════════════════════════════════════════════════════════╗"
    echo "║         MPM Memory Database Status v3.4               ║"
    echo "╚════════════════════════════════════════════════════════╝"
    echo ""
    
    sqlite3 "$DB" <<SQL
SELECT '━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━';
SELECT '📊 Summary';
SELECT '━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━';
SELECT 
    '  Sessions:     ' || COUNT(*) || ' (' || SUM(CASE WHEN is_archived THEN 1 ELSE 0 END) || ' archived)' FROM sessions;
SELECT 
    '  Messages:     ' || COUNT(*) FROM messages;
SELECT 
    '  Memories:     ' || COUNT(*) || 
    ' (' || SUM(CASE WHEN item_type='decision' THEN 1 ELSE 0 END) || ' decisions)' FROM memory_items;
SELECT 
    '  Entities:     ' || COUNT(*) || 
    ' (' || SUM(CASE WHEN entity_type='project' THEN 1 ELSE 0 END) || ' projects)' FROM entities;
SELECT 
    '  Topics:       ' || COUNT(*) FROM topics;
SELECT 
    '  Threads:      ' || COUNT(*) || ' active' FROM conversation_threads WHERE status='active';
SELECT '━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━';
SQL
    
    echo ""
    echo "🔥 Recent High-Priority Sessions:"
    sqlite3 -header "$DB" "SELECT id, channel, importance, relevance_score, 
    substr(summary,1,50) as summary FROM sessions 
    ORDER BY importance DESC, relevance_score DESC LIMIT 5;"
    
    echo ""
    echo "💎 Top Decisions:"
    sqlite3 -header "$DB" "SELECT m.id, m.item_type, s.channel, 
    substr(m.summary,1,50) as summary 
    FROM memory_items m 
    JOIN sessions s ON m.source_session_id = s.id 
    WHERE m.item_type = 'decision' 
    ORDER BY m.importance DESC, m.created_at DESC LIMIT 5;"
    
    echo ""
    echo "📝 Session Files vs DB:"
    local file_count=$(find "$SESSIONS_DIR" -name "*.jsonl" -type f 2>/dev/null | wc -l)
    local db_count=$(sqlite3 "$DB" "SELECT COUNT(*) FROM sessions;" 2>/dev/null || echo "0")
    echo "  Files: $file_count"
    echo "  In DB: $db_count"
    if (( file_count > db_count )); then
        echo "  ⚠️  $((file_count - db_count)) files pending sync"
    fi
}

watch_mode() {
    log "Starting watch mode (checks every 60s)..."
    
    init_db
    
    while true; do
        local pending=$(find "$SESSIONS_DIR" -name "*.jsonl" -newer "$DB" 2>/dev/null | wc -l)
        if (( pending > 0 )); then
            log "Found $pending new/changed session files"
            process_all
        fi
        sleep 60
    done
}

# =====================================================
# CLI
# =====================================================
case "${1:-}" in
    --process-all)
        process_all
        ;;
    --status)
        show_status
        ;;
    --watch)
        watch_mode
        ;;
    *)
        echo "MPM Auto Session Worker v3.4"
        echo ""
        echo "Usage:"
        echo "  $0 --process-all    Process all unprocessed sessions"
        echo "  $0 --status         Show database status"
        echo "  $0 --watch          Watch for new sessions (daemon)"
        echo ""
        echo "Environment:"
        echo "  MPM_WORKSPACE       Override workspace path"
        echo "  MPM_PYTHON          Python executable path"
        echo "  MPM_DEBUG=1         Enable verbose logging"
        show_status
        ;;
esac
