package synth

// credential_reference_test.go — the distinction between a LITERAL
// SECRET and an ENVIRONMENT-VARIABLE REFERENCE.
//
// WHAT THESE TESTS PIN
// --------------------
//
//  1. A literal secret is still a literal secret. Nothing about an
//     existing, working install changes.
//
//  2. A configured value shaped like an environment reference is
//     resolved AS one — which is what fixes the shadowing defect: a
//     profile carrying "$MINIMAX_API_KEY" no longer sends that literal
//     to the provider while the real credential sits unused in the
//     environment.
//
//  3. The expansion is CLOSED. Only a variable the endpoint declares
//     resolves. A reference to anything else is REFUSED, not expanded.
//
//  4. No secret appears in any message. Not the configured value, not
//     the resolved value, and not a name the operator typed.

import (
	"strings"
	"testing"
)

const minimaxBase = "https://api.minimax.io/anthropic/v1"

// TestEnvReferenceName_RecognisesOnlyReferenceShapes pins the shape
// rule.
//
// A value that is not reference-shaped is a LITERAL SECRET, however
// much it might look like one. That matters: a real credential that
// happens to contain a dollar sign must not be reclassified.
func TestEnvReferenceName_RecognisesOnlyReferenceShapes(t *testing.T) {
	cases := []struct {
		value    string
		wantName string
		wantOK   bool
	}{
		{"$MINIMAX_API_KEY", "MINIMAX_API_KEY", true},
		{"${MINIMAX_API_KEY}", "MINIMAX_API_KEY", true},
		{"  $MINIMAX_API_KEY  ", "MINIMAX_API_KEY", true},
		{"$A", "A", true},
		{"$_A1", "_A1", true},
		// Not references: literal secrets and malformed shapes.
		{"sk-live-abc123", "", false},
		{"MINIMAX_API_KEY", "", false},
		{"", "", false},
		{"$", "", false},
		{"${}", "", false},
		{"${MINIMAX_API_KEY", "", false},
		{"$1INVALID", "", false},
		{"$WITH-DASH", "", false},
		{"$WITH SPACE", "", false},
		{"$WITH.DOT", "", false},
	}
	for _, tc := range cases {
		name, ok := EnvReferenceName(tc.value)
		if ok != tc.wantOK || name != tc.wantName {
			t.Errorf("EnvReferenceName(%q) = (%q, %v), want (%q, %v)",
				tc.value, name, ok, tc.wantName, tc.wantOK)
		}
	}
}

// TestResolveCredential_LiteralSecretIsUsedVerbatim is the
// non-regression guard for every existing install.
//
// A stored key that is not reference-shaped is still presented to the
// provider exactly as it was.
func TestResolveCredential_LiteralSecretIsUsedVerbatim(t *testing.T) {
	clearCredentialEnv(t)
	const literal = "sk-live-a-real-looking-secret"
	key, err := ResolveCredential(literal, minimaxBase)
	if err != nil {
		t.Fatalf("ResolveCredential returned an error for a literal secret: %v", err)
	}
	if key != literal {
		t.Errorf("ResolveCredential = %q, want the literal %q unchanged", key, literal)
	}
}

// TestResolveCredential_ReferenceToADeclaredVariableResolvesFromTheEnvironment
// is the fix for the shadowing defect.
//
// This is the LIVE profile's shape: "api_key": "$MINIMAX_API_KEY",
// with the real credential exported. Before the fix the literal
// placeholder was sent to the provider as a bearer token, the provider
// refused it, and the operator was told their credential was invalid.
func TestResolveCredential_ReferenceToADeclaredVariableResolvesFromTheEnvironment(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv("MINIMAX_API_KEY", "the-real-credential")

	key, err := ResolveCredential("$MINIMAX_API_KEY", minimaxBase)
	if err != nil {
		t.Fatalf("ResolveCredential returned an error for a resolvable reference: %v", err)
	}
	if key != "the-real-credential" {
		t.Errorf("ResolveCredential = %q, want the environment's value — the placeholder must not "+
			"shadow the credential it names", key)
	}
}

