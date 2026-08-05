package capability

import "testing"

// TestCapabilityState_CanTransitionTo pins the §1.3 transition table.
// Adding a transition to validTransitions without updating this test
// will fail — that's intentional.
func TestCapabilityState_CanTransitionTo(t *testing.T) {
	tests := []struct {
		from, to CapabilityState
		want     bool
		desc     string
	}{
		// Legal transitions (§1.3)
		{StateDraft, StateLinted, true, "draft → linted (forge lint pass)"},
		{StateLinted, StateValidated, true, "linted → validated (dry-run exit 0)"},
		{StateValidated, StateProbation, true, "validated → probation (operator approval)"},
		{StateProbation, StateActive, true, "probation → active (criteria met)"},
		{StateActive, StateDegraded, true, "active → degraded (failure rate exceeds soft threshold)"},
		{StateActive, StateNeedsRevision, true, "active → needs_revision (operator flag)"},
		{StateActive, StateRolledBack, true, "active → rolled_back (observation window breach)"},
		{StateDegraded, StateActive, true, "degraded → active (recovery)"},
		{StateDegraded, StateFractured, true, "degraded → fractured (3-in-60s cluster)"},
		{StateFractured, StateNeedsRevision, true, "fractured → needs_revision (automatic on detection)"},
		{StateNeedsRevision, StateDraft, true, "needs_revision → draft (agent submits fix)"},

		// Illegal jumps — these MUST fail
		{StateDraft, StateActive, false, "draft cannot skip directly to active"},
		{StateLinted, StateProbation, false, "linted cannot skip validated"},
		{StateActive, StateDraft, false, "active has no direct demotion to draft"},
		{StateActive, StateFractured, false, "active must go via degraded to fractured"},
		{StateProbation, StateDraft, false, "probation cannot reverse to draft"},
		{StateFractured, StateActive, false, "fractured must go via needs_revision first"},
		{StateNeedsRevision, StateActive, false, "needs_revision must rewrite as draft first"},
		{StateRetired, StateActive, false, "retired is terminal"},
		{StateRetired, StateDraft, false, "retired is terminal"},
		{StateRolledBack, StateActive, false, "rolled_back is terminal (lineage only)"},
		{StateDraft, StateDraft, false, "no self-transitions"},
		{StateActive, StateActive, false, "no self-transitions"},

		// Wildcard "* → retired" from spec §1.3 last row: any
		// non-terminal state can transition to retired.
		{StateActive, StateRetired, true, "active can retire via supersede"},
		{StateProbation, StateRetired, true, "probation can retire (operator override)"},
		{StateDegraded, StateRetired, true, "degraded can retire (operator override)"},
		{StateNeedsRevision, StateRetired, true, "needs_revision can retire when fix superseded by fork"},
		{StateDraft, StateRetired, true, "draft can retire (proposal withdrawn)"},
		{StateLinted, StateRetired, true, "linted can retire (operator override)"},
		{StateValidated, StateRetired, true, "validated can retire (operator override)"},

		// Unknown states
		{CapabilityState("garbage"), StateActive, false, "unknown from-state"},
		{StateDraft, CapabilityState("garbage"), false, "unknown to-state"},
	}
	for _, tt := range tests {
		got := tt.from.CanTransitionTo(tt.to)
		if got != tt.want {
			t.Errorf("%s.CanTransitionTo(%s) [%s] = %v, want %v",
				tt.from, tt.to, tt.desc, got, tt.want)
		}
	}
}

func TestCapabilityState_Validate(t *testing.T) {
	// Every state in AllCapabilityStates() must Validate cleanly.
	for _, s := range AllCapabilityStates() {
		if err := s.Validate(); err != nil {
			t.Errorf("AllCapabilityStates() returned invalid state %q: %v", s, err)
		}
	}
	// Unknown states must fail.
	if err := CapabilityState("garbage").Validate(); err == nil {
		t.Error("Validate() accepted unknown state; expected error")
	}
	// Empty string must fail.
	if err := CapabilityState("").Validate(); err == nil {
		t.Error("Validate() accepted empty string; expected error")
	}
}

func TestCapabilityState_IsCallable(t *testing.T) {
	callable := map[CapabilityState]bool{
		StateProbation: true,
		StateActive:    true,
		StateDegraded:  true,
	}
	for _, s := range AllCapabilityStates() {
		if got, want := s.IsCallable(), callable[s]; got != want {
			t.Errorf("%s.IsCallable() = %v, want %v", s, got, want)
		}
	}
}

func TestCapabilityState_IsTerminal(t *testing.T) {
	terminal := map[CapabilityState]bool{
		StateRetired:    true,
		StateRolledBack: true,
	}
	for _, s := range AllCapabilityStates() {
		if got, want := s.IsTerminal(), terminal[s]; got != want {
			t.Errorf("%s.IsTerminal() = %v, want %v", s, got, want)
		}
	}
}

func TestExecutionDomain_Validate(t *testing.T) {
	for _, d := range AllExecutionDomains() {
		if err := d.Validate(); err != nil {
			t.Errorf("AllExecutionDomains() returned invalid domain %q: %v", d, err)
		}
	}
	if err := ExecutionDomain("garbage").Validate(); err == nil {
		t.Error("Validate() accepted unknown domain; expected error")
	}
	if err := ExecutionDomain("").Validate(); err == nil {
		t.Error("Validate() accepted empty domain; expected error")
	}
}

func TestEventType_Validate(t *testing.T) {
	for _, e := range AllEventTypes() {
		if err := e.Validate(); err != nil {
			t.Errorf("AllEventTypes() returned invalid event_type %q: %v", e, err)
		}
	}
	if err := EventType("garbage").Validate(); err == nil {
		t.Error("Validate() accepted unknown event_type; expected error")
	}
}

// TestTransitionMatrixCoverage is a defensive check: every state in
// AllCapabilityStates() must appear as a key in validTransitions.
// Catches the case where someone adds a new state constant but
// forgets to wire it into the matrix.
func TestTransitionMatrixCoverage(t *testing.T) {
	for _, s := range AllCapabilityStates() {
		if _, ok := validTransitions[s]; !ok {
			t.Errorf("state %q is in AllCapabilityStates() but missing from validTransitions", s)
		}
	}
}
