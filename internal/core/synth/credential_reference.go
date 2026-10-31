package synth

// credential_reference.go — the distinction between a LITERAL SECRET and
// an ENVIRONMENT-VARIABLE REFERENCE.
//
// THE DEFECT THIS FILE CLOSES
// ----------------------------
// A profile's `api_key` is a LITERAL SECRET. The substrate presents it
// to the provider verbatim. It is not a template, and it was never
// expanded.
//
// An operator who wrote
//
//	"api_key": "$MINIMAX_API_KEY"
//
// therefore had the LITERAL STRING "$MINIMAX_API_KEY" presented to the
// provider as a bearer token — while their real MINIMAX_API_KEY sat in
// the environment, correctly configured, and was never consulted.
//
// The provider refused the request, and the operator was told their
// credential was invalid. It was not invalid. It was SHADOWED by a
// placeholder that looked like a reference and was treated as a secret.
//
// The failure is silent in the worst way: the configuration looks
// correct, the environment is correct, and the only symptom is a
// credential refusal that sends the operator to rotate a working key.
//
// THE RULE
// -------
// A configured credential that has the SHAPE of an environment reference
// is resolved as one:
//
//   - "$NAME" and "${NAME}" are recognised as references to NAME;
//   - if NAME is a credential variable the endpoint DECLARES (see
//     APIKeyEnvVars) and the variable is SET, the environment's value is
//     used;
//   - if NAME is declared but the variable is NOT set, that is reported
//     as what it is — a reference to a variable that is not set;
//   - if NAME is NOT declared, the reference is REFUSED. It is not
//     expanded, and the literal placeholder is never sent to a
//     provider.
//
// A configured value that is not reference-shaped is still a literal
// secret and is still used verbatim. Nothing about a working install
// changes.
//
// WHY THE EXPANSION IS NOT ARBITRARY
// ----------------------------------
// Expanding "$ANYTHING" would let a configuration file name any
// environment variable and cause its value to be sent to a provider.
// That is a new exfiltration path: anything able to write the
// configuration could name a secret, and the operator would never know.
//
// The expansion here is therefore CLOSED over the substrate's own table.
// The only names that resolve are the ones the endpoint would have been
// given by the environment fallback anyway (APIKeyEnvVars). The
// expansion grants no read access the substrate did not already have,
// and it cannot be pointed at an unrelated variable. This is the
// difference between a REFERENCE and an expansion:
//
//	a reference names one of the credentials this endpoint already
//	accepts;
//	an expansion names anything at all.
//
// NO SECRET EVER APPEARS IN A MESSAGE
// -----------------------------------
// An error message about a credential may name a VARIABLE — but only a
// variable the SUBSTRATE declared, never one the operator typed. A name
// the operator typed is not echoed, because a pasted secret can have the
// shape of a variable name, and echoing it would print it.
//
// The resolved secret is never included in an error, a log line, or any
// other output.

import (
	"fmt"
	"os"
	"strings"
)

// CredentialErrorKind classifies a credential-resolution failure, so a
// caller can tell the two shapes apart without parsing the message.
type CredentialErrorKind int

const (
	// CredentialReferenceUnset: the configured credential named an
	// environment variable this endpoint declares, and that variable is
	// not set. The credential is not wrong; the REFERENCE is unresolved.
	CredentialReferenceUnset CredentialErrorKind = iota

	// CredentialReferenceUndeclared: the configured credential named an
	// environment variable this endpoint does NOT declare as a
	// credential variable. It was not expanded.
	CredentialReferenceUndeclared
)

// CredentialError is a credential that could not be resolved.
//
// It carries no secret. `Variable` is populated ONLY with a name the
// substrate itself declared, so it is always safe to print.
type CredentialError struct {
	// Kind says which shape of failure this is.
	Kind CredentialErrorKind

	// Variable is the DECLARED environment variable the reference
	// named, when it named one this endpoint declares. It is empty for
	// an undeclared reference — an operator-supplied name is never
	// echoed.
	Variable string

	// Recognised lists the credential variables this endpoint
	// declares. It is always safe to print: these names come from the
	// substrate's own table.
	Recognised []string
}

