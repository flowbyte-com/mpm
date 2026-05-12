package core

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// SessionRunner is the universal agent orchestrator.
// It handles the full message lifecycle: command interception,
// session history management, agent loop, and async self-improve.
// Both CLI and Telegram entry points share this implementation.
type SessionRunner struct {
	transport Transport

	// Identity
	sessionID    string
	identityPath string

	// Profile (chat/coding)
	profile     string
	toolProfile []string

	// Dependencies (injected to avoid import cycles)
	db       *sql.DB
	session  Persistence
	identity IdentityResolver

	// Sub-systems
	selfImprover *SelfImprover

	// In-memory history (sliding window, separate from Persistence)
	history []Message

	mu sync.Mutex // protects history during command operations
}

// Persistence abstracts session storage. Implemented by SessionManager.
type Persistence interface {
	// Get returns the conversation history.
	Get() ([]map[string]interface{}, error)
	// Save persists the conversation history.
	Save(messages []map[string]interface{}) error
	// SessionID returns the unique session identifier.
	SessionID() string
}

// IdentityResolver abstracts IDENTITY.md loading.
type IdentityResolver interface {
	ResolveIdentityPath(binaryDir, configured string) string
	LoadIdentity(path string) (*Identity, error)
}

// NewSessionRunner creates a new SessionRunner.
func NewSessionRunner(
	transport Transport,
	sessionID string,
	initialProfile string,
	db *sql.DB,
	session Persistence,
	identity IdentityResolver,
) *SessionRunner {
	sr := &SessionRunner{
		transport:    transport,
		sessionID:    sessionID,
		profile:      initialProfile,
		db:           db,
		session:      session,
		identity:     identity,
		identityPath: identity.ResolveIdentityPath(GetBinaryDir(), ""),
	}
	sr.selfImprover = &SelfImprover{
		dbPath: ResolveMiniBotDBPath(),
	}
	return sr
}

// Run starts the session loop, blocking on transport.ReadMessage.
// It processes messages until context is cancelled.
func (r *SessionRunner) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		input, err := r.transport.ReadMessage()
		if err != nil {
			if isClosedErr(err) {
				return nil
			}
			log.Printf("[session] read error: %v", err)
			continue
		}

		if err := r.handleMessage(ctx, input); err != nil {
			log.Printf("[session] handle error: %v", err)
		}
	}
}

// HandleInput processes a single message input.
// Exposed for use by REPL loop without ReadMessage.
func (r *SessionRunner) HandleInput(ctx context.Context, input string) error {
	return r.handleMessage(ctx, input)
}

// handleMessage dispatches commands vs. agent calls.
func (r *SessionRunner) handleMessage(ctx context.Context, input string) error {
	// Strip whitespace
	input = strings.TrimSpace(input)
	if input == "" {
		return nil
	}

	// Intercept built-in commands (no agent call)
	if strings.HasPrefix(input, "/") {
		return r.interceptCommand(ctx, input)
	}

	// Regular message → run agent
	return r.runAgentWithContext(ctx, input)
}

// interceptCommand handles built-in commands without invoking the agent.
// Returns error to signal command failure (not an agent error).
func (r *SessionRunner) interceptCommand(ctx context.Context, input string) error {
	// Strip bot mention if present: /new@mpm_808 -> /new
	if idx := strings.Index(input, "@"); idx != -1 {
		input = input[:idx]
	}

	parts := strings.Fields(input)
	if len(parts) == 0 {
		return nil
	}

	cmd := parts[0]

	// Profile switch: /tools <profile>
	if cmd == "/tools" {
		if len(parts) < 2 {
			r.transport.WriteChunk("Available profiles: chat, coding\n", false)
			return nil
		}
		return r.updateProfile(parts[1])
	}

	switch cmd {
	case "/new", "/clear":
		return r.resetSession()
	case "/status":
		return r.showStatus()
	case "/recall":
		if len(parts) < 2 {
			r.transport.WriteChunk("Usage: /recall <topic>\n", false)
			return nil
		}
		return r.recallTopic(strings.Join(parts[1:], " "))
	default:
		r.transport.WriteChunk(fmt.Sprintf("Unknown command: %s\n", cmd), false)
		return nil
	}
}

