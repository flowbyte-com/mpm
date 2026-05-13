package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"mpm-agent/core"
)

var _ core.Transport = (*TerminalTransport)(nil)

type TerminalTransport struct {
	reader *bufio.Reader
}

func NewTerminalTransport() *TerminalTransport {
	return &TerminalTransport{
		reader: bufio.NewReader(os.Stdin),
	}
}

func (t *TerminalTransport) ReadMessage() (string, error) {
	line, err := t.reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(line, "\n"), nil
}

func (t *TerminalTransport) WriteChunk(text string, isThinking bool) error {
	if isThinking {
		fmt.Fprintf(os.Stderr, "\033[2m%s\033[0m", text)
	} else {
		fmt.Fprint(os.Stdout, text)
	}
	return nil
}

func (t *TerminalTransport) RequestToolApproval(toolName string, args string) bool {
	fmt.Fprintf(os.Stderr, "\033[33m⚠️  Tool: %s\033[0m\nArgs: %s\n[Y/n] ", toolName, args)
	line, err := t.reader.ReadString('\n')
	if err != nil {
		return false
	}
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	return line != "n"
}
