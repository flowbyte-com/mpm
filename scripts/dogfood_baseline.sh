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
# FAIL CLOSED, NOT FAIL QUIET
# ----------------------------
# Every measurement in this script is either a number or an explicit
# UNAVAILABLE. A query that cannot run — a missing table, a renamed column,
# an unreadable database — is recorded as a MEASUREMENT GAP, printed in its
# own section, and makes the run exit non-zero. A measurement that cannot
# be taken is never reported as a measurement of zero.
#
# A SCHEMA DIFFERENCE IS NOT A GAP
# ---------------------------------
# The pre-upgrade schema (no `lesson_proposals` table) is a legitimate,
# supported state. It is reported as a VERSION DIFFERENCE in its own
# section, and the measurement it makes impossible is reported as
# unavailable. It is never reported as "an empty proposal queue".
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
#   0  the report was produced, completely, and no STOP condition is breached
#   1  the report was produced and at least one STOP condition is breached
#   2  the report was INCOMPLETE — a required measurement was missing or
#      unreadable, or the tool could not produce a report at all.
#      A partial report is never reported as a successful one.

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
		-h|--help) sed -n '2,80p' "$0"; exit 0 ;;
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
# RFC3339 boundary. `lessons.created` and `lesson_proposals.created` are
# TEXT columns holding RFC3339 UTC strings, so they are compared as
# strings against an RFC3339 boundary — NOT against an epoch integer.
SINCE_ISO_Z="$(date -u -d "@${SINCE_UNIX}" '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null || echo '1970-01-01T00:00:00Z')"

STOP_COUNT=0
stop() {
	STOP_COUNT=$((STOP_COUNT + 1))
	printf '  !! STOP  %s\n' "$1"
}

# Gaps are appended to a FILE, not to a shell variable: most queries in
# this script are invoked inside `$( )`, which is a subshell, and a
# subshell's variable assignments do not reach the parent.
GAP_FILE="$SNAP_DIR/gaps"
: >"$GAP_FILE"
gap() {
	printf '%s\n' "$1" >>"$GAP_FILE"
}
QERR="$SNAP_DIR/qerr"

# ---------------------------------------------------------------- helpers

# q <sql> — run one query against the snapshot and print the rows, one per
# line, tab-separated. An empty result prints nothing. A query that FAILS
# prints nothing and records a measurement gap.
q() {
	if ! sqlite3 -readonly -noheader -separator '	' "$SNAP_DB" "$1" 2>"$QERR"; then
		gap "query failed: $(head -1 "$QERR" | tr -d '\n')"
		return 1
	fi
	return 0
}

# q1 <sql> — run one query against the snapshot and print the first column
# of the first row. A query that FAILS prints nothing and records a gap.
q1() {
	local out
	if ! out="$(sqlite3 -readonly -noheader "$SNAP_DB" "$1" 2>"$QERR")"; then
		gap "query failed: $(head -1 "$QERR" | tr -d '\n')"
		return 1
	fi
	[[ -n "$out" ]] && printf '%s\n' "$out" | head -1
	return 0
}

# hq <sql> — run one query against the HOST database.
hq() {
	if ! sqlite3 -readonly -noheader -separator '	' "$HOST_DB" "$1" 2>"$QERR"; then
		gap "host database query failed: $(head -1 "$QERR" | tr -d '\n')"
		return 1
	fi
	return 0
}

hq1() {
	local out
	if ! out="$(sqlite3 -readonly -noheader "$HOST_DB" "$1" 2>"$QERR")"; then
		gap "host database query failed: $(head -1 "$QERR" | tr -d '\n')"
		return 1
	fi
	[[ -n "$out" ]] && printf '%s\n' "$out" | head -1
	return 0
}

# gq <sql> — like q, but prints an explicit UNAVAILABLE line when the
# query FAILED (as opposed to one that legitimately returned no rows).
gq() {
	local before after out
	before="$(wc -l <"$GAP_FILE" 2>/dev/null | tr -d ' ')"
	out="$(q "$1")"
	after="$(wc -l <"$GAP_FILE" 2>/dev/null | tr -d ' ')"
	if [[ -z "$out" && "${after:-0}" -gt "${before:-0}" ]]; then
		echo "      UNAVAILABLE — the query could not run (see MEASUREMENT GAPS)"
		return 1
	fi
	[[ -n "$out" ]] && printf '%s\n' "$out"
	return 0
}

# has_table <name> — is the table present in the snapshot? This is how a
# schema VERSION is detected. The lesson-proposal lifecycle is created by
# the base DDL of the post-upgrade runtime, not by a named migration, so
# `schema_migrations` cannot be used to detect it.
has_table() {
	[[ "$(sqlite3 -readonly "$SNAP_DB" "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='$1';")" == "1" ]]
}

