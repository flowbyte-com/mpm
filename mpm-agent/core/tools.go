package core

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
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

// resolveWorkspace returns the absolute workspace root path.
func resolveWorkspace(configured string) string {
	if configured == "" || configured == "." {
		cwd, _ := os.Getwd()
		return cwd
	}
	return os.ExpandEnv(configured)
}

// runRg executes ripgrep with JSON output, truncates at 50 results.
func runRg(query, path, fileFilter string) (string, error) {
	absPath := path
	if absPath == "" {
		absPath = resolveWorkspace("")
	}
	args := []string{"--json", query, absPath}
	if fileFilter != "" {
		args = []string{"--json", "--glob", fileFilter, query, absPath}
	}
	cmd := exec.Command("rg", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("rg error: %v\n%s", err, string(out))
	}
	// Truncate at 50 lines
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) > 50 {
		lines = lines[:50]
		result := strings.Join(lines, "\n")
		return result + "\n[Truncated at 50 results. Please refine your search.]", nil
	}
	return string(out), nil
}

// runSg executes ast-grep. If rule is provided use --rule, else use --query.
func runSg(path, rule, query string) (string, error) {
	absPath := path
	if absPath == "" {
		return "", fmt.Errorf("sg: path is required")
	}
	var cmd *exec.Cmd
	if rule != "" {
		cmd = exec.Command("sg", "query", "--rule", rule, absPath)
	} else if query != "" {
		cmd = exec.Command("sg", "query", "--query", query, absPath)
	} else {
		// List available rules
		cmd = exec.Command("sg", "lsm")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("sg lsm error: %v\n%s", err, string(out))
		}
		return string(out), nil
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("sg error: %v\n%s", err, string(out))
	}
	return string(out), nil
}

// runRepomap generates a symbol map using find + single rg batch call.
func runRepomap(path string, depth int) (string, error) {
	absPath := path
	if absPath == "" {
		absPath = resolveWorkspace("")
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("## Repo Map (depth=%d)\n\n", depth))

	cmd := exec.Command("find", absPath, "-maxdepth", fmt.Sprintf("%d", depth), "-name", "*.go", "-type", "f")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("find .go files: %v", err)
	}

	files := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(files) == 0 || (len(files) == 1 && files[0] == "") {
		return "", fmt.Errorf("no .go files found")
	}

	// Limit to 20 files
	if len(files) > 20 {
		files = files[:20]
		sb.WriteString("... (cap at 20 files)\n\n")
	}

	// Single rg call for all files
	args := []string{"--json", `^\s*(func|type|struct)\s+`}
	args = append(args, files...)
	grepCmd := exec.Command("rg", args...)
	grepOut, err := grepCmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("rg error: %v", err)
	}

	// Group output by file
	currentFile := ""
	for _, line := range strings.Split(strings.TrimRight(string(grepOut), "\n"), "\n") {
		if line == "" {
			continue
		}
		// Extract file path from JSON (ripgrep outputs JSON with file path)
		if strings.Contains(line, `"type":"match"`) {
			// Parse file from ripgrep JSON
			var entry struct {
				Type  string `json:"type"`
				Path  string `json:"path"`
				Lines string `json:"lines"`
				Text  string `json:"text"`
			}
			if err := json.Unmarshal([]byte(line), &entry); err == nil && entry.Type == "match" {
				if entry.Path != currentFile {
					if currentFile != "" {
						sb.WriteString("\n")
					}
					currentFile = entry.Path
					relPath, _ := filepath.Rel(absPath, currentFile)
					sb.WriteString(fmt.Sprintf("## %s\n", relPath))
				}
				if entry.Lines != "" {
					sb.WriteString(fmt.Sprintf("  %s\n", entry.Lines))
				}
			}
		}
	}

	return sb.String(), nil
}

// runGitStatus returns git status output.
func runGitStatus(repoPath string) (string, error) {
	absRepo := repoPath
	if absRepo == "" {
		absRepo = resolveWorkspace("")
	}
	cmd := exec.Command("git", "-C", absRepo, "status", "--porcelain")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git status error: %v", err)
	}
	return string(out), nil
}

