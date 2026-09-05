# Documentation-drift recurrence prevention

> Audit follow-up to commit `674b975` ("fix(docs): reconcile documentation
> drift across 11 user-facing files") and its correction `76b44f5` (lesson-ID
> restoration + embedding-OPTIONAL hint). Captures the four drift classes
> observed in the 2026-09-04 audit and proposes the smallest set of CI
> gates that would have caught each one.

## Drift classes observed

| # | Class | Concrete example (before) | Why it drifted |
|---|---|---|---|
| 1 | **Host-support list** | `docs/INSTALL.md:263` said "two supported hosts as of 2026-07-18" while the adapter tree had grown to five | The list was hand-edited in narrative prose; the adapter tree is added to via the install scripts |
| 2 | **Tool-count arithmetic** | `agent_installation/INSTALL.md:614` said "16-tool subset (13 Domain + 3 Standalones)"; Pi actually registers 17 tools (14 Domain + 3 Standalones) | Adapter `index.ts` was updated; the prose never was |
| 3 | **Lesson ID** | README cited `24be03ec71a5981f` (substrate-resident; the Lazy-Start Architecture lesson); `scripts/install.sh` cites `071911bc` (autostart `.desktop` mechanism). The audit silent-swapped `24be03ec` → `071911bc` in user-facing docs, which was wrong — the two IDs point at two mechanisms whose relationship is **not articulated in the cited material** (no substrate memory reconciles wake-layer-as-design with autostart-layer-as-fix). Cited both to surface the unresolved relationship rather than to assert one is the canonical replacement | Two IDs were conflated because both touch the same problem class (ecryptfs daemon auto-start); neither is canonical because each is canonical in its own context. The honest framing is "two mechanisms, relationship unresolved" |
| 4 | **Personal paths** | `/home/v/.mpm/...` and `/home/v/workspace/projects/mpm/...` leaked into 6+ user-facing files | Template files were committed from a single host; never generalised |

## Proposed gates

The smallest set of mechanical greps that catches each class, anchored
to the existing `scripts/pre-commit` hook and `mpm-lint --gate`
infrastructure. None requires new tooling.

### Gate 1 — Adapter-tree size match (catches class #1, #2)

Run as part of `scripts/pre-commit`. Reads the adapter source files
and asserts the prose in the docs matches.

```bash
#! /usr/bin/env bash
# scripts/doc-gate-tool-arithmetic.sh
set -euo pipefail

PI_DOMAINS=$(grep -cE "name:\s*['\"]mpm_" \
  agent_installation/pi-mpm/index.ts)
PI_STANDALONES=$(grep -cE "name:\s*['\"](log_to_changelog|request_review|mpm_retrieval_diagnose)" \
  agent_installation/pi-mpm/index.ts)
PI_TOTAL=$((PI_DOMAINS + PI_STANDALONES))

# ...same for opencode-mpm/src/index.ts...

for f in README.md docs/INSTALL.md agent_installation/INSTALL.md \
         agent_installation/pi-mpm/README.md agent_installation/pi-mpm/templates/AGENTS.md.snippet \
         agent_installation/opencode-mpm/README.md agent_installation/opencode-mpm/templates/AGENTS.md.snippet; do
  if grep -nE "(16-tool subset|13 Domain Tools|13 domain Tools)" "$f" >/dev/null; then
    echo "doc-gate: $f still cites stale 16-tool arithmetic" >&2
    exit 1
  fi
done
```

### Gate 2 — Host-support inventory (catches class #1)

```bash
# scripts/doc-gate-host-support.sh
ADAPTERS=$(ls -1 agent_installation/ | grep -E '-(mpm|memory)$|^pi-mpm$' | wc -l)
# Currently 5: claude-code-mpm, opencode-mpm, pi-mpm, hermes-mpm, openclaw-mpm-memory

for f in README.md docs/INSTALL.md agent_installation/INSTALL.md; do
  if grep -nE "(two supported hosts|2 supported hosts|two host adapters)" "$f" >/dev/null; then
    echo "doc-gate: $f cites stale host count" >&2
    exit 1
  fi
  # Optional positive assertion: any line saying "N hosts/adapters/integrations"
  # should resolve to the directory count.
done
```

