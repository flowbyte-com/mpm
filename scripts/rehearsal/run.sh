#!/usr/bin/env bash
# scripts/rehearsal/run.sh — run the whole rehearsal, in order, and report
# the aggregate.
#
# The suites are ordered by dependency:
#
#   setup.sh      builds the Git-free source export and the bundle
#   00_simulate   builds an isolated simulation of a checkout-style
#                 installation under a disposable HOME
#   10_obligations  the migration obligations — what must be true
#   20_negative     the negative suite — what must NOT happen, plus the
#                   harness's own integrity guards
#   30_schemas      the upgrade path against representative schemas
#
# The aggregate is reported as one number, because "the rehearsal passed"
# is only meaningful if every suite passed. A suite that fails to RUN is
# reported as a failure, not as a skip: a rehearsal that could not
# execute has proved nothing.
#
# Usage: run.sh
# Environment:
#   REH_REV       the revision to rehearse against (default: 4111c46)
#   REH_WORK      where the work products go (default: ./scripts/rehearsal/.work)
#   REH_SKIP_SETUP=1  reuse an existing bundle

set -uo pipefail

REH_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
. "$REH_DIR/lib.sh"

REPORT="$REH_WORK/rehearsal-report.txt"
: > "$REPORT"

TOTAL_PASS=0
TOTAL_FAIL=0
SUITES_FAILED=0

run_suite() { # run_suite <name> <script...>
  local name="$1"; shift
  local log="$REH_WORK/suite-$name.log"
  # A suite that produces no summary is treated as a failure UNLESS it is
  # declared to be a builder. The simulation suite builds the isolated
  # installation the others rehearse against; it asserts nothing, and it
  # has no summary to produce.
  local builder=0
  if [ "${1:-}" = "--builder" ]; then builder=1; shift; fi

  printf '\n\033[1m════════ %s ════════\033[0m\n' "$name"
  if ! bash "$@" > "$log" 2>&1; then
    printf '\033[31m  the suite did not exit 0\033[0m\n'
  fi
  # The suite's own output is echoed, so a failure is visible in the
  # transcript and not only in the log.
  sed 's/^/  /' "$log"

  # A builder has no assertions to summarise. Its line is recorded as
  # such, and it is not counted towards the aggregate.
  if [ "$builder" = "1" ]; then
    printf 'suite=%s passed=0 failed=0 (builder)\n' "$name" >> "$REPORT"
    return 0
  fi

  # The counts come from the suite's own summary block.
  local passed failed
  passed="$(grep -Eo '^\s*passed: [0-9]+' "$log" | tail -1 | grep -Eo '[0-9]+' || echo 0)"
  failed="$(grep -Eo '^\s*failed: [0-9]+' "$log" | tail -1 | grep -Eo '[0-9]+' || echo 0)"
  TOTAL_PASS=$((TOTAL_PASS + passed))
  TOTAL_FAIL=$((TOTAL_FAIL + failed))

  printf 'suite=%s passed=%s failed=%s\n' "$name" "$passed" "$failed" >> "$REPORT"

  if [ "$failed" != "0" ]; then
    SUITES_FAILED=$((SUITES_FAILED + 1))
    return 1
  fi
  # A suite that produced no summary at all did not run to completion.
  if ! grep -qE '^\s*passed: [0-9]+' "$log"; then
    printf '\033[31m  the suite produced no summary — it did not complete\033[0m\n'
    printf 'suite=%s passed=0 failed=1 (no summary)\n' "$name" >> "$REPORT"
    SUITES_FAILED=$((SUITES_FAILED + 1))
    TOTAL_FAIL=$((TOTAL_FAIL + 1))
    return 1
  fi
  return 0
}

printf '\033[1m════════ the runtime-migration rehearsal ════════\033[0m\n'
printf '  revision : %s\n' "$REH_REV"
printf '  work dir : %s\n' "$REH_WORK"

# ── 1. the build ──────────────────────────────────────────────────────────
if [ "${REH_SKIP_SETUP:-0}" = "1" ]; then
  printf '\n\033[1m════════ setup (skipped) ════════\033[0m\n'
else
  printf '\n\033[1m════════ setup ════════\033[0m\n'
  # REH_ARCHIVE_COMMIT is the OPERATOR's request to rehearse a committed
  # revision. It is deliberately NOT REH_COMMIT: that name is used by the
  # harness's own record of what was staged, and reading it here would
  # make every run an archive run -- which would rehearse the COMMITTED
  # tree and silently ignore the changes under review.
  if ! bash "$REH_DIR/setup.sh" "$REH_REV" "${REH_ARCHIVE_COMMIT:-}"; then
    printf '\033[31m  the setup failed — the rehearsal cannot run\033[0m\n'
    exit 1
  fi
fi

# ── 2. the suites ─────────────────────────────────────────────────────────
run_suite simulate --builder "$REH_DIR/00_simulate.sh"
run_suite obligations "$REH_DIR/10_obligations.sh"
run_suite negative "$REH_DIR/20_negative.sh"
run_suite schemas "$REH_DIR/30_schemas.sh"

# ── 3. the aggregate ──────────────────────────────────────────────────────
printf '\n\033[1m════════ the aggregate ════════\033[0m\n'
printf '  assertions passed : %d\n' "$TOTAL_PASS"
printf '  assertions failed : %d\n' "$TOTAL_FAIL"
printf '  suites failed     : %d\n' "$SUITES_FAILED"
printf '  report            : %s\n' "$REPORT"

if [ "$TOTAL_FAIL" != "0" ] || [ "$SUITES_FAILED" != "0" ]; then
  printf '\033[31mREHEARSAL FAILED\033[0m\n'
  exit 1
fi
printf '\033[32mREHEARSAL PASSED\033[0m — %d assertions, 0 failures\n' "$TOTAL_PASS"
