package main

// =============================================================================
// parsers.go — Parser library extracted from the deprecated watcher daemon.
//
// This file holds the pure-parsing logic that was originally part of the
// watcher daemon (cmd/mpm/watch.go, deprecated 2026-06-26). The daemon
// entrypoint, worker pool, fork-and-detach, fsnotify goroutines, and decay
// loop have all been removed; what remains here is the reusable parsing
// surface a future one-shot CLI command can build on top of:
//
//   - regex-based fact extraction (extractFacts / extractFromSessionLine /
//     looksLikeFact / extractKeywords)
//   - sessions.json snapshot parsing (parseSessionsSnapshot)
//   - JSONL line reader (readJSONLines)
//
// The original watcher orchestrator (processMarkdownFile / processSessionFile
// / ingestAs* / sweepDirectory / checkTopicClustering / etc.) lived as
// methods on watcherDaemon and was tightly coupled to daemon side effects
// (file deletion, archive, mirror append, worker-pool event dispatch). A
// one-shot CLI should call mpminternal.SaveMemory directly rather than
// re-creating the daemon ingestion pipeline.
//
// References to the daemon code are intentionally removed; this is a
// standalone parser library, not a daemon fragment.
// =============================================================================

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// Pre-compiled regex patterns (compiled once at package init, not per-call)
// ---------------------------------------------------------------------------

// factPatterns match declarative lines worth pulling out of a session log.
var factPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^.*"(name|key|value|path|url|endpoint|config|setting|preference)":\s*".*"$`),
	regexp.MustCompile(`(?i)^.*'(name|key|value|path|url|endpoint|config|setting|preference)':\s*'.*'$`),
	regexp.MustCompile(`(?i)^\s*[-*]\s+[A-Z].*:.*`),
	regexp.MustCompile(`(?i)^(user|preference|config|setting|path|name|key|value)[\s:-]+.+`),
}

// skipPatterns filter conversational / meta lines that should never be
// treated as facts.
var skipPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^(okay|ok|yes|yeah|yep|sure|great|thanks|thank you|i think|i believe|i feel)`),
	regexp.MustCompile(`(?i)^(the user|they|them|this is|here is|i'll|i will|i can|i could|let me|would you|could you)`),
	regexp.MustCompile(`^//.*`),
	regexp.MustCompile(`^\s*#.*`),
}

// looksLikeFactPatterns are heuristics for whether a user-message line is a
// declarative statement worth promoting.
var looksLikeFactPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^(my |the |user )?[a-z]+ [a-z]+ is|are|has|was|were`),
	regexp.MustCompile(`(?i)^(remember|note|fact|important|preference)`),
	regexp.MustCompile(`(?i)^\(.*\) `),
}

// extractKeywords patterns.
var (
	hashPattern  = regexp.MustCompile(`#([a-zA-Z][a-zA-Z0-9_-]*)`)
	camelPattern = regexp.MustCompile(`([A-Z][a-z]+[A-Z][a-zA-Z]*)`)
)

// ---------------------------------------------------------------------------
// sessionMessage — OpenClaw session JSON shape.
//
// Used by extractFromSessionLine to pull declarative facts out of structured
// session logs (cwd, model_change, user messages).
// ---------------------------------------------------------------------------

type sessionMessage struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	ParentID  string `json:"parentId,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
	Message   struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text,omitempty"`
		} `json:"content"`
	} `json:"message,omitempty"`
	CWD      string `json:"cwd,omitempty"`
	ModelID  string `json:"modelId,omitempty"`
	Provider string `json:"provider,omitempty"`
}

// ---------------------------------------------------------------------------
// File / JSONL helpers
// ---------------------------------------------------------------------------

// readJSONLines returns the non-empty trimmed lines of a JSONL file.
// Buffer is bumped to 1 MiB/line so big message blobs don't trip the
// default 64 KiB scanner limit.
func readJSONLines(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	const maxScanTokenSize = 1024 * 1024 // 1 MiB per line
	buf := make([]byte, maxScanTokenSize)

	var lines []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(buf, maxScanTokenSize)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines, scanner.Err()
}

