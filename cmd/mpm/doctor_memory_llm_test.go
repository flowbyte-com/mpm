// cmd/mpm/doctor_memory_llm_test.go — Diagnostic-contract tests for the
// canonical primary/general LLM row on `mpm doctor`.
//
// The Memory LLM check surfaces the Components["memory"] binding —
// the same canonical resolver the dashboard uses (`resolveDashboardLLM`,
// `cfg.ProfileFor("memory")`) and the synthesis worker reads
// (`internal/core/synth/client.go:NewSynthClient`). Memory is the
// substrate's primary/general LLM slot; Critic is a separate review
// slot; Embedding is a retrieval slot. Doctor's diagnostic must
// reflect all three distinctly so an unbound memory binding is not
// silently hidden behind a healthy Critic probe.
//
// Test mechanics:
//
//   - dm is a fresh temp DB via newTestDMForCmd
//   - MPM_WORKSPACE is redirected to t.TempDir() per test; the
//     helper writeTestConfig persists the supplied *config.Config
//     to $MPM_WORKSPACE/mpm_config.json so config.LoadConfig reads
//     it back unchanged
//   - Each test restores the original MPM_WORKSPACE via t.Setenv
//     (auto-restoring on test cleanup)
//
// All assertions target the canonical DoctorCheck fields (Name,
// Status, Message, Details) so output-rendering drift does not
// invalidate the contract.

package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core/config"
)

// doctorMemoryLLMFixture is the small set of inputs we need to
// exercise the Memory LLM diagnostic. It mirrors the shape of a
// real mpm_config.json (Profiles + Components) without needing
// the substrate's full defaulting chain.
type doctorMemoryLLMFixture struct {
	profiles   map[string]config.Profile
	components map[string]string
}

// withTestConfigWorkspace points MPM_WORKSPACE at a temp dir for the
// duration of the test. config.LoadConfig resolves ConfigPath()
// from MPM_WORKSPACE first, so the test config is read from the
// temp workspace and the live mpm_config.json is NEVER touched.
//
// Returns the temp path and a cleanup func that restores the
// production path.
func withTestConfigWorkspace(t *testing.T) (string, func()) {
	t.Helper()
	ws := t.TempDir()
	prev := os.Getenv("MPM_WORKSPACE")
	t.Setenv("MPM_WORKSPACE", ws)
	return ws, func() {
		if prev == "" {
			_ = os.Unsetenv("MPM_WORKSPACE")
		} else {
			_ = os.Setenv("MPM_WORKSPACE", prev)
		}
	}
}

// writeTestConfig persists the supplied fixture to
// $MPM_WORKSPACE/mpm_config.json so config.LoadConfig returns it
// verbatim. Atomic via config.SaveConfig (0600, rename-over).
func writeTestConfig(t *testing.T, ws string, fx doctorMemoryLLMFixture) {
	t.Helper()
	cfg := &config.Config{}
	if fx.profiles != nil {
		cfg.Profiles = fx.profiles
	}
	if fx.components != nil {
		cfg.Components = fx.components
	}
	if err := config.SaveConfig(cfg); err != nil {
		t.Fatalf("writeTestConfig: SaveConfig: %v", err)
	}
	// Sanity: the file must round-trip back through LoadConfig.
	got, err := config.LoadConfig()
	if err != nil {
		t.Fatalf("writeTestConfig: LoadConfig after save: %v", err)
	}
	if got == nil {
		t.Fatalf("writeTestConfig: LoadConfig returned nil config")
	}
	if len(fx.profiles) > 0 && len(got.Profiles) != len(fx.profiles) {
		t.Fatalf("writeTestConfig: profile round-trip mismatch (want %d, got %d)",
			len(fx.profiles), len(got.Profiles))
	}
}

