package core

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// ============================================================================
// Constants
// ============================================================================

const (
	maxHistoryMessages = 50
	maxIterations      = 10
)

// ============================================================================
// System Prompt Builder with Identity-First Approach
// ============================================================================

// BuildSystemPromptWithIdentity builds the full system prompt.
// Priority: IDENTITY.md (core, stable) + persona (costume overlay) + mode (behavior rules)
func BuildSystemPromptWithIdentity(identityPath, personaContent, modeContent string, memories, directives, references []string, anchors []Anchor) string {
	identity, _ := LoadIdentity(identityPath)

	var sb strings.Builder

	// Core identity from IDENTITY.md
	if identity != nil {
		sb.WriteString(fmt.Sprintf("You are %s (v%s) — %s. ", identity.Name, identity.Version, identity.Type))
		if identity.Traits != "" {
			sb.WriteString(fmt.Sprintf("Core traits: %s. ", identity.Traits))
		}
		if identity.Boundaries != "" {
			sb.WriteString(fmt.Sprintf("Boundaries: %s. ", identity.Boundaries))
		}
	} else {
		sb.WriteString("You are mini-bot. ")
	}

	// Anchors (high-priority memories)
	if len(anchors) > 0 {
		sb.WriteString("\n\n## Anchored Memories\n")
		for _, a := range anchors {
			sb.WriteString(fmt.Sprintf("- [weight:%d] %s\n", a.Weight, a.Content))
		}
	}

	// Persona costume overlay (MPM personas)
	if personaContent != "" {
		sb.WriteString(fmt.Sprintf("\n\n[Persona Costume] %s", personaContent))
	}

	// Mode behavior rules
	if modeContent != "" {
		sb.WriteString(fmt.Sprintf("\n\n[Mode] %s", modeContent))
	}

	// Memory context
	if len(memories) > 0 {
		sb.WriteString("\n\n## Relevant Memories\n")
		for _, m := range memories {
			sb.WriteString(fmt.Sprintf("- %s\n", m))
		}
	}

	// Directives
	if len(directives) > 0 {
		sb.WriteString("\n## Prime Directives\n")
		for _, d := range directives {
			sb.WriteString(fmt.Sprintf("- %s\n", d))
		}
	}

	// References
	if len(references) > 0 {
		sb.WriteString("\n## Reference Material\n")
		for _, r := range references {
			sb.WriteString(fmt.Sprintf("- %s\n", r))
		}
	}

	sb.WriteString(fmt.Sprintf("\n## Current Date: %s", time.Now().Format("2006-01-02")))
	return sb.String()
}

// retrieveMemories returns formatted relevant memories from MPM DB
func retrieveMemories(db *sql.DB, query string, limit int) []string {
	if query == "" {
		return nil
	}
	rows, err := db.Query(`
		SELECT content, tags FROM memories
		JOIN memories_fts fts ON memories.rowid = fts.rowid
		WHERE memories_fts MATCH ? AND deleted_at IS NULL
		ORDER BY rank LIMIT ?
	`, query, limit)
	if err != nil {
		like := "%" + query + "%"
		rows, err = db.Query(`
			SELECT content, tags FROM memories
			WHERE (content LIKE ? OR tags LIKE ?) AND deleted_at IS NULL
			ORDER BY created_at DESC LIMIT ?
		`, like, like, limit)
		if err != nil {
			return nil
		}
	}
	defer rows.Close()
	var results []string
	for rows.Next() {
		var content, tags string
		if err := rows.Scan(&content, &tags); err != nil {
			continue
		}
		results = append(results, content)
	}
	return results
}

// retrieveDirectives returns all prime directives
func retrieveDirectives(db *sql.DB) []string {
	rows, err := db.Query(`
		SELECT content FROM memories
		WHERE metadata LIKE '%is_prime_directive%' AND deleted_at IS NULL
		ORDER BY created_at DESC LIMIT 20
	`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var results []string
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			continue
		}
		results = append(results, content)
	}
	return results
}

// retrieveReferences returns formatted reference chunks
func retrieveReferences(db *sql.DB, query string, limit int) []string {
	if query == "" {
		return nil
	}
	rows, err := db.Query(`
		SELECT r.title, rc.content FROM reference_chunks rc
		JOIN reference_docs r ON rc.doc_id = r.id
		JOIN reference_chunks_fts fts ON rc.rowid = fts.rowid
		WHERE reference_chunks_fts MATCH ?
		ORDER BY rank LIMIT ?
	`, query, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var results []string
	for rows.Next() {
		var title, content string
		if err := rows.Scan(&title, &content); err != nil {
			continue
		}
		results = append(results, fmt.Sprintf("### %s\n%s", title, content))
	}
	return results
}

// apiMessage is a chat message for the Anthropic API.
type apiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// toolUse represents a tool call from the API.
type toolUse struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Input map[string]interface{} `json:"input"`
}