# has_column <name> <table>
has_column() {
	[[ "$(sqlite3 -readonly "$SNAP_DB" "SELECT COUNT(*) FROM pragma_table_info('$2') WHERE name='$1';")" != "0" ]]
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
echo "  integrity    : ${INTEGRITY:-UNAVAILABLE}"
if [[ -z "$INTEGRITY" ]]; then
	stop "integrity_check did not run — the substrate's trustworthiness is unknown"
elif [[ "$INTEGRITY" != "ok" ]]; then
	stop "database integrity_check returned '${INTEGRITY}' — the substrate is not trustworthy; stop the experiment"
fi

# The schema VERSION, detected by table presence. The pre-upgrade schema
# is a supported state and is reported as a version difference.
SCHEMA_VERSION="pre-upgrade (no lesson_proposals)"
if has_table lesson_proposals; then
	SCHEMA_VERSION="post-upgrade (lesson_proposals present)"
fi
echo "  schema        : ${SCHEMA_VERSION}"
if [[ -x "$MPM_BIN" ]]; then
	RUNTIME_REV="$("$MPM_BIN" version 2>/dev/null | awk '/^  commit:/ {print $2; exit}')"
	echo "  mpm binary   : ${RUNTIME_REV:-unreported}  ($MPM_BIN)"
	echo "                  (used for the wake reconstruction; the SCHEMA line"
	echo "                   above describes the SNAPSHOT, not this binary)"
else
	echo "  mpm binary   : none at ${MPM_BIN} — the wake reconstruction is unavailable"
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
echo "  new rows     : ${DELTA:-UNAVAILABLE} memory rows created in the window (all collections)"

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
	printf '  %-14s %10s %12s %10s\n' "$p" "${TAGGED:-UNAVAILABLE}" "${INCOLL:-UNAVAILABLE}" "$SESS"
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
	[[ -z "$total" ]] && total="UNAVAIL"
	[[ -z "$new" ]] && new="UNAVAIL"
	[[ -z "$surf" ]] && surf="UNAVAIL"
	[[ -z "$evid" ]] && evid="UNAVAIL"
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
LESSONS_TOTAL="$(q1 'SELECT COUNT(*) FROM lessons_base WHERE deleted_at IS NULL;')"
LESSONS_TOMB="$(q1 'SELECT COUNT(*) FROM lessons_base WHERE deleted_at IS NOT NULL;')"
LESSONS_EVID="$(q1 "SELECT COUNT(DISTINCT artifact_id) FROM evidence WHERE artifact_type='lesson';")"
inv "lessons (trusted)" "$LESSONS_TOTAL" \
	"$(q1 "SELECT COUNT(*) FROM lessons_base WHERE deleted_at IS NULL AND created >= '${SINCE_ISO_Z}';")" \
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
[[ "${LESSONS_TOMB:-0}" -gt 0 ]] && echo "  tombstoned lessons : ${LESSONS_TOMB}"

# ---- 4. proposal queues -------------------------------------------------

echo
echo "── 4. PROPOSAL QUEUES ─────────────────────────────────────────────"
T_HEORY_PENDING="$(q1 "SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL AND collection='theories' AND json_extract(metadata,'\$.status')='pending';")"
T_CLUSTER_ACTIVE="$(q1 "SELECT COUNT(*) FROM audit_cluster_proposals WHERE status='active';")"
T_CLUSTER_SNOOZED="$(q1 "SELECT COUNT(*) FROM audit_cluster_proposals WHERE status='snoozed';")"
T_CASCADE_PENDING="$(q1 "SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE status='pending';")"
T_CASCADE_FAILED="$(q1 "SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE status='failed';")"
echo "  theories pending (reviewable) : ${T_HEORY_PENDING:-UNAVAILABLE}"
echo "  audit clusters active          : ${T_CLUSTER_ACTIVE:-UNAVAILABLE}  (snoozed: ${T_CLUSTER_SNOOZED:-UNAVAILABLE})"
echo "  cascade outbox pending/failed  : ${T_CASCADE_PENDING:-UNAVAILABLE} / ${T_CASCADE_FAILED:-UNAVAILABLE}"
CLUST_ROWS="$(q 'SELECT component, message_hash, count, last_seen FROM audit_cluster_proposals WHERE status='"'"'active'"'"' ORDER BY count DESC;')"
if [[ -n "$CLUST_ROWS" ]]; then
	echo "  active clusters (component · count · last seen):"
	while IFS=$'\t' read -r comp _hash cnt seen; do
		[[ -z "$comp" ]] && continue
		printf '      %-22s %4s  %s\n' "$comp" "$cnt" "$seen"
	done <<<"$CLUST_ROWS"
fi

# ---- 4b. lesson proposals (the trust gate) -----------------------------

echo
echo "── 4b. LESSON PROPOSALS (the trust gate) ───────────────────────────"
# A lesson is TRUSTED ON ARRIVAL: it is in the trusted retrieval set the
# moment it exists. An agent therefore cannot create one; it PROPOSES one,
# and an operator promotes it. Compaction also proposes. The proposal
# queue is therefore the only place a new lesson can wait.
if ! has_table lesson_proposals; then
	cat <<'PRE'
  VERSION DIFFERENCE — this snapshot predates the lesson-proposal lifecycle.
  The `lesson_proposals` table does not exist. That is NOT an empty queue:
  the concept did not exist yet at this revision. The lesson-proposal
  lifecycle below is therefore UNAVAILABLE for this snapshot, by version.
PRE
	echo "  trusted lessons (the only lesson surface at this revision):"
	echo "      ${LESSONS_TOTAL:-UNAVAILABLE}"
else
	LP_TOTAL="$(q1 'SELECT COUNT(*) FROM lesson_proposals;')"
	LP_PENDING="$(q1 "SELECT COUNT(*) FROM lesson_proposals WHERE status='pending';")"
	LP_PROMOTED="$(q1 "SELECT COUNT(*) FROM lesson_proposals WHERE status='promoted';")"
	LP_REJECTED="$(q1 "SELECT COUNT(*) FROM lesson_proposals WHERE status='rejected';")"
	# Compaction-origin: the compaction path stores a {"title","body"}
	# envelope as the proposal content; the agent path stores the raw
	# fact text. A non-empty `source_ids` is a SECONDARY signal (cited
	# sources) and does not by itself identify compaction.
	LP_COMPACT="$(q1 "SELECT COUNT(*) FROM lesson_proposals WHERE json_valid(content) AND json_extract(content,'\$.title') IS NOT NULL;")"
	LP_CITED="$(q1 "SELECT COUNT(*) FROM lesson_proposals WHERE source_ids IS NOT NULL AND source_ids NOT IN ('null','','[]');")"
	LP_PROV="$(q1 "SELECT COUNT(*) FROM lesson_proposals WHERE source_session_id IS NOT NULL AND source_session_id != '';")"
	LP_FRAME="$(q1 "SELECT COUNT(*) FROM lesson_proposals WHERE framework_name IS NOT NULL AND framework_name != '';")"
	LP_MODEL="$(q1 "SELECT COUNT(*) FROM lesson_proposals WHERE model_name IS NOT NULL AND model_name != '';")"
	echo "  trusted lessons (immediately in the retrieval set) : ${LESSONS_TOTAL:-UNAVAILABLE}"
	echo "  proposals total / pending / promoted / rejected    : ${LP_TOTAL:-UNAVAILABLE} / ${LP_PENDING:-UNAVAILABLE} / ${LP_PROMOTED:-UNAVAILABLE} / ${LP_REJECTED:-UNAVAILABLE}"
	echo "  compaction-origin proposals                       : ${LP_COMPACT:-UNAVAILABLE} of ${LP_TOTAL:-UNAVAILABLE}   (content carries a {\"title\":...} envelope)"
	echo "  proposals carrying cited sources (source_ids)     : ${LP_CITED:-UNAVAILABLE} of ${LP_TOTAL:-UNAVAILABLE}"
	echo "  provenance populated: session ${LP_PROV:-UNAVAILABLE}, framework ${LP_FRAME:-UNAVAILABLE}, model ${LP_MODEL:-UNAVAILABLE}  (of ${LP_TOTAL:-UNAVAILABLE})"
	echo "  NOTE: a pending proposal is invisible to lesson search, lesson"
	echo "        list and the wake context until an operator promotes it."
	PENDING_ROWS="$(gq "SELECT id, created, CASE WHEN json_valid(content) AND json_extract(content,'\$.title') IS NOT NULL THEN 'compaction' ELSE 'agent' END, CASE WHEN source_ids IS NOT NULL AND source_ids NOT IN ('null','','[]') THEN json_array_length(source_ids) ELSE 0 END, COALESCE(source_session_id,'-'), COALESCE(framework_name,'-'), COALESCE(model_name,'-') FROM lesson_proposals WHERE status='pending' ORDER BY created;")"
	if [[ -n "$PENDING_ROWS" ]]; then
		echo "  pending proposals (id · created · origin · sources · session · framework · model):"
		while IFS=$'\t' read -r pid pcreated porig psrc psess pframe pmodel; do
			[[ -z "$pid" ]] && continue
			printf '      %-18s %-22s %-10s src=%-4s %-10s %-10s %s\n' \
				"$pid" "$pcreated" "$porig" "$psrc" "${psess:0:10}" "${pframe:0:10}" "${pmodel:0:10}"
		done <<<"$PENDING_ROWS"
	fi
	REVIEWED="$(gq "SELECT status||' '||COUNT(*) FROM lesson_proposals WHERE status IN ('promoted','rejected') GROUP BY status;")"
	if [[ -n "$REVIEWED" ]]; then
		echo "  reviewed proposals (promoted rows carry the durable lesson id):"
		while IFS=$'\t' read -r k n; do
			[[ -z "$k" ]] && continue
			printf '      %-10s %s\n' "$k" "$n"
		done <<<"$REVIEWED"
	fi
fi
if [[ "${LESSONS_TOTAL:-0}" -gt 0 && "${LESSONS_EVID:-0}" -eq 0 ]]; then
	echo "  lessons with NO DIRECT evidence link : 0 of ${LESSONS_TOTAL}"
	echo "    Zero DIRECTLY LINKED evidence rows means that particular linkage"
	echo "    is absent. It is NOT a claim that the lesson is false, and NOT a"
	echo "    claim that the lesson has no other provenance (provenance rows,"
	echo "    source sessions and review history are separate evidence paths)."
fi

# ---- 5. retrieval -------------------------------------------------------

echo
echo "── 5. RETRIEVAL ────────────────────────────────────────────────────"
SURF_TOTAL="$(q1 'SELECT COUNT(*) FROM retrieval_metadata;')"
SURF_EVER="$(q1 'SELECT COUNT(*) FROM retrieval_metadata WHERE reuse_count > 0;')"
SURF_WIN="$(q1 "SELECT COUNT(*) FROM retrieval_metadata WHERE last_retrieved_at >= ${SINCE_UNIX};")"
NEVER="$(q1 'SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL AND id NOT IN (SELECT node_id FROM retrieval_metadata);')"
echo "  tracked nodes / ever surfaced  : ${SURF_TOTAL:-UNAVAILABLE} / ${SURF_EVER:-UNAVAILABLE}"
echo "  nodes surfaced in the window  : ${SURF_WIN:-UNAVAILABLE}"
echo "  live memories NEVER surfaced  : ${NEVER:-UNAVAILABLE}"
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
echo "  invocations (all / window)     : ${INV_TOTAL:-UNAVAILABLE} / ${INV_WIN:-UNAVAILABLE}"
echo "  errors in the window           : ${INV_ERR:-UNAVAILABLE}"
ACTIVITY="$(q "SELECT tool_name||' '||COALESCE(action,''), COUNT(*) FROM tool_invocations WHERE started_at >= ${SINCE_UNIX} GROUP BY 1 ORDER BY 2 DESC LIMIT 8;")"
if [[ -n "$ACTIVITY" ]]; then
	echo "  window activity by tool:"
	while IFS=$'\t' read -r k n; do
		[[ -z "$k" ]] && continue
		printf '      %-34s %s\n' "$k" "$n"
	done <<<"$ACTIVITY"
fi

# ---- 7. write reliability (M5) -----------------------------------------
#
# M5 separates REPORTED SUCCESS from CONFIRMED DURABILITY.
#
# The attempted-write ledger is `tool_invocations`. It records what the
# tool SURFACE was asked to do and what it reported back. It does NOT
# record every write the substrate performs: the CLI and the internal
# paths write without a tool invocation, and `system_audit_log` is where
# some of those failures surface. So the attempted-write count below is a
# LOWER BOUND, and the blind spots are named rather than hidden.

echo
echo "── 7. WRITE RELIABILITY (M5) ───────────────────────────────────────"
echo "  Reported success is NOT durability. A tool that reports success has"
echo "  said the write completed; durability means the artifact can be FOUND"
echo "  afterwards. The two are measured separately below."
echo

# The set of tool/action pairs that are WRITES. Anything not in this list
# is a read and is excluded from the attempted-write count.
WRITE_PRED="(
	(tool_name='mpm_memory' AND action IN ('save','patch','delete','restore','reinforce','shred')) OR
	(tool_name='mpm_decisions' AND action IN ('record','supersede','delete','shred')) OR
	(tool_name='mpm_lessons' AND action IN ('save','propose','add','delete','restore','shred','promote','reject')) OR
	(tool_name='mpm_handoff' AND action IN ('write','delete','shred')) OR
	(tool_name='mpm_context' AND action='write_handoff') OR
	(tool_name='mpm_log_to_changelog') OR
	(tool_name='mpm_theories' AND action IN ('propose','resolve')) OR
	(tool_name='mpm_work' AND action IN ('create','note','complete','update')) OR
	(tool_name='mpm_skills' AND action='save') OR
	(tool_name='mpm_evidence' AND action='add') OR
	(tool_name='mpm_system' AND action IN ('compact','cascade_materialize')) OR
	(tool_name='mpm_scratchpad' AND action IN ('write','flush'))
)"

