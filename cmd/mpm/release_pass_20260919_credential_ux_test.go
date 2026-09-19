// release_pass_20260919_credential_ux_test.go — behavioural
// security tests for the 2026-09-19 credential-UX hardening pass.
//
// Two complementary defects are pinned here:
//
//  1. `mpm config profile add --model ... --base-url ...` MUST be
//     truly noninteractive (no wizard, no prompt for protocol/
//     model/base_url/api_key). Prior to this pass the path was
//     gated on `!isatty(os.Stdin)` which dropped supplied flags on
//     a real terminal — the precise defect observed on profile `x`.
//
//  2. `mpm config profile set <name> api_key <secret>` MUST be
//     rejected before any write. The replacement contract is:
//
//     a. `mpm config profile set <name> api_key`
//     prompt with terminal echo suppressed (TTY only;
//     refuses on non-TTY with --stdin guidance).
//
//     b. `mpm config profile set <name> api_key --stdin`
//     read from stdin (no TTY required).
//
//     The success output for both paths prints `updated` with no
//     value interpolation. The config file mode stays 0600.
//
// Each test uses a unique sentinel so leakage can be searched for
// precisely. Run with the canonical FTS5 flags:
//
//	CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1 go test -tags fts5 -run TestCred ./cmd/mpm
package main

import (
	"fmt"
	"os"
	stdlibexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// sentinelSecret is a high-entropy marker used across the credential
// tests. A leak can be searched for in stdout/stderr/log/config by
// substring match. Test failures will print this value once for
// diagnostic clarity; do not echo it from production code.
const sentinelSecret = "MPM_CRED_SENTINEL_7f39_8a2c_d3e1"

// profileAddViaFlags drives the canonical noninteractive `profile add`
// invocation and returns the captured stdout/stderr + exit code.
func profileAddViaFlags(t *testing.T, bin, ws string) (string, error) {
	t.Helper()
	cmd := stdlibexec.Command(bin,
		"config", "profile", "add", "permission-test",
		"--model", "test-model",
		"--base-url", "http://127.0.0.1:1")
	cmd.Env = clearEmbeddingEnv(ws)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// setAPIKeyViaStdin pipes the secret via stdin and runs the
// --stdin form. Returns the captured output and exit error.
func setAPIKeyViaStdin(t *testing.T, bin, ws, profileName, secret string) (string, error) {
	t.Helper()
	cmd := stdlibexec.Command(bin,
		"config", "profile", "set", profileName, "api_key", "--stdin")
	cmd.Env = clearEmbeddingEnv(ws)
	cmd.Stdin = strings.NewReader(secret + "\n")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// configFileMode returns the octal perm of mpm_config.json under ws
// as a string ("0600", "0644", ...). Empty string if the file is
// missing. The returned value is just the perm bits — `os.Stat`
// reports a full FileInfo string ("-rw-------") that confuses the
// perm-only assertion.
func configFileMode(t *testing.T, ws string) string {
	t.Helper()
	path := filepath.Join(ws, "mpm_config.json")
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%04o", info.Mode().Perm())
}

// readConfigFile returns the contents of mpm_config.json under ws.
func readConfigFile(t *testing.T, ws string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(ws, "mpm_config.json"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	return string(data)
}

// profileInConfig returns the JSON object string for a named
// profile, or "" if the profile is absent. Used by tests that
// need to assert material-field writes succeeded.
func profileInConfig(t *testing.T, ws, name string) string {
	t.Helper()
	profiles := readProfilesBlock(t, ws)
	return profiles[name]
}

// A. Flag-driven profile add is noninteractive.
//
// Pin: supplying --model and --base-url must save a complete profile
// WITHOUT prompting the operator for protocol/model/base_url/api_key.
// Prior to the 2026-09-19 hardening pass this branch was gated on
// `!isatty(os.Stdin)` — it would invoke the wizard on a real terminal
// and ignore supplied flags entirely.
func TestCred_A_ProfileAddFlagsAreNoninteractive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix-only tty semantics; skip on windows")
	}
	bin := buildRunawayBin(t)
	ws := t.TempDir()

	out, err := profileAddViaFlags(t, bin, ws)
	if err != nil {
		t.Fatalf("profile add --model --base-url: %v\nout=%s", err, out)
	}

	// No wizard prompts may appear.
	for _, want := range []string{
		"Choose [",        // protocol picker
		"Custom lets you", // wizard intro
	} {
		if strings.Contains(out, want) {
			t.Errorf("unexpected wizard prompt in flag-driven add: %q\nout=%s", want, out)
		}
	}

	// Profile must be saved with the supplied fields.
	body := readConfigFile(t, ws)
	if !strings.Contains(body, `"name": "permission-test"`) {
		t.Fatalf("profile not saved; body=%s", body)
	}
	if !strings.Contains(body, `"model": "test-model"`) {
		t.Errorf("model not saved; body=%s", body)
	}
	if !strings.Contains(body, `"base_url": "http://127.0.0.1:1"`) {
		t.Errorf("base_url not saved; body=%s", body)
	}
	// Default provider = "custom" (matches the wizard's first-option
	// default per 2026-09-19 hardening pass).
	if !strings.Contains(body, `"provider": "custom"`) {
		t.Errorf("provider default 'custom' missing; body=%s", body)
	}
	// No api_key was supplied; must remain unset. JSON's encoding
	// of an empty string is still present in the struct, but the
	// value MUST be "" — check no accidental write of a key.
	if strings.Contains(body, `"api_key":`) && !strings.Contains(body, `"api_key": ""`) {
		t.Errorf("api_key leaked from noninteractive add: %s", body)
	}

	// Config file mode is preserved by the existing 0077 umask + 0600
	// SaveConfig contract — pin it here.
	if mode := configFileMode(t, ws); mode != "0600" {
		t.Errorf("config mode = %s, want 0600", mode)
	}
}

// B. Positional API key is REJECTED before any write.
//
// Pin: `mpm config profile set NAME api_key SECRET` must:
//
//   - exit non-zero;
//   - print an actionable error explaining the hidden prompt / --stdin;
//   - NEVER interpolate SECRET into the error text;
//   - NOT write SECRET to the config file;
//   - NOT include SECRET in stdout or stderr.
func TestCred_B_PositionalAPIKeyRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix-only; skip on windows")
	}
	bin := buildRunawayBin(t)
	ws := t.TempDir()

	// Seed an existing profile so the rejected call would otherwise
	// have a valid target.
	profileAddViaFlags(t, bin, ws)

	cmd := stdlibexec.Command(bin,
		"config", "profile", "set", "permission-test", "api_key", sentinelSecret)
	cmd.Env = clearEmbeddingEnv(ws)
	out, err := cmd.CombinedOutput()
	sout := string(out)

	if err == nil {
		t.Fatalf("positional api_key accepted; out=%s", sout)
	}

	// Output must NOT contain the secret under any framing.
	if strings.Contains(sout, sentinelSecret) {
		t.Fatalf("rejection output leaked the secret:\n%s", sout)
	}

	// Output must be actionable — point to --stdin or hidden prompt.
	lc := strings.ToLower(sout)
	if !strings.Contains(lc, "stdin") && !strings.Contains(lc, "hidden") {
		t.Errorf("rejection message lacks actionable guidance; got:\n%s", sout)
	}

	// Config must NOT contain the secret.
	body := readConfigFile(t, ws)
	if strings.Contains(body, sentinelSecret) {
		t.Errorf("config file gained the rejected secret:\n%s", body)
	}
}

