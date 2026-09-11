// renderManualRoute builds a <system-reminder> block from the concrete
// persona/modes in active.json, without invoking the router. Used by
// `mpm route` when the user has manually selected persona/modes (manual
// mode — not the auto sentinel, not blank).
//
// The output shape mirrors renderRoute: a labelled block containing the
// persona .md content and mode .md content for everything selected. The
// difference is that renderRoute picks these names via the router;
// renderManualRoute uses whatever is on disk in active.json.
//
// Returns "" when the manual selection resolves to nothing usable (e.g.
// persona file missing on disk and modes array empty after filtering).
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

func renderManualRoute(workspace string, active *mpminternal.ActiveState) (string, error) {
	if active == nil || workspace == "" {
		return "", nil
	}
	// Skip the sentinel values explicitly — they shouldn't reach this path
	// (the gate in handleRoute catches them) but defense in depth.
	personaStr := active.PersonaString()
	if personaStr == "auto" {
		empty := ""
		active.Persona = &empty
		personaStr = ""
	}
	cleanedModes := make([]string, 0, len(active.ModesSlice()))
	for _, m := range active.ModesSlice() {
		if m != "auto" {
			cleanedModes = append(cleanedModes, m)
		}
	}

	if personaStr == "" && len(cleanedModes) == 0 {
		// After stripping sentinels, nothing left — treat as blank.
		return "", nil
	}

	var parts []string
	parts = append(parts, "<system-reminder>", "MPM mode/persona (manual selection)", "")

	for _, modeName := range cleanedModes {
		content, err := os.ReadFile(filepath.Join(workspace, "mode", modeName+".md"))
		if err != nil {
			// Mode file missing — refuse to inject partial operational rules.
			return "", nil
		}
		parts = append(parts, fmt.Sprintf("mode=%s", modeName), "", string(content), "", "---", "")
	}

	if personaStr != "" {
		content, err := os.ReadFile(filepath.Join(workspace, "persona", personaStr+".md"))
		if err != nil {
			// Persona missing — render modes only and surface a marker so
			// the operator sees the drift.
			body := strings.Join(parts, "\n") + "\n\n[persona " + personaStr + " not found on disk]"
			return "<system-reminder>\n" + body + "\n</system-reminder>", nil
		}
		parts = append(parts, fmt.Sprintf("persona=%s", personaStr), "", string(content))
	}

	return strings.Join(parts, "\n") + "\n</system-reminder>", nil
}

// end renderManualRoute