W_TOTAL="$(q1 "SELECT COUNT(*) FROM tool_invocations WHERE ${WRITE_PRED};")"
W_WIN="$(q1 "SELECT COUNT(*) FROM tool_invocations WHERE ${WRITE_PRED} AND started_at >= ${SINCE_UNIX};")"
W_OK="$(q1 "SELECT COUNT(*) FROM tool_invocations WHERE ${WRITE_PRED} AND started_at >= ${SINCE_UNIX} AND result_status='success';")"
W_ERR="$(q1 "SELECT COUNT(*) FROM tool_invocations WHERE ${WRITE_PRED} AND started_at >= ${SINCE_UNIX} AND result_status='error';")"
echo "  attempted writes OBSERVED (tool ledger, all time / window) : ${W_TOTAL:-UNAVAILABLE} / ${W_WIN:-UNAVAILABLE}"
echo "    reported success / reported failure in the window         : ${W_OK:-UNAVAILABLE} / ${W_ERR:-UNAVAILABLE}"
echo "    LOWER BOUND: CLI and internal write paths are not in this ledger."
echo

echo "  7a. reported outcome by class (window):"
OC_W="$(gq "SELECT COALESCE(NULLIF(outcome_class,''),'(unclassified)'), COUNT(*) FROM tool_invocations WHERE ${WRITE_PRED} AND started_at >= ${SINCE_UNIX} GROUP BY 1 ORDER BY 2 DESC;")"
if [[ -n "$OC_W" ]]; then
	while IFS=$'\t' read -r k n; do
		[[ -z "$k" ]] && continue
		printf '      %-20s %s\n' "$k" "$n"
	done <<<"$OC_W"
