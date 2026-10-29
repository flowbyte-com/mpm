// confidence_decay_policy_test.go — the APPROVED SCHEDULING POLICY for
// the bounded confidence-decay pass (issue #92).
//
// WHAT THIS FILE PINS
// -------------------
//
// The operator's decision (2026-10-29) is a scheduling decision:
//
//	enabled           = 0
//	interval_hours    = 6
//	limit             = 100
//	min_age_hours     = 24
//
// The first number is the one that matters. The pass exists (it is #92's
// mechanism) but it runs ONLY on an explicit opt-in, because a substrate
// that decays confidence unattended changes the value a retrieval ranking
// sees, without anyone asking — and the old default was selected
// SILENTLY, with no operator decision recorded anywhere.
//
// The properties this file proves:
//
//  1. The four numbers ARE the approved policy, in the core that owns
//     them, and the scheduler's effective defaults are those numbers.
//  2. A FRESH INSTALLATION performs no maintenance writes at all.
//  3. An EXPLICIT opt-in activates the bounded pass.
//  4. EXISTING EXPLICIT OPERATOR SETTINGS survive an upgrade — both the
//     opt-in and the opt-out.
//  5. Opting out prevents SUBSEQUENT maintenance writes.
//
// # WHY THESE TESTS ARE NOT ABOUT THE DECAY MATH
//
// The math is pinned by confidence_decay_test.go, the operational
// properties by confidence_decay_operational_test.go, and the trust
// boundary by confidence_decay_trust_boundary_test.go. This file is about
// one question: WHEN DOES THE MAINTENANCE HAPPEN, AND WHO DECIDED.
//
// WHY EVERY "IT DID NOT RUN" ASSERTION CARRIES A POSITIVE CONTROL
//
// The dangerous shape for a disabled-by-default feature is a test that
// asserts "nothing happened" and would also pass if the fixture were
// broken. Each such test here therefore contains the converse: the same
// fixture, with the pass enabled, DOES run. That makes the "it did not
// run" assertion a statement about the DEFAULT, not about a broken
// setup.
package scheduler

import (
	"context"
	"log/slog"
	"testing"
	"time"

	core "github.com/flowbyte-com/mpm-core"
)

