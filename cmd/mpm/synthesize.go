package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	mpminternal "mpm/internal"
)

// getOpenClawModelBaseURL reads OpenClaw config and returns the base URL for a provider.
func getOpenClawModelBaseURL(provider string) string {
	cfgPath := os.ExpandEnv("$HOME/.openclaw/openclaw.json")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return ""
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return ""
	}
	models, ok := cfg["models"].(map[string]interface{})
	if !ok {
		return ""
	}
	providers, ok := models["providers"].(map[string]interface{})
	if !ok {
		return ""
	}
	p, ok := providers[provider].(map[string]interface{})
	if !ok {
		return ""
	}
	if baseURL, ok := p["baseUrl"].(string); ok {
		return strings.TrimSuffix(baseURL, "/")
	}
	return ""
}

// =============================================================================
// mpm synthesize <uuid> — LLM-powered session fact extraction via MiniMax API
// =============================================================================

func handleSynthesize(args []string) int {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: mpm synthesize <session-uuid>\n")
		return 1
	}
	uuid := args[1]

	// Try .jsonl file first, then fall back to sessions table
	jsonlPath := findSessionJSONL(uuid)
	var lines []string
	var sessionContent string
	var meta sessionMetaRaw
	var transcript string
	var err error

	if jsonlPath != "" {
		lines, err = readJSONLinesRaw(jsonlPath)
		if err != nil || len(lines) == 0 {
			fmt.Fprintf(os.Stderr, "❌ Failed to read session file: %v\n", err)
			return 1
		}
		meta = extractSessionMetadataRaw(lines, filepath.Base(jsonlPath))
		transcript = condenseTranscriptRaw(lines)
	} else {
		// Fall back to sessions table
		sessionContent, err = getSessionFromDB(uuid)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Session not found: %s\n", uuid)
			return 1
		}
		meta = sessionMetaRaw{UUID: uuid, Model: "minimax/MiniMax-M2.7", Provider: "minimax"}
		transcript = sessionContent
		jsonlPath = "(from sessions table)"
	}

	if transcript == "" {
		fmt.Printf("⏭️  Empty session content, nothing to synthesize.\n")
		return 0
	}

	prompt := buildSynthesisPrompt(meta, transcript)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	result, err := callSynthesisLLM(ctx, prompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Synthesis failed: %v\n", err)
		return 1
	}

	// Connect to DB (writeable for synthesis)
	dbMgr, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ DB connection failed: %v\n", err)
		return 1
	}
	defer dbMgr.Close()

	// Find full session UUID from filename prefix (sessions table stores full UUIDs)
	fullUUID := findFullSessionUUID(uuid)
	if fullUUID == "" {
		fullUUID = uuid
	}

	// Ensure session exists in sessions table (FK: memories.session_id → sessions.id)
	sessionDBID, err := dbMgr.SaveSession(fullUUID, result.SessionSummary, jsonlPath, map[string]interface{}{
		"model":    meta.Model,
		"provider": meta.Provider,
		"synthesized_session": true,
	})
	if err != nil {
		// Session may already exist — look up its DB id
		sessionDBID = fullUUID
	}

	stored := 0
	for _, fact := range result.Memories {
		fact = strings.TrimSpace(fact)
		if fact == "" || fact == "null" || fact == "[]" {
			continue
		}
		tags := map[string]interface{}{
			"synthesized": true,
			"session-id":  uuid,
			"source":      "llm-synthesis",
		}
		for _, t := range result.Topics {
			tags[strings.ToLower(strings.TrimSpace(t))] = true
		}
		metadata := map[string]interface{}{
			"is_long_term": true,
			"weight":        8,
			"source_path":   jsonlPath,
			"session_id":    uuid,
			"synthesized":   true,
			"summary":       result.SessionSummary,
		}
		embedding := mpminternal.HashEmbed(fact)
		_, err := dbMgr.SaveMemory("memories", fact, sessionDBID, tags, metadata, embedding)
		if err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Failed to store fact: %v\n", err)
			continue
		}
		stored++
	}

	fmt.Printf("✅ Synthesized %d facts | topics: %v | summary: %s\n",
		stored, result.Topics, truncateStr(result.SessionSummary, 80))
	return 0
}

// SynthesisResult mirrors the LLM JSON output
type SynthesisResult struct {
	SessionSummary string   `json:"session_summary"`
	Topics         []string `json:"topics"`
	Memories       []string `json:"memories"`
}

