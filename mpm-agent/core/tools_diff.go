package core

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// applyDiff applies a unified diff to a file without overwriting.
// Input: { "file": "/path", "diff": "unified diff string" }
func applyDiff(args map[string]interface{}) (string, error) {
	file, _ := args["file"].(string)
	diff, _ := args["diff"].(string)
	if file == "" || diff == "" {
		return "", fmt.Errorf("apply_diff requires 'file' and 'diff' fields")
	}

	absPath, err := filepath.Abs(file)
	if err != nil {
		return "", fmt.Errorf("apply_diff: invalid path: %w", err)
	}

	content, err := os.ReadFile(absPath)
	if err != nil {
		return "", fmt.Errorf("apply_diff: read file: %w", err)
	}

	patch, err := parseUnifiedDiff(diff)
	if err != nil {
		return "", fmt.Errorf("apply_diff: parse diff: %w", err)
	}

	newContent, err := applyUnifiedDiff(string(content), patch)
	if err != nil {
		return "", fmt.Errorf("apply_diff: apply patch: %w", err)
	}

	if string(content) != newContent {
		if err := os.WriteFile(absPath, []byte(newContent), 0644); err != nil {
			return "", fmt.Errorf("apply_diff: write file: %w", err)
		}
		return fmt.Sprintf("Patched %d hunks in %s", len(patch.Hunks), absPath), nil
	}
	return "No changes needed", nil
}

// diffPatch holds a parsed unified diff.
type diffPatch struct {
	OldFile, NewFile string
	Hunks            []*diffHunk
}

type diffHunk struct {
	OldStart, OldCount, NewStart, NewCount int
	Lines []string // '+' prefix = add, '-' prefix = delete, ' ' prefix = context
}

var hunkHeaderRE = regexp.MustCompile(`@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

func parseUnifiedDiff(diff string) (*diffPatch, error) {
	patch := &diffPatch{}
	lines := strings.Split(diff, "\n")
	var currentHunk *diffHunk

	for _, line := range lines {
		if m := hunkHeaderRE.FindStringSubmatch(line); m != nil {
			oldStart, _ := strconv.Atoi(m[1])
			oldCount, _ := strconv.Atoi(m[2])
			if oldCount == 0 {
				oldCount = 1
			}
			newStart, _ := strconv.Atoi(m[3])
			newCount, _ := strconv.Atoi(m[4])
			if newCount == 0 {
				newCount = 1
			}
			currentHunk = &diffHunk{
				OldStart: oldStart, OldCount: oldCount,
				NewStart: newStart, NewCount: newCount,
			}
			patch.Hunks = append(patch.Hunks, currentHunk)
			continue
		}

		if currentHunk == nil {
			continue
		}

		if len(line) == 0 {
			continue
		}
		switch line[0] {
		case '+', '-', ' ':
			currentHunk.Lines = append(currentHunk.Lines, line)
		}
	}
	return patch, nil
}

func applyUnifiedDiff(content string, patch *diffPatch) (string, error) {
	if len(patch.Hunks) == 0 {
		return content, nil
	}

	origLines := strings.Split(content, "\n")
	result := make([]string, 0, len(origLines))
	origIdx := 0

	for _, hunk := range patch.Hunks {
		// Copy lines before hunk starts (origIdx is 0-based, OldStart is 1-based)
		for origIdx < hunk.OldStart-1 && origIdx < len(origLines) {
			result = append(result, origLines[origIdx])
			origIdx++
		}

		// Apply hunk lines
		for _, l := range hunk.Lines {
			switch l[0] {
			case '+':
				result = append(result, l[1:])
			case '-':
				origIdx++ // skip original line
			case ' ':
				if origIdx < len(origLines) {
					result = append(result, origLines[origIdx])
					origIdx++
				}
			}
		}
	}

	// Copy remaining lines (skip trailing empty element representing trailing newline)
	for origIdx < len(origLines) {
		if origIdx == len(origLines)-1 && origLines[origIdx] == "" {
			break
		}
		result = append(result, origLines[origIdx])
		origIdx++
	}

	newContent := strings.Join(result, "\n")
	// Restore trailing newline if original had one and join removed it
	if strings.HasSuffix(content, "\n") && !strings.HasSuffix(newContent, "\n") {
		newContent += "\n"
	}
	return newContent, nil
}