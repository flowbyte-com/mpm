-- MPM Memory Schema v3.4 - AI-Optimized for Retrieval
-- Focus: Fast context lookup, conversation continuity, priority ranking

-- =====================================================
-- CORE: Sessions with enhanced metadata for AI continuity
-- =====================================================
CREATE TABLE IF NOT EXISTS sessions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_key TEXT UNIQUE NOT NULL,
    -- Identifiers
    channel TEXT,
    thread_id TEXT,                    -- Links related sessions (continuations)
    parent_session_id INTEGER REFERENCES sessions(id),
    
    -- Timing
    start_time TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    end_time TIMESTAMP,
    duration_seconds INTEGER,
    
    -- Content summary
    summary TEXT,                      -- AI-generated summary
    user_intent TEXT,                  -- What did user want to accomplish?
    ai_actions TEXT,                   -- What did I do in response?
    outcome_status TEXT CHECK(outcome_status IN ('completed', 'pending', 'blocked', 'ongoing')),
    
    -- Relevance scoring
    importance INTEGER DEFAULT 3 CHECK(importance BETWEEN 1 AND 10),
    relevance_score REAL DEFAULT 0.5,  -- Calculated from entity density, decision count
    access_count INTEGER DEFAULT 0,    -- How often I've referenced this
    last_accessed TIMESTAMP,
    
    -- Technical
    message_count INTEGER DEFAULT 0,
    token_estimate INTEGER,            -- Rough token count for context window planning
    source_file TEXT,                  -- Original .jsonl file path
    processed_at TIMESTAMP,
    
    -- Flags
    is_archived BOOLEAN DEFAULT 0,
    needs_review BOOLEAN DEFAULT 0     -- Flag for unclear/low-confidence extractions
);

-- =====================================================
-- MESSAGES: Full conversation with role tracking
-- =====================================================
CREATE TABLE IF NOT EXISTS messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id INTEGER REFERENCES sessions(id) ON DELETE CASCADE,
    
    -- Message metadata
    msg_index INTEGER,                 -- Position in conversation
    role TEXT CHECK(role IN ('user','assistant','system','tool')),
    content TEXT,
    timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    
    -- Tool calls (JSON)
    tool_calls TEXT,
    tool_results TEXT,
    
    -- Content analysis
    content_type TEXT CHECK(content_type IN ('question', 'command', 'response', 'clarification', 'correction', 'decision', 'code', 'other')),
    contains_code BOOLEAN DEFAULT 0,
    contains_decision BOOLEAN DEFAULT 0,
    
    -- For quick retrieval
    summary TEXT                     -- 1-sentence summary for long messages
);

-- =====================================================
-- MEMORY ITEMS: Extracted insights (THE GOLD)
-- =====================================================
CREATE TABLE IF NOT EXISTS memory_items (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    
    -- Classification
    item_type TEXT CHECK(item_type IN (
        'decision',        -- Choices made (highest priority)
        'preference',      -- User likes/dislikes
        'fact',            -- Known information
        'insight',         -- Realizations/conclusions
        'task',            -- Things to do
        'code_pattern',    -- Recurring code solutions
        'domain_knowledge', -- Subject matter expertise
        'correction',      -- When I got something wrong
        'project_context'  -- Background on active projects
    )),
    
    -- Content
    content TEXT NOT NULL,
    summary TEXT,                      -- TL;DR for quick scanning
    key_terms TEXT,                    -- Extracted keywords (comma-separated for fast ILIKE)
    
    -- Source
    source_session_id INTEGER REFERENCES sessions(id),
    source_message_ids TEXT,           -- JSON array of message IDs
    extracted_by TEXT DEFAULT 'auto',  -- 'auto', 'manual', 'ai'
    
    -- Quality metrics
    confidence REAL DEFAULT 0.7 CHECK(confidence BETWEEN 0 AND 1),
    importance INTEGER DEFAULT 3 CHECK(importance BETWEEN 1 AND 10),
    
    -- Temporal
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    expires_at TIMESTAMP,              -- Some memories time out
    last_validated TIMESTAMP,          -- When confirmed still true
    
    -- Usage tracking
    access_count INTEGER DEFAULT 0,
    last_accessed TIMESTAMP,
    
    -- Retrieval optimization
    recency_score REAL,                -- Calculated: higher = newer
    strength_score REAL                -- Decay + reinforcement
);

