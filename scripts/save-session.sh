#!/bin/bash
#
# MPM Save Session - Manual session capture for non-persistent systems
# Usage: ./save-session.sh ["context summary"] ["channel"] ["importance(1-5)"]
# Trigger: ~m.mpm!ss

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MEMORY_DIR="$SCRIPT_DIR/../memory"
DB="$MEMORY_DIR/memory.db"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

log_info() { echo -e "${GREEN}[MPM]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[MPM]${NC} $1"; }
log_error() { echo -e "${RED}[MPM]${NC} $1"; }
log_step() { echo -e "${BLUE}[MPM]${NC} $1"; }

# ===== Main =====

# Check database exists
if [[ ! -f "$DB" ]]; then
    log_error "Database not found at $DB"
    log_info "Run: cd ~/.picoclaw/workspace/mpm && ./scripts/init-db.sh"
    exit 1
fi

# Get parameters or use defaults
CONTEXT="${1:-Manual session save}"
CHANNEL="${2:-cli}"
IMPORTANCE="${3:-3}"

# Generate session key
SESSION_KEY="manual_$(date +%s)_$(echo $RANDOM | head -c 4)"
START_TIME=$(date -u +"%Y-%m-%dT%H:%M:%SZ")

log_step "=== MPM Session Save ==="
echo ""
echo "  Channel:    $CHANNEL"
echo "  Context:    $CONTEXT"
echo "  Importance: $IMPORTANCE/5"
echo ""
log_info "Saving session..."

# Insert session (ended_at = start_time for manual saves, agent can update)
sqlite3 "$DB" <<SQL
INSERT INTO sessions (session_key, channel, start_time, end_time, summary, importance)
VALUES ('$SESSION_KEY', '$CHANNEL', '$START_TIME', '$START_TIME', 
        '$(echo "$CONTEXT" | sed "s/'/''/g")', $IMPORTANCE);
SQL

# Get the created session ID
SESSION_ID=$(sqlite3 "$DB" "SELECT id FROM sessions WHERE session_key = '$SESSION_KEY';")

if [[ -z "$SESSION_ID" ]]; then
    log_error "Failed to create session"
    exit 1
fi

log_info "✓ Session created: ID=$SESSION_ID, key=$SESSION_KEY"
echo ""
echo -e "${YELLOW}[ACTION REQUIRED]${NC}"
echo -e "Run: ${BLUE}~m.mpm!analyze $SESSION_ID${NC}"
echo ""
echo "Or manually:"
echo "  ./scripts/analyze-session.py $SESSION_ID \"<full context>\""
