# Tooling Gate Forensics + Correction — 2026-09-06

**Date:** 2026-09-06
**Branch:** `audit/c18-c19-final-p3-closure`
**Mode:** Read-only forensics followed by a documentation correction. No production code, tests, or schemas were modified.
**Trigger:** The mirrored-pair remediation's final validation report (commit `1ff96c8 docs(tools): document deliberate mirrored-pair asymmetries`) marked `mpm-lint --gate` and `python3 scripts/render_managed_blocks.py --check` as `N/A`. Both gates actually exist and pass today; the N/A markings were false negatives from running the commands at the wrong path. This file records the corrected state and the evidence that supports it.

---

## TL;DR (the correction in one paragraph)

| Tool | Previous report marking | Correct marking | Why the previous report was wrong |
|---|---|---|---|
| `mpm-lint --gate` | `N/A — no mpm-lint --gate in this repo` | **PASS** — runs as `go run ./cmd/mpm-lint --gate` from the project root, source at `cmd/mpm-lint/main.go`, exits 0 today | The tool exists at `cmd/mpm-lint/` (wired into `scripts/pre-commit:86` and `.github/workflows/build-test.yml:74-91`). It is not on `$PATH`, so the bare command `mpm-lint --gate` fails — but `go run ./cmd/mpm-lint --gate` works. The previous report did not search the repo for the tool before declaring it absent. |
| `python3 scripts/render_managed_blocks.py --check` | `N/A — no render_managed_blocks.py in this repo` | **PASS** — runs as `python3 agent_installation/scripts/render_managed_blocks.py --check`, source at `agent_installation/scripts/render_managed_blocks.py`, exits 0 today with "4 adapter(s) in byte-for-byte parity" | The renderer exists at `agent_installation/scripts/render_managed_blocks.py`. The path `scripts/render_managed_blocks.py` has **never** existed in git history — the report's command referenced a path that doesn't exist. The Makefile invokes it correctly from inside `agent_installation/` (`cd $(AGENT_INSTALL_DIR) && python3 scripts/render_managed_blocks.py --check`); the same command from the project root needs the `agent_installation/` prefix. |

**Release impact: none.** Both tools are alive, both work, both are gated. The N/A markings in the previous validation report were a documentation drift, not a missing-tool problem.

---

## 1. `mpm-lint --gate` — current state

### Source

- Path: `cmd/mpm-lint/main.go` (206 lines, Go 1.26.6 build tag `fts5`)
- Replaces 8 standalone `cmd/audit-*` binaries via commit `00041b9 build(hooks): wire mpm-lint and remove legacy audit binaries` (2026-08-07)
- 9 classification rules in one AST walk: `scans, closes, tx, ctx, go, mutex, sql, fd, imports`. Fatal classes are zero-tolerance; review classes ratchet against measured baselines so any NEW site fails the commit while the pre-existing backlog is grandfathered.

### Invocation

| Surface | Command | Status |
|---|---|---|
| Project root | `go run ./cmd/mpm-lint --gate` | **PASS** today (exit 0) |
| Pre-built binary | `./bin/mpm-lint --gate` (build via `make build` produces `bin/mpm-lint`) | **PASS** today (exit 0) |
| Pre-commit hook | `scripts/pre-commit:86` invokes `go run ./cmd/mpm-lint --gate` whenever a `.go` file is staged | Runs on every commit; verified during the recent remediation arc (`[pre-commit] mpm-lint gate OK` printed at commit time) |
| CI | `.github/workflows/build-test.yml:74-91` builds the binary, emits `::error` workflow annotations from `--json` output, then runs `--gate` to fail the `gate` job on threshold violation | Runs and gates every push to `main` and every PR |

### Evidence from current tree

```
$ go run ./cmd/mpm-lint --gate
[mpm-lint --gate] fd thresholds:
  fd-no-close            actual=0  threshold=0  ok
  fd-remove-only         actual=0  threshold=0  ok
  fd-unknown             actual=0  threshold=0  ok
  fd-explicit-close      actual=9  threshold=9  ok
[mpm-lint --gate] imports thresholds:
  import-forbidden       actual=0  threshold=0  ok
[mpm-lint --gate] PASS
$ echo $?
0
```

---

## 2. `render_managed_blocks.py --check` — current state

### Source

- Path: `agent_installation/scripts/render_managed_blocks.py` (633 lines, Python 3.12)
- Three commits, all under `agent_installation/scripts/` — the script has **never** lived at `scripts/render_managed_blocks.py`. Git history for that path is empty (`git log --all -- scripts/render_managed_blocks.py` returns no commits).

### Invocation

| Surface | Command | Status |
|---|---|---|
| Project root | `python3 agent_installation/scripts/render_managed_blocks.py --check` | **PASS** today (exit 0, "4 adapter(s) in byte-for-byte parity") |
| Inside `agent_installation/` (Makefile convention) | `cd agent_installation && python3 scripts/render_managed_blocks.py --check` | **PASS** today (same output) |
| Makefile target | `make refresh-installed` regenerates and runs `--check` as the post-refresh gate | Runs locally; not run in CI (covered in §3 below) |
| Parity test | `cd agent_installation && python3 -m unittest discover tests` | **PASS** (35 cases in `agent_installation/tests/test_render_managed_blocks.py` — covers canonical extraction, per-host prefix substitution, copy/paste parity, byte-for-byte match, OpenClaw exclusion, version-marker uniqueness) |

