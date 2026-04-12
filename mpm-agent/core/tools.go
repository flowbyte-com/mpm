package core

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Package-level compiled regexes for web scraping
var (
	linkPattern      = regexp.MustCompile(`<a href="(https?://[^"]+)"[^>]*class="[^"]*result[^"]*"[^>]*>([^<]+)</a>`)
	altPattern       = regexp.MustCompile(`<a href="(https?://[^"]+)"[^>]*>([^<]+)</a>`)
	urlSchemePattern = regexp.MustCompile(`^https?://`)
	scriptPattern    = regexp.MustCompile(`<script[^>]*>[\s\S]*?</script>`)
	stylePattern     = regexp.MustCompile(`<style[^>]*>[\s\S]*?</style>`)
	htmlTagPattern   = regexp.MustCompile(`<[^>]+>`)
	listPattern      = regexp.MustCompile(`^\d+\.`)
)

// ToolDefinition describes a callable tool.
type ToolDefinition struct {
	Name        string
	Description string
	InputSchema map[string]interface{}
}

// toolRegistry stores all registered tools.
var toolRegistry = make(map[string]ToolDefinition)
var toolsMu sync.RWMutex

// LoadedToolkits tracks which toolkits are currently loaded per-session.
// Key: sessionID, Value: map of toolkit name → true
var LoadedToolkits = make(map[string]map[string]bool)
var loadedMu sync.RWMutex

// RegisterTool adds a tool to the registry.
func RegisterTool(name string, def ToolDefinition) {
	toolsMu.Lock()
	defer toolsMu.Unlock()
	toolRegistry[name] = def
}

// GetTool returns a tool definition by name.
func GetTool(name string) (ToolDefinition, bool) {
	toolsMu.RLock()
	defer toolsMu.RUnlock()
	def, ok := toolRegistry[name]
	return def, ok
}

// ListTools returns all registered tools.
func ListTools() []ToolDefinition {
	toolsMu.RLock()
	defer toolsMu.RUnlock()
	result := make([]ToolDefinition, 0, len(toolRegistry))
	for _, def := range toolRegistry {
		result = append(result, def)
	}
	return result
}

// ListToolsByProfile returns tools matching the given profile (list of tool names).
func ListToolsByProfile(profile []string) []ToolDefinition {
	toolsMu.RLock()
	defer toolsMu.RUnlock()
	result := make([]ToolDefinition, 0, len(profile))
	for _, name := range profile {
		if def, ok := toolRegistry[name]; ok {
			result = append(result, def)
		}
	}
	return result
}

// LoadToolkit activates a toolkit for a session. Idempotent.
func LoadToolkit(sessionID, toolkit string) {
	loadedMu.Lock()
	defer loadedMu.Unlock()
	if LoadedToolkits[sessionID] == nil {
		LoadedToolkits[sessionID] = make(map[string]bool)
	}
	LoadedToolkits[sessionID][toolkit] = true
}

// UnloadToolkit deactivates a toolkit for a session.
func UnloadToolkit(sessionID, toolkit string) {
	loadedMu.Lock()
	defer loadedMu.Unlock()
	if LoadedToolkits[sessionID] != nil {
		delete(LoadedToolkits[sessionID], toolkit)
	}
}

// GetLoadedToolkits returns the list of loaded toolkit names for a session.
func GetLoadedToolkits(sessionID string) []string {
	loadedMu.RLock()
	defer loadedMu.RUnlock()
	if LoadedToolkits[sessionID] == nil {
		return nil
	}
	var names []string
	for k := range LoadedToolkits[sessionID] {
		names = append(names, k)
	}
	return names
}

// ClearSessionToolkits removes all toolkit state for a session.
func ClearSessionToolkits(sessionID string) {
	loadedMu.Lock()
	defer loadedMu.Unlock()
	delete(LoadedToolkits, sessionID)
}

