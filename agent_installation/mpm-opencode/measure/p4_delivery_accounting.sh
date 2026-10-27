#!/usr/bin/env bash
# measure/p4_delivery_accounting.sh — the delivery-accounting invariant,
# verified against a REAL disposable MPM event store.
#
# WHAT THIS PROVES.
#
# The unit tests use a fake transport, so they can only prove the CLIENT's
# arithmetic. This harness uses the REAL transport — a real
# `mpm call mpm_system --payload '{"action":"event_ingest"}'` — against a
# DISPOSABLE workspace, and then asks the CORE how many events it stored.
#
# The claim under test is the one the defect report turned on:
#
#   the client's `delivered` count and the core's stored count must
#   AGREE, and the client's own accounting must close.
#
# If the client reported "9 delivered" for 10 stored events, this script
# fails. That is the regression, stated as an executable check.
#
# TWO RUNS.
#
#   1. drain-all     — the observer is allowed to finish. The client must
#                      report N deliveries and the core must store N.
#   2. stop-in-flight— the observer is stopped while the LAST delivery is
#                      still in flight. This is the window the defect
#                      report captured: the OLD reading said
#                      `enqueued=10 delivered=9` with no way to say the
#                      tenth was outstanding. The corrected reading says
#                      `delivered=9 in_flight=1`, and the tenth lands and
#                      is stored — so the core still holds 10.
#
# NOTHING HERE TOUCHES THE LIVE INSTALLATION. The workspace, the database
# and the dump all live under a disposable root, and the script refuses to
# run if the pins point inside $HOME/.mpm.
#
# Usage:
#   MPM_BINARY=… measure/p4_delivery_accounting.sh [N]
#
# MPM_BINARY is required (the script does not build it; the caller
# decides which binary is under test).
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="${P4_ACCOUNTING_ROOT:-/tmp/opencode/p4-delivery-accounting}"
N="${1:-10}"

BIN="${MPM_BINARY:-}"
[ -n "$BIN" ] || { echo "MPM_BINARY is required" >&2; exit 2; }
[ -x "$BIN" ] || { echo "MPM_BINARY ($BIN) is not executable" >&2; exit 2; }

# The adapter must be built from THIS repository, or the harness would
# exercise a stale artifact.
if [ ! -f "$HERE/../dist/event-subscriber.js" ]; then
  echo "  the adapter is not built — run: (cd $HERE/.. && npm run build)" >&2
  exit 2
fi

rm -rf "$ROOT"
mkdir -p "$ROOT"

echo "P4 delivery accounting — real store"
echo "  binary    : $BIN ($("$BIN" --version 2>&1 | head -1))"
echo "  root      : $ROOT"
echo "  events    : $N"

