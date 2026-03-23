#!/bin/bash

# Session Processor - Extract, summarize, and archive sessions
# Usage: ./process-sessions.sh [--dry-run]
#
# Improves on sync-sessions.sh:
# 1. Extracts key info to categories (not just file tracking)
# 2. Creates brief summaries ( distill full logs )
# 3. Adds importance scoring

WORKSPACE="$HOME/.openclaw/workspace"
MPM_MEMORY="$WORKSPACE/MPM/memory"
SESSIONS="$MPM_MEMORY/sessions"
DB="$MPM_MEMORY/memory.db"

DRY_RUN=false
[ "$1" = "--dry-run" ] && DRY_RUN=true

echo "🔄 Session Processing"
echo "===================="
echo ""

# Ensure DB has importance column
if [ "$DRY_RUN" = false ]; then
    sqlite3 "$DB" "ALTER TABLE memory_entries ADD COLUMN importance INTEGER DEFAULT 0;" 2>/dev/null
    sqlite3 "$DB" "ALTER TABLE memory_entries ADD COLUMN summary TEXT;" 2>/dev/null
fi

# Security: Sanitize sensitive data from content
# Replaces API keys, passwords, tokens with [REDACTED]
sanitize() {
    local input="$1"
    
    # Check common env vars for secrets and redact if present in content
    # Catches things like $OPENAI_API_KEY appearing in context
    local sensitive_vars=(
        "OPENAI_API_KEY" "ANTHROPIC_API_KEY" "AWS_ACCESS_KEY" "AWS_SECRET_KEY"
        "DATABASE_URL" "DB_PASSWORD" "POSTGRES_PASSWORD" "MYSQL_PASSWORD"
        "GITHUB_TOKEN" "GITLAB_TOKEN" "SLACK_TOKEN" "DISCORD_TOKEN"
        "STRIPE_KEY" "RAZORPAY_KEY" "SENDGRID_KEY" "TWILIO_KEY"
    )
    
    for var in "${sensitive_vars[@]}"; do
        val="${!var}"
        if [ -n "$val" ] && [ ${#val} -gt 5 ]; then
            prefix="${val:0:8}"
            input=$(echo "$input" | sed -E "s/$prefix[a-zA-Z0-9_-]{4,}/[REDACTED]/g")
        fi
    done
    
    # Redact API keys (sk-, glpat-, common patterns)
    input=$(echo "$input" | sed -E 's/sk-[a-zA-Z0-9]{18,}/[REDACTED_KEY]/g')
    input=$(echo "$input" | sed -E 's/glpat-[a-zA-Z0-9]{18,}/[REDACTED_KEY]/g')
    input=$(echo "$input" | sed -E 's/pk_[a-zA-Z0-9]{18,}/[REDACTED_KEY]/g')
    # Redact tokens after "token:", "key:", "password:" (8+ chars)
    input=$(echo "$input" | sed -E 's/(token|key|password|secret)[=:][" ]?[a-zA-Z0-9_-]{8,}/[REDACTED]/gi')
    # Redact JWT-like tokens
    input=$(echo "$input" | sed -E 's/eyJ[a-zA-Z0-9_-]{10,}\.[a-zA-Z0-9_-]{10,}\.[a-zA-Z0-9_-]{10,}/[REDACTED_JWT]/g')
    echo "$input"
}

# Keywords for extraction + importance (higher = more important)
# Use | for OR matching
declare -A EXTRACT_RULES=(
    ["lesson"]="learned|lesson|realized|discovered|mistake|error|bug|fix|warning"
    ["decision"]="decided|chose|going with|settled on|agreed|will do"
    ["project"]="project|website|site|build|created|working on|developing"
    ["idea"]="idea|concept|insight|think|maybe|could|should"
    ["preference"]="prefer|like|hate|want|dont want|enjoys"
    ["tool"]="tool|script|setup|config|installed|configured"
    ["contact"]="person|user|developer|team member"
)

# Process each session file
total_extracted=0
declare -i total_extracted

for session in "$SESSIONS"/*.md; do
    [ -f "$session" ] || continue
    
    filename=$(basename "$session")
    echo "📄 Processing: $filename"
    
    # Extract timestamp from filename
    timestamp=$(echo "$filename" | sed 's/.md$//')
    
    # Get full content for analysis
    content=$(cat "$session")
    
    # Extract key items to categories
    grep -E "\*\*[0-9]{4}-[0-9]{2}-[0-9]{2}" "$session" | while read -r line; do
        [ -z "$line" ] && continue
        
        # Check each category
        for cat in lesson decision project idea preference tool contact; do
            keywords="${EXTRACT_RULES[$cat]}"
            if echo "$line" | grep -qiE "$keywords"; then
                # Extract meaningful content after timestamp, then sanitize
                raw_content=$(echo "$line" | sed 's/.*\*\*[0-9-]* [0-9:]*\*\* — //' | tr -d '"' | head -c 200)
                clean_content=$(sanitize "$raw_content")
                
                if [ -n "$clean_content" ] && [ ${#clean_content} -gt 10 ]; then
                    # Calculate importance (0-10)
                    importance=3
                    echo "$line" | grep -qiE "important|critical|urgent|error|bug|fix" && importance=8
                    echo "$line" | grep -qiE "decided|chose|going with" && importance=7
                    
                    if [ "$DRY_RUN" = true ]; then
                        echo "  → [$cat] (imp:$importance) $clean_content"
                    else
                        # Add to category file
                        echo "**$timestamp** — $clean_content" >> "$MPM_MEMORY/$cat.md"
                        
                        # Add to DB with importance + summary (skip duplicates)
                        sqlite3 "$DB" "INSERT OR IGNORE INTO memory_entries 
                            (category, filename, content, timestamp, source, importance, summary) 
                            VALUES ('$cat', '$filename', '$clean_content', '$timestamp', 'extracted', $importance, '');"
                        
                        ((total_extracted++))
                        echo "  → [$cat] extracted (imp: $importance)"
                    fi
                    break  # One category per line
                fi
            fi
        done
    done
    
    # Create summary (first 3 meaningful lines), sanitized
    if [ "$DRY_RUN" = false ]; then
        raw_summary=$(grep -v "^#" "$session" | grep -v "^---" | grep -v "^$" | head -3 | tr '\n' ' ' | head -c 150)
        summary=$(sanitize "$raw_summary")
        
        # Upsert session entry with summary
        existing=$(sqlite3 "$DB" "SELECT id FROM memory_entries WHERE category='session' AND filename='$filename' LIMIT 1;")
        if [ -n "$existing" ]; then
            sqlite3 "$DB" "UPDATE memory_entries SET summary='$summary' WHERE id='$existing';"
        else
            sqlite3 "$DB" "INSERT OR IGNORE INTO memory_entries (category, filename, content, timestamp, source, importance, summary) 
                VALUES ('session', '$filename', '', '$timestamp', 'file', 1, '$summary');"
        fi
    fi
done

# Update memory meta
if [ "$DRY_RUN" = false ]; then
    echo ""
    echo "📊 Updating meta..."
    
    for cat in lesson decision project idea preference tool contact subject; do
        count=$(sqlite3 "$DB" "SELECT COUNT(*) FROM memory_entries WHERE category='$cat';")
        sqlite3 "$DB" "INSERT OR REPLACE INTO memory_meta (category, entry_count, last_synced) 
            VALUES ('$cat', $count, datetime('now'));"
    done
fi

echo ""
echo "📊 DB Status (top by importance)"
echo "--------------------------------"
sqlite3 "$DB" "SELECT category, filename, importance, substr(content, 1, 40) as content 
    FROM memory_entries WHERE importance > 0 ORDER BY importance DESC LIMIT 10;"

echo ""
echo "📈 Summary Stats"
echo "----------------"
sqlite3 "$DB" "SELECT category, COUNT(*) as count FROM memory_entries GROUP BY category ORDER BY count DESC;"

echo ""
if [ "$DRY_RUN" = true ]; then
    echo "⚠️  Dry run complete"
else
    echo "✅ Processed: $total_extracted items extracted"
fi