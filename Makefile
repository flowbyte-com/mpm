# mpm Master Makefile
# Usage: make <target>
#
# Targets:
#   make build   - Build bin/mpm and bin/mpm-mcp
#   make install - Install both to $(PREFIX)/bin
#   make clean   - Remove bin/
#   make test    - Run tests
#   make help    - Show this help

BINARY_NAME := mpm
MCP_BINARY  := mpm-mcp
BUILD_DIR   := bin
PREFIX      ?= /usr/local

# Find go: prefer $PATH, fall back to common install locations.
# Allows `make` to work in non-interactive shells (CI, subshells) where
# ~/.bashrc isn't sourced.
GO := $(shell command -v go 2>/dev/null || echo /usr/local/go/bin/go)

VERSION     := $(shell git describe --tags 2>/dev/null || echo "dev")
BUILD_LDFLAGS := -ldflags "-X main.buildVersion=mpm-std"
CGO_CFLAGS := -DSQLITE_ENABLE_FTS5=1

.PHONY: all build install clean test help

all: build

# Build canonical binaries to bin/mpm and bin/mpm-mcp
# Requires CGO for mattn/go-sqlite3 with FTS5 support
build:
	@mkdir -p $(BUILD_DIR)
	CGO_CFLAGS=$(CGO_CFLAGS) $(GO) build -tags fts5 $(BUILD_LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/mpm
	CGO_CFLAGS=$(CGO_CFLAGS) $(GO) build -tags fts5 $(BUILD_LDFLAGS) -o $(BUILD_DIR)/$(MCP_BINARY) ./cmd/mpm-mcp
	@echo "🤖 Built $(BUILD_DIR)/$(BINARY_NAME) and $(BUILD_DIR)/$(MCP_BINARY) (mpm-std)"

# Install both binaries to PREFIX/bin
install: build
	@echo "🚀 Installing mpm and mpm-mcp to $(PREFIX)/bin/..."
	@sudo install -Dm755 $(BUILD_DIR)/$(BINARY_NAME) $(PREFIX)/bin/$(BINARY_NAME)
	@sudo install -Dm755 $(BUILD_DIR)/$(MCP_BINARY)  $(PREFIX)/bin/$(MCP_BINARY)
	@echo "✓ Installation complete!"

# Run tests
test:
	CGO_CFLAGS=$(CGO_CFLAGS) $(GO) test -tags fts5 -v ./...

# Clean build artifacts
clean:
	rm -rf $(BUILD_DIR)
	@echo "🧹 Cleaned $(BUILD_DIR)/"

# Show help
help:
	@echo "mpm Makefile"
	@echo ""
	@echo "  Targets:"
	@echo "    make build   - Build bin/mpm and bin/mpm-mcp"
	@echo "    make install - Install both to \$$(PREFIX)/bin/"
	@echo "    make test    - Run go tests"
	@echo "    make clean   - Remove bin/"
	@echo "    make help    - Show this help"
	@echo ""
	@echo "  Version: $(VERSION)"