// findFullSessionUUID looks up the full session UUID from the DB by prefix
func findFullSessionUUID(prefix string) string {
	dbPath := mpminternal.DefaultMemoryPaths().SQLiteDBPath
	db, err := sql.Open("sqlite3", dbPath+"?mode=ro")
	if err != nil {
		return ""
	}
	defer db.Close()
	var fullID string
	// Search by sessions.session_id (full UUID stored there)
	err = db.QueryRow(`SELECT id FROM sessions WHERE session_id LIKE ? || '%' LIMIT 1`, prefix).Scan(&fullID)
	if err == nil && fullID != "" {
		return fullID
	}
	// Fallback: search by sessions.id (the actual PK)
	err = db.QueryRow(`SELECT id FROM sessions WHERE id LIKE ? || '%' LIMIT 1`, prefix).Scan(&fullID)
	if err == nil {
		return fullID
	}
	return ""
}

// getSessionFromDB retrieves session content from the sessions table
func getSessionFromDB(uuid string) (string, error) {
	dbPath := mpminternal.DefaultMemoryPaths().SQLiteDBPath
	db, err := sql.Open("sqlite3", dbPath+"?mode=ro")
	if err != nil {
		return "", err
	}
	defer db.Close()
	var content string
	// Try by sessions.id first, then by sessions.session_id
	err = db.QueryRow(`SELECT content FROM sessions WHERE id = ?`, uuid).Scan(&content)
	if err == nil {
		return content, nil
	}
	err = db.QueryRow(`SELECT content FROM sessions WHERE session_id = ?`, uuid).Scan(&content)
	if err == nil {
		return content, nil
	}
	// Try prefix match
	rows, err := db.Query(`SELECT id, content FROM sessions WHERE id LIKE ? || '%' OR session_id LIKE ? || '%' LIMIT 1`, uuid, uuid)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	if rows.Next() {
		var id, content string
		if err := rows.Scan(&id, &content); err == nil {
			return content, nil
		}
	}
	return "", fmt.Errorf("session not found: %s", uuid)
}

// findSessionJSONL searches sessions directories for a .jsonl matching the prefix
func findSessionJSONL(uuid string) string {
	dirs := []string{
		"/home/v/.openclaw/agents/main/sessions",
	}
	prefix := uuid
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
				continue
			}
			if strings.HasSuffix(e.Name(), ".jsonl") {
				return filepath.Join(dir, e.Name())
			}
		}
	}
	return ""
}

// readJSONLinesRaw reads a .jsonl file and returns raw JSON lines
func readJSONLinesRaw(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if len(line) < 5 || !strings.HasPrefix(line, "{") {
			continue
		}
		lines = append(lines, line)
	}
	return lines, nil
}

// condenseTranscriptRaw converts raw .jsonl lines into a minimal transcript
func condenseTranscriptRaw(lines []string) string {
	var out strings.Builder
	for _, line := range lines {
		var ev struct {
			Type string `json:"type"`
			Message struct {
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text,omitempty"`
				} `json:"content"`
			} `json:"message,omitempty"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.Type == "message" && (ev.Message.Role == "user" || ev.Message.Role == "assistant") {
			var text string
			for _, c := range ev.Message.Content {
				if c.Type == "text" {
					text = c.Text
					break
				}
			}
			if text != "" && len(text) > 2 {
				role := strings.ToUpper(ev.Message.Role)
				if len(text) > 1000 {
					text = text[:1000] + " [...truncated]"
				}
				out.WriteString(role + ": " + text + "\n")
			}
		}
	}
	return out.String()
}

type sessionMetaRaw struct {
	UUID          string
	StartedAt     string
	EndedAt       string
	Model         string
	Provider      string
	UserMessages  int
	AssistantMsgs int
	ToolCalls     int
}

// extractSessionMetadataRaw extracts metadata from parsed .jsonl lines
func extractSessionMetadataRaw(lines []string, filename string) sessionMetaRaw {
	var meta sessionMetaRaw
	meta.UUID = strings.TrimSuffix(filename, ".jsonl.lock")
	if len(meta.UUID) != 36 {
		meta.UUID = strings.TrimSuffix(filename, ".jsonl")
	}
	meta.Model = "minimax/MiniMax-M2.7"
	meta.Provider = "minimax"
	for _, line := range lines {
		var ev struct {
			Type      string `json:"type"`
			Timestamp string `json:"timestamp"`
			Message   struct {
				Role string `json:"role"`
			} `json:"message,omitempty"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.Timestamp != "" && meta.StartedAt == "" {
			meta.StartedAt = ev.Timestamp
			meta.EndedAt = ev.Timestamp
		}
		if ev.Type == "message" {
			if ev.Message.Role == "user" {
				meta.UserMessages++
			} else if ev.Message.Role == "assistant" {
				meta.AssistantMsgs++
			}
		}
		if ev.Type == "tool_call" {
			meta.ToolCalls++
		}
	}
	return meta
}