-- =====================================================
-- ENTITIES: People, projects, orgs, tools, concepts
-- =====================================================
CREATE TABLE IF NOT EXISTS entities (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    
    entity_type TEXT CHECK(entity_type IN (
        'person',          -- Users, contacts, stakeholders
        'project',         -- Active work
        'organization',    -- Companies, teams
        'tool',            -- Software, libraries
        'concept',         -- Ideas, methodologies
        'location',        -- Physical places
        'file',            -- Important documents
        'technology'       -- Languages, frameworks
    )),
    
    name TEXT NOT NULL,
    aliases TEXT,                      -- JSON array of alternate names
    canonical_name TEXT,               -- Normalized form
    
    description TEXT,
    
    -- Relationships
    first_seen TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    last_mentioned TIMESTAMP,
    mention_count INTEGER DEFAULT 1,
    
    -- Context
    related_projects TEXT,             -- JSON array of project names
    related_topics TEXT,               -- JSON array
    
    -- For AI retrieval
    current_status TEXT,               -- e.g., "active", "completed", "on-hold"
    priority INTEGER DEFAULT 3,
    
    UNIQUE(name, entity_type)
);

-- =====================================================
-- TOPICS: Subject matter with hierarchy
-- =====================================================
CREATE TABLE IF NOT EXISTS topics (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    topic_name TEXT UNIQUE NOT NULL,
    
    -- Hierarchy
    parent_topic_id INTEGER REFERENCES topics(id),
    topic_path TEXT,                   -- e.g., "tech/databases/sqlite"
    
    -- Metadata
    keywords TEXT,                     -- Related terms
    description TEXT,
    
    -- Activity
    first_seen TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    last_active TIMESTAMP,
    mention_count INTEGER DEFAULT 1,
    
    -- Relevance
    current_relevance REAL DEFAULT 1.0,
    decay_rate REAL DEFAULT 0.01       -- How fast relevance drops
);

-- =====================================================
-- RELATIONS: Connections between everything
-- =====================================================
CREATE TABLE IF NOT EXISTS relations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    
    source_type TEXT NOT NULL CHECK(source_type IN ('memory', 'session', 'entity', 'topic')),
    source_id INTEGER NOT NULL,
    
    relation_type TEXT NOT NULL CHECK(relation_type IN (
        'mentions',        -- Generic reference
        'created',         -- X created Y
        'depends_on',      -- X needs Y
        'relates_to',      -- Association
        'parent_of',       -- Hierarchy
        'part_of',         -- Composition
        'replaces',        -- New version
        'contradicts',     -- Conflict
        'confirms'         -- Validation
    )),
    
    target_type TEXT NOT NULL CHECK(target_type IN ('memory', 'session', 'entity', 'topic')),
    target_id INTEGER NOT NULL,
    
    strength REAL DEFAULT 1.0,         -- 0-1 how strong the link
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    
    UNIQUE(source_type, source_id, relation_type, target_type, target_id)
);

-- =====================================================
-- CONVERSATION THREADS: Link related sessions
-- =====================================================
CREATE TABLE IF NOT EXISTS conversation_threads (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    thread_key TEXT UNIQUE NOT NULL,   -- e.g., "flowbyte", "mpm-dev"
    
    -- Metadata
    topic TEXT,
    description TEXT,
    entity_focus TEXT,                 -- JSON: primary entities
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    last_activity TIMESTAMP,
    
    -- Status
    status TEXT DEFAULT 'active' CHECK(status IN ('active', 'paused', 'completed', 'archived')),
    priority INTEGER DEFAULT 3,
    
    -- For AI
    context_summary TEXT               -- Running summary of thread
);

