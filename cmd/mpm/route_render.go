package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/config"
	"github.com/flowbyte-com/mpm-core/usererror"
)

// resolveRouteWorkspace returns the MPM workspace path for `mpm route`.
// Resolution order: MPM_ROUTE_WORKSPACE env var → config.GetMPMDir()
// (which honours MPM_WORKSPACE then falls back to $HOME/.mpm).
//
// Returning "." here is wrong — the UserPromptSubmit hook is invoked from
// an arbitrary cwd and ./mode does not resolve. The 2026-07-30 audit caught
// this; the canonical pattern is config.GetMPMDir() (see internal/core/config/config.go).
func resolveRouteWorkspace() string {
	if v := os.Getenv("MPM_ROUTE_WORKSPACE"); v != "" {
		return v
	}
	return config.GetMPMDir()
}

// extractRoutePrompt returns the user prompt for route evaluation.
// Precedence:
//  1. Positional arg (preferred for shells/hooks passing inline text)
//  2. Stdin parsed as JSON {"prompt": "..."} (canonical contract)
//  3. Stdin parsed as JSON {"user_prompt": "..."} (Claude Code's
//     UserPromptSubmit hook payload field name)
//  4. Stdin treated literally (human `echo "..." | mpm route` use)
//
// Returns "" if no prompt source yields content. Malformed JSON on stdin
// falls back to the literal stdin content (defense in depth — the binary
// should still be usable in a pipe even if the upstream is non-conformant).
//
// When both `prompt` and `user_prompt` are present, `prompt` wins — the
// original contract takes precedence over the Claude Code extension so
// scripts that build their own JSON envelope keep working unchanged.
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
			Prompt     string `json:"prompt"`
			UserPrompt string `json:"user_prompt"`
		}
		if err := json.Unmarshal([]byte(s), &hook); err == nil {
			if hook.Prompt != "" {
				return hook.Prompt
			}
			return hook.UserPrompt
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

// stripApplyFlag removes --apply from args and returns (apply, cleanedArgs).
// Other flags are passed through unchanged. Idempotent — calling twice
// produces the same result.
func stripApplyFlag(args []string) (bool, []string) {
	apply := false
	cleaned := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--apply" {
			apply = true
			continue
		}
		cleaned = append(cleaned, a)
	}
	return apply, cleaned
}

// applyRouteToActive merges the route report into active.json. Rules
// (2026-07-30 audit; makes active.json a live signal, not a stale bag):
//   - Persona: update when SelectedPersona is non-empty AND different from
//     current. Empty result preserves the operator's manually-set persona
//     on no-op routes (low-signal prompts).
//   - Modes: replace when SelectedModes is non-empty. Empty result
//     preserves current modes for the same reason.
//   - Updated: bump only when something actually changed.
//
// On any disk error (read or write), the function returns silently —
// routing must never block the user. Stderr is also gated behind isatty
// to avoid corrupting hook output.
func applyRouteToActive(report mpminternal.RoutingReport) {
	current, err := mpminternal.LoadActiveJSON()
	if err != nil {
		if isatty(os.Stderr) {
			usererror.Warn("route --apply: load active.json: %v", err)
		}
		return
	}

	next := *current // shallow copy — Modes slice is replaced wholesale below
	changed := false

	if report.SelectedPersona != "" && report.SelectedPersona != current.Persona {
		next.Persona = report.SelectedPersona
		changed = true
	}
	if len(report.SelectedModes) > 0 && !equalStringSlices(report.SelectedModes, current.Modes) {
		next.Modes = append([]string(nil), report.SelectedModes...)
		changed = true
	}

	if !changed {
		return
	}

	next.Updated = time.Now().UTC().Format(time.RFC3339)
	if err := mpminternal.SaveActiveJSON(&next); err != nil {
		if isatty(os.Stderr) {
			usererror.Warn("route --apply: save active.json: %v", err)
		}
	}
}

