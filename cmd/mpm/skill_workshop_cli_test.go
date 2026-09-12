// skill_workshop_cli_test.go — CLI regression coverage for the Skills
// Workshop operator surface.
//
// The workshop backend (`mpm_skills` action `workshop` +
// `internal.RunWorkshop`) shipped earlier; these tests pin the
// operator-facing CLI surface so a future refactor can't silently
// regress routing, help, or the JSON form/refine pipeline.
//
// All tests run in an isolated temp `MPM_WORKSPACE` so the production
// `~/.mpm` database is never touched. We invoke the freshly-built
// `bin/mpm` binary directly so the tests exercise the same wiring
// the operator does.

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runMpmSkillWorkspace spawns the built `bin/mpm` binary in a temp
// workspace, so tests don't touch the production database. The
// workspace lives under t.TempDir() and is cleaned up automatically.
func runMpmSkillWorkspace(t *testing.T, args ...string) (string, int) {
	t.Helper()
	binPath := filepath.Join("..", "..", "bin", "mpm")
	if _, err := os.Stat(binPath); os.IsNotExist(err) {
		t.Skip("bin/mpm not built; run `make build` first")
	}
	tmpDir := t.TempDir()
	workspace := filepath.Join(tmpDir, "workspace")
	cmd := exec.Command(binPath, args...)
	cmd.Env = append(os.Environ(), "MPM_WORKSPACE="+workspace)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("exec %v: %v", args, err)
	}
	return string(out), code
}

// parseWorkshopJSON extracts the JSON object from `out`. The CLI
// sometimes prepends boot lines (scheduler warnings, poison-phrase
// seed notice) before the JSON; we locate the first '{' and unmarshal
// from there. Returns an error if no JSON object can be found.
func parseWorkshopJSON(t *testing.T, out string) map[string]interface{} {
	t.Helper()
	return parseWorkshopJSONFromBytes(t, []byte(out))
}

// parseWorkshopJSONFromBytes is the byte-slice form used by helper
// closures that capture raw subprocess output.
func parseWorkshopJSONFromBytes(t *testing.T, out []byte) map[string]interface{} {
	t.Helper()
	idx := bytesIndexByte(out, '{')
	if idx < 0 {
		t.Fatalf("no JSON object in output:\n%s", string(out))
	}
	candidate := out[idx:]
	var resp map[string]interface{}
	if err := json.Unmarshal(candidate, &resp); err != nil {
		t.Fatalf("response not JSON: %v\noutput:\n%s", err, string(out))
	}
	return resp
}

// bytesIndexByte mirrors bytes.IndexByte without importing "bytes" —
// keeps the file's imports focused on what the workshop tests need.
func bytesIndexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// TestSkillWorkshop_Help_PrintsWorkshopGuide pins the help output
// for `mpm skill workshop --help`.
//
// Regression coverage for the pre-fix routing bug: without the
// `"skill"` entry in `commandsWithSubcommandDispatch`, the Stage-7
// help intercept routed `--help` to `handleSaveSkillHelp()` and
// printed the save-skill markdown page instead. We assert the help
// contains the workshop vocabulary AND does not contain the save-
// skill page text.
func TestSkillWorkshop_Help_PrintsWorkshopGuide(t *testing.T) {
	out, code := runMpmSkillWorkspace(t, "skill", "workshop", "--help")
	if code != 0 {
		t.Fatalf("exit %d; help must exit 0\noutput:\n%s", code, out)
	}
	for _, want := range []string{
		"form",
		"refine",
		"decision_model",
		"boundary",
		"procedure",
		"judgment",
		"knowledge",
		"change_type",
		"published",
		"candidate",
		"rejected",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("workshop help missing %q\noutput:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Save a skill from a markdown file") {
		t.Errorf("workshop help must not show save-skill help text:\n%s", out)
	}
}

// TestSkillWorkshop_Help_ShortFormsAlsoRoute exercises `-h` and the
// literal `help` token alongside the canonical `--help`. All three
// must route to the workshop help page.
func TestSkillWorkshop_Help_ShortFormsAlsoRoute(t *testing.T) {
	for _, args := range [][]string{
		{"skill", "workshop", "-h"},
		{"skill", "workshop", "--help"},
		{"skill", "workshop", "help"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			out, code := runMpmSkillWorkspace(t, args...)
			if code != 0 {
				t.Errorf("%v exited %d; help must exit 0\noutput:\n%s", args, code, out)
			}
			if !strings.Contains(out, "decision_model") {
				t.Errorf("%v did not print workshop help\noutput:\n%s", args, out)
			}
		})
	}
}

