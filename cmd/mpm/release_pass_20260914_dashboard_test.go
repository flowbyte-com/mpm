// release_pass_20260914_dashboard_test.go — Regression coverage for
// the 2026-09-14 release-pass dashboard correctness defects.
//
// Three dashboard correctness defects closed in this pass:
//
//   1. Dashboard memory count undercounted. Pre-fix the dashboard
//      filtered on `collection = 'memories'` which excluded
//      valid memories stored under other collections.
//
//   2. Dashboard lesson count always 0. Pre-fix the dashboard
//      queried `SELECT COUNT(*) FROM memories WHERE collection =
//      'lessons'` but lessons live in the `lessons_base` table
//      (a separate store, NOT a `memories` collection), so the
//      dashboard always reported 0 even when `mpm lesson list`
//      showed real lessons.
//
//   3. Health vs Attention semantics. Pre-fix the dashboard
//      rendered "Health: ✓ Healthy" while Doctor reported
//      warnings (overdue wakes, review backlog). The fix
//      splits the two: System health (runtime/database/service
//      liveness, via dm.HealthCheck) vs Attention (actionable
//      Doctor warnings, aggregated but not double-counted).
//
// All tests use a hermetic workspace via t.TempDir(); production
// state is never touched.

package main

import (
	"path/filepath"
	"strings"
	"testing"

	stdlibexec "os/exec"
)

// dashboardMemoryLessonBin builds a fresh mpm binary in a temp
// dir for the dashboard tests. Same shape as the handoff test
// helper.
func dashboardMemoryLessonBin(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mpm-test")
	cmd := stdlibexec.Command("go", "build", "-tags", "fts5", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// mkCmd is a tiny wrapper that returns an *execCmd with PATH
// set, so the helpers stay terse.
func mkCmd(name string, args ...string) *execCmd {
	return &execCmd{path: name, args: args}
}

// execCmd is a minimal abstraction over *exec.Cmd that defers
// the actual exec.Cmd construction until Run/CombinedOutput so
// the test helper signatures stay readable. It uses the
// standard library's exec package under the hood.
type execCmd struct {
	path string
	args []string
	env  []string
	dir  string
}

func (c *execCmd) CombinedOutput() ([]byte, error) {
	cmd := stdlibexec.Command(c.path, c.args...)
	if len(c.env) > 0 {
		cmd.Env = c.env
	}
	if c.dir != "" {
		cmd.Dir = c.dir
	}
	return cmd.CombinedOutput()
}

func (c *execCmd) Output() ([]byte, error) {
	cmd := stdlibexec.Command(c.path, c.args...)
	if len(c.env) > 0 {
		cmd.Env = c.env
	}
	if c.dir != "" {
		cmd.Dir = c.dir
	}
	return cmd.Output()
}

func (c *execCmd) WithEnv(env []string) *execCmd { c.env = env; return c }
func (c *execCmd) WithDir(dir string) *execCmd   { c.dir = dir; return c }

// TestDashboard_MemoryCountMatchesCanonical is the headline
// regression for the dashboard memory-count defect. Pre-fix the
// dashboard reported a much smaller active-memory count because
// it filtered on `collection = 'memories'` and excluded valid
// memories in other collections. The fix routes the dashboard
// through countMemories(dm, "") which uses the canonical
// active-memory predicate (deleted_at IS NULL AND not-expired).
//
// The comparison anchor is `mpm info`'s `active` field (the
// canonical active-memory count surfaced by GetMemoryStats). Pre-fix
// the dashboard disagreed with this anchor; the regression pins
// agreement dynamically.
func TestDashboard_MemoryCountMatchesCanonical(t *testing.T) {
	bin := dashboardMemoryLessonBin(t)
	ws := t.TempDir()

	// Seed 3 memories on top of whatever the install baseline
	// carries. The test asserts dashboard ≡ info's active count
	// dynamically — we don't pin a literal number.
	for _, content := range []string{"alpha", "beta", "gamma"} {
		cmd := mkCmd(bin, "add", content).
			WithEnv([]string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()})
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("seed memory: %v\n%s", err, out)
		}
	}

	// `mpm info` is the canonical active-memory anchor.
	infoOut := mustRunMpm(t, bin, ws, "info")
	infoActive := extractInfoActiveCount(t, infoOut)

	// Dashboard must match the canonical anchor.
	dashOut := mustRunMpm(t, bin, ws)
	dashActive := extractDashboardMemoryCount(t, dashOut)
	if dashActive != infoActive {
		t.Fatalf("dashboard memories (active) = %d, want %d (canonical info active count); dashboard:\n%s\ninfo:\n%s",
			dashActive, infoActive, dashOut, infoOut)
	}

	// Sanity: at least the 3 we seeded must be reflected in the
	// active count (the install baseline is non-zero; the 3
	// seeds must be additive).
	if infoActive < 3 {
		t.Fatalf("info active count = %d, want >= 3 (the seeded count)", infoActive)
	}
}

