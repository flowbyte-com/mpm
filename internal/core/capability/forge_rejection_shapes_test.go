package capability

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// =============================================================================
// forge_rejection_shapes_test.go — structured-rejection contract tests
//
// These tests pin the *shape* of every rejection pathway. The agent
// consuming the Forge's response needs to be able to parse the
// `reasons` array reliably — every entry must have a Step, Field,
// and Message, and the JSON form must round-trip cleanly. Changing
// the shape is a breaking change for every Forge consumer.
// =============================================================================

// assertRejectionShape validates the universal contract that
// every FieldError must satisfy: non-empty Step, non-empty
// Message, optional but typed Field.
func assertRejectionShape(t *testing.T, fe FieldError) {
	t.Helper()
	if fe.Step == "" {
		t.Error("FieldError.Step must not be empty")
	}
	if fe.Message == "" {
		t.Error("FieldError.Message must not be empty")
	}
	// Step must use the step-prefix conventions:
	//   "schema"            — payload validation
	//   "scanner:<name>"    — §7.1 poison pattern
	//   "linter[:<binary>]" — external linter
	//   "linter"            — linter infrastructure failure
	//   "dedup"             — duplicate purpose
	//   "dryrun"            — sandbox failure
	prefixes := []string{"schema", "scanner:", "linter", "dedup", "dryrun"}
	matched := false
	for _, p := range prefixes {
		if strings.HasPrefix(fe.Step, p) {
			matched = true
			break
		}
	}
	if !matched {
		t.Errorf("FieldError.Step %q does not use a known prefix (schema, scanner:, linter, dedup, dryrun)", fe.Step)
	}
}

func TestRejectionShape_SchemaError(t *testing.T) {
	forge, _, _, _ := newTestForge(t)
	p := goodPayload()
	p.Name = "" // schema rejection
	res, err := forge.Propose(context.Background(), p)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if !res.Rejected {
		t.Fatal("expected rejection")
	}
	for _, r := range res.Reasons {
		assertRejectionShape(t, r)
	}
}

func TestRejectionShape_ScannerError(t *testing.T) {
	forge, _, _, _ := newTestForge(t)
	p := goodPayload()
	p.SourceCode = "rm -rf /"
	res, err := forge.Propose(context.Background(), p)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if !res.Rejected {
		t.Fatal("expected rejection")
	}
	for _, r := range res.Reasons {
		assertRejectionShape(t, r)
		if !strings.HasPrefix(r.Step, "scanner:") {
			t.Errorf("scanner rejection step should start with 'scanner:', got %q", r.Step)
		}
		if r.Line <= 0 {
			t.Errorf("scanner rejection should report a line number, got %d", r.Line)
		}
		if r.Snippet == "" {
			t.Errorf("scanner rejection should report a snippet, got empty")
		}
	}
}

func TestRejectionShape_LinterError(t *testing.T) {
	forge, linter, _, _ := newTestForge(t)
	linter.Reports = []*LintReport{
		{Linter: "shellcheck", OK: false, Findings: []LintFinding{
			{Line: 7, Column: 12, Code: "SC2086", Severity: "error", Message: "double quote to prevent globbing"},
		}},
	}
	res, err := forge.Propose(context.Background(), goodPayload())
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if !res.Rejected {
		t.Fatal("expected rejection")
	}
	for _, r := range res.Reasons {
		assertRejectionShape(t, r)
		if !strings.HasPrefix(r.Step, "linter:") {
			t.Errorf("linter rejection step should start with 'linter:', got %q", r.Step)
		}
		if r.Line != 7 {
			t.Errorf("linter rejection should preserve line number, got %d", r.Line)
		}
	}
}

func TestRejectionShape_DedupError(t *testing.T) {
	forge, _, dedup, _ := newTestForge(t)
	dedup.Result = DedupResult{
		Matched: true, MatchedID: "cap_existing",
		MatchedName: "existing_tool", Similarity: 0.97,
	}
	res, err := forge.Propose(context.Background(), goodPayload())
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if !res.Rejected {
		t.Fatal("expected rejection")
	}
	for _, r := range res.Reasons {
		assertRejectionShape(t, r)
		if r.Step != "dedup" {
			t.Errorf("dedup rejection step should be exactly 'dedup', got %q", r.Step)
		}
		if r.Field != "purpose" {
			t.Errorf("dedup rejection field should be 'purpose', got %q", r.Field)
		}
		// Message should mention the existing tool by name
		// so the agent knows what to compare against.
		if !strings.Contains(r.Message, "existing_tool") {
			t.Errorf("dedup message should name the existing tool, got: %q", r.Message)
		}
	}
}

