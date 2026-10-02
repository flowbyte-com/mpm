# mpm Master Makefile
# Usage: make <target>
#
# Canonical binary location: $HOME/.mpm/bin/ (the same root as data, config,
# logs, and the runtime database). This is the install target for both the
# `make build` and `make install` paths — no sudo, no /usr/local copy, no
# XDG split. Agent integrations invoke $HOME/.mpm/bin/mpm directly; PATH
# is convenience, not contract.
#
# If you cloned to a different location, PREFIX defaults to whatever
# $(HOME)/.mpm resolves to via the standard mpm data-root convention.
# Override with `make install PREFIX=/somewhere` for non-standard layouts.
#
# Targets:
#   make build               - Build all five binaries to bin/
#                              (mpm, mpm-mcp, mpm-scheduler, mpm-critic, mpm-telemetry)
#   make install             - Verify binaries are at $(PREFIX)/bin/ (canonical). No copy step.
#   make service-scheduler   - Install mpm-scheduler systemd USER unit
#                              (fails on encrypted home dirs — use install.sh instead)
#   make service             - Alias for service-scheduler
#   make clean               - Remove bin/
#   make test                - Run tests
#   make lint                - Run golangci-lint (advisory; not CI-gated)
#   make help                - Show this help
#
# RECOMMENDED INSTALL PATH:
#   ./install.sh
# This single command builds, installs binaries + wrapper at $HOME/.mpm/bin/,
# creates ~/.local/bin symlinks for `mpm` and `mpm-mcp`, installs the
# USER-level systemd unit, registers with OpenClaw if present, and
# validates end-to-end. No sudo required. See INSTALL.md for full details.

BINARY_NAME := mpm
MCP_BINARY  := mpm-mcp
SCHED_BINARY := mpm-scheduler
CRITIC_BINARY := mpm-critic
TELEMETRY_BINARY := mpm-telemetry
BUILD_DIR   := bin
# Canonical install prefix: $HOME/.mpm (matches DATA_ROOT in install.sh).
# Override with `make install PREFIX=/somewhere` for non-standard layouts.
PREFIX      ?= $(HOME)/.mpm
SERVICE_NAME := mpm-scheduler
SERVICE_SRC  := contrib/systemd/$(SERVICE_NAME).service.user
SERVICE_DST := $(HOME)/.config/systemd/user/$(SERVICE_NAME).service

TELEMETRY_SERVICE_NAME := mpm-telemetry
TELEMETRY_SERVICE_SRC := contrib/systemd/$(TELEMETRY_SERVICE_NAME).service.user
TELEMETRY_SERVICE_DST := $(HOME)/.config/systemd/user/$(TELEMETRY_SERVICE_NAME).service

# Find go: prefer $PATH, fall back to common install locations.
# Allows `make` to work in non-interactive shells (CI, subshells) where
# ~/.bashrc isn't sourced.
GO := $(shell command -v go 2>/dev/null || echo /usr/local/go/bin/go)

VERSION     := $(shell git describe --tags 2>/dev/null || echo "dev")
BUILD_LDFLAGS := -ldflags "-X main.buildVersion=$(VERSION)"
CGO_CFLAGS := -DSQLITE_ENABLE_FTS5=1
CGO_LDFLAGS := -lm
# Run filter for make test-release; override with `make test-release RUN='-run TestFoo'`.
RELEASE_RUN ?=

# Where `make install-hooks` writes the git hook. Uses git's own answer so
# a repository that has relocated its hooks (core.hooksPath) is honoured
# rather than silently written to the wrong place. Overridable so tests can
# redirect it into a sandbox.
GIT_HOOKS_DIR ?= $(shell git rev-parse --git-path hooks 2>/dev/null || echo .git/hooks)

# The pre-commit guard subset for the nested tools module, and the guards
# that must never drop out of it. Both are Make variables rather than
# inline recipe text so TestBuildConfig_ToolsGuardSubsetIsNonVacuous can
# read the exact regex the gate runs and prove it is not empty.
#
# Why a subset rather than the whole module: the full module is ~50s, and
# no single test dominates it (the cost is spread across hundreds of
# DB-backed tests, each 0.1-0.9s). A subset can therefore not be
# meaningfully sped up by excluding "slow" tests, and the guards
# themselves are the part that must never be allowed to rot. Measured:
# this subset is 0.25s of test time, ~2.6s wall including compilation.
TOOLS_GUARD_RUN := TestOutputPolicy_OnlyMCPEnforces|TestParity_AllActionTools_LockEverySurface|TestAdapterCallsites_MatchGoSchema|TestGuardScopes_
# The three cross-surface guards. TestParity_ prefix and TestGuardScopes_
# are intentionally absent here: the first is covered by the
# AllActionTools sweep, the second tests the scope resolution the other
# three depend on. Both are in TOOLS_GUARD_RUN above.
TOOLS_REQUIRED_GUARDS := TestOutputPolicy_OnlyMCPEnforces TestParity_AllActionTools_LockEverySurface TestAdapterCallsites_MatchGoSchema

# The subprocess-isolation guards. Note which test is which — this is the
# single most important detail in this block, and getting it wrong produces
# a gate that is green and useless:
#
#   TestNoUnisolatedMPMSubprocess
#       THE scanner. It walks the repository and parses every _test.go
#       file, so a violation FAILS the commit regardless of which package
#       introduced it. This is the only test that can catch a bad
#       subprocess in code it has never seen.
#   TestScannerDetectsKnownBadForm
#       does NOT scan the repository. It runs the scanner's classification
#       logic against in-memory fixtures only — it proves the scanner
#       CATCHES known-bad shapes, not that it CATCHES YOUR shape. A gate
#       that runs only this test passes on a repository containing an
#       arbitrary unisolated subprocess.
#   TestEnv* / TestWithExtraOverridesBlank
#       assert the sanctioned helper is actually safe. Cheap (env-map and
#       path-string assertions, no subprocess, no DB).
TESTENV_GUARD_RUN := TestNoUnisolatedMPMSubprocess|TestScannerDetectsKnownBadForm|TestEnvSurvivesHostileParent|TestEnvIsolatesUnderRepoCwd|TestEnvHasNoDuplicateKeys|TestWithExtraOverridesBlank
# Both must be present. Without the scanner the gate catches nothing; with
# only the scanner a regression in the helper goes unnoticed, because the
# scanner's fixtures only assert that bad forms are REJECTED, never that
# the sanctioned escape hatch is SAFE. The two failures are independent
# and each is silent on its own.
TESTENV_REQUIRED_GUARDS := TestNoUnisolatedMPMSubprocess TestScannerDetectsKnownBadForm

