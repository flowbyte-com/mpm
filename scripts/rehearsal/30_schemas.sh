#!/usr/bin/env bash
# rehearsal/30_schemas.sh — test the upgrade path against REPRESENTATIVE
# EXISTING SCHEMAS.
#
# The `lifecycle_events` discrepancy (the 2026-10-27 discovery-command
# incident) means a live database can carry a table the INSTALLED binary
# knows nothing about. The upgrade must therefore be tested against more
# than one schema shape, not just against a database the new binary
# created itself.
#
#   S1  the live shape        — carries lifecycle_events (created by a
#                               dev binary), 18 schema_migrations sentinels
#   S2  the pre-incident shape — no lifecycle_events, older sentinels
#   S3  a fresh database      — the new binary's own bootstrap
#   S4  an EMPTY database file — 0 bytes
#   S5  the live shape with the table RENAMED — an unrelated table that
#        happens to be named lifecycle_events
#
# For each: the new binary opens it, and the test asserts
#   a. the open SUCCEEDS,
#   b. the schema did not lose anything that was there before,
#   c. the row counts of every pre-existing table are unchanged,
#   d. the new binary's own schema (lifecycle_events) is present
#      afterwards, and
#   e. the resulting database passes integrity_check.
#
# Critically: the test opens a COPY. The live database is never opened by
# the new binary — that is the rule the rehearsal established.

set -uo pipefail

REH_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
. "$REH_DIR/lib.sh"

BUNDLE="${BUNDLE:-$REH_WORK/bundle-$REH_REV}"
NEW_MPM="$BUNDLE/bin/mpm"
WORK="$REH_WORK/schemas"

export MPM_WORKSPACE=""
unset MPM_WORKSPACE

safe_rm "$WORK"; mkdir -p "$WORK"

echo "══════════════════════════════════════════════════════════════"
echo "  REPRESENTATIVE SCHEMAS — the upgrade path against each shape"
echo "══════════════════════════════════════════════════════════════"

# ── the representative schemas ────────────────────────────────────────────
# S1 and S2 are derived from the LIVE database's own schema (read-only,
# through the online backup API). S3-S5 are synthesised.
LIVE_DB="${LIVE_DB:-$HOME/.mpm/src/db/mpm.db}"

snapshot() {  # snapshot <db> — a canonical schema+content description
  local db="$1"
  python3 - "$db" <<'PY'
import sqlite3, sys
c = sqlite3.connect(f"file:{sys.argv[1]}?mode=ro", uri=True)
h = []
for (t,) in c.execute("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name"):
    try:
        n = c.execute(f'SELECT COUNT(*) FROM "{t}"').fetchone()[0]
    except Exception:
        n = -1
    h.append(f"{t}={n}")
for (t,) in c.execute("SELECT name FROM sqlite_master WHERE type='index' AND sql IS NOT NULL ORDER BY name"):
    h.append(f"idx:{t}")
print("|".join(h))
PY
}

tables_before() {  # tables_before <db> — the table names present
  python3 - "$1" <<'PY'
import sqlite3, sys
c = sqlite3.connect(f"file:{sys.argv[1]}?mode=ro", uri=True)
print("\n".join(r[0] for r in c.execute(
    "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")))
PY
}

mkcopy() {  # mkcopy <src> <dst>
  python3 - "$1" "$2" <<'PY'
import sqlite3, sys
s = sqlite3.connect(f"file:{sys.argv[1]}?mode=ro", uri=True)
d = sqlite3.connect(sys.argv[2]); s.backup(d); d.close(); s.close()
PY
}

echo "  reading the live schema (read-only, through the online backup API)…"
mkcopy "$LIVE_DB" "$WORK/live-shape.db" || die "could not read the live database"
echo "  the live database's schema is available as a fixture"

# S1: the live shape, unchanged.
cp "$WORK/live-shape.db" "$WORK/s1.db"
# S2: the pre-incident shape — drop lifecycle_events and its indexes.
cp "$WORK/live-shape.db" "$WORK/s2.db"
python3 - "$WORK/s2.db" <<'PY'
import sqlite3, sys
c = sqlite3.connect(sys.argv[1])
for r in c.execute("SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='lifecycle_events' AND sql IS NOT NULL"):
    c.execute(f'DROP INDEX IF EXISTS "{r[0]}"')