func TestRejectionShape_DryRunError(t *testing.T) {
	forge, _, _, dryRun := newTestForge(t)
	dryRun.Results = []DryRunResult{
		{Pass: false, Reason: "syntax error", Stderr: "line 3: unexpected token"},
	}
	res, err := forge.Propose(context.Background(), goodPayload())
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if !res.Rejected {
		t.Fatal("expected rejection")
	}
	for _, r := range res.Reasons {
		assertRejectionShape(t, r)
		if r.Step != "dryrun" {
			t.Errorf("dryrun rejection step should be exactly 'dryrun', got %q", r.Step)
		}
		if r.Field != "source_code" {
			t.Errorf("dryrun rejection field should be 'source_code', got %q", r.Field)
		}
		if !strings.Contains(r.Message, "syntax error") {
			t.Errorf("dryrun message should quote the failure reason, got: %q", r.Message)
		}
	}
}

// TestRejectionJSON_RoundTrip confirms the rejection list can be
// JSON-encoded and decoded without loss. This is the contract
// every CLI/MCP consumer depends on; if this test fails, every
// downstream renderer breaks.
func TestRejectionJSON_RoundTrip(t *testing.T) {
	forge, _, _, _ := newTestForge(t)
	// Build a payload that triggers schema + scanner + linter
	// rejections all at once.
	p := goodPayload()
	p.Name = "Bad-Name"
	p.Purpose = "short"
	p.SourceCode = "rm -rf /"
	res, err := forge.Propose(context.Background(), p)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if !res.Rejected {
		t.Fatal("expected rejection")
	}

	// Marshal the entire result (including reasons) to JSON.
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Decode into a fresh struct and check field equivalence.
	var back ForgeResult
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Rejected != res.Rejected {
		t.Errorf("Rejected mismatch: %v vs %v", back.Rejected, res.Rejected)
	}
	if len(back.Reasons) != len(res.Reasons) {
		t.Fatalf("reasons count mismatch: %d vs %d", len(back.Reasons), len(res.Reasons))
	}
	for i := range res.Reasons {
		if back.Reasons[i].Step != res.Reasons[i].Step {
			t.Errorf("reason[%d] Step: %q vs %q", i, back.Reasons[i].Step, res.Reasons[i].Step)
		}
		if back.Reasons[i].Field != res.Reasons[i].Field {
			t.Errorf("reason[%d] Field: %q vs %q", i, back.Reasons[i].Field, res.Reasons[i].Field)
		}
		if back.Reasons[i].Message != res.Reasons[i].Message {
			t.Errorf("reason[%d] Message: %q vs %q", i, back.Reasons[i].Message, res.Reasons[i].Message)
		}
		if back.Reasons[i].Line != res.Reasons[i].Line {
			t.Errorf("reason[%d] Line: %d vs %d", i, back.Reasons[i].Line, res.Reasons[i].Line)
		}
	}
}

// TestAcceptJSON_RoundTrip confirms the happy-path result is
// also JSON-stable. The CLI/MCP layer must be able to encode
// an accepted proposal back to the agent.
func TestAcceptJSON_RoundTrip(t *testing.T) {
	forge, _, _, _ := newTestForge(t)
	res, err := forge.Propose(context.Background(), goodPayload())
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if res.Rejected {
		t.Fatalf("expected accept, got: %+v", res.Reasons)
	}
	if res.ID == "" {
		t.Fatal("accept must include a non-empty ID")
	}
	if res.State == "" {
		t.Fatal("accept must include a non-empty State")
	}
	if res.Reasons != nil {
		t.Errorf("accept should have nil Reasons, got: %+v", res.Reasons)
	}

	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back ForgeResult
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.ID != res.ID || back.State != res.State {
		t.Errorf("ID/State mismatch after roundtrip: %+v vs %+v", back, res)
	}
}

// TestRejectionAccumulation_OrderIndependent confirms the Forge
// collects rejections across steps in a stable order. The order
// should be schema → scanner → linter (deterministic). The agent
// depends on this for stable test fixtures.
func TestRejectionAccumulation_OrderIndependent(t *testing.T) {
	forge, _, _, _ := newTestForge(t)
	p := goodPayload()
	p.Name = ""           // schema
	p.SourceCode = ""     // schema
	p.Purpose = "short"   // schema
	res, err := forge.Propose(context.Background(), p)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if !res.Rejected {
		t.Fatal("expected rejection")
	}
	// All three should be schema rejections.
	for _, r := range res.Reasons {
		if r.Step != "schema" {
			t.Errorf("expected all schema steps, got: %+v", res.Reasons)
			break
		}
	}
}
