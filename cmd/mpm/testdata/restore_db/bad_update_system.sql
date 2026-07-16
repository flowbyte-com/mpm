PRAGMA foreign_keys=OFF;
BEGIN TRANSACTION;
UPDATE memories SET weight = 0;
COMMIT;