// updateProfile switches the active tool profile and LLM routing.
// Valid profiles: "chat", "coding".
func (r *SessionRunner) updateProfile(profile string) error {
	switch profile {
	case "chat":
		r.mu.Lock()
		r.profile = "chat"
		r.toolProfile = []string{
			"list_toolkits", "load_toolkit", "unload_toolkit",
			"execute_mpm_command",
			"read_file", "write_file", "ReadFileSemantic", "ReadFileCompare",
			"WebSynthesize",
		}
		r.mu.Unlock()
		r.transport.WriteChunk("Switched to chat profile (MiniMax, basic tools)\n", false)
		return nil

	case "coding":
		r.mu.Lock()
		r.profile = "coding"
		r.toolProfile = []string{
			"list_toolkits", "load_toolkit", "unload_toolkit",
			"execute_mpm_command",
			"read_file", "write_file", "ReadFileSemantic", "ReadFileCompare",
			"rg", "sg", "repomap",
			"git_status", "git_commit", "git_diff",
			"apply_diff", "execute_cmd_with_timeout",
		}
		r.mu.Unlock()
		r.transport.WriteChunk("Switched to coding profile (OpenRouter, advanced tools)\n", false)
		return nil

	default:
		r.transport.WriteChunk(fmt.Sprintf("Unknown profile: %s\n", profile), false)
		return nil
	}
}

// resetSession clears session history and toolkits.
func (r *SessionRunner) resetSession() error {
	// Clear in-memory history and toolkits
	r.mu.Lock()
	r.history = nil
	r.session.Save(nil)
	ClearSessionToolkits(r.sessionID)
	r.mu.Unlock()

	r.transport.WriteChunk("Session cleared. Starting fresh.\n", false)
	return nil
}

// showStatus displays current profile and active state.
func (r *SessionRunner) showStatus() error {
	r.mu.Lock()
	profile := r.profile
	r.mu.Unlock()

	model := "MiniMax-Text-01"
	baseURL := "https://api.minimax.io/anthropic/v1"
	if profile == "coding" {
		model = "anthropic/claude-3.5-sonnet"
		baseURL = "https://openrouter.ai/api/v1"
	}

	r.transport.WriteChunk(fmt.Sprintf(
		"Profile: %s | Model: %s | URL: %s\n", profile, model, baseURL,
	), false)
	return nil
}

// recallTopic queries MPM memories about a topic and injects them into context.
func (r *SessionRunner) recallTopic(topic string) error {
	memories := retrieveMemories(r.db, topic, 5)
	if len(memories) == 0 {
		r.transport.WriteChunk(fmt.Sprintf("No memories found for: %s\n", topic), false)
		return nil
	}

	r.transport.WriteChunk(fmt.Sprintf("## Memories for: %s\n", topic), false)
	for _, m := range memories {
		r.transport.WriteChunk(fmt.Sprintf("- %s\n", m), false)
	}
	return nil
}