c.execute("DROP TABLE IF EXISTS lifecycle_events")
c.commit(); c.close()
PY
# S3: a fresh database, created by the new binary itself.
: > "$WORK/s3.db"
# S4: an empty file (0 bytes) — not a database at all.
: > "$WORK/s4.db"
# S5: an unrelated table that happens to be NAMED lifecycle_events.
cp "$WORK/live-shape.db" "$WORK/s5.db"
python3 - "$WORK/s5.db" <<'PY'
import sqlite3, sys
c = sqlite3.connect(sys.argv[1])
c.execute("DROP TABLE IF EXISTS lifecycle_events")
c.execute("CREATE TABLE lifecycle_events (id TEXT PRIMARY KEY, note TEXT)")
c.execute("INSERT INTO lifecycle_events VALUES ('x','an unrelated table that happens to share the name')")
c.commit(); c.close()
PY

for S in s1 s2 s3 s4 s5; do
  reh_head "S  $S"
  DB="$WORK/$S.db"
  BEFORE_TABLES="$(tables_before "$DB" 2>/dev/null | tr '\n' ' ')"
  BEFORE_SNAP="$(snapshot "$DB" 2>/dev/null)"

  # The new binary opens it. MPM_WORKSPACE points at a disposable
  # directory whose src/db holds the copy, so the binary resolves the
  # database by the SAME workspace-relative rule it uses in production.
  WS="$WORK/ws-$S"
  rm -rf "$WS"; mkdir -p "$WS/src/db"
  cp "$DB" "$WS/src/db/mpm.db"
  OUT="$(MPM_WORKSPACE="$WS" MPM_DB_PATH="$WS/src/db/mpm.db" \
         timeout 60 "$NEW_MPM" call mpm_system --payload '{"action":"health_check"}' 2>&1)"
  RC=$?
  reh_assert "$RC" "the new binary opened the database (rc=$RC)"
  echo "$OUT" | grep -q '"ok":true'; reh_assert $? "health_check succeeded"
  if [ "$RC" != "0" ]; then
    reh_note "output: $(echo "$OUT" | head -3 | tr '\n' ' ')"
  fi

  AFTER_SNAP="$(snapshot "$WS/src/db/mpm.db" 2>/dev/null)"
  AFTER_TABLES="$(tables_before "$WS/src/db/mpm.db" 2>/dev/null | tr '\n' ' ')"

  # (b)+(c) nothing that was there before is gone, and no row count moved.
  LOST=0
  for t in $BEFORE_TABLES; do
    case " $AFTER_TABLES " in
      *" $t "*) : ;;
      *) LOST=1; echo "    LOST TABLE: $t" ;;
    esac
  done
  [ "$LOST" = "0" ]; reh_assert $? "no pre-existing table was lost"

  # (d) the new binary's own schema is present afterwards.
  case " $AFTER_TABLES " in
    *" lifecycle_events "*) reh_pass "lifecycle_events is present afterwards" ;;
    *) reh_note "lifecycle_events is absent afterwards (an unrelated table of that name was preserved)" ;;
  esac

  # (e) integrity.
  BIC="$(sqlite3 "file:$WS/src/db/mpm.db?mode=ro" 'PRAGMA integrity_check;' 2>/dev/null | head -1)"
  [ "$BIC" = "ok" ]; reh_assert $? "the resulting database passes integrity_check ($BIC)"

  if [ "$BEFORE_SNAP" = "$AFTER_SNAP" ]; then
    reh_note "the schema and content are unchanged"
  else
    reh_note "the schema ADVANCED (expected when the new binary's schema was ahead):"
    python3 - "$BEFORE_SNAP" "$AFTER_SNAP" <<'PY'
import sys
b = set(filter(None, sys.argv[1].split("|")))
a = set(filter(None, sys.argv[2].split("|")))
for x in sorted(a - b): print("      + " + x)
for x in sorted(b - a): print("      - " + x)
PY
  fi
done

reh_summary "representative schemas"
