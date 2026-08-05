package capability

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// =============================================================================
// forge_test.go — end-to-end pipeline integration tests
//
// Strategy: wire a real Store (in-memory SQLite) with FakeLinter +
// FakeDedup + FakeDryRunner. This exercises:
//   * Store-level checks (size, uniqueness, dep liveness, domain policy)
//   * Forge-level checks (schema, scanner, lint, dedup, dry-run)
//   * InitialState routing (validated vs linted)
//
// Real bwrap / shellcheck / ruff are NOT exercised here — the
// Fakes are sufficient because the Forge's contract is with the
// interfaces, not the implementations.
// =============================================================================

// goodPayload is the canonical valid proposal. Tests clone and
// mutate this for their specific rejection scenario.
func goodPayload() *ForgePayload {
	return &ForgePayload{
		Name:            "cleanup_paths",
		Purpose:         "remove temporary build artifacts older than 7 days from /var/tmp/build",
		SourceCode:      "#!/bin/bash\nset -euo pipefail\nfind /var/tmp/build -type f -mtime +7 -delete\n",
		SourceLanguage:  "bash",
		RequestedDomain: "sandbox",
		Tags:            []string{"cleanup", "tmpfs"},
	}
}

func newTestForge(t *testing.T) (*Forge, *FakeLinter, *FakeDedup, *FakeDryRunner) {
	t.Helper()
	store, _, _ := newTestStore(t)
	linter := &FakeLinter{}
	dedup := &FakeDedup{}
	dryRun := &FakeDryRunner{}
	forge := NewForge(store, linter, dedup, dryRun, nil)
	return forge, linter, dedup, dryRun
}

func TestForge_HappyPath(t *testing.T) {
	forge, linter, dedup, dryRun := newTestForge(t)
	payload := goodPayload()

	res, err := forge.Propose(context.Background(), payload)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if res.Rejected {
		t.Fatalf("expected accept, got rejection: %+v", res.Reasons)
	}
	if res.ID == "" {
		t.Error("expected non-empty capability ID")
	}
	if res.State != StateValidated {
		t.Errorf("initial state = %s, want validated (dry-run passed)", res.State)
	}
	if res.Name != payload.Name {
		t.Errorf("name = %q, want %q", res.Name, payload.Name)
	}
	if len(linter.Calls) != 1 {
		t.Errorf("expected 1 linter call, got %d", len(linter.Calls))
	}
	if len(dedup.Calls) != 1 {
		t.Errorf("expected 1 dedup call, got %d", len(dedup.Calls))
	}
	if len(dryRun.Calls) != 1 {
		t.Errorf("expected 1 dry-run call, got %d", len(dryRun.Calls))
	}
}

func TestForge_RejectsSchema(t *testing.T) {
	forge, _, _, _ := newTestForge(t)

	cases := []struct {
		name     string
		mutate   func(*ForgePayload)
		wantStep string
		wantField string
	}{
		{"missing name", func(p *ForgePayload) { p.Name = "" }, "schema", "name"},
		{"bad name slug", func(p *ForgePayload) { p.Name = "Bad-Name-123" }, "schema", "name"},
		{"purpose too short", func(p *ForgePayload) { p.Purpose = "short" }, "schema", "purpose"},
		{"empty source", func(p *ForgePayload) { p.SourceCode = "" }, "schema", "source_code"},
		{"unknown language", func(p *ForgePayload) { p.SourceLanguage = "ruby" }, "schema", "source_language"},
		{"operator domain", func(p *ForgePayload) { p.RequestedDomain = "operator" }, "schema", "requested_domain"},
		{"missing domain", func(p *ForgePayload) { p.RequestedDomain = "" }, "schema", "requested_domain"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := goodPayload()
			tc.mutate(p)
			res, err := forge.Propose(context.Background(), p)
			if err != nil {
				t.Fatalf("Propose: %v", err)
			}
			if !res.Rejected {
				t.Fatalf("expected rejection, got accept: %+v", res)
			}
			found := false
			for _, r := range res.Reasons {
				if r.Step == tc.wantStep && r.Field == tc.wantField {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected rejection on %s.%s, got: %+v",
					tc.wantStep, tc.wantField, res.Reasons)
			}
		})
	}
}

func TestForge_RejectsScannerMatches(t *testing.T) {
	forge, linter, dedup, dryRun := newTestForge(t)

	p := goodPayload()
	p.SourceCode = "#!/bin/bash\nrm -rf /\n"
	res, err := forge.Propose(context.Background(), p)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if !res.Rejected {
		t.Fatal("expected scanner rejection")
	}
	scannerFound := false
	for _, r := range res.Reasons {
		if strings.HasPrefix(r.Step, "scanner:") {
			scannerFound = true
		}
	}
	if !scannerFound {
		t.Errorf("expected scanner:* rejection, got: %+v", res.Reasons)
	}
	// Linter and dedup and dry-run should be SKIPPED when scanner rejected.
	if len(linter.Calls) != 0 {
		t.Errorf("linter should be skipped after scanner rejection, got %d calls", len(linter.Calls))
	}
	if len(dedup.Calls) != 0 {
		t.Errorf("dedup should be skipped after scanner rejection, got %d calls", len(dedup.Calls))
	}
	if len(dryRun.Calls) != 0 {
		t.Errorf("dry-run should be skipped after scanner rejection, got %d calls", len(dryRun.Calls))
	}
}

