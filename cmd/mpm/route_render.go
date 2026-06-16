package main

import (
	"encoding/json"
	"io"
	"os"
	"strings"
)

// resolveRouteWorkspace returns the MPM workspace path for `mpm route`.
// Resolution order: MPM_ROUTE_WORKSPACE env var → "." (current directory).
// The router inside will then load mode/ and persona/ from this base path.
func resolveRouteWorkspace() string {
	if v := os.Getenv("MPM_ROUTE_WORKSPACE"); v != "" {
		return v
	}
	return "."
}

// extractRoutePrompt returns the user prompt for route evaluation.
// Precedence:
//  1. Positional arg (preferred for shells/hooks passing inline text)
//  2. Stdin parsed as JSON {"prompt": "..."} (Claude Code hook format)
//  3. Stdin treated literally (human `echo "..." | mpm route` use)
//
// Returns "" if no prompt source yields content. Malformed JSON on stdin
// falls back to the literal stdin content (defense in depth — the binary
// should still be usable in a pipe even if the upstream is non-conformant).
func extractRoutePrompt(args []string, stdin io.Reader) string {
	if len(args) > 0 {
		return strings.TrimSpace(args[0])
	}
	if stdin == nil {
		return ""
	}
	data, err := io.ReadAll(stdin)
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "{") {
		var hook struct {
			Prompt string `json:"prompt"`
		}
		if err := json.Unmarshal([]byte(s), &hook); err == nil {
			return hook.Prompt
		}
		// Malformed JSON: fall through to literal
	}
	return s
}

// shouldSkipRoute returns (true, reason) if the prompt should bypass routing.
// Reason is one of: "empty", "noroute", "env". Reason is "" when skip=false.
//
// Checked in this order (first match wins):
//  1. Empty or whitespace-only prompt → "empty"
//  2. Prompt contains "/noroute" anywhere → "noroute"
//  3. MPM_ROUTE=off env var → "env"
//
// The envLookup indirection lets tests simulate the env var without
// mutating process state.
func shouldSkipRoute(prompt string, envLookup func(string) string) (bool, string) {
	if strings.TrimSpace(prompt) == "" {
		return true, "empty"
	}
	if strings.Contains(prompt, "/noroute") {
		return true, "noroute"
	}
	if envLookup("MPM_ROUTE") == "off" {
		return true, "env"
	}
	return false, ""
}

const routeOutputCap = 9500 // under Claude Code's 10,000-char hook stdout limit
const routeModeHardCap = 9000

// applyRouteLengthCap returns the rendered <system-reminder> body given
// pre-extracted mode and persona text. Priority: preserve mode (operational
// rules) over persona (voice/tone).
//
// Rules:
//   - Combined length ≤ routeOutputCap: return both unchanged
//   - Combined length > cap with persona present: truncate persona, append marker
//   - Mode alone exceeds routeModeHardCap: truncate mode, append marker
func applyRouteLengthCap(modeText, personaText string) string {
	shortMarker := "\n[...truncated, see mode/<name>.md for full content]"
	longMarker := "\n[...truncated]"

	combined := modeText + personaText
	if len(combined) <= routeOutputCap {
		return modeText + personaText
	}
	if len(modeText) > routeModeHardCap {
		return modeText[:routeModeHardCap] + longMarker
	}
	if personaText != "" {
		return modeText + shortMarker
	}
	return modeText + longMarker
}