// mpmExec runs an MPM command and returns stdout.
func mpmExec(cmd string) (string, error) {
	args := strings.Fields(cmd)
	if len(args) == 0 {
		return "", fmt.Errorf("empty command")
	}
	cmdExec := exec.Command("mpm", args...)
	cmdExec.Env = append(os.Environ(), "MPM_WORKSPACE="+os.Getenv("MPM_WORKSPACE"))
	out, err := cmdExec.CombinedOutput()
	return string(out), err
}

// ToolProgressReporter receives tool execution events for streaming to UI.
// Nil reporter means no streaming — fully backward-compatible.
type ToolProgressReporter interface {
	// ToolStarted is called before executing a tool. Returns a reportID
	// used to correlate ToolStarted/ToolCompleted calls.
	ToolStarted(chatID int64, toolName string, input map[string]interface{}) string
	// ToolCompleted is called after a tool finishes with a one-line summary.
	ToolCompleted(chatID int64, reportID string, toolName string, summary string)
	// SendAlert posts a standalone risk warning to the chat (bypasses streaming).
	SendAlert(chatID int64, message string)
}

// TokenUsageReporter receives per-call token usage for session-level tracking.
type TokenUsageReporter interface {
	// ReportUsage is called after each successful API call with usage data.
	ReportUsage(chatID int64, inputTokens, outputTokens int, model string)
}

// TokenTotals holds running token totals for a session.
type TokenTotals struct {
	InputTokens  int
	OutputTokens int
	Calls        int
}

// summarize produces a one-line summary of a tool result.
func summarize(toolName string, result string, err error) string {
	if err != nil {
		return fmt.Sprintf("✗ %s: %v", toolName, err)
	}
	switch toolName {
	case "read_file":
		lines := strings.Count(result, "\n") + 1
		return fmt.Sprintf("✓ read %d lines", lines)
	case "write_file":
		return "✓ wrote"
	case "execute_shell":
		lines := strings.Count(result, "\n") + 1
		return fmt.Sprintf("✓ %d lines output", lines)
	}
	return truncate(result, 60)
}

