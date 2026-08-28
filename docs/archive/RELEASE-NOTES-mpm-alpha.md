# MPM Alpha — 2026-08-08

First public alpha of MPM (Memory-Persona-Mode Manager). Ships a hardened
SQLite data plane, an `mpm-agent` companion, and a one-shot UX gate
(`scripts/stranger-test.sh`) that walks a fresh install through the README
quickstart.

This release resolves the findings from the 2026-08-08 alpha audit and
the three Pass 1 README audit findings. Every claim here is verified by
the repository's green test suite (14 packages, 0 FAIL) and the 12-step
stranger test in an isolated `$HOME` / `$MPM_WORKSPACE`.

## What's in this release

### Fixed

- **Semantic search type mismatch (CRITICAL).** `mpm recall --semantic`
  crashed on first run with `sql: Scan error converting driver.Value
  type float64 to int`. The `weight` column is INTEGER-declared but
  SQLite stores fractional values via type affinity, so `Scan` had to
  use `float64` destinations. Fixed by switching `ftsEntry.Weight`,
  `HybridResult.Weight`, and `hybridEntry.Weight` to `float64`. Without
  this fix, the README quickstart's first example ran a hard runtime
  crash on the very first `mpm recall --semantic` invocation.

- **Silent `--semantic` fallback (HIGH).** When no embedding provider
  was configured, `HybridSearch` returned a degraded FTS5-only result
  tagged `[fts5]` without telling the user. Fixed by checking
  `DefaultEmbeddingConfig().ProviderName == "null"` and failing clean
  with a documented error pointing to `mpm config profile set`.

- **LIKE fallback Scan mismatch (HIGH).** When FTS5 failed to init
  (hermetic install, no compile flag, fresh workspace), the LIKE
  fallback `SELECT` in `keywordSearchWithTime` returned 9 columns but
  the `Scan` destination in `handleRecall` (and the user-facing
  `mpm recall`) had 10 args. The first `mpm recall` on a fresh install
  crashed with `sql: expected 9 destination arguments in Scan, not 10`.
  Fixed by adding `metadata` to the LIKE SELECT to match the FTS5
  column order. The corresponding test Scan destinations were also
  updated to use `sql.NullString` (metadata) and `sql.NullTime`
  (created_at / last_accessed_at) — matching the actual driver
  return types and the production nullable handling.

- **README quickstart bugs (3 HIGH).** Three corrections to the
  README quickstart section that surfaced wrong commands or referenced
  subcommands that didn't exist:
  1. `mpm recall alpha` swapped to the right CLI invocation.
  2. Removed `mpm ops stance promote` (subcommand does not exist).
  3. Removed `mpm kb topic link` (subcommand does not exist).

- **Ingest roadmap leakage.** `mpm ingest` printed internal roadmap
  text to the user-facing output. Stripped.

### Changed

- **Permanent release gate.** `scripts/stranger-test.sh` is now part
  of the repository. It boots MPM against an isolated `$HOME` and
  `$MPM_WORKSPACE` and walks the README §5.3 quickstart end-to-end
  in 12 steps. Exits non-zero on the first command that fails to
  produce the documented behavior. Intended to run as a pre-release
  gate and on every PR.

### Tests

- **Hermetic fixtures for three previously-flaky tests.**
  - `TestRenderRoute` no longer reads the live workspace. It builds
    a `t.TempDir()` with synthetic mode/persona files matched to the
    test prompts, so the assertions don't drift when live content
    changes.
  - `TestHandleRoute_ApplyFlag` seeds active.json with `Persona="auto"`
    / `Modes=["auto"]` (the only path that honors `--apply`). It
    previously seeded with `"default"` / `"standard"`, which landed
    in the manual branch and silently skipped the assertion.
  - `TestFetchGitLog_Integration` uses a `t.TempDir()` git fixture
    with a synthetic `v0.0.1` tag and 60 post-tag commits. The
    original test hardcoded `v1.0.0-hardened`, which no longer exists
    after the git history rewrite.

### Other

- Removed two diagnostic binaries (`check_db.go`, `check_fts.go`) that
  were committed for the 2026-06-28 external ChatGPT review and were
  no longer needed.

## Verified by

- **Stranger test** (12 steps, hermetic install): all 12 PASS.
  ```
  bash scripts/stranger-test.sh
  ✅ stranger test passed (12 steps, scratch cleaned up)
  ```
- **Go test suite** (`make test`): 14 packages, 0 FAIL lines.
  ```
  ok    github.com/flowbyte-com/mpm/cmd/gen-cli
  ok    github.com/flowbyte-com/mpm/cmd/mpm            1.213s
  ok    github.com/flowbyte-com/mpm/cmd/mpm-mcp
  ok    github.com/flowbyte-com/mpm-core
  ok    github.com/flowbyte-com/mpm-core/capability
  ok    github.com/flowbyte-com/mpm-core/config
  ok    github.com/flowbyte-com/mpm-core/logging
  ok    github.com/flowbyte-com/mpm-core/mpmcli
  ok    github.com/flowbyte-com/mpm-core/orchestration
  ok    github.com/flowbyte-com/mpm-core/renderers
  ok    github.com/flowbyte-com/mpm-core/seed
  ok    github.com/flowbyte-com/mpm-core/synth
  ok    github.com/flowbyte-com/mpm-core/tools
  ok    github.com/flowbyte-com/mpm-core/usererror
  ok    github.com/flowbyte-com/mpm/internal/scheduler 5.704s
  ```
- **Pre-commit hook** (8-gate `mpm-lint`): all 8 gates PASS on every
  commit in this release.

## Out of scope (post-alpha)

- Plan / WISHLIST items
- mpm-agent Telegram / MCP surface
- Synthesis engine hardening (it has its own gate, not in this release)
- Real embedding provider coverage (no Ollama probe in CI yet)

## How to install

```bash
git clone https://github.com/flowbyte-com/mpm ~/mpm
cd ~/mpm
make install BIN=mpm
bash scripts/stranger-test.sh   # verify your install
```

Or run the quickstart directly:

```bash
mpm add "your first memory"
mpm recall "first"
mpm wake
```

See the README §5.3 quickstart for the full 12-step walk.

## License

AGPL-3.0. See [`LICENSE`](LICENSE) for the full text.
