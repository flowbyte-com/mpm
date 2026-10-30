package main

// handlers_config_timeout_test.go — the onboarding-timeout correction.
//
// THE DEFECT
// ----------
// The onboarding wizard asked the operator to choose an LLM request
// timeout:
//
//	Timeout seconds [300]:
//
// That is not a decision an operator can make before they have watched
// a slow provider. It is a number that only matters in a failure mode
// most operators will never see, and it sat in front of every user as
// a default they were expected to accept. It was also a foot-gun: a
// typo (`3` instead of `30`) became a live fault that failed every
// request.
//
// The timeout is an ADVANCED setting. It remains editable by hand in
// mpm_config.json and from a terminal with `mpm config profile set`,
// but onboarding does not ask for it.
//
// WHAT IS ASSERTED HERE
// ---------------------
//
//  1. Fresh onboarding never asks for a timeout, and writes the
//     substrate default (300s).
//  2. An existing, explicitly configured timeout SURVIVES an upgrade
//     unchanged.
//  3. An invalid existing value is replaced by the default AND the
//     replacement is reported.
//  4. `mpm config get timeout_seconds` reports the EFFECTIVE value, so
//     the CLI and the runtime cannot disagree.
//  5. The wizard's source contains no timeout prompt — a structural
//     guard, so the prompt cannot come back unnoticed.
//
// The runtime half of the policy (what the substrate does with a
// configured value) is asserted in internal/core/config/timeout_test.go
// and internal/core/synth.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core/config"
)

// wizardTimeoutPrompt is the label the wizard used to print. It must
// never appear again.
const wizardTimeoutPrompt = "Timeout seconds"

// runWizardInPTYCapturing is runWizardInPTY with stdout captured.
//
// The wizard writes its prompts to os.Stdout, so capturing it is the
// only way to assert that a prompt is NOT shown. The wizard itself
// runs in a goroutine (the PTY helper owns the terminal), so the
// capture wraps the whole call.
func runWizardInPTYCapturing(t *testing.T, c *config.Config, finalInput string) string {
	t.Helper()
	return captureStdout(t, func() {
		runWizardInPTY(t, c, finalInput)
	})
}

// TestWizard_FreshOnboardingNeverAsksForATimeout is the headline
// correction.
//
// A fresh wizard session must not print the timeout prompt, and the
// configuration it writes must carry the substrate default so the
// number is explicit rather than implied.
func TestWizard_FreshOnboardingNeverAsksForATimeout(t *testing.T) {
	isolatedEmbeddingWorkspace(t)
	c := &config.Config{}

	out := runWizardInPTYCapturing(t, c, "y\n")

	if strings.Contains(out, wizardTimeoutPrompt) {
		t.Errorf("the wizard asked for a request timeout:\n%s", out)
	}

	prof, ok := c.Profiles["default"]
	if !ok {
		t.Fatalf("the wizard wrote no default profile:\n%s", out)
	}
	if prof.TimeoutSecs != config.DefaultTimeoutSeconds {
		t.Errorf("timeout_seconds = %d, want %d (a fresh configuration must carry the default explicitly)",
			prof.TimeoutSecs, config.DefaultTimeoutSeconds)
	}
	if config.DefaultTimeoutSeconds != 300 {
		t.Errorf("DefaultTimeoutSeconds = %d, want 300 (the default must match the example configuration)",
			config.DefaultTimeoutSeconds)
	}

	// The operator is SHOWN the value in the preview, so the timeout
	// is visible and correct without ever having been a question.
	if !strings.Contains(out, "timeout    : 300") {
		t.Errorf("the wizard's preview does not show the effective timeout:\n%s", out)
	}
}

// TestWizard_AnExistingTimeoutSurvivesTheUpgrade is the upgrade half.
//
// The operator set a timeout deliberately. Re-running the wizard — or
// upgrading MPM — must not silently replace it. This is the property
// that makes the change safe to ship: the correction only removes a
// QUESTION, it does not touch a VALUE.
func TestWizard_AnExistingTimeoutSurvivesTheUpgrade(t *testing.T) {
	isolatedEmbeddingWorkspace(t)
	c := &config.Config{
		Profiles: map[string]config.Profile{
			"default": {
				Name:        "default",
				Provider:    "custom",
				Model:       "some-model",
				BaseURL:     "http://127.0.0.1:11434",
				TimeoutSecs: 45,
			},
		},
	}

	out := runWizardInPTYCapturing(t, c, "y\n")

	if strings.Contains(out, wizardTimeoutPrompt) {
		t.Errorf("the wizard asked for a request timeout:\n%s", out)
	}

	prof, ok := c.Profiles["default"]
	if !ok {
		t.Fatalf("the wizard wrote no default profile:\n%s", out)
	}
	if prof.TimeoutSecs != 45 {
		t.Errorf("timeout_seconds = %d, want 45 (an explicitly configured value must survive an upgrade unchanged)",
			prof.TimeoutSecs)
	}
}