### Gate 3 — Personal-path scrub (catches class #4)

```bash
# scripts/doc-gate-no-personal-paths.sh
EXCLUDE='(docs/archive/|VALIDATION-|test_|/home/v/workspace/projects/mpm/src/db/mpm.db)'
VIOLATIONS=$(grep -rn '/home/v/' \
  README.md docs/INSTALL.md agent_installation/INSTALL.md \
  agent_installation/*/README.md agent_installation/*/SKILL.md \
  agent_installation/*/.mcp.json agent_installation/*/.mcp.json.template \
  agent_installation/*/templates/ \
  2>/dev/null | grep -vE "$EXCLUDE" || true)
if [[ -n "$VIOLATIONS" ]]; then
  echo "doc-gate: personal paths found:" >&2
  echo "$VIOLATIONS" >&2
  exit 1
fi
```

The `EXCLUDE` allowlist captures the two legitimate uses:
archival evidence under `docs/archive/` and the canonical DB path
that appears in validation-report tables.

### Gate 4 — Citation-integrity check (catches class #3)

**REVISED 2026-09-05 — the original premise of this gate was wrong.**

This gate originally enforced a **dual-presence** rule: any doc touching the
ecryptfs daemon-auto-start surface had to cite both `24be03ec71a5981f` and
`071911bc`, on the theory that each documented a different architecture layer.

That theory was disproved. `071911bc` is **not a lesson ID and never was**:
it resolves to no row in any substrate table (checked across every table in
`src/db/mpm.db`) and to no git object in this repository (`git cat-file -t`
fatals; `--disambiguate` returns nothing). Its origin is unknown. The commit
that actually introduced the post-decrypt autostart mechanism is `14ac32b`.
Shipping the dual-presence gate would have trained CI and contributors to
keep re-adding a citation to something that does not exist — precisely the
drift this gate exists to prevent.

`24be03ec71a5981f` is at least ID-shaped (16 hex chars) but also does not
resolve as a row. Its content survives only inside memory
`463fb2c8014fc1f1` (collection `notes`, weight 94), which IS retrievable.

So the drift signature is no longer "missing the other ID." It is
**"citing a dead reference with no pointer to the live memory."**

Implemented at `scripts/doc-gate-lesson-id.sh`. Three checks over
README.md, docs/INSTALL.md, agent_installation/INSTALL.md,
scripts/install.sh, and contrib/systemd/mpm-scheduler.service.user:

1. **No lesson label.** Fail on `lesson 071911bc` (any case, optional
   backticks) anywhere. That string should never reappear.
2. **Residue scoping.** Any mention of `071911bc` outside the two sanctioned
   passages — the README.md and docs/INSTALL.md paragraphs documenting it as
   an unresolvable string, anchored by the phrase `carries the string` — is
   flagged.
3. **Live pointer required.** A `24be03ec71a5981f` citation must have
   `463fb2c8014fc1f1` within 12 lines (approximating "same paragraph"
   without parsing Markdown structure).

Verified against four fixtures: clean tree passes; a reintroduced
`lesson 071911bc` fails checks 1+2; a stray `071911bc` in a non-residue line
of a sanctioned file fails check 2; a bare `24be03ec71a5981f` fails check 3
and passes once the live pointer is added.

## Deployment plan

1. Land the four `doc-gate-*.sh` scripts under `scripts/`.
2. Wire them into `scripts/pre-commit` (after the existing router
   linter pass).
3. Add a CI workflow step (`.github/workflows/ci.yml` or equivalent)
   that runs the same gates on push.
4. Update `mpm-lint --gate` documentation to note that doc gates
   are an adjacent-but-separate enforcement surface (lint gates code;
   doc gates prose).

### Gate 5 — Threshold-table positive-presence checks (catches class #5)

**ADDED 2026-09-05** — extends the four negative-style gates above with a
matched pair of **positive-presence** gates. Where Gates 1-4 fail-stop
when a *known-bad phrase* is found, Gate 5 fails when a *known-good row*
is missing — a fundamentally different failure mode that the existing
gates couldn't cover.