# Floor on the number of tests `test-scripts` must collect. `unittest`
# exits 0 when it discovers nothing, so a directory rename, a broken
# importable-path, or a pattern typo would turn the gate into a silent
# pass — the same failure shape as the gate having never been wired at
# all. This is the Python analogue of the TOOLS_REQUIRED_GUARDS /
# TESTENV_REQUIRED_GUARDS checks above. Measured population: 139 tests.
# The floor is deliberately well below that so ordinary test removal
# does not trip it; it exists to catch a gate that has stopped seeing
# anything.
SCRIPTS_TEST_MIN_TESTS := 50

# Floors for the two agent_installation gates. Same reasoning as
# SCRIPTS_TEST_MIN_TESTS: `python3 -m unittest discover` and
# `node --test` both exit 0 when they collect nothing, so a directory
# rename or a pattern change would turn either gate into a silent pass.
# Measured populations: 318 (agent_installation/tests/), 178 Python +
# 137 passing JS (the per-adapter suites).
AGENT_INSTALL_TEST_MIN_TESTS := 300
AGENT_ADAPTER_TEST_MIN_PY_TESTS := 150
AGENT_ADAPTER_TEST_MIN_JS_TESTS := 100
# Adapters whose Python suites are reachable via `unittest discover`.
# Every directory with a tests/ package is listed explicitly; a new
# adapter must be added here or it is silently skipped.
AGENT_ADAPTERS := mpm-claude-code mpm-hermes mpm-memory-openclaw mpm-opencode mpm-pi
# Adapters that additionally ship a `node --test` suite.
AGENT_ADAPTERS_JS := mpm-memory-openclaw mpm-auto-mode-persona-openclaw

.PHONY: all build install install-hooks service-scheduler service-telemetry service uninstall-service gen-cli test test-release test-race release-gate test-core-precommit test-tools test-tools-race test-tools-precommit test-testenv test-testenv-race test-testenv-precommit test-scripts test-agent-installation test-agent-adapters lint help refresh-installed check-installed-drift check-installed

all: build

# Build canonical binaries to bin/.
# Requires CGO for mattn/go-sqlite3 with FTS5 support.
# mpm-critic is invoked by mpm-scheduler as a payload handler
# (kind=critic_audit) — built alongside the other daemons so a
# single `make build` produces a complete installable set.
#
# Pre-build cleanup: if $(BUILD_DIR)/mpm exists but is NOT a compiled
# ELF (e.g. a legacy wrapper script left by an older install.sh that
# separated mpm into wrapper + mpm.real), Go will refuse to overwrite
# a non-object file at the same path. Detect and remove the stale
# wrapper so `make build` works standalone on already-installed repos.
# The corresponding cleanup in install.sh also removes any leftover
# mpm.real and mpm.pre-wrapper.* sidecars from older installs.
build:
	@mkdir -p $(BUILD_DIR)
	@if [ -f "$(BUILD_DIR)/$(BINARY_NAME)" ] \
	   && head -c 2 "$(BUILD_DIR)/$(BINARY_NAME)" 2>/dev/null | grep -q '^#!'; then \
	    echo "  removing stale wrapper at $(BUILD_DIR)/$(BINARY_NAME)"; \
	    rm -f "$(BUILD_DIR)/$(BINARY_NAME)"; \
	fi
	CGO_CFLAGS=$(CGO_CFLAGS) $(GO) build -tags fts5 $(BUILD_LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)    ./cmd/mpm
	CGO_CFLAGS=$(CGO_CFLAGS) $(GO) build -tags fts5 $(BUILD_LDFLAGS) -o $(BUILD_DIR)/$(MCP_BINARY)   ./cmd/mpm-mcp
	CGO_CFLAGS=$(CGO_CFLAGS) $(GO) build -tags fts5 $(BUILD_LDFLAGS) -o $(BUILD_DIR)/$(SCHED_BINARY) ./cmd/mpm-scheduler
	CGO_CFLAGS=$(CGO_CFLAGS) $(GO) build -tags fts5 $(BUILD_LDFLAGS) -o $(BUILD_DIR)/$(CRITIC_BINARY) ./cmd/mpm-critic
	CGO_CFLAGS=$(CGO_CFLAGS) $(GO) build -tags fts5 $(BUILD_LDFLAGS) -o $(BUILD_DIR)/$(TELEMETRY_BINARY) ./cmd/mpm-telemetry
	@echo "🤖 Built $(BUILD_DIR)/$(BINARY_NAME), $(BUILD_DIR)/$(MCP_BINARY), $(BUILD_DIR)/$(SCHED_BINARY), $(BUILD_DIR)/$(CRITIC_BINARY), and $(BUILD_DIR)/$(TELEMETRY_BINARY) (mpm-alpha)"

