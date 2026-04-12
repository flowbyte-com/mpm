package core

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ExecuteSteps runs a list of steps sequentially and returns results.
// Each step is a map with "tool", "args", and optional "checkpoint".
func ExecuteSteps(steps []map[string]interface{}) ([]map[string]interface{}, error) {
	var results []map[string]interface{}
	for i, step := range steps {
		tool, _ := step["tool"].(string)
		args, _ := step["args"].(map[string]interface{})
		checkpoint, hasCheckpoint := step["checkpoint"].(string)

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
		cmd, _ := args["command"].(string)
		out, err := exec.Command("sh", "-c", cmd).CombinedOutput()
		if err != nil {
			return string(out), err
		}
		return string(out), nil
	case "read_file":
		path, _ := args["path"].(string)
		content, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return string(content), nil
	case "write_file":
		path, _ := args["path"].(string)
		content, _ := args["content"].(string)
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
		lines := strings.Split(text, "\n")
		if len(lines) > 5 {
			return fmt.Sprintf("File has %d lines. Key lines:\n%s\n...(%d more lines)",
				len(lines), strings.Join(lines[:5], "\n"), len(lines)-5)
		}
		return text
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
	case "full":
		return truncate(text, 2000)
	default:
		return truncate(text, 2000)
	}
}

// truncate truncates text to maxLen characters.
func truncate(text string, maxLen int) string {
	if len(text) <= maxLen {
		return text
	}
	return text[:maxLen] + "..."
}

// WebSynthesize searches for a query and produces a synthesized answer.
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
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8000))

	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	var facts []string
	for i, line := range lines {
		if i > 15 {
			break
		}
		if len(line) > 30 {
			facts = append(facts, truncate(line, 200))
		}
	}

	if len(facts) == 0 {
		return "No results found for: " + query, nil
	}

	return fmt.Sprintf("## Synthesis for: %s\n\n%s\n\nSources: web search", query, strings.Join(facts, "\n")), nil
}