// restrictPath validates path is within mpm-agent directory tree.
// Returns resolved absolute path or error if outside sandbox.
func restrictPath(path string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("invalid path: %w", err)
	}
	// Get mpm-agent root: executable is in <root>/cmd/telegram or <root>/cmd/mini-bot
	execPath, err := os.Executable()
	var root string
	if err == nil {
		root = filepath.Dir(filepath.Dir(execPath)) // dir of bin -> project root
	}
	if root == "" || root == "." {
		root, _ = os.Getwd()
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("invalid root: %w", err)
	}
	if !strings.HasPrefix(absPath, rootAbs) {
		return "", fmt.Errorf("path outside mpm-agent sandbox: %s", path)
	}
	return absPath, nil
}

// ExecuteSteps runs a list of steps sequentially and returns results.
// Each step is a map with "tool", "args", and optional "checkpoint".
func ExecuteSteps(steps []map[string]interface{}) ([]map[string]interface{}, error) {
	var results []map[string]interface{}
	for i, step := range steps {
		var tool string
		if t, ok := step["tool"].(string); ok {
			tool = t
		}
		var args map[string]interface{}
		if a, ok := step["args"].(map[string]interface{}); ok {
			args = a
		}
		var checkpoint string
		var hasCheckpoint bool
		if cp, ok := step["checkpoint"].(string); ok {
			checkpoint, hasCheckpoint = cp, true
		}

		stepResult := map[string]interface{}{
			"step": i,
		}
		if hasCheckpoint {
			stepResult["checkpoint"] = checkpoint
		}

		if tool == "" {
			if hasCheckpoint {
				stepResult["error"] = "no tool specified"
			}
			results = append(results, stepResult)
			continue
		}

		result, err := executeTool(tool, args)
		stepResult["tool"] = tool
		stepResult["result"] = result
		if err != nil {
			stepResult["error"] = err.Error()
		}
		results = append(results, stepResult)

		// Stop on error unless checkpoint says to continue
		if err != nil {
			break
		}
	}

	// Record remaining steps as skipped if we broke early
	initialLen := len(results)
	for i := initialLen; i < len(steps); i++ {
		step := steps[i]
		checkpoint, hasCheckpoint := step["checkpoint"].(string)
		stepResult := map[string]interface{}{
			"step": i,
		}
		if hasCheckpoint {
			stepResult["checkpoint"] = checkpoint
		}
		results = append(results, stepResult)
	}

	return results, nil
}