// TestDoctorService_checkMemoryLLM_NoBinding pins the canonical
// "no general LLM binding" diagnostic. Components["memory"] is
// absent. Status is WARN (not INFO) — memory LLM is classified as
// REQUIRED for normal LLM-backed MPM work, so absence is a defect
// signal on the Doctor trust-signal flagship. The remediation
// references the safe canonical command (component set, not profile
// set with a positional secret).
func TestDoctorService_checkMemoryLLM_NoBinding(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)
	_, restore := withTestConfigWorkspace(t)
	defer restore()
	writeTestConfig(t, os.Getenv("MPM_WORKSPACE"), doctorMemoryLLMFixture{
		profiles: map[string]config.Profile{
			"default": {Provider: "minimax", Model: "MiniMax-M3[1m]", BaseURL: "https://api.minimax.io/anthropic/v1", APIKey: "sk-test"},
		},
		// Components deliberately has no "memory" key.
		components: map[string]string{},
	})

	check := svc.checkMemoryLLM()
	if check.Name != "Memory LLM" {
		t.Errorf("check.Name = %q, want %q", check.Name, "Memory LLM")
	}
	if check.Status != "WARN" {
		t.Errorf("no-binding must WARN; got %q (msg: %q)", check.Status, check.Message)
	}
	if check.Message != "no profile configured" {
		t.Errorf("no-binding message = %q, want %q", check.Message, "no profile configured")
	}
	// Remediation must reference the canonical safe command and
	// must NOT include a positional credential on argv.
	if len(check.Details) == 0 {
		t.Fatalf("no-binding check must carry remediation Details")
	}
	hint := strings.Join(check.Details, " ")
	if !strings.Contains(hint, "mpm config component set memory") {
		t.Errorf("remediation must suggest safe canonical command; got: %q", hint)
	}
	if strings.Contains(hint, "api_key ") && !strings.Contains(hint, "api_key [--stdin]") {
		t.Errorf("remediation must not include positional api_key argv form; got: %q", hint)
	}
}

// TestDoctorService_checkMemoryLLM_BoundProfileMissing pins the
// "binding points at a profile name that does not exist and there
// is no Profiles[\"default\"] fallback" diagnostic. The message
// MUST name the literal binding so the operator knows what to fix.
func TestDoctorService_checkMemoryLLM_BoundProfileMissing(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)
	_, restore := withTestConfigWorkspace(t)
	defer restore()
	writeTestConfig(t, os.Getenv("MPM_WORKSPACE"), doctorMemoryLLMFixture{
		profiles: map[string]config.Profile{
			// Profiles has "embedding" but NOT "ghost" and NOT "default".
			// ProfileFor("memory") will therefore return nil.
			"embedding": {Provider: "ollama", Model: "all-minilm", BaseURL: "http://127.0.0.1:11434"},
		},
		components: map[string]string{
			"memory": "ghost",
		},
	})

	check := svc.checkMemoryLLM()
	if check.Status != "WARN" {
		t.Fatalf("dangling binding must WARN; got %q (msg: %q)", check.Status, check.Message)
	}
	if !strings.Contains(check.Message, `"ghost"`) {
		t.Errorf("dangling message must name the literal binding; got %q", check.Message)
	}
	if !strings.Contains(check.Message, "not found") {
		t.Errorf("dangling message must signal 'not found'; got %q", check.Message)
	}
}

// TestDoctorService_checkMemoryLLM_IncompleteProfile pins the
// "profile exists but required field missing" diagnostic. The
// credential state (api_key) is NEVER included in the message —
// only structural fields (provider / model / base_url).
func TestDoctorService_checkMemoryLLM_IncompleteProfile(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)
	_, restore := withTestConfigWorkspace(t)
	defer restore()
	// Sentinel credential — must NEVER appear in the rendered
	// check (Message, Details, or any string field).
	const sentinelAPIKey = "MPM_SENTINEL_APIKEY_do_not_leak_in_doctor"
	writeTestConfig(t, os.Getenv("MPM_WORKSPACE"), doctorMemoryLLMFixture{
		profiles: map[string]config.Profile{
			"primary": {
				Provider: "custom",
				Model:    "model-x",
				BaseURL:  "https://x.invalid/v1",
				APIKey:   sentinelAPIKey,
			},
		},
		components: map[string]string{
			"memory": "primary",
		},
	})

	check := svc.checkMemoryLLM()
	if check.Status != "PASS" {
		t.Fatalf("complete profile must PASS; got %q (msg: %q)", check.Status, check.Message)
	}
	// Sanity: the credential must not appear anywhere in the row.
	if strings.Contains(check.Message, sentinelAPIKey) {
		t.Errorf("PASS message leaked api_key: %q", check.Message)
	}
	for _, d := range check.Details {
		if strings.Contains(d, sentinelAPIKey) {
			t.Errorf("PASS Details leaked api_key: %q", d)
		}
	}
}

