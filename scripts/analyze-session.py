#!/usr/bin/env python3
#
# MPM Analyze Session - Extract memories/entities/topics from session context
# Usage: ./analyze-session.py <session_id> ["context_text"]
# Trigger: ~m.mpm!analyze <id> [context]
#

import sys
import sqlite3
import json
import re
from pathlib import Path
from datetime import datetime
from collections import Counter

DB_PATH = Path(__file__).parent.parent / "memory" / "memory.db"

def extract_entities(text):
    """Extract entities from context with validation"""
    entities = []
    
    # Known project names (whitelist for high confidence)
    known_projects = {
        'MPM', 'Flowbyte', 'PicoClaw', 'SQLite', 'PostgreSQL', 'MySQL',
        'Docker', 'Kubernetes', 'React', 'Vue', 'WordPress', 'WooCommerce',
        'Laravel', 'Django', 'Flask', 'Express', 'MongoDB', 'Redis'
    }
    # Organizations: common companies
    known_orgs = {
        'Google', 'Microsoft', 'Amazon', 'Meta', 'OpenAI', 'Anthropic',
        'Claude', 'ChatGPT', 'GitHub', 'GitLab', 'Hostinger', 'WordPress'
    }
    
    # Detect CamelCase but require 2+ capital letters (not just sentence starters)
    projects = re.findall(r'\b([A-Z][a-z]+[A-Z][a-z]+(?:[A-Z][a-z]+)*)\b', text)
    for p in set(projects):
        if p in known_projects or len(p) >= 4 and sum(c.isupper() for c in p) >= 2:
            entities.append({"name": p, "type": "project", "confidence": 0.9})
    
    # Organizations - whitelist based
    for org in known_orgs:
        if re.search(r'\b' + re.escape(org) + r'\b', text, re.I):
            entities.append({"name": org, "type": "organization", "confidence": 0.9})
    
    # Tools - require surrounding context
    tools = re.findall(r'\b(SQLite|PostgreSQL|MySQL|Docker|PHP|Python|Go|Rust|JavaScript|TypeScript)\b', text, re.I)
    for t in set(tools):
        entities.append({"name": t, "type": "tool", "confidence": 0.8})
    
    # Client/customer detection from context patterns
    client_patterns = re.findall(r'\b(client\s+\w+|customer\s+\w+|for\s+([A-Z][a-z]+(?:\s+[A-Z][a-z]+)?))\b', text, re.I)
    for match in client_patterns:
        name = match[1] if match[1] else match[0]
        if name and name not in {'the', 'a', 'an'}:
            entities.append({"name": name, "type": "person", "confidence": 0.7})
    
    # Deduplicate by name
    seen = set()
    unique = []
    for e in entities:
        if e['name'] not in seen:
            seen.add(e['name'])
            unique.append(e)
    
    return unique

def extract_topics(text):
    """Extract key topics from text with filtering"""
    # Technical terms and keywords
    words = re.findall(r'\b[a-z]{5,15}\b', text.lower())
    
    # Comprehensive stop words - remove weak terms
    stop_words = {
        # Generic verbs
        'testing', 'tested', 'test', 'migrated', 'refactoring', 'working',
        'using', 'based', 'moved', 'created', 'adding', 'getting', 'doing',
        'looking', 'finding', 'making', 'taking', 'coming', 'going',
        # Generic nouns
        'manual', 'analysis', 'context', 'content', 'information', 'reference',
        'something', 'anything', 'everything', 'nothing', 'things',
        # Connectors
        'because', 'therefore', 'however', 'meanwhile', 'although', 'since'
    }
    
    # Technical keywords that ARE interesting (whitelist)
    tech_keywords = {
        'database', 'schema', 'migration', 'sqlite', 'postgres', 'mongodb',
        'api', 'rest', 'graphql', 'frontend', 'backend', 'fullstack',
        'deployment', 'docker', 'kubernetes', 'ci', 'cd', 'pipeline',
        'memory', 'caching', 'search', 'indexing', 'fts5', 'query',
        'session', 'storage', 'backup', 'sync', 'export', 'import'
    }
    
    filtered = [w for w in words if w not in stop_words or w in tech_keywords]
    
    # Prefer technical keywords
    topics = Counter(w for w in filtered if w in tech_keywords).most_common(5)
    if len(topics) < 5:
        # Fill remainder from general terms
        general = Counter(w for w in filtered if w not in tech_keywords).most_common(5 - len(topics))
        topics.extend(general)
    
    return [t[0] for t in topics]

