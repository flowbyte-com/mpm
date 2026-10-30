#!/usr/bin/env bash
# scripts/dogfood_baseline.sh — the daily review block for the seven-day
# OpenCode + MPM dogfood evaluation.
#
# WHAT THIS IS
# ------------
# One command that prints the whole day-zero / day-N state of the substrate
# in a fixed format, so the daily review is a read and five answers rather
# than a half-hour of ad-hoc querying.
#
# WHAT THIS IS NOT
# ----------------
# It is NOT a second analytics subsystem. It is a thin, read-only wrapper
# around:
#
#   * `sqlite3` against the substrate database, and
#   * `sqlite3` against the host's own session database, and
#   * the substrate's own `mpm wake --compact`, run against a SNAPSHOT.
#
# There is no schema of its own, no state of its own, and no second place
# where a measurement is defined. Every number below is a SELECT against
# an authoritative table.
#
# THE READ-ONLY POSTURE IS STRUCTURAL, NOT A PROMISE
# ---------------------------------------------------
# The substrate database is opened through `.backup` into a temporary
# directory first. `.backup` reads from the source and writes only to the
# destination, and the source connection is opened with `mode=ro`, so the
# live database is never opened for writing at any point in this script.
# Every subsequent query runs against the snapshot.
#
# The one command that would otherwise write — `mpm wake`, which records a
# `tool_invocations` row — is run with `MPM_WORKSPACE` pointed at a
# scratch workspace whose `src/db/mpm.db` IS the snapshot. The substrate
# resolves its database from the workspace root, so the wake context is
# reconstructed faithfully while the live installation is untouched.
# This was verified empirically: the live database's size and mtime are
# unchanged by a full run.
#
# USAGE
# -----
#   scripts/dogfood_baseline.sh [--db PATH] [--host-db PATH] [--days N]
#                              [--mpm PATH] [--projects DIR] [--no-wake]
#
#   --db PATH        substrate database
#                    (default: $MPM_WORKSPACE/src/db/mpm.db, else ~/.mpm/src/db/mpm.db)
#   --host-db PATH   host session database
#                    (default: ~/.local/share/opencode/opencode.db)
#   --days N         measurement window in days (default: 7)
#   --mpm PATH       mpm binary used for the wake-context reconstruction
#                    (default: $MPM_BIN, else ~/.mpm/bin/mpm)
#   --no-wake        skip the wake-context reconstruction
#
# EXIT STATUS
# -----------
#   0  the report was produced and no STOP condition is breached
#   1  the report was produced and at least one STOP condition is breached
#   2  the report could not be produced (missing tool, unreadable database)

set -uo pipefail

# ---------------------------------------------------------------- defaults

MPM_WORKSPACE="${MPM_WORKSPACE:-$HOME/.mpm}"
DB="${MPM_WORKSPACE}/src/db/mpm.db"
HOST_DB="$HOME/.local/share/opencode/opencode.db"
DAYS=7
MPM_BIN="${MPM_BIN:-$HOME/.mpm/bin/mpm}"
RUN_WAKE=1

while [[ $# -gt 0 ]]; do
	case "$1" in
		--db) DB="$2"; shift 2 ;;
		--host-db) HOST_DB="$2"; shift 2 ;;
		--days) DAYS="$2"; shift 2 ;;
		--mpm) MPM_BIN="$2"; shift 2 ;;
		--no-wake) RUN_WAKE=0; shift ;;
		-h|--help) sed -n '2,45p' "$0"; exit 0 ;;
		*) echo "unknown argument: $1" >&2; exit 2 ;;
	esac
done

# ---------------------------------------------------------------- preflight

command -v sqlite3 >/dev/null 2>&1 || {
	echo "sqlite3 is required and was not found on PATH" >&2
	exit 2
}
[[ -r "$DB" ]] || {
	echo "substrate database is not readable: $DB" >&2
	exit 2
}

SNAP_DIR="$(mktemp -d)"
trap 'rm -rf "$SNAP_DIR"' EXIT
SNAP_DB="$SNAP_DIR/mpm.db"

# `.backup` reads the source and writes the destination. The source is
# opened read-only, so this cannot write to the live database. It also
# folds the WAL into the destination, so the snapshot is self-contained
# and consistent.
if ! sqlite3 -readonly "file:$DB?mode=ro" ".backup '$SNAP_DB'" 2>/dev/null; then
	echo "could not snapshot $DB" >&2
	exit 2
fi