else
	echo "      (no write invocations in the window)"
fi
echo "    'internal' does NOT mean the substrate failed a well-formed"
echo "    request: it is dominated by misclassified payload-shape errors"
echo "    (a caller sent the wrong shape) and by policy refusals. The"
echo "    breakdown below is the honest reading of that class."

echo
echo "  7b. the 'internal' class, broken down by cause (window):"
INT_ROWS="$(gq "SELECT substr(COALESCE(error_message,'(none)'),1,72), COUNT(*) FROM tool_invocations WHERE ${WRITE_PRED} AND started_at >= ${SINCE_UNIX} AND outcome_class='internal' GROUP BY 1 ORDER BY 2 DESC LIMIT 6;")"
if [[ -n "$INT_ROWS" ]]; then
	while IFS=$'\t' read -r msg n; do
		[[ -z "$msg" ]] && continue
		printf '      %4s  %s\n' "$n" "$msg"
	done <<<"$INT_ROWS"
else
	echo "      (none in the window)"
fi

echo
echo "  7c. reported success vs CONFIRMED durability, per surface:"
printf '      %-26s %8s %8s %10s %s\n' "surface" "ok" "sound" "confirmed" "verdict"
# A link is only USED when it is SOUND:
#   * tool_invocations.invocation_id is NOT unique (one id can carry rows
#     from several tools), and
#   * artifact_provenance can carry several rows for one invocation_id.
# A join on a shared id multiplies rows and manufactures a confirmation
# that did not happen, so a link is counted only when the invocation_id
# maps to exactly one ledger row AND exactly one provenance row.
# 'confirmed' then means the artifact named by that provenance row is
# FOUND in the table its artifact_type names.
CONFIRM_SQL="SELECT
	ti.tool_name||' '||ti.action AS surface,
	SUM(CASE WHEN ti.result_status='success' THEN 1 ELSE 0 END) AS ok,
	SUM(CASE WHEN ti.result_status='success' AND lk.n=1 AND pv.n=1 THEN 1 ELSE 0 END) AS sound,
	SUM(CASE WHEN ti.result_status='success' AND lk.n=1 AND pv.n=1 AND (
		(ap.artifact_type='memory'  AND m.id IS NOT NULL) OR
		(ap.artifact_type='decision' AND md.id IS NOT NULL) OR
		(ap.artifact_type='theory'   AND mt.id IS NOT NULL) OR
		(ap.artifact_type='lesson'   AND lb.id IS NOT NULL) OR
		(ap.artifact_type='work'     AND wk.id IS NOT NULL) OR
		(ap.artifact_type='handoff'  AND ho.id IS NOT NULL)) THEN 1 ELSE 0 END) AS confirmed
