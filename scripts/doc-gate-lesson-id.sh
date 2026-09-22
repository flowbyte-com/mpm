#!/usr/bin/env bash
# doc-gate-lesson-id.sh — citation integrity for the ecryptfs autostart docs.
#
# WHY THIS GATE EXISTS, AND WHY ITS LOGIC CHANGED
# -----------------------------------------------
# The first version of this gate enforced a DUAL-PRESENCE rule: any doc
# touching the ecryptfs daemon-auto-start surface had to cite both
# `24be03ec71a5981f` and `071911bc`, on the theory that each documented a
# different architecture layer.
#
# That premise was wrong. `071911bc` is not a lesson ID and never was: it
# resolves to no row in any substrate table and to no git object in this
# repository, and its origin is unknown. The commit that actually introduced
# the post-decrypt autostart mechanism is `14ac32b`. The old gate would have
# trained contributors (and CI) to keep re-adding a citation to something
# that does not exist — the exact drift this gate is supposed to stop.
#
# `24be03ec71a5981f` is at least ID-shaped, but it also does not resolve as a
# row. Its content survives only inside memory `463fb2c8014fc1f1`, which IS
# retrievable. So the modern drift signature is not "missing the other ID" —
# it is "citing a dead reference with no pointer to the live memory."
#
# Three checks below, in that order.

set -euo pipefail

# Files that touch the ecryptfs daemon-auto-start problem surface.
PROBLEM_SURFACE_FILES=(
  docs/SPEC.md
  docs/INSTALL.md
  agent_installation/INSTALL.md
  install.sh
  contrib/systemd/mpm-scheduler.service.user
)

# The only sanctioned mentions of `071911bc` are the passages in docs/SPEC.md and
# docs/INSTALL.md that document it AS an unresolvable string. Those passages
# are identified by this anchor phrase; any other mention is drift.
RESIDUE_ANCHOR='carries the string'
RESIDUE_FILES=(docs/SPEC.md docs/INSTALL.md)

# How far from a `24be03ec71a5981f` citation we look for the live pointer.
# Approximates "same paragraph/section" without parsing Markdown structure.
PROXIMITY_LINES=12

fail=0

is_residue_file() {
  local needle=$1 f
  for f in "${RESIDUE_FILES[@]}"; do
    [[ "$f" == "$needle" ]] && return 0
  done
  return 1
}

for f in "${PROBLEM_SURFACE_FILES[@]}"; do
  [[ -f "$f" ]] || continue

  # --- Check 1: the string must never again be called a lesson. ------------
  # `071911bc` is not a lesson ID (see header). Matches "lesson 071911bc" with
  # optional backticks and any capitalisation.
  if grep -niE 'lesson[[:space:]]+`?071911bc`?' "$f" >&2; then
    echo "doc-gate: $f calls \`071911bc\` a lesson ID." >&2
    echo "  It is not one: no substrate row, no git object, origin unknown." >&2
    echo "  The autostart mechanism was introduced by commit 14ac32b — cite that." >&2
    fail=1
  fi

  # --- Check 2: no unsanctioned mentions of the string at all. -------------
  # Sanctioned = the docs/SPEC.md / docs/INSTALL.md residue paragraphs that
  # explicitly document `071911bc` as unresolvable. Everything else is drift.
  while IFS=: read -r lineno text; do
    [[ -n "$lineno" ]] || continue
    if is_residue_file "$f" && [[ "$text" == *"$RESIDUE_ANCHOR"* ]]; then
      continue
    fi
    echo "doc-gate: $f:$lineno mentions \`071911bc\` outside the sanctioned residue." >&2
    echo "  Only the docs/SPEC.md / docs/INSTALL.md paragraphs documenting it as an" >&2
    echo "  unresolvable string may reference it. Cite commit 14ac32b instead." >&2
    fail=1
  done < <(grep -nE '\b071911bc\b' "$f" || true)

  # --- Check 3: a dead reference needs a live pointer beside it. -----------
  # `24be03ec71a5981f` does not resolve as a row; memory `463fb2c8014fc1f1`
  # carries its content forward and IS retrievable. Citing the former without
  # the latter nearby sends readers to a query that returns nothing.
  while IFS=: read -r lineno _; do
    [[ -n "$lineno" ]] || continue
    lo=$(( lineno - PROXIMITY_LINES )); (( lo < 1 )) && lo=1
    hi=$(( lineno + PROXIMITY_LINES ))
    if ! sed -n "${lo},${hi}p" "$f" | grep -qE '\b463fb2c8014fc1f1\b'; then
      echo "doc-gate: $f:$lineno cites \`24be03ec71a5981f\` with no live pointer." >&2
      echo "  That reference does not resolve as a substrate row. Cite memory" >&2
      echo "  463fb2c8014fc1f1 (which carries its content forward) in the same" >&2
      echo "  paragraph, within ${PROXIMITY_LINES} lines." >&2
      fail=1
    fi
  done < <(grep -nE '\b24be03ec71a5981f\b' "$f" || true)
done

exit "$fail"
