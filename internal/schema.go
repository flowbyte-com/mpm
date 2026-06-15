// schema.go - Shared SQLite schema definitions for MPM
// Version: 2026-03-28 (Consolidated Schema)
// All tables should be defined here and imported by DatabaseManager and MemoryStore

package internal

// BaseTables contains the core table creation statements.
// These are always created on database initialization.
var BaseTables = []string{
	// Sessions table - stores session metadata and transcripts
	`CREATE TABLE IF NOT EXISTS sessions (
		id TEXT PRIMARY KEY, session_id TEXT NOT NULL, content TEXT NOT NULL,
		content_hash TEXT UNIQUE NOT NULL, created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		source_path TEXT, metadata JSON, embedding BLOB
	);`,
	`CREATE INDEX IF NOT EXISTS idx_sessions_session_id ON sessions(session_id);`,

	// Topics table - organizes memories and sessions into topics
	`CREATE TABLE IF NOT EXISTS topics (
		id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, description TEXT,
		parent_topic_id TEXT, tags JSON, is_active INTEGER DEFAULT 1,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP, embedding BLOB
	);`,

	// Topic memberships - many-to-many relationship between memories/sessions and topics
	`CREATE TABLE IF NOT EXISTS topic_memberships (
		memory_id TEXT, session_id TEXT, topic_id TEXT NOT NULL,
		role TEXT DEFAULT 'related', created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (memory_id, topic_id),
		UNIQUE (session_id, topic_id),
		CHECK (memory_id IS NOT NULL OR session_id IS NOT NULL)
	);`,

	// Memories table - core storage for memory content
	`CREATE TABLE IF NOT EXISTS memories (
		id TEXT PRIMARY KEY, collection TEXT NOT NULL, content TEXT NOT NULL,
		session_id TEXT, tags JSON, metadata JSON, embedding BLOB,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		source_db TEXT, source_id TEXT, promoted_at REAL,
		FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE SET NULL
	);`,

	// System config table - stores system configuration snapshots
	`CREATE TABLE IF NOT EXISTS system_config (
		key TEXT PRIMARY KEY,
		raw_json TEXT NOT NULL,
		content_hash TEXT NOT NULL,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		config_snapshot JSON
	);`,

	// Lessons table - stores learned lessons (insights, warnings, practices)
	`CREATE TABLE IF NOT EXISTS lessons (
		id TEXT PRIMARY KEY,
		type TEXT NOT NULL DEFAULT 'insight',
		content TEXT NOT NULL,
		tags JSON,
		reinforcement_count INTEGER DEFAULT 1,
		source_session_id TEXT,
		created TEXT NOT NULL,
		content_hash TEXT
	);`,

	// Raw memories table - for v6 ingest workflow
	`CREATE TABLE IF NOT EXISTS raw_memories (
		id            TEXT PRIMARY KEY,
		source_id     TEXT,
		source_db     TEXT NOT NULL DEFAULT 'openclaw',
		content_hash  TEXT NOT NULL,
		text          TEXT NOT NULL,
		metadata      TEXT NOT NULL,
		ingested_at   REAL NOT NULL,
		status        TEXT NOT NULL DEFAULT 'pending',
		llm_verdict   TEXT,
		llm_notes     TEXT,
		reviewer_prompt TEXT,
		expires_at    REAL,
		import_batch  TEXT,
		updated_at    REAL NOT NULL
	);`,

	// External database cursors - tracks sync position with external dbs
	`CREATE TABLE IF NOT EXISTS external_db_cursors (
		db_label TEXT PRIMARY KEY,
		last_cursor TEXT NOT NULL,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);`,

	// Reference documents table - aligned with ReferenceDB in reference_new.go
	`CREATE TABLE IF NOT EXISTS reference_docs (
		id TEXT PRIMARY KEY, title TEXT NOT NULL, file_path TEXT,
		source_type TEXT, tags TEXT, content TEXT NOT NULL, content_hash TEXT,
		total_chunks INTEGER DEFAULT 0, last_indexed TEXT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);`,

	// Reference chunks table - stores chunked content of reference documents
	`CREATE TABLE IF NOT EXISTS reference_chunks (
		id TEXT PRIMARY KEY, doc_id TEXT NOT NULL, chunk_index INTEGER NOT NULL,
		section TEXT, content TEXT NOT NULL, source_path TEXT,
		FOREIGN KEY (doc_id) REFERENCES reference_docs(id) ON DELETE CASCADE
	);`,

	// Memory revisions table - historical ledger for point-in-time reconstruction
	`CREATE TABLE IF NOT EXISTS memory_revisions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		memory_id TEXT NOT NULL,
		version INTEGER NOT NULL,
		content TEXT NOT NULL,
		weight INTEGER NOT NULL,
		collection TEXT NOT NULL,
		is_long_term INTEGER NOT NULL DEFAULT 0,
		is_challenged INTEGER NOT NULL DEFAULT 0,
		challenged_theory_id TEXT,
		created_at TEXT DEFAULT (STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW')),
		FOREIGN KEY(memory_id) REFERENCES memories(id) ON DELETE CASCADE,
		UNIQUE(memory_id, version)
	);`,
}

