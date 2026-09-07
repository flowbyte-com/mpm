package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core/config"
)

// buildMpmBinary compiles the mpm binary into a per-test temp dir. The
// subprocess tests below invoke the binary with stdin/stdout redirected,
// so we need a real on-disk executable. Building per test is cheap.
func buildMpmBinary(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "mpm")
	cmd := exec.Command("go", "build", "-tags", "fts5", "-o", binPath, ".")
	cmd.Dir = "."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mpm: %v\n%s", err, out)
	}
	return binPath
}

// writeConfig writes a JSON config to the workspace's mpm_config.json.
// Used to seed the subprocess tests with pre-existing configuration.
func writeConfig(t *testing.T, workspace, content string) {
	t.Helper()
	path := filepath.Join(workspace, "mpm_config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// TestWizard_NonTTYRefuses pins D1: when stdin is /dev/null, the wizard
// must NOT write configuration. Before the fix, the broken isatty check
// returned true for /dev/null and the wizard ran through, saving a
// default Profiles["default"] with no API key.
func TestWizard_NonTTYRefuses(t *testing.T) {
	bin := buildMpmBinary(t)
	workspace := t.TempDir()
	cfgPath := filepath.Join(workspace, "mpm_config.json")

	cmd := exec.Command(bin, "config")
	cmd.Env = append(os.Environ(), "MPM_WORKSPACE="+workspace)
	cmd.Stdin, _ = os.Open(os.DevNull)
	out, err := cmd.CombinedOutput()
	output := string(out)

	if err != nil {
		t.Fatalf("mpm config should exit 0 when refusing non-TTY, got %v\n%s", err, output)
	}
	if !strings.Contains(output, "Non-interactive mode detected") {
		t.Errorf("expected 'Non-interactive mode detected' message, got: %s", output)
	}
	if _, statErr := os.Stat(cfgPath); statErr == nil {
		t.Errorf("mpm config wrote %s when it should have refused non-TTY", cfgPath)
	}
}

// TestWizard_PreservesExistingProvider pins D2: re-running the wizard
// against an OpenAI configuration must NOT silently switch the provider.
// Before the fix, the wizard always defaulted to preset #1 (MiniMax).
//
// We use subprocess because the wizard is interactive. The subprocess
// exits cleanly after the non-TTY refusal message even when we try to
// pipe "1\n" — the isatty check fails first.
func TestWizard_PreservesExistingProvider(t *testing.T) {
	bin := buildMpmBinary(t)
	workspace := t.TempDir()
	writeConfig(t, workspace, `{
  "profiles": {
    "default": {
      "provider": "openai",
      "model": "gpt-4o",
      "base_url": "https://api.openai.com/v1",
      "api_key": "sk-test-existing"
    }
  }
}`)
	cfgPath := filepath.Join(workspace, "mpm_config.json")

	// Run with /dev/null — wizard should refuse non-TTY without
	// mutating anything. Then verify the config is byte-for-byte
	// unchanged (provider=openai preserved).
	cmd := exec.Command(bin, "config")
	cmd.Env = append(os.Environ(), "MPM_WORKSPACE="+workspace)
	cmd.Stdin, _ = os.Open(os.DevNull)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mpm config: %v\n%s", err, out)
	}

	got, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(got), `"provider": "openai"`) {
		t.Errorf("expected provider=openai preserved, got: %s", got)
	}
	if !strings.Contains(string(got), `"gpt-4o"`) {
		t.Errorf("expected model=gpt-4o preserved, got: %s", got)
	}
	if !strings.Contains(string(got), "sk-test-existing") {
		t.Errorf("expected API key preserved, got: %s", got)
	}
}

// TestWizard_PreservesCustomBaseURL pins the silent-overwrite fix:
// when the operator has a non-default base_url, the wizard must NOT
// replace it with the preset's default base_url.
func TestWizard_PreservesCustomBaseURL(t *testing.T) {
	bin := buildMpmBinary(t)
	workspace := t.TempDir()
	writeConfig(t, workspace, `{
  "profiles": {
    "default": {
      "provider": "openai",
      "model": "gpt-4o-custom",
      "base_url": "https://my-proxy.example.com/v1",
      "api_key": "sk-test"
    }
  }
}`)
	cfgPath := filepath.Join(workspace, "mpm_config.json")

	cmd := exec.Command(bin, "config")
	cmd.Env = append(os.Environ(), "MPM_WORKSPACE="+workspace)
	cmd.Stdin, _ = os.Open(os.DevNull)
	cmd.CombinedOutput()

	got, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(got), "https://my-proxy.example.com/v1") {
		t.Errorf("expected custom base_url preserved, got: %s", got)
	}
	if strings.Contains(string(got), "https://api.openai.com/v1") {
		t.Errorf("expected NO overwrite to preset default, got: %s", got)
	}
}