# A workspace for the wake-context reconstruction. The substrate resolves
# its database from the workspace root, so a scratch workspace whose
# src/db/mpm.db is the snapshot gives a faithful wake context with zero
# production writes.
WAKE_WS="$SNAP_DIR/workspace"
mkdir -p "$WAKE_WS/src/db"
cp "$SNAP_DB" "$WAKE_WS/src/db/mpm.db"
# `active.json` carries the workspace's current MPM session id. Without
# it the wake context reports an empty session, which would silently
# understate what a real session sees.
[[ -r "$MPM_WORKSPACE/active.json" ]] && cp "$MPM_WORKSPACE/active.json" "$WAKE_WS/active.json"

NOW="$(date -u '+%Y-%m-%d %H:%M:%S UTC')"
SINCE_UNIX="$(date -u -d "${DAYS} days ago" '+%s' 2>/dev/null || echo 0)"
SINCE_ISO="$(date -u -d "@${SINCE_UNIX}" '+%Y-%m-%d %H:%M' 2>/dev/null || echo 'unknown')"

STOP_COUNT=0
stop() {
	STOP_COUNT=$((STOP_COUNT + 1))
	printf '  !! STOP  %s\n' "$1"
}

# ---------------------------------------------------------------- helpers

# q <sql> — run one query against the snapshot and print the rows, one per
# line, tab-separated. An empty result prints nothing.
q() {
	sqlite3 -readonly -noheader -separator '	' "$SNAP_DB" "$1" 2>/dev/null
}

# q1 <sql> — run one query against the snapshot and print the first column
# of the first row.
q1() {
	sqlite3 -readonly -noheader "$SNAP_DB" "$1" 2>/dev/null | head -1
}

# hq <sql> — run one query against the HOST database.
hq() {
	sqlite3 -readonly -noheader -separator '	' "$HOST_DB" "$1" 2>/dev/null
}

hq1() {
	sqlite3 -readonly -noheader "$HOST_DB" "$1" 2>/dev/null | head -1
}

# ---------------------------------------------------------------- report

echo "════════════════════════════════════════════════════════════════════"
echo " MPM DOGFOOD — DAILY REVIEW"
echo " generated : ${NOW}"
echo " window    : last ${DAYS} days (from ${SINCE_ISO})"
echo "════════════════════════════════════════════════════════════════════"

# ---- 1. substrate identity ---------------------------------------------

echo
echo "── 1. SUBSTRATE IDENTITY ──────────────────────────────────────────"
echo "  source db    : ${DB}"
echo "  snapshot     : ${SNAP_DIR}/mpm.db (read-only source; snapshot is the only thing queried)"
INTEGRITY="$(q1 'PRAGMA integrity_check;')"
echo "  integrity    : ${INTEGRITY}"
if [[ "$INTEGRITY" != "ok" ]]; then
	stop "database integrity_check returned '${INTEGRITY}' — the substrate is not trustworthy; stop the experiment"
fi
COLL_ROWS="$(q 'SELECT collection, COUNT(*) FROM memories WHERE deleted_at IS NULL GROUP BY collection ORDER BY 2 DESC;')"
echo "  collections  :"
if [[ -n "$COLL_ROWS" ]]; then
	while IFS=$'\t' read -r c n; do
		[[ -z "$c" ]] && continue
		printf '      %-14s %s\n' "$c" "$n"
	done <<<"$COLL_ROWS"
else
	echo "      (none)"
fi
DELTA="$(q1 "SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL AND created_at >= ${SINCE_UNIX};")"
echo "  new rows     : ${DELTA} memory rows created in the window (all collections)"

# ---- 2. project coverage -----------------------------------------------

echo
echo "── 2. PROJECT COVERAGE ────────────────────────────────────────────"
echo "  The project dimension is the HOST's project directory (always recorded)."
echo "  MPM's own project dimension lives in tags; collections are named"
echo "  after a project for AgentShell only. Both are reported."
echo
# slug | MPM tag | host directory fragment. The directory fragment is the
# host's own project identity; the tag is MPM's (incomplete) project
# dimension. The two are reported side by side so the gap is visible.
PROJECTS=(
	"themailroom|themailroom|projects/themailroom"
	"agentshell|agentshell|projects/agent-shell-theme"
	"unaccounted|unaccounted|projects/Unaccounted"
	"deepclaw|deepclaw|projects/deepclaw"
)
printf '  %-14s %10s %12s %10s\n' "project" "tagged rows" "collection" "host sess"
for entry in "${PROJECTS[@]}"; do
	IFS='|' read -r p tag frag <<<"$entry"
	TAGGED="$(q1 "SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL AND tags LIKE '%\"${tag}\"%';")"
	INCOLL="$(q1 "SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL AND collection = '${p}';")"
	SESS="$(hq1 "SELECT COUNT(*) FROM session_v2 WHERE directory LIKE '%${frag}%';")"
	[[ -z "$SESS" ]] && SESS=0
	printf '  %-14s %10s %12s %10s\n' "$p" "$TAGGED" "$INCOLL" "$SESS"