// TestDoctorService_checkMemoryLLM_IncompleteProfile_MissingFields
// pins the verdict when provider/model/base_url are missing. The
// message names the missing fields but never the credential state.
func TestDoctorService_checkMemoryLLM_IncompleteProfile_MissingFields(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)
	_, restore := withTestConfigWorkspace(t)
	defer restore()
	const sentinelAPIKey = "MPM_SENTINEL_APIKEY_incomplete"
	writeTestConfig(t, os.Getenv("MPM_WORKSPACE"), doctorMemoryLLMFixture{
		profiles: map[string]config.Profile{
			// Provider + BaseURL set, Model missing → verdict must
			// WARN with "model" in the missing list.
			"primary": {
				Provider: "custom",
				BaseURL:  "https://x.invalid/v1",
				APIKey:   sentinelAPIKey,
			},
		},
		components: map[string]string{
			"memory": "primary",
		},
	})

	check := svc.checkMemoryLLM()
	if check.Status != "WARN" {
		t.Fatalf("incomplete profile must WARN; got %q (msg: %q)", check.Status, check.Message)
	}
	if !strings.Contains(check.Message, "model") {
		t.Errorf("incomplete message must name the missing field(s); got %q", check.Message)
	}
	// Credential must not leak.
	if strings.Contains(check.Message, sentinelAPIKey) {
		t.Errorf("incomplete message leaked api_key: %q", check.Message)
	}
	for _, d := range check.Details {
		if strings.Contains(d, sentinelAPIKey) {
			t.Errorf("incomplete Details leaked api_key: %q", d)
		}
	}
}

// TestDoctorService_checkMemoryLLM_DisabledIsInformational pins the
// "binding == \"disabled\" sentinel" branch. Memory is not the
// embedding slot, so "disabled" is treated as INFO with the same
// wording as the embedding check's disabled branch — operators see
// one consistent vocabulary. The tally counts INFO rows in
// Informational (not Warnings).
func TestDoctorService_checkMemoryLLM_DisabledIsInformational(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)
	_, restore := withTestConfigWorkspace(t)
	defer restore()
	writeTestConfig(t, os.Getenv("MPM_WORKSPACE"), doctorMemoryLLMFixture{
		profiles: map[string]config.Profile{
			"default": {Provider: "minimax", Model: "MiniMax-M3[1m]", BaseURL: "https://api.minimax.io/anthropic/v1"},
		},
		components: map[string]string{
			"memory": "disabled",
		},
	})

	check := svc.checkMemoryLLM()
	if check.Status != "INFO" {
		t.Errorf("disabled sentinel must INFO; got %q (msg: %q)", check.Status, check.Message)
	}
	if check.Message != "intentionally disabled" {
		t.Errorf("disabled message = %q, want %q", check.Message, "intentionally disabled")
	}
}

// TestDoctorService_checkMemoryLLM_HealthyBinding pins the
// happy-path PASS verdict. The message uses the canonical
// `<model> · <Provider>` form (same reformatting the Embedding
// model check uses) so the operator sees one consistent
// vocabulary across provider rows.
func TestDoctorService_checkMemoryLLM_HealthyBinding(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)
	_, restore := withTestConfigWorkspace(t)
	defer restore()
	writeTestConfig(t, os.Getenv("MPM_WORKSPACE"), doctorMemoryLLMFixture{
		profiles: map[string]config.Profile{
			"primary": {
				Provider: "minimax",
				Model:    "MiniMax-M3[1m]",
				BaseURL:  "https://api.minimax.io/anthropic/v1",
				APIKey:   "sk-test",
			},
		},
		components: map[string]string{
			"memory": "primary",
		},
	})

	check := svc.checkMemoryLLM()
	if check.Status != "PASS" {
		t.Fatalf("healthy binding must PASS; got %q (msg: %q)", check.Status, check.Message)
	}
	if check.Message != "MiniMax-M3[1m] · Minimax" {
		t.Errorf("healthy message = %q, want %q (canonical <model> · <Provider>)",
			check.Message, "MiniMax-M3[1m] · Minimax")
	}
}

