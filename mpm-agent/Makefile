.PHONY: all build install clean test-core

BIN := bin
BINS := $(BIN)/mini-bot $(BIN)/mini-bot-telegram $(BIN)/mini-bot-mcp

all: build

build: $(BINS)

$(BIN):
	mkdir -p $(BIN)

$(BIN)/mini-bot: cmd/mini-bot/main.go cmd/mini-bot/repl.go core/*.go | $(BIN)
	go build -o $@ ./cmd/mini-bot

$(BIN)/mini-bot-telegram: cmd/telegram/*.go core/*.go | $(BIN)
	go build -o $@ ./cmd/telegram

$(BIN)/mini-bot-mcp: cmd/mcp/main.go core/*.go | $(BIN)
	go build -o $@ ./cmd/mcp

install: $(BINS)
	install -Dm755 $(BIN)/mini-bot /usr/local/bin/mini-bot
	install -Dm755 $(BIN)/mini-bot-telegram /usr/local/bin/mini-bot-telegram
	install -Dm755 $(BIN)/mini-bot-mcp /usr/local/bin/mini-bot-mcp

clean:
	rm -rf $(BIN)

test-core:
	CGO_CFLAGS="-DSQLITE_ENABLE_FTS5" CGO_LDFLAGS="-lm" go test ./core/...
