package core

import (
	"testing"
)

func TestBuildToolListWithLoaded(t *testing.T) {
	profile := []string{"list_toolkits", "load_toolkit", "unload_toolkit"}
	toolkitMap := map[string][]string{
		"files": {"read_file", "write_file"},
		"web":   {"WebSynthesize"},
	}

	LoadToolkit("test-session", "files")
	defer UnloadToolkit("test-session", "files")

	tools := buildToolListWithLoaded(profile, "test-session", toolkitMap)

	if len(tools) == 0 {
		t.Error("expected non-empty tool list")
	}

	names := make(map[string]bool)
	for _, t := range tools {
		if n, ok := t["name"].(string); ok {
			names[n] = true
		}
	}

	if !names["execute_mpm_command"] {
		t.Error("expected execute_mpm_command in tools")
	}
	if !names["list_toolkits"] {
		t.Error("expected list_toolkits in tools")
	}
	if !names["read_file"] {
		t.Error("expected read_file (from loaded toolkit) in tools")
	}
}

func TestBuildToolListWithLoadedEmpty(t *testing.T) {
	profile := []string{"list_toolkits"}
	toolkitMap := map[string][]string{}

	tools := buildToolListWithLoaded(profile, "nonexistent-session", toolkitMap)

	if len(tools) != 2 {
		t.Errorf("expected 2 tools (execute_mpm_command + list_toolkits), got %d", len(tools))
	}
}

func TestMaxHistoryMessages(t *testing.T) {
	if maxHistoryMessages <= 0 {
		t.Error("maxHistoryMessages should be positive")
	}
	if maxHistoryMessages > 20 {
		t.Error("maxHistoryMessages seems too large for reasonable context window")
	}
}