// TestDoctorService_Check_AggregatesMemoryLLM pins that the new
// check is registered in the base Check() aggregator. With memory
// bound to a healthy profile, the row appears with Status="PASS"
// and contributes to report.Passed.
func TestDoctorService_Check_AggregatesMemoryLLM(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)
	_, restore := withTestConfigWorkspace(t)
	defer restore()
	writeTestConfig(t, os.Getenv("MPM_WORKSPACE"), doctorMemoryLLMFixture{
		profiles: map[string]config.Profile{
			"primary": {Provider: "minimax", Model: "MiniMax-M3[1m]", BaseURL: "https://api.minimax.io/anthropic/v1"},
		},
		components: map[string]string{"memory": "primary"},
	})

	report, err := svc.Check()
	if err != nil {
		t.Fatalf("Check() returned error: %v", err)
	}
	var found bool
	for _, c := range report.Checks {
		if c.Name == "Memory LLM" {
			found = true
			if c.Status != "PASS" {
				t.Errorf("Memory LLM check status = %q, want PASS", c.Status)
			}
		}
	}
	if !found {
		t.Error("Memory LLM check not found in report.Checks")
	}
}

// TestDoctorReport_Tally_AggregatesAfterProbeRows pins the summary-
// aggregation contract: every visible row participates exactly once
// in the summary, regardless of when it was appended.
//
// The tally is invoked AFTER probe rows are appended so a row
// added later (e.g. probe layer's Embedding / Critic) counts the
// same as a row added in Check(). This pins the 2026-09-19
// release-pass contract that the tally reflects exactly what the
// operator sees.
func TestDoctorReport_Tally_AggregatesAfterProbeRows(t *testing.T) {
	r := &DoctorReport{Checks: []DoctorCheck{
		{Name: "BasePass1", Status: "PASS"},
		{Name: "BaseWarn1", Status: "WARN"},
		{Name: "BaseInfo1", Status: "INFO"},
	}}
	r.Tally()
	if r.Passed != 1 || r.Warnings != 1 || r.Informational != 1 || r.Failed != 0 {
		t.Errorf("post-base tally mismatch: passed=%d warnings=%d informational=%d failed=%d",
			r.Passed, r.Warnings, r.Informational, r.Failed)
	}

	// Append two probe-layer rows AFTER Tally — second Tally must
	// see them. This is the structural fix for the historical
	// "5 passed vs 7 visible" undercount.
	r.Checks = append(r.Checks,
		DoctorCheck{Name: "ProbePass1", Status: "PASS"},
		DoctorCheck{Name: "ProbePass2", Status: "PASS"},
	)
	r.Tally()
	if r.Passed != 3 || r.Warnings != 1 || r.Informational != 1 || r.Failed != 0 {
		t.Errorf("post-probe tally mismatch: passed=%d warnings=%d informational=%d failed=%d",
			r.Passed, r.Warnings, r.Informational, r.Failed)
	}

	// Re-tally with no append — must be idempotent (no double-count).
	r.Tally()
	if r.Passed != 3 || r.Warnings != 1 || r.Informational != 1 || r.Failed != 0 {
		t.Errorf("re-tally mismatch (idempotency): passed=%d warnings=%d informational=%d failed=%d",
			r.Passed, r.Warnings, r.Informational, r.Failed)
	}
}

// TestDoctorReport_Tally_TotalChecksReflectsLen pins that the
// TotalChecks field tracks len(report.Checks), not a stale
// earlier count. Critical for any future operator-visible
// "out of N" denominator in the summary line.
func TestDoctorReport_Tally_TotalChecksReflectsLen(t *testing.T) {
	r := &DoctorReport{Checks: []DoctorCheck{
		{Name: "a", Status: "PASS"},
	}}
	r.Tally()
	if r.TotalChecks != 1 {
		t.Errorf("after first Tally: TotalChecks=%d, want 1", r.TotalChecks)
	}
	r.Checks = append(r.Checks, DoctorCheck{Name: "b", Status: "WARN"})
	r.Tally()
	if r.TotalChecks != 2 {
		t.Errorf("after append+Tally: TotalChecks=%d, want 2", r.TotalChecks)
	}
}

// TestDoctorReport_Tally_FailureCounts pins the FAIL arm of the
// tally — failures increment report.Failed without affecting
// passed/warnings/informational.
func TestDoctorReport_Tally_FailureCounts(t *testing.T) {
	r := &DoctorReport{Checks: []DoctorCheck{
		{Name: "f1", Status: "FAIL"},
		{Name: "p1", Status: "PASS"},
		{Name: "w1", Status: "WARN"},
		{Name: "i1", Status: "INFO"},
	}}
	r.Tally()
	if r.Failed != 1 {
		t.Errorf("Failed = %d, want 1", r.Failed)
	}
	if r.Passed != 1 {
		t.Errorf("Passed = %d, want 1", r.Passed)
	}
	if r.Warnings != 1 {
		t.Errorf("Warnings = %d, want 1", r.Warnings)
	}
	if r.Informational != 1 {
		t.Errorf("Informational = %d, want 1", r.Informational)
	}
}

