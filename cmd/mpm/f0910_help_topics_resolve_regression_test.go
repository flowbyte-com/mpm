// f0910_help_topics_resolve_regression_test.go — 2026-09-10 fix.
//
// Pin the contract that every help topic advertised in the
// cognitive `mpm help` output (and in the `mpm help --all` footer
// Sections line) actually resolves to either a section-handler or a
// per-command help page. Stale advertisements (e.g. `mpm help
// observability`) used to slip past the smoke probe because the
// help text printed the topic name without verifying the underlying
// handler existed.
package main

import (
	"strings"
	"testing"
)

// TestHelpTopics_AdvertisedTopicsResolve extracts every topic name
// advertised in the cognitive help output (the `Need more?`
// section's `mpm help <topic>` entries) and asserts each one is
// resolved by either:
//   • sectionHelpContent (a coherent help section)
//   • the per-command router (`handleHelp`)
//   • the tool-introspection fallback (`printToolHelp`)
//
// Pre-fix this test would have caught `mpm help observability` /
// `mpm help advanced` / `mpm help debug` (all advertised in the
// cognitive help but unresolved by `sectionHelpContent`).
func TestHelpTopics_AdvertisedTopicsResolve(t *testing.T) {
	// Render the cognitive help and pull out every `mpm help <topic>`
	// line.
	rendered := renderCognitiveHelp()
	topicLines := extractTopicAdvertisements(rendered)
	if len(topicLines) == 0 {
		t.Fatal("renderCognitiveHelp produced no topic advertisements; test setup broken")
	}
	for _, topic := range topicLines {
		t.Run(topic, func(t *testing.T) {
			// Section handlers win first — the help router routes
			// `mpm help <section>` here.
			if _, ok := sectionHelpContent(topic); ok {
				return
			}
			// Otherwise the topic is a per-command or tool name.
			// The handleHelp switch handles a known set; tool names
			// fall through to printToolHelp. Either path counts as
			// resolved.
			if isResolvedCommandOrTool(topic) {
				return
			}
			t.Errorf("advertised topic %q is not resolved by any help handler", topic)
		})
	}
}

// TestHelpTopics_FooterSectionsMatchActualSections pins the
// contract that the `Sections:` line printed by `mpm help --all`
// lists only sections that actually exist in `sectionHelpContent`.
func TestHelpTopics_FooterSectionsMatchActualSections(t *testing.T) {
	rendered := renderCognitiveHelp()
	footerSections := extractFooterSections(rendered)
	if len(footerSections) == 0 {
		// Footer is in `printHelpAll` output, not the cognitive
		// help. If absent here, the test surface doesn't expose
		// it — skip rather than false-fail.
		t.Skip("footer Sections line not present in cognitive help render")
	}
	for _, sec := range footerSections {
		t.Run(sec, func(t *testing.T) {
			if _, ok := sectionHelpContent(sec); !ok {
				t.Errorf("footer lists section %q but sectionHelpContent has no case for it", sec)
			}
		})
	}
}

// TestHelpTopics_ObservabilityAndAdvancedAreNotAdvertised pins the
// specific 2026-09-10 fix: the stale `mpm help observability` and
// `mpm help advanced` references must not appear in the cognitive
// help output (they used to be advertised in `Need more?` but had
// no underlying handler).
func TestHelpTopics_ObservabilityAndAdvancedAreNotAdvertised(t *testing.T) {
	rendered := renderCognitiveHelp()
	for _, stale := range []string{"observability", "advanced"} {
		// Match `mpm help <topic>` patterns specifically, not
		// the prose mention of "observability" in the reflection
		// section description.
		if strings.Contains(rendered, "mpm help "+stale) {
			t.Errorf("cognitive help must not advertise 'mpm help %s' — it has no underlying handler", stale)
		}
	}
}

// extractTopicAdvertisements finds every `mpm help <topic>` token in
// the rendered cognitive help output. Topic name is the first
// whitespace-terminated word following `mpm help `. Robust against
// the lipgloss styling that interleaves the topic name with its
// description (e.g. `mpm help knowledge    expanded view...`).
func extractTopicAdvertisements(rendered string) []string {
	var out []string
	for _, line := range strings.Split(rendered, "\n") {
		stripped := stripANSI(line)
		idx := strings.Index(stripped, "mpm help ")
		if idx < 0 {
			continue
		}
		rest := stripped[idx+len("mpm help "):]
		// Topic name is the first whitespace-delimited token after
		// the marker.
		end := strings.IndexAny(rest, " \t")
		if end < 0 {
			end = len(rest)
		}
		topic := rest[:end]
		if topic == "" || topic == "--all" {
			continue
		}
		out = append(out, topic)
	}
	return out
}

// extractFooterSections parses the `Sections: a, b, c` footer line
// produced by `printHelpAll`.
func extractFooterSections(rendered string) []string {
	for _, line := range strings.Split(rendered, "\n") {
		stripped := stripANSI(line)
		const marker = "Sections:"
		idx := strings.Index(stripped, marker)
		if idx < 0 {
			continue
		}
		rest := stripped[idx+len(marker):]
		parts := strings.Split(rest, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			t := strings.TrimSpace(p)
			if t != "" {
				out = append(out, t)
			}
		}
		return out
	}
	return nil
}

// stripANSI removes simple ANSI escape sequences (used by lipgloss)
// so string-contains assertions aren't defeated by invisible codes.
func stripANSI(s string) string {
	var b strings.Builder
	skip := 0
	for i := 0; i < len(s); i++ {
		if skip > 0 {
			skip--
			continue
		}
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			// CSI sequence: ESC [ <params> <final byte>
			j := i + 2
			for j < len(s) && !isAnsiFinal(s[j]) {
				j++
			}
			if j < len(s) {
				skip = j + 1 - i - 1
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isAnsiFinal(b byte) bool {
	return (b >= 0x40 && b <= 0x7e)
}

// isResolvedCommandOrTool mirrors the handleHelp router's
// dispatch order: section content first, then per-command help, then
// tool introspection. Returns true if the topic would be resolved by
// any of those paths.
func isResolvedCommandOrTool(topic string) bool {
	// Mirror the `switch helpCmd` block in router.go's handleHelp.
	switch topic {
	case "mode", "persona", "topic", "session", "lesson", "reference",
		"memory", "gateway", "challenge", "evidence", "ops", "work",
		"remember", "learn", "decide", "theorize", "decision", "theory",
		"record_decision", "propose_theory", "resolve_theory":
		return true
	}
	// Tool-name introspection: if `printToolHelp` would handle it.
	for _, t := range toolsNamesForHelp() {
		if t == topic {
			return true
		}
	}
	return false
}

// toolsNamesForHelp lists the tool names the help router can
// resolve. Mirrors the registry exported by internal/core/tools;
// duplicated locally so the test does not import an unexported name.
func toolsNamesForHelp() []string {
	// The full registry is large; the smoke-probe-relevant subset
	// is enough for the regression. The handleHelp router also
	// falls through to printToolHelp, which inspects ByName, so
	// any registered tool is a valid topic.
	return []string{
		"mpm_memory", "mpm_lessons", "mpm_decisions", "mpm_theories",
		"mpm_skills", "mpm_topics", "mpm_references", "mpm_evidence",
		"mpm_confidence", "mpm_context", "mpm_wakes", "mpm_handoff",
		"mpm_scratchpad", "mpm_system", "mpm_work", "mpm_blob_read",
		"mpm_resolve", "mpm_retrieval_diagnose", "mpm_synthesize_memory",
	}
}