# Verify the canonical install location contains all five binaries.
# `make build` already writes to bin/, which IS $(PREFIX)/bin/ when the repo
# is cloned at $HOME/.mpm (the standard layout). On a non-standard layout
# (repo cloned somewhere other than $HOME/.mpm), this target copies the
# build output into the canonical location. No sudo — the canonical
# location is always user-writable.
#
# 2026-09-14 release-pass: this target is intended for development
# workflows. For the canonical user-facing install — including
# ~/.local/bin/mpm symlinks, PATH integration, and the user-level
# systemd unit — run ./install.sh. The two routes produce
# the same canonical layout ($PREFIX/bin/) for the binaries
# themselves; install.sh adds the PATH surface that
# `make install` does not.
install: build refresh-installed
	@echo "🚀 Verifying canonical install at $(PREFIX)/bin/..."
	@mkdir -p $(PREFIX)/bin
	@if [ "$(BUILD_DIR)" != "$(PREFIX)/bin" ] && [ ! -L "$(BUILD_DIR)" ] && [ ! -L "$(PREFIX)" ]; then \
	    echo "==> syncing bin/ -> $(PREFIX)/bin/ (all five must land; a partial sync is a failure)"; \
	    install -m755 $(BUILD_DIR)/$(BINARY_NAME)    $(PREFIX)/bin/$(BINARY_NAME) && \
	    install -m755 $(BUILD_DIR)/$(MCP_BINARY)    $(PREFIX)/bin/$(MCP_BINARY) && \
	    install -m755 $(BUILD_DIR)/$(SCHED_BINARY)  $(PREFIX)/bin/$(SCHED_BINARY) && \
	    install -m755 $(BUILD_DIR)/$(CRITIC_BINARY) $(PREFIX)/bin/$(CRITIC_BINARY) && \
	    install -m755 $(BUILD_DIR)/$(TELEMETRY_BINARY) $(PREFIX)/bin/$(TELEMETRY_BINARY) && \
	    echo "    (synced bin/ to $(PREFIX)/bin/)" || \
	    { echo "    FAIL: could not sync all five binaries into $(PREFIX)/bin/." >&2; \
	      echo "    The success line that follows asserts all five are present there; it" >&2; \
	      echo "    must not be printed over a partial install. Inspect $(PREFIX)/bin/ to" >&2; \
	      echo "    see which binaries landed, then re-run make install." >&2; \
	      exit 1; }; \
	else \
	    echo "    (bin/ is the canonical location; no copy needed)"; \
	fi
	@echo "✓ Canonical binaries at $(PREFIX)/bin/: $(BINARY_NAME) $(MCP_BINARY) $(SCHED_BINARY) $(CRITIC_BINARY) $(TELEMETRY_BINARY)"
	@echo ""
	@echo "ℹ  For the full user install (PATH symlinks + systemd unit),"
	@echo "    run: ./install.sh"

# Install the mpm-scheduler systemd user service.
# The unit is templated for the standard ~/projects/mpm layout; override
# paths via:
#   1. Drop-in:  systemctl --user edit mpm-scheduler
#   2. Env file: ~/.config/mpm/mpm.env  (sourced as EnvironmentFile=-)
# mpm-mcp is intentionally NOT shipped as a systemd unit — it's spawned
# by MCP hosts (Claude Code, OpenClaw) as a stdio child process.
#
# ⚠  This target installs a USER-level systemd unit. It silently fails
#    on hosts with encrypted home directories (eCryptfs/LUKS). For the
#    full install flow (PATH symlinks, MCP registration, validation),
#    use install.sh instead.
service-scheduler:
	@echo "⚠  This target installs a USER-level systemd unit."
	@echo "   It silently fails on encrypted home directories."
	@echo "   For the full install flow, use: install.sh"
	@echo ""
	@install -Dm644 $(SERVICE_SRC) $(SERVICE_DST)
	@systemctl --user daemon-reload
	@echo "✓ Installed $(SERVICE_DST)"
	@echo ""
	@echo "  Next steps:"
	@echo "    systemctl --user enable --now $(SERVICE_NAME)"
	@echo "    systemctl --user status $(SERVICE_NAME)"
	@echo "    journalctl --user -u $(SERVICE_NAME) -f"

# Install the mpm-telemetry systemd user service.
# Same lazy-start architecture as mpm-scheduler: designed to stay dead
# at boot on encrypted-home hosts; lock file inside encrypted tree makes
# pre-decrypt start impossible. Wake event from scheduler is the trigger.
service-telemetry:
	@echo "⚠  This target installs a USER-level systemd unit."
	@echo "   It silently fails on encrypted home directories."
	@echo ""
	@install -Dm644 $(TELEMETRY_SERVICE_SRC) $(TELEMETRY_SERVICE_DST)
	@mkdir -p $(HOME)/.mpm/run
	@systemctl --user daemon-reload
	@echo "✓ Installed $(TELEMETRY_SERVICE_DST)"
	@echo ""
	@echo "  Next steps:"
	@echo "    systemctl --user enable --now $(TELEMETRY_SERVICE_NAME)"
	@echo "    systemctl --user status $(TELEMETRY_SERVICE_NAME)"
	@echo "    journalctl --user -u $(TELEMETRY_SERVICE_NAME) -f"

# Alias for the common case.
service: service-scheduler

# Remove the installed systemd user unit. Safe to run even if not installed.
uninstall-service:
	@rm -f $(SERVICE_DST)
	-@systemctl --user daemon-reload
	@echo "✓ Removed $(SERVICE_DST) (if it existed)"

# Install the tracked pre-commit hook into this repository's git hooks
# directory. This is what scripts/pre-commit's own header tells users to
# run; the instruction existed for a long time with no corresponding
# Makefile target, so following the hook's own instructions produced
# "No rule to make target 'install-hooks'".
#
# Deliberately local and offline: it copies one tracked file and sets the
# executable bit. It performs no network action and no push. Idempotent —
# re-running overwrites the hook with the current tracked source, which is
# the point: a hook left over from an older revision gates on stale rules.
install-hooks:
	@mkdir -p "$(GIT_HOOKS_DIR)"
	@cp scripts/pre-commit "$(GIT_HOOKS_DIR)/pre-commit"
	@chmod +x "$(GIT_HOOKS_DIR)/pre-commit"
	@echo "✓ installed scripts/pre-commit -> $(GIT_HOOKS_DIR)/pre-commit"

# Regenerate the CLI command catalogue in docs/SPEC.md (sent-injected
# auto-generated block in §8). Walks r.Commands via go/ast — no
# reflection, no runtime import, source-level extraction. Idempotent.
gen-cli:
	$(GO) run ./cmd/gen-cli

