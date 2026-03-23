#!/usr/bin/env python3
#
# MPM Analyze Session v3.4
# Extract memories/entities/topics from session context
# Enhanced with decision detection and priority scoring
#

import sys
import sqlite3
import json
import re
from pathlib import Path
from datetime import datetime
from collections import Counter

# Determine paths
SCRIPT_DIR = Path(__file__).parent
DB_PATH = SCRIPT_DIR.parent / "memory" / "memory.db"

# High-value patterns
DECISION_PATTERNS = [
    r'\b(decided|decision|we chose|went with|opted for|settled on)\b',
    r'\b(agreed|consensus|final choice|selected|picked)\b',
    r'\b(will|going to|plan to|need to|should|must)\s+\w+\b',
    r'\b(build|create|implement|add|fix|update|delete)\s+(this|the)\b',
]

PREFERENCE_PATTERNS = [
    r'\b(prefer|like|want|dislike|hate|avoid)\b',
    r'\b(good|bad|better|worse|best|worst)\s+(to|for|than)\b',
    r'\b(important|critical|key|essential)\b',
]

INSIGHT_PATTERNS = [
    r'\b(realized|understood|found that|discovered|see that)\b',
    r'\b(the (issue|problem|solution) is|root cause)\b',
    r'\b(turns out|meaning|indicates|shows)\b',
]

KNOWN_PROJECTS = {
    'MPM', 'Flowbyte', 'PicoClaw', 'SymAI', 'OpenClaw',
    'WordPress', 'WooCommerce', 'Laravel', 'Django',
    'React', 'Vue', 'Svelte', 'Angular',
    'PostgreSQL', 'MySQL', 'MongoDB', 'Redis', 'SQLite',
    'Docker', 'Kubernetes', 'AWS', 'GCP', 'Azure',
}

KNOWN_TOOLS = {
    'Python', 'JavaScript', 'TypeScript', 'Go', 'Rust', 'PHP',
    'Bash', 'Shell', 'SQL', 'SQLite',
    'Git', 'GitHub', 'GitLab',
    'VSCode', 'Vim', 'Neovim', 'Cursor',
    'Figma', 'Notion', 'Slack',
}

KNOWN_ORGS = {
    'Google', 'Microsoft', 'Amazon', 'Meta', 'OpenAI', 'Anthropic',
    'Claude', 'ChatGPT', 'Perplexity', 'GitHub', 'GitLab', 'Hostinger',
    'Sipeed', 'Flowbyte',
}

def get_session_messages(conn, session_id):
    """Fetch all messages for a session"""
    cur = conn.cursor()
    cur.execute('''
        SELECT msg_index, role, content, content_type
        FROM messages 
        WHERE session_id = ? 
        ORDER BY msg_index
    ''', (session_id,))
    return cur.fetchall()

def extract_user_intent(messages):
    """Analyze first few user messages to determine intent"""
    user_msgs = [m for m in messages if m[1] == 'user']
    if not user_msgs:
        return "Unknown"
    
    first_msg = user_msgs[0][2] if len(user_msgs[0]) > 2 else ""
    
    # Pattern matching
    if re.search(r'\b(create|make|build|implement|set up)\b', first_msg, re.I):
        return "Creation request"
    elif re.search(r'\b(fix|debug|solve|error|problem|issue)\b', first_msg, re.I):
        return "Problem solving"
    elif re.search(r'\b(explain|how|what|why)\b', first_msg, re.I):
        return "Information seeking"
    elif re.search(r'\b(review|check|look at|analyze)\b', first_msg, re.I):
        return "Review/Analysis"
    else:
        return first_msg[:60] + "..." if len(first_msg) > 60 else first_msg

def extract_ai_actions(messages):
    """Summarize what the AI did in response"""
    ai_msgs = [m for m in messages if m[1] == 'assistant']
    actions = []
    
    for msg in ai_msgs[:5]:  # Check first few
        content = msg[2] if len(msg) > 2 else ""
        
        if re.search(r'```\w+', content):
            actions.append("provided code")
        if re.search(r'wrote.*file', content, re.I) or re.search(r'functions?\.', content):
            actions.append("wrote files")
        if re.search(r'identified|found|discovered', content, re.I):
            actions.append("analyzed")
        if re.search(r'suggest|recommend|should', content, re.I):
            actions.append("made recommendations")
    
    return ", ".join(actions) if actions else "Responded"