// parseSessionsSnapshot extracts a structured, queryable snapshot from the
// raw bytes of an OpenClaw sessions.json file. Returns a flat map of the
// most useful fields (session_id, model, channel, skills, etc.) or nil on
// parse failure. The caller decides what to do with the snapshot.
func parseSessionsSnapshot(data []byte) (map[string]interface{}, error) {
	var sessions map[string]json.RawMessage
	if err := json.Unmarshal(data, &sessions); err != nil {
		return nil, err
	}

	// sessions.json is a map of sessionKey → sessionData. Prefer
	// "agent:main:main" if present, otherwise the first available key.
	var sessionKey string
	var sessionData map[string]interface{}
	for key, raw := range sessions {
		if err := json.Unmarshal(raw, &sessionData); err != nil {
			continue
		}
		sessionKey = key
		if key == "agent:main:main" {
			break
		}
		break // first available
	}
	if sessionData == nil {
		return nil, fmt.Errorf("no session data found in sessions.json")
	}

	snapshot := map[string]interface{}{
		"session_key":         sessionKey,
		"session_id":          "",
		"channel":             "",
		"model":               "",
		"provider":            "",
		"skills_count":        0,
		"skills":              []string{},
		"workspace_files":     []string{},
		"system_prompt_chars": 0,
		"context_tokens":      0,
		"runtime_ms":          int64(0),
		"updated_at":          int64(0),
	}

	if v, ok := sessionData["sessionId"].(string); ok {
		snapshot["session_id"] = v
	}
	if v, ok := sessionData["model"].(string); ok {
		snapshot["model"] = v
	}
	if v, ok := sessionData["modelProvider"].(string); ok {
		snapshot["provider"] = v
	}
	if v, ok := sessionData["contextTokens"].(float64); ok {
		snapshot["context_tokens"] = int(v)
	}
	if v, ok := sessionData["runtimeMs"].(float64); ok {
		snapshot["runtime_ms"] = int64(v)
	}
	if v, ok := sessionData["updatedAt"].(float64); ok {
		snapshot["updated_at"] = int64(v)
	}

	if dc, ok := sessionData["deliveryContext"].(map[string]interface{}); ok {
		if v, ok := dc["channel"].(string); ok {
			snapshot["channel"] = v
		}
	}

	if origin, ok := sessionData["origin"].(map[string]interface{}); ok {
		if v, ok := origin["label"].(string); ok {
			snapshot["origin_label"] = v
		}
	}

	// Extract skills from skillsSnapshot.skills
	var skillNames []string
	if ss, ok := sessionData["skillsSnapshot"].(map[string]interface{}); ok {
		if skills, ok := ss["skills"].([]interface{}); ok {
			for _, s := range skills {
				if m, ok := s.(map[string]interface{}); ok {
					if name, ok := m["name"].(string); ok {
						skillNames = append(skillNames, name)
					}
				}
			}
		}
		if spr, ok := ss["systemPromptReport"].(map[string]interface{}); ok {
			if files, ok := spr["injectedWorkspaceFiles"].([]interface{}); ok {
				for _, f := range files {
					if m, ok := f.(map[string]interface{}); ok {
						if name, ok := m["name"].(string); ok {
							snapshot["workspace_files"] = append(
								snapshot["workspace_files"].([]string), name)
						}
					}
				}
			}
		}
		if sp, ok := ss["systemPrompt"].(map[string]interface{}); ok {
			if v, ok := sp["chars"].(float64); ok {
				snapshot["system_prompt_chars"] = int(v)
			}
		}
	}
	snapshot["skills"] = skillNames
	snapshot["skills_count"] = len(skillNames)

	return snapshot, nil
}

// ---------------------------------------------------------------------------
// Fact / keyword extractors
// ---------------------------------------------------------------------------