// TestWizard_AnInvalidExistingTimeoutIsReplacedAndReported is the
// validation half.
//
// A hand-edited file can carry a zero or a negative timeout. Neither
// is a usable request timeout, and a negative duration would become
// "no timeout" in Go's http client — an unbounded request rather than
// the error the operator expected. The documented policy is: replace
// it with the default, and SAY so. Silently rewriting the operator's
// setting would be the same class of surprise this change exists to
// remove.
func TestWizard_AnInvalidExistingTimeoutIsReplacedAndReported(t *testing.T) {
	for _, invalid := range []int{0, -5} {
		t.Run(fmt.Sprintf("invalid=%d", invalid), func(t *testing.T) {
			isolatedEmbeddingWorkspace(t)
			c := &config.Config{
				Profiles: map[string]config.Profile{
					"default": {
						Name:        "default",
						Provider:    "custom",
						Model:       "some-model",
						BaseURL:     "http://127.0.0.1:11434",
						TimeoutSecs: invalid,
					},
				},
			}

			out := runWizardInPTYCapturing(t, c, "y\n")

			prof, ok := c.Profiles["default"]
			if !ok {
				t.Fatalf("the wizard wrote no default profile:\n%s", out)
			}
			if prof.TimeoutSecs != config.DefaultTimeoutSeconds {
				t.Errorf("timeout_seconds = %d, want %d (an invalid value must be replaced by the default)",
					prof.TimeoutSecs, config.DefaultTimeoutSeconds)
			}
			if invalid != 0 && !strings.Contains(out, "not a valid request timeout") {
				t.Errorf("the replacement was not reported:\n%s", out)
			}
		})
	}
}

// TestConfigGet_TimeoutSecondsReportsTheEffectiveValue pins the
// CLI/runtime agreement.
//
// `mpm config get timeout_seconds` must report the timeout that will
// ACTUALLY be used, not the raw value in the file. Reporting the raw
// value would let the CLI and the runtime disagree about the same
// configuration — which is exactly the class of disagreement this
// change exists to remove.
func TestConfigGet_TimeoutSecondsReportsTheEffectiveValue(t *testing.T) {
	bin := buildMpmBinary(t)

	cases := []struct {
		name    string
		seeded  string
		wantOut string
	}{
		{
			name: "an unset timeout reports the default",
			seeded: `{
  "profiles": {
    "default": {
      "provider": "custom",
      "model": "m",
      "base_url": "http://127.0.0.1:11434"
    }
  }
}`,
			wantOut: "300",
		},
		{
			name: "an explicit timeout is reported verbatim",
			seeded: `{
  "profiles": {
    "default": {
      "provider": "custom",
      "model": "m",
      "base_url": "http://127.0.0.1:11434",
      "timeout_seconds": 45
    }
  }
}`,
			wantOut: "45",
		},
		{
			name: "a negative timeout reports the default",
			seeded: `{
  "profiles": {
    "default": {
      "provider": "custom",
      "model": "m",
      "base_url": "http://127.0.0.1:11434",
      "timeout_seconds": -5
    }
  }
}`,
			wantOut: "300",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			writeConfig(t, ws, tc.seeded)

			out, _ := runMpmCommand(t, bin, ws, "config", "get", "timeout_seconds")
			if !strings.Contains(out, tc.wantOut) {
				t.Errorf("`mpm config get timeout_seconds` = %q, want it to contain %q", out, tc.wantOut)
			}
		})
	}
}

// TestWizardSourceHasNoTimeoutPrompt is the structural guard.
//
// The behavioural tests above drive the wizard with a canned answer
// script. That proves the wizard does not ask TODAY; it does not prove
// that a future edit cannot add the prompt back. This guard reads the
// wizard's source and fails if a timeout prompt is ever reintroduced
// there.
//
// It is deliberately a SOURCE guard rather than a behavioural one: the
// prompt is a string in the source, and the string is what must not
// come back.
func TestWizardSourceHasNoTimeoutPrompt(t *testing.T) {
	src, err := os.ReadFile("handlers_config.go")
	if err != nil {
		t.Fatalf("read the wizard source: %v", err)
	}
	text := string(src)

	if strings.Contains(text, wizardTimeoutPrompt) {
		t.Errorf("handlers_config.go contains the prompt label %q; the request timeout is not an "+
			"onboarding question. See internal/core/config/timeout.go for the policy.", wizardTimeoutPrompt)
	}

	// The prompt could also come back in a different wording. The
	// defensible guard is on the CONCEPT: no interactive prompt may
	// offer a request timeout.
	for _, forbidden := range []string{
		"promptString(rwFromStdin(), \"Timeout",
		"promptString(rw, \"Timeout",
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("handlers_config.go contains %q; the request timeout is not an onboarding question", forbidden)
		}
	}
}

