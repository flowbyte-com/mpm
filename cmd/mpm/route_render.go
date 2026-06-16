package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"mpm/internal"
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

// applyRouteLengthCap returns (truncatedMode, truncatedPersona) such that the
// total rendered body stays under routeOutputCap (9,500 chars) — under Claude
// Code's 10,000-char hook stdout limit. Priority: preserve mode (operational
// rules) over persona (voice/tone).
//
// Rules:
//   - Combined length ≤ routeOutputCap: return both unchanged
//   - Combined length > cap with persona present: drop persona, return mode with shortMarker suffix
//   - Mode alone exceeds routeModeHardCap: truncate mode, append longMarker
//
// Returning the two components separately (rather than a pre-joined string) lets
// renderRoute rebuild the labeled body after truncation, so the labels and `---`
// separators can be re-emitted around the truncated content. The persona-priority
// semantic must be enforced at the *cap* layer — not by the renderer — otherwise
// the renderer would need to know which sub-strings to keep, which it cannot do
// without the priority logic living somewhere. Hence this function returns the
// pieces, and the renderer reassembles.
func applyRouteLengthCap(modeText, personaText string) (string, string) {
	shortMarker := "\n[...truncated, see mode/<name>.md for full content]"
	longMarker := "\n[...truncated]"

	combined := modeText + personaText
	if len(combined) <= routeOutputCap {
		return modeText, personaText
	}
	if len(modeText) > routeModeHardCap {
		// Mode alone exceeds the hard cap — must truncate it regardless of persona
		return modeText[:routeModeHardCap] + longMarker, ""
	}
	if personaText != "" {
		// Persona gets dropped; append marker to mode to preserve the signal
		return modeText + shortMarker, ""
	}
	// Both empty after combined > cap is unusual (modeText > 0, personaText == "", combined > cap)
	// — only happens if modeText itself is between 9,000 and 9,500
	return modeText + longMarker, ""
}

// renderRoute evaluates prompt against the workspace's mode+persona files
// and returns a <system-reminder> block for the LLM. Returns ("", nil) when
// no mode or persona matched (low-signal prompt) or when the workspace is
// unusable. Returns error only for unexpected internal failures.
//
// Behavior matches the spec (Component 2):
//   - Empty/low-signal prompt → "", nil
//   - Missing/unusable workspace → "", nil
//   - Mode file missing for selected mode → "", nil (operational rules are load-bearing)
//   - Persona file missing → render mode only, append marker
//   - Combined output > 9500 chars → applyRouteLengthCap
func renderRoute(workspace, prompt string) (string, error) {
	if workspace == "" {
		return "", nil
	}

	router, err := internal.NewRouter(workspace)
	if err != nil {
		// Workspace unusable (missing mode/persona dirs etc.) — graceful exit
		return "", nil
	}

	report := router.Evaluate(prompt)
	if len(report.SelectedModes) == 0 && report.SelectedPersona == "" {
		// No mode and no persona matched — don't inject anything
		return "", nil
	}

	// Build the mode and persona sections as separate labeled strings. We track
	// them independently so applyRouteLengthCap can enforce persona-priority
	// truncation against the actual labeled content (what the LLM sees). Labels
	// and `---` separators are part of the accumulated strings — they count
	// toward the cap, which is what we want, because they're part of the
	// rendered output.
	var modeText, personaText string
	if len(report.SelectedModes) > 0 {
		// Take the first selected mode's file. If multiple, concatenate with
		// separators so the LLM sees all of them.
		var modeBuilder strings.Builder
		for i, modeName := range report.SelectedModes {
			content, err := os.ReadFile(filepath.Join(workspace, "mode", modeName+".md"))
			if err != nil {
				// Mode file missing — refuse to inject partial operational rules
				return "", nil
			}
			if i == 0 {
				fmt.Fprintf(&modeBuilder, "mode=%s\n\n%s", modeName, string(content))
			} else {
				fmt.Fprintf(&modeBuilder, "\n\n---\n\nmode=%s\n\n%s", modeName, string(content))
			}
		}
		modeText = modeBuilder.String()
	}

	if report.SelectedPersona != "" {
		content, err := os.ReadFile(filepath.Join(workspace, "persona", report.SelectedPersona+".md"))
		if err != nil {
			// Persona missing — render mode only, append marker
			return wrapReminder(modeText) + "\n\n[persona " + report.SelectedPersona + " not found on disk]", nil
		}
		// Persona label is included in personaText so it counts toward the cap.
		var personaBuilder strings.Builder
		fmt.Fprintf(&personaBuilder, "persona=%s\n\n%s", report.SelectedPersona, string(content))
		personaText = personaBuilder.String()
	}

	// Length cap operates on the labeled components. Returns the (possibly
	// truncated) pieces — we rebuild the final body from them so the `---`
	// separator between mode and persona is only emitted if both survived.
	truncatedMode, truncatedPersona := applyRouteLengthCap(modeText, personaText)

	var body strings.Builder
	if truncatedMode != "" {
		body.WriteString(truncatedMode)
	}
	if truncatedPersona != "" {
		if body.Len() > 0 {
			body.WriteString("\n\n---\n\n")
		}
		body.WriteString(truncatedPersona)
	}
	return wrapReminder(body.String()), nil
}

// wrapReminder wraps body in a <system-reminder> block with the auto-route header.
// Format: <system-reminder> + "MPM auto-route active" header + body + </system-reminder>
func wrapReminder(body string) string {
	return fmt.Sprintf("<system-reminder>\nMPM auto-route active\n\n%s\n</system-reminder>", body)
}