// Error renders the failure. The message contains no secret and no
// operator-supplied name.
func (e *CredentialError) Error() string {
	recognised := "none"
	if len(e.Recognised) > 0 {
		recognised = strings.Join(e.Recognised, ", ")
	}
	switch e.Kind {
	case CredentialReferenceUnset:
		return fmt.Sprintf(
			"the profile's api_key is an environment-variable reference to %s, and that variable is not set in the environment; "+
				"unset the profile's api_key to let the environment credential through, or export %s",
			e.Variable, e.Variable)
	default:
		return fmt.Sprintf(
			"the profile's api_key is an environment-variable reference, but it does not name a credential variable for this endpoint "+
				"(the credential variables this endpoint declares are: %s); "+
				"MPM resolves only the variables it declares for the endpoint and does not expand arbitrary environment variables",
			recognised)
	}
}

// EnvReferenceName reports whether a configured credential is an
// environment-variable REFERENCE rather than a literal secret, and
// returns the variable's name.
//
// Recognised shapes:
//
//	"$NAME"
//	"${NAME}"
//
// where NAME is a POSIX environment-variable name: a letter or an
// underscore, followed by letters, digits and underscores.
//
// A value that does not match this shape is a literal secret, however
// much it might look like one.
func EnvReferenceName(configured string) (string, bool) {
	s := strings.TrimSpace(configured)
	if s == "" {
		return "", false
	}
	var name string
	switch {
	case strings.HasPrefix(s, "${") && strings.HasSuffix(s, "}") && len(s) > 3:
		name = s[2 : len(s)-1]
	case strings.HasPrefix(s, "$") && len(s) > 1:
		name = s[1:]
	default:
		return "", false
	}
	if !isEnvVarName(name) {
		return "", false
	}
	return name, true
}

// isEnvVarName reports whether s is a POSIX environment-variable name.
func isEnvVarName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r == '_':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// ResolveCredential resolves the effective credential for a profile and
// reports WHY when it cannot.
//
// The rules, in order:
//
//  1. A configured value that is NOT reference-shaped is a LITERAL
//     SECRET and is used verbatim.
//
//  2. A configured value that IS reference-shaped names an environment
//     variable:
//     a. if the endpoint declares that variable as a credential
//     variable (APIKeyEnvVars) and the variable is SET, the
//     environment's value is used;
//     b. if the endpoint declares it and the variable is NOT set, the
//     resolution fails with a CredentialError that says exactly that;
//     c. if the endpoint does NOT declare it, the resolution fails with
//     a CredentialError that says that — and the reference is NOT
//     expanded.
//
//  3. With no configured value, the endpoint's own credential
//     variables are consulted, in the order APIKeyEnvVars returns.
//
// The returned error NEVER contains the configured value and NEVER
// contains the resolved secret.
func ResolveCredential(configuredKey, baseURL string) (string, error) {
	if configuredKey != "" {
		name, isReference := EnvReferenceName(configuredKey)
		if !isReference {
			// A literal secret. Used verbatim, exactly as before.
			return configuredKey, nil
		}
		declared := APIKeyEnvVars(baseURL)
		for _, candidate := range declared {
			if candidate != name {
				continue
			}
			// The reference names a variable this endpoint declares.
			// This is the ONLY expansion the substrate performs, and it
			// resolves exactly the variable the environment fallback
			// would have used.
			if value := os.Getenv(name); value != "" {
				return value, nil
			}
			return "", &CredentialError{
				Kind:       CredentialReferenceUnset,
				Variable:   name,
				Recognised: declared,
			}
		}
		// The reference names a variable this endpoint does not
		// declare. It is NOT expanded: the name the operator typed is
		// not echoed, and the placeholder is never sent to a provider.
		return "", &CredentialError{
			Kind:       CredentialReferenceUndeclared,
			Recognised: declared,
		}
	}
	for _, name := range APIKeyEnvVars(baseURL) {
		if value := os.Getenv(name); value != "" {
			return value, nil
		}
	}
	return "", nil
}
