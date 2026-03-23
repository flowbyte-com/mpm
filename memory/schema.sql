-- MPM Memory Schema v3.3
-- SQLite database optimized for agent memory retrieval

-- Sessions: Conversation sessions
CREATE TABLE sessions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_key TEXT UNIQUE NOT NULL,
    channel TEXT,
    start_time TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    end_time TIMESTAMP,
    summary TEXT,
    topic_tags TEXT,
    importance INTEGER DEFAULT 0
);

-- Messages: Individual messages
CREATE TABLE messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id INTEGER REFERENCES sessions(id) ON DELETE CASCADE,
    role TEXT CHECK(role IN ('user','assistant','system','tool')),
    content TEXT,
    timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    tool_calls TEXT
);

-- Memory items: Extracted knowledge
CREATE TABLE memory_items (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    item_type TEXT CHECK(item_type IN ('fact','decision','task','preference','entity','insight')),
    content TEXT NOT NULL,
    summary TEXT,
    source_session_id INTEGER REFERENCES sessions(id),
    confidence REAL DEFAULT 1.0,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    expires_at TIMESTAMP,
    access_count INTEGER DEFAULT 0,
    last_accessed TIMESTAMP
);

-- Entities: People, projects, orgs, tools, concepts, locations
CREATE TABLE entities (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    entity_type TEXT CHECK(entity_type IN ('person','project','organization','concept','tool','location')),
    name TEXT NOT NULL,
    aliases TEXT,
    description TEXT,
    first_seen TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    last_mentioned TIMESTAMP,
    mention_count INTEGER DEFAULT 1
);

-- Topics: Conversation themes
CREATE TABLE topics (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    topic_name TEXT UNIQUE NOT NULL,
    keywords TEXT,
    first_seen TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    last_active TIMESTAMP,
    mention_count INTEGER DEFAULT 1,
    parent_topic_id INTEGER REFERENCES topics(id)
);

-- Relations: Links between items
CREATE TABLE relations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    source_type TEXT NOT NULL,
    source_id INTEGER NOT NULL,
    relation_type TEXT NOT NULL,
    target_type TEXT NOT NULL,
    target_id INTEGER NOT NULL,
    strength REAL DEFAULT 1.0,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- Search log: For query optimization
CREATE TABLE search_log (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    query TEXT NOT NULL,
    query_type TEXT,
    results_count INTEGER,
    duration_ms INTEGER,
    timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- Sync metadata
CREATE TABLE sync_meta (
    id INTEGER PRIMARY KEY,
    last_sync TIMESTAMP,
    items_synced INTEGER,
    version TEXT
);

-- FTS5 virtual tables for search
CREATE VIRTUAL TABLE memory_fts USING FTS5(
    content, summary, content='memory_items', content_rowid='id'
);
CREATE VIRTUAL TABLE message_fts USING FTS5(
    content, content='messages', content_rowid='id'
);

-- Indexes
CREATE INDEX idx_sessions_time ON sessions(start_time);
CREATE INDEX idx_sessions_channel ON sessions(channel);
CREATE INDEX idx_messages_session ON messages(session_id);
CREATE INDEX idx_messages_time ON messages(timestamp);
CREATE INDEX idx_memory_type ON memory_items(item_type);
CREATE INDEX idx_memory_created ON memory_items(created_at);
CREATE INDEX idx_memory_session ON memory_items(source_session_id);
CREATE UNIQUE INDEX idx_entity_name_type ON entities(name, entity_type);
CREATE INDEX idx_relations_source ON relations(source_type, source_id);
CREATE INDEX idx_relations_target ON relations(target_type, target_id);

-- FTS sync triggers
CREATE TRIGGER memory_ai AFTER INSERT ON memory_items BEGIN
  INSERT INTO memory_fts(rowid, content, summary) VALUES (new.id, new.content, new.summary);
END;
CREATE TRIGGER memory_ad AFTER DELETE ON memory_items BEGIN
  INSERT INTO memory_fts(memory_fts, rowid, content, summary) VALUES ('delete', old.id, old.content, old.summary);
END;
CREATE TRIGGER memory_au AFTER UPDATE ON memory_items BEGIN
  INSERT INTO memory_fts(memory_fts, rowid, content, summary) VALUES ('delete', old.id, old.content, old.summary);
  INSERT INTO memory_fts(rowid, content, summary) VALUES (new.id, new.content, new.summary);
END;
CREATE TRIGGER messages_ai AFTER INSERT ON messages BEGIN
  INSERT INTO message_fts(rowid, content) VALUES (new.id, new.content);
END;