func TestForge_RejectsLinterErrors(t *testing.T) {
	forge, linter, _, _ := newTestForge(t)
	linter.Reports = []*LintReport{
		{Linter: "shellcheck", OK: false, Findings: []LintFinding{
			{Line: 5, Column: 3, Code: "SC2068", Severity: "error", Message: "unquoted array"},
		}},
	}

	res, err := forge.Propose(context.Background(), goodPayload())
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if !res.Rejected {
		t.Fatal("expected linter rejection")
	}
	found := false
	for _, r := range res.Reasons {
		if strings.HasPrefix(r.Step, "linter:") && r.Line == 5 {
			found = true
		}
	}
	if !found {
		t.Errorf("expected linter rejection at line 5, got: %+v", res.Reasons)
	}
}

func TestForge_LinterWarningsDoNotReject(t *testing.T) {
	forge, linter, _, _ := newTestForge(t)
	// Only warnings (severity=warning, not error). Should not reject.
	linter.Reports = []*LintReport{
		{Linter: "shellcheck", OK: true, Findings: []LintFinding{
			{Line: 1, Code: "SC2034", Severity: "warning", Message: "unused var"},
		}},
	}
	res, err := forge.Propose(context.Background(), goodPayload())
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if res.Rejected {
		t.Errorf("warnings should not reject, got: %+v", res.Reasons)
	}
}

func TestForge_LinterInfrastructureErrorRejects(t *testing.T) {
	forge, linter, _, _ := newTestForge(t)
	linter.Errors = []error{errors.New("shellcheck: command not found")}

	res, err := forge.Propose(context.Background(), goodPayload())
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if !res.Rejected {
		t.Fatal("expected linter-infrastructure rejection")
	}
	found := false
	for _, r := range res.Reasons {
		if r.Step == "linter" && strings.Contains(r.Message, "shellcheck") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected linter infrastructure error, got: %+v", res.Reasons)
	}
}

func TestForge_RejectsDedupMatch(t *testing.T) {
	forge, _, dedup, dryRun := newTestForge(t)
	dedup.Result = DedupResult{
		Matched: true, MatchedID: "cap_existing",
		MatchedName: "existing_tool", Similarity: 0.97,
	}

	res, err := forge.Propose(context.Background(), goodPayload())
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if !res.Rejected {
		t.Fatal("expected dedup rejection")
	}
	found := false
	for _, r := range res.Reasons {
		if r.Step == "dedup" && strings.Contains(r.Message, "existing_tool") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected dedup rejection mentioning existing_tool, got: %+v", res.Reasons)
	}
	// Dry-run is skipped when dedup matches.
	if len(dryRun.Calls) != 0 {
		t.Errorf("dry-run should be skipped on dedup match, got %d calls", len(dryRun.Calls))
	}
}

func TestForge_ReplacesIDSkipsDedupForThatID(t *testing.T) {
	store, db, _ := newTestStore(t)
	dedup := &FakeDedup{}
	forge := NewForge(store, &FakeLinter{}, dedup, &FakeDryRunner{}, nil)

	// Pre-seed the predecessor so the FK on created_from_id
	// (auto-populated from replaces_id by ToProposal) is
	// satisfied. The test focuses on the dedup skip-list
	// behaviour, not on the lineage FK.
	seedCapability(t, db, "cap_existing", "existing_tool", StateActive, nil)

	p := goodPayload()
	rid := "cap_existing"
	p.ReplacesID = &rid

	res, err := forge.Propose(context.Background(), p)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if res.Rejected {
		t.Errorf("expected accept (dedup defaulted to non-match), got: %+v", res.Reasons)
	}
	// The dedup call should have received cap_existing in its
	// skip list (so even if the dedup had matched, the fork
	// flow would have let it through).
	if len(dedup.Calls) != 1 {
		t.Fatalf("expected 1 dedup call, got %d", len(dedup.Calls))
	}
	got := dedup.Calls[0].SkipIDs
	if len(got) != 1 || got[0] != rid {
		t.Errorf("skip list = %v, want [%q]", got, rid)
	}
}

func TestForge_RejectsDryRunFailure(t *testing.T) {
	forge, _, _, dryRun := newTestForge(t)
	dryRun.Results = []DryRunResult{
		{Pass: false, Reason: "syntax error", Stderr: "syntax error near 'fi'"},
	}

	res, err := forge.Propose(context.Background(), goodPayload())
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if !res.Rejected {
		t.Fatal("expected dry-run rejection")
	}
	found := false
	for _, r := range res.Reasons {
		if r.Step == "dryrun" && strings.Contains(r.Message, "syntax error") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected dryrun rejection mentioning syntax error, got: %+v", res.Reasons)
	}
}