The reason this is a pair, not a single gate: README §6.4 (MCP
Integration) and §8 (CLI Reference) are deliberately distinct surfaces
in the README's own narrative, with different audiences (agents see §6.4
output; operators see §8 output) and different constant inventories.
Conflating them in tooling would blur a distinction the docs
intentionally keep separate, so the two gates are kept as separate
shell scripts even though they share structure.

**Gate 5a — `scripts/doc-gate-threshold-table.sh` (MCP-output caps, §6.4).**

Catches the recurrence pattern that motivated the gate: three
independent passes each flagged the `mpm_blob_search` row as missing
from the §6.4 threshold table without re-deriving that the original
F-S-3 sweep had explicitly classified those limits as a scan window,
not a response-size cap (correct classification, but the reasoning
wasn't visible to readers of the table alone). A positive-presence
gate catches the omission at the source, not at the next audit.

Scope: production source `internal/core`, `internal/core/tools`,
`cmd/mpm`, `cmd/mpm-mcp`; constant-shaped `MaxBytes`/`maxBytes`/
`Threshold`/`Bound`/`MaxLen`/`maxLen`/`serverMax*`/`defaultMax*`
declarations that bound MCP-output size or count. Each must have a
peer row in the §6.4 table citing the file:line. CLI input/output
limits (`memoryMaxFileBytes`, `maxAddBytes`, etc.) are deliberately
excluded — they belong in §8, not §6.4.

**Gate 5b — `scripts/doc-gate-cli-limits.sh` (CLI-side limits, §8).**

Sibling of Gate 5a, covering the §8 CLI Reference surface. Same
shape: every named CLI-limit constant in `cmd/mpm/` must have a peer
row in the §8 CLI-side input/output limits table. Internal substrate
thresholds (`ProvenanceThreshold`, `HardConfidenceInvalidationThreshold`,
etc.) and inline `fs.Int("limit", N)` defaults across 8+ files are
deliberately excluded — internal thresholds never reach the CLI,
and the per-command `--limit` defaults are already represented in the
auto-generated Command Catalogue block (`<!-- cli:begin -->`) plus the
per-subcommand description prose; gating each would duplicate the
Catalogue's role.

Verified by removing a row from the §8 table in a sandboxed copy and
confirming the gate fails with the right diagnostic naming the
constant. Restored after.

**Why a matched pair, not one merged gate with a scope parameter.**

Two scripts, two `REQUIRED_*` lists, two anchor-region ranges.
Sharing structure would invite a future editor to add a constant
that crosses surfaces (e.g. one whose default differs between the
MCP and CLI paths) and silently undergate it. Separate scripts make
the surface boundary explicit at the tooling level, matching the
docs' own narrative boundary.

## What this deliberately does NOT do

- **Parse prose for arithmetic.** The grep is a string-equality check
  on the specific stale phrases (`16-tool subset`, `13 Domain Tools`).
  Trying to do real arithmetic from prose is a much larger problem.
- **Auto-fix drift.** The gate is fail-stop with a one-line hint;
  humans fix the docs. Auto-edit would invite silent mistakes on
  semantically-loaded prose.
- **Cover `docs/archive/`.** Archival evidence is allowed to retain
  historical paths/IDs. The audit's hard boundary was "do not
  modify archival validation files merely to reduce drift-scan
  counts" — the gate respects that.
- **Cover changelogs or release notes.** Those are append-only
  history; drift there is content, not a defect.

## Estimated CI cost

Each gate is one `grep` over ≤ 20 files. Combined wall time:
~200ms. Trivial.

## Open questions

1. Should the gate fail the commit (current proposal) or warn and
   require a `--no-verify` ack? Failing is simpler; warn-and-ack
   is friendlier. Recommend fail (matches `mpm-lint --gate`).
2. Should the personal-path scrub include `agent_installation/<host>/
   test_*.py` and `agent_installation/<host>/VALIDATION-*.md`?
   Recommendation: yes, both are operator-instruction files; the
   gate's `EXCLUDE` allowlist only covers `docs/archive/`.
3. Does the host-support gate need a positive assertion (prose must
   say "five"), or only a negative one (prose must not say "two")?
   Recommendation: negative-only. Positive assertions invite the
   same drift class in reverse (count goes from 5 to 6, prose still
   says "five").
