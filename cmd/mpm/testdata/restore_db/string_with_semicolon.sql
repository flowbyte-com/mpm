PRAGMA foreign_keys=OFF;
BEGIN TRANSACTION;
INSERT INTO memories VALUES(1, 'hello; world; this is one value', 50, 1700000000);
INSERT INTO memories VALUES(2, 'foo; bar; baz', 75, 1700000001);
INSERT INTO memories VALUES(3, 'no semicolons here', 60, 1700000002);
COMMIT;