FROM tool_invocations ti
LEFT JOIN (SELECT invocation_id, COUNT(*) n FROM tool_invocations GROUP BY invocation_id) lk ON lk.invocation_id = ti.invocation_id
LEFT JOIN (SELECT invocation_id, COUNT(*) n FROM artifact_provenance WHERE invocation_id IS NOT NULL AND invocation_id != '' GROUP BY invocation_id) pv ON pv.invocation_id = ti.invocation_id
LEFT JOIN artifact_provenance ap ON ap.invocation_id = ti.invocation_id AND pv.n=1 AND lk.n=1
LEFT JOIN memories m  ON m.id  = ap.artifact_id AND ap.artifact_type='memory'
LEFT JOIN memories md ON md.id = ap.artifact_id AND ap.artifact_type='decision'
LEFT JOIN memories mt ON mt.id = ap.artifact_id AND ap.artifact_type='theory'
LEFT JOIN lessons_base lb ON lb.id = ap.artifact_id AND ap.artifact_type='lesson'
LEFT JOIN works wk ON wk.id = ap.artifact_id AND ap.artifact_type='work'
LEFT JOIN session_handoffs ho ON ho.id = ap.artifact_id AND ap.artifact_type='handoff'
WHERE ${WRITE_PRED} AND ti.result_status='success'
GROUP BY 1 ORDER BY 2 DESC;"
GAP_BEFORE="$(wc -l <"$GAP_FILE" 2>/dev/null | tr -d ' ')"
CONFIRM_ROWS="$(q "$CONFIRM_SQL")"
GAP_AFTER="$(wc -l <"$GAP_FILE" 2>/dev/null | tr -d ' ')"
if [[ -n "$CONFIRM_ROWS" ]]; then
	while IFS=$'\t' read -r surface ok sound confirmed; do
		[[ -z "$surface" ]] && continue
		if [[ "${sound:-0}" -gt 0 && "${confirmed:-0}" == "${sound}" ]]; then
			verdict="CONFIRMED per write"
		elif [[ "${sound:-0}" -gt 0 ]]; then
			verdict="PARTIAL: ${confirmed:-0} of ${sound} confirmed"
		else
			verdict="UNCONFIRMABLE per write (no sound link)"
		fi
		printf '      %-26s %8s %8s %10s %s\n' "$surface" "$ok" "$sound" "$confirmed" "$verdict"
	done <<<"$CONFIRM_ROWS"
elif [[ "${GAP_AFTER:-0}" -gt "${GAP_BEFORE:-0}" ]]; then
	echo "      UNAVAILABLE — the confirmation query could not run (see MEASUREMENT GAPS)"
else
	echo "      (no successful write invocations to confirm)"
fi
echo "    Only the surfaces with a non-zero 'sound' column can be confirmed"
echo "    PER WRITE. Every other surface is AGGREGATE-ONLY: the write and"
echo "    the artifact cannot be paired, so its durability is unmeasured."
echo

