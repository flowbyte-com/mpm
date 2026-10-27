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
#   5. the delivery ACCOUNTING closes at every reading: every enqueued
#      envelope is in exactly one bucket — delivered, failed, evicted,
#      abandoned, queued, or in flight;
#   6. with MPM_EVENT_INGEST=mpm_system the ingest endpoint accepts the
#      envelopes (it is no longer absent: P5 shipped it), so the run
#      delivers rather than degrading to `unavailable`.
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

# The delivery accounting must close at EVERY reading — the teardown one
# (written synchronously, so it is the one that survives a host exit) and
# the final one. An envelope that is in flight is named as in flight; it
# is never simply absent from the totals.
def balanced(row):
    c = row["counters"]
    st = row.get("state") or {"queued": 0, "in_flight": 0}
    accounted = (c["delivered"] + c["unavailable"] + c["rejected"]
                 + c["malformed_receipt"] + c.get("delivery_failed", 0)
                 + c["dropped_overflow"] + c.get("abandoned", 0)
                 + st["queued"] + st["in_flight"])
    return accounted, c["enqueued"]

for row in counters:
    accounted, enqueued = balanced(row)
    check(accounted == enqueued,
          f"the {row.get('phase')} reading does not balance: {accounted} != {enqueued}")

final = [c for c in counters if c.get("phase") == "final"] or counters
c = final[-1]["counters"] if final else {}
check(c.get("invalid_envelope", 0) == 0, "an envelope failed validation")
check(c.get("delivered", 0) == 0, "nothing should be delivered without an ingest endpoint")
check(c.get("dropped_unconfigured", 0) == len(env), "unconfigured drops do not match envelopes built")

print(f"PASS: {len(env)} envelope(s), counters={json.dumps(c)[:300]}")
sys.exit(0 if ok else 1)
PY
[ $? -ne 0 ] && fail "default run"

# ── 2. ingest enabled: the P5 ingest endpoint accepts the envelopes ───────
#
# NOTE: this run's expectation CHANGED with P5. The endpoint used to be
# absent, and the assertion was that it degraded to `unavailable`. P5
# shipped `event_ingest`, so the endpoint is present and the envelopes are
# DELIVERED. The stale assertion failed on the pre-correction code too
# (verified 2026-10-27 against origin/main) — it was a stale harness, not
# a regression.
#
# What is asserted now is stronger and is the point of the run: the
# endpoint's receipt is VALID (no malformed receipt, no rejection), at
# least one envelope was really delivered, and the accounting closes.
export MPM_EVENT_INGEST=mpm_system
run_host p4-ingest "Use the shell tool to run: echo p4-e2e-ingest. Then reply DONE."
unset MPM_EVENT_INGEST

python3 - "$DUMP" <<'PY' || exit 1
import json, sys
rows = [json.loads(l) for l in open(sys.argv[1])]
snaps = [r for r in rows if r.get("kind") == "observer.counters"]
env = [r for r in rows if "event_id" in r]
deliveries = [r for r in rows if r.get("kind") == "observer.delivery"]
c = (snaps[-1]["counters"] if snaps else {})
print("ingest-enabled counters:", json.dumps({k: c.get(k) for k in
      ("envelopes_built","enqueued","delivered","duplicates","unavailable",
       "rejected","malformed_receipt","delivery_attempts","abandoned")}))

ok = True
def check(cond, msg):
    global ok
    if not cond:
        ok = False
        print("FAIL:", msg, file=sys.stderr)

check(len(env) > 0, "no envelopes were produced")
check(c.get("delivered", 0) > 0, "the ingest endpoint accepted no envelope")
check(c.get("malformed_receipt", 0) == 0, "the endpoint's receipt was malformed")
check(c.get("rejected", 0) == 0, "the endpoint rejected a valid envelope")

# Every reading must balance: delivered + failed + evicted + abandoned
# + queued + in_flight == enqueued.
for row in snaps:
    cc = row["counters"]
    st = row.get("state") or {"queued": 0, "in_flight": 0}
    accounted = (cc["delivered"] + cc["unavailable"] + cc["rejected"]
                 + cc["malformed_receipt"] + cc.get("delivery_failed", 0)
                 + cc["dropped_overflow"] + cc.get("abandoned", 0)
                 + st["queued"] + st["in_flight"])
    check(accounted == cc["enqueued"],
          f"the {row.get('phase')} reading does not balance: {accounted} != {cc['enqueued']}")

# Every acknowledged delivery is on record, so it survives even if the
# final reading is lost to an interrupted teardown. The direction is ">=":
# a delivery that completes after the last reading adds a record without
# moving the counter that reading carries.
acked = [d for d in deliveries if d.get("outcome") == "delivered"]
check(len(acked) >= c.get("delivered", 0),
      f"the dump carries {len(acked)} acknowledgements but the counter claims {c.get('delivered')}")
check(all("event_id" not in d for d in deliveries),
      "a delivery row must not carry `event_id` — the dump is also the envelope dump")

sys.exit(0 if ok else 1)
PY
[ $? -ne 0 ] && fail "ingest run"

echo
if [ $verdict -eq 0 ]; then
  echo "P4 lifecycle E2E: PASS"
  echo "artifacts: $ROOT"
else
  echo "P4 lifecycle E2E: FAIL" >&2
fi
exit $verdict