// executeTool runs a single tool by name with args.
func executeTool(tool string, args map[string]interface{}) (string, error) {
	switch tool {
	case "read_file":
		path, _ := args["path"].(string)
		if path == "" {
			return "", fmt.Errorf("read_file: path is required")
		}
		restricted, err := restrictPath(path)
		if err != nil {
			return "", err
		}
		content, err := os.ReadFile(restricted)
		if err != nil {
			return "", err
		}
		return string(content), nil

	case "write_file":
		path, _ := args["path"].(string)
		content, _ := args["content"].(string)
		if path == "" {
			return "", fmt.Errorf("write_file: path is required")
		}
		restricted, err := restrictPath(path)
		if err != nil {
			return "", err
		}
		if dir := filepath.Dir(restricted); dir != "" && dir != "." {
			os.MkdirAll(dir, 0755)
		}
		err = os.WriteFile(restricted, []byte(content), 0644)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Written %d bytes to %s", len(content), restricted), nil

	case "ReadFileSemantic":
		path, _ := args["path"].(string)
		mode, _ := args["mode"].(string)
		if path == "" {
			return "", fmt.Errorf("ReadFileSemantic: path is required")
		}
		restricted, err := restrictPath(path)
		if err != nil {
			return "", err
		}
		return ReadFileSemantic(restricted, mode), nil

	case "ReadFileCompare":
		pathA, _ := args["pathA"].(string)
		pathB, _ := args["pathB"].(string)
		if pathA == "" || pathB == "" {
			return "", fmt.Errorf("ReadFileCompare: pathA and pathB are required")
		}
		resA, err := restrictPath(pathA)
		if err != nil {
			return "", err
		}
		resB, err := restrictPath(pathB)
		if err != nil {
			return "", err
		}
		return ReadFileCompare(resA, resB), nil

	case "WebSynthesize":
		query, _ := args["query"].(string)
		if query == "" {
			return "", fmt.Errorf("WebSynthesize: query is required")
		}
		result, err := WebSynthesize(query)
		if err != nil {
			return "", err
		}
		return result, nil

	case "jq":
		filter, _ := args["filter"].(string)
		file, _ := args["file"].(string)
		if filter == "" || file == "" {
			return "", fmt.Errorf("jq: filter and file are required")
		}
		restricted, err := restrictPath(file)
		if err != nil {
			return "", err
		}
		if !strings.HasSuffix(restricted, ".json") && !strings.HasSuffix(restricted, ".jsonl") {
			return "", fmt.Errorf("jq: only *.json and *.jsonl files allowed")
		}
		out, err := exec.Command("jq", filter, restricted).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("jq error: %v\n%s", err, string(out))
		}
		return string(out), nil

	case "list_toolkits":
		// Returns available toolkits and their tools
		loaded := GetLoadedToolkits("telegram")
		var sb strings.Builder
		sb.WriteString("Available toolkits:\n")
		// Hardcoded for now — toolkits are registered at init
		toolkitInfo := map[string]string{
			"files": "read_file, write_file, ReadFileSemantic, ReadFileCompare — file operations in mpm-agent/",
			"web":  "WebSynthesize — DuckDuckGo search with citations",
			"jq":   "jq — query and transform JSON files",
			"mpm":  "execute_mpm_command — all MPM CLI commands (recall, mode, persona, etc.)",
		}
		for name, desc := range toolkitInfo {
			loadedMark := ""
			for _, l := range loaded {
				if l == name {
					loadedMark = " [LOADED]"
					break
				}
			}
			sb.WriteString(fmt.Sprintf("  %s%s — %s\n", name, loadedMark, desc))
		}
		sb.WriteString("\nUse load_toolkit(\"<name>\") to load a toolkit.")
		return sb.String(), nil

	case "load_toolkit":
		// This is intercepted in the agent loop — but if called directly, execute here
		name, _ := args["name"].(string)
		if name == "" {
			return "", fmt.Errorf("load_toolkit: name is required")
		}
		LoadToolkit("telegram", name)
		return fmt.Sprintf("Toolkit '%s' loaded.", name), nil

	case "unload_toolkit":
		name, _ := args["name"].(string)
		if name == "" {
			return "", fmt.Errorf("unload_toolkit: name is required")
		}
		UnloadToolkit("telegram", name)
		return fmt.Sprintf("Toolkit '%s' unloaded.", name), nil

	default:
		return "", fmt.Errorf("unknown tool: %s", tool)
	}
}

// ReadFileSemantic reads a file with semantic understanding.
// mode: "full" (default), "summary", "code", "compare"
func ReadFileSemantic(path, mode string) string {
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("error reading %s: %v", path, err)
	}

	text := string(content)
	switch mode {
	case "summary":
		return summarizeDocument(text)
	case "code":
		var codeLines []string
		for i, line := range strings.Split(text, "\n") {
			if strings.Contains(line, "func ") || strings.Contains(line, "type ") ||
				strings.Contains(line, "const ") || strings.Contains(line, "var ") {
				codeLines = append(codeLines, fmt.Sprintf("%d: %s", i+1, line))
			}
		}
		if len(codeLines) == 0 {
			return "No code structures found"
		}
		return strings.Join(codeLines, "\n")
	case "compare":
		return "compare mode requires path in format: fileA::fileB"
	case "full":
		return truncate(text, 2000)
	default:
		return truncate(text, 2000)
	}
}

