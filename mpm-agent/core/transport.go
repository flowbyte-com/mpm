package core

import (
	"fmt"
	"strings"
)

// Transport abstracts all IO for SessionRunner.
// Each entry point (CLI, Telegram, MCP) implements this interface.
type Transport interface {
	// ReadMessage blocks waiting for the next user input line.
	// Returns ("", io.EOF) when the input stream is closed.
	ReadMessage() (string, error)

	// WriteChunk streams output. isThinking=true routes to stderr/dim.
	WriteChunk(text string, isThinking bool) error

	// RequestToolApproval blocks until user approves/rejects.
	// Returns true if approved, false if rejected.
	RequestToolApproval(toolName string, args string) bool
}

// Message represents a turn in the agent conversation.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// formatArgs formats tool arguments for HITL display.
func formatArgs(args map[string]interface{}) string {
	if args == nil {
		return "(no args)"
	}
	var parts []string
	for k, v := range args {
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}
	return strings.Join(parts, ", ")
}

// isRiskyTool returns true if a tool requires HITL approval.
func isRiskyTool(toolName string) bool {
	switch toolName {
	case "execute_shell", "write_file", "apply_diff", "execute_cmd_with_timeout":
		return true
	}
	return false
}