# Aggregate cross-check: rows that exist against rows the ledger reports.
MEM_OK="$(q1 "SELECT COUNT(*) FROM tool_invocations WHERE tool_name='mpm_memory' AND action='save' AND result_status='success';")"
MEM_PROV="$(q1 "SELECT COUNT(*) FROM artifact_provenance ap JOIN memories m ON m.id=ap.artifact_id WHERE ap.artifact_type='memory';")"
DEC_OK="$(q1 "SELECT COUNT(*) FROM tool_invocations WHERE tool_name='mpm_decisions' AND action='record' AND result_status='success';")"
DEC_ROWS="$(q1 "SELECT COUNT(*) FROM memories WHERE collection='decisions' AND deleted_at IS NULL;")"
LES_OK="$(q1 "SELECT COUNT(*) FROM tool_invocations WHERE tool_name='mpm_lessons' AND action='save' AND result_status='success';")"
LES_ROWS="$(q1 "SELECT COUNT(*) FROM lessons_base WHERE deleted_at IS NULL;")"
HO_OK="$(q1 "SELECT COUNT(*) FROM tool_invocations WHERE tool_name='mpm_handoff' AND action='write' AND result_status='success';")"
HO_ROWS="$(q1 "SELECT COUNT(*) FROM session_handoffs;")"
echo "  7d. aggregate cross-checks (reported vs rows that exist):"
printf '      %-34s %8s %8s %s\n' "surface" "reported" "rows" "note"
printf '      %-34s %8s %8s %s\n' "mpm_memory save" "${MEM_OK:-?}" "${MEM_PROV:-?}" "provenance rows matching a live memory"
printf '      %-34s %8s %8s %s\n' "mpm_decisions record" "${DEC_OK:-?}" "${DEC_ROWS:-?}" "decisions collection rows"
printf '      %-34s %8s %8s %s\n' "mpm_lessons save" "${LES_OK:-?}" "${LES_ROWS:-?}" "trusted lesson rows"
printf '      %-34s %8s %8s %s\n' "mpm_handoff write" "${HO_OK:-?}" "${HO_ROWS:-?}" "session_handoffs rows (upsert on session_id)"
echo "    These are NOT per-write comparisons, and the arithmetic does not"
echo "    isolate loss in either direction. A handoff upsert on an existing"
echo "    session_id REPLACES a row instead of adding one, so a lower row"
echo "    count is expected. The row counts also include CLI and internal"
echo "    writes that were never tool calls."

echo
echo "  7e. writes with an UNKNOWN outcome:"
LINK_SPLIT="$(gq "SELECT CASE WHEN NOT EXISTS (SELECT 1 FROM artifact_provenance ap WHERE ap.invocation_id=ti.invocation_id) THEN 'no provenance row at all' WHEN lk.n>1 THEN 'invocation_id shared by more than one ledger row' WHEN pv.n>1 THEN 'more than one provenance row for the invocation' ELSE 'sound link' END, COUNT(*) FROM tool_invocations ti LEFT JOIN (SELECT invocation_id, COUNT(*) n FROM tool_invocations GROUP BY invocation_id) lk ON lk.invocation_id=ti.invocation_id LEFT JOIN (SELECT invocation_id, COUNT(*) n FROM artifact_provenance WHERE invocation_id IS NOT NULL AND invocation_id!='' GROUP BY invocation_id) pv ON pv.invocation_id=ti.invocation_id WHERE ti.tool_name='mpm_memory' AND ti.action='save' AND ti.result_status='success' GROUP BY 1 ORDER BY 2 DESC;")"
if [[ -n "$LINK_SPLIT" ]]; then
	while IFS=$'\t' read -r k n; do
		[[ -z "$k" ]] && continue
		printf '      %4s  mpm_memory save: %s\n' "$n" "$k"
	done <<<"$LINK_SPLIT"
fi
echo "    A reported success with no provenance row and no returned artifact"
echo "    id has an UNKNOWN outcome: neither confirmed nor lost, unmeasured."
echo "    A shared invocation_id makes the link ambiguous, which is a"
echo "    measurement blind spot rather than a lost write."

echo
echo "  7f. SQLite busy/locked (the #178 failure mode):"
BUSY_TOOL="$(q1 "SELECT COUNT(*) FROM tool_invocations WHERE outcome_code='wal_busy_timeout';")"
BUSY_WIN="$(q1 "SELECT COUNT(*) FROM tool_invocations WHERE outcome_code='wal_busy_timeout' AND started_at >= ${SINCE_UNIX};")"
BUSY_AUDIT="$(q1 "SELECT COUNT(*) FROM system_audit_log WHERE lower(message) LIKE '%database is locked%' OR lower(message) LIKE '%sqlite_busy%' OR lower(message) LIKE '%busy%';")"
echo "    tool ledger  wal_busy_timeout (all time / window) : ${BUSY_TOOL:-UNAVAILABLE} / ${BUSY_WIN:-UNAVAILABLE}"
echo "    audit log    busy/locked messages (all time)       : ${BUSY_AUDIT:-UNAVAILABLE}"
BUSY_ROWS="$(gq "SELECT datetime(started_at,'unixepoch'), tool_name||' '||action, substr(COALESCE(error_message,''),1,50) FROM tool_invocations WHERE outcome_code='wal_busy_timeout' ORDER BY started_at;")"
if [[ -n "$BUSY_ROWS" ]]; then
	while IFS=$'\t' read -r at k msg; do
		[[ -z "$at" ]] && continue
		printf '      %s  %-22s %s\n' "$at" "$k" "$msg"
	done <<<"$BUSY_ROWS"
fi
echo "    NOTE: system_audit_log.created_at is a TEXT timestamp stored in"
echo "    an INTEGER-declared column, so an epoch comparison against it"
echo "    matches EVERY row. This script converts with strftime('%s',…)."

