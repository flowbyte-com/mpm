// cmd/mpm/draft.go — interactive $EDITOR-based drafting helper.
//
// Used by `mpm memory add --interactive` to let an operator compose
// long-form memory content in their editor of choice, see a preview,
// and confirm before insertion. Lives in its own file because the
// helper has no relationship to any single handler and pulls in
// separate process-management concerns (os/exec, tempfile cleanup).
//
// Originally tucked at the bottom of handlers_watch.go during the
// 2026-06 watcher deprecation sweep; promoted to a sibling file on
// 2026-07-29 when the watch handler was hard-removed.

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// draftInteractiveContent opens $EDITOR on a temp file, waits for the user
// to save+exit, reads the result, shows a preview, and asks for y/N
// confirmation. Returns the drafted content or an empty string if declined.
func draftInteractiveContent() (string, error) {
	tmpFile, err := os.CreateTemp("", "mpm_draft_*.md")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmpPath)

	editor := os.Getenv("EDITOR")
	if editor == "" {
		if _, err := exec.LookPath("nano"); err == nil {
			editor = "nano"
		} else if _, err := exec.LookPath("vim"); err == nil {
			editor = "vim"
		} else {
			return "", fmt.Errorf("no editor found — set $EDITOR or install nano/vim")
		}
	}

	cmd := exec.Command(editor, tmpPath)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("editor exited with error: %w", err)
	}

	data, err := os.ReadFile(tmpPath)
	if err != nil {
		return "", fmt.Errorf("read drafted content: %w", err)
	}
	content := strings.TrimSpace(string(data))
	if content == "" {
		return "", nil
	}

	// Preview
	fmt.Printf("\n%s─── Draft Preview ──────────────────────────────%s\n", "\033[1m\033[36m", "\033[0m")
	fmt.Println(content)
	fmt.Printf("%s──────────────────────────────────────────────────%s\n", "\033[1m\033[36m", "\033[0m")
	fmt.Printf("\nSave this memory? [y/N] ")

	var answer string
	fmt.Scanln(&answer)
	answer = strings.TrimSpace(strings.ToLower(answer))
	if answer != "y" && answer != "yes" {
		return "", nil
	}

	return content, nil
}