// ReadFileCompare compares two files and returns a diff-like output.
func ReadFileCompare(pathA, pathB string) string {
	contentA, err := os.ReadFile(pathA)
	if err != nil {
		return fmt.Sprintf("error reading %s: %v", pathA, err)
	}
	contentB, err := os.ReadFile(pathB)
	if err != nil {
		return fmt.Sprintf("error reading %s: %v", pathB, err)
	}

	linesA := strings.Split(strings.TrimRight(string(contentA), "\n"), "\n")
	linesB := strings.Split(strings.TrimRight(string(contentB), "\n"), "\n")

	var result strings.Builder
	result.WriteString(fmt.Sprintf("Compare: %s :: %s\n\n", pathA, pathB))
	result.WriteString(fmt.Sprintf("File A: %d lines | File B: %d lines\n\n", len(linesA), len(linesB)))

	// Simple line-by-line comparison
	maxLines := len(linesA)
	if len(linesB) > maxLines {
		maxLines = len(linesB)
	}

	added, removed, modified := 0, 0, 0
	var diffLines []string

	for i := 0; i < maxLines; i++ {
		var lineA, lineB string
		if i < len(linesA) {
			lineA = linesA[i]
		}
		if i < len(linesB) {
			lineB = linesB[i]
		}

		if lineA == lineB {
			diffLines = append(diffLines, fmt.Sprintf("  %4d: %s", i+1, lineA))
		} else {
			if lineA == "" {
				diffLines = append(diffLines, fmt.Sprintf("+ %4d: %s", i+1, lineB))
				added++
			} else if lineB == "" {
				diffLines = append(diffLines, fmt.Sprintf("- %4d: %s", i+1, lineA))
				removed++
			} else {
				diffLines = append(diffLines, fmt.Sprintf("- %4d: %s", i+1, lineA))
				diffLines = append(diffLines, fmt.Sprintf("+ %4d: %s", i+1, lineB))
				modified++
			}
		}
	}

	result.WriteString(fmt.Sprintf("Stats: %d added, %d removed, %d modified\n\n", added, removed, modified))
	result.WriteString("Diff:\n")
	result.WriteString(strings.Join(diffLines, "\n"))

	return result.String()
}

// summarizeDocument extracts structure and key points from text.
func summarizeDocument(text string) string {
	lines := strings.Split(text, "\n")
	totalLines := len(lines)

	var headers []string
	var keySections []string
	var inCodeBlock bool

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "```") {
			inCodeBlock = !inCodeBlock
			continue
		}
		if inCodeBlock || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "#") {
			continue
		}

		if (strings.HasPrefix(trimmed, "#") && len(trimmed) > 1) ||
			(len(trimmed) > 3 && len(trimmed) < 80 && trimmed == strings.ToUpper(trimmed) &&
				!strings.HasSuffix(trimmed, ":") && !strings.Contains(trimmed, " ")) {
			headers = append(headers, fmt.Sprintf("  Line %d: %s", i+1, truncated(trimmed, 60)))
		}

		if len(trimmed) > 50 && !strings.HasPrefix(trimmed, "#") &&
			(strings.Contains(trimmed, ":") || strings.Contains(trimmed, ".") ||
				strings.Contains(trimmed, "-") || listPattern.MatchString(trimmed)) {
			keySections = append(keySections, fmt.Sprintf("  Line %d: %s", i+1, truncated(trimmed, 70)))
		}
	}

	var result strings.Builder
	result.WriteString(fmt.Sprintf("## Document Summary\n\nFile has %d lines.\n\n", totalLines))

	if len(headers) > 0 {
		result.WriteString("### Structure (Headers)\n")
		for _, h := range headers {
			result.WriteString(h + "\n")
		}
		result.WriteString("\n")
	}

	if len(keySections) > 0 {
		result.WriteString("### Key Sections\n")
		for _, s := range keySections[:10] {
			result.WriteString(s + "\n")
		}
		if len(keySections) > 10 {
			result.WriteString(fmt.Sprintf("  ... (%d more sections)\n", len(keySections)-10))
		}
		result.WriteString("\n")
	}

	result.WriteString("### Preview\n")
	previewLines := 8
	if totalLines < previewLines {
		previewLines = totalLines
	}
	for i := 0; i < previewLines; i++ {
		result.WriteString(fmt.Sprintf("  %4d: %s\n", i+1, truncated(lines[i], 80)))
	}
	if totalLines > previewLines {
		result.WriteString(fmt.Sprintf("  ... (%d more lines)\n", totalLines-previewLines))
	}

	return result.String()
}

