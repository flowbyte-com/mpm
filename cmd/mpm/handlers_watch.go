package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func handleWatch(args []string) int {
	subCmd := "status"
	if len(args) > 0 {
		subCmd = args[0]
	}
	fmt.Printf("⚠️  mpm watch %s is deprecated (removed 2026-06-26).\n", subCmd)
	fmt.Println("    The watcher daemon was an auto-ingest workaround for pre-MCP agents.")
	fmt.Println("    Use `mpm ops maintain` for decay/cleanup on demand.")
	fmt.Println("    Use `mpm ops synthesize` for synthesis on demand.")
	fmt.Println("    Use `mpm ops ingest --source <path>` for one-shot external DB ingestion.")
	fmt.Println("    The parser library lives at cmd/mpm/parsers.go if a future one-shot")
	fmt.Println("    CLI needs to be rebuilt from scratch.")
	return 0
}

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