// C. --stdin secret update is accepted and the secret is stored.
//
// Pin: `mpm config profile set NAME api_key --stdin` MUST:
//
//   - read the secret from stdin (no TTY required);
//   - persist the value to the config file;
//   - exit 0;
//   - NOT include the secret in stdout or stderr;
//   - print a `set` / `updated` success line with NO value.
func TestCred_C_StdinAPIKeyUpdate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix-only; skip on windows")
	}
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	profileAddViaFlags(t, bin, ws)

	out, err := setAPIKeyViaStdin(t, bin, ws, "permission-test", sentinelSecret)
	if err != nil {
		t.Fatalf("set api_key --stdin: %v\nout=%s", err, out)
	}
	if strings.Contains(out, sentinelSecret) {
		t.Fatalf("stdout/stderr leaked secret:\n%s", out)
	}

	// The config file MUST contain the secret after a successful
	// --stdin save — that's the canonical storage path.
	body := readConfigFile(t, ws)
	if !strings.Contains(body, sentinelSecret) {
		t.Fatalf("config does not contain the secret after --stdin save:\n%s", body)
	}

	// File mode preserved.
	if mode := configFileMode(t, ws); mode != "0600" {
		t.Errorf("config mode = %s, want 0600", mode)
	}
}

// D. Stdin secret rewrite replaces the previous value.
//
// Pin: piping a second secret via --stdin replaces the first;
// neither old nor new appears in stdout/stderr.
func TestCred_D_StdinSecretRewrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix-only; skip on windows")
	}
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	profileAddViaFlags(t, bin, ws)

	oldSecret := sentinelSecret + "_v1"
	newSecret := sentinelSecret + "_v2"

	if _, err := setAPIKeyViaStdin(t, bin, ws, "permission-test", oldSecret); err != nil {
		t.Fatalf("first --stdin set: %v", err)
	}
	out, err := setAPIKeyViaStdin(t, bin, ws, "permission-test", newSecret)
	if err != nil {
		t.Fatalf("second --stdin set: %v\nout=%s", err, out)
	}
	if strings.Contains(out, oldSecret) || strings.Contains(out, newSecret) {
		t.Errorf("success output leaked secret: %q or %q in\n%s",
			oldSecret, newSecret, out)
	}

	body := readConfigFile(t, ws)
	if !strings.Contains(body, newSecret) {
		t.Errorf("config missing new secret:\n%s", body)
	}
	if strings.Contains(body, oldSecret) {
		t.Errorf("config still contains old secret:\n%s", body)
	}

	// File mode preserved.
	if mode := configFileMode(t, ws); mode != "0600" {
		t.Errorf("config mode = %s, want 0600", mode)
	}
}

