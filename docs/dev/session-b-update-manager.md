# Session B — update manager: architecture, evidence, and boundary log

> Date: 2026-10-31. Branch: `feat/update-manager` (from `origin/main` = `4f81690f`).
> Worktree: `/tmp/opencode/updater`. No live installation changes.

## What was built

A safe, convenient update system: `mpm update` for ordinary users
(latest stable release, no Go/Node/Git needed), explicit
development sources (`--to/--branch/--commit/--from`), a standalone
`mpm-update` recovery binary installed outside every release bundle,
atomic promotion with automatic rollback, runtime-vs-database rollback
separation with refusal on schema drift, stale-process reporting that
never touches agent sessions, and a release workflow that publishes
verified bundles.

## Files

| Area | Files |
|---|---|
| core logic | `internal/update/` (13 files: paths, release, lock, history, backup, schemafp, verify, source, resolve, preflight, activate, rollback, processes, health, orchestrate) |
| standalone binary | `cmd/mpm-update/` (main + tests) |
| CLI integration | `cmd/mpm/handlers_update.go`, `+_test.go`, router registration, SPEC regen |
| build | `Makefile` (+1 binary), `scripts/assemble-release-bundle.sh` |
| release | `.github/workflows/release-bundles.yml` |
| docs | `docs/UPDATE.md` |
| tests | `scripts/tests/test_release_bundles_workflow.py`, build-separation test updates |

## Design decisions (with reasons)

1. **`--to`, not `--version`, for the target release.** The router
   rewrites `--version` to the literal `version` before dispatch, so a
   value-taking `--version` flag can never work. `--to v0.2.0` is the
   consistent name; the translation of the rewrite artifact is pinned
   by test.
2. **Bare `mpm update` applies the default update.** The proposal lists
   `mpm update` as an upgrade verb; read-only views have explicit flags
   (`--status/--check/--dry-run`).
3. **`--force` comes from the global `-f/--force`** (consumed into
   `MPM_FORCE` before dispatch). No local `--force` is declared — it
   would never arrive.
4. **Go validator mirrors the shell validator** (same 5 checks, same
   forbidden list) with a parity test, instead of shelling out —
   because the updater must validate bundles when no checkout exists.
   install.sh and the shared libs are untouched.
5. **`make install` does NOT promote mpm-update.** The stable copy is
   installed by the update workflow itself at the data-root top level.
   The build-separation test pins the split.
6. **Signature verification deferred** (checksum + TLS + attestation +
   recorded provenance today). The checksum hook is the seam.

## Failures this work found in itself

1. **Unstamped dev builds refused by anti-mixture.** `GoBuilder` passed
   no ldflags; the validator correctly refused. Fix: stamp from
   `build-provenance.sh` (single source of truth).
2. **GOCACHE under the data root** when HOME points at it. Fix: builder
   isolates GOCACHE in the work dir; staging cleanup repairs permissions
   first.
3. **`--with-db-restore` without `--force` proceeded when no schema
   drift existed.** Fix: refuse up front, always.
4. **Rollback didn't refresh `.previous-release`.** Fix: the undone
   release becomes the new previous.
5. **Test env has a live scheduler.** Service probes hit it. Fix:
   injectable `ServiceControl`; tests use `QuietServices`.

## Pre-existing observations (out of scope, not changed)

- `mpm add` with no embedding provider stores the literal string
  `'null'` in the embedding BLOB column. Retrieval/display surfaces
  (`ls`/`show`/`recall`) then exclude the row while SQL sees it.
  Identical in old and new binaries; storage layer intact. Session A
  territory (retrieval/display), not the updater.
- `mpm audit` is unreachable (dispatch switch but no command-table
  entry). Pre-existing.
- `mpm save-skill --version` never reaches its handler (global
  rewrite). Pre-existing; the reason `--to` was chosen.

## Coordination

- No changes to `install.sh`, `runtime_bundle.sh`,
  `binary_transaction.sh`, `schema_preflight.py`, or the systemd
  units. Overlap surface with Session A: `Makefile` (one additive
  binary line + echo), `docs/SPEC.md` (§8 regen only), `cmd/mpm`
  (one new command + help cases), `scripts/tests`
  (build-separation expectations). Rebase onto integrated main and
  re-run combined tests before declaring ready.
- Live `~/.mpm` untouched (verified: no fixture rows, no new tables,
  scheduler never restarted, no config/credential writes). All tests
  hermetic (`t.TempDir`, isolated workspaces, local HTTP servers,
  fake builders/binaries).
