package tools

import (
	"strings"
)

// domainOrder is the canonical display order for tool groups in the
// README MCP tool surface. Tools not in any of these categories fall
// into "other" and sort alphabetically at the bottom. Update this when
// adding new domains.
var domainOrder = []string{
	"memory",           // save_to_memory, query_long_term_memory, challenge_memory
	"feedback loop",    // shred/reinforce/weaken/snooze/set/patch/promote
	"lessons",          // save/search/list lessons
	"topics",           // create/search/link topics
	"references",       // add/search/list references
	"theories",         // propose/resolve theories
	"decisions",        // record_decision
	"evidence",         // add/list evidence
	"confidence",       // query/show/recompute/explain confidence
	"audit",            // query_audit_log
	"system",           // read_wake_context, read_directives, route, proactive_recall_hint
	"session handoffs", // session_end, session_handoff, list_handoffs
	"workflow",         // review_memories, synthesize_memory, gc_run
}

// groupByDomain groups tools by domain using name-prefix heuristics.
// Returns a map of domain → []Tool in registry order.
func groupByDomain(ts []Tool) map[string][]Tool {
	groups := map[string][]Tool{}

	heuristic := func(name string) string {
		switch {
		case strings.HasPrefix(name, "save_to_memory"),
			strings.HasPrefix(name, "query_long_term"),
			strings.HasPrefix(name, "challenge_memory"):
			return "memory"
		case strings.HasPrefix(name, "shred"),
			strings.HasPrefix(name, "reinforce"),
			strings.HasPrefix(name, "weaken"),
			strings.HasPrefix(name, "snooze"),
			strings.HasPrefix(name, "set_memory"),
			strings.HasPrefix(name, "patch_memory"),
			strings.HasPrefix(name, "promote"):
			return "feedback loop"
		case strings.HasPrefix(name, "save_lesson"),
			strings.HasPrefix(name, "search_lesson"),
			strings.HasPrefix(name, "list_lesson"):
			return "lessons"
		case strings.HasPrefix(name, "create_topic"),
			strings.HasPrefix(name, "search_topic"),
			strings.HasPrefix(name, "link_topic"),
			strings.HasPrefix(name, "list_topic"):
			return "topics"
		case strings.HasPrefix(name, "add_reference"),
			strings.HasPrefix(name, "search_reference"),
			strings.HasPrefix(name, "list_reference"):
			return "references"
		case strings.HasPrefix(name, "propose_theory"),
			strings.HasPrefix(name, "resolve_theory"):
			return "theories"
		case strings.HasPrefix(name, "record_decision"):
			return "decisions"
		case strings.HasPrefix(name, "add_evidence"),
			strings.HasPrefix(name, "list_evidence"):
			return "evidence"
		case strings.HasPrefix(name, "query_confidence"),
			strings.HasPrefix(name, "show_confidence"),
			strings.HasPrefix(name, "recompute"),
			strings.HasPrefix(name, "explain_confidence"):
			return "confidence"
		case strings.HasPrefix(name, "query_audit"):
			return "audit"
		case strings.HasPrefix(name, "read_wake"),
			strings.HasPrefix(name, "read_directives"),
			strings.HasPrefix(name, "proactive_recall"),
			strings.HasPrefix(name, "route"):
			return "system"
		case strings.HasPrefix(name, "session_"),
			strings.HasPrefix(name, "list_handoffs"):
			return "session handoffs"
		case strings.HasPrefix(name, "review_"),
			strings.HasPrefix(name, "synthesize"),
			strings.HasPrefix(name, "gc_"):
			return "workflow"
		}
		return "other"
	}

	for _, t := range ts {
		g := heuristic(t.Name)
		groups[g] = append(groups[g], t)
	}
	return groups
}

// RenderToolSurface produces the README block body (between the
// sentinels). Exported so tests can compare without invoking the file IO.
func RenderToolSurface() string {
	groups := groupByDomain(Registry)

	// Iterate domains in canonical order; "other" (if any) at the end.
	domainList := make([]string, 0, len(groups))
	domainList = append(domainList, domainOrder...)
	for k := range groups {
		found := false
		for _, d := range domainList {
			if d == k {
				found = true
				break
			}
		}
		if !found {
			domainList = append(domainList, k)
		}
	}

	var b strings.Builder
	for di, domain := range domainList {
		tools := groups[domain]
		if len(tools) == 0 {
			continue
		}
		// 3 tools per row, padded to 24 chars. Trailing whitespace is
		// trimmed per-line to keep the diff readable.
		for i := 0; i < len(tools); i++ {
			name := tools[i].Name
			end := i + 1
			isLast := end == len(tools)
			if !isLast && end%3 != 0 {
				b.WriteString(name)
				b.WriteString(strings.Repeat(" ", 24-len(name)))
				b.WriteString(" ")
			} else {
				b.WriteString(name)
				b.WriteString("\n")
			}
		}
		// Blank line between groups (except after the last).
		if di != len(domainList)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}