// E. Non-sensitive fields still work via positional value.
//
// Pin: `mpm config profile set NAME model VALUE` and similar
// non-secret fields continue to work via positional value. Only the
// api_key / token / apikey field is special-cased.
func TestCred_E_NonSecretFieldsKeepPositionalSyntax(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix-only; skip on windows")
	}
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	profileAddViaFlags(t, bin, ws)

	// Set a new model via positional value.
	out, err := runMpmCommand(t, bin, ws,
		"config", "profile", "set", "permission-test", "model", "new-model")
	if err != nil {
		t.Fatalf("set model: %v\nout=%s", err, out)
	}
	body := readConfigFile(t, ws)
	if !strings.Contains(body, `"model": "new-model"`) {
		t.Errorf("model not updated; body=%s", body)
	}

	// Set a new base_url via positional value.
	if _, err := runMpmCommand(t, bin, ws,
		"config", "profile", "set", "permission-test",
		"base_url", "http://127.0.0.1:9999"); err != nil {
		t.Fatalf("set base_url: %v", err)
	}
	body = readConfigFile(t, ws)
	if !strings.Contains(body, `"base_url": "http://127.0.0.1:9999"`) {
		t.Errorf("base_url not updated; body=%s", body)
	}

	// Set a temperature via positional value (sanity).
	if _, err := runMpmCommand(t, bin, ws,
		"config", "profile", "set", "permission-test",
		"temperature", "0.7"); err != nil {
		t.Fatalf("set temperature: %v", err)
	}
	body = readConfigFile(t, ws)
	if !strings.Contains(body, `"temperature": 0.7`) {
		t.Errorf("temperature not updated; body=%s", body)
	}
}

// F. Non-TTY without --stdin must fail promptly (no hang).
//
// Pin: `mpm config profile set NAME api_key` without a TTY and
// without --stdin must reject immediately with an actionable
// message, NOT hang waiting for an invisible prompt.
func TestCred_F_NonTTYRefusesWithoutStdin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix-only; skip on windows")
	}
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	profileAddViaFlags(t, bin, ws)

	// Pipe /dev/null so stdin is not a TTY and EOF is immediate.
	cmd := stdlibexec.Command(bin,
		"config", "profile", "set", "permission-test", "api_key")
	cmd.Env = clearEmbeddingEnv(ws)
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}
	cmd.Stdin = devNull
	out, err := cmd.CombinedOutput()
	sout := string(out)

	if err == nil {
		t.Fatalf("non-TTY secret prompt succeeded; expected refusal; out=%s", sout)
	}
	if strings.Contains(sout, sentinelSecret) {
		t.Errorf("output leaked a placeholder secret:\n%s", sout)
	}
	lc := strings.ToLower(sout)
	if !strings.Contains(lc, "stdin") {
		t.Errorf("non-TTY error lacks --stdin guidance; got:\n%s", sout)
	}

	// The refusal must not have written a key.
	body := readConfigFile(t, ws)
	if strings.Contains(body, sentinelSecret) {
		t.Errorf("non-TTY refusal wrote api_key:\n%s", body)
	}
	if strings.Contains(body, `"api_key": "`) && !strings.Contains(body, `"api_key": ""`) {
		t.Errorf("non-TTY refusal wrote a non-empty api_key; body=%s", body)
	}
}