// equalStringSlices reports whether two string slices have identical contents
// in identical order. Modes are ordered in the route report (threshold-filtered,
// score-sorted) so positional equality is the right check.
func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
//
// Directive injection (opt-in, off by default):
//   - Set MPM_ROUTE_DIRECTIVES=N (N > 0) to prepend the top N prime directives
//   - Directives are pulled from the MPM database, ordered by confidence DESC
//   - DB unavailable / no directives / query error → no injection (fail open)
//   - Default behavior (env var unset) preserves the microsecond contract
func renderRoute(workspace, prompt string) (string, error) {
	if workspace == "" {
		return "", nil
	}

	router, err := mpminternal.NewRouter(workspace)
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

	// Prime directive injection (opt-in via MPM_ROUTE_DIRECTIVES=N).
	// Always runs LAST so it can prepend to the assembled body. Fail-open:
	// any failure here is silently swallowed and the original body is returned.
	// Uses fetchTopDirectivesCached to absorb concurrent route calls without
	// hammering SQLite on every prompt (see cache docs below).
	if n := directiveInjectionLimit(); n > 0 {
		if directiveText := fetchTopDirectivesCached(workspace, n); directiveText != "" {
			bodyStr := directiveText
			if body.Len() > 0 {
				bodyStr += "\n\n---\n\n"
			}
			bodyStr += body.String()
			return wrapReminder(bodyStr), nil
		}
	}

	return wrapReminder(body.String()), nil
}