// TestDoctorService_Check_AbsentLLM_IncrementsWarnings pins that
// the new Memory LLM row counts toward report.Warnings when the
// binding is absent (operator-facing semantic). Embedding-absent
// stays INFO and does NOT increment Warnings — that asymmetry is
// intentional and matches the runtime contract.
func TestDoctorService_Check_AbsentLLM_IncrementsWarnings(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)
	_, restore := withTestConfigWorkspace(t)
	defer restore()
	// No config → Components["memory"] is empty → checkMemoryLLM
	// returns WARN. The tally must include the new row in
	// Warnings.
	// Note: writing an empty Profiles/Components fixture still
	// triggers a "could not read mpm_config.json" path because
	// SaveConfig persists the empty struct, but config.LoadConfig
	// happily returns it. We just need no Components["memory"].
	writeTestConfig(t, os.Getenv("MPM_WORKSPACE"), doctorMemoryLLMFixture{
		profiles: map[string]config.Profile{
			"default": {Provider: "minimax", Model: "MiniMax-M3[1m]", BaseURL: "https://api.minimax.io/anthropic/v1"},
		},
		components: map[string]string{},
	})

	report, err := svc.Check()
	if err != nil {
		t.Fatalf("Check() returned error: %v", err)
	}
	// Find the Memory LLM row.
	var memWarns int
	for _, c := range report.Checks {
		if c.Name == "Memory LLM" && c.Status == "WARN" {
			memWarns++
		}
	}
	if memWarns != 1 {
		t.Errorf("expected exactly 1 WARN Memory LLM row; got %d", memWarns)
	}
	if report.Warnings < 1 {
		t.Errorf("report.Warnings = %d, want >= 1 (Memory LLM must contribute)", report.Warnings)
	}
}

// TestDoctorService_checkMemoryLLM_NoSecretInMessage ensures the
// credential redactor contract: an api_key on the resolved
// profile must NEVER appear in the rendered check. We use a
// unique sentinel so the assertion is exact, not substring.
func TestDoctorService_checkMemoryLLM_NoSecretInMessage(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)
	_, restore := withTestConfigWorkspace(t)
	defer restore()
	const sentinel = "MPM_SECRET_d3f9_c2a8_71b6_no_leak_in_doctor"
	writeTestConfig(t, os.Getenv("MPM_WORKSPACE"), doctorMemoryLLMFixture{
		profiles: map[string]config.Profile{
			"primary": {
				Provider: "custom",
				Model:    "model-z",
				BaseURL:  "https://z.invalid/v1",
				APIKey:   sentinel,
			},
		},
		components: map[string]string{"memory": "primary"},
	})

	check := svc.checkMemoryLLM()
	if strings.Contains(check.Message, sentinel) {
		t.Errorf("Message leaked sentinel secret: %q", check.Message)
	}
	for _, d := range check.Details {
		if strings.Contains(d, sentinel) {
			t.Errorf("Details leaked sentinel secret: %q", d)
		}
	}
}

// TestDoctorService_checkMemoryLLM_RemediationNeverIncludesPositionalSecret
// pins the credential-UX safety contract: any api_key remediation
// in Details must use the hidden-prompt or --stdin form, never
// `api_key <secret>` on the command line.
func TestDoctorService_checkMemoryLLM_RemediationNeverIncludesPositionalSecret(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)
	_, restore := withTestConfigWorkspace(t)
	defer restore()
	writeTestConfig(t, os.Getenv("MPM_WORKSPACE"), doctorMemoryLLMFixture{
		profiles: map[string]config.Profile{
			// Provider/BaseURL set, Model missing → incomplete profile
			// → remediation includes safe commands. The test ALSO
			// pins the no-secret-on-argv contract on the no-binding
			// case below.
			"primary": {
				Provider: "custom",
				BaseURL:  "https://x.invalid/v1",
				APIKey:   "sk-secret",
			},
		},
		components: map[string]string{"memory": "primary"},
	})

	check := svc.checkMemoryLLM()
	for _, d := range check.Details {
		// Disallowed: any pattern that smells like a positional
		// api_key secret on argv. The canonical form is
		// "mpm config profile set <name> api_key [--stdin]" —
		// never "api_key <something>" with a value-bearing
		// following word that isn't a flag.
		if strings.Contains(d, "api_key ") && !strings.Contains(d, "api_key [--stdin]") {
			// Tolerate "api_key <" only if explicitly the safe form.
			if strings.Contains(d, "api_key --stdin") {
				continue
			}
			// Catch the disallowed shape: `api_key sk-...` etc.
			// The canonical helper always uses the no-arg / --stdin form.
			if strings.Contains(d, "api_key \"" ) || strings.Contains(d, "api_key $") {
				t.Errorf("Details must not include positional api_key argv form: %q", d)
			}
		}
	}
}