def classify_outcome(messages):
    """Determine if conversation was completed or pending"""
    if not messages:
        return "unknown"
    
    last_msgs = messages[-3:]  # Last 3 messages
    last_content = " ".join([m[2] for m in last_msgs if len(m) > 2])
    
    completion_indicators = [
        r'\b(complete|done|finished|success|solved)\b',
        r'\b(let me know|anything else|happy to help)\b',
        r'\b(working now|looks good|perfect)\b',
    ]
    
    for pattern in completion_indicators:
        if re.search(pattern, last_content, re.I):
            return "completed"
    
    continuation_indicators = [
        r'\?(\s|$)',  # Ends with question
        r'\b(next|then|after that|when)\b',
        r'\b(need to|should|will)\s+\w+\b',
    ]
    
    for pattern in continuation_indicators:
        if re.search(pattern, last_content, re.I):
            return "ongoing"
    
    return "completed"

def detect_decisions(all_content, messages):
    """Find decision points in the conversation"""
    decisions = []
    
    # Look for decision patterns
    for pattern in DECISION_PATTERNS:
        matches = re.finditer(pattern, all_content, re.I)
        for match in matches:
            # Get context around the match
            start = max(0, match.start() - 100)
            end = min(len(all_content), match.end() + 100)
            context = all_content[start:end].strip()
            
            # Clean up context
            context = re.sub(r'\s+', ' ', context)
            if len(context) > 200:
                context = context[:200] + "..."
            
            decisions.append({
                'text': context,
                'confidence': 0.9,
                'type': 'decision'
            })
    
    return decisions[:5]  # Top 5 decisions

def detect_preferences(all_content):
    """Find stated preferences"""
    prefs = []
    for pattern in PREFERENCE_PATTERNS:
        matches = re.finditer(pattern, all_content, re.I)
        for match in matches:
            start = max(0, match.start() - 80)
            end = min(len(all_content), match.end() + 80)
            context = all_content[start:end].strip()
            context = re.sub(r'\s+', ' ', context)
            if len(context) > 150:
                context = context[:150] + "..."
            prefs.append({'text': context, 'confidence': 0.75, 'type': 'preference'})
    return prefs[:3]

def detect_insights(all_content):
    """Find realizations/insights"""
    insights = []
    for pattern in INSIGHT_PATTERNS:
        matches = re.finditer(pattern, all_content, re.I)
        for match in matches:
            start = max(0, match.start() - 100)
            end = min(len(all_content), match.end() + 100)
            context = all_content[start:end].strip()
            context = re.sub(r'\s+', ' ', context)
            if len(context) > 180:
                context = context[:180] + "..."
            insights.append({'text': context, 'confidence': 0.8, 'type': 'insight'})
    return insights[:3]

def extract_entities(all_content, decisions, insights):
    """Extract named entities from content"""
    entities = []
    
    # Projects - CamelCase with known list
    for proj in KNOWN_PROJECTS:
        if re.search(r'\b' + re.escape(proj) + r'\b', all_content, re.I):
            entities.append({"name": proj, "type": "project", "confidence": 0.95})
    
    # Additional CamelCase detection
    camel = re.findall(r'\b([A-Z][a-z]+(?:[A-Z][a-z]+)+)\b', all_content)
    for c in set(camel):
        if c not in KNOWN_PROJECTS and len(c) >= 3:
            entities.append({"name": c, "type": "project", "confidence": 0.6})
    
    # Tools
    for tool in KNOWN_TOOLS:
        if re.search(r'\b' + re.escape(tool) + r'\b', all_content, re.I):
            entities.append({"name": tool, "type": "tool", "confidence": 0.9})
    
    # Organizations
    for org in KNOWN_ORGS:
        if re.search(r'\b' + re.escape(org) + r'\b', all_content, re.I):
            entities.append({"name": org, "type": "organization", "confidence": 0.9})
    
    # Technologies mentioned in code blocks
    code_langs = re.findall(r'```(\w+)', all_content)
    for lang in set(code_langs):
        entities.append({"name": lang, "type": "tool", "confidence": 0.85})
    
    # Files - look for path mentions
    files = re.findall(r'\b([\w\-]+\.(?:py|js|ts|go|rs|php|sql|sh|yaml|yml|json|md|txt))\b', all_content, re.I)
    for f in set(files):
        entities.append({"name": f, "type": "file", "confidence": 0.7})
    
    return entities

