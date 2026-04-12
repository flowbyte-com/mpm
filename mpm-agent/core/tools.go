package core

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
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
	case "shell":
		var cmd string
		if c, ok := args["command"].(string); ok {
			cmd = c
		}
		out, err := exec.Command("sh", "-c", cmd).CombinedOutput()
		if err != nil {
			return string(out), err
		}
		return string(out), nil
	case "read_file":
		var path string
		if p, ok := args["path"].(string); ok {
			path = p
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return string(content), nil
	case "write_file":
		var path string
		if p, ok := args["path"].(string); ok {
			path = p
		}
		var content string
		if c, ok := args["content"].(string); ok {
			content = c
		}
		err := os.WriteFile(path, []byte(content), 0644)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Written %d bytes", len(content)), nil
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