// runAgentWithContext is the core agent loop.
// Extracts and refactors from handler.go runAgentWithTimeout.
func (r *SessionRunner) runAgentWithContext(ctx context.Context, query string) error {
	// 90s hard timeout
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	// Load history
	history, err := r.session.Get()
	if err != nil {
		return fmt.Errorf("load history: %w", err)
	}
	if history == nil {
		history = []map[string]interface{}{}
	}

	// Get anchors and lessons
	anchors, _ := GetRecentAnchors(r.db, 3)
	lessons, _ := GetRecentLessons(r.db, 1)

	// Build context
	memories := retrieveMemories(r.db, query, 3)
	directives := retrieveDirectives(r.db)
	references := retrieveReferences(r.db, query, 2)

	// Load front cortex (generic fallback — Telegram-specific SessionManager has its own)
	fcText := loadFrontCortexGeneric(r.db)

	// Build system prompt
	systemPrompt := BuildSystemPromptWithIdentity(r.identityPath, "", "", fcText,
		memories, directives, references, anchors, lessons)

	// Build messages: convert history to apiMessage type
	apiMessages := r.historyToAPIMessages(history)
	messages := make([]apiMessage, 0, len(apiMessages)+1)
	for _, m := range apiMessages {
		messages = append(messages, apiMessage{Role: m.Role, Content: m.Content})
	}
	messages = append(messages, apiMessage{Role: "user", Content: query})

	// Get LLM config for profile
	synthCfg := RouteProfile(r.profile)

	// Start typing indicator
	r.transport.SendTypingIndicator()

	// Tool loop
	for iteration := 0; iteration < maxIterations; iteration++ {
		availableTools := buildToolListWithLoaded(r.toolProfile, r.sessionID, GetToolkitMap())

		responseText, toolCalls, thinkingText, _, err := callSynthAPIWithTools(
			ctx, systemPrompt, messages, &synthCfg, availableTools,
		)
		if err != nil {
			r.transport.StopTypingIndicator()
			if ctx.Err() == context.DeadlineExceeded {
				r.transport.WriteChunk("⚠️ Request timed out (90s).\n", false)
			} else {
				r.transport.WriteChunk(fmt.Sprintf("⚠️ Error: %v\n", err), false)
			}
			return err
		}

		// Stream thinking block to transport if present
		if thinkingText != "" {
			r.transport.WriteChunk(thinkingText, true)
		}

		// If no tool calls, stream final response and return
		if len(toolCalls) == 0 {
			r.transport.StopTypingIndicator()
			if responseText != "" {
				r.transport.WriteChunk(responseText, false)
			}
			// Save to history
			r.appendHistory("user", query)
			r.appendHistory("assistant", responseText)
			r.session.Save(r.historyToMap(r.history))
			// Trigger async self-improve
			r.selfImprove(query, responseText)
			return nil
		}

		// Execute tool calls
		for _, tc := range toolCalls {
			// Framework tool interception (no agent round-trip)
			switch tc.Name {
			case "load_toolkit":
				name, _ := tc.Input["name"].(string)
				if name == "" {
					r.appendToolResult("load_toolkit", "error: name is required")
				} else {
					LoadToolkit(r.sessionID, name)
					r.appendToolResult("load_toolkit", fmt.Sprintf("Toolkit '%s' loaded.", name))
				}
				continue
			case "unload_toolkit":
				name, _ := tc.Input["name"].(string)
				if name == "" {
					r.appendToolResult("unload_toolkit", "error: name is required")
				} else {
					UnloadToolkit(r.sessionID, name)
					r.appendToolResult("unload_toolkit", fmt.Sprintf("Toolkit '%s' unloaded.", name))
				}
				continue
			}

			// HITL approval for risky tools
			if isRiskyTool(tc.Name) {
				r.transport.StopTypingIndicator()
				approved := r.transport.RequestToolApproval(tc.Name, formatArgs(tc.Input))
				r.transport.SendTypingIndicator()
				if !approved {
					r.appendToolResult(tc.Name, fmt.Sprintf("Tool %s rejected by user.", tc.Name))
					messages = append(messages, apiMessage{Role: "user", Content: fmt.Sprintf("[%s result]: Tool rejected.\n", tc.Name)})
					continue
				}
			}

			// Execute the tool
			result, err := executeTool(tc.Name, tc.Input, r.sessionID)
			if err != nil {
				result = fmt.Sprintf("error: %v", err)
			}
			r.appendToolResult(tc.Name, result)
		}

		// Check loop breaker
		if iteration == maxIterations-1 && len(toolCalls) > 0 {
			r.transport.StopTypingIndicator()
			r.transport.WriteChunk("⚠️ Loop terminated: Exceeded max reasoning steps.\n", false)
			return nil
		}
	}

	r.transport.StopTypingIndicator()
	return nil
}

// appendHistory adds a turn to the in-memory history.
func (r *SessionRunner) appendHistory(role, content string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.history = append(r.history, Message{Role: role, Content: content})
	if len(r.history) > maxHistoryMessages {
		r.history = r.history[len(r.history)-maxHistoryMessages:]
	}
}

// appendToolResult appends a tool result as a user message.
func (r *SessionRunner) appendToolResult(toolName, result string) {
	r.appendHistory("user", fmt.Sprintf("[%s result]: %s", toolName, result))
}