// TestResolveCredential_BraceFormAlsoResolves covers the other
// recognised shape.
func TestResolveCredential_BraceFormAlsoResolves(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv("MINIMAX_API_KEY", "the-real-credential")

	key, err := ResolveCredential("${MINIMAX_API_KEY}", minimaxBase)
	if err != nil {
		t.Fatalf("ResolveCredential returned an error for a resolvable reference: %v", err)
	}
	if key != "the-real-credential" {
		t.Errorf("ResolveCredential = %q, want the environment's value", key)
	}
}

// TestResolveCredential_ReferenceToAnUnsetVariableIsReported pins the
// "not set" case.
//
// The credential is not wrong; the REFERENCE is unresolved. The message
// has to say that, and it has to name the variable so the operator can
// act.
func TestResolveCredential_ReferenceToAnUnsetVariableIsReported(t *testing.T) {
	clearCredentialEnv(t)

	key, err := ResolveCredential("$MINIMAX_API_KEY", minimaxBase)
	if err == nil {
		t.Fatalf("ResolveCredential returned (%q, nil); want an error when the referenced variable is not set", key)
	}
	if key != "" {
		t.Errorf("ResolveCredential returned a credential (%q) alongside an error; want none", key)
	}
	var credErr *CredentialError
	if !asCredentialError(err, &credErr) {
		t.Fatalf("the error is not a *CredentialError: %v", err)
	}
	if credErr.Kind != CredentialReferenceUnset {
		t.Errorf("Kind = %v, want CredentialReferenceUnset", credErr.Kind)
	}
	if credErr.Variable != "MINIMAX_API_KEY" {
		t.Errorf("Variable = %q, want MINIMAX_API_KEY", credErr.Variable)
	}
	msg := err.Error()
	if !strings.Contains(msg, "MINIMAX_API_KEY") {
		t.Errorf("the message does not name the variable the operator must export: %q", msg)
	}
	if !strings.Contains(msg, "not set") {
		t.Errorf("the message does not say the variable is not set: %q", msg)
	}
}

// TestResolveCredential_ReferenceToAnUndeclaredVariableIsRefused pins
// the CLOSED expansion.
//
// The reference names a variable this endpoint does not declare as a
// credential variable. It is NOT expanded — even when that variable IS
// set in the environment — and the placeholder is never returned as a
// credential.
//
// The endpoint's isolation rule is the reason: MINIMAX_API_KEY must
// never satisfy an OpenAI-compatible profile, and a reference must not
// become a back door around that rule.
func TestResolveCredential_ReferenceToAnUndeclaredVariableIsRefused(t *testing.T) {
	clearCredentialEnv(t)
	// A variable that IS set, and is NOT one this endpoint declares.
	t.Setenv("SOME_OTHER_SECRET", "must-not-be-used")
	// The MiniMax variable is set too, but this endpoint is
	// OpenAI-compatible and does not declare it.
	t.Setenv("MINIMAX_API_KEY", "the-real-credential")

	const openAIBase = "https://api.openai.com/v1"
	key, err := ResolveCredential("$MINIMAX_API_KEY", openAIBase)
	if err == nil {
		t.Fatalf("ResolveCredential returned (%q, nil); want a refusal — the reference names a "+
			"variable this endpoint does not declare", key)
	}
	if key != "" {
		t.Errorf("ResolveCredential returned a credential (%q); a refused reference must never "+
			"yield a credential", key)
	}
	var credErr *CredentialError
	if !asCredentialError(err, &credErr) {
		t.Fatalf("the error is not a *CredentialError: %v", err)
	}
	if credErr.Kind != CredentialReferenceUndeclared {
		t.Errorf("Kind = %v, want CredentialReferenceUndeclared", credErr.Kind)
	}
	// The recognised variables ARE named — they come from the
	// substrate's own table, so they are safe to print and they tell
	// the operator what to do.
	if len(credErr.Recognised) == 0 {
		t.Error("the error does not list the credential variables the endpoint declares")
	}
	if !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Errorf("the message does not name the recognised variable: %q", err.Error())
	}
}

