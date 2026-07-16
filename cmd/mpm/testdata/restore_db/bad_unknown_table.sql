PRAGMA foreign_keys=OFF;
BEGIN TRANSACTION;
INSERT INTO evil_table VALUES(1, 'attacker-controlled table', 50, 1700000000);
COMMIT;