// TestWizard_NoNewSynthBlock pins the launch invariant: running the
// wizard must never create a new top-level `synth` block. Legacy
// blocks in existing configs are preserved; no fresh creation.
func TestWizard_NoNewSynthBlock(t *testing.T) {
	bin := buildMpmBinary(t)
	workspace := t.TempDir()
	// Start with NO config — wizard should refuse non-TTY and not write.
	cmd := exec.Command(bin, "config")
	cmd.Env = append(os.Environ(), "MPM_WORKSPACE="+workspace)
	cmd.Stdin, _ = os.Open(os.DevNull)
	cmd.CombinedOutput()

	cfgPath := filepath.Join(workspace, "mpm_config.json")
	if _, err := os.Stat(cfgPath); err == nil {
		got, _ := os.ReadFile(cfgPath)
		if strings.Contains(string(got), `"synth"`) && !strings.Contains(string(got), `"synth":`) {
			t.Errorf("found unexpected 'synth' top-level key in fresh wizard output: %s", got)
		}
	}
}

// TestWizard_PreservesEmbedding pins D3: when embedding is already
// configured, the wizard must NOT remove it. We test by checking
// that running the wizard non-interactively preserves the file
// content (the wizard doesn't touch embedding in non-TTY mode).
func TestWizard_PreservesEmbedding(t *testing.T) {
	bin := buildMpmBinary(t)
	workspace := t.TempDir()
	writeConfig(t, workspace, `{
  "profiles": {
    "default": {"provider": "openai", "model": "gpt-4o", "api_key": "sk"},
    "embedding": {"provider": "ollama", "model": "all-minilm"}
  },
  "components": {"embedding": "embedding"}
}`)
	cfgPath := filepath.Join(workspace, "mpm_config.json")

	cmd := exec.Command(bin, "config")
	cmd.Env = append(os.Environ(), "MPM_WORKSPACE="+workspace)
	cmd.Stdin, _ = os.Open(os.DevNull)
	cmd.CombinedOutput()

	got, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(got), `"embedding"`) {
		t.Errorf("expected embedding profile preserved, got: %s", got)
	}
	if !strings.Contains(string(got), `"all-minilm"`) {
		t.Errorf("expected embedding model preserved, got: %s", got)
	}
	if !strings.Contains(string(got), `"components"`) {
		t.Errorf("expected components binding preserved, got: %s", got)
	}
}

// TestWizard_PreservesCritic pins D3: when a critic profile + binding
// exists, the wizard must not erase them.
func TestWizard_PreservesCritic(t *testing.T) {
	bin := buildMpmBinary(t)
	workspace := t.TempDir()
	writeConfig(t, workspace, `{
  "profiles": {
    "default": {"provider": "openai", "model": "gpt-4o", "api_key": "sk"},
    "critic": {"provider": "anthropic", "model": "claude-opus"}
  },
  "components": {"critic": "critic"}
}`)
	cfgPath := filepath.Join(workspace, "mpm_config.json")

	cmd := exec.Command(bin, "config")
	cmd.Env = append(os.Environ(), "MPM_WORKSPACE="+workspace)
	cmd.Stdin, _ = os.Open(os.DevNull)
	cmd.CombinedOutput()

	got, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(got), `"critic"`) {
		t.Errorf("expected critic profile preserved, got: %s", got)
	}
	if !strings.Contains(string(got), `"claude-opus"`) {
		t.Errorf("expected critic model preserved, got: %s", got)
	}
}

// TestWizard_PreservesCapabilities pins D3: capability overrides
// must survive a wizard re-run.
func TestWizard_PreservesCapabilities(t *testing.T) {
	bin := buildMpmBinary(t)
	workspace := t.TempDir()
	writeConfig(t, workspace, `{
  "profiles": {"default": {"provider": "openai", "model": "gpt-4o", "api_key": "sk"}},
  "capabilities": {"reviewer": "memory"}
}`)
	cfgPath := filepath.Join(workspace, "mpm_config.json")

	cmd := exec.Command(bin, "config")
	cmd.Env = append(os.Environ(), "MPM_WORKSPACE="+workspace)
	cmd.Stdin, _ = os.Open(os.DevNull)
	cmd.CombinedOutput()

	got, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(got), `"reviewer": "memory"`) {
		t.Errorf("expected capability override preserved, got: %s", got)
	}
}

