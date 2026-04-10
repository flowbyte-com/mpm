BINARY_NAME := mpm-agent
BUILD_DIR   := .

.PHONY: all build install clean

all: build

build:
	go build -o $(BUILD_DIR)/$(BINARY_NAME) .

install: build
	install -Dm755 $(BINARY_NAME) /usr/local/bin/$(BINARY_NAME)

clean:
	rm -f $(BINARY_NAME)