# Run tests
test:
	CGO_CFLAGS=$(CGO_CFLAGS) CGO_LDFLAGS=$(CGO_LDFLAGS) $(GO) test -tags fts5 -v ./cmd/...
	CGO_CFLAGS=$(CGO_CFLAGS) CGO_LDFLAGS=$(CGO_LDFLAGS) $(GO) test -tags fts5 -v ./internal/telemetry/...
	cd internal/core && CGO_CFLAGS=$(CGO_CFLAGS) CGO_LDFLAGS=$(CGO_LDFLAGS) $(GO) test -tags fts5 -v ./...
	CGO_CFLAGS=$(CGO_CFLAGS) CGO_LDFLAGS=$(CGO_LDFLAGS) $(GO) test -tags fts5 -v ./internal/scheduler/...
	$(MAKE) test-testenv
	$(MAKE) test-tools
	$(MAKE) test-scripts
	$(MAKE) test-agent-installation

# Run the pre-commit subset of internal/core tests with the same FTS5 flag
# discipline as `make test`. The pre-commit hook invokes this target rather
# than running `go test` directly so the flag set has a single source of
# truth (the Makefile CGO_CFLAGS / CGO_LDFLAGS lines) and a fresh checkout
# without a build cache still passes — the `-tags fts5` build tag alone is
# not enough; the C-level FTS5 compile flag must also be set, and that flag
# lives here, not in the hook.
#
# RUN takes a `-run` regex; defaults to the same set scripts/pre-commit was
# previously invoking directly.
test-core-precommit:
	cd internal/core && CGO_CFLAGS=$(CGO_CFLAGS) CGO_LDFLAGS=$(CGO_LDFLAGS) $(GO) test -short -count=1 -tags fts5 ./... \
		-run "TestSynthesis|TestReliability|TestLifecycle|TestHybrid|TestGetRecentUserTopics|TestDBSafety"

# Pre-commit gate for the nested tools module (internal/core/tools).
#
# This module is a separate Go module, so `./...` from the root does not
# reach it. It holds three cross-surface guards — MCP output-policy
# enforcement, registry/dispatcher parity, and cross-language adapter
# schema drift — all of which inspect source OUTSIDE the Go package they
# live in. Before this target existed they were named only in comments in
# scripts/pre-commit and .github/workflows/build-test.yml, and nothing
# executed them.
#
# The gate refuses to run on an empty population. `go test -run <regex>`
# exits 0 when the regex matches nothing, so a renamed test or a typo
# would silently disable the gate — the same failure shape as the guards
# themselves had. The recipe therefore expands the regex with
# `go test -list` first and fails if it resolves to zero tests, or if any
# of TOOLS_REQUIRED_GUARDS is missing from it.
test-tools-precommit:
	@cd internal/core/tools && \
	 matched=$$(CGO_CFLAGS=$(CGO_CFLAGS) CGO_LDFLAGS=$(CGO_LDFLAGS) $(GO) test -tags fts5 -list "$(TOOLS_GUARD_RUN)" ./ | grep -E '^Test' || true); \
	 if [ -z "$$matched" ]; then \
	   echo "[test-tools-precommit] FATAL: TOOLS_GUARD_RUN matched ZERO tests." >&2; \
	   echo "  \`go test -run\` exits 0 on an empty match, so the gate would pass" >&2; \
	   echo "  without running anything. Check for a renamed or deleted test." >&2; \
	   exit 1; \
	 fi; \
	 for guard in $(TOOLS_REQUIRED_GUARDS); do \
	   echo "$$matched" | grep -qx "$$guard" || { \
	     echo "[test-tools-precommit] FATAL: required guard $$guard is not in TOOLS_GUARD_RUN." >&2; \
	     echo "  matched instead: $$matched" >&2; \
	     exit 1; }; \
	 done; \
	 echo "[test-tools-precommit] guard population ($$(echo "$$matched" | wc -l | tr -d ' ') tests):"; \
	 echo "$$matched" | sed 's/^/    - /'; \
	 CGO_CFLAGS=$(CGO_CFLAGS) CGO_LDFLAGS=$(CGO_LDFLAGS) $(GO) test -short -count=1 -tags fts5 -v ./ \
		-run "$(TOOLS_GUARD_RUN)"

# Full test sweep of the nested tools module. Wired into `make test` and
# `make test-race` so a green repository-wide result can no longer be
# achieved while this module is red or untested.
test-tools:
	cd internal/core/tools && CGO_CFLAGS=$(CGO_CFLAGS) CGO_LDFLAGS=$(CGO_LDFLAGS) $(GO) test -count=1 -tags fts5 -v ./...

# Race-enabled sweep of the nested tools module. Included because it is
# practical and matches the policy applied to every other module: the
# tools module dispatches registry handlers against a shared DatabaseManager
# and is exactly where a concurrency defect in the tool layer would live.
# Measured cost: ~57s.
test-tools-race:
	cd internal/core/tools && CGO_CFLAGS=$(CGO_CFLAGS) CGO_LDFLAGS=$(CGO_LDFLAGS) $(GO) test -race -count=1 -tags fts5 -v ./...

# The subprocess-isolation guard. This package is a repository SAFETY
# mechanism, not a feature, which makes its inclusion in the gate a
# correctness question rather than a coverage one: a guard that no
# target runs is not a guard, it is a file. `./...` from the root does
# not reach internal/testenv (the Makefile enumerates package roots
# explicitly, as it does for internal/telemetry and internal/scheduler),
# so these targets exist to keep the scanner and the hostile-parent
# proofs inside `make test` / `make test-race`.
#
# The scanner parses every .go file in the repository, so a violation in
# any package fails the gate even though the package containing it may
# not itself be listed here.
test-testenv:
	CGO_CFLAGS=$(CGO_CFLAGS) CGO_LDFLAGS=$(CGO_LDFLAGS) $(GO) test -count=1 -tags fts5 -v ./internal/testenv/...