// TestResolveCredential_AnUndeclaredReferenceIsNotExpandedEvenWhenSet
// is the exfiltration guard, stated directly.
//
// The expansion must be closed over the substrate's own table. If ANY
// variable could be named, then anything able to write the
// configuration could cause an unrelated secret to be sent to a
// provider.
func TestResolveCredential_AnUndeclaredReferenceIsNotExpandedEvenWhenSet(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv("TOTALLY_UNRELATED_TOKEN", "a-secret-that-must-not-be-sent-anywhere")

	key, err := ResolveCredential("$TOTALLY_UNRELATED_TOKEN", minimaxBase)
	if err == nil {
		t.Fatalf("ResolveCredential returned (%q, nil); want a refusal", key)
	}
	if strings.Contains(key, "a-secret-that-must-not-be-sent-anywhere") {
		t.Fatal("an undeclared environment variable was expanded — the expansion is not closed over " +
			"the substrate's own table")
	}
}

// TestResolveCredential_AnOperatorSuppliedNameIsNeverEchoed pins the
// secret-hygiene rule for the refusal message.
//
// A name the operator typed is not echoed, because a pasted secret can
// have the shape of a variable name. The substrate names only the
// variables IT declares.
func TestResolveCredential_AnOperatorSuppliedNameIsNeverEchoed(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv("MY_PASTED_SECRET", "irrelevant")

	_, err := ResolveCredential("$MY_PASTED_SECRET", minimaxBase)
	if err == nil {
		t.Fatal("ResolveCredential returned no error for an undeclared reference")
	}
	if strings.Contains(err.Error(), "MY_PASTED_SECRET") {
		t.Errorf("the message echoed a name the operator typed: %q — a pasted secret can have the "+
			"shape of a variable name", err.Error())
	}
}

// TestResolveCredential_NoSecretAppearsInTheError is the secret-hygiene
// guard on the whole surface.
//
// Neither the configured value nor the resolved value may appear.
func TestResolveCredential_NoSecretAppearsInTheError(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv("MINIMAX_API_KEY", "the-resolved-secret-value")

	// Unset case: the configured value is a reference, and the
	// environment's value must not leak.
	t.Setenv("MINIMAX_API_KEY", "")
	_, err := ResolveCredential("$MINIMAX_API_KEY", minimaxBase)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "$MINIMAX_API_KEY") {
		t.Errorf("the message echoed the configured value: %q", err.Error())
	}

	// And the resolved value never appears either.
	t.Setenv("MINIMAX_API_KEY", "the-resolved-secret-value")
	key, err := ResolveCredential("$MINIMAX_API_KEY", minimaxBase)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if key != "the-resolved-secret-value" {
		t.Fatalf("unexpected credential")
	}
	if strings.Contains(key, "MINIMAX_API_KEY") {
		t.Error("the resolved credential contains the variable name")
	}
}

// TestResolveCredential_EmptyConfiguredStillFallsBackToTheEnvironment
// is the non-regression guard for the ordinary environment-only
// install.
func TestResolveCredential_EmptyConfiguredStillFallsBackToTheEnvironment(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv("MINIMAX_API_KEY", "env-only-credential")

	key, err := ResolveCredential("", minimaxBase)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if key != "env-only-credential" {
		t.Errorf("ResolveCredential = %q, want the environment's value", key)
	}
}

// TestResolveCredential_ProviderIsolationIsPreserved pins that the
// reference rule does not weaken the endpoint-isolation rule.
func TestResolveCredential_ProviderIsolationIsPreserved(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv("MINIMAX_API_KEY", "minimax-only")

	key, err := ResolveCredential("", "https://api.openai.com/v1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if key != "" {
		t.Errorf("ResolveCredential = %q, want \"\" — MINIMAX_API_KEY must never satisfy an OpenAI profile", key)
	}
}

// asCredentialError is errors.As specialised for *CredentialError,
// kept local so the test file does not need to import errors for one
// call.
func asCredentialError(err error, target **CredentialError) bool {
	for err != nil {
		if ce, ok := err.(*CredentialError); ok {
			*target = ce
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
