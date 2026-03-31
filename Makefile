# mpm Master Makefile
# Usage: make <target>
#
# Targets:
#   make 808     - 808 build (full flavor with lobster, fortunes, achievements)
#   make dev     - Quick dev build (outputs to bin/mpm)
#   make install  - Install to $(PREFIX)/bin
#   make clean    - Remove bin/
#   make test     - Run tests
#   make help     - Show this help

BINARY_NAME := mpm
BUILD_DIR   := bin
PREFIX      ?= /usr/local

# Git-based version (falls back to "dev" if not a git repo)
VERSION     := $(shell git describe --tags 2>/dev/null || echo "dev")
BUILD_FLAVOR_808 := -ldflags "-X main.buildVersion=808-$(VERSION)"

.PHONY: all 808 dev install clean test help

all: 808 dev

# 808 Build (The_Great_808 flavor - full lobster experience)
808:
	@mkdir -p $(BUILD_DIR)
	go build $(BUILD_FLAVOR_808) -tags "808" -o $(BUILD_DIR)/$(BINARY_NAME)-808 ./cmd/mpm
	@echo "🤖 Built $(BUILD_DIR)/$(BINARY_NAME)-808 (808 edition)"

# Dev build - quick rebuild to bin/mpm
dev:
	@mkdir -p $(BUILD_DIR)
	go build $(BUILD_FLAVOR_808) -tags "808" -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/mpm
	@echo "🔧 Dev build ready at $(BUILD_DIR)/$(BINARY_NAME)"

# Install
install: 808
	@echo "🚀 Installing mpm-808 to $(PREFIX)/bin/mpm..."
	@sudo install -Dm755 $(BUILD_DIR)/$(BINARY_NAME)-808 $(PREFIX)/bin/mpm
	@echo "✓ Installation complete!"

# Run tests
test:
	go test -v ./...

# Clean build artifacts
clean:
	rm -rf $(BUILD_DIR)
	@echo "🧹 Cleaned $(BUILD_DIR)/"

# Show help
help:
	@echo "mpm Makefile"
	@echo ""
	@echo "  Targets:"
	@echo "    make 808     - 808 build (full flavor)"
	@echo "    make dev     - Quick dev build to bin/mpm"
	@echo "    make install - Install to $(PREFIX)/bin (requires sudo)"
	@echo "    make test    - Run go tests"
	@echo "    make clean   - Remove bin/"
	@echo "    make help    - Show this help"
	@echo ""
	@echo "  Version: $(VERSION)"
