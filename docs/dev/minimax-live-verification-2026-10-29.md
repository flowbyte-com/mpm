# MiniMax live-provider verification — issue #60

**Date:** 2026-10-29
**Verified against:** `origin/main` at `517edab6` (`docs: describe the layout #156 landed, not only the one it kept (#171)`)
**Endpoint category:** `https://api.minimax.io/anthropic/v1` — MiniMax's **Anthropic-compatible** surface
**Model:** `MiniMax-M2.7` (the release candidate's own default for a MiniMax profile)
**Credential:** the operator's `MINIMAX_API_KEY`, resolved from the environment by the production resolver. Never printed, logged, copied or persisted.
**Requests made:** **1**

## Outcome

| Field | Value |
|---|---|
| Probe status | `healthy` |
| Authentication scheme presented | `bearer` |
| Authentication header sent | `Authorization: Bearer …` |
| `X-Api-Key` also sent | **no** |
| Request path | `/anthropic/v1/messages` |
| Authentication failure class | *(empty — not a refusal)* |
| Transport error class | *(empty)* |
| Latency | 2793 ms |
| **Requests observed** | **1** |

**Classification: SUCCESS.** The live provider accepted the request. This is
the first time the HAL-9001 Finding 1 fix has been exercised against the real
MiniMax endpoint rather than a hermetic fixture.

The recorded request shape was `path=/anthropic/v1/messages auth_header=Authorization scheme="Bearer" x_api_key_present=false`.

## What each acceptance criterion now has

| # | Criterion | Status | Evidence |
|---|---|---|---|
| 1 | The live MiniMax probe reaches `healthy`, not `auth_failed` | **met** | `probe status: healthy` |
| 2 | The resolved scheme for the MiniMax profile is `bearer` | **met** | `probe auth scheme: bearer`; and offline, `synth.ResolveAuthScheme("https://api.minimax.io/anthropic/v1", …) == bearer` |
| 3 | No cross-origin credential leakage | **met** | The credential was presented to the configured origin and **not** forwarded: the second origin was reached **0 times**. See below. |
| 4 | Ordinary Anthropic authentication is unregressed | **met** | `TestTransport_AnthropicNativePresentsXAPIKey` passes; the offline host-exactness check resolves `api.anthropic.com` to `x-api-key` |
| 5 | Bounded negotiation does not fire in normal operation | **met** | Exactly **1** request reached the provider. A negotiation attempt would have been a second request. |
| 6 | The failure classification still distinguishes causes | **partly met** | The failure taxonomy is exercised by the regression suite, and the healthy run's `auth failure class` and `error class` are both empty as they must be. **Not** live-verified: this run did not present an invalid key (the operator's credential must not be modified, and no second credential was available). See "What is still outstanding". |

## The credential-origin safeguard, examined without the real key

The redirect half of the verification runs entirely against **two local
servers** with a **placeholder** secret, so the operator's credential is
never presented to a second origin.

```
redirect refused             : credential-bearing request refused: Post "https://second.example/anthropic/v1/messages":
                               refusing credential-bearing redirect from https://first.example to https://second.example:
                               the credential would leave the configured origin (a different host — including a subdomain — is a different origin)
first origin saw credential  : true
second origin reached        : 0 time(s)
second origin saw credential : false
```

The demonstration deliberately uses the **Anthropic-style** credential
(`X-Api-Key`), because that is where Go's standard library is dangerous: it
drops `Authorization` on a cross-host redirect but copies `X-Api-Key`
*anywhere*. The guard has to hold for the header the standard library would
have leaked.

The refusal is **typed** (`synth.ErrCredentialRedirectRefused`), not a
transport failure — a transport failure would be retried and reported to the
operator as "the endpoint is unreachable", which is a different and wrong
diagnosis.

## The host-exactness rule, offline

The documented-rule tier matches on the **parsed host** against an exact
domain set, so a lookalike host does not inherit the bearer rule:

| Base URL | Resolved scheme |
|---|---|
| `https://api.minimax.io/anthropic/v1` | `bearer` |
| `https://api.minimaxi.com/anthropic/v1` | `bearer` |
| `https://api.anthropic.com/v1` | `x-api-key` |
| `https://notminimax.io/anthropic/v1` | `x-api-key` |
| `https://minimax.io.example.com/anthropic/v1` | `x-api-key` |

## What is still outstanding

**Issue #60 asks for verification against a *deployed revision*.** This run
verified the **release-candidate source** against the live provider. Nothing
here ran the *installed* binary, and no deployment was performed — deployment
is outside the authority of this pass.

Therefore:

- The **live-provider** criterion is **met** and recorded above.
- The **deployed-revision** criterion is **still open**. It closes when a
  controlled deployment runs the same check — the same opt-in test, against
  the installed build — and records the outcome against that build's
  identity.

**Criterion 6's negative half is also open**: this run did not present an
invalid key, so the live provider's refusal of a bad credential has not been
observed. Doing so would require a second, deliberately invalid credential,
and creating or modifying credentials is explicitly outside this issue's
constraints.

## Reproducing it

```bash
# In a disposable workspace, against the release candidate:
export MINIMAX_API_KEY=…            # the operator's own credential
go test -tags fts5 -count=1 -v \
  -run 'TestLive_MiniMax' ./cmd/mpm/probe/
```

The test is **opt-in**: without `MINIMAX_API_KEY` in the environment it
skips, and it makes no request. It reads the credential through the
production resolver, so the secret never passes through a variable or a
literal in the test, and it records only the request's *shape* — the path,
the header name, the scheme prefix — never the credential.

See `cmd/mpm/probe/minimax_live_verification_test.go`.
