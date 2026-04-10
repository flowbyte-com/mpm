// mpm-agent — MPM companion agent
// MIT License
package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// ============================================================================
// Config & Paths
// ============================================================================

// Config from mpm_config.json (synth section)
type Config struct {
	Model        string `json:"model"`
	APIKey      string `json:"api_key"`
	BaseURL     string `json:"base_url"`
	MaxTokens   int    `json:"max_tokens"`
	TimeoutSecs int    `json:"timeout_seconds"`
}

// MPM paths — resolved same way as MPM itself
var (
	configPath = resolvePath("mpm_config.json", "mpm_config.json")
	dbPath     = resolvePath("src/db/mpm.db", "src/db/mpm.db")
)

// resolvePath resolves a path: MPM_WORKSPACE env var → executable-relative → CWD
func resolvePath(envKey, defaultRel string) string {
	if ws := os.Getenv("MPM_WORKSPACE"); ws != "" {
		return ws + "/flowbyte/mpm/" + defaultRel
	}
	exec, err := os.Executable()
	if err == nil {
		dir := exec
		for i := 0; i < 4; i++ {
			dir = dir[:len(dir)-len("/"+trimDir(dir))]
			if len(dir) == 0 {
				break
			}
			if trimDir(dir) == "mpm" {
				return dir + "/" + defaultRel
			}
		}
	}
	cwd, _ := os.Getwd()
	return cwd + "/" + defaultRel
}

func trimDir(p string) string {
	i := len(p) - 1
	for i > 0 && p[i] == '/' {
		i--
	}
	j := i
	for j > 0 && p[j] != '/' {
		j--
	}
	return p[j+1 : i+1]
}

