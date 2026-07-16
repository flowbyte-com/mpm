PRAGMA foreign_keys=OFF;
BEGIN TRANSACTION;
aTtAcH DATABASE '/tmp/evil.db' AS evil;
INSERT INTO memories VALUES(1, 'pwned via case variation', 50, 1700000000);
COMMIT;