const synthesisSystemPrompt = `You are the Memory Synthesis Engine for the Flowbyte Memory Persona Mode (MPM) pipeline.
Your objective is to analyze a sanitized transcript of a user's session and extract structured metadata: the Session Summary, overarching Topics, and discrete Memories.

DEFINITIONS & EXTRACTION RULES:

SESSION SUMMARY (String)

Provide a concise, 1-2 sentence executive summary of what was fundamentally achieved or explored during the session.

Ignore transient debugging; focus on the primary goal or outcome.

TOPICS (Array of Strings)

Extract 1 to 3 high-level categories or domains discussed in the session.

Keep topics broad and reusable (e.g., "Data Pipeline", "Security", "State Management").

MEMORIES (Array of Strings)

Extract 0 to 3 high-signal, permanent facts.

A "high-signal fact" is a permanent architectural decision, a new workflow established, a milestone completed, or a foundational constraint learned.

DO NOT extract conversational filler, typos, minor code syntax, or temporary errors.

If the session was purely transient chatter or failed debugging, return an empty array [].

Write facts in the third person (e.g., "User decided to...", "Architecture shifted to...").

Each memory must be standalone and make perfect sense out of context.

OUTPUT FORMAT:
You must respond strictly with a valid JSON object matching the requested schema. Do not wrap the JSON in markdown formatting blocks or include any introductory text.`

func buildSynthesisPrompt(meta sessionMetaRaw, transcript string) string {
	return fmt.Sprintf(`Session ID: %s
Session started: %s | ended: %s
Messages: %d user, %d assistant, %d tool calls

Transcript:
%s`, meta.UUID, meta.StartedAt, meta.EndedAt,
		meta.UserMessages, meta.AssistantMsgs, meta.ToolCalls,
		transcript)
}

func callSynthesisLLM(ctx context.Context, prompt string) (*SynthesisResult, error) {
	apiKey := os.Getenv("MINIMAX_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("MINIMAX_API_KEY not set in environment")
	}

	baseURL := os.Getenv("OPENAI_BASE_URL")
	if baseURL == "" {
		// Try to read from OpenClaw config
		baseURL = getOpenClawModelBaseURL("minimax")
	}
	if baseURL == "" {
		baseURL = "https://api.minimax.io/anthropic"
	}

	messages := []map[string]interface{}{
		{"role": "system", "content": synthesisSystemPrompt},
		{"role": "user", "content": prompt},
	}

	body := map[string]interface{}{
		"model":      "MiniMax-M2.7",
		"max_tokens": 1024,
		"messages":   messages,
	}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/messages", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("API request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text,omitempty"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	var responseText string
	for _, block := range result.Content {
		if block.Type == "text" {
			responseText = strings.TrimSpace(block.Text)
			break
		}
	}
	if responseText == "" {
		return nil, fmt.Errorf("no text block in response")
	}
	// Strip markdown code fences
	for _, fence := range []string{"```json", "```", "`"} {
		responseText = strings.TrimPrefix(responseText, fence)
	}
	responseText = strings.TrimSpace(responseText)
	// Extract first JSON object
	start := strings.Index(responseText, "{")
	end := strings.LastIndex(responseText, "}")
	if start == -1 || end == -1 || end <= start {
		return nil, fmt.Errorf("no JSON object found in response: %s", truncateStr(responseText, 300))
	}
	responseText = responseText[start : end+1]

	var synthesisResult SynthesisResult
	if err := json.Unmarshal([]byte(responseText), &synthesisResult); err != nil {
		return nil, fmt.Errorf("failed to parse synthesis JSON: %w\nRaw: %s", err, truncateStr(responseText, 300))
	}

	return &synthesisResult, nil
}

func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
