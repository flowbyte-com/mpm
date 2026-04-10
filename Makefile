# mpm Master Makefile
# Usage: make <target>
#
# Targets:
#   make build   - Build canonical binary to bin/mpm
#   make install - Install to $(PREFIX)/bin
#   make clean   - Remove bin/
#   make test    - Run tests
#   make help    - Show this help

BINARY_NAME := mpm
BUILD_DIR   := bin
PREFIX      ?= /usr/local

VERSION     := $(shell git describe --tags 2>/dev/null || echo "dev")
BUILD_LDFLAGS := -ldflags "-X main.buildVersion=mpm-std"

.PHONY: all build install clean test help

all: build

# Build canonical binary to bin/mpm
build:
	@mkdir -p $(BUILD_DIR)
	go build $(BUILD_LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/mpm
	@echo "🤖 Built $(BUILD_DIR)/$(BINARY_NAME) (mpm-std)"

# Install to PREFIX/bin
install: build
	@echo "🚀 Installing mpm to $(PREFIX)/bin/mpm..."
	@sudo install -Dm755 $(BUILD_DIR)/$(BINARY_NAME) $(PREFIX)/bin/mpm
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
	@echo "    make build   - Build to bin/mpm"
	@echo "    make install - Install to $(PREFIX)/bin/mpm"
	@echo "    make test    - Run go tests"
	@echo "    make clean   - Remove bin/"
	@echo "    make help    - Show this help"
	@echo ""
	@echo "  Version: $(VERSION)"
