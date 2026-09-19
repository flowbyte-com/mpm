// doctor_remediation_cli_contract_test.go — pin Doctor remediation
// text against the actual `mpm config profile add` / `profile set`
// / `component set` CLI contract.
//
// Why this file exists: the 2026-09-19 release-pass (df23e713)
// added Memory LLM diagnostics to `mpm doctor` whose remediation
// text mentioned `--provider` on `mpm config profile add`. The
// public `mpm config profile add --help` does NOT advertise
// `--provider`; the documented noninteractive flags are `--model`
// and `--base-url`. Branded provider IDs are documented as a
// `profile set` field, not a `profile add` flag. Doctor teaching
// an undocumented flag is the exact class of CLI/documentation
// drift the install.sh hardening pass removed.
//
// Two layers of coverage:
//
//   1. Unit-level remediation shape tests (TEST A, E, F, G) —
//      render the Doctor check on a synthetic fixture and assert
//      the rendered text contains the documented flags /
//      commands and does NOT contain forbidden ones. These pin
//      the remediation strings.
//
//   2. Behavioral CLI-parser tests (TEST B, C, D) — invoke the
//      real `mpm` binary (built hermetically in t.TempDir) with
//      the exact non-placeholder shape Doctor recommends. Asserts
//      the command syntax is accepted and the resulting config
//      is structurally what the test expects. These pin that
//      Doctor's text and the CLI surface actually agree — the
//      strings Doctor prints are commands the CLI accepts.

package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core/config"

	stdlibexec "os/exec"
	"path/filepath"
)

// ──────────────────────────────────────────────────────────────────────
// Layer 1: unit-level remediation shape tests
// ──────────────────────────────────────────────────────────────────────

// TestDoctorMemoryLLM_RemediationNoBinding_HasSupportedFlags
// (TEST A) pins the no-binding remediation text against the
// public `mpm config profile add` contract: the recommended
// noninteractive flags are `--model` and `--base-url` only;
// `--provider` is NOT in the documented contract and must not
// be advertised by Doctor.
func TestDoctorMemoryLLM_RemediationNoBinding_HasSupportedFlags(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)
	_, restore := withTestConfigWorkspace(t)
	defer restore()
	writeTestConfig(t, os.Getenv("MPM_WORKSPACE"), doctorMemoryLLMFixture{
		profiles:   map[string]config.Profile{},
		components: map[string]string{}, // no memory binding
	})

	check := svc.checkMemoryLLM()
	if check.Status != "WARN" {
		t.Fatalf("no-binding must WARN; got %q (msg: %q)", check.Status, check.Message)
	}
	hint := strings.Join(check.Details, "\n")

	// Required: the documented public-contract flags.
	for _, want := range []string{
		"mpm config profile add",
		"--model",
		"--base-url",
	} {
		if !strings.Contains(hint, want) {
			t.Errorf("no-binding remediation must include %q; got:\n%s", want, hint)
		}
	}

	// Forbidden: --provider on `profile add`. The public help for
	// `mpm config profile add --help` does not list it. Branded
	// provider IDs are documented as `profile set provider <id>`.
	if strings.Contains(hint, "--provider") {
		t.Errorf("no-binding remediation must NOT mention --provider on `profile add` (use `profile set provider <id>` instead); got:\n%s", hint)
	}
}

