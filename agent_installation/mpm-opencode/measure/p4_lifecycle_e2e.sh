#!/usr/bin/env bash
# measure/p4_lifecycle_e2e.sh — end-to-end check for the P4 lifecycle observer.
#
# Runs the repository's plugin in the ISOLATED development host
# (scripts/dev_host.sh) against a DISPOSABLE MPM workspace, with the
# adapter's diagnostic dump enabled, then asserts:
#
#   1. the observer initialised and found the host event stream;
#   2. envelopes were produced and every one validates (required fields,
#      payload null, no ingested_at_ms, strict or best-effort id shape);
#   3. NO tool input/output, assistant text or compaction summary appears in
#      any envelope — the dump is scanned for the probe's own strings;
#   4. the counters line reports zero invalid envelopes;
#   5. with MPM_EVENT_INGEST=mpm_system the absent P5 endpoint degrades to
#      `unavailable`, never to a fabricated delivery.
#
# Usage:
#   MPM_WORKSPACE=… MPM_DB_PATH=… MPM_BINARY=… measure/p4_lifecycle_e2e.sh
#
# The pins are required by scripts/dev_host.sh itself; this script adds no
# way to skip them.
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
ROOT="${P4_E2E_ROOT:-/tmp/opencode/p4-lifecycle-e2e}"
DUMP=""   # set per run so both runs are preserved

mkdir -p "$ROOT"

run_host() {
  local label="$1" prompt="$2"
  DUMP="$ROOT/$label.envelopes.jsonl"
  export MPM_EVENT_DEBUG_DUMP="$DUMP"
  export MPM_DEV_HOST_LABEL="$label"
  rm -f "$DUMP"
  ( cd "$REPO_ROOT" && timeout 300 scripts/dev_host.sh run "$prompt" ) \
    > "$ROOT/$label.stdout" 2> "$ROOT/$label.stderr"
}

verdict=0
fail() { echo "FAIL: $*" >&2; verdict=1; }

# ── 1. default: no ingest endpoint configured ──────────────────────────────
unset MPM_EVENT_INGEST
run_host p4-default "Use the shell tool to run: echo p4-e2e-marker. Then reply DONE."

python3 - "$DUMP" <<'PY' || exit 1
import json, sys
path = sys.argv[1]
rows = [json.loads(l) for l in open(path)]
init = [r for r in rows if r.get("kind") == "observer.init"]
env  = [r for r in rows if "event_id" in r]
counters = [r for r in rows if r.get("kind") == "observer.counters"]

ok = True
def check(cond, msg):
    global ok
    if not cond:
        ok = False
        print("FAIL:", msg, file=sys.stderr)

check(bool(init) and init[0].get("has_event_stream") is True, "observer did not see the host event stream")
check(len(env) > 0, "no envelopes were produced")
check(any(c["phase"] == "teardown" for c in counters), "no teardown counters line")

for e in env:
    for key in ("schema_version","event_id","idempotency","event_type","observation",
                "observed_at_ms","host","host_version","source","adapter_version",
                "operation","result_status","trust","meta","payload"):
        check(key in e, f"envelope missing required field {key}")
    check(e["schema_version"] == "mpm-event/0.1", "wrong schema version")
    check(e["payload"] is None, "payload must be null in 0.1")
    check("ingested_at_ms" not in e, "an adapter envelope must not carry ingested_at_ms")
    check(e["observation"] == "observed", "only observed events may be emitted")
    check(e["trust"] == "observation", "trust must be observation")
    check(e["event_type"] in {"session.started","session.observed","context.changed",
                              "tool.outcome","task.boundary","compaction.started",
                              "compaction.completed","handoff.written","critic.proposal"},
          f"event type outside the closed vocabulary: {e['event_type']}")
    check(bool(e["event_id"].startswith("ev1_")) or bool(e["event_id"].startswith("ev1b_")),
          "event_id lacks its idempotency prefix")
    if e["idempotency"] == "strict":
        check(len(e["event_id"]) == 4 + 64, "strict event_id is not ev1_+64 hex")
    for key in e["meta"]:
        check(key in {"host_event_type","host_tool_name","host_action","host_result",
                      "host_agent","host_model_provider","host_model_id","host_message_kind",
                      "host_error_code","compaction_kind","overflow","changed_paths_count",
                      "duration_ms","diff_stat_summary","payload_hash","host_call_id",
                      "host_message_id"}, f"non-allowlisted meta key {key}")

text = open(path, encoding="utf-8").read()
for needle in ("p4-e2e-marker", "echo p4-e2e-marker"):
    check(needle not in text, f"content leaked into the dump: {needle}")

final = [c for c in counters if c.get("phase") == "final"] or counters
c = final[-1]["counters"] if final else {}
check(c.get("invalid_envelope", 0) == 0, "an envelope failed validation")
check(c.get("delivered", 0) == 0, "nothing should be delivered without an ingest endpoint")
check(c.get("dropped_unconfigured", 0) == len(env), "unconfigured drops do not match envelopes built")

print(f"PASS: {len(env)} envelope(s), counters={json.dumps(c)[:300]}")
sys.exit(0 if ok else 1)
PY
[ $? -ne 0 ] && fail "default run"

# ── 2. ingest enabled: the P5 endpoint does not exist yet ──────────────────
export MPM_EVENT_INGEST=mpm_system
run_host p4-ingest "Use the shell tool to run: echo p4-e2e-ingest. Then reply DONE."
unset MPM_EVENT_INGEST

python3 - "$DUMP" <<'PY' || exit 1
import json, sys
rows = [json.loads(l) for l in open(sys.argv[1])]
snaps = [r for r in rows if r.get("kind") == "observer.counters"]
env = [r for r in rows if "event_id" in r]
c = (snaps[-1]["counters"] if snaps else {})
print("ingest-enabled counters:", json.dumps({k: c.get(k) for k in
      ("envelopes_built","delivered","unavailable","rejected","malformed_receipt")}))
ok = (c.get("delivered", 0) == 0 and c.get("malformed_receipt", 0) == 0
      and c.get("unavailable", 0) > 0 and len(env) > 0)
sys.exit(0 if ok else 1)
PY
[ $? -ne 0 ] && fail "ingest-degradation run"

echo
if [ $verdict -eq 0 ]; then
  echo "P4 lifecycle E2E: PASS"
  echo "artifacts: $ROOT"
else
  echo "P4 lifecycle E2E: FAIL" >&2
fi
exit $verdict
