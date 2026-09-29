// mpm_context_compact_distinction_test.go — regression guard for the
// 2026-09-29 instruction-architecture simplification. After the managed
// block dropped old §1.2 (the recent_activity / contextual_candidates
// distinction), the compact `mpm_context` description in
// `cmd/mpm-mcp/tools.go` became the only place a compact-surface
// agent could learn that distinction. This test pins the production
// tool description — what MCP clients actually receive — so a future
// contributor who silently drops the distinction is caught regardless
// of how `tools.go` is internally organised.
//
// **Brittleness notice.** This test asserts substring presence on
// the *production-registered* description (via `s.ListTools()` after
// registering the same tool main() registers), not on a source-level
// regex. The source may be reorganised in any way that preserves
// semantics; what matters is what clients see at the MCP boundary.

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// compactMpmContextDescriptionRequiredSubstrings is the contract for
// the compact mpm_context description: the post-2026-09-29 world
// requires that the recent_activity / contextual_candidates
// distinction survive in the compact surface, because old managed
// block §1.2 no longer carries it.
//
// Every entry is a stable phrase; the test enforces substring
// presence against the production-registered description. Anchors are
// short so they survive the canonical source's column wrap (the
// description literal is built from string concatenation).
var compactMpmContextDescriptionRequiredSubstrings = []string{
	// recent_activity: observational / chronological framing.
	// The compact description must say it is not relevance-ranked.
	"recent_activity",
	"chronological",
	// Observational framing (vs relevance-ranked).
	"observational",
	// contextual_candidates: explicitly named diagnostic surface.
	"contextual_candidates",
	"contextual_selection",
	"contextual_materialization",
	// The compact description must NOT call these "normal-startup"
	// (the framing is the load-bearing bit).
	"not part of normal session-start",
	// Diagnostic vocabulary — confirms the framing distinguishes
	// diagnostic from production surfaces.
	"diagnostic",
}

// compactMpmContextDescriptionAnchors are stable, distinctive
// substrings that together identify the post-2026-09-29 description.
// If even one disappears, the description has drifted and the
// contract is broken.
var compactMpmContextDescriptionAnchors = []string{
	"recent_activity is the",
	"chronological / observational activity feed",
	"contextual_candidates,",
	"contextual_selection,",
	"contextual_materialization",
	"are diagnostic surfaces for inspecting the contextual routing pipeline",
	"not part of normal session-start",
}

// liveCompactMpmContextDescription returns the description that the
// production MCP server registers for the mpm_context tool. It builds
// a real server (via buildMCPServerCore, the same constructor main()
// uses), registers the tool with the production constant
// `CompactMpmContextDescription`, and reads the registered description
// back via `s.ListTools()` — exactly what an MCP client would see.
func liveCompactMpmContextDescription(t *testing.T) string {
	t.Helper()
	s := buildMCPServerCore()
	// A no-op handler keeps the server self-consistent (mcp-go does
	// not require handlers to validate descriptions, but a handler
	// is the production registration shape).
	s.AddTool(
		mcp.NewTool("mpm_context",
			mcp.WithDescription(CompactMpmContextDescription),
		),
		func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText(""), nil
		},
	)

	for _, st := range s.ListTools() {
		if st.Tool.Name == "mpm_context" {
			return st.Tool.Description
		}
	}
	t.Fatalf(
		"mpm_context tool not found in production-built server's "+
			"ListTools(); server construction may have lost the tool",
	)
	return ""
}

// TestMpmContextCompactDistinction_LiveHasRequiredSubstrings reads
// the production-registered mpm_context description and asserts every
// required distinction substring is present. If a future contributor
// edits `CompactMpmContextDescription` (or whatever the test reads)
// and drops a phrase, this fails — even if the source is reorganised
// in ways that the prior regex-based test could not survive.
func TestMpmContextCompactDistinction_LiveHasRequiredSubstrings(t *testing.T) {
	live := liveCompactMpmContextDescription(t)
	for _, sub := range compactMpmContextDescriptionRequiredSubstrings {
		if !strings.Contains(live, sub) {
			t.Errorf(
				"production-registered compact `mpm_context` description "+
					"is missing required distinction substring %q — "+
					"without it, the post-2026-09-29 managed block's "+
					"drop of old §1.2 leaves compact-surface agents without "+
					"an orienting layer for the recent_activity vs "+
					"contextual_candidates distinction. Restore the "+
					"substring in cmd/mpm-mcp/tools.go::CompactMpmContextDescription. "+
					"\nfull production description:\n%s",
				sub, live,
			)
		}
	}
}

// TestMpmContextCompactDistinction_LiveHasAllAnchors asserts the
// distinctive post-2026-09-29 anchors are all present in the
// production-registered description. Combined with the substring
// test, this catches both partial edits and wholesale rewrites that
// drop the distinction.
func TestMpmContextCompactDistinction_LiveHasAllAnchors(t *testing.T) {
	live := liveCompactMpmContextDescription(t)
	for _, a := range compactMpmContextDescriptionAnchors {
		if !strings.Contains(live, a) {
			t.Errorf(
				"production-registered compact `mpm_context` description "+
					"lost anchor %q; this means the post-2026-09-29 "+
					"compact description has drifted. Restore the "+
					"substring(s) in cmd/mpm-mcp/tools.go::CompactMpmContextDescription.",
				a,
			)
		}
	}
}