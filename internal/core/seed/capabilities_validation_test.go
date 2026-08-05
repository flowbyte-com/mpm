package seed

import (
	"strings"
	"testing"
)

// =============================================================================
// capabilities_validation_test.go — CS-1.3 unit tests
//
// Pins the validation rules that the SeedCapability struct
// enforces. A future patch that loosens a rule (e.g., allows
// uppercase names) must update these tests too, so the
// contract change is visible.
//
// Coverage:
//
//   * SavedID — happy path, rejects whitespace, uppercase,
//     colons, empty source.
//   * Validate — rejects invalid source_language, invalid
//     requested_domain, empty purpose, empty source_code.
//   * ContentHash — stable, distinct on source change.
//   * SeedCapabilitiesByName / ByStableID — happy path,
//     unknown name returns false.
// =============================================================================

// validSeedCapability is the canonical "happy path" entry
// used by tests that need a structurally sound SeedCapability.
// Mirrors the Tier 1 primitives in the registry; tests that
// mutate one field start from this.
func validSeedCapability() SeedCapability {
	return SeedCapability{
		StableID:        "cap-seed-test",
		Name:            "test_capability",
		Purpose:         "A test primitive for unit tests.",
		SourceLanguage:  "bash",
		RequestedDomain: "sandbox",
		Tags:            []string{"capability", "test"},
		SourceCode:      "#!/bin/bash\necho hello\n",
	}
}

// =============================================================================
// SavedID tests
// =============================================================================

func TestSavedID_HappyPath(t *testing.T) {
	sc := validSeedCapability()
	got, err := sc.SavedID()
	if err != nil {
		t.Fatalf("SavedID: %v", err)
	}
	if got != "cap.test_capability" {
		t.Fatalf("expected cap.test_capability, got %q", got)
	}
}

func TestSavedID_RejectsEmptyName(t *testing.T) {
	sc := validSeedCapability()
	sc.Name = ""
	_, err := sc.SavedID()
	if err == nil {
		t.Fatal("expected error for empty name, got nil")
	}
	if !strings.Contains(err.Error(), "name") {
		t.Fatalf("expected error mentioning name, got: %v", err)
	}
}

func TestSavedID_RejectsWhitespaceName(t *testing.T) {
	sc := validSeedCapability()
	sc.Name = "has spaces"
	_, err := sc.SavedID()
	if err == nil {
		t.Fatal("expected error for whitespace in name, got nil")
	}
}

func TestSavedID_RejectsUppercaseName(t *testing.T) {
	sc := validSeedCapability()
	sc.Name = "Has_Uppercase"
	_, err := sc.SavedID()
	if err == nil {
		t.Fatal("expected error for uppercase name, got nil")
	}
}

func TestSavedID_RejectsColonName(t *testing.T) {
	sc := validSeedCapability()
	sc.Name = "has:colon"
	_, err := sc.SavedID()
	if err == nil {
		t.Fatal("expected error for colon in name, got nil")
	}
}

func TestSavedID_RejectsEmptySource(t *testing.T) {
	sc := validSeedCapability()
	sc.SourceCode = ""
	_, err := sc.SavedID()
	if err == nil {
		t.Fatal("expected error for empty source, got nil")
	}
}

func TestSavedID_AcceptsDotsUnderscoresDashes(t *testing.T) {
	for _, name := range []string{"a.b", "a_b", "a-b", "abc123", "v1.0.0"} {
		sc := validSeedCapability()
		sc.Name = name
		got, err := sc.SavedID()
		if err != nil {
			t.Errorf("name %q: unexpected error: %v", name, err)
			continue
		}
		if got != "cap."+name {
			t.Errorf("name %q: expected cap.%s, got %q", name, name, got)
		}
	}
}

// =============================================================================
// Validate tests
// =============================================================================