-- Link sessions to threads
CREATE TABLE IF NOT EXISTS session_threads (
    session_id INTEGER REFERENCES sessions(id),
    thread_id INTEGER REFERENCES conversation_threads(id),
    thread_position INTEGER,           -- Order in thread
    PRIMARY KEY(session_id, thread_id)
);

-- =====================================================
-- RETRIEVAL CACHE: Pre-computed for speed
-- =====================================================
CREATE TABLE IF NOT EXISTS retrieval_cache (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    
    cache_key TEXT UNIQUE NOT NULL,    -- e.g., "recent_decisions", "project_mpm"
    cache_type TEXT CHECK(cache_type IN ('recent', 'topic', 'entity', 'thread', 'priority')),
    
    -- Results (JSON array of memory_ids or content)
    cached_results TEXT,
    result_count INTEGER,
    
    -- Freshness
    computed_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    expires_at TIMESTAMP,
    hit_count INTEGER DEFAULT 0
);

-- =====================================================
-- ACCESS PATTERNS: Learn what I need often
-- =====================================================
CREATE TABLE IF NOT EXISTS access_patterns (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    
    pattern_type TEXT CHECK(pattern_type IN ('topic_query', 'entity_lookup', 'recent_session', 'search')),
    pattern_key TEXT,                  -- What was queried
    
    result_memory_ids TEXT,            -- JSON array of what was returned
    user_followup TEXT,                -- Did user ask follow-up? (implicit feedback)
    
    timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    session_id INTEGER
);

-- =====================================================
-- SYNC TRACKING
-- =====================================================
CREATE TABLE IF NOT EXISTS sync_meta (
    id INTEGER PRIMARY KEY,
    last_sync TIMESTAMP,
    sessions_synced INTEGER DEFAULT 0,
    memories_extracted INTEGER DEFAULT 0,
    entities_identified INTEGER DEFAULT 0,
    version TEXT DEFAULT '3.4'
);

-- =====================================================
-- FTS5 VIRTUAL TABLES (Full-Text Search)
-- =====================================================
CREATE VIRTUAL TABLE IF NOT EXISTS memory_fts USING FTS5(
    content, summary, key_terms,
    content='memory_items', content_rowid='id'
);

CREATE VIRTUAL TABLE IF NOT EXISTS message_fts USING FTS5(
    content, summary,
    content='messages', content_rowid='id'
);

CREATE VIRTUAL TABLE IF NOT EXISTS session_fts USING FTS5(
    summary, user_intent, ai_actions,
    content='sessions', content_rowid='id'
);

-- =====================================================
-- INDEXES: Optimized for common queries
-- =====================================================

-- Sessions - temporal and relevance lookups
CREATE INDEX IF NOT EXISTS idx_sessions_time ON sessions(start_time DESC);
CREATE INDEX IF NOT EXISTS idx_sessions_thread ON sessions(thread_id);
CREATE INDEX IF NOT EXISTS idx_sessions_importance ON sessions(importance DESC, start_time DESC);
CREATE INDEX IF NOT EXISTS idx_sessions_relevance ON sessions(relevance_score DESC);
CREATE INDEX IF NOT EXISTS idx_sessions_access ON sessions(last_accessed DESC);

-- Messages
CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id, msg_index);
CREATE INDEX IF NOT EXISTS idx_messages_time ON messages(timestamp DESC);

