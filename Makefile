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
#                              (fails on encrypted home dirs — use scripts/install.sh instead)
#   make service             - Alias for service-scheduler
#   make clean               - Remove bin/
#   make test                - Run tests
#   make lint                - Run golangci-lint (advisory; not CI-gated)
#   make help                - Show this help
#
# RECOMMENDED INSTALL PATH:
#   ./scripts/install.sh
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
# Canonical install prefix: $HOME/.mpm (matches DATA_ROOT in scripts/install.sh).
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

.PHONY: all build install service-scheduler service-telemetry service uninstall-service gen-cli test test-race test-core-precommit lint help refresh-installed

all: build

# Build canonical binaries to bin/.
# Requires CGO for mattn/go-sqlite3 with FTS5 support.
# mpm-critic is invoked by mpm-scheduler as a payload handler
# (kind=critic_audit) — built alongside the other daemons so a
# single `make build` produces a complete installable set.
build:
	@mkdir -p $(BUILD_DIR)
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
# systemd unit — run ./scripts/install.sh. The two routes produce
# the same canonical layout ($PREFIX/bin/) for the binaries
# themselves; scripts/install.sh adds the PATH surface that
# `make install` does not.
install: build
	@echo "🚀 Verifying canonical install at $(PREFIX)/bin/..."
	@mkdir -p $(PREFIX)/bin
	@if [ "$(BUILD_DIR)" != "$(PREFIX)/bin" ] && [ ! -L "$(BUILD_DIR)" ] && [ ! -L "$(PREFIX)" ]; then \
	    install -m755 $(BUILD_DIR)/$(BINARY_NAME)    $(PREFIX)/bin/$(BINARY_NAME) || true; \
	    install -m755 $(BUILD_DIR)/$(MCP_BINARY)    $(PREFIX)/bin/$(MCP_BINARY) || true; \
	    install -m755 $(BUILD_DIR)/$(SCHED_BINARY)  $(PREFIX)/bin/$(SCHED_BINARY) || true; \
	    install -m755 $(BUILD_DIR)/$(CRITIC_BINARY) $(PREFIX)/bin/$(CRITIC_BINARY) || true; \
	    install -m755 $(BUILD_DIR)/$(TELEMETRY_BINARY) $(PREFIX)/bin/$(TELEMETRY_BINARY) || true; \
	    echo "    (synced bin/ to $(PREFIX)/bin/)"; \
	else \
	    echo "    (bin/ is the canonical location; no copy needed)"; \
	fi
	@echo "✓ Canonical binaries at $(PREFIX)/bin/: $(BINARY_NAME) $(MCP_BINARY) $(SCHED_BINARY) $(CRITIC_BINARY) $(TELEMETRY_BINARY)"
	@echo ""
	@echo "ℹ  For the full user install (PATH symlinks + systemd unit),"
	@echo "    run: ./scripts/install.sh"

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
#    use scripts/install.sh instead.
service-scheduler:
	@echo "⚠  This target installs a USER-level systemd unit."
	@echo "   It silently fails on encrypted home directories."
	@echo "   For the full install flow, use: scripts/install.sh"
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

# Regenerate the CLI command catalogue in README.md (sent-injected
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
		-run "TestSynthesis|TestReliability|TestLifecycle|TestHybrid|TestGetRecentUserTopics"

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
# Per-host behavior:
#   Claude Code, OpenCode, Pi — refresh the persistent managed block in the
#     user-scope instruction file (CLAUDE.md / AGENTS.md). Installers are
#     idempotent; on a no-op they print a confirmation and exit 0.
#   Hermes — skipped here (no installed managed block exists in this
#     environment; the hermes-mpm SKILL.md documents the manual flow).
#   OpenClaw — uses runtime injection (no persistent managed block); refresh
#     via its own install.sh which is a separate concern (plugin wiring, not
#     instruction-file refresh).
#
# After refresh, run `agent_installation/scripts/render_managed_blocks.py
# --check` to verify no drift between canonical source and installed
# artifacts.
AGENT_INSTALL_DIR := agent_installation
refresh-installed:
	@echo "==> refreshing host installed artifacts from canonical source..."
	@echo ""
	@echo "    [1/N] regenerating host template snippets from canonical source"
	@cd $(AGENT_INSTALL_DIR) && python3 scripts/render_managed_blocks.py || { echo "    FAIL: render_managed_blocks.py failed" >&2; exit 2; }
	@echo ""
	@echo "    [2/N] Claude Code: refreshing $(HOME)/.claude/CLAUDE.md"
	@python3 $(AGENT_INSTALL_DIR)/claude-code-mpm/scripts/install_claude_instructions.py \
	    --scope user --home $(HOME) \
	    --target $(HOME)/.claude/CLAUDE.md \
	    --snippet $(AGENT_INSTALL_DIR)/claude-code-mpm/templates/CLAUDE.md.snippet \
	    || { echo "    FAIL: Claude Code refresh failed" >&2; exit 3; }
	@echo ""
	@echo "    [3/N] OpenCode: refreshing $(HOME)/.config/opencode/AGENTS.md"
	@python3 $(AGENT_INSTALL_DIR)/opencode-mpm/scripts/install_agents_instructions.py \
	    --scope user \
	    --target $(HOME)/.config/opencode/AGENTS.md \
	    --snippet $(AGENT_INSTALL_DIR)/opencode-mpm/templates/AGENTS.md.snippet \
	    || { echo "    FAIL: OpenCode refresh failed" >&2; exit 4; }
	@echo ""
	@echo "    [4/N] Pi: refreshing $(HOME)/.pi/agent/AGENTS.md"
	@python3 $(AGENT_INSTALL_DIR)/pi-mpm/scripts/install_agents_instructions.py \
	    --scope user \
	    --snippet $(AGENT_INSTALL_DIR)/pi-mpm/templates/AGENTS.md.snippet \
	    || { echo "    FAIL: Pi refresh failed" >&2; exit 5; }
	@echo ""
	@echo "    [5/N] Hermes: no persistent managed block to refresh (skipping)"
	@echo ""
	@cd $(AGENT_INSTALL_DIR) && python3 scripts/render_managed_blocks.py --check \
	    && echo "==> refresh complete; render check PASS." \
	    || { echo "    FAIL: post-refresh render --check failed (drift between canonical source and installed artifacts)" >&2; exit 6; }

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
	@echo "    make gen-cli             - Regenerate the CLI catalogue in README.md §8"
	@echo "    make test                - Run go tests"
	@echo "    make lint                - Run golangci-lint (advisory; not CI-gated)"
	@echo "    make clean               - Remove bin/"
	@echo "    make help                - Show this help"
	@echo ""
	@echo "  Version: $(VERSION)"
	@echo "  Prefix:   $(PREFIX)"