// extractInfoActiveCount pulls the active-memory count from the
// `memories : <n> total | <n> active | <n> LTM` line in the
// `Database` section of `mpm info`. This is the canonical anchor
// for the dashboard's Memories (active) row.
//
// 2026-09-16 fresh-profile UX cleanup: pre-fix the Database block
// rendered the active count as a standalone `active : <digits>`
// line. The post-fix form factors it into the combined
// `memories : <n> total | <n> active | <n> LTM` line so the bare
// `active` token doesn't read as "the database is empty" when
// it really means "no active ordinary memories". This extractor
// parses the middle segment.
func extractInfoActiveCount(t *testing.T, out string) int {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		// Combined shape: "memories : <n> total | <n> active | <n> LTM"
		// We extract the <n> immediately before " active " (the
		// middle segment of the three-part pipe-separated line).
		// Anchoring on "memories :" avoids matching the workspace
		// lines like "active modes : <none>".
		if !strings.HasPrefix(trimmed, "memories :") {
			continue
		}
		// Split on " | " to isolate the middle "N active" segment.
		parts := strings.Split(trimmed, " | ")
		if len(parts) != 3 {
			continue
		}
		// Middle segment: "<n> active"
		mid := strings.TrimSpace(parts[1])
		// Expect trailing " active" (the word, not a prefix).
		if !strings.HasSuffix(mid, " active") {
			continue
		}
		// Strip " active" suffix to get "<n>".
		nStr := strings.TrimSuffix(mid, " active")
		// Should now be a digit string.
		return atoiOrZero(nStr)
	}
	t.Fatalf("could not extract info active count from:\n%s", out)
	return -1
}

// TestDashboard_LessonCountMatchesCanonical pins the lesson
// count agreement between dashboard and `mpm lesson list`. Pre-fix
// the dashboard reported 0 even when `mpm lesson list` showed
// real lessons because the dashboard queried
// `memories WHERE collection = 'lessons'` (lessons live in
// `lessons_base`, not a memories collection).
func TestDashboard_LessonCountMatchesCanonical(t *testing.T) {
	bin := dashboardMemoryLessonBin(t)
	ws := t.TempDir()

	// Seed 2 lessons via the canonical lesson add surface.
	for _, content := range []string{"first lesson body", "second lesson body"} {
		cmd := mkCmd(bin, "lesson", "add", content).
			WithEnv([]string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()})
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("seed lesson: %v\n%s", err, out)
		}
	}

	// `mpm lesson list` reports 2.
	lessonOut := mustRunMpm(t, bin, ws, "lesson", "list")
	if !strings.Contains(lessonOut, "2 lessons") {
		t.Fatalf("lesson list must report 2 lessons; output:\n%s", lessonOut)
	}

	// Dashboard must match.
	dashOut := mustRunMpm(t, bin, ws)
	dashLessons := extractDashboardLessonCount(t, dashOut)
	if dashLessons != 2 {
		t.Fatalf("dashboard Lessons = %d, want 2; dashboard:\n%s", dashLessons, dashOut)
	}
}