echo
echo "  7g. handoff persistence failures (audit log):"
HO_FAIL="$(q1 "SELECT COUNT(*) FROM system_audit_log WHERE component='handoff' AND message LIKE '%failed%';")"
echo "    'EndSessionV2 … failed' rows (all time) : ${HO_FAIL:-UNAVAILABLE}"
HO_FAIL_ROWS="$(gq "SELECT created_at, substr(message,1,58) FROM system_audit_log WHERE component='handoff' AND message LIKE '%failed%' ORDER BY created_at;")"
if [[ -n "$HO_FAIL_ROWS" ]]; then
	while IFS=$'\t' read -r at msg; do
		[[ -z "$at" ]] && continue
		EP="$(sqlite3 -readonly "$SNAP_DB" "SELECT CAST(strftime('%s','${at}') AS INTEGER);" 2>/dev/null)"
		NEAR="no nearby tool call"
		if [[ -n "$EP" ]]; then
			NEAR="$(sqlite3 -readonly "$SNAP_DB" "SELECT ti.result_status||'/'||COALESCE(NULLIF(ti.outcome_class,''),'-') FROM tool_invocations ti WHERE ti.tool_name='mpm_handoff' AND ti.action='write' AND ti.started_at BETWEEN ${EP}-5 AND ${EP}+5 ORDER BY ABS(ti.started_at-${EP}) LIMIT 1;" 2>/dev/null)"
			[[ -z "$NEAR" ]] && NEAR="no nearby tool call"
		fi
		printf '      %s  %-60s -> %s\n' "$at" "$msg" "$NEAR"
	done <<<"$HO_FAIL_ROWS"
fi
echo "    A handoff write that reaches the table and violates a CHECK is a"
echo "    REPORTED failure, not a silent loss. The post-upgrade runtime"
echo "    validates the enumeration BEFORE the write, so this failure mode"
echo "    is expected to stop appearing — watch for it, do not assume it."

echo
echo "  7h. provenance integrity (the link itself):"
ORPHAN="$(q1 "SELECT COUNT(*) FROM artifact_provenance ap WHERE (ap.artifact_type='memory' AND ap.artifact_id NOT IN (SELECT id FROM memories)) OR (ap.artifact_type='lesson' AND ap.artifact_id NOT IN (SELECT id FROM lessons_base)) OR (ap.artifact_type='decision' AND ap.artifact_id NOT IN (SELECT id FROM memories WHERE collection='decisions')) OR (ap.artifact_type='theory' AND ap.artifact_id NOT IN (SELECT id FROM memories WHERE collection='theories'));")"
NOINV="$(q1 "SELECT COUNT(*) FROM artifact_provenance ap WHERE ap.invocation_id IS NOT NULL AND ap.invocation_id != '' AND NOT EXISTS (SELECT 1 FROM tool_invocations ti WHERE ti.invocation_id = ap.invocation_id);")"
echo "    provenance rows whose artifact is NOT in the table its type names : ${ORPHAN:-UNAVAILABLE}"
echo "    provenance rows citing an invocation the ledger does not contain  : ${NOINV:-UNAVAILABLE}"
echo "    The first count is normally 1, not 0: the artifact-type namespace"
echo "    collides. A MEMORY saved into the 'lessons' collection is recorded"
echo "    with artifact_type='lesson' while the id belongs to a memory row."

echo
echo "  7i. scheduler correlation (busy/locked vs scheduler ticks):"
TICKS_FILE="$SNAP_DIR/ticks.txt"
: >"$TICKS_FILE"
BUSY_FILE="$SNAP_DIR/busy.txt"
: >"$BUSY_FILE"
if command -v journalctl >/dev/null 2>&1; then
	JOURNAL="$(journalctl --user -u mpm-scheduler --no-pager -o cat --since "$(date -u -d "@${SINCE_UNIX}" '+%Y-%m-%d %H:%M:%S' 2>/dev/null || echo '7 days ago')" 2>/dev/null)"
	printf '%s\n' "$JOURNAL" | grep -oE 'time=[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:.]+Z' | cut -d= -f2 >"$SNAP_DIR/ticks.iso"
	# ONE `date -f` for the whole batch. Converting each tick with its own
	# `date` invocation costs a process per tick — thousands of them on a
	# busy week, which made the daily run take twelve seconds.
	date -u -f "$SNAP_DIR/ticks.iso" '+%s' >"$TICKS_FILE" 2>/dev/null || : >"$TICKS_FILE"
	q "SELECT started_at FROM tool_invocations WHERE outcome_code='wal_busy_timeout';" | tr -d ' ' >"$BUSY_FILE"
	TICK_N="$(wc -l <"$TICKS_FILE" | tr -d ' ')"
	BUSY_N="$(wc -l <"$BUSY_FILE" | tr -d ' ')"
	echo "    scheduler ticks observed in the window : ${TICK_N}"
	echo "    busy/locked events in the window       : ${BUSY_N}"
	if [[ "${TICK_N:-0}" -eq 0 && "${BUSY_N:-0}" -eq 0 ]]; then
		echo "    no busy/locked event to correlate."
	elif [[ "${TICK_N:-0}" -eq 0 ]]; then
		echo "    UNAVAILABLE: the scheduler journal holds no tick for this window,"
		echo "    so the busy/locked -> tick correlation could not be computed."
		gap "scheduler journal produced no tick for the window"
	else
		awk 'NR==FNR { tick[FNR]=$1; n=FNR; next }
		     { for (i=1;i<=n;i++) { d=$1-tick[i]; if (d<0) d=-d; if (d<=60) { near++; break } }
		       total++ }
		     END { printf "    busy/locked events within 60s of a scheduler tick : %d of %d\n", near+0, total+0 }' \
			"$TICKS_FILE" "$BUSY_FILE"
	fi