// RunAgent runs a single agent query with full identity and context.
// ctx is the parent context (90s timeout from handler).
// identityPath is the resolved path to IDENTITY.md.
// toolProfile is the list of base tool names (framework tools).
// sessionID is used to scope LoadedToolkits per conversation.
// toolkitMap maps toolkit names to tool names (from config).
func RunAgent(ctx context.Context, query string, history []map[string]interface{}, db *sql.DB, identityPath string, cfg *SynthConfig, toolProfile []string, sessionID string, toolkitMap map[string][]string, chatID int64, reporter ToolProgressReporter, tokenReporter TokenUsageReporter) (string, error) {
	// Get anchors as high-priority context
	anchors, _ := GetRecentAnchors(db, 5)

	// Build context arrays
	memories := retrieveMemories(db, query, 5)
	directives := retrieveDirectives(db)
	references := retrieveReferences(db, query, 3)

	// Build system prompt with identity-first approach
	systemPrompt := BuildSystemPromptWithIdentity(identityPath, "", "",
		memories, directives, references, anchors)

	// Build messages array: history + current user message
	messages := make([]apiMessage, 0, len(history)+1)
	for _, h := range history {
		role, _ := h["role"].(string)
		content, _ := h["content"].(string)
		if role == "" {
			role = "user"
		}
		messages = append(messages, apiMessage{Role: role, Content: content})
	}
	messages = append(messages, apiMessage{Role: "user", Content: query})

	// Tool loop: call API, execute tools, repeat
	for iteration := 0; iteration < maxIterations; iteration++ {
		// Rebuild tool list: base tools + loaded toolkit tools (dynamic)
		availableTools := buildToolListWithLoaded(toolProfile, sessionID, toolkitMap)

		responseText, toolCalls, usage, err := callSynthAPIWithTools(ctx, systemPrompt, messages, cfg, availableTools)
		if err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				return "⚠️ Request timed out (90s). Try a simpler query.", nil
			}
			return "", err
		}
		if tokenReporter != nil {
			tokenReporter.ReportUsage(chatID, usage.InputTokens, usage.OutputTokens, usage.Model)
		}

		// If no tool calls, return the text response
		if len(toolCalls) == 0 {
			return responseText, nil
		}

		// Execute each tool call and append results to messages
		for _, tc := range toolCalls {
			var result string
			var err error

			// Framework tools — intercepted here, not executed
			switch tc.Name {
			case "load_toolkit":
				name, _ := tc.Input["name"].(string)
				if name == "" {
					result = "error: name is required"
				} else {
					LoadToolkit(sessionID, name)
					result = fmt.Sprintf("Toolkit '%s' loaded. You now have access to: %v",
						name, toolkitMap[name])
				}
				// Continue loop with updated availableTools
				messages = append(messages, apiMessage{
					Role:    "user",
					Content: fmt.Sprintf("[%s result]: %s", tc.Name, result),
				})
				continue
			case "unload_toolkit":
				name, _ := tc.Input["name"].(string)
				if name == "" {
					result = "error: name is required"
				} else {
					UnloadToolkit(sessionID, name)
					result = fmt.Sprintf("Toolkit '%s' unloaded.", name)
				}
				messages = append(messages, apiMessage{
					Role:    "user",
					Content: fmt.Sprintf("[%s result]: %s", tc.Name, result),
				})
				continue
			case "execute_mpm_command":
				reportID := ""
				if reporter != nil {
					reportID = reporter.ToolStarted(chatID, "execute_mpm_command", tc.Input)
				}
				cmd, _ := tc.Input["command"].(string)
				result, err = mpmExec(cmd)
				if reporter != nil {
					summary := summarize("execute_mpm_command", result, err)
					reporter.ToolCompleted(chatID, reportID, "execute_mpm_command", summary)
				}
			default:
				// Local tools from core/tools.go
				reportID := ""
				if reporter != nil {
					reportID = reporter.ToolStarted(chatID, tc.Name, tc.Input)
				}
				result, err = executeTool(tc.Name, tc.Input, sessionID)
				if reporter != nil {
					summary := summarize(tc.Name, result, err)
					reporter.ToolCompleted(chatID, reportID, tc.Name, summary)
					if isRiskyOperation(tc.Name, tc.Input, result, err) {
						reporter.SendAlert(chatID, riskWarning(tc.Name, tc.Input, result))
					}
				}
			}

			if err != nil {
				result = fmt.Sprintf("error: %v", err)
			}
			messages = append(messages, apiMessage{
				Role:    "user",
				Content: fmt.Sprintf("[%s result]: %s", tc.Name, result),
			})
		}

		// Check: did we make tool calls without returning text? (loop breaker)
		if iteration == maxIterations-1 && len(toolCalls) > 0 {
			return "⚠️ Loop terminated: Exceeded max reasoning steps.", nil
		}
	}

	// Max iterations reached (should not reach here due to breaker above)
	return "(tool loop limit reached)", nil
}

var riskyCommandPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\brm\s+-(rf|r)\b`),
	regexp.MustCompile(`(?i)git\s+push\s+.*--force`),
	regexp.MustCompile(`(?i)\bdd\b.*\bof=`),
	regexp.MustCompile(`(?i)(mkfs|shred|wipe)\s`),
	regexp.MustCompile(`(?i)(chmod|chown)\s+777`),
	regexp.MustCompile(`(?i)sudo\s+rm\s+`),
	regexp.MustCompile(`(?i):\(\)\{.*:\|.*:\}`), // fork bomb
}

var riskyWritePaths = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(^|/)(\.ssh|aws|credentials|secrets|env)($|/)`),
	regexp.MustCompile(`(?i)/etc/|/sys/|/proc/`),
}

func isRiskyOperation(toolName string, input map[string]interface{}, result string, err error) bool {
	switch toolName {
	case "execute_shell":
		cmd, _ := input["command"].(string)
		for _, re := range riskyCommandPatterns {
			if re.MatchString(cmd) {
				return true
			}
		}
	case "write_file":
		path, _ := input["path"].(string)
		for _, re := range riskyWritePaths {
			if re.MatchString(path) {
				return true
			}
		}
	}
	return false
}

func riskWarning(toolName string, input map[string]interface{}, result string) string {
	switch toolName {
	case "execute_shell":
		cmd, _ := input["command"].(string)
		return fmt.Sprintf("⚠️ DANGEROUS: execute_shell running `%s`", truncate(cmd, 100))
	case "write_file":
		path, _ := input["path"].(string)
		return fmt.Sprintf("⚠️ RISKY WRITE: writing to %s", path)
	}
	return fmt.Sprintf("⚠️ RISKY: %s", toolName)
}