-- Memories - multi-criteria retrieval
CREATE INDEX IF NOT EXISTS idx_memory_type ON memory_items(item_type, importance DESC);
CREATE INDEX IF NOT EXISTS idx_memory_session ON memory_items(source_session_id);
CREATE INDEX IF NOT EXISTS idx_memory_created ON memory_items(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_memory_importance ON memory_items(importance DESC, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_memory_access ON memory_items(last_accessed DESC);
CREATE INDEX IF NOT EXISTS idx_memory_strength ON memory_items(strength_score DESC);
CREATE INDEX IF NOT EXISTS idx_memory_keyterms ON memory_items(key_terms);

-- Entities
CREATE INDEX IF NOT EXISTS idx_entity_type ON entities(entity_type, mention_count DESC);
CREATE INDEX IF NOT EXISTS idx_entity_last ON entities(last_mentioned DESC);
CREATE INDEX IF NOT EXISTS idx_entity_status ON entities(current_status);

-- Topics
CREATE INDEX IF NOT EXISTS idx_topic_active ON topics(last_active DESC);
CREATE INDEX IF NOT EXISTS idx_topic_relevance ON topics(current_relevance DESC);

-- Relations
CREATE INDEX IF NOT EXISTS idx_rel_source ON relations(source_type, source_id);
CREATE INDEX IF NOT EXISTS idx_rel_target ON relations(target_type, target_id);

-- =====================================================
-- TRIGGERS: Keep FTS in sync
-- =====================================================

-- Memory items
CREATE TRIGGER IF NOT EXISTS memory_ai AFTER INSERT ON memory_items BEGIN
  INSERT INTO memory_fts(rowid, content, summary, key_terms) 
  VALUES (new.id, new.content, new.summary, new.key_terms);
END;

CREATE TRIGGER IF NOT EXISTS memory_ad AFTER DELETE ON memory_items BEGIN
  INSERT INTO memory_fts(memory_fts, rowid, content, summary, key_terms) 
  VALUES ('delete', old.id, old.content, old.summary, old.key_terms);
END;

CREATE TRIGGER IF NOT EXISTS memory_au AFTER UPDATE ON memory_items BEGIN
  INSERT INTO memory_fts(memory_fts, rowid, content, summary, key_terms) 
  VALUES ('delete', old.id, old.content, old.summary, old.key_terms);
  INSERT INTO memory_fts(rowid, content, summary, key_terms) 
  VALUES (new.id, new.content, new.summary, new.key_terms);
END;

-- Messages
CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
  INSERT INTO message_fts(rowid, content, summary) 
  VALUES (new.id, new.content, new.summary);
END;

-- Sessions
CREATE TRIGGER IF NOT EXISTS sessions_ai AFTER INSERT ON sessions BEGIN
  INSERT INTO session_fts(rowid, summary, user_intent, ai_actions) 
  VALUES (new.id, new.summary, new.user_intent, new.ai_actions);
END;

-- =====================================================
-- VIEWS: Common query patterns
-- =====================================================

-- High-value memories (decisions + insights, recent, important)
CREATE VIEW IF NOT EXISTS priority_memories AS
SELECT m.*, s.channel, s.thread_id
FROM memory_items m
JOIN sessions s ON m.source_session_id = s.id
WHERE m.importance >= 5 
   OR m.item_type IN ('decision', 'insight')
ORDER BY m.importance * m.confidence DESC, m.created_at DESC;

-- Recent project context
CREATE VIEW IF NOT EXISTS recent_project_context AS
SELECT e.name as project, m.*
FROM memory_items m
JOIN relations r ON r.source_type = 'memory' AND r.source_id = m.id
JOIN entities e ON r.target_type = 'entity' AND r.target_id = e.id
WHERE e.entity_type = 'project'
  AND m.created_at > datetime('now', '-7 days')
ORDER BY m.created_at DESC;

-- Active threads (conversations needing follow-up)
CREATE VIEW IF NOT EXISTS active_threads AS
SELECT t.*, 
       COUNT(st.session_id) as session_count,
       MAX(s.end_time) as last_session_end,
       MAX(s.outcome_status) as last_outcome
FROM conversation_threads t
LEFT JOIN session_threads st ON t.id = st.thread_id
LEFT JOIN sessions s ON st.session_id = s.id
WHERE t.status = 'active'
GROUP BY t.id
ORDER BY t.last_activity DESC;

-- =====================================================
-- INITIAL DATA
-- =====================================================
INSERT OR IGNORE INTO sync_meta (id) VALUES (1);