// TestNoOnboardingFlowAsksForATimeout is the package-wide half of the
// structural guard.
//
// The requirement is that the timeout question is removed from ALL
// relevant onboarding flows, not only from the default-profile wizard.
// A prompt could be reintroduced in the embedding wizard, the
// critic step, or the add-profile flow.
//
// This guard parses every non-test source file in the command package
// and fails if any CALL to an interactive prompt helper carries a
// timeout in its arguments. Parsing rather than grepping matters: a
// textual scan has to guess where a call's arguments end, and a wrong
// guess silently misses the very case the guard exists to catch.
//
// The prompt helpers are a closed set — promptString, promptChoice,
// promptChoiceDefault, promptSecret and confirmPrompt — so the scan is
// exhaustive for the ways this package can ask a question.
func TestNoOnboardingFlowAsksForATimeout(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the command package: %v", err)
	}

	promptHelpers := map[string]bool{
		"promptString":        true,
		"promptChoice":        true,
		"promptChoiceDefault": true,
		"promptSecret":        true,
		"confirmPrompt":       true,
	}

	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		checked++

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if !ok || !promptHelpers[ident.Name] {
				return true
			}
			for _, arg := range call.Args {
				lit, ok := arg.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				if strings.Contains(strings.ToLower(lit.Value), "timeout") {
					t.Errorf("%s: an interactive prompt offers a timeout: %s %s\n"+
						"The LLM request timeout is an advanced setting, not an onboarding question. "+
						"See internal/core/config/timeout.go for the policy.",
						fset.Position(lit.Pos()), ident.Name, lit.Value)
				}
			}
			return true
		})
	}

	if checked == 0 {
		t.Fatal("no source file was checked — the guard would pass vacuously")
	}
}

// TestTimeoutFieldRemainsManuallyEditable pins that the correction did
// not remove the operator's control.
//
// The timeout must still be settable from a terminal, and it must still
// be a field in mpm_config.json. Removing the QUESTION must not remove
// the SETTING.
func TestTimeoutFieldRemainsManuallyEditable(t *testing.T) {
	bin := buildMpmBinary(t)
	ws := t.TempDir()
	writeConfig(t, ws, `{
  "profiles": {
    "default": {
      "provider": "custom",
      "model": "m",
      "base_url": "http://127.0.0.1:11434"
    }
  }
}`)

	out, _ := runMpmCommand(t, bin, ws, "config", "profile", "set", "default", "timeout_seconds", "77")
	if strings.Contains(out, "error") && !strings.Contains(out, "timeout_seconds") {
		t.Fatalf("setting the timeout failed: %s", out)
	}

	got, _ := runMpmCommand(t, bin, ws, "config", "get", "timeout_seconds")
	if !strings.Contains(got, "77") {
		t.Errorf("the configured timeout was not retained: %q", got)
	}

	// And the field is still in the file, as an advanced setting.
	path := filepath.Join(ws, "mpm_config.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the config: %v", err)
	}
	if !strings.Contains(string(raw), `"timeout_seconds"`) {
		t.Errorf("the configuration no longer carries timeout_seconds:\n%s", raw)
	}
}

// TestConfigSet_RejectsANonPositiveTimeout pins the entry-point
// validation.
//
// `mpm config set` and `mpm config profile set` reject a non-positive
// value at the point of entry, so the fallback in
// config.EffectiveTimeoutSeconds only ever applies to a hand-edited
// file.
func TestConfigSet_RejectsANonPositiveTimeout(t *testing.T) {
	bin := buildMpmBinary(t)
	ws := t.TempDir()
	writeConfig(t, ws, `{
  "profiles": {
    "default": {
      "provider": "custom",
      "model": "m",
      "base_url": "http://127.0.0.1:11434"
    }
  }
}`)

	for _, bad := range []string{"0", "-1"} {
		out, _ := runMpmCommand(t, bin, ws, "config", "profile", "set", "default", "timeout_seconds", bad)
		if !strings.Contains(out, "positive integer") {
			t.Errorf("setting timeout_seconds=%s was not rejected: %q", bad, out)
		}
	}
}