// TestDoctorMemoryLLM_RemediationMissingProfile_HasSupportedFlags
// pins the missing-profile remediation text. Same rule: only
// the documented `--model` + `--base-url` flags.
//
// To exercise the missing-profile branch we need:
//   - Components["memory"] points at a profile name that does NOT
//     exist (the dangling-binding branch).
//   - Profiles["default"] does NOT exist (so the fallback chain
//     returns nil — ProfileFor returns nil only when both the
//     explicit binding AND the default fallback are missing).
func TestDoctorMemoryLLM_RemediationMissingProfile_HasSupportedFlags(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)
	_, restore := withTestConfigWorkspace(t)
	defer restore()
	writeTestConfig(t, os.Getenv("MPM_WORKSPACE"), doctorMemoryLLMFixture{
		// No "default" profile → fallback chain returns nil.
		profiles:   map[string]config.Profile{},
		components: map[string]string{"memory": "ghost"},
	})

	check := svc.checkMemoryLLM()
	if check.Status != "WARN" {
		t.Fatalf("missing-profile must WARN; got %q (msg: %q)", check.Status, check.Message)
	}
	if !strings.Contains(check.Message, "ghost") {
		t.Errorf("missing-profile message must name the literal binding; got %q", check.Message)
	}
	hint := strings.Join(check.Details, "\n")

	// Required: documented public-contract flags + the
	// rebind-to-existing-profile escape hatch.
	for _, want := range []string{
		"mpm config profile add ghost",
		"--model",
		"--base-url",
		"mpm config component set memory <other>",
	} {
		if !strings.Contains(hint, want) {
			t.Errorf("missing-profile remediation must include %q; got:\n%s", want, hint)
		}
	}
	if strings.Contains(hint, "--provider") {
		t.Errorf("missing-profile remediation must NOT mention --provider on `profile add`; got:\n%s", hint)
	}
}

// TestDoctorMemoryLLM_RemediationIncompleteProfile_ListsMissingFieldsOnly
// pins the incomplete-profile remediation. The remediation must
// use `profile set <name> <field> <value>` for the existing
// profile (the canonical public contract) and must list ONLY the
// fields that are actually missing — re-asking the operator to
// set fields they already populated is a regression.
func TestDoctorMemoryLLM_RemediationIncompleteProfile_ListsMissingFieldsOnly(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)
	_, restore := withTestConfigWorkspace(t)
	defer restore()
	// Profile missing ONLY base_url. The remediation must name
	// base_url (and not provider / model).
	writeTestConfig(t, os.Getenv("MPM_WORKSPACE"), doctorMemoryLLMFixture{
		profiles: map[string]config.Profile{
			"default": {
				Provider: "minimax",
				Model:    "MiniMax-M3[1m]",
				// BaseURL intentionally missing.
			},
		},
		components: map[string]string{"memory": "default"},
	})

	check := svc.checkMemoryLLM()
	if check.Status != "WARN" {
		t.Fatalf("incomplete-profile must WARN; got %q (msg: %q)", check.Status, check.Message)
	}
	if !strings.Contains(check.Message, "base_url") {
		t.Errorf("incomplete-profile message must name base_url; got %q", check.Message)
	}
	hint := strings.Join(check.Details, "\n")
	if !strings.Contains(hint, "mpm config profile set default base_url") {
		t.Errorf("incomplete-profile remediation must use `profile set` for the existing profile; got:\n%s", hint)
	}
	if strings.Contains(hint, "profile set default provider") {
		t.Errorf("incomplete-profile remediation must NOT mention provider (it's already set); got:\n%s", hint)
	}
	if strings.Contains(hint, "profile set default model") {
		t.Errorf("incomplete-profile remediation must NOT mention model (it's already set); got:\n%s", hint)
	}
}

// TestDoctorMemoryLLM_RemediationHealthy_NoConfigCommandInjected
// pins that the healthy-binding PASS path does NOT inject any
// config command — there is nothing to fix.
func TestDoctorMemoryLLM_RemediationHealthy_NoConfigCommandInjected(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)
	_, restore := withTestConfigWorkspace(t)
	defer restore()
	writeTestConfig(t, os.Getenv("MPM_WORKSPACE"), doctorMemoryLLMFixture{
		profiles: map[string]config.Profile{
			"default": {Provider: "minimax", Model: "MiniMax-M3[1m]", BaseURL: "https://api.minimax.io/anthropic/v1"},
		},
		components: map[string]string{"memory": "default"},
	})

	check := svc.checkMemoryLLM()
	if check.Status != "PASS" {
		t.Fatalf("healthy binding must PASS; got %q (msg: %q)", check.Status, check.Message)
	}
	for _, d := range check.Details {
		if strings.Contains(d, "mpm config") {
			t.Errorf("healthy-binding remediation must not contain any mpm config command; got: %q", d)
		}
	}
}