# One run: a disposable workspace, the client harness, and the core's own
# count of what it stored.
run_case() {
  local label="$1" stop_in_flight="$2"
  local ws="$ROOT/$label/workspace"
  local db="$ws/src/db/mpm.db"
  local dump="$ROOT/$label/dump.jsonl"
  mkdir -p "$ws/src/db"

  # The pins may not point at the live installation.
  local live_ws
  live_ws="$(cd "$HOME/.mpm" 2>/dev/null && pwd -P || echo "$HOME/.mpm")"
  case "$(cd "$ws" && pwd -P)" in
    "$live_ws"|"$live_ws"/*) echo "refusing to run against the live workspace" >&2; return 2 ;;
  esac

  MPM_WORKSPACE="$ws" \
  MPM_DB_PATH="$db" \
  MPM_EVENT_DEBUG_DUMP="$dump" \
  MPM_ACCOUNTING_STOP_IN_FLIGHT="$stop_in_flight" \
    node "$HERE/p4_accounting_harness.mjs" "$N" > "$ROOT/$label/client.json" 2> "$ROOT/$label/client.err"
  if [ $? -ne 0 ]; then
    echo "  the client harness FAILED:" >&2
    cat "$ROOT/$label/client.err" >&2
    return 1
  fi

  MPM_WORKSPACE="$ws" MPM_DB_PATH="$db" \
    "$BIN" call mpm_system --payload '{"action":"event_counters"}' \
    > "$ROOT/$label/core.json" 2> "$ROOT/$label/core.err"
}

verdict=0

# ── run 1: drain-all ─────────────────────────────────────────────────────
run_case drain-all 0 || { echo "run 1 (drain-all) could not run" >&2; exit 1; }

python3 - "$ROOT/drain-all/client.json" "$ROOT/drain-all/core.json" "$ROOT/drain-all/dump.jsonl" "$N" <<'PY' || verdict=1
import json, sys
client_path, core_path, dump_path, expected = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4])
client = json.load(open(client_path))
core_raw = open(core_path).read().strip()
core = None
for line in reversed(core_raw.split("\n")):
    line = line.strip()
    if line.startswith("{") and line.endswith("}"):
        try:
            core = json.loads(line); break
        except Exception:
            pass
if core is None:
    print("FAIL: the core returned no parseable counters:", core_raw[:400], file=sys.stderr); sys.exit(1)
stored = core.get("counters", {}).get("stored_total")
snap = client["snapshot"]; c = snap["counters"]; st = snap["state"]; balance = client["balance"]
ok = True
def check(cond, msg):
    global ok
    if not cond:
        ok = False; print("FAIL:", msg, file=sys.stderr)
print("RUN 1 — drain-all")
print(f"  client: enqueued={c['enqueued']} delivered={c['delivered']} queued={st['queued']} in_flight={st['in_flight']}")
print(f"  core  : stored_total={stored} by_type={core.get('counters',{}).get('stored_by_event_type')}")
print(f"  balance: {balance}")
print()
check(c["delivered"] == expected, f"the client must report {expected} deliveries, reported {c['delivered']}")
check(stored == expected, f"the core must store {expected} events, stored {stored}")
check(c["delivered"] == stored, f"the client reported {c['delivered']} deliveries but the core stored {stored}")
check(balance["balanced"] is True, f"the client's accounting does not close: {balance}")
check(c["malformed_receipt"] == 0, "no receipt may be malformed")
check(st["in_flight"] == 0, "nothing may still be in flight after a settled teardown")
rows = [json.loads(l) for l in open(dump_path) if l.strip()]
deliveries = [r for r in rows if r.get("kind") == "observer.delivery"]
delivered_records = [r for r in deliveries if r.get("outcome") == "delivered"]
check(len(delivered_records) == expected, f"the dump must carry {expected} acknowledgements, carries {len(delivered_records)}")
check(all("event_id" not in r for r in deliveries), "a delivery row must not carry `event_id`")
envelopes = [r for r in rows if "event_id" in r]
check(len(envelopes) == expected, f"the dump must carry {expected} envelopes, carries {len(envelopes)}")
snapshots = [r for r in rows if r.get("kind") == "observer.counters"]
check(any(s.get("phase") == "teardown" for s in snapshots), "no teardown reading was written")
for s in snapshots:
    b = (s["counters"]["delivered"] + s["counters"]["unavailable"] + s["counters"]["rejected"]
         + s["counters"]["malformed_receipt"] + s["counters"]["delivery_failed"]
         + s["counters"]["dropped_overflow"] + s["counters"]["abandoned"]
         + s["state"]["queued"] + s["state"]["in_flight"])
    check(b == s["counters"]["enqueued"], f"the {s['phase']} reading does not balance: {b} != {s['counters']['enqueued']}")
print("  summary:", client["summary"])
print("PASS" if ok else "FAIL")
sys.exit(0 if ok else 1)
PY

# ── run 2: stop-in-flight (the reported case) ────────────────────────────
run_case stop-in-flight 1 || { echo "run 2 (stop-in-flight) could not run" >&2; exit 1; }

python3 - "$ROOT/stop-in-flight/client.json" "$ROOT/stop-in-flight/core.json" "$ROOT/stop-in-flight/dump.jsonl" "$N" <<'PY' || verdict=1
import json, sys
client_path, core_path, dump_path, expected = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4])
client = json.load(open(client_path))
core_raw = open(core_path).read().strip()
core = None
for line in reversed(core_raw.split("\n")):
    line = line.strip()
    if line.startswith("{") and line.endswith("}"):
        try:
            core = json.loads(line); break
        except Exception:
            pass
if core is None:
    print("FAIL: the core returned no parseable counters:", core_raw[:400], file=sys.stderr); sys.exit(1)
stored = core.get("counters", {}).get("stored_total")
snap = client["snapshot"]; c = snap["counters"]; st = snap["state"]; balance = client["balance"]
ok = True
def check(cond, msg):
    global ok
    if not cond:
        ok = False; print("FAIL:", msg, file=sys.stderr)

rows = [json.loads(l) for l in open(dump_path) if l.strip()]
snapshots = [r for r in rows if r.get("kind") == "observer.counters"]
teardown = next((s for s in snapshots if s.get("phase") == "teardown"), None)

print("RUN 2 — stop-in-flight (THE REPORTED CASE)")
print(f"  teardown reading: enqueued={teardown['counters']['enqueued']} "
      f"delivered={teardown['counters']['delivered']} "
      f"queued={teardown['state']['queued']} in_flight={teardown['state']['in_flight']}")
print(f"  core            : stored_total={stored}")
print(f"  final reading   : delivered={c['delivered']} in_flight={st['in_flight']} balance={balance}")
print()

check(teardown is not None, "no teardown reading was written")
check(teardown["counters"]["delivered"] == expected - 1,
      f"the teardown reading must show {expected-1} deliveries, showed {teardown['counters']['delivered']}")
check(teardown["state"]["in_flight"] == 1,
      "THE FIX: the teardown reading must name the outstanding delivery")
tb = (teardown["counters"]["delivered"] + teardown["counters"]["unavailable"] + teardown["counters"]["rejected"]
      + teardown["counters"]["malformed_receipt"] + teardown["counters"]["delivery_failed"]
      + teardown["counters"]["dropped_overflow"] + teardown["counters"]["abandoned"]
      + teardown["state"]["queued"] + teardown["state"]["in_flight"])
check(tb == teardown["counters"]["enqueued"],
      f"the teardown reading does not balance: {tb} != {teardown['counters']['enqueued']}")
check(stored == expected,
      f"the core must store {expected} events — the outstanding delivery was NOT lost, it landed")
check(c["delivered"] == expected,
      f"once settled, the client must report {expected} deliveries, reported {c['delivered']}")
check(st["in_flight"] == 0, "nothing may still be in flight once settled")
check(balance["balanced"] is True, f"the final reading does not close: {balance}")
print("  the OLD reading would have said: enqueued=10 delivered=9 — with nothing to")
print("  distinguish an outstanding delivery from a lost one. The corrected reading")
print("  says delivered=9 in_flight=1, and the tenth event is stored all the same.")
print("PASS" if ok else "FAIL")
sys.exit(0 if ok else 1)
PY

echo
if [ $verdict -eq 0 ]; then
  echo "P4 delivery accounting (real store): PASS"
  echo "artifacts: $ROOT"
else
  echo "P4 delivery accounting (real store): FAIL" >&2
fi
exit $verdict