### Evidence from current tree

```
$ python3 agent_installation/scripts/render_managed_blocks.py --check
[render_managed_blocks] 4 adapter(s) in byte-for-byte parity with canonical source; 4 copy/paste example(s) in parity; instructions primer in parity.
$ echo $?
0

$ cd agent_installation && python3 -m unittest discover tests
...
Ran 35 tests in 0.005s
OK
```

### What `--check` actually verifies

The renderer reads `agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md`, extracts the LAST occurrence of `<!-- BEGIN MPM MANAGED BLOCK --> ... <!-- END MPM MANAGED BLOCK -->` (host-neutral canonical), renders it for each adapter (Claude `mpm__`, OpenCode/Pi bare, Hermes `mcp__mpm__`) via word-boundary substitution on the leading `mpm_` of each canonical tool name, and compares the rendered output to the checked-in snippets at:
- `agent_installation/claude-code-mpm/templates/CLAUDE.md.snippet`
- `agent_installation/opencode-mpm/templates/AGENTS.md.snippet`
- `agent_installation/pi-mpm/templates/AGENTS.md.snippet`
- `agent_installation/hermes-mpm/templates/hermes.md.snippet`

If any of these four drifts from the canonical source, `--check` exits non-zero.

---

## 3. Why the previous validation report got it wrong

The previous report's relevant line was:

```text
| mpm-lint --gate                                  | N/A | ...
| python3 scripts/render_managed_blocks.py --check | N/A | ...
```

The mpm-lint command failed because the bare command `mpm-lint` is not on `$PATH` — the binary lives at `./bin/mpm-lint` and the source at `./cmd/mpm-lint/`. The renderer command failed because `scripts/render_managed_blocks.py` does not exist — the script lives at `agent_installation/scripts/render_managed_blocks.py`.

Both failures are **"tool absent from this environment, present in repo."** The correct investigation step would have been `find . -name 'render_managed*' -not -path './.git/*'` and `find . -name 'mpm-lint*' -not -path './.git/*'` — those searches would have surfaced the correct paths in seconds and the report would have read `PASS` for both.

This is the same failure mode flagged in memory `feedback-verify-code-claims-before-stating-them`: claiming a tool state without verifying the tool's actual location.

---

## 4. Drift worth surfacing (not blocking)

1. **Managed-block parity is not in CI today.** The 35-case unittest lives at `agent_installation/tests/test_render_managed_blocks.py` and is locally executable. CI's `build-test.yml` does not invoke it. Parity drift between canonical source and adapter snippets could land without CI catching it. **Status:** a pre-existing gap (not a regression introduced by the recent remediation). Item 2 of the cleanup sequence adds a CI step to close it.

2. **The CI `--check` invocation is local-only today.** `python3 agent_installation/scripts/render_managed_blocks.py --check` is invoked from the Makefile `refresh-installed` target and from the 35-case unittest, but the workflow file does not invoke it. After Item 2 lands, the unittest becomes the parity enforcement on every CI run; `--check` remains a manual / pre-release gate.

3. **Path ambiguity in user-facing docs.** `scripts/render_managed_blocks.py` (without the `agent_installation/` prefix) is correct in CWD-correct contexts (inside `agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md` and `agent_installation/README.md`) but is ambiguous when cited from elsewhere. Item 3 of the cleanup sequence resolves the root-relative citations to the unambiguous form.

---

## 5. Evidence summary (file:line citations)

| Claim | Evidence |
|---|---|
| `cmd/mpm-lint/main.go` exists | `git ls-files \| grep mpm-lint/main.go` → `cmd/mpm-lint/main.go` (206 lines) |
| `mpm-lint --gate` runs in pre-commit | `scripts/pre-commit:86` (`go run ./cmd/mpm-lint --gate` with `exit 1` on threshold exceedance) |
| `mpm-lint --gate` runs in CI | `.github/workflows/build-test.yml:74-91` (gating `gate` job step) |
| `agent_installation/scripts/render_managed_blocks.py` exists | `git ls-files` includes the path; 633 lines |
| Renderer parity test | `agent_installation/tests/test_render_managed_blocks.py`, 35 cases pass |
| Renderer invoked by Makefile | `Makefile:228` (regenerate), `Makefile:252` (`--check` post-refresh) |
| Renderer never at `scripts/render_managed_blocks.py` | `git log --all -- scripts/render_managed_blocks.py` returns no commits |
| Renderer NOT in pre-commit | `grep -c "render_managed\|renderer" scripts/pre-commit` = 0 |
| Renderer NOT directly in CI (gated by 35-case unittest after Item 2) | `grep -n "render_managed" .github/workflows/build-test.yml` returns no matches before Item 2 |
| Neither tool removed during recent remediation | `git log --diff-filter=D` for `mpm-lint` and `render_managed_blocks` returns no entries in commits `a2eceb1..1ff96c8` |
| CI step added by Item 2 | `.github/workflows/build-test.yml` `gate` job new step `Renderer parity (managed-block byte-for-byte)` |