func TestForge_DryRunInfrastructureErrorRejects(t *testing.T) {
	forge, _, _, dryRun := newTestForge(t)
	dryRun.Errors = []error{errors.New("bwrap: command not found")}

	res, err := forge.Propose(context.Background(), goodPayload())
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if !res.Rejected {
		t.Fatal("expected dry-run-infrastructure rejection")
	}
	found := false
	for _, r := range res.Reasons {
		if r.Step == "dryrun" && strings.Contains(r.Message, "bwrap") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected dryrun infrastructure error, got: %+v", res.Reasons)
	}
}

func TestForge_InsertNameCollisionRejects(t *testing.T) {
	forge, _, _, _ := newTestForge(t)

	// First proposal succeeds.
	res1, err := forge.Propose(context.Background(), goodPayload())
	if err != nil {
		t.Fatalf("first Propose: %v", err)
	}
	if res1.Rejected {
		t.Fatalf("first proposal should accept, got: %+v", res1.Reasons)
	}

	// Second proposal with the same name should fail at insert.
	// The Store's ErrAlreadyExists surfaces as a wrapped error
	// from the Forge (not a structured rejection), because the
	// Store's error happens AFTER the Forge's accumulated
	// pre-insert checks. The Forge returns nil ForgeResult and
	// a non-nil error.
	res2, err := forge.Propose(context.Background(), goodPayload())
	if err == nil {
		t.Fatal("expected error from second insert (name collision)")
	}
	if !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("expected ErrAlreadyExists in chain, got: %v", err)
	}
	if res2 != nil {
		t.Errorf("expected nil result on insert failure, got: %+v", res2)
	}
}

func TestForge_NilDependencies(t *testing.T) {
	// Defensive: a nil store, linter, dedup, or dry-run should
	// error early, not panic.
	t.Run("nil store", func(t *testing.T) {
		_, err := NewForge(nil, &FakeLinter{}, &FakeDedup{}, &FakeDryRunner{}, nil).
			Propose(context.Background(), goodPayload())
		if err == nil {
			t.Error("expected error for nil store")
		}
	})
	t.Run("nil linter", func(t *testing.T) {
		store, _, _ := newTestStore(t)
		_, err := NewForge(store, nil, &FakeDedup{}, &FakeDryRunner{}, nil).
			Propose(context.Background(), goodPayload())
		if err == nil {
			t.Error("expected error for nil linter")
		}
	})
	t.Run("nil dedup", func(t *testing.T) {
		store, _, _ := newTestStore(t)
		_, err := NewForge(store, &FakeLinter{}, nil, &FakeDryRunner{}, nil).
			Propose(context.Background(), goodPayload())
		if err == nil {
			t.Error("expected error for nil dedup")
		}
	})
	t.Run("nil dryrun", func(t *testing.T) {
		store, _, _ := newTestStore(t)
		_, err := NewForge(store, &FakeLinter{}, &FakeDedup{}, nil, nil).
			Propose(context.Background(), goodPayload())
		if err == nil {
			t.Error("expected error for nil dryrun")
		}
	})
	t.Run("nil payload", func(t *testing.T) {
		store, _, _ := newTestStore(t)
		_, err := NewForge(store, &FakeLinter{}, &FakeDedup{}, &FakeDryRunner{}, nil).
			Propose(context.Background(), nil)
		if err == nil {
			t.Error("expected error for nil payload")
		}
	})
}

func TestForge_AccumulatesMultipleRejections(t *testing.T) {
	forge, _, _, _ := newTestForge(t)
	p := goodPayload()
	p.Name = "Bad-Name"           // schema reject
	p.Purpose = "short"            // schema reject
	p.SourceCode = "rm -rf /"      // scanner reject
	res, err := forge.Propose(context.Background(), p)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if !res.Rejected {
		t.Fatal("expected rejection")
	}
	if len(res.Reasons) < 3 {
		t.Errorf("expected at least 3 reasons (schema x2, scanner x1), got %d: %+v",
			len(res.Reasons), res.Reasons)
	}
}

func TestForge_AcceptedCapabilityIsPersisted(t *testing.T) {
	forge, _, _, _ := newTestForge(t)
	store, db, _ := newTestStore(t)
	// Re-wire with the same store for verification.
	forge = NewForge(store, &FakeLinter{}, &FakeDedup{}, &FakeDryRunner{}, nil)

	res, err := forge.Propose(context.Background(), goodPayload())
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if res.Rejected {
		t.Fatalf("expected accept, got: %+v", res.Reasons)
	}

	// Look up the row in the DB and verify it's there with the
	// expected state and source.
	var state, name, lang string
	if err := db.QueryRow(
		`SELECT state, name, source_language FROM capabilities WHERE id = ?`,
		res.ID,
	).Scan(&state, &name, &lang); err != nil {
		t.Fatalf("query row: %v", err)
	}
	if state != "validated" {
		t.Errorf("persisted state = %q, want validated", state)
	}
	if name != goodPayload().Name {
		t.Errorf("persisted name = %q, want %q", name, goodPayload().Name)
	}
	if lang != "bash" {
		t.Errorf("persisted language = %q, want bash", lang)
	}
}