func TestValidate_HappyPath(t *testing.T) {
	sc := validSeedCapability()
	if err := sc.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidate_RejectsInvalidSourceLanguage(t *testing.T) {
	sc := validSeedCapability()
	sc.SourceLanguage = "ruby"
	if err := sc.Validate(); err == nil {
		t.Fatal("expected error for invalid source_language")
	}
}

func TestValidate_AcceptsAllValidLanguages(t *testing.T) {
	for _, lang := range []string{"bash", "python", "jq"} {
		sc := validSeedCapability()
		sc.SourceLanguage = lang
		if err := sc.Validate(); err != nil {
			t.Errorf("language %q: unexpected error: %v", lang, err)
		}
	}
}

func TestValidate_RejectsInvalidRequestedDomain(t *testing.T) {
	sc := validSeedCapability()
	sc.RequestedDomain = "super-trusted"
	if err := sc.Validate(); err == nil {
		t.Fatal("expected error for invalid requested_domain")
	}
}

func TestValidate_RejectsEmptyPurpose(t *testing.T) {
	sc := validSeedCapability()
	sc.Purpose = ""
	if err := sc.Validate(); err == nil {
		t.Fatal("expected error for empty purpose")
	}
}

// =============================================================================
// ContentHash tests
// =============================================================================

func TestContentHash_Stable(t *testing.T) {
	sc := validSeedCapability()
	h1 := sc.ContentHash()
	h2 := sc.ContentHash()
	if h1 != h2 {
		t.Fatalf("ContentHash not stable: %s vs %s", h1, h2)
	}
	if len(h1) != 64 { // SHA-256 hex = 64 chars
		t.Fatalf("expected 64-char hex SHA-256, got %d chars (%q)", len(h1), h1)
	}
}

func TestContentHash_DistinctOnSourceChange(t *testing.T) {
	sc1 := validSeedCapability()
	sc2 := validSeedCapability()
	sc2.SourceCode = "#!/bin/bash\necho goodbye\n"
	if sc1.ContentHash() == sc2.ContentHash() {
		t.Fatal("expected distinct hashes for distinct source")
	}
}

func TestContentHash_DistinctOnOtherFields(t *testing.T) {
	// Changing metadata / tags / purpose should NOT change
	// the hash — only source_code is part of the fingerprint.
	sc1 := validSeedCapability()
	sc2 := validSeedCapability()
	sc2.Purpose = "different purpose"
	sc2.Tags = []string{"different"}
	sc2.Metadata = map[string]string{"foo": "bar"}
	if sc1.ContentHash() != sc2.ContentHash() {
		t.Fatal("expected identical hashes when source_code is unchanged")
	}
}

// =============================================================================
// Lookup tests
// =============================================================================

func TestSeedCapabilitiesByName_Found(t *testing.T) {
	// Use a known Tier 1 name.
	got, ok := SeedCapabilitiesByName("list_capabilities")
	if !ok {
		t.Fatal("expected to find list_capabilities in registry")
	}
	if got.Purpose == "" {
		t.Fatal("expected non-empty Purpose on registry hit")
	}
}

func TestSeedCapabilitiesByName_Missing(t *testing.T) {
	_, ok := SeedCapabilitiesByName("nonexistent_capability")
	if ok {
		t.Fatal("expected ok=false for unknown name")
	}
}

func TestSeedCapabilitiesByStableID_Found(t *testing.T) {
	got, ok := SeedCapabilitiesByStableID("cap-seed-list-capabilities")
	if !ok {
		t.Fatal("expected to find cap-seed-list-capabilities in registry")
	}
	if got.Name != "list_capabilities" {
		t.Fatalf("stable_id lookup returned wrong entry: %q", got.Name)
	}
}

func TestSeedCapabilitiesByStableID_Missing(t *testing.T) {
	_, ok := SeedCapabilitiesByStableID("cap-seed-nonexistent")
	if ok {
		t.Fatal("expected ok=false for unknown stable_id")
	}
}

// =============================================================================
// Registry integrity tests
// =============================================================================

// TestRegistry_NoDuplicateNames pins that every SeedCapabilities
// entry has a unique name (SavedID uniqueness). A duplicate would
// cause INSERT OR IGNORE to silently skip — surfacing the
// ambiguity at seed init is better than letting the second
// insert vanish.
func TestRegistry_NoDuplicateNames(t *testing.T) {
	seen := make(map[string]string) // name → stable_id
	for _, sc := range SeedCapabilities {
		if prev, dup := seen[sc.Name]; dup {
			t.Fatalf("duplicate name %q in registry (stable_ids: %q, %q)",
				sc.Name, prev, sc.StableID)
		}
		seen[sc.Name] = sc.StableID
	}
}

func TestRegistry_NoDuplicateStableIDs(t *testing.T) {
	seen := make(map[string]bool)
	for _, sc := range SeedCapabilities {
		if seen[sc.StableID] {
			t.Fatalf("duplicate stable_id %q in registry", sc.StableID)
		}
		seen[sc.StableID] = true
	}
}

func TestRegistry_AllEntriesValidate(t *testing.T) {
	// Every shipped Tier 1 primitive must pass its own
	// validation. Caught at test time (not at first seed)
	// so a future patch that breaks the registry is loud.
	for _, sc := range SeedCapabilities {
		if err := sc.Validate(); err != nil {
			t.Errorf("SeedCapability %q failed Validate: %v", sc.StableID, err)
		}
		if _, err := sc.SavedID(); err != nil {
			t.Errorf("SeedCapability %q failed SavedID: %v", sc.StableID, err)
		}
	}
}

func TestRegistry_AllEntriesAreSandbox(t *testing.T) {
	// Tier 1 is "read-only primitive" — every shipped
	// primitive MUST be sandbox domain. A future patch that
	// ships a restricted/trusted/operator entry in Tier 1
	// is a contract violation; this test fails first.
	for _, sc := range SeedCapabilities {
		if sc.RequestedDomain != "sandbox" {
			t.Errorf("Tier 1 primitive %q has domain=%q (must be sandbox)",
				sc.StableID, sc.RequestedDomain)
		}
	}
}

func TestRegistry_AllEntriesHaveTier1Metadata(t *testing.T) {
	for _, sc := range SeedCapabilities {
		if sc.Metadata["tier"] != "1" {
			t.Errorf("Tier 1 primitive %q has metadata.tier=%q (must be \"1\")",
				sc.StableID, sc.Metadata["tier"])
		}
		if sc.Metadata["primitive"] != "true" {
			t.Errorf("Tier 1 primitive %q has metadata.primitive=%q (must be \"true\")",
				sc.StableID, sc.Metadata["primitive"])
		}
		if sc.Metadata["risk_class"] == "" {
			t.Errorf("Tier 1 primitive %q has empty metadata.risk_class", sc.StableID)
		}
	}
}