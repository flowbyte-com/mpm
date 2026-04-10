// mpm-agent — MPM companion agent
// MIT License
package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
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

func main() {
	fmt.Println("mpm-agent v0.1.0 — not yet implemented")
	os.Exit(0)
}