// extractFacts scans JSON lines for declarative facts/configurations. JSON
// lines bypass the regex pass and go straight to extractFromSessionLine;
// non-JSON lines are scored against the factPatterns / skipPatterns
// heuristics. Verbose controls whether recognized facts are printed.
func extractFacts(lines []string, verbose bool) []string {
	var facts []string

	// Panic guard carried over from the original daemon — JSON parse errors
	// have surprised us before; recovery here costs nothing.
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "   ⚠️  extractFacts recovered from panic: %v\n", r)
		}
	}()

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if len(line) < 5 {
			continue
		}

		isJSON := strings.HasPrefix(line, "{")
		if !isJSON {
			shouldSkip := false
			for _, pattern := range skipPatterns {
				if pattern.MatchString(line) {
					shouldSkip = true
					break
				}
			}
			if shouldSkip {
				continue
			}

			for _, pattern := range factPatterns {
				if pattern.MatchString(line) {
					facts = append(facts, line)
					break
				}
			}
		}

		if fact := extractFromSessionLine(line); fact != "" {
			if verbose {
				fmt.Printf("   [fact] %s\n", fact[:min(80, len(fact))])
			}
			facts = append(facts, fact)
		}
	}

	// Deduplicate while preserving first-seen order.
	seen := make(map[string]bool)
	var unique []string
	for _, f := range facts {
		if !seen[f] {
			seen[f] = true
			unique = append(unique, f)
		}
	}
	return unique
}

// extractFromSessionLine pulls declarative facts out of one OpenClaw
// session JSON line (cwd, model_change, user messages that look like
// declarative statements). Returns "" if the line carries no fact.
func extractFromSessionLine(line string) string {
	var msg sessionMessage
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		return ""
	}

	switch msg.Type {
	case "session":
		if msg.CWD != "" {
			return fmt.Sprintf("session cwd: %s", msg.CWD)
		}
	case "model_change":
		if msg.ModelID != "" {
			return fmt.Sprintf("model: %s (provider: %s)", msg.ModelID, msg.Provider)
		}
	case "message":
		if msg.Message.Role == "user" {
			for _, content := range msg.Message.Content {
				if content.Type == "text" && len(content.Text) > 10 {
					text := strings.TrimSpace(content.Text)
					if looksLikeFact(text) {
						if len(text) > 200 {
							text = text[:200] + "..."
						}
						return text
					}
				}
			}
		}
	}
	return ""
}

// looksLikeFact decides whether a user-message line reads like a
// declarative fact rather than a command or social pleasantry.
func looksLikeFact(text string) bool {
	// Facts don't start with these patterns.
	nonFactIndicators := []string{
		"[cron:", "[heartbeat:", "[system",
		"Run ", "Check ", "Execute ",
		"808", "Hello", "Hey",
	}
	for _, indicator := range nonFactIndicators {
		if strings.HasPrefix(text, indicator) {
			return false
		}
	}
	for _, p := range looksLikeFactPatterns {
		if p.MatchString(text) {
			return true
		}
	}
	return false
}

// extractKeywords returns up to 5 tags pulled from #hashtags, CamelCase
// words, and a small set of known project names. Order is preserved; later
// matches can override earlier duplicates.
func extractKeywords(content string) []string {
	var tags []string
	seen := make(map[string]bool)

	for _, match := range hashPattern.FindAllStringSubmatch(content, -1) {
		tag := strings.ToLower(match[1])
		if !seen[tag] && len(tag) > 2 {
			tags = append(tags, tag)
			seen[tag] = true
		}
	}

	for _, match := range camelPattern.FindAllStringSubmatch(content, -1) {
		tag := strings.ToLower(match[1])
		if !seen[tag] && len(tag) > 2 {
			tags = append(tags, tag)
			seen[tag] = true
		}
	}

	knownProjects := []string{"mpm", "symai", "desp", "openclaw", "github"}
	contentLower := strings.ToLower(content)
	for _, project := range knownProjects {
		if strings.Contains(contentLower, project) && !seen[project] {
			tags = append(tags, project)
			seen[project] = true
		}
	}

	if len(tags) > 5 {
		tags = tags[:5]
	}
	return tags
}