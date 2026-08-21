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
# $(HOME)/.mpm resolves to via the standard mpm data-root convention. To
# override (rare; for shared-host system mode), pass PREFIX=/usr/local or
# use scripts/install.sh --system.
#
# Targets:
#   make build               - Build bin/mpm, bin/mpm-mcp, bin/mpm-scheduler, bin/mpm-critic
#   make install             - Verify binaries are at $(PREFIX)/bin/ (canonical). No copy step.
#   make service-scheduler   - [LEGACY/OPT-IN] Install mpm-scheduler systemd USER unit
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
# creates the data root, installs the USER-level systemd unit, registers with
# OpenClaw if present, and validates end-to-end. No sudo required. See
# INSTALL.md for full details.

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
SERVICE_SRC  := contrib/systemd/$(SERVICE_NAME).service
SERVICE_DST := $(HOME)/.config/systemd/user/$(SERVICE_NAME).service
SYSTEM_SERVICE_SRC := contrib/systemd/$(SERVICE_NAME).service.system
SYSTEM_SERVICE_DST := /etc/systemd/system/$(SERVICE_NAME).service

# Find go: prefer $PATH, fall back to common install locations.
# Allows `make` to work in non-interactive shells (CI, subshells) where
# ~/.bashrc isn't sourced.
GO := $(shell command -v go 2>/dev/null || echo /usr/local/go/bin/go)

VERSION     := $(shell git describe --tags 2>/dev/null || echo "dev")
BUILD_LDFLAGS := -ldflags "-X main.buildVersion=$(VERSION)"
CGO_CFLAGS := -DSQLITE_ENABLE_FTS5=1
CGO_LDFLAGS := -lm

.PHONY: all build install service-scheduler service install-system-service uninstall-service gen-cli clean test lint help

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

# Verify the canonical install location contains all four binaries.
# `make build` already writes to bin/, which IS $(PREFIX)/bin/ when the repo
# is cloned at $HOME/.mpm (the standard layout). On a non-standard layout
# (repo cloned somewhere other than $HOME/.mpm), this target copies the
# build output into the canonical location. No sudo — the canonical
# location is always user-writable.
install: build
	@echo "🚀 Verifying canonical install at $(PREFIX)/bin/..."
	@mkdir -p $(PREFIX)/bin
	@if [ "$(BUILD_DIR)" != "$(PREFIX)/bin" ] && [ ! -L "$(BUILD_DIR)" ] && [ ! -L "$(PREFIX)" ]; then \
	    install -m755 $(BUILD_DIR)/$(BINARY_NAME)    $(PREFIX)/bin/$(BINARY_NAME) || true; \
	    install -m755 $(BUILD_DIR)/$(MCP_BINARY)    $(PREFIX)/bin/$(MCP_BINARY) || true; \
	    install -m755 $(BUILD_DIR)/$(SCHED_BINARY)  $(PREFIX)/bin/$(SCHED_BINARY) || true; \
	    install -m755 $(BUILD_DIR)/$(CRITIC_BINARY) $(PREFIX)/bin/$(CRITIC_BINARY) || true; \
	    echo "    (synced bin/ to $(PREFIX)/bin/)"; \
	else \
	    echo "    (bin/ is the canonical location; no copy needed)"; \
	fi
	@echo "✓ Canonical binaries at $(PREFIX)/bin/: $(BINARY_NAME) $(MCP_BINARY) $(SCHED_BINARY) $(CRITIC_BINARY)"

# Install the mpm-scheduler systemd user service.
# The unit is templated for the standard ~/projects/mpm layout; override
# paths via:
#   1. Drop-in:  systemctl --user edit mpm-scheduler
#   2. Env file: ~/.config/mpm/mpm.env  (sourced as EnvironmentFile=-)
# mpm-mcp is intentionally NOT shipped as a systemd unit — it's spawned
# by MCP hosts (Claude Code, OpenClaw) as a stdio child process.
#
# ⚠ OPT-IN ONLY: this installs a USER-level service which silently fails
# on hosts with encrypted home directories (eCryptfs/LUKS). For the
# default SYSTEM-level install, use `sudo scripts/install.sh`.
service-scheduler:
	@echo "⚠  This target installs a USER-level systemd unit."
	@echo "   It silently fails on encrypted home directories."
	@echo "   For the default system-level install, use: sudo scripts/install.sh"
	@echo ""
	@install -Dm644 $(SERVICE_SRC) $(SERVICE_DST)
	@systemctl --user daemon-reload
	@echo "✓ Installed $(SERVICE_DST)"
	@echo ""
	@echo "  Next steps:"
	@echo "    systemctl --user enable --now $(SERVICE_NAME)"
	@echo "    systemctl --user status $(SERVICE_NAME)"
	@echo "    journalctl --user -u $(SERVICE_NAME) -f"

# Alias for the common case.
service: service-scheduler

# Remove the installed systemd user unit. Safe to run even if not installed.
uninstall-service:
	@rm -f $(SERVICE_DST)
	-@systemctl --user daemon-reload
	@echo "✓ Removed $(SERVICE_DST) (if it existed)"

# Install the SYSTEM-level systemd unit. Requires sudo.
# Use scripts/install.sh instead — it does this and more (binaries,
# wrapper, /var/lib/mpm, MCP registration, validation).
install-system-service:
	@echo "⚠  For full install use: sudo scripts/install.sh"
	@echo "   This target only installs the system unit."
	@sudo install -Dm644 $(SYSTEM_SERVICE_SRC) $(SYSTEM_SERVICE_DST)
	@sudo systemctl daemon-reload
	@echo "✓ Installed $(SYSTEM_SERVICE_DST)"
	@echo ""
	@echo "  Next steps:"
	@echo "    sudo systemctl enable --now $(SERVICE_NAME)"
	@echo "    systemctl status $(SERVICE_NAME)"

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

# Run golangci-lint (advisory only — does not gate CI).
# Install: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
lint:
	@command -v golangci-lint >/dev/null 2>&1 || { echo "golangci-lint not installed. Run: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest"; exit 1; }
	golangci-lint run ./...

# Clean build artifacts
clean:
	rm -rf $(BUILD_DIR)
	@echo "🧹 Cleaned $(BUILD_DIR)/"

# Show help
help:
	@echo "mpm Makefile"
	@echo ""
	@echo "  Canonical location: \$$HOME/.mpm/bin/ (no sudo, no /usr/local copy)"
	@echo ""
	@echo "  Targets:"
	@echo "    make build               - Build all four binaries to bin/"
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