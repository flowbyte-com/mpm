PRAGMA foreign_keys=OFF;
BEGIN TRANSACTION;
CREATE TABLE IF NOT EXISTS memories (id INTEGER PRIMARY KEY, content TEXT, weight REAL DEFAULT 1.0, created_at INTEGER);
INSERT INTO memories VALUES(1, 'hello world', 1.0, 1700000000);
INSERT INTO memories VALUES(2, 'second memory', 0.75, 1700000001);
CREATE INDEX idx_memories_weight ON memories(weight);
COMMIT;