// truncate truncates text to maxLen characters.
func truncate(text string, maxLen int) string {
	if len(text) <= maxLen {
		return text
	}
	return text[:maxLen] + "..."
}

// truncated is a local alias for truncate to avoid conflicts.
func truncated(text string, maxLen int) string {
	return truncate(text, maxLen)
}

// WebSynthesize searches for a query and produces a synthesized answer with proper citations.
func WebSynthesize(query string) (string, error) {
	searchURL := "https://duckduckgo.com/html/?q=" + url.QueryEscape(query)
	req, _ := http.NewRequest("GET", searchURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("web search: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 15000))
	if err != nil {
		return "", fmt.Errorf("reading response body: %w", err)
	}

	html := string(body)

	// Parse titles and URLs from search results
	var citations []struct {
		title string
		url   string
	}

	// DuckDuckGo HTML result patterns
	// Pattern: <a href="URL" class="result__a">TITLE</a>
	matches := linkPattern.FindAllStringSubmatch(html, -1)

	// Also try alternative pattern for result titles
	altMatches := altPattern.FindAllStringSubmatch(html, -1)

	seen := make(map[string]bool)
	for _, m := range matches {
		if len(m) == 3 {
			url := strings.TrimSpace(m[1])
			title := strings.TrimSpace(unescapeHTML(m[2]))
			if url != "" && !seen[url] && len(title) > 5 {
				seen[url] = true
				citations = append(citations, struct {
					title string
					url   string
				}{title: title, url: url})
			}
		}
	}

	// Fallback: try alternative matches
	if len(citations) == 0 {
		for _, m := range altMatches {
			if len(m) == 3 {
				url := strings.TrimSpace(m[1])
				title := strings.TrimSpace(m[2])
				if url != "" && !seen[url] && len(title) > 5 &&
					urlSchemePattern.MatchString(url) {
					seen[url] = true
					citations = append(citations, struct {
						title string
						url   string
					}{title: title, url: url})
				}
			}
		}
	}

	// Extract key facts from content
	lines := strings.Split(html, "\n")
	var facts []string
	for _, line := range lines {
		// Look for result snippets
		if strings.Contains(line, "result__snippet") || strings.Contains(line, "snippet") {
			// Extract text between tags
			text := stripHTML(line)
			if len(text) > 30 {
				facts = append(facts, truncate(strings.TrimSpace(text), 200))
			}
		}
	}

	// Build output
	var result bytes.Buffer
	result.WriteString(fmt.Sprintf("## Synthesis for: %s\n\n", query))

	if len(citations) > 0 {
		result.WriteString("### Sources\n")
		for i, c := range citations {
			if i >= 5 { // Limit to 5 sources
				break
			}
			result.WriteString(fmt.Sprintf("%d. [%s](%s)\n", i+1, c.title, c.url))
		}
		result.WriteString("\n")
	}

	if len(facts) > 0 {
		result.WriteString("### Key Information\n")
		factCount := 5
		if len(facts) < factCount {
			factCount = len(facts)
		}
		for _, fact := range facts[:factCount] {
			result.WriteString(fmt.Sprintf("- %s\n", fact))
		}
	} else {
		result.WriteString("No additional details found.\n")
	}

	return result.String(), nil
}

// stripHTML removes HTML tags from text.
func stripHTML(html string) string {
	// Remove script and style blocks
	html = scriptPattern.ReplaceAllString(html, "")
	html = stylePattern.ReplaceAllString(html, "")
	// Remove all HTML tags
	html = htmlTagPattern.ReplaceAllString(html, "")
	// Decode HTML entities
	html = unescapeHTML(html)
	return strings.TrimSpace(html)
}

// unescapeHTML converts common HTML entities to characters.
func unescapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&quot;", "\"")
	s = strings.ReplaceAll(s, "&#39;", "'")
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	return s
}