test-testenv-race:
	CGO_CFLAGS=$(CGO_CFLAGS) CGO_LDFLAGS=$(CGO_LDFLAGS) $(GO) test -race -count=1 -tags fts5 -v ./internal/testenv/...

# Pre-commit subset of the subprocess-isolation guard, mirroring the
# zero-match protection `test-tools-precommit` uses.
#
# Why this target exists at all, when `go test ./...` from the repo root
# already reaches internal/testenv (it is part of the main module — only
# internal/core and internal/core/tools are nested modules): the root
# traversal runs in the CI full-sweep jobs, but NOT in this repository's
# own pre-commit hook, which calls make targets one at a time. A guard
# that only runs in CI fails a developer's commit at push time instead of
# at commit time, which is the whole point of a pre-commit gate.
#
# The scanner is fast (parses the tree once, no subprocess, no DB), so
# the cost of running it on every commit is a file walk. It is not gated
# on staged-file types on purpose: a violation can be introduced by a
# hand-edited file, and conditioning on what `git add` happened to pick up
# would make the gate's coverage depend on staging discipline.
#
# `go test -run <regex>` exits 0 when the regex matches nothing, so a
# renamed test would silently disable this gate — the same failure shape
# as the guard having never been wired at all. The recipe therefore
# resolves the population with `go test -list` and fails on an empty or
# incomplete match.
test-testenv-precommit:
	@matched=$$(CGO_CFLAGS=$(CGO_CFLAGS) CGO_LDFLAGS=$(CGO_LDFLAGS) $(GO) test -tags fts5 -list "$(TESTENV_GUARD_RUN)" ./internal/testenv/ | grep -E '^Test' || true); \
	 if [ -z "$$matched" ]; then \
	   echo "[test-testenv-precommit] FATAL: TESTENV_GUARD_RUN matched ZERO tests." >&2; \
	   echo "  \`go test -run\` exits 0 on an empty match, so the gate would pass" >&2; \
	   echo "  without running anything. Check for a renamed or deleted test." >&2; \
	   exit 1; \
	 fi; \
	 for guard in $(TESTENV_REQUIRED_GUARDS); do \
	   echo "$$matched" | grep -qx "$$guard" || { \
	     echo "[test-testenv-precommit] FATAL: required guard $$guard is not in TESTENV_GUARD_RUN." >&2; \
	     echo "  matched instead: $$matched" >&2; \
	     exit 1; }; \
	 done; \
	 echo "[test-testenv-precommit] guard population ($$(echo "$$matched" | wc -l | tr -d ' ') tests):"; \
	 echo "$$matched" | sed 's/^/    - /'; \
	 CGO_CFLAGS=$(CGO_CFLAGS) CGO_LDFLAGS=$(CGO_LDFLAGS) $(GO) test -short -count=1 -tags fts5 -v ./internal/testenv/ \
		-run "$(TESTENV_GUARD_RUN)"

# Validation gate for the Python test suites under scripts/tests/.
#
# WHY THIS TARGET EXISTS
#
# Nothing in this repository's validation path could see a Python test
# under scripts/tests/. Three separate boundaries conspired:
#
#   1. `scripts/` is its own Go module, so `go test ./...` from the repo
#      root cannot descend into it. This is the same nested-module shape
#      that already forced test-tools / test-tools-race and
#      test-testenv / test-testenv-race into `make test` / `make test-race`.
#   2. The Makefile's only Python invocation in its entire history was
#      `check-installed-drift` -> agent_installation's
#      tests.test_installed_block_drift. No target enumerated
#      scripts/tests/ at all.
#   3. scripts/pre-commit runs no Python whatsoever; it invokes
#      make test-core-precommit, test-tools-precommit, and
#      test-testenv-precommit, all Go.
#
# The consequence was measured, not assumed: with a deliberately failing
# test in scripts/tests/test_install_phase_binaries.py, `make test`
# exited 0, `make test-race` exited 0, `make release-gate` exited 0, and
# scripts/pre-commit exited 0 — and the sabotaged test's name appeared
# zero times in any of their logs, because none of them executed the
# file. Meanwhile that file had been reporting 2 failures and 3 errors
# since dd91fbf8 (2026-09-25) removed the shell wrapper the tests
# asserted. A green gate was reporting on a suite it never ran.
#
# SCOPE
#
# scripts/tests/ only. agent_installation/tests/ is NOT duplicated here:
# it is already owned by CI (`cd agent_installation && python3 -m unittest
# discover tests` in .github/workflows/build-test.yml), and running its
# ~36s locally as well would double the gate's cost for no additional
# signal. If that ownership ever changes, move it here rather than
# adding a third runner.
#
# Deliberately NOT wired into scripts/pre-commit: that suite costs ~16s
# against a pre-commit budget the Go subsets keep at seconds, and it is
# not a staged-file-type-guarded fast subset the way test-testenv-
# precommit is. `make test` / `make test-race` / `make release-gate` are
# the supported validation gates, and they now run it.
#
# `discover` is the same invocation CI already uses for
# agent_installation; `scripts/tests/` is a namespace package with no
# __init__.py, so `discover` only works with `tests` as a relative start
# directory from `scripts/`, not with an absolute `-s` path.
#
# NON-VACUITY GUARD
#
# See SCRIPTS_TEST_MIN_TESTS above. The recipe parses the reported test
# count and fails on an unparseable count, a zero count, or a count
# below the floor, before propagating the suite's real exit status.
test-scripts:
	@cd scripts && out=$$(python3 -m unittest discover tests 2>&1); status=$$?; \
	  printf '%s\n' "$$out"; \
	  n=$$(printf '%s\n' "$$out" | sed -n 's/^Ran \([0-9][0-9]*\) tests\?.*/\1/p' | tail -1); \
	  if [ -z "$$n" ]; then \
	    echo "[test-scripts] FATAL: could not parse a test count from the run." >&2; \
	    echo "  A silent pass here is indistinguishable from a green suite." >&2; \
	    exit 1; \
	  fi; \
	  if [ "$$n" -lt $(SCRIPTS_TEST_MIN_TESTS) ]; then \
	    echo "[test-scripts] FATAL: collected $$n tests, floor is $(SCRIPTS_TEST_MIN_TESTS)." >&2; \
	    echo "  A rename or deletion has probably silently reduced this gate." >&2; \
	    exit 1; \
	  fi; \
	  echo "[test-scripts] collected $$n tests (floor $(SCRIPTS_TEST_MIN_TESTS))"; \
	  exit $$status