// historyToAPIMessages converts persisted history and merges with in-memory history.
// Returns Message slice (local type, not apiMessage).
func (r *SessionRunner) historyToAPIMessages(persisted []map[string]interface{}) []Message {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Start with in-memory history
	msgs := make([]Message, 0, len(r.history))
	for _, h := range r.history {
		msgs = append(msgs, h)
	}

	// Append persisted history (deduplicated)
	for _, h := range persisted {
		role, _ := h["role"].(string)
		content, _ := h["content"].(string)
		if role == "" {
			role = "user"
		}
		// Skip if same as last in-memory message
		if len(msgs) > 0 && msgs[len(msgs)-1].Role == role && msgs[len(msgs)-1].Content == content {
			continue
		}
		msgs = append(msgs, Message{Role: role, Content: content})
	}
	return msgs
}

// historyToMap converts Message slice to []map[string]interface{}.
func (r *SessionRunner) historyToMap(messages []Message) []map[string]interface{} {
	result := make([]map[string]interface{}, len(messages))
	for i, m := range messages {
		result[i] = map[string]interface{}{"role": m.Role, "content": m.Content}
	}
	return result
}

// SelfImprover handles async memory anchoring and lesson extraction.
type SelfImprover struct {
	dbPath string
}

// selfImprove runs after each response in a goroutine.
// Opens its own db connection to avoid SQLite lock contention.
func (r *SessionRunner) selfImprove(userText, responseText string) {
	go func() {
		db, err := OpenDBForPath(r.selfImprover.dbPath)
		if err != nil {
			log.Printf("[session] selfImprove: open db: %v", err)
			return
		}
		defer db.Close()

		// Anchor user message if substantial
		if len(userText) > 100 {
			weight := 1
			if len(userText) > 200 {
				weight = 2
			}
			facts := ExtractFactsFromText(userText)
			tags := ExtractTagsFromText(userText)
			summary := truncate(userText, 100)
			if err := InsertAnchor(db, facts, summary, tags, "user_message", r.sessionID, weight); err != nil {
				log.Printf("[session] InsertAnchor error: %v", err)
			}
		}

		// Extract lesson if exchange was informative
		if len(responseText) > 100 && len(userText) > 20 {
			lessonContent := fmt.Sprintf("User asked: %s | Response: %s",
				truncate(userText, 100), truncate(responseText, 200))
			lesson := Lesson{
				ID:      GenerateID(),
				Content: lessonContent,
				Type:    "exchange",
				Tags:    "cli,session",
			}
			if err := ExtractLesson(db, lesson); err != nil {
				log.Printf("[session] ExtractLesson error: %v", err)
			}
		}
	}()
}

// isClosedErr returns true if the error indicates the read source was closed.
func isClosedErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "use of closed")
}

// GetToolkitMap returns the default toolkit map for profile routing.
func GetToolkitMap() map[string][]string {
	return map[string][]string{
		"files":   {"read_file", "write_file", "ReadFileSemantic", "ReadFileCompare"},
		"web":     {"WebSynthesize"},
		"mpm":     {"execute_mpm_command"},
		"minimax": {"generate_image", "synthesize_speech", "web_search", "understand_image"},
		"shell":   {"execute_shell"},
	}
}

// loadFrontCortexGeneric loads basic front cortex without Telegram-specific data.
func loadFrontCortexGeneric(db *sql.DB) string {
	// For now, return empty string — Telegram handler uses its own LoadFrontCortex
	_ = db
	return ""
}

// RouteProfile returns the LLM configuration for a given profile.
// Pure config lookup — no hardcoded URLs, models, or API keys.
func RouteProfile(profile string) SynthConfig {
	cfg, _ := LoadMiniBotConfig(GetConfigPath())
	if cfg == nil {
		cfg = DefaultMiniBotConfig()
	}

	// Try the requested profile first
	if cfg.SynthProfiles != nil {
		if synth, ok := cfg.SynthProfiles[profile]; ok {
			return synth
		}
	}

	// Fallback to "chat" profile
	if profile != "chat" {
		if cfg.SynthProfiles != nil {
			if synth, ok := cfg.SynthProfiles["chat"]; ok {
				return synth
			}
		}
	}

	// Final fallback: legacy single Synth config
	return cfg.Synth
}