def extract_topics(all_content):
    """Extract key topics with technical weighting"""
    # Tech terms (high weight)
    tech_terms = {
        'database', 'schema', 'migration', 'sqlite', 'postgres', 'mongodb',
        'api', 'rest', 'graphql', 'frontend', 'backend', 'fullstack',
        'deployment', 'docker', 'kubernetes', 'pipeline', 'ci', 'cd',
        'memory', 'caching', 'search', 'indexing', 'fts5', 'query',
        'session', 'storage', 'backup', 'sync', 'export', 'import',
        'optimization', 'performance', 'security', 'testing',
        'plugin', 'skill', 'persona', 'mode', 'automation', 'script',
        'workflow', 'integration', 'mcp', 'llm', 'ai',
    }
    
    # Get all words
    words = re.findall(r'\b[a-z]{4,15}\b', all_content.lower())
    
    # Stop words
    stop_words = {
        'this', 'that', 'with', 'from', 'have', 'been', 'were', 'they',
        'their', 'there', 'would', 'could', 'should', 'about',
    }
    
    # Score words
    scored = {}
    for w in words:
        if w in stop_words:
            continue
        if w in tech_terms:
            scored[w] = scored.get(w, 0) + 3
        else:
            scored[w] = scored.get(w, 0) + 1
    
    # Get top topics
    top = sorted(scored.items(), key=lambda x: x[1], reverse=True)[:8]
    return [t[0] for t in top]

def calculate_importance(decisions, insights, entities, topics):
    """Calculate importance score based on content value"""
    score = 3  # Base
    
    # Decisions are highest value
    score += len(decisions) * 2
    
    # Insights add value
    score += len(insights)
    
    # Project entities bump priority
    proj_count = len([e for e in entities if e['type'] == 'project'])
    score += proj_count
    
    # Cap at 10
    return min(score, 10)

def get_or_create_entity(conn, cur, entity, session_id):
    """Get existing entity or create new, link to session"""
    cur.execute('''SELECT id, mention_count FROM entities WHERE name = ? AND entity_type = ?''',
                (entity['name'], entity['type']))
    result = cur.fetchone()
    
    if result:
        entity_id, count = result
        cur.execute('''UPDATE entities 
                       SET last_mentioned = ?, mention_count = mention_count + 1
                       WHERE id = ?''',
                    (datetime.now().isoformat(), entity_id))
    else:
        cur.execute('''INSERT INTO entities (entity_type, name, description, canonical_name)
                       VALUES (?, ?, ?, ?)''',
                    (entity['type'], entity['name'], 
                     f"Mentioned in session {session_id}",
                     entity['name'].lower().replace(' ', '_')))
        entity_id = cur.lastrowid
    
    return entity_id

def get_or_create_topic(conn, cur, topic_name, session_id):
    """Get existing topic or create new"""
    cur.execute('SELECT id FROM topics WHERE topic_name = ?', (topic_name,))
    result = cur.fetchone()
    
    if result:
        topic_id = result[0]
        cur.execute('''UPDATE topics 
                       SET last_active = ?, mention_count = mention_count + 1
                       WHERE id = ?''',
                    (datetime.now().isoformat(), topic_id))
    else:
        cur.execute('''INSERT INTO topics (topic_name, keywords)
                       VALUES (?, ?)''', (topic_name, topic_name))
        topic_id = cur.lastrowid
    
    return topic_id

def insert_memory_item(conn, cur, item, session_id, key_terms_str, idx):
    """Insert a memory item"""
    summary = item['text'][:120] + "..." if len(item['text']) > 120 else item['text']
    
    cur.execute('''
        INSERT INTO memory_items 
        (item_type, content, summary, key_terms, source_session_id, 
         confidence, importance, extracted_by)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)
    ''', (item['type'], item['text'], summary, key_terms_str, 
          session_id, item['confidence'], 5 if item['type'] == 'decision' else 3, 'auto'))
    
    return cur.lastrowid