done
ORPHAN_TAGGED="$(q1 "SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL AND collection IN ('themailroom','unaccounted','deepclaw');")"
if [[ "${ORPHAN_TAGGED:-0}" -gt 0 ]]; then
	stop "${ORPHAN_TAGGED} row(s) live in a project-named collection; the collection dimension carries a project name"
fi

# ---- 3. knowledge inventory --------------------------------------------

echo
echo "── 3. KNOWLEDGE INVENTORY ─────────────────────────────────────────"
printf '  %-22s %8s %10s %12s %12s\n' "kind" "total" "new(win)" "surfaced" "evidence"
inv() {
	local label="$1" total="$2" new="$3" surf="$4" evid="$5"
	printf '  %-22s %8s %10s %12s %12s\n' "$label" "$total" "$new" "$surf" "$evid"
}
inv "memory rows (all)" \
	"$(q1 'SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL;')" \
	"$(q1 "SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL AND created_at >= ${SINCE_UNIX};")" \
	"$(q1 "SELECT COUNT(DISTINCT node_id) FROM retrieval_metadata WHERE last_retrieved_at >= ${SINCE_UNIX};")" \
	"$(q1 "SELECT COUNT(DISTINCT artifact_id) FROM evidence WHERE artifact_type='memory';")"
inv "decisions" \
	"$(q1 "SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL AND collection='decisions';")" \
	"$(q1 "SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL AND collection='decisions' AND created_at >= ${SINCE_UNIX};")" \
	"$(q1 "SELECT COUNT(DISTINCT node_id) FROM retrieval_metadata WHERE node_type='decisions' AND last_retrieved_at >= ${SINCE_UNIX};")" \
	"$(q1 "SELECT COUNT(DISTINCT artifact_id) FROM evidence WHERE artifact_type='decision';")"
inv "theories" \
	"$(q1 "SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL AND collection='theories';")" \
	"$(q1 "SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL AND collection='theories' AND created_at >= ${SINCE_UNIX};")" \
	"$(q1 "SELECT COUNT(DISTINCT node_id) FROM retrieval_metadata WHERE node_type='theories' AND last_retrieved_at >= ${SINCE_UNIX};")" \
	"$(q1 "SELECT COUNT(DISTINCT artifact_id) FROM evidence WHERE artifact_type='theory';")"
LESSONS_TOTAL="$(q1 'SELECT COUNT(*) FROM lessons;')"
LESSONS_EVID="$(q1 "SELECT COUNT(DISTINCT artifact_id) FROM evidence WHERE artifact_type='lesson';")"
inv "lessons" "$LESSONS_TOTAL" \
	"$(q1 "SELECT COUNT(*) FROM lessons WHERE created_at >= ${SINCE_UNIX};")" \
	"$(q1 "SELECT COUNT(DISTINCT node_id) FROM retrieval_metadata WHERE node_type='lesson' AND last_retrieved_at >= ${SINCE_UNIX};")" \
	"$LESSONS_EVID"
inv "evidence rows" \
	"$(q1 'SELECT COUNT(*) FROM evidence;')" \
	"$(q1 "SELECT COUNT(*) FROM evidence WHERE created_at >= ${SINCE_UNIX};")" \
	"-" "-"
inv "handoffs" \
	"$(q1 'SELECT COUNT(*) FROM session_handoffs;')" \
	"$(q1 "SELECT COUNT(*) FROM session_handoffs WHERE ended_at >= ${SINCE_UNIX};")" \
	"-" "-"
inv "wakes (scheduled)" \
	"$(q1 'SELECT COUNT(*) FROM scheduled_wakes;')" \
	"$(q1 "SELECT COUNT(*) FROM scheduled_wakes WHERE created_at >= ${SINCE_UNIX};")" \
	"-" "-"

# ---- 4. proposal queues -------------------------------------------------