// TestSkillWorkshop_Form_PublishesSkill pins the `mode=form` happy
// path: a valid JSON payload with `decision_model` total ≥ 6,
// `boundary=procedure`, and a populated proposal must reach the
// canonical publication path (SkillID populated, outcome=published).
func TestSkillWorkshop_Form_PublishesSkill(t *testing.T) {
	payload := map[string]interface{}{
		"mode": "form",
		"decision_model": map[string]interface{}{
			"reusability":     2,
			"non_obviousness": 2,
			"stability":       2,
			"leverage":        2,
			"boundary":        "procedure",
		},
		"proposal": map[string]interface{}{
			"name":        "cli-test-form-skill",
			"version":     "1.0.0",
			"description": "form-mode regression skill",
			"when_to_use": "running a CLI regression test for the form mode of the workshop",
			"domain":      "test",
			"constraints": []string{},
			"steps": []map[string]interface{}{
				{"call": "do the thing", "args_from": ""},
			},
		},
		"task_context":       "TDD regression for mpm skill workshop --mode form",
		"workflow_description": "fill decision model, validate, publish",
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	tmpDir := t.TempDir()
	workspace := filepath.Join(tmpDir, "workspace")
	payloadPath := filepath.Join(tmpDir, "form.json")
	if err := os.WriteFile(payloadPath, payloadBytes, 0644); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	cmd := exec.Command(filepath.Join("..", "..", "bin", "mpm"),
		"skill", "workshop", "--file", payloadPath)
	cmd.Env = append(os.Environ(), "MPM_WORKSPACE="+workspace)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	if code != 0 {
		t.Fatalf("form should succeed; exit %d\noutput:\n%s", code, out)
	}
	resp := parseWorkshopJSONFromBytes(t, out)
	if got, _ := resp["outcome"].(string); got != "published" {
		t.Errorf("outcome = %q, want published\nresp: %v", got, resp)
	}
	if _, ok := resp["skill_id"].(string); !ok {
		t.Errorf("skill_id missing or non-string in resp: %v", resp)
	}
}

// TestSkillWorkshop_Refine_MissingChangeType_Rejected pins that
// `mode=refine` without `change_type` is rejected with a non-zero
// exit and a clear error message. The substrate requirement is
// documented in `validateInput` (skill_workshop.go:214).
func TestSkillWorkshop_Refine_MissingChangeType_Rejected(t *testing.T) {
	payload := map[string]interface{}{
		"mode":   "refine",
		"intent": "nonexistent-skill",
		"decision_model": map[string]interface{}{
			"reusability": 2, "non_obviousness": 2, "stability": 2, "leverage": 2,
			"boundary": "procedure",
		},
		"proposal": map[string]interface{}{
			"name": "refine-no-change-type",
			"version": "1.0.0",
			"description": "should be rejected",
			"when_to_use": "trying to refine without change_type is not allowed",
		},
	}
	payloadBytes, _ := json.Marshal(payload)
	tmpDir := t.TempDir()
	payloadPath := filepath.Join(tmpDir, "bad.json")
	os.WriteFile(payloadPath, payloadBytes, 0644)

	// Don't even need a real workspace — validation happens before DB.
	out, code := runMpmSkillWorkspace(t, "skill", "workshop", "--file", payloadPath)
	if code == 0 {
		t.Fatalf("missing change_type should fail; output:\n%s", out)
	}
	// The error is surfaced via the JSON envelope (`outcome:"rejected",
	// reason:"input_validation_failed"`), or via stderr if validateInput
	// propagates the err. Either way, the validation message must
	// mention change_type.
	if !strings.Contains(strings.ToLower(out), "change_type") &&
		!strings.Contains(strings.ToLower(out), "change-type") {
		t.Errorf("error output should mention change_type, got:\n%s", out)
	}
}

// TestSkillWorkshop_InvalidBoundary_Rejected pins that an out-of-
// vocabulary `decision_model.boundary` is rejected with a clear
// error referencing the legal enum.
func TestSkillWorkshop_InvalidBoundary_Rejected(t *testing.T) {
	payload := map[string]interface{}{
		"mode": "form",
		"decision_model": map[string]interface{}{
			"reusability": 2, "non_obviousness": 2, "stability": 2, "leverage": 2,
			"boundary": "fact", // not in {procedure, judgment, knowledge}
		},
		"proposal": map[string]interface{}{
			"name": "bad-boundary",
			"version": "1.0.0",
			"when_to_use": "trying a non-canonical boundary value should be rejected",
		},
	}
	payloadBytes, _ := json.Marshal(payload)
	tmpDir := t.TempDir()
	payloadPath := filepath.Join(tmpDir, "bad.json")
	os.WriteFile(payloadPath, payloadBytes, 0644)

	out, code := runMpmSkillWorkspace(t, "skill", "workshop", "--file", payloadPath)
	if code == 0 {
		t.Fatalf("invalid boundary should fail; output:\n%s", out)
	}
	// The error must name the legal vocabulary so operators can fix
	// their payload without reading the source.
	if !strings.Contains(out, "procedure") ||
		!strings.Contains(out, "judgment") ||
		!strings.Contains(out, "knowledge") {
		t.Errorf("error should enumerate the boundary vocabulary; got:\n%s", out)
	}
}

// TestSkillWorkshop_CLIEqualsMCPRoute pins CLI/MCP parity: an
// equivalent payload delivered through `mpm skill workshop` and
// through `mpm call mpm_skills --payload ...` must produce the same
// canonical outcome (same `outcome`, same `decision_model.boundary`).
// The CLI/MCP parity principle is in the user's task brief: the
// CLI must be a thin shim, not a duplicate pipeline.
func TestSkillWorkshop_CLIEqualsMCPRoute(t *testing.T) {
	payload := map[string]interface{}{
		"mode": "form",
		"decision_model": map[string]interface{}{
			"reusability":     1,
			"non_obviousness": 1,
			"stability":       1,
			"leverage":        1,
			"boundary":        "procedure",
		},
		"proposal": map[string]interface{}{
			"name":        "cli-mcp-parity-skill",
			"version":     "1.0.0",
			"when_to_use": "proving that the CLI and MCP routes share a canonical pipeline",
		},
	}
	payloadBytes, _ := json.Marshal(payload)
	tmpDir := t.TempDir()
	workspace := filepath.Join(tmpDir, "workspace")
	payloadPath := filepath.Join(tmpDir, "parity.json")
	os.WriteFile(payloadPath, payloadBytes, 0644)

	binPath := filepath.Join("..", "..", "bin", "mpm")
	runInWorkspace := func(args ...string) (map[string]interface{}, int) {
		cmd := exec.Command(binPath, args...)
		cmd.Env = append(os.Environ(), "MPM_WORKSPACE="+workspace)
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		resp := parseWorkshopJSONFromBytes(t, out)
		return resp, code
	}

	// CLI route.
	cliResp, cliCode := runInWorkspace("skill", "workshop", "--file", payloadPath)
	if cliCode != 0 {
		t.Fatalf("CLI route failed; resp: %v", cliResp)
	}

	// MCP route — same payload, wrapped in the canonical envelope.
	mcpEnvelope := map[string]interface{}{
		"action": "workshop",
		"params": payload,
	}
	envelopeBytes, _ := json.Marshal(mcpEnvelope)
	envelopePath := filepath.Join(tmpDir, "envelope.json")
	os.WriteFile(envelopePath, envelopeBytes, 0644)
	mcpResp, mcpCode := runInWorkspace("call", "mpm_skills", "--payload-file", envelopePath)
	if mcpCode != 0 {
		t.Fatalf("MCP route failed; resp: %v", mcpResp)
	}

	// Compare the canonical fields the workshop pipeline always emits.
	for _, key := range []string{"outcome", "skill_id", "decision_model"} {
		if !equalJSONish(cliResp[key], mcpResp[key]) {
			t.Errorf("CLI vs MCP mismatch on %q:\n  cli=%v\n  mcp=%v", key, cliResp[key], mcpResp[key])
		}
	}
}

// equalJSONish is a loose comparison: both inputs are decoded into
// `interface{}` after `json.Unmarshal`, so a string vs a number from
// different unmarshallers would fail `==`. We round-trip through
// json.Marshal to normalize.
func equalJSONish(a, b interface{}) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}