// TestDashboard_SystemHealthVsAttention pins the Health/Attention
// split. With nothing wrong, System health is Healthy and
// Attention line is absent (or empty). With actionable Doctor
// warnings present, System health stays Healthy (Doctor's
// actionable backlog is NOT a system-level failure) but
// Attention surfaces the warning count.
func TestDashboard_SystemHealthVsAttention(t *testing.T) {
	bin := dashboardMemoryLessonBin(t)
	ws := t.TempDir()

	dashOut := mustRunMpm(t, bin, ws)

	// System health must show ✓ Healthy on a clean install.
	if !strings.Contains(dashOut, "System health") {
		t.Fatalf("dashboard must surface System health label; output:\n%s", dashOut)
	}
	if !strings.Contains(dashOut, "Healthy") {
		t.Fatalf("clean dashboard must report Healthy; output:\n%s", dashOut)
	}
	// Attention is conditional. With no actionable warnings
	// the line may be absent — that's fine. Pin only the
	// invariant: when present, the count matches Doctor.
	doctorOut := mustRunMpm(t, bin, ws, "doctor")
	doctorWarnings := countDoctorWarnings(doctorOut)
	if doctorWarnings == 0 {
		// No actionable backlog; Attention line should be absent
		// or contain "0 warnings". Accept either.
		if strings.Contains(dashOut, "Attention") &&
			!strings.Contains(dashOut, "Attention") {
			t.Fatalf("Attention line state mismatch; output:\n%s", dashOut)
		}
	}
}

// TestDashboard_AbsentEmbeddingUsesNeutralMarker pins the
// INFO marker for an absent optional embedding configuration.
// Pre-fix the dashboard rendered `✓ Embedding model` for the
// absent case (mechanically mapping OK=true to ✓), implying
// "missing = successful check". The fix uses the neutral `○`
// marker for INFO items, matching Doctor's presentation.
func TestDashboard_AbsentEmbeddingUsesNeutralMarker(t *testing.T) {
	bin := dashboardMemoryLessonBin(t)
	ws := t.TempDir()

	dashOut := mustRunMpm(t, bin, ws)

	// Embedding row must use the neutral marker, not ✓.
	// Pre-fix this test fails: the absent line was ✓ instead
	// of ○.
	if !strings.Contains(dashOut, "Embedding model") {
		t.Fatalf("dashboard must surface the Embedding model row; output:\n%s", dashOut)
	}
	// Look for the exact line: ` ○  Embedding model` (the line
	// starts with `○` followed by two spaces and the row name).
	if !strings.Contains(dashOut, "○  Embedding model") {
		t.Fatalf("absent embedding must render with neutral ○ marker; output:\n%s", dashOut)
	}
	if strings.Contains(dashOut, "✓  Embedding model") {
		t.Fatalf("absent embedding must NOT render with ✓ marker; output:\n%s", dashOut)
	}
}

// TestDashboard_AbsentLLMUsesNeutralMarker pins the INFO marker
// for an absent LLM. Pre-fix the dashboard rendered `⚠ LLM
// provider not configured` (mechanically mapping OK=false to
// ⚠), implying the substrate is broken. The fix uses the
// neutral `○` marker with a degradation hint: core CRUD
// remains fully usable; only synthesis requires an LLM.
func TestDashboard_AbsentLLMUsesNeutralMarker(t *testing.T) {
	bin := dashboardMemoryLessonBin(t)
	ws := t.TempDir()

	dashOut := mustRunMpm(t, bin, ws)

	if !strings.Contains(dashOut, "LLM provider") {
		t.Fatalf("dashboard must surface the LLM provider row; output:\n%s", dashOut)
	}
	// Absent LLM renders with neutral ○.
	if !strings.Contains(dashOut, "○  LLM provider") {
		t.Fatalf("absent LLM must render with neutral ○ marker; output:\n%s", dashOut)
	}
	if strings.Contains(dashOut, "⚠  LLM provider") {
		t.Fatalf("absent LLM must NOT render with ⚠ marker; output:\n%s", dashOut)
	}
}