// init registers all standard tools on package load.
func init() {
	RegisterTool("read_file", ToolDefinition{
		Name:        "read_file",
		Description: "Read the full contents of a file within the mpm-agent directory.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "Relative or absolute path to the file",
				},
			},
			"required": []string{"path"},
		},
	})
	RegisterTool("write_file", ToolDefinition{
		Name:        "write_file",
		Description: "Write content to a file within the mpm-agent directory. Creates or overwrites.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "Relative or absolute path to the file",
				},
				"content": map[string]interface{}{
					"type":        "string",
					"description": "The content to write",
				},
			},
			"required": []string{"path", "content"},
		},
	})
	RegisterTool("ReadFileSemantic", ToolDefinition{
		Name:        "ReadFileSemantic",
		Description: "Read a file with semantic understanding. Modes: summary (structure overview), code (function/type lines), compare (diff two files).",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "Path to the file",
				},
				"mode": map[string]interface{}{
					"type":        "string",
					"description": "Mode: summary, code, compare",
				},
			},
			"required": []string{"path"},
		},
	})
	RegisterTool("ReadFileCompare", ToolDefinition{
		Name:        "ReadFileCompare",
		Description: "Compare two files and show a diff-like output with line-level changes.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"pathA": map[string]interface{}{
					"type":        "string",
					"description": "Path to the first file",
				},
				"pathB": map[string]interface{}{
					"type":        "string",
					"description": "Path to the second file",
				},
			},
			"required": []string{"pathA", "pathB"},
		},
	})
	RegisterTool("WebSynthesize", ToolDefinition{
		Name:        "WebSynthesize",
		Description: "Search the web using DuckDuckGo and produce a synthesized answer with citations.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{
					"type":        "string",
					"description": "The search query",
				},
			},
			"required": []string{"query"},
		},
	})
	RegisterTool("jq", ToolDefinition{
		Name:        "jq",
		Description: "Execute a jq filter on a JSON file. Only *.json and *.jsonl files within mpm-agent are allowed.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"filter": map[string]interface{}{
					"type":        "string",
					"description": "The jq filter expression (e.g., '.name' or '.[]|.id')",
				},
				"file": map[string]interface{}{
					"type":        "string",
					"description": "Path to the JSON file (*.json or *.jsonl)",
				},
			},
			"required": []string{"filter", "file"},
		},
	})
	RegisterTool("list_toolkits", ToolDefinition{
		Name:        "list_toolkits",
		Description: "List available toolkits and their current load status.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{},
		},
	})
	RegisterTool("load_toolkit", ToolDefinition{
		Name:        "load_toolkit",
		Description: "Load a toolkit to unlock its tools. Use list_toolkits to see available options.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{
					"type":        "string",
					"description": "Toolkit name to load",
				},
			},
			"required": []string{"name"},
		},
	})
	RegisterTool("unload_toolkit", ToolDefinition{
		Name:        "unload_toolkit",
		Description: "Unload a toolkit to free up context space.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{
					"type":        "string",
					"description": "Toolkit name to unload",
				},
			},
			"required": []string{"name"},
		},
	})
}