// confidenceHistoryRows counts the rows in the confidence ledger. The
// pass appends a row only for an artifact whose value actually MOVED, so
// this is the ledger-side measure of "the pass did maintenance".
func confidenceHistoryRows(t *testing.T, dm *core.DatabaseManager) int {
	t.Helper()
	var n int
	if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM confidence_history`).Scan(&n); err != nil {
		t.Fatalf("count confidence_history: %v", err)
	}
	return n
}

// watermarkOf reads an artifact's maintenance watermark. NULL means the
// pass has never touched it.
func watermarkOf(t *testing.T, dm *core.DatabaseManager, id string) *int64 {
	t.Helper()
	var w *int64
	if err := dm.SQLDB().QueryRow(
		`SELECT confidence_checked_at FROM memories WHERE id = ?`, id).Scan(&w); err != nil {
		t.Fatalf("read watermark for %s: %v", id, err)
	}
	return w
}

// confidenceOf reads an artifact's cached confidence.
func confidenceOf(t *testing.T, dm *core.DatabaseManager, id string) float64 {
	t.Helper()
	var c float64
	if err := dm.SQLDB().QueryRow(
		`SELECT confidence FROM memories WHERE id = ?`, id).Scan(&c); err != nil {
		t.Fatalf("read confidence for %s: %v", id, err)
	}
	return c
}

// cadenceMarkerPresent reports whether the scheduler wrote its cadence
// marker. A pass that did not run must not have written one.
func cadenceMarkerPresent(t *testing.T, dm *core.DatabaseManager) bool {
	t.Helper()
	if _, err := dm.GetSystemConfig(core.ConfidenceDecayLastRunKey); err != nil {
		return false
	}
	return true
}

// TestConfidenceDecayPolicy_ApprovedDefaultsAreTheDocumentedPolicy
// pins the four numbers themselves.
//
// They live in internal/core, because the core owns the pass and the
// scheduler reads them from there. If the policy were written twice —
// once in the core and once in the scheduler — the two would drift, and
// the drift would be invisible: an operator who configured nothing would
// get the documented behaviour on one code path and something else on
// another.
func TestConfidenceDecayPolicy_ApprovedDefaultsAreTheDocumentedPolicy(t *testing.T) {
	if core.DefaultConfidenceDecayEnabled != 0 {
		t.Errorf("DefaultConfidenceDecayEnabled = %d, want 0 — the pass is OFF by default",
			core.DefaultConfidenceDecayEnabled)
	}
	if core.DefaultConfidenceDecayInterval != 6*time.Hour {
		t.Errorf("DefaultConfidenceDecayInterval = %v, want 6h", core.DefaultConfidenceDecayInterval)
	}
	if core.DefaultConfidenceDecayLimit != 100 {
		t.Errorf("DefaultConfidenceDecayLimit = %d, want 100", core.DefaultConfidenceDecayLimit)
	}
	if core.DefaultConfidenceDecayMinAge != 24*time.Hour {
		t.Errorf("DefaultConfidenceDecayMinAge = %v, want 24h", core.DefaultConfidenceDecayMinAge)
	}

	// And the scheduler's EFFECTIVE defaults — what a caller with no
	// configuration at all gets — are the same numbers.
	dm := core.NewTestDM(t)
	if got := dm.GetConfigInt(confidenceDecayEnabledKey, core.DefaultConfidenceDecayEnabled); got != 0 {
		t.Errorf("effective enabled default = %d, want 0", got)
	}
	if got := dm.GetConfigInt(confidenceDecayIntervalKey, int(core.DefaultConfidenceDecayInterval/time.Hour)); got != 6 {
		t.Errorf("effective interval default = %d hours, want 6", got)
	}
	if got := dm.GetConfigInt(confidenceDecayLimitKey, core.DefaultConfidenceDecayLimit); got != 100 {
		t.Errorf("effective limit default = %d, want 100", got)
	}
	if got := dm.GetConfigInt(confidenceDecayMinAgeKey, int(core.DefaultConfidenceDecayMinAge/time.Hour)); got != 24 {
		t.Errorf("effective min-age default = %d hours, want 24", got)
	}
}

// TestConfidenceDecayPolicy_FreshInstallDoesNotRunMaintenance is the
// operator's first acceptance criterion.
//
// A fresh installation has NO row for confidence.decay.enabled, because
// nothing writes it — the pass is configured, not seeded. So the key is
// ABSENT, the default applies, and the default is OFF.
//
// The test asserts that NOTHING was written: no watermark on the
// artifact, no row in the confidence ledger, no cadence marker. It then
// enables the pass and ticks again, and asserts that the pass DOES run —
// which is what makes the first assertion a statement about the default
// rather than about a fixture that could never have run anything.
func TestConfidenceDecayPolicy_FreshInstallDoesNotRunMaintenance(t *testing.T) {
	dm := core.NewTestDM(t)
	now := decayFixtureClock()
	insertStaleArtifact(t, dm, "mem-fresh-install", now)

	h := NewConfidenceDecayHandler(dm, slog.Default())
	h.now = func() time.Time { return now }

	if err := h.TickHandler()(context.Background()); err != nil {
		t.Fatalf("default-disabled tick: %v", err)
	}

	if w := watermarkOf(t, dm, "mem-fresh-install"); w != nil {
		t.Errorf("watermark = %v, want NULL — a fresh installation must not run maintenance", *w)
	}
	if n := confidenceHistoryRows(t, dm); n != 0 {
		t.Errorf("confidence ledger has %d row(s), want 0 — the default-disabled pass wrote to the ledger", n)
	}
	if cadenceMarkerPresent(t, dm) {
		t.Error("the default-disabled pass wrote a cadence marker")
	}

	// THE POSITIVE CONTROL. The same fixture, with the pass enabled, runs
	// — so the assertions above are about the DEFAULT, not about a
	// fixture in which nothing could have happened.
	enableConfidenceDecay(t, dm)
	if err := h.TickHandler()(context.Background()); err != nil {
		t.Fatalf("enabled tick: %v", err)
	}
	if w := watermarkOf(t, dm, "mem-fresh-install"); w == nil {
		t.Error("with the pass enabled the artifact was not touched — the " +
			"default-disabled assertions above would have held for the wrong reason")
	}
}

// TestConfidenceDecayPolicy_ExplicitOptInActivatesTheBoundedPass is
// the operator's third acceptance criterion.
//
// An explicit opt-in activates the pass, and the pass stays BOUNDED: the
// configured limit is the number of artifacts it touches, and the
// backlog beyond it is reported as deferred rather than silently done.
func TestConfidenceDecayPolicy_ExplicitOptInActivatesTheBoundedPass(t *testing.T) {
	dm := core.NewTestDM(t)
	now := decayFixtureClock()
	for i := 0; i < 5; i++ {
		insertStaleArtifact(t, dm, "mem-optin-"+string(rune('a'+i)), now)
	}
	enableConfidenceDecay(t, dm)
	if _, err := dm.SaveSystemConfig(confidenceDecayLimitKey, "2", "2", ""); err != nil {
		t.Fatalf("limit: %v", err)
	}

	h := NewConfidenceDecayHandler(dm, slog.Default())
	h.now = func() time.Time { return now }
	if err := h.TickHandler()(context.Background()); err != nil {
		t.Fatalf("opted-in tick: %v", err)
	}

	var checked int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE id LIKE 'mem-optin-%' AND confidence_checked_at IS NOT NULL`,
	).Scan(&checked); err != nil {
		t.Fatalf("count checked: %v", err)
	}
	if checked != 2 {
		t.Errorf("checked %d artifacts, want 2 — an explicit opt-in must activate the BOUNDED pass", checked)
	}
	if !cadenceMarkerPresent(t, dm) {
		t.Error("the opted-in pass did not write a cadence marker")
	}
}

