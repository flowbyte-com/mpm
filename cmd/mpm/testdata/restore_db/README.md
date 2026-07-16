# restore-db test fixtures

These SQL dumps exercise `internal/core.DumpValidator` for the `mpm restore-db`
primitive. Each fixture is a self-contained .sql file that the validator
must accept or reject per the threat model.

| Fixture | Threat vector | Expected |
|---|---|---|
| `good_dump.sql` | Typical valid dump (PRAGMA + BEGIN + CREATE TABLE + INSERT + CREATE INDEX + COMMIT) | accept |
| `bad_attach.sql` | `ATTACH DATABASE '/tmp/evil.db' AS evil` — external DB attach | reject |
| `bad_select.sql` | Bare `SELECT * FROM memories` — read primitive | reject |
| `bad_pragmas.sql` | `PRAGMA writable_schema = 1` — schema-write primitive | reject |
| `bad_unknown_table.sql` | `INSERT INTO evil_table VALUES(...)` — table outside known allow-list | reject |
| `bad_delete_system.sql` | `DELETE FROM memories` — mass delete | reject |
| `bad_update_system.sql` | `UPDATE memories SET ...` — mass overwrite of identity/weight/ltm | reject |
| `comment_spoof.sql` | `-- ATTACH DATABASE 'evil'` before valid INSERT — comment-based bypass attempt | accept (comment stripped, INSERT is the only executable statement) |
| `case_variation.sql` | `aTtAcH DATABASE 'evil.db'` — case-variation bypass | reject |
| `string_with_semicolon.sql` | `INSERT INTO memories VALUES(1, 'hello; world')` — semicolon inside string literal | accept (statement-splitter respects string boundaries) |

Each fixture is a complete, syntactically-valid SQLite dump that could in
principle be passed to `tx.Exec`. The validator's job is to reject the
malicious ones (and `good_dump.sql` / `comment_spoof.sql` / `string_with_semicolon.sql`)
must accept to avoid false positives.

The fixtures are loaded from disk by `internal/core/sql_dump_validator_test.go`
via Go's `testdata/` convention.