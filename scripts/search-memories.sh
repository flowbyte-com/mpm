#!/bin/bash
#
# MPM Search - Query memories using FTS5
# Usage: ./search-memories.sh "query" [limit]
#

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MEMORY_DIR="$SCRIPT_DIR/../memory"
DB="$MEMORY_DIR/memory.db"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m'

log_info() { echo -e "${GREEN}[MPM]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[MPM]${NC} $1"; }
log_error() { echo -e "${RED}[MPM]${NC} $1"; }

# ===== Main =====

if [[ -z "$1" ]]; then
    echo "Usage: $0 \"your search query\" [limit]"
    exit 1
fi

QUERY="$1"
LIMIT="${2:-10}"

if [[ ! -f "$DB" ]]; then
    log_error "Database not found at $DB"
    exit 1
fi

log_info "Searching for: $QUERY"
echo ""

# Search FTS index
echo -e "${CYAN}=== Memories ===${NC}"
sqlite3 "$DB" <<SQL
SELECT 
    'Memory_' || mi.id,
    '[' || mi.item_type || ']',
    substr(mi.summary || ' ' || mi.content, 0, 100) || '...',
    ' (conf: ' || mi.confidence || ')'
FROM memory_fts fts
JOIN memory_items mi ON mi.id = fts.rowid
WHERE memory_fts MATCH '$QUERY'
LIMIT $LIMIT;
SQL

echo ""
echo -e "${CYAN}=== Messages ===${NC}"
sqlite3 "$DB" <<SQL
SELECT 
    'Message_' || m.id,
    '[' || m.role || ']',
    substr(m.content, 0, 100) || '...',
    ' (' || datetime(m.timestamp, 'unixepoch') || ')'
FROM message_fts fts
JOIN messages m ON m.id = fts.rowid
WHERE message_fts MATCH '$QUERY'
LIMIT $LIMIT;
SQL

echo ""
echo -e "${CYAN}=== Entities ===${NC}"
sqlite3 "$DB" <<SQL
SELECT 
    name,
    '[' || entity_type || ']',
    'mentioned ' || mention_count || ' times'
FROM entities
WHERE name LIKE '%${QUERY}%'
LIMIT $LIMIT;
SQL

echo ""
echo -e "${CYAN}=== Topics ===${NC}"
sqlite3 "$DB" <<SQL
SELECT 
    topic_name,
    mention_count || ' mentions'
FROM topics
WHERE topic_name LIKE '%${QUERY}%'
ORDER BY mention_count DESC
LIMIT $LIMIT;
SQL
