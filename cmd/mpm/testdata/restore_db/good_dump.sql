PRAGMA foreign_keys=OFF;
BEGIN TRANSACTION;
CREATE TABLE IF NOT EXISTS memories (id INTEGER PRIMARY KEY, content TEXT, weight INTEGER, created_at INTEGER);
INSERT INTO memories VALUES(1, 'hello world', 50, 1700000000);
INSERT INTO memories VALUES(2, 'second memory', 75, 1700000001);
CREATE INDEX idx_memories_weight ON memories(weight);
COMMIT;