// CommonIndexes contains indexes for fast lookups.
var CommonIndexes = []string{
	`CREATE INDEX IF NOT EXISTS idx_topic_memberships_topic ON topic_memberships(topic_id);`,
	`CREATE INDEX IF NOT EXISTS idx_topic_memberships_memory ON topic_memberships(memory_id);`,
	`CREATE INDEX IF NOT EXISTS idx_topic_memberships_session ON topic_memberships(session_id);`,
	`CREATE INDEX IF NOT EXISTS idx_topics_parent ON topics(parent_topic_id);`,
	`CREATE INDEX IF NOT EXISTS idx_memories_session ON memories(session_id);`,
	`CREATE INDEX IF NOT EXISTS idx_memories_collection ON memories(collection);`,
	`CREATE INDEX IF NOT EXISTS idx_memories_deleted ON memories(deleted_at);`,
	`CREATE INDEX IF NOT EXISTS idx_memories_collection_deleted_created ON memories(collection, deleted_at, created_at);`,
	`CREATE INDEX IF NOT EXISTS idx_memories_longterm ON memories(is_long_term, weight);`,
	`CREATE INDEX IF NOT EXISTS idx_memories_reinforcement ON memories(reinforcement_count);`,
	`CREATE INDEX IF NOT EXISTS idx_memories_expires ON memories(expires_at);`,
	`CREATE INDEX IF NOT EXISTS idx_memories_accessed ON memories(last_accessed_at);`,
	`CREATE INDEX IF NOT EXISTS idx_lessons_type ON lessons(type);`,
	`CREATE INDEX IF NOT EXISTS idx_lessons_reinforcement ON lessons(reinforcement_count);`,
	`CREATE INDEX IF NOT EXISTS idx_raw_memories_status ON raw_memories(status);`,
	`CREATE INDEX IF NOT EXISTS idx_raw_memories_expires ON raw_memories(expires_at);`,
	`CREATE INDEX IF NOT EXISTS idx_raw_memories_import_batch ON raw_memories(import_batch);`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_raw_memories_source_dedup ON raw_memories(source_db, source_id);`,
	`CREATE INDEX IF NOT EXISTS idx_reference_chunks_doc_id ON reference_chunks(doc_id);`,
	`CREATE INDEX IF NOT EXISTS idx_reference_chunks_section ON reference_chunks(section);`,
	`CREATE INDEX IF NOT EXISTS idx_revisions_timeline ON memory_revisions(memory_id, created_at);`,
}

// SafeMigrations contains column additions that may be needed for existing databases.
// Format: table name, column name, column type
var SafeMigrations = [][3]string{
	{"topics", "parent_topic_id", "TEXT"},
	{"topics", "embedding", "BLOB"},
	{"memories", "embedding", "BLOB"},
	{"memories", "deleted_at", "TEXT"},
	{"memories", "reference_id", "TEXT"},
	{"memories", "content_hash", "TEXT"},
	{"memories", "is_prime_directive", "INTEGER DEFAULT 0"},
	{"memories", "toxicity_score", "REAL"},
	{"memories", "session_id", "TEXT"},
	{"memories", "is_long_term", "INTEGER DEFAULT 0"},
	{"memories", "weight", "INTEGER DEFAULT 1"},
	{"memories", "reinforcement_count", "INTEGER DEFAULT 0"},
	{"memories", "last_accessed_at", "DATETIME"},
	{"memories", "updated_at", "DATETIME"},
	{"memories", "expires_at", "DATETIME"},
	{"memories", "source_db", "TEXT"},
	{"memories", "source_id", "TEXT"},
	{"memories", "promoted_at", "REAL"},
	{"sessions", "embedding", "BLOB"},
	{"sessions", "metadata", "TEXT"},
	{"raw_memories", "next_retry", "TEXT"},
	{"raw_memories", "attempt", "INTEGER DEFAULT 0"},
}