echo
echo "── 4. PROPOSAL QUEUES ─────────────────────────────────────────────"
T_HEORY_PENDING="$(q1 "SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL AND collection='theories' AND json_extract(metadata,'\$.status')='pending';")"
T_CLUSTER_ACTIVE="$(q1 "SELECT COUNT(*) FROM audit_cluster_proposals WHERE status='active';")"
T_CLUSTER_SNOOZED="$(q1 "SELECT COUNT(*) FROM audit_cluster_proposals WHERE status='snoozed';")"
T_CASCADE_PENDING="$(q1 "SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE status='pending';")"
T_CASCADE_FAILED="$(q1 "SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE status='failed';")"
echo "  theories pending (reviewable) : ${T_HEORY_PENDING}"
echo "  audit clusters active          : ${T_CLUSTER_ACTIVE}  (snoozed: ${T_CLUSTER_SNOOZED})"
echo "  cascade outbox pending/failed  : ${T_CASCADE_PENDING} / ${T_CASCADE_FAILED}"
if [[ "${LESSONS_TOTAL:-0}" -gt 0 && "${LESSONS_EVID:-0}" -eq 0 ]]; then
	echo "  lessons WITHOUT evidence       : ${LESSONS_TOTAL} of ${LESSONS_TOTAL}  <- unsupported-assertion watch list"
fi
CLUST_ROWS="$(q 'SELECT component, message_hash, count, last_seen FROM audit_cluster_proposals WHERE status='"'"'active'"'"' ORDER BY count DESC;')"
if [[ -n "$CLUST_ROWS" ]]; then
	echo "  active clusters (component · count · last seen):"
	while IFS=$'\t' read -r comp _hash cnt seen; do
		[[ -z "$comp" ]] && continue
		printf '      %-22s %4s  %s\n' "$comp" "$cnt" "$seen"
	done <<<"$CLUST_ROWS"
fi

# ---- 5. retrieval -------------------------------------------------------

echo
echo "── 5. RETRIEVAL ────────────────────────────────────────────────────"
SURF_TOTAL="$(q1 'SELECT COUNT(*) FROM retrieval_metadata;')"
SURF_EVER="$(q1 'SELECT COUNT(*) FROM retrieval_metadata WHERE reuse_count > 0;')"
SURF_WIN="$(q1 "SELECT COUNT(*) FROM retrieval_metadata WHERE last_retrieved_at >= ${SINCE_UNIX};")"
NEVER="$(q1 'SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL AND id NOT IN (SELECT node_id FROM retrieval_metadata);')"
echo "  tracked nodes / ever surfaced  : ${SURF_TOTAL} / ${SURF_EVER}"
echo "  nodes surfaced in the window  : ${SURF_WIN}"
echo "  live memories NEVER surfaced  : ${NEVER}"
echo "  NOTE: retrieval_metadata records only the mpm_memory query path."
echo "        The wake-context path (the dominant one) records no per-node"
echo "        trace; see section 9."
TOP="$(q 'SELECT node_type, node_id, reuse_count, datetime(last_retrieved_at,'"'"'unixepoch'"'"') FROM retrieval_metadata ORDER BY reuse_count DESC LIMIT 5;')"
if [[ -n "$TOP" ]]; then
	echo "  most-reused nodes:"
	while IFS=$'\t' read -r nt nid rc seen; do
		[[ -z "$nt" ]] && continue
		printf '      %-14s %-18s x%-4s %s\n' "$nt" "${nid:0:16}" "$rc" "$seen"
	done <<<"$TOP"
fi

# ---- 6. tool activity ---------------------------------------------------

echo
echo "── 6. TOOL ACTIVITY ────────────────────────────────────────────────"
INV_TOTAL="$(q1 'SELECT COUNT(*) FROM tool_invocations;')"
INV_WIN="$(q1 "SELECT COUNT(*) FROM tool_invocations WHERE started_at >= ${SINCE_UNIX};")"
INV_ERR="$(q1 "SELECT COUNT(*) FROM tool_invocations WHERE started_at >= ${SINCE_UNIX} AND result_status='error';")"
echo "  invocations (all / window)     : ${INV_TOTAL} / ${INV_WIN}"
echo "  errors in the window           : ${INV_ERR}"
ACTIVITY="$(q "SELECT tool_name||' '||COALESCE(action,''), COUNT(*) FROM tool_invocations WHERE started_at >= ${SINCE_UNIX} GROUP BY 1 ORDER BY 2 DESC LIMIT 8;")"
if [[ -n "$ACTIVITY" ]]; then
	echo "  window activity by tool:"
	while IFS=$'\t' read -r k n; do
		[[ -z "$k" ]] && continue
		printf '      %-34s %s\n' "$k" "$n"
	done <<<"$ACTIVITY"