def get_or_create_entity(conn, cur, entity):
    """Get existing entity or create new one"""
    cur.execute('''SELECT id FROM entities WHERE name = ? AND entity_type = ?''',
                (entity['name'], entity['type']))
    result = cur.fetchone()
    
    if result:
        entity_id = result[0]
        cur.execute('''UPDATE entities 
                       SET last_mentioned = ?, mention_count = mention_count + 1
                       WHERE id = ?''',
                    (datetime.now().isoformat(), entity_id))
    else:
        cur.execute('''INSERT INTO entities (entity_type, name, description)
                       VALUES (?, ?, ?)''',
                    (entity['type'], entity['name'], f"Extracted from session"))
        entity_id = cur.lastrowid
    
    return entity_id

def get_or_create_topic(conn, cur, topic_name):
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

def analyze_session(session_id, context_text):
    """Main analysis function"""
    print(f"[MPM] Analyzing session {session_id}...")
    print(f"[MPM] Context: {context_text[:60]}...")
    
    if not DB_PATH.exists():
        print(f"[MPM] ERROR: Database not found at {DB_PATH}")
        return None
    
    conn = sqlite3.connect(DB_PATH)
    cur = conn.cursor()
    
    # Verify session exists
    cur.execute('SELECT id FROM sessions WHERE id = ?', (session_id,))
    if not cur.fetchone():
        print(f"[MPM] ERROR: Session {session_id} not found")
        return None
    
    print(f"[MPM] Extracting entities...")
    entities = extract_entities(context_text)
    entity_ids = []
    for entity in entities:
        eid = get_or_create_entity(conn, cur, entity)
        entity_ids.append(eid)
        print(f"  - {entity['name']} ({entity['type']})")
    
    print(f"[MPM] Extracting topics...")
    topics = extract_topics(context_text)
    topic_ids = []
    for topic in topics:
        tid = get_or_create_topic(conn, cur, topic)
        topic_ids.append(tid)
        print(f"  - {topic}")
    
    # Insert memory items
    print(f"[MPM] Creating memory entries...")
    
    # Decision/context memory
    cur.execute('''INSERT INTO memory_items 
                   (item_type, content, summary, source_session_id, confidence)
                   VALUES (?, ?, ?, ?, ?)''',
                ('decision', context_text, 
                 context_text[:80] + "..." if len(context_text) > 80 else context_text,
                 session_id, 0.8))
    memory_id = cur.lastrowid
    print(f"  - decision: memory_{memory_id}")
    
    # Facts - split sentences
    sentences = re.split(r'[.!?]+', context_text)
    fact_count = 0
    for sentence in sentences:
        sentence = sentence.strip()
        if len(sentence) > 20 and len(sentence) < 200:
            cur.execute('''INSERT INTO memory_items 
                           (item_type, content, summary, source_session_id, confidence)
                           VALUES (?, ?, ?, ?, ?)''',
                        ('fact', sentence, sentence[:60] + "..." if len(sentence) > 60 else sentence,
                         session_id, 0.7))
            fact_id = cur.lastrowid
            fact_count += 1
    print(f"  - {fact_count} facts extracted")
    
    # Create relations
    print(f"[MPM] Creating relations...")
    
    # Memory -> Entities
    for eid in entity_ids:
        cur.execute('''INSERT INTO relations 
                       (source_type, source_id, relation_type, target_type, target_id)
                       VALUES (?, ?, 'mentions', ?, ?)''',
                    ('memory', memory_id, 'entity', eid))
    
    # Memory -> Topics
    for tid in topic_ids:
        cur.execute('''INSERT INTO relations
                       (source_type, source_id, relation_type, target_type, target_id)
                       VALUES (?, ?, 'related_to', ?, ?)''',
                    ('memory', memory_id, 'topic', tid))
    
    # Session -> Entities (mentioned in)
    for eid in entity_ids:
        cur.execute('''INSERT INTO relations
                       (source_type, source_id, relation_type, target_type, target_id)
                       VALUES (?, ?, 'mentions', ?, ?)''',
                    ('session', session_id, 'entity', eid))
    
    conn.commit()
    conn.close()
    
    print(f"")
    print(f"[MPM] ✓ Analysis complete!")
    print(f"  Session: {session_id}")
    print(f"  Entities: {len(entity_ids)}")
    print(f"  Topics: {len(topic_ids)}")
    print(f"  Total memories: {fact_count + 1}")
    print(f"")
    print(f"Query with: ./scripts/search-memories.sh \"your topic\"")
    
    return memory_id

if __name__ == "__main__":
    if len(sys.argv) < 2:
        print("Usage: ./analyze-session.py <session_id> [\"context text\"]")
        print("       ./analyze-session.py 1 \"Migrated database schema...\"")
        sys.exit(1)
    
    session_id = int(sys.argv[1])
    context = sys.argv[2] if len(sys.argv) > 2 else "Manual analysis"
    analyze_session(session_id, context)
