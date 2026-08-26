// cmd/mpm/services/continue_service.go — ContinueService for Wave 1.
//
// ContinueService EARNED its existence (RFC §7) by owning behaviour
// that crosses subsystem boundaries:
//
//   - Compose: assembles the dashboard for `mpm continue` by fanning
//     out to the WorkingContext service, the wake-context load, the
//     decisions/skills/theories queries, and the status snapshot.
//     This is *composition* — the cross-cutting behaviour that makes
//     `mpm continue` distinct from any single section command.
//
// If this service disappeared, every command that wanted a
// session-resumption dashboard would reimplement the fan-out
// and the per-section data-shape contracts. Composition behaviour
// lost = architectural collapse.
//
// Layering contract (RFC §7):
//   ContinueService NEVER owns SQLite queries for section data. It
//   composes other services / direct substrate reads. It does NOT
//   own rendering — DashboardRenderer does that. It does NOT call
//   commands.
//
// Composition discipline: a service that calls another service is
// composition (allowed). A service that calls another command is
// scripting (forbidden). ContinueService composes services only.
//
// Wave 1 ships a minimal Compose path:
//   - Working Context section (real, via WorkingContextService)
//   - Wake Context section (real, via DatabaseManager.GatherWakeContext)
//   - Status section (real, via DatabaseManager.GetMemoryStats)
//
// Future waves add: Recent Decisions, Loaded Skills, Active Theories,
// Active Goal. Each lands as its own wave's section addition; the
// service layer grows organically as section owners earn their place.

package main

import (
	"fmt"
	"strings"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// ContinueService orchestrates the per-section reads for `mpm continue`.
// Each section has a single owner (the data source it composes), and
// ContinueService is the only place that fans out across them.
type ContinueService struct {
	workingCtxSvc *WorkingContextService
	dm            *mpminternal.DatabaseManager
}

// NewContinueService wires the service to its dependencies. Returns
// nil if either dep is nil.
func NewContinueService(workingCtxSvc *WorkingContextService, dm *mpminternal.DatabaseManager) *ContinueService {
	if workingCtxSvc == nil || dm == nil {
		return nil
	}
	return &ContinueService{workingCtxSvc: workingCtxSvc, dm: dm}
}

// Compose assembles the dashboard model for the given session. Returns
// (*DashboardModel, error). An error from one section does NOT abort
// the whole dashboard — it surfaces as a (none)-style placeholder so
// the operator always sees a coherent output.
//
// The sessionID argument is used to scope the WorkingContext section.
// Other sections are session-agnostic (subsystem-level).
func (s *ContinueService) Compose(sessionID string) (*DashboardModel, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}
	now := time.Now().UTC()

	model := &DashboardModel{
		Title:       "MPM · Session resume",
		Subtitle:    fmt.Sprintf("session_id: %s", sessionID),
		GeneratedAt: now,
	}

	// 1. Working Context section (owning service: WorkingContextService).
	if wcSection, err := s.composeWorkingContext(sessionID, now); err == nil {
		model.Sections = append(model.Sections, *wcSection)
	} else {
		model.Sections = append(model.Sections, errorSection("Working Context", err))
	}

	// 2. Wake Context section (owning source: dm.GatherWakeContext).
	//    Errors here are non-fatal — fall back to empty section.
	model.Sections = append(model.Sections, s.composeWakeContext(now))

	// 3. Memory Status section (owning source: dm.GetMemoryStats).
	//    This is the substrate's high-level health surface — not the
	//    same as `mpm status` but provides a quick "you have N memories
	//    / X long-term" snapshot for the dashboard.
	model.Sections = append(model.Sections, s.composeMemoryStats(now))

	return model, nil
}

// composeWorkingContext loads + formats the Working Context section.
func (s *ContinueService) composeWorkingContext(sessionID string, now time.Time) (*DashboardSection, error) {
	wc, err := s.workingCtxSvc.GetCurrent(sessionID)
	if err != nil {
		return nil, err
	}
	if wc == nil {
		return &DashboardSection{
			Name: "Working Context",
			Kind: "working_context",
			Body: "  (no active working context)",
		}, nil
	}

	view := &WorkingContextView{
		SessionID:  wc.SessionID,
		Thesis:     wc.Thesis,
		Supporting: wc.Supporting,
		AgeLabel:   formatAgeUnix(wc.UpdatedAt),
		ExpiresIn:  formatExpiresInFromNowUnix(wc.ExpiresAt),
	}
	return &DashboardSection{
		Name: "Working Context",
		Kind: "working_context",
		Body: view.CompactMarkdown(),
	}, nil
}