# Local mirror of the `agent_installation/tests/` suite that CI already
# runs (.github/workflows/build-test.yml, "Renderer parity
# (managed-block byte-for-byte)": `cd agent_installation && python3 -m
# unittest discover tests`).
#
# WHY THIS TARGET EXISTS
#
# That 318-case suite was reachable from CI and from nowhere else. No
# make target and no pre-commit step ran it, so the one Python suite
# the project actually treats as a merge gate could not be reproduced
# or triaged locally — and when it went red, the first person to notice
# was CI.
#
# Measured with a deliberately failing test added to a per-adapter
# suite (a file this command does not even reach): `make test` exited 0,
# `scripts/pre-commit` exited 0, and neither mentioned the sabotaged
# test. Same class of gap as the scripts/tests/ one closed by
# test-scripts: a suite with no runner is not a gate, it is a file.
#
# SAFETY
#
# The suite is read-only with respect to live state. It reads the
# operator's installed host files (`~/.claude/CLAUDE.md`,
# `~/.pi/agent/AGENTS.md`, per-host install targets) and the repository
# working tree, and degrades explicitly: snippet fallbacks for hosts
# with no globally installed file, and a per-host skip driven by an
# integration-installed probe, so a machine without a given host
# installed does not produce spurious failures. It writes nothing
# outside temp dirs. Measured cost: ~33s.
#
# SCOPE
#
# This is the CI-owned suite only. The per-adapter suites under
# mpm-*/tests/ are a different, larger problem — see
# test-agent-adapters below. They are deliberately NOT folded in here.
test-agent-installation:
	@cd agent_installation && out=$$(python3 -m unittest discover tests 2>&1); status=$$?; \
	  printf '%s\n' "$$out"; \
	  n=$$(printf '%s\n' "$$out" | sed -n 's/^Ran \([0-9][0-9]*\) tests\?.*/\1/p' | tail -1); \
	  if [ -z "$$n" ]; then \
	    echo "[test-agent-installation] FATAL: could not parse a test count." >&2; \
	    echo "  A silent pass here is indistinguishable from a green suite." >&2; \
	    exit 1; \
	  fi; \
	  if [ "$$n" -lt $(AGENT_INSTALL_TEST_MIN_TESTS) ]; then \
	    echo "[test-agent-installation] FATAL: collected $$n tests, floor is $(AGENT_INSTALL_TEST_MIN_TESTS)." >&2; \
	    echo "  A rename or deletion has probably silently reduced this gate." >&2; \
	    exit 1; \
	  fi; \
	  echo "[test-agent-installation] collected $$n tests (floor $(AGENT_INSTALL_TEST_MIN_TESTS))"; \
	  exit $$status