// LoadConfig reads mpm_config.json and returns the synth config
func LoadConfig() (*Config, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("cannot read config at %s: %w", configPath, err)
	}
	var raw struct {
		Synth *Config `json:"synth"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	if raw.Synth == nil {
		return &Config{}, nil // all defaults
	}
	// Fallbacks from env
	if raw.Synth.APIKey == "" {
		raw.Synth.APIKey = os.Getenv("MINIMAX_API_KEY")
	}
	if raw.Synth.BaseURL == "" {
		if os.Getenv("MINIMAX_BASE_URL") != "" {
			raw.Synth.BaseURL = os.Getenv("MINIMAX_BASE_URL")
		} else {
			raw.Synth.BaseURL = "https://api.minimax.io/anthropic"
		}
	}
	if raw.Synth.Model == "" {
		raw.Synth.Model = "MiniMax-M2.7"
	}
	if raw.Synth.MaxTokens == 0 {
		raw.Synth.MaxTokens = 1024
	}
	if raw.Synth.TimeoutSecs == 0 {
		raw.Synth.TimeoutSecs = 300
	}
	return raw.Synth, nil
}

// ============================================================================
// Database
// ============================================================================

// OpenDB opens the MPM database (read-write for tool execution).
func OpenDB() (*sql.DB, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("cannot open mpm.db at %s: %w", dbPath, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("cannot connect to mpm.db at %s: %w", dbPath, err)
	}
	return db, nil
}

// ============================================================================
// LLM Client
// ============================================================================

// ToolCall represents a tool call from the LLM
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string
}

// LLMResponse from the model
type LLMResponse struct {
	ID      string   `json:"id"`
	Choices []Choice `json:"choices"`
}

type Choice struct {
	Delta        Delta      `json:"delta"`
	FinishReason string     `json:"finish_reason"`
	Message      *LLMMessage `json:"message,omitempty"`
}

type Delta struct {
	Content string `json:"content"`
}

type LLMMessage struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

const (
	modelDefault   = "MiniMax-M2.7"
	baseURLDefault = "https://api.minimax.io/anthropic"
)

// LLMCall makes a streaming LLM call.
// On success, calls onToken for each chunk and onComplete when done.
// Returns (response text, error).
func LLMCall(cfg *Config, messages []map[string]interface{}, tools []map[string]interface{}, streaming bool, onToken func(string), onComplete func(*LLMResponse)) (string, error) {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = baseURLDefault
	}
	model := cfg.Model
	if model == "" {
		model = modelDefault
	}
	maxTokens := cfg.MaxTokens
	if maxTokens == 0 {
		maxTokens = 1024
	}

	payload := map[string]interface{}{
		"model":    model,
		"max_tokens": maxTokens,
		"messages": messages,
	}
	if len(tools) > 0 {
		payload["tools"] = tools
		payload["tool_choice"] = "auto"
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	if streaming {
		req.Header.Set("Accept", "text/event-stream")
	}

	timeout := 300 * time.Second
	if cfg.TimeoutSecs > 0 {
		timeout = time.Duration(cfg.TimeoutSecs) * time.Second
	}

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("LLM request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("LLM returned %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var fullContent strings.Builder
	reader := resp.Body

	if !streaming {
		// Batch mode: read all, parse once
		var llmResp LLMResponse
		bodyBytes, _ := io.ReadAll(resp.Body)
		if err := json.Unmarshal(bodyBytes, &llmResp); err != nil {
			return "", fmt.Errorf("parse LLM response: %w", err)
		}
		onComplete(&llmResp)
		if len(llmResp.Choices) > 0 && llmResp.Choices[0].Message != nil {
			return llmResp.Choices[0].Message.Content, nil
		}
		return "", nil
	}

	// Streaming mode: read SSE line by line
	buf := make([]byte, 0, 4096)
	for {
		chunk := make([]byte, 1024)
		n, err := reader.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			// Process complete lines from buf
			for {
				line := ""
				i := bytes.Index(buf, []byte("\n"))
				if i < 0 {
					break
				}
				line = string(buf[:i])
				buf = buf[i+1:]
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				data := strings.TrimPrefix(line, "data: ")
				if data == "[DONE]" {
					break
				}
				var event LLMResponse
				if err := json.Unmarshal([]byte(data), &event); err != nil {
					continue
				}
				if len(event.Choices) > 0 && event.Choices[0].Delta.Content != "" {
					token := event.Choices[0].Delta.Content
					fullContent.WriteString(token)
					if onToken != nil {
						onToken(token)
					}
				}
				if len(event.Choices) > 0 && event.Choices[0].FinishReason != "" {
					onComplete(&event)
				}
			}
		}
		if err != nil {
			break
		}
		if n == 0 {
			break
		}
	}
	return fullContent.String(), nil
}

// ============================================================================
// Tools
// ============================================================================

// ToolHandler is a function that executes a tool with given arguments
type ToolHandler func(args map[string]interface{}, db *sql.DB) (string, error)

// toolDefinitions is the OpenAI-compatible tool schema
var toolDefinitions = []map[string]interface{}{
	// Filesystem
	{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "read_file",
			"description": "Read the contents of a file. Returns the full file content.",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{"type": "string", "description": "Absolute path to the file"},
				},
				"required": []string{"path"},
			},
		},
	},
	{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "write_file",
			"description": "Write content to a file at the given path. Creates or overwrites.",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path":    map[string]interface{}{"type": "string", "description": "Absolute path to the file"},
					"content": map[string]interface{}{"type": "string", "description": "Content to write"},
				},
				"required": []string{"path", "content"},
			},
		},
	},
	// Shell
	{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "shell",
			"description": "Execute a shell command and return stdout+stderr.",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"command": map[string]interface{}{"type": "string", "description": "Shell command to execute"},
				},
				"required": []string{"command"},
			},
		},
	},
	// Web
	{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "web_search",
			"description": "Search the web for a query and return top results with snippets.",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"query": map[string]interface{}{"type": "string", "description": "Search query"},
				},
				"required": []string{"query"},
			},
		},
	},
	{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "web_fetch",
			"description": "Fetch the content of a URL.",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"url": map[string]interface{}{"type": "string", "description": "URL to fetch"},
				},
				"required": []string{"url"},
			},
		},
	},
	// MPM Read
	{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "mpm_memory_search",
			"description": "Search MPM's long-term memory using FTS5 full-text search.",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"query": map[string]interface{}{"type": "string", "description": "Search query"},
					"limit": map[string]interface{}{"type": "integer", "description": "Max results (default 5)"},
				},
				"required": []string{"query"},
			},
		},
	},
	{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "mpm_lesson_search",
			"description": "Search MPM's lessons (warnings, practices, insights).",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"query": map[string]interface{}{"type": "string", "description": "Search query"},
				},
				"required": []string{"query"},
			},
		},
	},
	{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "mpm_directive_list",
			"description": "List all prime directives stored in MPM.",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{},
			},
		},
	},
	{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "mpm_reference_search",
			"description": "Search MPM's reference document library.",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"query": map[string]interface{}{"type": "string", "description": "Search query"},
					"limit": map[string]interface{}{"type": "integer", "description": "Max results (default 3)"},
				},
				"required": []string{"query"},
			},
		},
	},
	{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "mpm_mode_list",
			"description": "List all available MPM modes.",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{},
			},
		},
	},
	{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "mpm_persona_list",
			"description": "List all available MPM personas.",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{},
			},
		},
	},
	// MPM Write
	{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "mpm_lesson_add",
			"description": "Add a lesson to MPM's memory.",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"content": map[string]interface{}{"type": "string", "description": "Lesson content"},
					"type":    map[string]interface{}{"type": "string", "description": "Type: warning, practice, or insight"},
					"tags":    map[string]interface{}{"type": "string", "description": "Comma-separated tags"},
				},
				"required": []string{"content"},
			},
		},
	},
	{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "mpm_mode_set",
			"description": "Set the active MPM mode.",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"mode": map[string]interface{}{"type": "string", "description": "Mode name"},
				},
				"required": []string{"mode"},
			},
		},
	},
	{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "mpm_persona_set",
			"description": "Set the active MPM persona.",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"persona": map[string]interface{}{"type": "string", "description": "Persona name"},
				},
				"required": []string{"persona"},
			},
		},
	},
	{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "mpm_directive_add",
			"description": "Add a prime directive to MPM.",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"content": map[string]interface{}{"type": "string", "description": "Directive content"},
				},
				"required": []string{"content"},
			},
		},
	},
	{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "mpm_synthesize",
			"description": "Trigger MPM session synthesis for a given UUID.",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"session_uuid": map[string]interface{}{"type": "string", "description": "Session UUID to synthesize"},
				},
				"required": []string{"session_uuid"},
			},
		},
	},
}

// toolHandlers maps tool names to their handlers
var toolHandlers = map[string]ToolHandler{
	"read_file":            handleReadFile,
	"write_file":           handleWriteFile,
	"shell":                handleShell,
	"web_search":           handleWebSearch,
	"web_fetch":            handleWebFetch,
	"mpm_memory_search":    handleMPMMemorySearch,
	"mpm_lesson_search":    handleMPMLessonSearch,
	"mpm_directive_list":   handleMPMDirectiveList,
	"mpm_reference_search": handleMPMReferenceSearch,
	"mpm_mode_list":       handleMPMModeList,
	"mpm_persona_list":    handleMPMPersonaList,
	"mpm_lesson_add":      handleMPMLessonAdd,
	"mpm_mode_set":        handleMPMModeSet,
	"mpm_persona_set":     handleMPMPersonaSet,
	"mpm_directive_add":   handleMPMDirectiveAdd,
	"mpm_synthesize":      handleMPMSynthesize,
}

// ---- Filesystem Tools ----

func handleReadFile(args map[string]interface{}, db *sql.DB) (string, error) {
	path, ok := args["path"].(string)
	if !ok || path == "" {
		return "", fmt.Errorf("read_file: path is required")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read_file: %w", err)
	}
	return truncate(string(content), 2000), nil
}

func handleWriteFile(args map[string]interface{}, db *sql.DB) (string, error) {
	path, ok := args["path"].(string)
	if !ok || path == "" {
		return "", fmt.Errorf("write_file: path is required")
	}
	content, ok := args["content"].(string)
	if !ok {
		return "", fmt.Errorf("write_file: content is required")
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return "", fmt.Errorf("write_file: %w", err)
	}
	return fmt.Sprintf("Written %d bytes to %s", len(content), path), nil
}

// ---- Shell Tool ----

func handleShell(args map[string]interface{}, db *sql.DB) (string, error) {
	cmd, ok := args["command"].(string)
	if !ok || cmd == "" {
		return "", fmt.Errorf("shell: command is required")
	}
	out, err := exec.Command("sh", "-c", cmd).CombinedOutput()
	result := string(out)
	if err != nil {
		return truncate(result, 1000) + "\n[exit error: "+err.Error()+"]", nil
	}
	return truncate(result, 1000), nil
}

// ---- Web Tools ----

func handleWebSearch(args map[string]interface{}, db *sql.DB) (string, error) {
	query, ok := args["query"].(string)
	if !ok || query == "" {
		return "", fmt.Errorf("web_search: query is required")
	}
	searchURL := "https://duckduckgo.com/html/?q=" + url.QueryEscape(query)
	req, _ := http.NewRequest("GET", searchURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("web_search: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8000))
	return string(body), nil
}

func handleWebFetch(args map[string]interface{}, db *sql.DB) (string, error) {
	rawURL, ok := args["url"].(string)
	if !ok || rawURL == "" {
		return "", fmt.Errorf("web_fetch: url is required")
	}
	if !strings.HasPrefix(rawURL, "http") {
		return "", fmt.Errorf("web_fetch: url must start with http")
	}
	req, _ := http.NewRequest("GET", rawURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many redirects")
		}
		return nil
	}}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("web_fetch: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 10000))
	return truncate(string(body), 2000), nil
}

// ---- MPM Read Tools ----

func handleMPMMemorySearch(args map[string]interface{}, db *sql.DB) (string, error) {
	query, _ := args["query"].(string)
	limit := 5
	if l, ok := args["limit"].(float64); ok {
		limit = int(l)
	}
	rows, err := db.Query(`
		SELECT m.content, m.tags, m.created_at
		FROM memories m
		JOIN memories_fts fts ON m.rowid = fts.rowid
		WHERE memories_fts MATCH ? AND m.deleted_at IS NULL
		ORDER BY rank
		LIMIT ?
	`, query, limit)
	if err != nil {
		// Fallback to LIKE
		like := "%" + query + "%"
		rows, err = db.Query(`
			SELECT content, tags, created_at FROM memories
			WHERE (content LIKE ? OR tags LIKE ?) AND deleted_at IS NULL
			ORDER BY created_at DESC LIMIT ?
		`, like, like, limit)
		if err != nil {
			return "", fmt.Errorf("memory search: %w", err)
		}
	}
	defer rows.Close()
	var results []string
	for rows.Next() {
		var content, tags, created string
		rows.Scan(&content, &tags, &created)
		results = append(results, fmt.Sprintf("- [%s] %s", created, truncate(content, 200)))
	}
	if len(results) == 0 {
		return "No memories found.", nil
	}
	return strings.Join(results, "\n"), nil
}

func handleMPMLessonSearch(args map[string]interface{}, db *sql.DB) (string, error) {
	query, _ := args["query"].(string)
	like := "%" + query + "%"
	rows, err := db.Query(`
		SELECT id, content, type FROM lessons
		WHERE content LIKE ? OR tags LIKE ?
		ORDER BY reinforcement_count DESC LIMIT 10
	`, like, like)
	if err != nil {
		return "", fmt.Errorf("lesson search: %w", err)
	}
	defer rows.Close()
	var results []string
	for rows.Next() {
		var id, content, lt string
		rows.Scan(&id, &content, &lt)
		results = append(results, fmt.Sprintf("[%s] %s: %s", lt, id[:8], truncate(content, 150)))
	}
	if len(results) == 0 {
		return "No lessons found.", nil
	}
	return strings.Join(results, "\n"), nil
}

func handleMPMDirectiveList(args map[string]interface{}, db *sql.DB) (string, error) {
	rows, err := db.Query(`
		SELECT id, content FROM memories
		WHERE metadata LIKE '%is_prime_directive%' AND deleted_at IS NULL
		ORDER BY created_at DESC LIMIT 50
	`)
	if err != nil {
		return "", fmt.Errorf("directive list: %w", err)
	}
	defer rows.Close()
	var results []string
	for rows.Next() {
		var id, content string
		rows.Scan(&id, &content)
		results = append(results, fmt.Sprintf("- %s [%s]", truncate(content, 200), id[:8]))
	}
	if len(results) == 0 {
		return "No directives found.", nil
	}
	return strings.Join(results, "\n"), nil
}

func handleMPMReferenceSearch(args map[string]interface{}, db *sql.DB) (string, error) {
	query, _ := args["query"].(string)
	limit := 3
	if l, ok := args["limit"].(float64); ok {
		limit = int(l)
	}
	rows, err := db.Query(`
		SELECT r.title, rc.content
		FROM reference_chunks rc
		JOIN reference_docs r ON rc.doc_id = r.id
		JOIN reference_chunks_fts fts ON rc.rowid = fts.rowid
		WHERE reference_chunks_fts MATCH ?
		ORDER BY rank
		LIMIT ?
	`, query, limit)
	if err != nil {
		return "", fmt.Errorf("reference search: %w", err)
	}
	defer rows.Close()
	var results []string
	for rows.Next() {
		var title, content string
		rows.Scan(&title, &content)
		results = append(results, fmt.Sprintf("## %s\n%s", title, truncate(content, 300)))
	}
	if len(results) == 0 {
		return "No references found.", nil
	}
	return strings.Join(results, "\n---\n"), nil
}

func handleMPMModeList(args map[string]interface{}, db *sql.DB) (string, error) {
	rows, err := db.Query(`SELECT id, name FROM modes ORDER BY name`)
	if err != nil {
		return "", fmt.Errorf("mode list: %w", err)
	}
	defer rows.Close()
	var results []string
	for rows.Next() {
		var id, name string
		rows.Scan(&id, &name)
		results = append(results, fmt.Sprintf("- %s", name))
	}
	if len(results) == 0 {
		return "No modes found.", nil
	}
	return strings.Join(results, "\n"), nil
}

func handleMPMPersonaList(args map[string]interface{}, db *sql.DB) (string, error) {
	rows, err := db.Query(`SELECT id, name FROM personas ORDER BY name`)
	if err != nil {
		return "", fmt.Errorf("persona list: %w", err)
	}
	defer rows.Close()
	var results []string
	for rows.Next() {
		var id, name string
		rows.Scan(&id, &name)
		results = append(results, fmt.Sprintf("- %s", name))
	}
	if len(results) == 0 {
		return "No personas found.", nil
	}
	return strings.Join(results, "\n"), nil
}

// ---- MPM Write Tools ----

func handleMPMLessonAdd(args map[string]interface{}, db *sql.DB) (string, error) {
	content, _ := args["content"].(string)
	lessonType := "insight"
	if lt, ok := args["type"].(string); ok && lt != "" {
		lessonType = lt
	}
	tags := ""
	if t, ok := args["tags"].(string); ok {
		tags = t
	}
	id := generateID()
	now := time.Now().Format(time.RFC3339)
	_, err := db.Exec(`
		INSERT INTO lessons (id, content, type, tags, created_at, reinforcement_count)
		VALUES (?, ?, ?, ?, ?, 1)
	`, id, content, lessonType, tags, now)
	if err != nil {
		return "", fmt.Errorf("lesson add: %w", err)
	}
	return fmt.Sprintf("Lesson added: %s [%s]", id[:8], lessonType), nil
}

func handleMPMModeSet(args map[string]interface{}, db *sql.DB) (string, error) {
	mode, _ := args["mode"].(string)
	if mode == "" {
		return "", fmt.Errorf("mode_set: mode name is required")
	}
	stateDir := dbPath[:len(dbPath)-len("/mpm.db")] + "/state"
	os.MkdirAll(stateDir, 0755)
	if err := os.WriteFile(stateDir+"/active_mode", []byte(mode), 0644); err != nil {
		return "", fmt.Errorf("mode_set: %w", err)
	}
	return fmt.Sprintf("Mode set to: %s", mode), nil
}

func handleMPMPersonaSet(args map[string]interface{}, db *sql.DB) (string, error) {
	persona, _ := args["persona"].(string)
	if persona == "" {
		return "", fmt.Errorf("persona_set: persona name is required")
	}
	stateDir := dbPath[:len(dbPath)-len("/mpm.db")] + "/state"
	os.MkdirAll(stateDir, 0755)
	if err := os.WriteFile(stateDir+"/active_persona", []byte(persona), 0644); err != nil {
		return "", fmt.Errorf("persona_set: %w", err)
	}
	return fmt.Sprintf("Persona set to: %s", persona), nil
}

func handleMPMDirectiveAdd(args map[string]interface{}, db *sql.DB) (string, error) {
	content, _ := args["content"].(string)
	if content == "" {
		return "", fmt.Errorf("directive_add: content is required")
	}
	id := generateID()
	now := time.Now().Format(time.RFC3339)
	metadata := fmt.Sprintf(`{"is_prime_directive":true,"source":"mpm-agent"}`)
	_, err := db.Exec(`
		INSERT INTO memories (id, collection, content, session_id, tags, metadata, created_at)
		VALUES (?, 'directives', ?, '', '', ?, ?)
	`, id, content, metadata, now)
	if err != nil {
		return "", fmt.Errorf("directive_add: %w", err)
	}
	return fmt.Sprintf("Directive added: %s", id[:8]), nil
}

func handleMPMSynthesize(args map[string]interface{}, db *sql.DB) (string, error) {
	uuid, _ := args["session_uuid"].(string)
	if uuid == "" {
		return "", fmt.Errorf("synthesize: session_uuid is required")
	}
	return fmt.Sprintf("Synthesis triggered for session %s. Run 'mpm synthesize %s' to execute.", uuid[:8], uuid), nil
}

func generateID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// truncate shortens s to maxLen, preserving start and end
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 10 {
		return s[:maxLen]
	}
	half := (maxLen - 20) / 2
	return s[:half] + fmt.Sprintf("\n...(%d chars truncated)...\n", len(s)-maxLen) + s[len(s)-half:]
}

// ============================================================================
// Context Building
// ============================================================================

// buildSystemPrompt creates the system prompt with tools and context
func buildSystemPrompt(memories, directives, references string) string {
	var sb strings.Builder
	sb.WriteString("You are mpm-agent — MPM's companion AI agent. ")
	sb.WriteString("You have access to tools listed below. Use them to help the user.\n\n")
	sb.WriteString("## Tools\n")
	for _, t := range toolDefinitions {
		fn := t["function"].(map[string]interface{})
		sb.WriteString(fmt.Sprintf("- %s: %s\n", fn["name"], fn["description"]))
	}
	sb.WriteString("\n## Guidelines\n")
	sb.WriteString("- Use tools when they help answer the user's question\n")
	sb.WriteString("- Be concise and practical\n")
	sb.WriteString("- When using shell, explain what you're doing briefly\n")
	sb.WriteString("- Format file paths and code in code blocks\n\n")
	if memories != "" {
		sb.WriteString("## Relevant Memories\n")
		sb.WriteString(memories)
		sb.WriteString("\n\n")
	}
	if directives != "" {
		sb.WriteString("## Prime Directives\n")
		sb.WriteString(directives)
		sb.WriteString("\n\n")
	}
	if references != "" {
		sb.WriteString("## Reference Material\n")
		sb.WriteString(references)
		sb.WriteString("\n\n")
	}
	sb.WriteString("## Current Date\n")
	sb.WriteString(time.Now().Format("2006-01-02"))
	return sb.String()
}

// retrieveMemories returns formatted relevant memories from MPM DB
func retrieveMemories(db *sql.DB, query string, limit int) string {
	if query == "" {
		return ""
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
			return ""
		}
	}
	defer rows.Close()
	var results []string
	for rows.Next() {
		var content, tags string
		rows.Scan(&content, &tags)
		results = append(results, fmt.Sprintf("- %s", truncate(content, 300)))
	}
	return strings.Join(results, "\n")
}

// retrieveDirectives returns all prime directives
func retrieveDirectives(db *sql.DB) string {
	rows, err := db.Query(`
		SELECT content FROM memories
		WHERE metadata LIKE '%is_prime_directive%' AND deleted_at IS NULL
		ORDER BY created_at DESC LIMIT 20
	`)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var results []string
	for rows.Next() {
		var content string
		rows.Scan(&content)
		results = append(results, fmt.Sprintf("- %s", truncate(content, 200)))
	}
	return strings.Join(results, "\n")
}

// retrieveReferences returns formatted reference chunks
func retrieveReferences(db *sql.DB, query string, limit int) string {
	if query == "" {
		return ""
	}
	rows, err := db.Query(`
		SELECT r.title, rc.content FROM reference_chunks rc
		JOIN reference_docs r ON rc.doc_id = r.id
		JOIN reference_chunks_fts fts ON rc.rowid = fts.rowid
		WHERE reference_chunks_fts MATCH ?
		ORDER BY rank LIMIT ?
	`, query, limit)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var results []string
	for rows.Next() {
		var title, content string
		rows.Scan(&title, &content)
		results = append(results, fmt.Sprintf("### %s\n%s", title, truncate(content, 400)))
	}
	return strings.Join(results, "\n---\n")
}

// ============================================================================
// Agent Loop
// ============================================================================

// conversationTurn is a single user/assistant exchange
type conversationTurn struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// runAgentLoop runs the agent with the given query and streaming setting.
func runAgentLoop(query string, streaming bool, cfg *Config) error {
	db, err := OpenDB()
	if err != nil {
		return fmt.Errorf("mpm-agent needs MPM running. Start with 'mpm start': %w", err)
	}
	defer db.Close()

	// Build context
	memories := retrieveMemories(db, query, 5)
	directives := retrieveDirectives(db)
	references := retrieveReferences(db, query, 3)

	systemPrompt := buildSystemPrompt(memories, directives, references)

	// Conversation: system + user
	messages := []map[string]interface{}{
		{"role": "system", "content": systemPrompt},
		{"role": "user", "content": query},
	}

	for attempt := 0; attempt < 10; attempt++ {
		var accumulated strings.Builder
		var lastResp *LLMResponse
		_, err := LLMCall(cfg, messages, toolDefinitions, streaming, func(token string) {
			if streaming {
				fmt.Print(token)
				os.Stdout.Sync()
			}
			accumulated.WriteString(token)
		}, func(resp *LLMResponse) {
			lastResp = resp
		})
		if err != nil {
			return fmt.Errorf("LLM error: %w", err)
		}

		if streaming {
			fmt.Println()
		}

		// If no tool calls, we're done
		if lastResp == nil || len(lastResp.Choices) == 0 {
			break
		}
		msg := lastResp.Choices[0].Message
		if msg == nil || len(msg.ToolCalls) == 0 {
			break
		}

		// Execute tool calls and append results
		for _, tc := range msg.ToolCalls {
			args := make(map[string]interface{})
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				messages = append(messages, map[string]interface{}{
					"role":         "tool",
					"tool_call_id": tc.ID,
					"content":      fmt.Sprintf("error parsing arguments: %v", err),
				})
				continue
			}
			handler, ok := toolHandlers[tc.Function.Name]
			if !ok {
				messages = append(messages, map[string]interface{}{
					"role":         "tool",
					"tool_call_id": tc.ID,
					"content":      fmt.Sprintf("unknown tool: %s", tc.Function.Name),
				})
				continue
			}
			result, err := handler(args, db)
			if err != nil {
				result = "error: " + err.Error()
			}
			result = truncate(result, 800)
			messages = append(messages, map[string]interface{}{
				"role":         "tool",
				"tool_call_id": tc.ID,
				"content":      result,
			})
		}
	}

	return nil
}

// ============================================================================
// CLI
// ============================================================================

func main() {
	args := os.Args[1:]
	cfg, err := LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}

	batch := false
	var query string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--batch":
			batch = true
		case "--help", "-h":
			printHelp()
			os.Exit(0)
		default:
			if !strings.HasPrefix(args[i], "-") {
				query = strings.Join(args[i:], " ")
				break
			}
		}
	}

	if query != "" {
		if err := runAgentLoop(query, !batch, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "\nerror: %v\n", err)
			os.Exit(1)
		}
		return
	}

	repl(cfg)
}

func printHelp() {
	fmt.Print(`mpm-agent — MPM companion agent

Usage:
  mpm-agent [options] [query]    Run query or start REPL
  mpm-agent --help               Show this help

Options:
  --batch   Disable streaming (batch mode)

Modes:
  No query  REPL mode — type queries interactively
  With query Single-shot mode — streaming response

Tools available:
  read_file, write_file, shell, web_search, web_fetch
  mpm_memory_search, mpm_lesson_search, mpm_directive_list
  mpm_reference_search, mpm_mode_list, mpm_persona_list
  mpm_lesson_add, mpm_mode_set, mpm_persona_set
  mpm_directive_add, mpm_synthesize

Environment:
  MINIMAX_API_KEY   API key (or set in mpm_config.json)
  MPM_WORKSPACE     MPM workspace path
`)
}

func repl(cfg *Config) {
	fmt.Println("mpm-agent REPL (Ctrl+C to exit)")
	fmt.Println("Type your query and press Enter.")
	fmt.Println()
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("> ")
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF || strings.Contains(err.Error(), "closed") {
				break
			}
			continue
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == "exit" || line == "quit" {
			break
		}
		fmt.Println()
		if err := runAgentLoop(line, true, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "\nerror: %v\n", err)
		}
		fmt.Println()
	}
}