// composeWakeContext loads the wake context and renders a compact
// summary. Errors are caught and surfaced as the (none) placeholder —
// the dashboard always shows every section.
func (s *ContinueService) composeWakeContext(now time.Time) DashboardSection {
	wake, err := s.dm.GatherWakeContextReadOnly() // pure projection — do not consume the handoff (F18)
	if err != nil {
		return errorSection("Wake Context", err)
	}
	if wake.SessionID == "" && wake.LastHandoff == nil && len(wake.GlobalRules) == 0 &&
		wake.ScratchpadOrphans == "" && len(wake.AvailableSkills) == 0 {
		return DashboardSection{
			Name: "Wake Context",
			Kind: "wake_context",
			Body: "  (no wake context loaded)",
		}
	}

	var b strings.Builder
	if wake.SessionID != "" {
		fmt.Fprintf(&b, "  session           : %s\n", wake.SessionID)
	}
	if wake.LastHandoff != nil {
		fmt.Fprintf(&b, "  last handoff      : %s\n", abbreviate(wake.LastHandoff.Summary, 80))
	}
	fmt.Fprintf(&b, "  raw / lesson      : %d / %d (ratio %.2f)\n",
		wake.EpistemicPressure.RawCount, wake.EpistemicPressure.LessonCount, wake.EpistemicPressure.Ratio)
	fmt.Fprintf(&b, "  pressure          : exceeded=%v threshold=%d\n",
		wake.EpistemicPressure.Exceeded, wake.EpistemicPressure.Threshold)
	if len(wake.AvailableSkills) > 0 {
		fmt.Fprintf(&b, "  available skills  : %d\n", len(wake.AvailableSkills))
	}
	if wake.ScratchpadOrphans != "" {
		fmt.Fprintf(&b, "  scratchpad        : orphaned scratchpads present\n")
	}
	return DashboardSection{
		Name: "Wake Context",
		Kind: "wake_context",
		Body: strings.TrimRight(b.String(), "\n"),
	}
}

// composeMemoryStats builds a tiny memory-store snapshot section.
// The full status belongs in `mpm status`; this is the dashboard
// one-liner.
//
// Source-of-truth key names verified against internal/core/web_db.go's
// GetMemoryStats. Decisions and theories live in their own collections;
// the GetMemoryStats map doesn't surface per-collection counts. We
// fall back to a direct SQL count for those.
func (s *ContinueService) composeMemoryStats(now time.Time) DashboardSection {
	stats, err := s.dm.GetMemoryStats()
	if err != nil {
		return errorSection("Memory Stats", err)
	}
	total := formatStatInt(stats, "total")
	ltm := formatStatInt(stats, "ltm")
	theories := formatStatIntRaw(s.dm, "theories", "deleted_at IS NULL")
	decisions := formatStatIntRaw(s.dm, "decisions", "deleted_at IS NULL")
	body := fmt.Sprintf("  total      : %s\n  long-term  : %s\n  theories   : %s\n  decisions  : %s",
		total, ltm, theories, decisions)
	return DashboardSection{
		Name: "Memory Stats",
		Kind: "memory_stats",
		Body: body,
	}
}

// errorSection produces a placeholder section when a section owner
// fails. The error is recorded in the section body so the operator
// can see *which* section is missing and why — never silently empty.
func errorSection(name string, err error) DashboardSection {
	msg := err.Error()
	if msg == "" {
		msg = "unknown error"
	}
	return DashboardSection{
		Name: name,
		Body: fmt.Sprintf("  ⚠ %s", msg),
	}
}

// formatExpiresInFromNow produces a human-readable "in N units" label,
// or "EXPIRED" if the expiry is in the past.
func formatExpiresInFromNow(now, expires time.Time) string {
	if expires.IsZero() {
		return "(no expiry)"
	}
	d := expires.Sub(now)
	if d <= 0 {
		return "EXPIRED"
	}
	switch {
	case d < time.Hour:
		return fmt.Sprintf("in %d minutes", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("in %d hours", int(d.Hours()))
	default:
		return fmt.Sprintf("in %d days", int(d.Hours()/24))
	}
}

// abbreviate truncates s to max chars with a trailing "…" if needed.
func abbreviate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max < 4 {
		return s[:max]
	}
	return s[:max-1] + "…"
}

// formatStatInt extracts an int value from a GetMemoryStats map.
// Returns "(unknown)" on type mismatch rather than panicking —
// dashboard sections must never crash on substrate quirk.
func formatStatInt(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case int:
			return fmt.Sprintf("%d", n)
		case int64:
			return fmt.Sprintf("%d", n)
		case float64:
			return fmt.Sprintf("%.0f", n)
		}
	}
	return "(unknown)"
}

// formatStatIntRaw runs a SELECT COUNT(*) against the memories table
// filtered by collection + optional where clause. Returns "(error)"
// on failure so dashboard rendering continues.
func formatStatIntRaw(dm *mpminternal.DatabaseManager, collection, where string) string {
	if dm == nil {
		return "(no db)"
	}
	query := "SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL AND collection = ?"
	args := []interface{}{collection}
	if where != "" {
		query += " AND " + where
	}
	var n int
	row := dm.QueryRowTracked(query, args...)
	if err := row.Scan(&n); err != nil {
		return "(error)"
	}
	return fmt.Sprintf("%d", n)
}