# Diagnostic runner for the per-adapter suites under mpm-*/tests/.
#
# *** THIS TARGET IS CURRENTLY RED AND IS DELIBERATELY NOT GATED. ***
#
# It exists because those 20 files (10 .py, 9 .test.js, 1 .sh) were
# reachable from NO runner at all — not from make test / make test-race
# / make release-gate, not from scripts/pre-commit, and not from the
# CI job either, whose only Python step is `unittest discover tests`
# from inside agent_installation/. `unittest discover` does not descend
# into a subdirectory, and the CI workflow has no Node step at all, so
# `node --test` was never run by anything. 19 of the 20 run here; the
# 20th is the live host check documented at the bottom of this block.
#
# The JS families are invoked as `node --test 'tests/*.test.js'`, not
# `node --test tests/`. On Node 24.20 the directory form resolves
# `tests` as a MODULE and dies with MODULE_NOT_FOUND, exiting 1 with a
# plausible-looking summary and zero passes. The pass-count floor below
# is what turns that into an explicit refusal instead of a number.
#
# Measured current state (2026-10-02, at 1e410f45):
#
#   mpm-claude-code            34 tests   2 failures
#   mpm-hermes                 38 tests   4 failures
#   mpm-memory-openclaw        13 tests   OK
#   mpm-opencode               31 tests   1 failure
#   mpm-pi                     62 tests   3 failures
#   mpm-memory-openclaw/*.js  117 tests   1 failure  (installer.test.js)
#   mpm-auto-mode-*/*.js       21 tests   OK
#
# 10 Python failures and 1 JS failure. Wiring that into `make test`
# would turn a green gate red without fixing anything, which is a
# regression dressed as coverage. So this target is runnable and
# honest, but not a gate, until those failures have individual
# root-cause work.
#
# Each family's tests are counted and checked against a floor before
# the real exit status is propagated, so a suite that stops being
# discovered cannot hide behind the other suite's result. A missing
# `node` is a hard error rather than a skip: silently skipping the JS
# suites on a host that cannot run them is how they went unnoticed.
#
# NOT INCLUDED: mpm-claude-code/tests/session_start_hook.test.sh. It is
# a host-specific live check — it exports MPM_WORKSPACE="$HOME/.mpm" and
# invokes "$HOME/.local/bin/mpm" — so it depends on this machine's
# actual install and must not join a hermetic gate. Run it by hand.
test-agent-adapters:
	@command -v node >/dev/null 2>&1 || { \
	   echo "[test-agent-adapters] FATAL: node not found on PATH." >&2; \
	   echo "  The .test.js suites would be silently skipped, which is how" >&2; \
	   echo "  they became unreachable in the first place." >&2; \
	   exit 1; }; \
	 command -v python3 >/dev/null 2>&1 || { \
	   echo "[test-agent-adapters] FATAL: python3 not found on PATH." >&2; \
	   exit 1; }; \
	 py_total=0; js_total=0; failed=0; \
	 for d in $(AGENT_ADAPTERS); do \
	   if [ ! -d "agent_installation/$$d/tests" ]; then \
	     echo "[test-agent-adapters] FATAL: agent_installation/$$d/tests is missing." >&2; \
	     echo "  AGENT_ADAPTERS lists it, so its absence is a packaging error." >&2; \
	     exit 1; \
	   fi; \
	   out=$$(cd "agent_installation/$$d" && python3 -m unittest discover tests 2>&1); status=$$?; \
	   printf '%s\n' "$$out"; \
	   n=$$(printf '%s\n' "$$out" | sed -n 's/^Ran \([0-9][0-9]*\) tests\?.*/\1/p' | tail -1); \
	   if [ -z "$$n" ]; then \
	     echo "[test-agent-adapters] FATAL: $$d reported no test count." >&2; \
	     exit 1; \
	   fi; \
	   py_total=$$((py_total + n)); \
	   if [ $$status -ne 0 ]; then failed=1; fi; \
	 done; \
	 if [ $$py_total -lt $(AGENT_ADAPTER_TEST_MIN_PY_TESTS) ]; then \
	   echo "[test-agent-adapters] FATAL: collected $$py_total Python tests, floor is $(AGENT_ADAPTER_TEST_MIN_PY_TESTS)." >&2; \
	   exit 1; \
	 fi; \
	 echo "[test-agent-adapters] Python: $$py_total tests (floor $(AGENT_ADAPTER_TEST_MIN_PY_TESTS))"; \
	 for d in $(AGENT_ADAPTERS_JS); do \
	   if [ ! -d "agent_installation/$$d/tests" ]; then \
	     echo "[test-agent-adapters] FATAL: agent_installation/$$d/tests is missing." >&2; \
	     exit 1; \
	   fi; \
	   out=$$(cd "agent_installation/$$d" && node --test 'tests/*.test.js' 2>&1); status=$$?; \
	   printf '%s\n' "$$out"; \
	   n=$$(printf '%s\n' "$$out" | sed -n 's/^ℹ pass \([0-9][0-9]*\).*/\1/p' | tail -1); \
	   if [ -z "$$n" ]; then \
	     echo "[test-agent-adapters] FATAL: $$d reported no passing-test count." >&2; \
	     echo "  node --test exited 0 without a pass line — refusing to count it." >&2; \
	     exit 1; \
	   fi; \
	   js_total=$$((js_total + n)); \
	   if [ $$status -ne 0 ]; then failed=1; fi; \
	 done; \
	 if [ $$js_total -lt $(AGENT_ADAPTER_TEST_MIN_JS_TESTS) ]; then \
	   echo "[test-agent-adapters] FATAL: collected $$js_total JS passing tests, floor is $(AGENT_ADAPTER_TEST_MIN_JS_TESTS)." >&2; \
	   exit 1; \
	   fi; \
	 echo "[test-agent-adapters] JS: $$js_total passing tests (floor $(AGENT_ADAPTER_TEST_MIN_JS_TESTS))"; \
	 if [ $$failed -ne 0 ]; then \
	   echo "[test-agent-adapters] KNOWN FAILURES PRESENT — this target is not a gate." >&2; \
	   echo "  See the counts in this target's Makefile comment. Fix them" >&2; \
	   echo "  individually before considering gating it." >&2; \
	   exit 1; \
	 fi; \
	 echo "[test-agent-adapters] all adapter suites green"

# Release acceptance suite — cross-agent continuity, public-CLI parity,
# supersession trace, scale/e2e boundary tests. Spins up real subprocess
# invocations of `bin/mpm` so it requires `make build` first; the FTS5
# build flags must match the production binary or the schema-migration
# path leaves lessons_fts (and other FTS5 modules) unbuilt, surfacing
# as "no such table: main.<base>_fts" at INSERT time on the lessons
# view. This target was previously omitted from `make test` because it
# is slower (subprocess overhead per case) and has a build-artifact
# dependency; promoted to a dedicated target so the regression class
# above cannot recur silently. Run pre-release.
#
# RUN forwards to the release_acceptance package; default is all tests.
test-release: build
	cd release_acceptance && CGO_CFLAGS=$(CGO_CFLAGS) CGO_LDFLAGS=$(CGO_LDFLAGS) $(GO) test -count=1 -tags fts5 $(RELEASE_RUN) ./...

# Aggregate release gate. Composes the broad race-detector suite with
# the cross-agent continuity / public-CLI subprocess suite. The
# subprocess suite requires `make build` so bin/mpm is available to
# the public-CLI test cases; `make test-race` does NOT depend on
# build (it's the inner dev-loop race gate), so we keep test-race and
# test-release as separate leaf targets and compose them at the
# release-gate level rather than threading build into every test-race
# invocation. Release_acceptance cannot silently fall outside future
# release validation while this aggregate exists.
release-gate: test-race test-release

# Run tests with the race detector enabled.
# Mirrors `make test` but adds `-race`. Both flags are required:
#   * `-race`                   — race detector for goroutine/data races
#   * `CGO_CFLAGS=...FTS5`      — C-level compile flag enabling FTS5 in SQLite
#   * `-tags fts5`              — Go build tag selecting the FTS5 driver path
# Bare `go test -race ./...` (no flags) compiles go-sqlite3 without FTS5 and
# breaks the search, pointer-resolution, and scheduler-triggers code paths
# that unconditionally assume FTS5 is compiled in. Both flags are part of
# the substrate's mandatory FTS5 integrity guarantee — see CLAUDE.md §3.
test-race:
	CGO_CFLAGS=$(CGO_CFLAGS) CGO_LDFLAGS=$(CGO_LDFLAGS) $(GO) test -race -tags fts5 -v ./cmd/...
	CGO_CFLAGS=$(CGO_CFLAGS) CGO_LDFLAGS=$(CGO_LDFLAGS) $(GO) test -race -tags fts5 -v ./internal/telemetry/...
	cd internal/core && CGO_CFLAGS=$(CGO_CFLAGS) CGO_LDFLAGS=$(CGO_LDFLAGS) $(GO) test -race -tags fts5 -v ./...
	CGO_CFLAGS=$(CGO_CFLAGS) CGO_LDFLAGS=$(CGO_LDFLAGS) $(GO) test -race -tags fts5 -v ./internal/scheduler/...
	$(MAKE) test-testenv-race
	$(MAKE) test-tools-race
	$(MAKE) test-scripts
	$(MAKE) test-agent-installation

