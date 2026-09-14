// cmd/mpm/handlers_why.go — `mpm why <id>` handler.
//
// Tiny adapter composing WhyService.Explain + WhyRenderer.Render.
// Per RFC §7: commands own no behaviour, no SQL, no rendering.
//
// `mpm why <id>` accepts:
//   - bare id (auto-detect kind from memories/decisions/theories/skills)
//   - explicit kind via --kind memory|decision|theory|skill|lesson|work
//   - skill id (canonical "skill:<name>-v<version>" prefix auto-routes)
//
// Output is always prose + counts.

package main

import (
	"os"
	"strings"

	"github.com/flowbyte-com/mpm-core/usererror"

	"github.com/flowbyte-com/mpm/cmd/mpm/render"
)

// handleWhy parses args for an optional --kind flag followed by the
// artifact id, runs WhyService.Explain, and renders the report.
//
// Args:
//   mpm why <id>           (kind auto-detected)
//   mpm why --kind theory <id>
//   mpm why skill:agentshell-v1.0.0   (prefix auto-routes to skill)
//
// 2026-09-14 release-pass: --help / -h / "help" short-circuits to
// printWhyHelp() so asking for help is never a mutation or lookup.
// Without this, `mpm why --help` fell into ExplainWithHint("help",
// "") which did a real artifact lookup and exited 1 on
// SkipReason.
func handleWhy(args []string) int {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "help" {
			printWhyHelp()
			return 0
		}
	}
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

	// Validate --kind against the canonical set so a typo produces a
	// clear error instead of silently aliasing to auto-detect. Auto-
	// detect is the fallback when --kind is unset. The accepted set
	// is the verified registry (see TestWhyHelp_KindVocabulary
	// in handlers_why_help_test.go for the runtime pin).
	switch kind {
	case "", "memory", "decision", "theory", "skill", "lesson", "work":
		// canonical set; pass through to ExplainWithHint
	default:
		usererror.Error("mpm why: --kind %q is not a canonical artifact kind (use memory|decision|theory|skill|lesson|work or omit for auto-detect)", kind)
		return 1
	}

	report, err := svc.ExplainWithHint(id, kind)
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

// printWhyHelp prints `mpm why`'s help page via the canonical
// visual grammar (cmd/mpm/render). The verified --kind vocabulary
// is the canonical set validated by handleWhy above.
func printWhyHelp() {
	render.Heading(os.Stdout, "Why")
	render.BlankLine(os.Stdout)
	render.Section(os.Stdout, "Provenance + evidence + confidence for any artifact")
	render.Plain(os.Stdout, "Composes the substrate's evidence, confidence, and")
	render.Plain(os.Stdout, "provenance views for a single artifact id.")
	render.BlankLine(os.Stdout)
	render.Section(os.Stdout, "Usage")
	render.Label(os.Stdout, "mpm why <id>", "Auto-detect kind from id")
	render.Label(os.Stdout, "mpm why --kind <kind> <id>", "Force a specific kind")
	render.Plain(os.Stdout, "  skill ids auto-route via the canonical skill:<name>-v<version> prefix")
	render.BlankLine(os.Stdout)
	render.Section(os.Stdout, "Flags")
	render.Label(os.Stdout, "--kind <memory|decision|theory|skill|lesson|work>", "Pin the artifact kind; omit for auto-detect")
	render.BlankLine(os.Stdout)
	render.Section(os.Stdout, "Related surfaces")
	render.Label(os.Stdout, "mpm ops confidence show --artifact <id>", "JSON-form confidence snapshot")
	render.Label(os.Stdout, "mpm ops confidence history --artifact <id>", "Confidence change timeline")
	render.Label(os.Stdout, "mpm call mpm_evidence", "Machine evidence list via MCP boundary")
	render.BlankLine(os.Stdout)
}