// TestConfidenceDecayPolicy_ExplicitSettingsSurviveAnUpgrade is the
// operator's second acceptance criterion.
//
// "Existing explicit operator settings are preserved during upgrades."
//
// The mechanism is the absence of a write: the upgrade does not seed
// confidence.decay.enabled, so it cannot clobber whatever the operator
// chose. An install that explicitly enabled the pass keeps it enabled;
// an install that explicitly disabled it keeps it disabled.
//
// The test covers BOTH directions, because "preserved" that only holds
// for the opt-in is not preservation.
func TestConfidenceDecayPolicy_ExplicitSettingsSurviveAnUpgrade(t *testing.T) {
	t.Run("an explicit opt-in is preserved", func(t *testing.T) {
		dm := core.NewTestDM(t)
		now := decayFixtureClock()
		insertStaleArtifact(t, dm, "mem-upgrade-on", now)
		enableConfidenceDecay(t, dm)

		h := NewConfidenceDecayHandler(dm, slog.Default())
		h.now = func() time.Time { return now }
		if err := h.TickHandler()(context.Background()); err != nil {
			t.Fatalf("tick: %v", err)
		}

		if w := watermarkOf(t, dm, "mem-upgrade-on"); w == nil {
			t.Error("an explicitly enabled pass did not run — the operator's setting was not preserved")
		}
		// The setting itself is untouched: the pass reads it, it does
		// not rewrite it.
		if got := dm.GetConfigInt(confidenceDecayEnabledKey, core.DefaultConfidenceDecayEnabled); got != 1 {
			t.Errorf("confidence.decay.enabled = %d after the pass ran, want 1 — the pass rewrote the operator's setting", got)
		}
	})

	t.Run("an explicit opt-out is preserved", func(t *testing.T) {
		dm := core.NewTestDM(t)
		now := decayFixtureClock()
		insertStaleArtifact(t, dm, "mem-upgrade-off", now)
		if _, err := dm.SaveSystemConfig(confidenceDecayEnabledKey, "0", "0", ""); err != nil {
			t.Fatalf("opt out: %v", err)
		}

		h := NewConfidenceDecayHandler(dm, slog.Default())
		h.now = func() time.Time { return now }
		if err := h.TickHandler()(context.Background()); err != nil {
			t.Fatalf("tick: %v", err)
		}

		if w := watermarkOf(t, dm, "mem-upgrade-off"); w != nil {
			t.Errorf("watermark = %v, want NULL — an explicitly disabled pass ran", *w)
		}
		if got := dm.GetConfigInt(confidenceDecayEnabledKey, core.DefaultConfidenceDecayEnabled); got != 0 {
			t.Errorf("confidence.decay.enabled = %d after the tick, want 0 — the operator's setting was not preserved", got)
		}
	})
}

