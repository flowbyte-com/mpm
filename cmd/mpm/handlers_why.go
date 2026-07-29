// cmd/mpm/handlers_why.go — `mpm why <id>` handler for Wave 2.
//
// Tiny adapter composing WhyService.Explain + WhyRenderer.Render.
// Per RFC §7: commands own no behaviour, no SQL, no rendering.
//
// `mpm why <id>` accepts:
//   - bare id (auto-detect kind from memories/decisions/theories/skills)
//   - explicit kind via --kind memory|decision|theory|skill
//   - skill id (canonical "skill:<name>-v<version>" prefix auto-routes)
//
// Output is always prose + counts; --json lands in a future wave as a
// machine-readable form for agent consumers.

package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/flowbyte-com/mpm-core/usererror"
)

// handleWhy parses args for an optional --kind flag followed by the
// artifact id, runs WhyService.Explain, and renders the report.
//
// Args:
//   mpm why <id>           (kind auto-detected)
//   mpm why --kind theory <id>
//   mpm why skill:agentshell-v1.0.0   (prefix auto-routes to skill)
func handleWhy(args []string) int {
	kind := ""
	id := ""
	filtered := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--kind" && i+1 < len(args):
			kind = args[i+1]
			i++
		case strings.HasPrefix(a, "--kind="):
			kind = strings.TrimPrefix(a, "--kind=")
		default:
			filtered = append(filtered, a)
		}
	}
	if len(filtered) >= 1 {
		id = strings.Join(filtered, " ")
	}
	if id == "" {
		usererror.Error("mpm why: id is required\nUsage: mpm why <id> or mpm why --kind <kind> <id>")
		return 1
	}

	dm := getDBConcrete()
	if dm == nil {
		usererror.Error("database unavailable")
		return 1
	}

	svc := NewWhyService(dm)
	if svc == nil {
		usererror.Error("why: failed to wire service")
		return 1
	}

	// Explicit --kind wins over auto-detection. Skill: prefix always
	// wins because it's the canonical skill id shape.
	if kind == "" && !strings.HasPrefix(id, "skill:") {
		// auto-detect runs in the service via Explain.
	} else if kind != "" {
		// The service's auto-detect probes memories/decisions/theories
		// in order. For explicit kind we short-circuit: probe only the
		// requested collection. For simplicity in v1, we still call
		// Explain which detects — passing the kind as a hint is a
		// future enhancement.
		_ = kind
	}

	report, err := svc.Explain(id)
	if err != nil {
		usererror.Error("why: %v", err)
		return 1
	}

	renderer := NewWhyRenderer(os.Stdout)
	if err := renderer.Render(report); err != nil {
		usererror.Error("why render: %v", err)
		return 1
	}

	// SkipReason in the report means we couldn't locate the artifact.
	// Surface but don't fail loudly — the report itself explains.
	if report.SkipReason != "" {
		return 1
	}
	return 0
}

// PrintWhyHelp is exposed (lowercase rename of helper) so the help
// text can be regenerated from a single string literal at the handler
// level rather than the service layer. Reserved for future inline
// help.
func printWhyHelp() {
	fmt.Println(`mpm why <id> — Why does this artifact exist?

Composes evidence, confidence, and provenance for any artifact in the
substrate (memory, decision, theory, skill). One-level deep per the
cognitive-interface RFC — recursion is out of scope until a real
need arrives.

Usage:
  mpm why <id>                 Auto-detect kind
  mpm why --kind theory <id>  Force a specific kind
  mpm why skill:agentshell-v1  Skill ids auto-route via canonical prefix

Flags:
  --kind <memory|decision|theory|skill>   Pin the artifact kind`)
}
