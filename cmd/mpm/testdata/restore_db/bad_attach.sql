PRAGMA foreign_keys=OFF;
BEGIN TRANSACTION;
ATTACH DATABASE '/tmp/evil.db' AS evil;
INSERT INTO memories VALUES(1, 'pwned via attach', 50, 1700000000);
COMMIT;