def analyze_session(session_id, jsonl_file=None):
    """Main analysis function"""
    
    if not DB_PATH.exists():
        print(f"[MPM] ERROR: Database not found at {DB_PATH}")
        return None
    
    conn = sqlite3.connect(DB_PATH)
    cur = conn.cursor()
    
    # Verify session
    cur.execute('SELECT id, summary FROM sessions WHERE id = ?', (session_id,))
    sess = cur.fetchone()
    if not sess:
        print(f"[MPM] ERROR: Session {session_id} not found")
        return None
    
    print(f"[MPM] Analyzing session {session_id}...")
    
    # Get messages
    messages = get_session_messages(conn, session_id)
    if not messages:
        print(f"[MPM] WARN: No messages found for session {session_id}")
        conn.close()
        return None
    
    all_content = "\n".join([m[2] for m in messages if len(m) > 2])
    
    # Extract user intent and AI actions
    user_intent = extract_user_intent(messages)
    ai_actions = extract_ai_actions(messages)
    outcome = classify_outcome(messages)
    
    print(f"  Intent: {user_intent[:50]}...")
    print(f"  Actions: {ai_actions}")
    
    # Detect content
    decisions = detect_decisions(all_content, messages)
    preferences = detect_preferences(all_content)
    insights = detect_insights(all_content)
    
    print(f"  Found: {len(decisions)} decisions, {len(preferences)} preferences, {len(insights)} insights")
    
    # Extract entities
    entities = extract_entities(all_content, decisions, insights)
    entity_ids = []
    for e in entities:
        eid = get_or_create_entity(conn, cur, e, session_id)
        entity_ids.append(eid)
        if e['confidence'] > 0.7:
            print(f"    Entity: {e['name']} ({e['type']})")
    
    # Extract topics
    topics = extract_topics(all_content)
    topic_ids = []
    for t in topics:
        tid = get_or_create_topic(conn, cur, t, session_id)
        topic_ids.append(tid)
    print(f"  Topics: {', '.join(topics[:5])}")
    
    # Calculate importance
    importance = calculate_importance(decisions, insights, entities, topics)
    
    # Build key terms string
    key_terms = topics + [e['name'] for e in entities]
    key_terms_str = ", ".join(set(key_terms))[:200]  # Limit length
    
    # Insert memory items
    memory_ids = []
    
    for d in decisions:
        d['type'] = 'decision'
        mid = insert_memory_item(conn, cur, d, session_id, key_terms_str, len(memory_ids))
        memory_ids.append((mid, 'decision'))
    
    for p in preferences:
        mid = insert_memory_item(conn, cur, p, session_id, key_terms_str, len(memory_ids))
        memory_ids.append((mid, 'preference'))
    
    for i in insights:
        mid = insert_memory_item(conn, cur, i, session_id, key_terms_str, len(memory_ids))
        memory_ids.append((mid, 'insight'))
    
    # Add general facts (key sentences)
    sentences = re.split(r'[.!?]+', all_content)
    fact_count = 0
    for sent in sentences[:20]:  # First 20 sentences
        sent = sent.strip()
        if len(sent) > 30 and len(sent) < 200:
            # Skip if too generic
            if not re.search(r'^[A-Za-z]+,? (I|we|you|they) ', sent):
                cur.execute('''
                    INSERT INTO memory_items 
                    (item_type, content, summary, key_terms, source_session_id, confidence)
                    VALUES (?, ?, ?, ?, ?, ?)
                ''', ('fact', sent, sent[:80] + "..." if len(sent) > 80 else sent,
                      key_terms_str, session_id, 0.6))
                fact_count += 1
    
    # Create relations
    for mid, mtype in memory_ids:
        # Memory -> Entities
        for eid in entity_ids:
            cur.execute('''INSERT OR IGNORE INTO relations 
                (source_type, source_id, relation_type, target_type, target_id)
                VALUES (?, ?, 'mentions', ?, ?)''',
                ('memory', mid, 'entity', eid))
        
        # Memory -> Topics
        for tid in topic_ids:
            cur.execute('''INSERT OR IGNORE INTO relations
                (source_type, source_id, relation_type, target_type, target_id)
                VALUES (?, ?, 'related_to', ?, ?)''',
                ('memory', mid, 'topic', tid))
        
        # Session -> Memory
        cur.execute('''INSERT OR IGNORE INTO relations
            (source_type, source_id, relation_type, target_type, target_id)
            VALUES (?, ?, 'contains', ?, ?)''',
            ('session', session_id, 'memory', mid))
    
    # Update session with extracted info
    cur.execute('''
        UPDATE sessions 
        SET user_intent = ?, ai_actions = ?, outcome_status = ?, importance = ?
        WHERE id = ?
    ''', (user_intent, ai_actions, outcome, importance, session_id))
    
    conn.commit()
    conn.close()
    
    print(f"")
    print(f"[MPM] ✓ Analysis complete!")
    print(f"  Session: {session_id}")
    print(f"  Importance: {importance}/10")
    print(f"  Entities: {len(entity_ids)}")
    print(f"  Topics: {len(topics)}")
    print(f"  Memories: {len(memory_ids)} structured + ~{fact_count} facts")
    
    return len(memory_ids)

if __name__ == "__main__":
    if len(sys.argv) < 2:
        print("Usage: ./analyze-session-v3.4.py <session_id> [jsonl_file]")
        sys.exit(1)
    
    session_id = int(sys.argv[1])
    jsonl_file = sys.argv[2] if len(sys.argv) > 2 else None
    analyze_session(session_id, jsonl_file)
