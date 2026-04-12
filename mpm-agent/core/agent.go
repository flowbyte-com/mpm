package core

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
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

// RunAgent runs a single agent query with full identity and context.
// identityPath is the resolved path to IDENTITY.md.
// toolProfile is the list of tool names to expose to the AI.
func RunAgent(query string, history []map[string]interface{}, db *sql.DB, identityPath string, cfg *SynthConfig, toolProfile []string) (string, error) {
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

	// Build tool list from active profile (includes execute_mpm_command + local tools)
	availableTools := buildToolList(toolProfile)

	// Tool loop: call API, execute tools, repeat
	maxIterations := 5
	for iteration := 0; iteration < maxIterations; iteration++ {
		responseText, toolCalls, err := callSynthAPIWithTools(systemPrompt, messages, cfg, availableTools)
		if err != nil {
			return "", err
		}

		// If no tool calls, return the text response
		if len(toolCalls) == 0 {
			return responseText, nil
		}

		// Execute each tool call and append results to messages
		for _, tc := range toolCalls {
			var result string
			var err error
			switch tc.Name {
			case "execute_mpm_command":
				cmd, _ := tc.Input["command"].(string)
				result, err = mpmExec(cmd)
			default:
				// Local tools from core/tools.go
				result, err = executeTool(tc.Name, tc.Input)
			}
			if err != nil {
				result = fmt.Sprintf("error: %v", err)
			}
			messages = append(messages, apiMessage{
				Role:    "user",
				Content: fmt.Sprintf(`[tool result for %s]: %s`, tc.Name, result),
			})
		}
	}

	// Max iterations reached
	return "(tool loop limit reached)", nil
}

// buildToolList returns MCP tool + local tools for the given profile.
func buildToolList(profile []string) []map[string]interface{} {
	tools := []map[string]interface{}{
		{
			"name":        "execute_mpm_command",
			"description": "Execute an MPM CLI command. Pass the full command string after 'mpm'. Example: 'recall hello' runs 'mpm recall hello'.",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"command": map[string]interface{}{
						"type":        "string",
						"description": "The MPM command arguments (e.g., 'recall hello' runs 'mpm recall hello')",
					},
				},
				"required": []string{"command"},
			},
		},
	}
	// Add local tools from registry for this profile
	for _, name := range profile {
		def, ok := GetTool(name)
		if !ok {
			continue
		}
		tools = append(tools, map[string]interface{}{
			"name":        def.Name,
			"description": def.Description,
			"inputSchema": def.InputSchema,
		})
	}
	return tools
}

// callSynthAPIWithTools makes an Anthropic API call and returns response text and any tool calls.
func callSynthAPIWithTools(systemPrompt string, messages []apiMessage, cfg *SynthConfig, tools []map[string]interface{}) (string, []toolUse, error) {
	if cfg.APIKey == "" {
		return "", nil, fmt.Errorf("no API key configured")
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
		return "", nil, fmt.Errorf("marshal request: %w", err)
	}

	url := strings.TrimSuffix(cfg.BaseURL, "/") + "/v1/messages"
	httpReq, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return "", nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", cfg.APIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	timeout := time.Duration(cfg.TimeoutSecs) * time.Second
	if timeout == 0 {
		timeout = 300 * time.Second
	}
	client := &http.Client{Timeout: timeout}

	resp, err := client.Do(httpReq)
	if err != nil {
		return "", nil, fmt.Errorf("API call failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
	}

	var result anthropicResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", nil, fmt.Errorf("parse response: %w", err)
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
		}
	}

	return textResponse, toolCalls, nil
}