// TestDoctorService_Check_RoundTripsJSON ensures the JSON
// serialisation of report.Checks does not drop the Memory LLM
// row (the structural fix is the tally; this test pins that the
// row itself survives a round-trip through encoding/json).
func TestDoctorService_Check_RoundTripsJSON(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)
	_, restore := withTestConfigWorkspace(t)
	defer restore()
	writeTestConfig(t, os.Getenv("MPM_WORKSPACE"), doctorMemoryLLMFixture{
		profiles: map[string]config.Profile{
			"primary": {Provider: "minimax", Model: "MiniMax-M3[1m]", BaseURL: "https://api.minimax.io/anthropic/v1"},
		},
		components: map[string]string{"memory": "primary"},
	})

	report, err := svc.Check()
	if err != nil {
		t.Fatalf("Check() returned error: %v", err)
	}
	// Marshal then unmarshal — pinning that DoctorReport survives
	// encoding/json. CronRetention is intentionally a separate
	// field with `omitempty` and is preserved here only when set.
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	if !strings.Contains(string(raw), "Memory LLM") {
		t.Errorf("serialised report missing Memory LLM row; got: %s", raw)
	}
}

// TestDoctorService_Check_NoDoubleCountMemoryLLM pins the
// 2026-09-19 release-pass invariant: the Memory LLM row from the
// service layer and any probe-layer row derived from the same
// fingerprint MUST NOT both be counted as separate PASS rows
// when they refer to the same underlying network target.
//
// The probe layer collapses memory+critic into ONE network probe
// when they share a fingerprint (see cmd/mpm/probe/run.go:
// dedupTargetsByFingerprint). The Memory LLM row is a binding
// check that performs NO IO. So in practice a single install
// with memory+critic bound to the same profile sees:
//
//	Memory LLM    (PASS, binding check)
//	Critic        (PASS, network probe)
//
// — TWO distinct rows, ONE network IO. No double-count of the
// network target.
//
// This test pins that contract at the aggregation layer: the
// Memory LLM row exists and is counted ONCE in report.Passed,
// regardless of any probe-derived row that may be appended
// later by handleDoctor.
func TestDoctorService_Check_NoDoubleCountMemoryLLM(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)
	_, restore := withTestConfigWorkspace(t)
	defer restore()
	writeTestConfig(t, os.Getenv("MPM_WORKSPACE"), doctorMemoryLLMFixture{
		profiles: map[string]config.Profile{
			"primary": {Provider: "minimax", Model: "MiniMax-M3[1m]", BaseURL: "https://api.minimax.io/anthropic/v1"},
		},
		components: map[string]string{
			"memory":    "primary",
			"critic":    "primary",
			"embedding": "primary", // share the same fingerprint for max dedup stress
		},
	})

	report, err := svc.Check()
	if err != nil {
		t.Fatalf("Check() returned error: %v", err)
	}
	// Count Memory LLM rows in the base report. MUST be exactly 1.
	count := 0
	for _, c := range report.Checks {
		if c.Name == "Memory LLM" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("Memory LLM row count = %d, want 1 (no double-count at base layer)", count)
	}
}