// G. --api-key flag on `profile add` is REJECTED.
//
// Pin: passing --api-key on `profile add` (the legacy argv-leak
// path) is refused before any write. The dispatcher above rejects
// the flag outright and emits an actionable message.
func TestCred_G_ProfileAddRejectsAPIKeyFlag(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix-only; skip on windows")
	}
	bin := buildRunawayBin(t)
	ws := t.TempDir()

	cmd := stdlibexec.Command(bin,
		"config", "profile", "add", "permission-test",
		"--model", "test-model",
		"--base-url", "http://127.0.0.1:1",
		"--api-key", sentinelSecret)
	cmd.Env = clearEmbeddingEnv(ws)
	out, err := cmd.CombinedOutput()
	sout := string(out)
	if err == nil {
		t.Fatalf("--api-key on profile add was accepted; out=%s", sout)
	}
	if strings.Contains(sout, sentinelSecret) {
		t.Fatalf("rejection leaked the supplied --api-key value:\n%s", sout)
	}
	lc := strings.ToLower(sout)
	if !strings.Contains(lc, "stdin") && !strings.Contains(lc, "hidden") {
		t.Errorf("--api-key refusal lacks actionable guidance; got:\n%s", sout)
	}
	// Profile may or may not have been written — the secret must
	// definitely NOT be present. readConfigFile fatals if missing;
	// tolerate absence since the rejection is a no-write path.
	if body, err := os.ReadFile(filepath.Join(ws, "mpm_config.json")); err == nil {
		if strings.Contains(string(body), sentinelSecret) {
			t.Errorf("config contains the rejected --api-key:\n%s", body)
		}
	}
}

// H. Success output for api_key omits the value.
//
// Pin: the success line emitted after a successful api_key update
// must NOT contain the secret value. The new contract is:
//
//	✓ profile "<name>" api_key updated
//
// NOT:
//
//	✓ profile "<name>" api_key set to "<value>"
func TestCred_H_SuccessLineOmitsSecretValue(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix-only; skip on windows")
	}
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	profileAddViaFlags(t, bin, ws)

	out, err := setAPIKeyViaStdin(t, bin, ws, "permission-test", sentinelSecret)
	if err != nil {
		t.Fatalf("set api_key --stdin: %v\nout=%s", err, out)
	}
	if strings.Contains(out, sentinelSecret) {
		t.Fatalf("success line echoed the secret:\n%s", out)
	}
	if !strings.Contains(out, "updated") {
		t.Errorf("expected `updated` success line; got:\n%s", out)
	}
	if !strings.Contains(out, "permission-test") {
		t.Errorf("success line missing profile name; got:\n%s", out)
	}
	if strings.Contains(out, "set to") {
		t.Errorf("success line used legacy `set to` form for secret:\n%s", out)
	}
}

// I. Legacy `mpm config set api_key SECRET` is REJECTED.
//
// Pin: the legacy top-level `mpm config set api_key SECRET` form
// is also closed, since it carries the same argv-leak class. The
// scriptable alias is `mpm config profile set default api_key
// --stdin`.
func TestCred_I_LegacyConfigSetAPIKeyRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix-only; skip on windows")
	}
	bin := buildRunawayBin(t)
	ws := t.TempDir()

	cmd := stdlibexec.Command(bin,
		"config", "set", "api_key", sentinelSecret)
	cmd.Env = clearEmbeddingEnv(ws)
	out, err := cmd.CombinedOutput()
	sout := string(out)
	if err == nil {
		t.Fatalf("legacy `mpm config set api_key` accepted secret; out=%s", sout)
	}
	if strings.Contains(sout, sentinelSecret) {
		t.Errorf("legacy rejection leaked the secret:\n%s", sout)
	}
}

// J. promptSecret refuses on non-TTY; resolveSecretFieldValue
// rejects any positional secret argument.
//
// Pin: the underlying helpers behave as documented. promptSecret
// refuses on non-TTY with an actionable message. resolveSecretFieldValue
// rejects any non-empty positional argument and returns exit code 1.
func TestCred_J_SecretHelpersNonTTY(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix-only; skip on windows")
	}
	// Stdin is the test runner's stdin — not a TTY here.
	_, err := promptSecret("API key", "")
	if err == nil {
		t.Errorf("promptSecret must refuse on non-TTY stdin")
	}

	// resolveSecretFieldValue rejects any positional secret argument.
	_, code := resolveSecretFieldValue([]string{sentinelSecret})
	if code != 1 {
		t.Errorf("resolveSecretFieldValue exit code = %d, want 1", code)
	}
}