fi
OC="$(q "SELECT COALESCE(outcome_class,'(unclassified)'), COUNT(*) FROM tool_invocations WHERE started_at >= ${SINCE_UNIX} GROUP BY 1 ORDER BY 2 DESC;")"
if [[ -n "$OC" ]]; then
	echo "  window outcome classes:"
	while IFS=$'\t' read -r k n; do
		[[ -z "$k" ]] && continue
		printf '      %-20s %s\n' "$k" "$n"
	done <<<"$OC"
fi

# ---- 7. tokens ----------------------------------------------------------

echo
echo "── 7. TOKENS (provider-reported, host-side) ────────────────────────"
if [[ ! -r "$HOST_DB" ]]; then
	echo "  host session database not readable: ${HOST_DB}"
	echo "  provider-reported token usage is UNAVAILABLE for this review."
else
	printf '  %-34s %8s %12s %12s %10s %10s %10s\n' \
		"project directory" "sessions" "uncached_in" "cached_in" "output" "reasoning" "cost"
	TOK="$(hq "SELECT s.directory, COUNT(*), SUM(s.tokens_input), SUM(s.tokens_cache_read), SUM(s.tokens_output), SUM(s.tokens_reasoning), ROUND(SUM(s.cost),4) FROM session_v2 s WHERE s.time_created >= ${SINCE_UNIX} GROUP BY s.directory ORDER BY 3 DESC;")"
	if [[ -n "$TOK" ]]; then
		while IFS=$'\t' read -r d n ti tcr to_ tr tc; do
			[[ -z "$d" ]] && continue
			printf '  %-34s %8s %12s %12s %10s %10s %10s\n' \
				"${d:0:34}" "$n" "${ti:-0}" "${tcr:-0}" "${to_:-0}" "${tr:-0}" "${tc:-0}"
		done <<<"$TOK"
	fi
	echo "  cached/uncached/output/reasoning are PROVIDER-REPORTED counters."
	echo "  They are never summed with the local character-based estimate,"
	echo "  which is reported separately and labelled as an estimate."
	COSTZERO="$(hq1 "SELECT COUNT(*) FROM session_v2 s JOIN session_message m ON m.session_id = s.id WHERE s.time_created >= ${SINCE_UNIX} AND json_extract(m.data,'\$.model.providerID')='minimax-coding-plan' AND COALESCE(json_extract(m.data,'\$.cost'),0)=0;")"
	if [[ "${COSTZERO:-0}" -gt 0 ]]; then
		echo "  cost reported as 0 for ${COSTZERO} minimax-coding-plan message(s):"
		echo "    the coding-plan provider reports usage but NOT cost. Price those"
		echo "    from the telemetry pricing catalog at read time and label the"
		echo "    result as an ESTIMATE, never as provider-reported."
	fi
fi

# ---- 8. operational -----------------------------------------------------

echo
echo "── 8. OPERATIONAL ──────────────────────────────────────────────────"
STATE_FILE="${MPM_WORKSPACE}/run/scheduler.state"
if [[ -r "$STATE_FILE" ]]; then
	echo "  scheduler state: ${STATE_FILE}"
	q1 "SELECT '    pid='||pid||'  tick_count='||tick_count||'  last_status='||last_status||'  last_error='||COALESCE(NULLIF(last_error,''),'(none)')||'  last_tick='||datetime(last_tick_unix,'unixepoch')||'  age_s='||(strftime('%s','now') - last_tick_unix) FROM (SELECT json_extract(readfile('${STATE_FILE}'),'\$.pid') AS pid, json_extract(readfile('${STATE_FILE}'),'\$.tick_count') AS tick_count, json_extract(readfile('${STATE_FILE}'),'\$.last_status') AS last_status, json_extract(readfile('${STATE_FILE}'),'\$.last_error') AS last_error, json_extract(readfile('${STATE_FILE}'),'\$.last_tick_unix') AS last_tick_unix);"
	SPID="$(q1 "SELECT json_extract(readfile('${STATE_FILE}'),'\$.pid');")"
	AGE="$(q1 "SELECT strftime('%s','now') - json_extract(readfile('${STATE_FILE}'),'\$.last_tick_unix');")"
	STATUS="$(q1 "SELECT json_extract(readfile('${STATE_FILE}'),'\$.last_status');")"
	if [[ -n "$SPID" && ! -d "/proc/${SPID}" ]]; then
		stop "the scheduler pid ${SPID} is not running — the substrate is not being maintained"
	fi
	if [[ "${AGE:-0}" -gt 600 ]]; then
		stop "the scheduler's last tick was ${AGE}s ago — it is stuck or stopped"
	fi
	if [[ "$STATUS" != "ok" && -n "$STATUS" ]]; then
		stop "the scheduler's last status is '${STATUS}'"
	fi