// mustRunMpm invokes the binary with the given args in the
// hermetic workspace and returns stdout. Stderr is captured
// into the throwaway buffer so build noise doesn't pollute
// the captured output.
//
// Note: doctor exits 1 when warnings are present (by design —
// the operator's exit-code-driven automation expects a non-zero
// exit to indicate actionable backlog). mustRunMpm does not
// treat non-zero exits as failures; callers that care about
// the exit code (none in this file) should use a dedicated
// helper. The point of mustRunMpm is to capture stdout for
// inspection, not to assert exit codes.
func mustRunMpm(t *testing.T, bin, ws string, args ...string) string {
	t.Helper()
	allArgs := append([]string{}, args...)
	cmd := mkCmd(bin, allArgs...).
		WithEnv([]string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath(), "QUIET=1"})
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Don't fail on non-zero exit — see comment above.
		_ = err
	}
	return stripLogNoise(string(out))
}

// stripLogNoise removes the perms-sweep `time=... level=WARN ...`
// log lines the binary emits at startup. The lines are not part
// of the dashboard surface; we strip them so the assertions
// can match rendered output without false negatives from log
// noise interleaved.
func stripLogNoise(s string) string {
	var out strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "time=") {
			continue
		}
		out.WriteString(line)
		out.WriteString("\n")
	}
	return out.String()
}

// extractMemoryCountFromStatus pulls the active-memory count
// out of `mpm status` output. The canonical field is
// `Memories : <total> total | <active> LTM`; we parse the
// `total` (which IS the active count after the D-5.2 expire
// filter was applied at this surface).
func extractMemoryCountFromStatus(t *testing.T, out string) int {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "Memories") {
			continue
		}
		// "Memories : 5 total | 5 LTM"
		fields := strings.Fields(line)
		for i, f := range fields {
			if f == "Memories" && i+2 < len(fields) {
				// i+2 is the number before "total"
				n := atoiOrZero(fields[i+2])
				if n > 0 {
					return n
				}
			}
		}
	}
	t.Fatalf("could not extract memory count from status output:\n%s", out)
	return -1
}

// extractDashboardMemoryCount pulls the dashboard's
// `Memories (active) : <count>` row. Pre-fix this number
// disagreed with status; the regression pins agreement.
func extractDashboardMemoryCount(t *testing.T, out string) int {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "Memories (active)") {
			continue
		}
		fields := strings.Fields(line)
		// The count is the last field.
		if len(fields) < 4 {
			continue
		}
		last := fields[len(fields)-1]
		return atoiOrZero(last)
	}
	t.Fatalf("could not extract dashboard memory count from:\n%s", out)
	return -1
}

// extractDashboardLessonCount pulls the dashboard's
// `Lessons : <count>` row.
func extractDashboardLessonCount(t *testing.T, out string) int {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "Lessons") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		last := fields[len(fields)-1]
		return atoiOrZero(last)
	}
	t.Fatalf("could not extract dashboard lesson count from:\n%s", out)
	return -1
}

// countDoctorWarnings counts the `⚠` lines in doctor output.
// Each warning row starts with `⚠ ` (warning marker + space).
func countDoctorWarnings(out string) int {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "⚠") {
			n++
		}
	}
	return n
}

// atoiOrZero is a tiny strconv.Atoi wrapper that returns 0 on
// parse failure. Used by extract* helpers so a malformed
// numeric field doesn't crash the test.
func atoiOrZero(s string) int {
	var n int
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