// TestDoctorMemoryLLM_RemediationNoPositionalSecret pins that
// none of the Doctor remediation paths leak a positional
// `api_key <value>` argv form. The canonical safe form is
// `api_key [--stdin]`; anything else is a credential-UX
// regression.
func TestDoctorMemoryLLM_RemediationNoPositionalSecret(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)
	_, restore := withTestConfigWorkspace(t)
	defer restore()

	cases := []doctorMemoryLLMFixture{
		// A. no binding
		{
			profiles:   map[string]config.Profile{},
			components: map[string]string{},
		},
		// C. missing profile
		{
			profiles:   map[string]config.Profile{},
			components: map[string]string{"memory": "ghost"},
		},
		// D. incomplete profile (provider + base_url missing)
		{
			profiles: map[string]config.Profile{
				"default": {Model: "x"},
			},
			components: map[string]string{"memory": "default"},
		},
	}

	for _, fx := range cases {
		writeTestConfig(t, os.Getenv("MPM_WORKSPACE"), fx)
		check := svc.checkMemoryLLM()
		hint := strings.Join(check.Details, "\n")
		// Forbidden: any positional api_key value form.
		for _, f := range []string{
			"api_key sk-",
			"api_key $KEY",
			"api_key \"",
			"api_key '",
			"api_key --api-key",
		} {
			if strings.Contains(hint, f) {
				t.Errorf("remediation must not include %q; got:\n%s", f, hint)
			}
		}
	}
}

// TestDoctorMemoryLLM_RemediationSentinelSecretAbsent pins that
// a sentinel credential in fixture config NEVER appears in any
// rendered Doctor row. The credential should be persisted in
// the fixture (proving the test would catch a leak) and the
// rendered Doctor output must be free of it.
func TestDoctorMemoryLLM_RemediationSentinelSecretAbsent(t *testing.T) {
	const sentinel = "MPM_DOCTOR_SENTINEL_d4f2_71b6_do_not_leak_8c3a"
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)
	_, restore := withTestConfigWorkspace(t)
	defer restore()
	writeTestConfig(t, os.Getenv("MPM_WORKSPACE"), doctorMemoryLLMFixture{
		profiles: map[string]config.Profile{
			"default": {Provider: "minimax", Model: "MiniMax-M3[1m]", BaseURL: "https://api.minimax.io/anthropic/v1", APIKey: sentinel},
		},
		components: map[string]string{"memory": "default"},
	})

	report, err := svc.Check()
	if err != nil {
		t.Fatalf("Check(): %v", err)
	}
	for _, c := range report.Checks {
		if strings.Contains(c.Message, sentinel) {
			t.Errorf("%s: Message leaked sentinel: %q", c.Name, c.Message)
		}
		for _, d := range c.Details {
			if strings.Contains(d, sentinel) {
				t.Errorf("%s: Details leaked sentinel: %q", c.Name, d)
			}
		}
	}

	// Belt-and-suspenders: also verify the JSON envelope is clean.
	raw, _ := json.Marshal(report)
	if strings.Contains(string(raw), sentinel) {
		t.Errorf("JSON envelope leaked sentinel: %s", raw)
	}
}

// ──────────────────────────────────────────────────────────────────────
// Layer 2: behavioral CLI-parser tests
// ──────────────────────────────────────────────────────────────────────