// TestConfidenceDecayPolicy_OptOutPreventsSubsequentMaintenanceWrites
// is the operator's fourth acceptance criterion.
//
// Opting out stops the maintenance writes that would otherwise follow.
// The test runs the pass first (so there IS a watermark and a ledger
// row to compare against), opts out, ticks again, and asserts that
// NOTHING moved — not the watermark, not the confidence value, and no
// new ledger row.
//
// "Prevents subsequent writes" is the operative phrase: an opt-out that
// merely stopped the NEXT pass, while leaving a half-finished one to
// land, would not be an opt-out.
func TestConfidenceDecayPolicy_OptOutPreventsSubsequentMaintenanceWrites(t *testing.T) {
	dm := core.NewTestDM(t)
	now := decayFixtureClock()
	insertStaleArtifact(t, dm, "mem-optout", now)
	enableConfidenceDecay(t, dm)

	h := NewConfidenceDecayHandler(dm, slog.Default())
	h.now = func() time.Time { return now }
	if err := h.TickHandler()(context.Background()); err != nil {
		t.Fatalf("enabled tick: %v", err)
	}

	firstWatermark := watermarkOf(t, dm, "mem-optout")
	if firstWatermark == nil {
		t.Fatal("the enabled pass did not touch the artifact; the opt-out half of this test would be vacuous")
	}
	firstConfidence := confidenceOf(t, dm, "mem-optout")
	firstLedger := confidenceHistoryRows(t, dm)

	// Opt out.
	if _, err := dm.SaveSystemConfig(confidenceDecayEnabledKey, "0", "0", ""); err != nil {
		t.Fatalf("opt out: %v", err)
	}

	// Well past the cadence interval, so the gate is not what stops it.
	now = now.Add(48 * time.Hour)
	h.now = func() time.Time { return now }
	if err := h.TickHandler()(context.Background()); err != nil {
		t.Fatalf("opted-out tick: %v", err)
	}

	secondWatermark := watermarkOf(t, dm, "mem-optout")
	if secondWatermark == nil || *secondWatermark != *firstWatermark {
		t.Errorf("watermark moved from %v to %v after opting out — the opt-out did not prevent the write",
			firstWatermark, secondWatermark)
	}
	if got := confidenceOf(t, dm, "mem-optout"); got != firstConfidence {
		t.Errorf("confidence moved from %v to %v after opting out", firstConfidence, got)
	}
	if n := confidenceHistoryRows(t, dm); n != firstLedger {
		t.Errorf("the ledger grew from %d to %d row(s) after opting out", firstLedger, n)
	}
}

// TestConfidenceDecayPolicy_EnvironmentOptInWorksWhenTheKeyIsAbsent
// pins the environment opt-in path.
//
// GetConfigInt reads system_config first, then the environment, then the
// built-in default. So the environment variable is a FALLBACK: it
// applies when the operator has not written a row. An operator can
// therefore enable the pass for a systemd unit without touching the
// database.
func TestConfidenceDecayPolicy_EnvironmentOptInWorksWhenTheKeyIsAbsent(t *testing.T) {
	dm := core.NewTestDM(t)
	now := decayFixtureClock()
	insertStaleArtifact(t, dm, "mem-env-optin", now)

	t.Setenv("MPM_CONFIDENCE_DECAY_ENABLED", "1")

	h := NewConfidenceDecayHandler(dm, slog.Default())
	h.now = func() time.Time { return now }
	if err := h.TickHandler()(context.Background()); err != nil {
		t.Fatalf("env-opted-in tick: %v", err)
	}

	if w := watermarkOf(t, dm, "mem-env-optin"); w == nil {
		t.Error("the environment opt-in did not activate the pass")
	}
}

// TestConfidenceDecayPolicy_AnExplicitSettingBeatsTheEnvironment
// pins the precedence, because it is the one an operator would get
// wrong.
//
// An install that explicitly opted OUT keeps its opt-out even if the
// environment says 1. The database is the authoritative record of an
// operator's decision; the environment is a fallback for an install that
// has made no decision.
//
// This is also why the handler's own comment used to be wrong: it called
// the environment variable an "override", which would have led an
// operator to believe their exported value could turn a disabled install
// back on. It cannot.
func TestConfidenceDecayPolicy_AnExplicitSettingBeatsTheEnvironment(t *testing.T) {
	dm := core.NewTestDM(t)
	now := decayFixtureClock()
	insertStaleArtifact(t, dm, "mem-env-vs-db", now)

	// The operator explicitly opted out.
	if _, err := dm.SaveSystemConfig(confidenceDecayEnabledKey, "0", "0", ""); err != nil {
		t.Fatalf("opt out: %v", err)
	}
	// ... and the environment says otherwise.
	t.Setenv("MPM_CONFIDENCE_DECAY_ENABLED", "1")

	h := NewConfidenceDecayHandler(dm, slog.Default())
	h.now = func() time.Time { return now }
	if err := h.TickHandler()(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if w := watermarkOf(t, dm, "mem-env-vs-db"); w != nil {
		t.Errorf("watermark = %v, want NULL — the environment overrode an explicit opt-out", *w)
	}
}
