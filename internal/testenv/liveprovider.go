package testenv

// liveprovider.go — the sanctioned gate for a test that reaches a REAL
// provider.
//
// # Why this exists
//
// A test that makes a request to a live generative provider is not hermetic:
// its result depends on the provider's response, on the network, and on the
// operator's credential. Worse, it is not merely non-hermetic — it can cost
// money and it can present a credential to a third party.
//
// The substrate had one such test:
// `cmd/mpm/probe/minimax_live_verification_test.go`
// (`TestLive_MiniMaxAnthropicSurfaceAuthenticatesWithBearer`). It was gated
// on `MINIMAX_API_KEY` being present:
//
//	if os.Getenv("MINIMAX_API_KEY") == "" {
//	    t.Skip("MINIMAX_API_KEY is not set; the live provider verification is opt-in")
//	}
//
// That gate is not an opt-in. The presence of a credential is a property of
// the OPERATOR'S ENVIRONMENT, not a decision anyone made about this test run.
// So on any host where the credential happens to be exported — which is the
// normal state of a machine that uses the provider — `make test` and
// `make test-race` silently reached `api.minimax.io` and made a real,
// authenticated, paid request.
//
// That is the defect this file closes: **the credential's presence is
// necessary but NOT sufficient.** A live-provider test runs only when the
// operator has ALSO said "yes, run the live verification" for this run.
//
// # The contract
//
// Use LiveProviderEnabled for every test that can reach a real provider:
//
//	func TestLive_Something(t *testing.T) {
//	    if !testenv.LiveProviderEnabled(t) {
//	        t.Skip("live provider verification is not enabled for this run")
//	    }
//	    ...
//	}
//
// LiveProviderEnabled requires BOTH:
//
//  1. the explicit opt-in variable (LiveProviderOptInEnv), set to a truthy
//     value; and
//  2. at least one provider credential in the environment.
//
// Requirement (1) is the one that matters: it is a decision about the RUN,
// not an accident of the machine. Requirement (2) is kept because a live
// verification with no credential is not a verification — it would fail for a
// reason that has nothing to do with the code under test.
//
// # Why the credential is not enough, and the failure it produced
//
// The old gate produced a real, observed failure. On a credentialed host the
// live probe ran against the real provider and the provider returned a
// thinking-only response, so the test failed — for a reason that had nothing
// to do with the change under test. The same test failed on pristine
// `origin/main` with a different failure mode (a 12-second transport
// timeout), which is what proved the failure was environmental rather than
// caused by the change.
//
// A test whose outcome the provider decides is not a gate. It is a coin toss
// with a network round-trip.
//
// # The opt-in variable
//
// MPM_TEST_LIVE_PROVIDER — set it to 1 (or any non-empty value other than
// "0"/"false") to run the live verifications for one invocation. It is
// deliberately NOT named after a provider: the point is that the operator is
// choosing to reach THE LIVE PROVIDER, whichever provider it is.
//
// CI does not set it, so CI never reaches the network — and that is now a
// property of the test suite rather than a property of CI's environment.

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// LiveProviderOptInEnv is the environment variable that explicitly opts a
// test run into live-provider verification.
//
// It is unset by default. Set it to "1", "true", "yes" or "on" to enable.
const LiveProviderOptInEnv = "MPM_TEST_LIVE_PROVIDER"

// providerCredentialKeys are the environment variables that can supply a
// provider CREDENTIAL.
//
// This is deliberately NOT the providerKeys list above: that list also names
// non-credential variables (base URLs, model names, endpoints), and one of
// those being set must not count as "a credential is present".
var providerCredentialKeys = []string{
	"OPENAI_API_KEY",
	"ANTHROPIC_API_KEY",
	"MINIMAX_API_KEY",
	"GOOGLE_API_KEY",
	"GEMINI_API_KEY",
	"XAI_API_KEY",
	"MISTRAL_API_KEY",
	"COHERE_API_KEY",
	"OPENROUTER_API_KEY",
	"OAI_COMPAT_API_KEY",
}

// liveProviderOptIn reports whether the explicit opt-in variable is set to a
// truthy value.
//
// It is separate from LiveProviderEnabled so the guard test can assert the
// two requirements — the decision and the credential — independently.
func liveProviderOptIn() bool {
	raw, ok := os.LookupEnv(LiveProviderOptInEnv)
	if !ok {
		return false
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	// A boolean-shaped value is interpreted as a boolean, so "0" and
	// "false" disable and "1"/"true" enable. Anything else is enabled by
	// being non-empty: an operator who exports a value means "yes".
	if b, err := strconv.ParseBool(raw); err == nil {
		return b
	}
	return true
}

// LiveProviderEnabled reports whether this run may reach a real provider.
//
// It is true only when the explicit opt-in variable is set AND at least one
// provider credential is present in the environment. The credential alone is
// NOT sufficient — see the file comment.
func LiveProviderEnabled(t *testing.T) bool {
	t.Helper()
	return LiveProviderEnabledForKeys(t, providerCredentialKeys...)
}

// LiveProviderEnabledForKeys is LiveProviderEnabled with an explicit list of
// credential environment variables.
//
// It exists so the guard test can assert the two requirements
// independently, and so a caller that knows which provider it needs can name
// exactly that provider's variable.
func LiveProviderEnabledForKeys(t *testing.T, keys ...string) bool {
	t.Helper()
	if !liveProviderOptIn() {
		return false
	}
	for _, k := range keys {
		if os.Getenv(k) != "" {
			return true
		}
	}
	return false
}