else
	echo "  scheduler state file not found: ${STATE_FILE}"
	stop "no scheduler state file — the substrate's wake executor is not observable"
fi
CONFLICTED=0
for f in "${MPM_WORKSPACE}/run/"*"conflicted"*.state; do
	[[ -e "$f" ]] || continue
	CONFLICTED=$((CONFLICTED + 1))
	echo "  stale state file present: ${f}"
done
echo "  conflicted state files  : ${CONFLICTED}"
echo "  WAL size                : $(du -h "$(dirname "$DB")/mpm.db-wal" 2>/dev/null | cut -f1 || echo unknown)"
AUDIT="$(q "SELECT level||' '||component, COUNT(*) FROM system_audit_log WHERE created_at >= ${SINCE_UNIX} AND level IN ('error','warn','fatal','critical') GROUP BY 1 ORDER BY 2 DESC;")"
if [[ -n "$AUDIT" ]]; then
	echo "  audit log (window, warn+):"
	while IFS=$'\t' read -r k n; do
		[[ -z "$k" ]] && continue
		printf '      %-30s %s\n' "$k" "$n"
	done <<<"$AUDIT"
else
	echo "  audit log (window)      : no warn/error entries"
fi

# ---- 9. wake context ----------------------------------------------------

echo
echo "── 9. WAKE CONTEXT (reconstructed on the snapshot) ──────────────────"
if [[ "$RUN_WAKE" -eq 1 && -x "$MPM_BIN" ]]; then
	WAKE_OUT="$(MPM_WORKSPACE="$WAKE_WS" timeout 120 "$MPM_BIN" wake --compact 2>/dev/null)"
	if [[ -n "$WAKE_OUT" ]]; then
		W_IDS="$(printf '%s' "$WAKE_OUT" | grep -o '"[0-9a-f]\{16\}"' | tr -d '"' | sort -u | wc -l | tr -d ' ')"
		echo "  wake context reconstructed: ${W_IDS} distinct artifact ids surfaced"
		printf '%s' "$WAKE_OUT" | grep -o '"last_handoff_summary":[^,]*' | sed 's/^/  /' | cut -c1-160
	else
		echo "  wake context could not be reconstructed (the mpm binary produced no output)"
	fi
	echo "  This is the CONTINUITY SURFACE the substrate actually uses."
	echo "  It is reconstructed rather than observed: the live run's wake"
	echo "  context is not persisted, so it cannot be read back after the fact."
else
	echo "  skipped (no mpm binary, or --no-wake)"
fi

# ---- 10. review ---------------------------------------------------------

echo
echo "── 10. REVIEW — five answers ───────────────────────────────────────"
cat <<'REVIEW'
  Answer these from the session transcripts, not from the numbers above.
  The numbers tell you WHAT happened; only you can say whether it was
  correct.

  [ ] 1. Was the knowledge you needed in the wake context or a query?
        (missed retrievals — §M3 of the plan)
  [ ] 2. Did anything you read belong to a DIFFERENT project?
        (cross-project contamination — §M4 of the plan)
  [ ] 3. Did anything you read turn out to be unsupported?
        (unsupported assertions — §5 of the plan)
  [ ] 4. What did you have to re-learn that MPM already knew?
        (cross-session recall — §M1 of the plan)
  [ ] 5. Anything the substrate did that you did not ask for?
        (surprise / noise — record it verbatim)
REVIEW

echo
echo "── STOP CONDITIONS ─────────────────────────────────────────────────"
if [[ "$STOP_COUNT" -eq 0 ]]; then
	echo "  none breached by this run."
else
	echo "  ${STOP_COUNT} breached — see the STOP markers above."
	echo "  A breached stop condition ends the experiment; see §10 of the plan."
fi
echo
echo "════════════════════════════════════════════════════════════════════"

if [[ "$STOP_COUNT" -gt 0 ]]; then
	exit 1
fi
exit 0