// runGitCommit creates a commit.
func runGitCommit(message string) (string, error) {
	cmd := exec.Command("git", "add", "-A")
	cmd.Run()
	cmd = exec.Command("git", "commit", "-m", message)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git commit error: %v\n%s", err, string(out))
	}
	return string(out), nil
}

// runGitDiff returns git diff output.
func runGitDiff(file string) (string, error) {
	repo := resolveWorkspace("")
	var cmd *exec.Cmd
	if file != "" {
		cmd = exec.Command("git", "-C", repo, "diff", "--", file)
	} else {
		cmd = exec.Command("git", "-C", repo, "diff")
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git diff error: %v", err)
	}
	return string(out), nil
}

// runShell executes an arbitrary shell command.
func runShell(command, cwd string) (string, error) {
	absCwd := cwd
	if absCwd == "" {
		absCwd = resolveWorkspace("")
	}
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = absCwd
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("shell error: %v\n%s", err, string(out))
	}
	return string(out), nil
}

// ExecuteSteps runs a list of steps sequentially and returns results.
// Each step is a map with "tool", "args", and optional "checkpoint".
// sessionID is used by toolkit management tools to track loaded toolkits.
func ExecuteSteps(steps []map[string]interface{}, sessionID string) ([]map[string]interface{}, error) {
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

		result, err := executeTool(tool, args, sessionID)
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
// sessionID is used by toolkit management tools (list_toolkits, load_toolkit, unload_toolkit)
// to track which toolkits are loaded for the current session.
func executeTool(tool string, args map[string]interface{}, sessionID string) (string, error) {
	switch tool {
	case "read_file":
		path, _ := args["path"].(string)
		if path == "" {
			return "", fmt.Errorf("read_file: path is required")
		}
		absPath, err := filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("read_file: invalid path: %w", err)
		}
		content, err := os.ReadFile(absPath)
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
		absPath, err := filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("write_file: invalid path: %w", err)
		}
		if dir := filepath.Dir(absPath); dir != "" && dir != "." {
			os.MkdirAll(dir, 0755)
		}
		err = os.WriteFile(absPath, []byte(content), 0644)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Written %d bytes to %s", len(content), absPath), nil

	case "ReadFileSemantic":
		path, _ := args["path"].(string)
		mode, _ := args["mode"].(string)
		if path == "" {
			return "", fmt.Errorf("ReadFileSemantic: path is required")
		}
		absPath, err := filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("ReadFileSemantic: invalid path: %w", err)
		}
		return ReadFileSemantic(absPath, mode), nil

	case "ReadFileCompare":
		pathA, _ := args["pathA"].(string)
		pathB, _ := args["pathB"].(string)
		if pathA == "" || pathB == "" {
			return "", fmt.Errorf("ReadFileCompare: pathA and pathB are required")
		}
		absA, err := filepath.Abs(pathA)
		if err != nil {
			return "", fmt.Errorf("ReadFileCompare: invalid pathA: %w", err)
		}
		absB, err := filepath.Abs(pathB)
		if err != nil {
			return "", fmt.Errorf("ReadFileCompare: invalid pathB: %w", err)
		}
		return ReadFileCompare(absA, absB), nil

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
		absFile, err := filepath.Abs(file)
		if err != nil {
			return "", fmt.Errorf("jq: invalid path: %w", err)
		}
		out, err := exec.Command("jq", filter, absFile).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("jq error: %v\n%s", err, string(out))
		}
		return string(out), nil

	case "list_toolkits":
		// Returns available toolkits and their tools
		loaded := GetLoadedToolkits(sessionID)
		var sb strings.Builder
		sb.WriteString("Available toolkits:\n")
		// Hardcoded for now — toolkits are registered at init
		toolkitInfo := map[string]string{
			"files":   "read_file, write_file, ReadFileSemantic, ReadFileCompare — file operations in mpm-agent/",
			"web":     "WebSynthesize — DuckDuckGo search with citations",
			"jq":      "jq — query and transform JSON files",
			"mpm":     "execute_mpm_command — all MPM CLI commands (recall, mode, persona, etc.)",
			"minimax": "generate_image, synthesize_speech, web_search, understand_image — MiniMax Token Plan features",
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
		LoadToolkit(sessionID, name)
		return fmt.Sprintf("Toolkit '%s' loaded.", name), nil

	case "unload_toolkit":
		name, _ := args["name"].(string)
		if name == "" {
			return "", fmt.Errorf("unload_toolkit: name is required")
		}
		UnloadToolkit(sessionID, name)
		return fmt.Sprintf("Toolkit '%s' unloaded.", name), nil

	case "rg":
		query, _ := args["query"].(string)
		searchPath, _ := args["path"].(string)
		fileFilter, _ := args["file_filter"].(string)
		if query == "" {
			return "", fmt.Errorf("rg: query is required")
		}
		return runRg(query, searchPath, fileFilter)

	case "sg":
		path, _ := args["path"].(string)
		rule, _ := args["rule"].(string)
		q, _ := args["query"].(string)
		if path == "" {
			return "", fmt.Errorf("sg: path is required")
		}
		return runSg(path, rule, q)

	case "repomap":
		path, _ := args["path"].(string)
		depth, _ := args["depth"].(int)
		if depth == 0 {
			depth = 2
		}
		return runRepomap(path, depth)

	case "git_status":
		repo, _ := args["repo"].(string)
		return runGitStatus(repo)

	case "git_commit":
		message, _ := args["message"].(string)
		if message == "" {
			return "", fmt.Errorf("git_commit: message is required")
		}
		return runGitCommit(message)

	case "git_diff":
		file, _ := args["file"].(string)
		return runGitDiff(file)

	case "execute_shell":
		command, _ := args["command"].(string)
		cwd, _ := args["cwd"].(string)
		if command == "" {
			return "", fmt.Errorf("execute_shell: command is required")
		}
		return runShell(command, cwd)

	case "generate_image":
		prompt, _ := args["prompt"].(string)
		aspectRatio, _ := args["aspect_ratio"].(string)
		if prompt == "" {
			return "", fmt.Errorf("generate_image: prompt is required")
		}
		if aspectRatio == "" {
			aspectRatio = "1:1"
		}
		return generateImage(prompt, aspectRatio)

	case "synthesize_speech":
		text, _ := args["text"].(string)
		voice, _ := args["voice"].(string)
		if text == "" {
			return "", fmt.Errorf("synthesize_speech: text is required")
		}
		return synthesizeSpeech(text, voice)

	case "web_search":
		query, _ := args["query"].(string)
		if query == "" {
			return "", fmt.Errorf("web_search: query is required")
		}
		return webSearch(query)

	case "understand_image":
		imagePath, _ := args["image_path"].(string)
		prompt, _ := args["prompt"].(string)
		if imagePath == "" || prompt == "" {
			return "", fmt.Errorf("understand_image: image_path and prompt are required")
		}
		return understandImage(imagePath, prompt)

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
			headers = append(headers, fmt.Sprintf("  Line %d: %s", i+1, truncate(trimmed, 60)))
		}

		if len(trimmed) > 50 && !strings.HasPrefix(trimmed, "#") &&
			(strings.Contains(trimmed, ":") || strings.Contains(trimmed, ".") ||
				strings.Contains(trimmed, "-") || listPattern.MatchString(trimmed)) {
			keySections = append(keySections, fmt.Sprintf("  Line %d: %s", i+1, truncate(trimmed, 70)))
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
		result.WriteString(fmt.Sprintf("  %4d: %s\n", i+1, truncate(lines[i], 80)))
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

var minimaxImageCacheDir string
var cachedAPIKey string

func getMediaDir() string {
	if minimaxImageCacheDir != "" {
		return minimaxImageCacheDir
	}
	execPath, _ := os.Executable()
	binDir := filepath.Dir(execPath)
	mediaDir := filepath.Join(binDir, "media")
	os.MkdirAll(mediaDir, 0755)
	minimaxImageCacheDir = mediaDir
	return mediaDir
}

func getMiniMaxAPIKey() string {
	if cachedAPIKey != "" {
		return cachedAPIKey
	}
	if key := os.Getenv("MINIMAX_API_KEY"); key != "" {
		cachedAPIKey = key
		return key
	}
	cfg, _ := LoadMiniBotConfig(GetConfigPath())
	if cfg != nil && cfg.Synth.APIKey != "" {
		cachedAPIKey = cfg.Synth.APIKey
		return cachedAPIKey
	}
	return ""
}

func generateImage(prompt, aspectRatio string) (string, error) {
	apiKey := getMiniMaxAPIKey()
	if apiKey == "" {
		return "", fmt.Errorf("generate_image: MINIMAX_API_KEY not set")
	}

	url := "https://api.minimax.io/v1/image_generation"
	payload := map[string]interface{}{
		"model":           "image-01",
		"prompt":          prompt,
		"aspect_ratio":    aspectRatio,
		"response_format": "base64",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("generate_image: marshal: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("generate_image: request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("generate_image: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("generate_image: read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("generate_image: API error %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Data struct {
			ImageBase64 []string `json:"image_base64"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("generate_image: parse: %w", err)
	}

	if len(result.Data.ImageBase64) == 0 {
		return "", fmt.Errorf("generate_image: no images returned")
	}

	mediaDir := getMediaDir()
	for i, b64 := range result.Data.ImageBase64 {
		data, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			continue
		}
		filename := filepath.Join(mediaDir, fmt.Sprintf("img_%s_%d.jpg", fmt.Sprintf("%x", time.Now().UnixNano())[:16], i))
		if err := os.WriteFile(filename, data, 0600); err != nil {
			continue
		}
		return filename, nil
	}
	return "", fmt.Errorf("generate_image: failed to save image")
}

func synthesizeSpeech(text, voice string) (string, error) {
	apiKey := getMiniMaxAPIKey()
	if apiKey == "" {
		return "", fmt.Errorf("synthesize_speech: MINIMAX_API_KEY not set")
	}

	if voice == "" {
		voice = "female-qn-qingse"
	}

	url := "https://api.minimax.io/v1/t2a"
	payload := map[string]interface{}{
		"model":           "speech-2.8",
		"text":            text,
		"voice_id":        voice,
		"response_format": "mp3",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("synthesize_speech: marshal: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("synthesize_speech: request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("synthesize_speech: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("synthesize_speech: API error %d: %s", resp.StatusCode, string(respBody))
	}

	mediaDir := getMediaDir()
	filename := filepath.Join(mediaDir, fmt.Sprintf("speech_%s.mp3", fmt.Sprintf("%x", time.Now().UnixNano())[:16]))
	out, err := os.Create(filename)
	if err != nil {
		return "", fmt.Errorf("synthesize_speech: create file: %w", err)
	}
	defer out.Close()

	if _, err := io.Copy(out, resp.Body); err != nil {
		return "", fmt.Errorf("synthesize_speech: write: %w", err)
	}
	return filename, nil
}

func webSearch(query string) (string, error) {
	apiKey := getMiniMaxAPIKey()
	if apiKey == "" {
		return "", fmt.Errorf("web_search: MINIMAX_API_KEY not set")
	}

	url := "https://api.minimax.io/v1/search"
	payload := map[string]interface{}{
		"model":         "minimax-text-01",
		"query":         query,
		"search_result": true,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("web_search: marshal: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("web_search: request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("web_search: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 15000))
	if err != nil {
		return "", fmt.Errorf("web_search: read: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("web_search: API error %d: %s", resp.StatusCode, string(respBody))
	}

	return string(respBody), nil
}

func understandImage(imagePath, prompt string) (string, error) {
	apiKey := getMiniMaxAPIKey()
	if apiKey == "" {
		return "", fmt.Errorf("understand_image: MINIMAX_API_KEY not set")
	}

	absPath, err := filepath.Abs(imagePath)
	if err != nil {
		return "", fmt.Errorf("understand_image: invalid path: %w", err)
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", fmt.Errorf("understand_image: read: %w", err)
	}

	mediaDir := getMediaDir()
	tmpFile := filepath.Join(mediaDir, fmt.Sprintf("tmp_%s.jpg", fmt.Sprintf("%x", time.Now().UnixNano())[:8]))
	if err := os.WriteFile(tmpFile, data, 0600); err != nil {
		return "", fmt.Errorf("understand_image: temp file: %w", err)
	}
	defer os.Remove(tmpFile)

	url := "https://api.minimax.io/v1/image_understanding"
	payload := map[string]interface{}{
		"model":        "image-01",
		"image_base64": base64.StdEncoding.EncodeToString(data),
		"prompt":       prompt,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("understand_image: marshal: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("understand_image: request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("understand_image: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("understand_image: read: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("understand_image: API error %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Data struct {
			Text string `json:"text"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("understand_image: parse: %w", err)
	}
	return result.Data.Text, nil
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
			"type":       "object",
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

	// MiniMax Token Plan tools
	// Coding tools
	RegisterTool("rg", ToolDefinition{
		Name:        "rg",
		Description: "Search files using ripgrep. Returns JSON results (first 50). Use for finding code patterns, function definitions, imports.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query":       map[string]interface{}{"type": "string", "description": "Regex search pattern"},
				"path":        map[string]interface{}{"type": "string", "description": "Directory to search (default: workspace root)"},
				"file_filter": map[string]interface{}{"type": "string", "description": "Glob filter, e.g. *.go"},
			},
			"required": []string{"query"},
		},
	})

	RegisterTool("sg", ToolDefinition{
		Name:        "sg",
		Description: "Run ast-grep code analysis. Use 'rule' for named rules (e.g. return-error-no-log) or 'query' for custom patterns.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":  map[string]interface{}{"type": "string", "description": "Directory or file to analyze"},
				"rule":  map[string]interface{}{"type": "string", "description": "ast-grep rule name (e.g. return-error-no-log)"},
				"query": map[string]interface{}{"type": "string", "description": "Custom ast-grep query pattern"},
			},
			"required": []string{"path"},
		},
	})

	RegisterTool("repomap", ToolDefinition{
		Name:        "repomap",
		Description: "Generate a symbol map of a project (functions, structs, types). Use depth to control traversal depth.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":  map[string]interface{}{"type": "string", "description": "Project root (default: workspace root)"},
				"depth": map[string]interface{}{"type": "integer", "description": "Traversal depth (default: 2)"},
			},
		},
	})

	RegisterTool("git_status", ToolDefinition{
		Name:        "git_status",
		Description: "Show git worktree status. Returns list of modified, staged, untracked files.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"repo": map[string]interface{}{"type": "string", "description": "Repository path (default: workspace root)"},
			},
		},
	})

	RegisterTool("git_commit", ToolDefinition{
		Name:        "git_commit",
		Description: "Create a git commit with the given message.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"message": map[string]interface{}{"type": "string", "description": "Commit message"},
			},
			"required": []string{"message"},
		},
	})

	RegisterTool("git_diff", ToolDefinition{
		Name:        "git_diff",
		Description: "Show uncommitted changes. Use 'file' to diff a specific file.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"file": map[string]interface{}{"type": "string", "description": "Specific file to diff (default: all)"},
			},
		},
	})

	RegisterTool("execute_shell", ToolDefinition{
		Name:        "execute_shell",
		Description: "Execute an arbitrary shell command with the bot's permissions.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"command": map[string]interface{}{"type": "string", "description": "Shell command to execute"},
				"cwd":     map[string]interface{}{"type": "string", "description": "Working directory (default: workspace root)"},
			},
			"required": []string{"command"},
		},
	})

}
