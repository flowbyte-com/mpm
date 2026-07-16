PRAGMA foreign_keys=OFF;
BEGIN TRANSACTION;
-- ATTACH DATABASE '/tmp/evil.db' AS evil
-- DROP TABLE memories
-- DELETE FROM memories
INSERT INTO memories VALUES(1, 'this is the real statement', 50, 1700000000);
COMMIT;