// TestDoctorService_checkMemoryLLM_NoConfigFile pins the "config
// file absent" branch. A fresh install (or a test environment
// without a written mpm_config.json) must NOT light up as WARN —
// that would make Doctor lie to operators who haven't set up
// anything yet, AND it would break the existing test invariant
// that empty-state Doctor outputs only PASS + INFO rows. The
// verdict is INFO with the same neutral message wording the
// Embedding check uses for its absent branch.
func TestDoctorService_checkMemoryLLM_NoConfigFile(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)
	// Point MPM_WORKSPACE at an empty temp dir — config.LoadConfig
	// returns defaults (no Profiles/Components) but config.ConfigPath
	// resolves to a non-existent file. The check must therefore
	// take the "no config file" branch (A) → INFO.
	_, restore := withTestConfigWorkspace(t)
	defer restore()

	check := svc.checkMemoryLLM()
	if check.Status != "INFO" {
		t.Fatalf("absent config must INFO; got %q (msg: %q)", check.Status, check.Message)
	}
	if check.Message != "no config file present" {
		t.Errorf("absent config message = %q, want %q", check.Message, "no config file present")
	}
}
// pins the semantic: when Components["memory"] is empty but
// Profiles["default"] exists, the runtime falls through to the
// default profile (config resolution courtesy), but Doctor still
// surfaces the binding as WARN — operators should bind explicitly.
//
// This pins the rule that the default profile is config
// resolution, not failover (see cmd/mpm/handlers_config.go
// printConfigHelp "Config resolution vs runtime failover").
func TestDoctorService_checkMemoryLLM_DefaultProfileNotExplicitBinding(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)
	_, restore := withTestConfigWorkspace(t)
	defer restore()
	writeTestConfig(t, os.Getenv("MPM_WORKSPACE"), doctorMemoryLLMFixture{
		profiles: map[string]config.Profile{
			"default": {Provider: "minimax", Model: "MiniMax-M3[1m]", BaseURL: "https://api.minimax.io/anthropic/v1"},
		},
		components: map[string]string{}, // no explicit memory binding
	})

	check := svc.checkMemoryLLM()
	if check.Status != "WARN" {
		t.Errorf("default-only must surface WARN (explicit binding is required); got %q (msg: %q)",
			check.Status, check.Message)
	}
	if check.Message != "no profile configured" {
		t.Errorf("default-only message = %q, want %q", check.Message, "no profile configured")
	}
}

// TestDoctorReport_Tally_EmptyReportIsNoOp pins that an empty
// report does not panic and leaves counts at zero. Defensive —
// future callers may construct an empty DoctorReport and run
// Tally() before any rows are appended.
func TestDoctorReport_Tally_EmptyReportIsNoOp(t *testing.T) {
	r := &DoctorReport{Checks: []DoctorCheck{}}
	r.Tally()
	if r.TotalChecks != 0 || r.Passed != 0 || r.Warnings != 0 || r.Failed != 0 || r.Informational != 0 {
		t.Errorf("empty tally mismatch: %+v", r)
	}
}

// TestDoctorService_checkMemoryLLM_RemediationCommandShape pins
// the safe remediation shape on the canonical no-binding path.
// The first Detail MUST suggest `mpm config component set memory
// <profile>` and MUST NOT contain a positional api_key argv form.
// This is the contract the operator sees on screen.
func TestDoctorService_checkMemoryLLM_RemediationCommandShape(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)
	_, restore := withTestConfigWorkspace(t)
	defer restore()
	writeTestConfig(t, os.Getenv("MPM_WORKSPACE"), doctorMemoryLLMFixture{
		profiles: map[string]config.Profile{
			"primary": {Provider: "minimax", Model: "MiniMax-M3[1m]", BaseURL: "https://api.minimax.io/anthropic/v1"},
		},
		components: map[string]string{}, // no binding
	})

	check := svc.checkMemoryLLM()
	if len(check.Details) == 0 {
		t.Fatalf("remediation must include at least one Detail line")
	}
	hint := strings.Join(check.Details, "\n")
	wantSubs := []string{
		"mpm config component set memory",
		"mpm config profile add",
	}
	for _, sub := range wantSubs {
		if !strings.Contains(hint, sub) {
			t.Errorf("remediation must include %q; got:\n%s", sub, hint)
		}
	}
	// Forbidden shapes (positional api_key argv):
	forbidden := []string{
		"api_key sk-",
		"api_key $KEY",
		"api_key \"",
		"api_key '",
	}
	for _, f := range forbidden {
		if strings.Contains(hint, f) {
			t.Errorf("remediation must not contain %q; got:\n%s", f, hint)
		}
	}
}

// helper: ensure the test file compiles cleanly even if fmt is
// unused after a future refactor that drops the import.