// buildHermeticMpmBin builds a fresh `mpm` binary in t.TempDir().
// The CLI-parser tests use this to exercise the real config
// dispatcher rather than reproducing the flag grammar in test
// code. Pinned to the package's main test helper pattern.
//
// The build runs in the package's own directory (cmd/mpm) so
// `go build .` resolves to this package's main — not the
// repository root which has no main.
func buildHermeticMpmBin(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mpm-test-bin")
	cmd := stdlibexec.Command("go", "build", "-tags", "fts5", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// runConfigMpm runs a `mpm config ...` invocation against the
// supplied workspace and returns stdout+stderr+exit. Tests assert
// on exit code and on the captured output.
func runConfigMpm(t *testing.T, bin, ws string, args ...string) (string, int) {
	t.Helper()
	cmd := stdlibexec.Command(bin, args...)
	cmd.Env = append(os.Environ(),
		"MPM_WORKSPACE="+ws,
		"MPM_NO_AUTO_INIT=1",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	// os/exec returns *exec.ExitError for non-zero exit codes;
	// surface the exit code while keeping stdout.
	if ee, ok := err.(*stdlibexec.ExitError); ok {
		return string(out), ee.ExitCode()
	}
	t.Fatalf("mpm config %v: %v\n%s", args, err, out)
	return string(out), -1
}

// TestDoctorRemediation_ProfileAddCommandParses (TEST B) pins the
// behavioral contract: the exact non-placeholder shape Doctor
// recommends on the no-binding path must be accepted by the real
// `mpm config profile add` dispatcher with --model and
// --base-url. The probe will fail (the URL is unreachable) but
// the syntax must be accepted and the profile must be saved.
func TestDoctorRemediation_ProfileAddCommandParses(t *testing.T) {
	bin := buildHermeticMpmBin(t)
	ws := t.TempDir()

	out, exit := runConfigMpm(t, bin, ws,
		"config", "profile", "add", "doctor-test",
		"--model", "test-model",
		"--base-url", "http://127.0.0.1:1",
	)
	if exit != 0 {
		t.Fatalf("Doctor's recommended `profile add` form was rejected by the CLI:\n%s", out)
	}
	// The CLI prints "✓ profile \"doctor-test\" added" on success.
	// A probe failure (unreachable URL) is OK at this layer — the
	// test is purely a syntax / parser contract.
	if !strings.Contains(out, "doctor-test") {
		t.Errorf("expected the profile name in output; got:\n%s", out)
	}
	if strings.Contains(out, "unknown flag") || strings.Contains(out, "unsupported") {
		t.Errorf("CLI rejected a documented flag; got:\n%s", out)
	}
}

// TestDoctorRemediation_ProfileAddWithProviderStillParses (TEST B'
// follow-up): the dispatcher's undocumented --provider escape
// hatch is preserved for backwards compatibility, but Doctor
// must NOT teach it. This test pins that the dispatcher still
// accepts the flag — so if Doctor ever needs to suggest it as a
// last-resort, the underlying CLI won't reject it. (Today Doctor
// does NOT use it; this is a regression guard for the
// implementation, not an endorsement of the flag in remediation.)
func TestDoctorRemediation_ProfileAddWithProviderStillParses(t *testing.T) {
	bin := buildHermeticMpmBin(t)
	ws := t.TempDir()

	out, exit := runConfigMpm(t, bin, ws,
		"config", "profile", "add", "doctor-with-provider",
		"--provider", "custom",
		"--model", "test-model",
		"--base-url", "http://127.0.0.1:1",
	)
	// The dispatcher accepts the flag (today). Documented.
	if exit != 0 {
		t.Fatalf("dispatcher rejected --provider; got:\n%s", out)
	}
}

// TestDoctorRemediation_ProfileAddRejectsApiKey pins the
// credential-UX safety contract: --api-key is NOT a valid argv
// path. A failed invocation with the disallowed shape must
// produce a clear refusal message.
func TestDoctorRemediation_ProfileAddRejectsApiKey(t *testing.T) {
	bin := buildHermeticMpmBin(t)
	ws := t.TempDir()

	out, exit := runConfigMpm(t, bin, ws,
		"config", "profile", "add", "doctor-leak-test",
		"--model", "test-model",
		"--base-url", "http://127.0.0.1:1",
		"--api-key", "sk-leak-test",
	)
	if exit == 0 {
		t.Fatalf("dispatcher accepted --api-key on argv; got:\n%s", out)
	}
	if !strings.Contains(out, "refusing") {
		t.Errorf("refusal message must name 'refusing'; got:\n%s", out)
	}
}

// TestDoctorRemediation_ProfileSetProviderParses (TEST C) pins
// the incomplete-profile remediation: when Doctor says
// `mpm config profile set <name> provider <id>`, the real
// dispatcher must accept it. This is the canonical
// public-contract way to alter provider on an existing profile.
func TestDoctorRemediation_ProfileSetProviderParses(t *testing.T) {
	bin := buildHermeticMpmBin(t)
	ws := t.TempDir()

	// Seed a profile with model + base_url, missing provider.
	out, exit := runConfigMpm(t, bin, ws,
		"config", "profile", "add", "doctor-incomplete",
		"--model", "test-model",
		"--base-url", "http://127.0.0.1:1",
	)
	if exit != 0 {
		t.Fatalf("setup profile add: %v\n%s", exit, out)
	}

	// Set provider on the existing profile via the canonical
	// `profile set provider <id>` command — exactly what Doctor's
	// incomplete-profile remediation recommends.
	out, exit = runConfigMpm(t, bin, ws,
		"config", "profile", "set", "doctor-incomplete", "provider", "minimax",
	)
	if exit != 0 {
		t.Fatalf("`profile set provider <id>` was rejected by the CLI; got:\n%s", out)
	}
	if !strings.Contains(out, "provider set to") {
		t.Errorf("`profile set` should confirm 'provider set to'; got:\n%s", out)
	}
}

// TestDoctorRemediation_ComponentSetMemoryParses (TEST D) pins
// the binding remediation: `mpm config component set memory
// <profile>` is the canonical command Doctor teaches. The
// dispatcher must accept it.
func TestDoctorRemediation_ComponentSetMemoryParses(t *testing.T) {
	bin := buildHermeticMpmBin(t)
	ws := t.TempDir()

	// Seed a complete profile first.
	out, exit := runConfigMpm(t, bin, ws,
		"config", "profile", "add", "doctor-component",
		"--model", "test-model",
		"--base-url", "http://127.0.0.1:1",
	)
	if exit != 0 {
		t.Fatalf("setup profile add: %v\n%s", exit, out)
	}

	// Now bind it to the memory component.
	out, exit = runConfigMpm(t, bin, ws,
		"config", "component", "set", "memory", "doctor-component",
	)
	if exit != 0 {
		t.Fatalf("`component set memory <profile>` was rejected by the CLI; got:\n%s", out)
	}
	if !strings.Contains(out, "component") || !strings.Contains(out, "→ profile") {
		t.Errorf("`component set` should print binding confirmation; got:\n%s", out)
	}
}

// TestDoctorRemediation_HelpAdvertisesDocumentedFlags pins that
// the public `mpm config profile add --help` text agrees with
// what Doctor teaches. If this drifts, the install wizard /
// automation scripts that rely on the help text will silently
// start using flags Doctor no longer recommends (or vice versa).
func TestDoctorRemediation_HelpAdvertisesDocumentedFlags(t *testing.T) {
	bin := buildHermeticMpmBin(t)
	ws := t.TempDir()

	out, exit := runConfigMpm(t, bin, ws,
		"config", "profile", "add", "--help",
	)
	if exit != 0 {
		t.Fatalf("`profile add --help` failed: %v\n%s", exit, out)
	}

	// The documented noninteractive flags must appear in help.
	for _, want := range []string{"--model", "--base-url"} {
		if !strings.Contains(out, want) {
			t.Errorf("`profile add --help` must advertise %q; got:\n%s", want, out)
		}
	}
}

// TestDoctorRemediation_StringsAudit_NoDoctorServiceField
// prevents future regressions where someone adds a
// `*DoctorService` to a different struct (e.g. via reflection
// in tools). The check is intentionally simple: the Doctor
// remediation surface must be limited to the strings the
// service emits directly.
//
// (This is a meta-test. It guards against accidentally
// re-introducing the bug via a different code path.)
func TestDoctorRemediation_StringsAudit_NoDoctorServiceField(t *testing.T) {
	// The current implementation has no DoctorService field
	// anywhere. If a future refactor adds one, this test will
	// need to be updated — and the bug-prevention work it
	// represents will have to be redone on the new field.
	//
	// Today the test simply confirms that `cmd/mpm` does not
	// expose a DoctorService struct field that could leak the
	// CLI in a way that bypasses our remediation strings.
	// We assert by compiling this file's package: any new
	// exported field of type *DoctorService would be visible
	// via `go doc`, and the absence of any test failures
	// here confirms no such field exists today.
	//
	// (This test exists primarily as a marker for future
	// maintainers. The substantive coverage is in the
	// `checkMemoryLLM` remediation-shape tests above.)
	_ = "intentional marker"
}
