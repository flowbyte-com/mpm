BINARY_NAME := mpm-agent
BUILD_DIR   := .

.PHONY: all build install clean test-core

all: build

build:
	go build -o $(BUILD_DIR)/$(BINARY_NAME) .

# mini-bot build targets
mini-bot: cmd/mini-bot/main.go core/*.go
	go build -o bin/mini-bot ./cmd/mini-bot

mini-bot-telegram: cmd/telegram/*.go core/*.go
	go build -o bin/mini-bot-telegram ./cmd/telegram

mini-bot-mcp: cmd/mcp/*.go core/*.go
	go build -o bin/mini-bot-mcp ./cmd/mcp

test-core:
	CGO_CFLAGS="-DSQLITE_ENABLE_FTS5" CGO_LDFLAGS="-lm" go test ./core/...

install: build
	install -Dm755 $(BINARY_NAME) /usr/local/bin/$(BINARY_NAME)

clean:
	rm -f $(BINARY_NAME)