else
	echo "    UNAVAILABLE: journalctl is not available on this host, so the"
	echo "    busy/locked -> scheduler-tick correlation could not be computed."
	gap "journalctl is unavailable — the scheduler correlation could not be computed"
fi

# ---- 8. tokens ----------------------------------------------------------

echo
echo "── 8. TOKENS (provider-reported, host-side) ────────────────────────"
if [[ ! -r "$HOST_DB" ]]; then
	echo "  host session database not readable: ${HOST_DB}"
	echo "  provider-reported token usage is UNAVAILABLE for this review."
	gap "host session database is not readable — token accounting is unavailable"
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
	else
		echo "  (no host sessions in the window)"
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

# ---- 9. operational -----------------------------------------------------

echo
echo "── 9. OPERATIONAL ──────────────────────────────────────────────────"
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
# `system_audit_log.created_at` is TEXT inside an INTEGER-declared column.
# An epoch comparison against it matches every row, so the window is
# computed with strftime('%s', created_at).
AUDIT="$(q "SELECT level||' '||component, COUNT(*) FROM system_audit_log WHERE CAST(strftime('%s',created_at) AS INTEGER) >= ${SINCE_UNIX} AND level IN ('error','warn','fatal','critical') GROUP BY 1 ORDER BY 2 DESC;")"
if [[ -n "$AUDIT" ]]; then
	echo "  audit log (window, warn+):"
	while IFS=$'\t' read -r k n; do
		[[ -z "$k" ]] && continue
		printf '      %-30s %s\n' "$k" "$n"
	done <<<"$AUDIT"
else
	echo "  audit log (window)      : no warn/error entries"
fi

# ---- 10. wake context ----------------------------------------------------

echo
echo "── 10. WAKE CONTEXT (reconstructed on the snapshot) ─────────────────"
if [[ "$RUN_WAKE" -eq 1 && -x "$MPM_BIN" ]]; then
	WAKE_OUT="$(MPM_WORKSPACE="$WAKE_WS" timeout 120 "$MPM_BIN" wake --compact 2>/dev/null)"
	if [[ -n "$WAKE_OUT" ]]; then
		W_IDS="$(printf '%s' "$WAKE_OUT" | grep -o '"[0-9a-f]\{16\}"' | tr -d '"' | sort -u | wc -l | tr -d ' ')"
		echo "  wake context reconstructed: ${W_IDS} distinct artifact ids surfaced"
		printf '%s' "$WAKE_OUT" | grep -o '"last_handoff_summary":[^,]*' | sed 's/^/  /' | cut -c1-160
	else
		echo "  wake context could not be reconstructed (the mpm binary produced no output)"
		gap "the wake context could not be reconstructed on the snapshot"
	fi
	echo "  This is the CONTINUITY SURFACE the substrate actually uses."
	echo "  It is reconstructed rather than observed: the live run's wake"
	echo "  context is not persisted, so it cannot be read back after the fact."
	echo "  It therefore carries NO per-node retrieval trace: the wake path"
	echo "  records no retrieval_metadata row, so M1 is a lower bound."
else
	echo "  skipped (no mpm binary, or --no-wake)"
fi

# ---- 11. review ---------------------------------------------------------

echo
echo "── 11. REVIEW — five answers ───────────────────────────────────────"
cat <<'REVIEW'
  Answer these from the session transcripts, not from the numbers above.
  The numbers tell you WHAT happened; only you can say whether it was
  correct.

  [ ] 1. Was the knowledge you needed in the wake context or a query?
        (missed retrievals — §M3 of the plan)
  [ ] 2. Did anything you read belong to a DIFFERENT project?
        (cross-project contamination — §M4 of the plan)
  [ ] 3. Did anything you read turn out to be unsupported?
        (support boundary — §6 of the plan)
  [ ] 4. What did you have to re-learn that MPM already knew?
        (cross-session recall — §M1 of the plan)
  [ ] 5. Anything the substrate did that you did not ask for?
        (surprise / noise — record it verbatim)
REVIEW

echo
echo "── MEASUREMENT GAPS ─────────────────────────────────────────────────"
GAP_COUNT="$(wc -l <"$GAP_FILE" 2>/dev/null | tr -d ' ')"
GAP_COUNT="${GAP_COUNT:-0}"
if [[ "$GAP_COUNT" -eq 0 ]]; then
	echo "  none. Every measurement in this report was taken."
else
	echo "  ${GAP_COUNT} measurement(s) could NOT be taken. A missing"
	echo "  measurement is NOT a measurement of zero. This report is"
	echo "  INCOMPLETE and its exit status is 2."
	while IFS= read -r g; do
		[[ -z "$g" ]] && continue
		printf '      - %s\n' "$g"
	done <"$GAP_FILE"
fi

echo
echo "── STOP CONDITIONS ─────────────────────────────────────────────────"
if [[ "$STOP_COUNT" -eq 0 ]]; then
	echo "  none breached by this run."
else
	echo "  ${STOP_COUNT} breached — see the STOP markers above."
	echo "  A breached stop condition ends the experiment; see §9 of the plan."
fi
echo
echo "════════════════════════════════════════════════════════════════════"

if [[ "$GAP_COUNT" -gt 0 ]]; then
	exit 2
fi
if [[ "$STOP_COUNT" -gt 0 ]]; then
	exit 1
fi
exit 0
