# mpm Master Makefile
# Usage: make <target>
#
# Targets:
#   make build               - Build bin/mpm, bin/mpm-mcp, bin/mpm-scheduler, bin/mpm-critic
#   make install             - Install all four to $(PREFIX)/bin (system-wide, requires sudo)
#   make service-scheduler   - [LEGACY/OPT-IN] Install mpm-scheduler systemd USER unit
#                              (fails on encrypted home dirs — use scripts/install.sh instead)
#   make service             - Alias for service-scheduler
#   make clean               - Remove bin/
#   make test                - Run tests
#   make lint                - Run golangci-lint (advisory; not CI-gated)
#   make help                - Show this help
#
# RECOMMENDED INSTALL PATH:
#   sudo scripts/install.sh
# This single command builds, installs binaries + wrapper, creates /var/lib/mpm,
# installs the SYSTEM-level systemd unit, registers with OpenClaw if present,
# and validates end-to-end. See INSTALL.md for full details.

BINARY_NAME := mpm
MCP_BINARY  := mpm-mcp
SCHED_BINARY := mpm-scheduler
CRITIC_BINARY := mpm-critic
BUILD_DIR   := bin
PREFIX      ?= /usr/local
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
	@echo "🤖 Built $(BUILD_DIR)/$(BINARY_NAME), $(BUILD_DIR)/$(MCP_BINARY), $(BUILD_DIR)/$(SCHED_BINARY), and $(BUILD_DIR)/$(CRITIC_BINARY) (mpm-std)"

# Install all four binaries to PREFIX/bin (system-wide, requires sudo).
install: build
	@echo "🚀 Installing mpm, mpm-mcp, mpm-scheduler, and mpm-critic to $(PREFIX)/bin/..."
	@sudo install -Dm755 $(BUILD_DIR)/$(BINARY_NAME)    $(PREFIX)/bin/$(BINARY_NAME)
	@sudo install -Dm755 $(BUILD_DIR)/$(MCP_BINARY)    $(PREFIX)/bin/$(MCP_BINARY)
	@sudo install -Dm755 $(BUILD_DIR)/$(SCHED_BINARY)  $(PREFIX)/bin/$(SCHED_BINARY)
	@sudo install -Dm755 $(BUILD_DIR)/$(CRITIC_BINARY) $(PREFIX)/bin/$(CRITIC_BINARY)
	@echo "✓ Installation complete!"

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
	CGO_CFLAGS=$(CGO_CFLAGS) $(GO) test -tags fts5 -v ./cmd/...
	cd internal/core && CGO_CFLAGS=$(CGO_CFLAGS) $(GO) test -tags fts5 -v ./...
	CGO_CFLAGS=$(CGO_CFLAGS) $(GO) test -tags fts5 -v ./internal/scheduler/...

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
	@echo "  Targets:"
	@echo "    make build               - Build all four binaries"
	@echo "    make install             - Install all four to \$$(PREFIX)/bin/"
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