// directiveInjectionLimit returns the N for directive injection, or 0 if
// disabled. Reads MPM_ROUTE_DIRECTIVES env var. Returns 0 for unset, empty,
// non-numeric, or non-positive values. The opt-in is deliberately off by
// default to preserve the "zero-latency, no round-trip" route contract.
func directiveInjectionLimit() int {
	v := os.Getenv("MPM_ROUTE_DIRECTIVES")
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// directiveCacheTTL bounds the staleness window for cached prime-directive
// injection. 5 seconds is short enough that operator-driven changes (e.g.,
// `mpm call mpm_memory '{"action":"challenge",...}'`) surface within a few
// prompts of an active conversation, and long enough to absorb a typical
// Claude Code hook burst
// (the route command is invoked on every user message in the hook chain).
// Operators who need stricter freshness can call InvalidateDirectiveCache()
// after mutating directives.
const directiveCacheTTL = 5 * time.Second

// directiveCache stores the rendered top-N directive block keyed by
// (workspace, limit). Entries auto-expire after directiveCacheTTL. The
// underlying fetchTopDirectives call only happens on cache miss; concurrent
// readers share the entry via RWMutex.RLock.
var (
	directiveCacheMu sync.RWMutex
	directiveCache   = map[string]directiveCacheEntry{}
)

type directiveCacheEntry struct {
	text      string
	expiresAt time.Time
}

// fetchTopDirectivesCached returns the cached top-N directive block when
// fresh, otherwise fetches via fetchTopDirectives and updates the cache.
// Empty input still consults the cache (workspace="", limit<=0) so the
// short-circuit lives in fetchTopDirectives itself; this function is the
// only path that touches the cache.
func fetchTopDirectivesCached(workspace string, limit int) string {
	if workspace == "" || limit <= 0 {
		return fetchTopDirectives(workspace, limit)
	}

	key := fmt.Sprintf("%s\x00%d", workspace, limit)
	now := time.Now()

	directiveCacheMu.RLock()
	entry, ok := directiveCache[key]
	directiveCacheMu.RUnlock()
	if ok && now.Before(entry.expiresAt) {
		return entry.text
	}

	// Cache miss or expired. Fetch outside the write lock so concurrent
	// callers don't block on each other's SQLite query. The double-check
	// pattern is unnecessary here: any extra fetches during the race just
	// re-populate the same key with the same value (idempotent SELECT with
	// confidence DESC LIMIT is stable for the same data).
	text := fetchTopDirectives(workspace, limit)

	directiveCacheMu.Lock()
	directiveCache[key] = directiveCacheEntry{
		text:      text,
		expiresAt: now.Add(directiveCacheTTL),
	}
	directiveCacheMu.Unlock()

	return text
}

// fetchTopDirectives returns a formatted Markdown block of the top N prime
// directives from the MPM database, sorted by confidence descending. Returns
// "" if: DB unavailable, no DB at workspace, query fails, or zero directives
// match. The function is intentionally defensive — any failure mode degrades
// to "no injection" rather than failing the route. Prime directives are
// identified by either `collection = 'directives'` (the MCP path) or
// `is_prime_directive = 1` in the metadata JSON.
//
// DB path resolution: walks the workspace looking for mpm.db, mpm.sqlite, or
// src/db/mpm.db (in that order). This matches the common MPM layouts without
// coupling route to DatabaseManager (which would require init that we want
// to keep out of the route hot path).
func fetchTopDirectives(workspace string, limit int) string {
	if limit <= 0 || workspace == "" {
		return ""
	}

	dbPath := resolveMPMDatabase(workspace)
	if dbPath == "" {
		return ""
	}

	db, err := sql.Open("sqlite3", dbPath+"?mode=ro&_journal_mode=WAL")
	if err != nil {
		return ""
	}
	defer db.Close()

	const query = `
		SELECT id, content, COALESCE(confidence, 0.8) AS conf
		FROM memories
		WHERE collection = 'directives'
		   OR json_extract(metadata, '$.is_prime_directive') = 1
		ORDER BY conf DESC, created_at DESC
		LIMIT ?`
	rows, err := db.Query(query, limit)
	if err != nil {
		return ""
	}
	defer rows.Close()

	var directives []struct {
		ID         string
		Content    string
		Confidence float64
	}
	for rows.Next() {
		var d struct {
			ID         string
			Content    string
			Confidence float64
		}
		scanErr := func() error {
			for rows.Next() {
				if err := rows.Scan(&d.ID, &d.Content, &d.Confidence); err != nil {
					return fmt.Errorf("scanning directive row for route: %w", err)
				}
				directives = append(directives, d)
			}
			return nil
		}()
		if scanErr != nil {
			usererror.Warn("fetchTopDirectives: %v", scanErr)
			return ""
		}
	}
	if len(directives) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("### Prime Directives (auto-injected, confidence-sorted)\n\n")
	for i, d := range directives {
		fmt.Fprintf(&b, "%d. **[conf=%.2f]** %s\n", i+1, d.Confidence, strings.TrimSpace(d.Content))
	}
	return b.String()
}

// resolveMPMDatabase locates the MPM SQLite database relative to the workspace.
// Returns "" if no plausible DB file is found. Common MPM layouts:
//   - <workspace>/mpm.db
//   - <workspace>/mpm.sqlite
//   - <workspace>/src/db/mpm.db
//
// The function is read-only and tolerant: missing files return "" rather than
// erroring. We use the first match in priority order.
//
// Empty workspace: returns "" immediately. Without this guard, the
// candidates resolve to relative paths (e.g. `src/db/mpm.db`) and the
// function would probe the CWD — which is the 2026-07-21 ghost-DB
// failure mode that lesson 59fe3f8ff3e1549e retired. The empty-workspace
// contract is enforced by TestResolveMPMDatabase/empty_workspace_returns_empty.
func resolveMPMDatabase(workspace string) string {
	// Empty workspace means "don't resolve" — returning a relative path
	// would let os.Stat probe CWD accidentally and pick up a fixture
	// file (e.g. when tests run from the repo root). The caller treats
	// "" as "no DB" and skips the read-only scan.
	if workspace == "" {
		return ""
	}
	candidates := []string{
		filepath.Join(workspace, "mpm.db"),
		filepath.Join(workspace, "mpm.sqlite"),
		filepath.Join(workspace, "src", "db", "mpm.db"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// wrapReminder wraps body in a <system-reminder> block with the auto-route header.
// Format: <system-reminder> + "MPM auto-route active" header + body + </system-reminder>
func wrapReminder(body string) string {
	return fmt.Sprintf("<system-reminder>\nMPM auto-route active\n\n%s\n</system-reminder>", body)
}

// _ ensures sql package is referenced even if route is built without DB driver.
// This is a forward-compat guard: if the sqlite3 driver is later removed, the
// import will still resolve at compile time. The driver is registered by the
// _ "github.com/mattn/go-sqlite3" import elsewhere in the package.
var _ = sql.ErrNoRows
