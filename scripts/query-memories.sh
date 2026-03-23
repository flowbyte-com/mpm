#!/bin/bash
#
# MPM Memory Query System v3.4
# Fast retrieval for AI context assembly
# Usage: ./query-memories.sh [search|recent|thread|entity|project|decisions|help]

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORKSPACE="${MPM_WORKSPACE:-$HOME/.picoclaw/workspace}"
DB="$WORKSPACE/mpm/memory/memory.db"

# Colors
BLUE='\033[0;34m'
CYAN='\033[0;36m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

# =====================================================
# Utility Functions
# =====================================================
ensure_db() {
    if [[ ! -f "$DB" ]]; then
        echo "Database not found. Run: ./auto-session-worker.sh --process-all"
        exit 1
    fi
}

fmt_header() { echo -e "${BLUE}$1${NC}"; }
fmt_section() { echo -e "${CYAN}$1${NC}"; }
fmt_item() { echo -e "${GREEN}•${NC} $1"; }

# =====================================================
# Query Functions
# =====================================================
cmd_search() {
    local query="${1:-}"
    if [[ -z "$query" ]]; then
        echo "Usage: query-memories.sh search <term> [--limit=N]"
        exit 1
    fi
    
    local limit="${2:-10}"
    limit=${limit#--limit=}
    
    fmt_header "🔍 Searching: $query"
    echo ""
    
    # FTS search on memories
    sqlite3 "$DB" <<-SQL
.headers on
.mode table
SELECT 
    m.id,
    m.item_type,
    substr(m.content, 1, 80) as content_preview,
    m.confidence,
    m.importance,
    datetime(m.created_at) as when_created
FROM memory_items m
JOIN memory_fts fts ON m.id = fts.rowid
WHERE memory_fts MATCH '${query}'
ORDER BY m.importance DESC, m.confidence DESC, rank
LIMIT ${limit};
SQL
    
    echo ""
    fmt_section "Related Sessions:"
    sqlite3 "$DB" <<-SQL
SELECT 
    s.channel,
    datetime(s.start_time) as session_time,
    substr(s.summary, 1, 60)
FROM sessions s
JOIN session_fts fts ON s.id = fts.rowid
WHERE session_fts MATCH '${query}'
ORDER BY s.relevance_score DESC
LIMIT 5;
SQL
}

cmd_recent() {
    local hours="${1:-24}"
    hours=${hours#--hours=}
    
    fmt_header "📅 Recent Activity (past ${hours}h)"
    echo ""
    
    fmt_section "Latest Sessions:"
    sqlite3 "$DB" <<-SQL
.headers on
.mode table
SELECT 
    id,
    channel,
    importance,
    relevance_score,
    substr(summary, 1, 50) as summary
FROM sessions
WHERE start_time > datetime('now', '-${hours} hours')
ORDER BY start_time DESC
LIMIT 10;
SQL
    
    echo ""
    fmt_section "Recent Decisions:"
    sqlite3 "$DB" <<-SQL
SELECT 
    s.channel,
    datetime(m.created_at) as when,
    substr(m.content, 1, 70) as decision
FROM memory_items m
JOIN sessions s ON m.source_session_id = s.id
WHERE m.item_type = 'decision'
    AND m.created_at > datetime('now', '-${hours} hours')
ORDER BY m.importance DESC, m.created_at DESC
LIMIT 5;
SQL
}

cmd_thread() {
    local thread_key="$1"
    if [[ -z "$thread_key" ]]; then
        fmt_section "Active Threads:"
        sqlite3 "$DB" <<-SQL
SELECT 
    thread_key,
    status,
    priority,
    datetime(last_activity) as last_active,
    substr(context_summary, 1, 50) as context
FROM conversation_threads
WHERE status = 'active'
ORDER BY last_activity DESC;
SQL
        return
    fi
    
    fmt_header "🧵 Thread: $thread_key"
    echo ""
    
    sqlite3 "$DB" <<-SQL
SELECT 
    s.id,
    s.channel,
    datetime(s.start_time) as when,
    s.importance,
    substr(s.summary, 1, 60) as summary
FROM sessions s
JOIN session_threads st ON s.id = st.session_id
JOIN conversation_threads t ON st.thread_id = t.id
WHERE t.thread_key = '${thread_key}'
ORDER BY s.start_time;
SQL
}

cmd_entity() {
    local entity_name="${1:-}"
    
    if [[ -z "$entity_name" ]]; then
        fmt_header "🏢 Entities by Type"
        echo ""
        
        fmt_section "Projects:"
        sqlite3 "$DB" "SELECT name, mention_count, last_mentioned FROM entities WHERE entity_type='project' ORDER BY mention_count DESC LIMIT 10;"
        
        echo ""
        fmt_section "Tools/Technologies:"
        sqlite3 "$DB" "SELECT name, mention_count, last_mentioned FROM entities WHERE entity_type='tool' ORDER BY mention_count DESC LIMIT 10;"
        
        echo ""
        fmt_section "People:"
        sqlite3 "$DB" "SELECT name, mention_count, last_mentioned FROM entities WHERE entity_type='person' ORDER BY mention_count DESC LIMIT 5;"
        return
    fi
    
    fmt_header "🏢 Entity: $entity_name"
    echo ""
    
    # Get entity info
    sqlite3 "$DB" <<-SQL
SELECT * FROM entities WHERE name LIKE '%${entity_name}%' OR canonical_name LIKE '%${entity_name}%';
SQL
    
    echo ""
    fmt_section "Related Memories:"
    sqlite3 "$DB" <<-SQL
SELECT 
    m.item_type,
    datetime(m.created_at) as when,
    substr(m.content, 1, 80)
FROM memory_items m
JOIN relations r ON r.source_type='memory' AND r.source_id=m.id
JOIN entities e ON r.target_type='entity' AND r.target_id=e.id
WHERE e.name LIKE '%${entity_name}%'
ORDER BY m.created_at DESC
LIMIT 10;
SQL
}

cmd_project() {
    local project_name="${1:-}"
    
    if [[ -z "$project_name" ]]; then
        fmt_header "🚀 Active Projects"
        echo ""
        
        sqlite3 "$DB" <<-SQL
SELECT 
    e.name as project,
    e.mention_count,
    e.current_status,
    datetime(e.last_mentioned) as last_active,
    e.related_topics
FROM entities e
WHERE e.entity_type = 'project'
ORDER BY e.last_mentioned DESC;
SQL
        return
    fi
    
    # Get project info
    fmt_header "🚀 Project Context: $project_name"
    echo ""
    
    fmt_section "Decisions Made:"
    sqlite3 "$DB" <<-SQL
SELECT 
    datetime(m.created_at) as when,
    substr(m.content, 1, 100) as decision
FROM memory_items m
JOIN relations r ON r.source_type='memory' AND r.source_id=m.id
JOIN entities e ON r.target_type='entity' AND r.target_id=e.id
WHERE e.name LIKE '%${project_name}%'
    AND m.item_type = 'decision'
ORDER BY m.created_at DESC
LIMIT 10;
SQL
    
    echo ""
    fmt_section "Recent Activity:"
    sqlite3 "$DB" <<-SQL
SELECT 
    datetime(s.start_time) as when,
    s.channel,
    s.outcome_status,
    substr(s.summary, 1, 70)
FROM sessions s
JOIN session_threads st ON s.id = st.session_id
JOIN conversation_threads t ON st.thread_id = t.id
WHERE t.entity_focus LIKE '%${project_name}%'
    OR s.summary LIKE '%${project_name}%'
ORDER BY s.start_time DESC
LIMIT 5;
SQL
}

cmd_decisions() {
    local limit="${1:-15}"
    limit=${limit#--limit=}
    
    fmt_header "⚡ Key Decisions"
    echo ""
    
    sqlite3 "$DB" <<-SQL
.headers on
.mode table
SELECT 
    m.id,
    m.importance as imp,
    s.channel,
    datetime(m.created_at) as when,
    substr(m.content, 1, 100) as decision
FROM memory_items m
JOIN sessions s ON m.source_session_id = s.id
WHERE m.item_type = 'decision'
ORDER BY m.importance DESC, m.created_at DESC
LIMIT ${limit};
SQL
}

cmd_context() {
    # Build quick context for current session
    local thread="${1:-$(sqlite3 "$DB" "SELECT thread_key FROM conversation_threads WHERE status='active' ORDER BY last_activity DESC LIMIT 1;" 2>/dev/null || echo "")}"
    
    fmt_header "🧠 Quick Context Assembly"
    echo ""
    
    if [[ -n "$thread" ]]; then
        fmt_section "Thread: $thread"
        sqlite3 "$DB" <<-SQL
SELECT 
    s.id,
    datetime(s.start_time) as when,
    s.user_intent,
    s.ai_actions,
    s.outcome_status
FROM sessions s
JOIN session_threads st ON s.id = st.session_id
JOIN conversation_threads t ON st.thread_id = t.id
WHERE t.thread_key = '${thread}'
ORDER BY s.start_time DESC
LIMIT 3;
SQL
        echo ""
    fi
    
    fmt_section "Recent High-Value Memories:"
    sqlite3 "$DB" <<-SQL
SELECT 
    item_type,
    importance as imp,
    substr(content, 1, 80)
FROM memory_items
WHERE importance >= 5 
    OR item_type IN ('decision', 'insight')
ORDER BY created_at DESC
LIMIT 8;
SQL
    
    echo ""
    fmt_section "Active Entities:"
    sqlite3 "$DB" <<-SQL
SELECT 
    entity_type || ': ' || name as entity,
    mention_count as mentions,
    current_status
FROM entities
WHERE last_mentioned > datetime('now', '-7 days')
ORDER BY mention_count DESC
LIMIT 8;
SQL
}

cmd_stats() {
    fmt_header "📊 Memory Database Statistics"
    echo ""
    
    sqlite3 "$DB" <<-SQL
SELECT '━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━' as "Statistics";
SELECT 'Sessions:      ' || COUNT(*) || ' (' || SUM(CASE WHEN is_archived THEN 1 ELSE 0 END) || ' archived)' FROM sessions;
SELECT 'Messages:      ' || COUNT(*) FROM messages;
SELECT 'Memories:      ' || COUNT(*) ||
    ' (' || SUM(CASE WHEN item_type='decision' THEN 1 ELSE 0 END) || ' decisions, ' ||
    SUM(CASE WHEN item_type='insight' THEN 1 ELSE 0 END) || ' insights)' FROM memory_items;
SELECT 'Entities:      ' || COUNT(*) FROM entities;
SELECT 'Topics:        ' || COUNT(*) FROM topics;
SELECT 'Threads:       ' || COUNT(*) FROM conversation_threads;
SELECT '━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━';

SELECT '' as '';
SELECT 'Memory by Type:' as "";
SELECT '  ' || item_type || ': ' || COUNT(*) FROM memory_items GROUP BY item_type ORDER BY COUNT(*) DESC;

SELECT '' as '';
SELECT 'Top Entities:' as "";
SELECT '  ' || name || ' (' || entity_type || '): ' || mention_count as ""
FROM entities ORDER BY mention_count DESC LIMIT 10;

SELECT '' as '';
SELECT 'Sync Status:' as "";
SELECT '  Last sync: ' || COALESCE(last_sync, 'Never') || 
    ' | Sessions: ' || sessions_synced || 
    ' | Memories: ' || memories_extracted
FROM sync_meta WHERE id = 1;
SQL
}

cmd_help() {
    cat <<EOF
MPM Memory Query System v3.4

Commands:
  search <term> [--limit=N]     Search across all memories
  recent [--hours=24]           Recent activity
  thread [name]                 Show thread or list active threads
  entity [name]                 Show entity or list top entities
  project [name]                Show project context or list projects
  decisions [--limit=15]        Key decisions
  context [thread]              Quick context assembly
  stats                         Database statistics
  help                          This help

Examples:
  $0 search "database schema"
  $0 recent --hours=48
  $0 thread mpm-dev
  $0 entity Flowbyte
  $0 project "MPM"
  $0 decisions --limit=20

Environment:
  MPM_WORKSPACE                 Override workspace path
EOF
}

# =====================================================
# Main
# =====================================================
ensure_db

cmd="${1:-stats}"
shift || true

case "$cmd" in
    search)
        cmd_search "$@"
        ;;
    recent)
        cmd_recent "$@"
        ;;
    thread)
        cmd_thread "$@"
        ;;
    entity)
        cmd_entity "$@"
        ;;
    project)
        cmd_project "$@"
        ;;
    decisions)
        cmd_decisions "$@"
        ;;
    context)
        cmd_context "$@"
        ;;
    stats|status)
        cmd_stats
        ;;
    help|--help|-h)
        cmd_help
        ;;
    *)
        echo "Unknown command: $cmd"
        echo "Run: $0 help"
        exit 1
        ;;
esac