# Run golangci-lint (advisory only — does not gate CI).
# Install: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
lint:
	@command -v golangci-lint >/dev/null 2>&1 || { echo "golangci-lint not installed. Run: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest"; exit 1; }
	golangci-lint run ./...

# Clean build artifacts. Also removes stray root-level binaries: a bare
# `go build ./cmd/mpm` (without -o bin/ and without the FTS5 flags)
# drops a non-canonical, non-FTS5 `mpm` at the repo root that shadows
# nothing but confuses everything. Canonical output is bin/ only.
clean:
	rm -rf $(BUILD_DIR)
	rm -f ./mpm ./mpm-critic ./mpm-scheduler ./mpm-mcp ./mpm-telemetry
	@echo "🧹 Cleaned $(BUILD_DIR)/ (plus stray root binaries)"

# Refresh all currently supported locally-installed MPM agent-integration
# artifacts from the canonical repository sources. Safe to re-run (each
# adapter's installer preserves user-authored content outside the managed
# block). Fails clearly if an adapter cannot be refreshed.
#
# This target is host-agnostic: it does not name any specific adapter.
# The reconciliation entry point discovers every adapter under
# agent_installation/mpm-* that contributes a `reconcile.json` manifest
# and invokes its installer. Opt-in adapters (those whose managed block
# is intentionally absent on this machine) ship with `opt_in: true` so
# the generic pass leaves them alone.
#
# Per-adapter behavior is declared in each adapter's `reconcile.json`.
# Installers are idempotent; on a no-op they print a confirmation and
# exit 0. After refresh, `make check-installed` and `make
# check-installed-drift` verify byte-parity.
AGENT_INSTALL_DIR := agent_installation
refresh-installed:
	@echo "==> refreshing host installed artifacts from canonical source..."
	@echo ""
	@echo "    [1/N] regenerating host template snippets from canonical source"
	@cd $(AGENT_INSTALL_DIR) && python3 scripts/render_managed_blocks.py || { echo "    FAIL: render_managed_blocks.py failed" >&2; exit 2; }
	@echo ""
	@echo "    [2/N] reconciling installed managed blocks via the host-agnostic entry point"
	@python3 $(AGENT_INSTALL_DIR)/scripts/reconcile_managed_blocks.py --home $(HOME) || { echo "    FAIL: reconcile_managed_blocks.py failed" >&2; exit 3; }
	@echo ""
	@cd $(AGENT_INSTALL_DIR) && python3 scripts/render_managed_blocks.py --check \
	    && echo "==> refresh complete; render check PASS." \
	    || { echo "    FAIL: post-refresh render --check failed (drift between canonical source and installed artifacts)" >&2; exit 4; }

# Verify that every persistent-file host's installed managed block
# byte-matches the canonical render. Closes the gap exposed on
# 2026-09-28 when the Pi, Claude, and OpenCode managed blocks
# remained on the 2026-09-04 7-section contract three weeks after
# the 2026-09-27 1.0.0 -> 1.2.0 expansion: the render script's
# `--check` mode validated the snippet, but no check validated the
# next hop in the chain (snippet -> installed persistent file).
#
# `make refresh-installed` is the canonical repair path. This
# target is read-only (does not modify the install targets); on
# failure it prints the per-host drift, first divergent line, and
# the repair command.
check-installed-drift:
	@cd $(AGENT_INSTALL_DIR) && python3 -m unittest tests.test_installed_block_drift -v \
	    || { echo "    FAIL: installed managed blocks drifted from canonical render; run \`make refresh-installed\` to repair" >&2; exit 1; }

# Read-only diagnostic for installed managed blocks. Distinguishes
# PASS / WARN / ABSENT / ERROR per host, with the canonical repair
# path printed for each non-PASS verdict. Exit code is 0 when all
# installed hosts are PASS, 1 when any WARN or ERROR is observed.
# This is the operational diagnostic complement to
# check-installed-drift (which only fails on drift and does not
# report presence separately).
check-installed:
	@python3 $(AGENT_INSTALL_DIR)/scripts/check_installed_managed_blocks.py

# Show help
help:
	@echo "mpm Makefile"
	@echo ""
	@echo "  Canonical location: \$$HOME/.mpm/bin/ (no sudo, no /usr/local copy)"
	@echo ""
	@echo "  Targets:"
	@echo "    make build               - Build all five binaries to bin/ (mpm, mpm-mcp,"
	@echo "                              mpm-scheduler, mpm-critic, mpm-telemetry)"
	@echo "    make install             - Verify/sync bin/ to \$$(PREFIX)/bin/ (default \$$HOME/.mpm)"
	@echo "    make service-scheduler   - Install mpm-scheduler systemd user unit"
	@echo "    make service             - Alias for service-scheduler"
	@echo "    make uninstall-service   - Remove the installed systemd user unit"
	@echo "    make gen-cli             - Regenerate the CLI catalogue in docs/SPEC.md §8"
	@echo "    make test                - Run go tests (root + internal/core + scheduler + tools)"
	@echo "    make test-race           - Same, under -race (the pre-merge gate)"
	@echo "    make test-tools          - Run the full internal/core/tools module"
	@echo "    make test-tools-precommit- Run the tools guard subset the pre-commit hook uses"
	@echo "    make install-hooks       - Install scripts/pre-commit into .git/hooks (no network)"
	@echo "    make lint                - Run golangci-lint (advisory; not CI-gated)"
	@echo "    make clean               - Remove bin/"
	@echo "    make help                - Show this help"
	@echo ""
	@echo "  Version: $(VERSION)"
	@echo "  Prefix:   $(PREFIX)"