// buildToolListWithLoaded returns base tools + dynamically loaded toolkit tools.
func buildToolListWithLoaded(baseTools []string, sessionID string, toolkitMap map[string][]string) []map[string]interface{} {
	var tools []map[string]interface{}

	// Always include execute_mpm_command
	tools = append(tools, map[string]interface{}{
		"name":        "execute_mpm_command",
		"description": "Execute an MPM CLI command. Pass the full command string after 'mpm'. Example: 'recall hello' runs 'mpm recall hello'.",
		"input_schema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"command": map[string]interface{}{
					"type":        "string",
					"description": "The MPM command arguments (e.g., 'recall hello' runs 'mpm recall hello')",
				},
			},
			"required": []string{"command"},
		},
	})

	// Add base framework tools (list_toolkits, load_toolkit, unload_toolkit)
	for _, name := range baseTools {
		def, ok := GetTool(name)
		if !ok || name == "execute_mpm_command" {
			continue
		}
		tools = append(tools, map[string]interface{}{
			"name":        def.Name,
			"description": def.Description,
			"input_schema": def.InputSchema,
		})
	}

	// Add tools from loaded toolkits
	loaded := GetLoadedToolkits(sessionID)
	for _, tkName := range loaded {
		tkTools, ok := toolkitMap[tkName]
		if !ok {
			continue
		}
		for _, toolName := range tkTools {
			def, ok := GetTool(toolName)
			if !ok {
				continue
			}
			// Avoid duplicates
			exists := false
			for _, t := range tools {
				if t["name"] == toolName {
					exists = true
					break
				}
			}
			if !exists {
				tools = append(tools, map[string]interface{}{
					"name":        def.Name,
					"description": def.Description,
					"input_schema": def.InputSchema,
				})
			}
		}
	}

	return tools
}

// APIUsage holds token usage from a single API call.
type APIUsage struct {
	InputTokens  int
	OutputTokens int
	Model        string
}

// callSynthAPIWithTools makes an Anthropic API call and returns response text, tool calls, and usage.
func callSynthAPIWithTools(ctx context.Context, systemPrompt string, messages []apiMessage, cfg *SynthConfig, tools []map[string]interface{}) (string, []toolUse, APIUsage, error) {
	if cfg.APIKey == "" {
		return "", nil, APIUsage{}, fmt.Errorf("no API key configured")
	}

	type anthropicRequest struct {
		Model     string                    `json:"model"`
		MaxTokens int                       `json:"max_tokens"`
		System    string                    `json:"system"`
		Messages  []apiMessage              `json:"messages"`
		Tools     []map[string]interface{} `json:"tools,omitempty"`
	}

	type anthropicResponse struct {
		Type        string `json:"type"`
		Content     []struct {
			Type  string                 `json:"type"`
			Text  string                 `json:"text"`
			Name  string                 `json:"name"`
			ID    string                 `json:"id"`
			Input map[string]interface{} `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage       struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}

	reqBody := anthropicRequest{
		Model:     cfg.Model,
		MaxTokens: cfg.MaxTokens,
		System:    systemPrompt,
		Messages:  messages,
		Tools:     tools,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", nil, APIUsage{}, fmt.Errorf("marshal request: %w", err)
	}

	url := strings.TrimSuffix(cfg.BaseURL, "/") + "/v1/messages"
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return "", nil, APIUsage{}, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", cfg.APIKey)

	client := &http.Client{Transport: &http.Transport{}}

	resp, err := client.Do(httpReq)
	if err != nil {
		return "", nil, APIUsage{}, fmt.Errorf("API call failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, APIUsage{}, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", nil, APIUsage{}, fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
	}

	var result anthropicResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", nil, APIUsage{}, fmt.Errorf("parse response: %w", err)
	}

	// Check for top-level error type (e.g., MiniMax returns {"type":"error","error":...})
	if result.Type == "error" {
		var errResp struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(respBody, &errResp); err == nil && errResp.Error.Message != "" {
			return "", nil, APIUsage{}, fmt.Errorf("API error: %s", errResp.Error.Message)
		}
		return "", nil, APIUsage{}, fmt.Errorf("API error: %s", string(respBody))
	}

	// Collect text response and tool calls
	var textResponse string
	var toolCalls []toolUse
	for _, block := range result.Content {
		if block.Type == "text" && block.Text != "" {
			textResponse = block.Text
		} else if block.Type == "tool_use" {
			toolCalls = append(toolCalls, toolUse{
				Type:  block.Type,
				Name:  block.Name,
				Input: block.Input,
			})
		} else if block.Type == "error" {
			// Error block from API — surface it as an error
			return "", nil, APIUsage{}, fmt.Errorf("API error: %s", block.Text)
		}
	}

	log.Printf("[agent] API usage: input=%d output=%d (model=%s)", result.Usage.InputTokens, result.Usage.OutputTokens, cfg.Model)

	return textResponse, toolCalls, APIUsage{InputTokens: result.Usage.InputTokens, OutputTokens: result.Usage.OutputTokens, Model: cfg.Model}, nil
}