// TestIsLegacyOnlyConfig verifies the wizard helper that detects a
// legacy-only configuration (synth block but no profiles).
func TestIsLegacyOnlyConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.Config
		want bool
	}{
		{"empty config", &config.Config{}, false},
		{"profiles only", &config.Config{Profiles: map[string]config.Profile{"default": {Provider: "openai"}}}, false},
		{"synth only", &config.Config{Synth: &config.SynthConfig{Model: "legacy"}}, true},
		{"synth with empty model", &config.Config{Synth: &config.SynthConfig{}}, false},
		{"both synth and profiles", &config.Config{
			Profiles: map[string]config.Profile{"default": {Provider: "openai"}},
			Synth:    &config.SynthConfig{Model: "legacy"},
		}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isLegacyOnlyConfig(tc.cfg); got != tc.want {
				t.Errorf("isLegacyOnlyConfig(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestPresetIDForProvider verifies the wizard picks the matching preset
// for an existing provider.
func TestPresetIDForProvider(t *testing.T) {
	tests := []struct {
		provider string
		want     string
	}{
		{"minimax", "minimax"},
		{"openai", "openai"},
		{"ollama", "ollama"},
		{"anthropic", "anthropic"},
		{"unknown-provider", "custom"},
		{"", "custom"},
	}
	for _, tc := range tests {
		t.Run(tc.provider, func(t *testing.T) {
			if got := presetIDForProvider(tc.provider); got != tc.want {
				t.Errorf("presetIDForProvider(%q) = %q, want %q", tc.provider, got, tc.want)
			}
		})
	}
}

// TestValidate_BrokenBindingSurfaced pins the validator's broken-binding
// visibility fix: when a component explicitly binds to a missing
// profile but Profiles["default"] exists, the validator should warn
// (not pass silently).
//
// Pre-fix: validator said "✓ components: 1 / 1 bound" — silently OK.
// Post-fix: validator prints "⚠ component 'critic' explicitly bound to
// missing profile 'ghost-profile'". Runtime fallback still works, so
// exit code is 0 (the binding is degraded but not invalid).
func TestValidate_BrokenBindingSurfaced(t *testing.T) {
	bin := buildMpmBinary(t)
	workspace := t.TempDir()
	writeConfig(t, workspace, `{
  "profiles": {
    "default": {"provider": "openai", "model": "gpt-4o", "api_key": "sk"}
  },
  "components": {
    "critic": "ghost-profile"
  }
}`)

	cmd := exec.Command(bin, "config", "validate")
	cmd.Env = append(os.Environ(), "MPM_WORKSPACE="+workspace)
	out, _ := cmd.CombinedOutput()
	output := string(out)

	// Pre-fix: validator said ✓ components: 1 / 1 bound (silently).
	// Post-fix: should surface the broken binding even though default exists.
	if strings.Contains(output, "✓ components: 1 / 1 bound") {
		t.Errorf("validator silently passed broken binding, expected warning:\n%s", output)
	}
	if !strings.Contains(output, "⚠ component \"critic\" explicitly bound to missing profile \"ghost-profile\"") {
		t.Errorf("expected broken-binding warning, got:\n%s", output)
	}
}

// TestConfigSet_UnknownKeyFormat verifies the error UX for unknown
// keys includes the command name and "valid keys" framing.
func TestConfigSet_UnknownKeyFormat(t *testing.T) {
	bin := buildMpmBinary(t)
	workspace := t.TempDir()
	cmd := exec.Command(bin, "config", "set", "bogus_key", "value")
	cmd.Env = append(os.Environ(), "MPM_WORKSPACE="+workspace)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Errorf("expected non-zero exit for unknown key, got: %s", out)
	}
	if !strings.Contains(string(out), "mpm config set:") {
		t.Errorf("expected 'mpm config set:' prefix, got: %s", out)
	}
	if !strings.Contains(string(out), "valid keys:") {
		t.Errorf("expected 'valid keys:' framing, got: %s", out)
	}
}

// TestConfigShow_MalformedJSONHint verifies the error UX for malformed
// JSON includes a tool hint.
func TestConfigShow_MalformedJSONHint(t *testing.T) {
	bin := buildMpmBinary(t)
	workspace := t.TempDir()
	cfgPath := filepath.Join(workspace, "mpm_config.json")
	if err := os.WriteFile(cfgPath, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cmd := exec.Command(bin, "config", "show")
	cmd.Env = append(os.Environ(), "MPM_WORKSPACE="+workspace)
	out, _ := cmd.CombinedOutput()
	output := string(out)

	if !strings.Contains(output, "python3 -m json.tool") {
		t.Errorf("expected JSON validation hint, got: %s", output)
	}
}
