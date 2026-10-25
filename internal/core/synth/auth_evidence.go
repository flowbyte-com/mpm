// auth_evidence.go — the EVIDENCE rule for the bounded authentication
// negotiation.
//
// WHY THIS FILE EXISTS
//
// The bounded negotiation is permitted only when the provider's refusal
// EXPLICITLY names a different authentication header. That is the
// difference between:
//
//   - "I presented the credential in the wrong form" — retrying with the
//     named header may work; and
//   - "your credential is wrong" / "you may not use this model" —
//     retrying with another header cannot help, and repeating a
//     credential-bearing request is itself a cost.
//
// The first implementation of the rule matched a bare substring ANYWHERE
// in the response body. That is much weaker than the stated contract, and
// it made three whole classes of refusal authorise a credential-bearing
// retry:
//
//   - a front-end proxy's own 401 page. `<title>401 Authorization
//     Required</title>` contains "authorization", so the negotiation fired
//     against an nginx or Apache that was never the provider. The
//     provider's opinion about the header was never expressed.
//   - a 403 permission body. `{"error":{"message":"your organization does
//     not have authorization to use model X"}}` fired even though the
//     credential was ACCEPTED and the refusal was about permissions.
//   - an echoed request. `{"received":{"headers":{"authorization":"Bearer
//     ..."}}}` fired on the word alone.
//
// A mention is not a requirement. This file implements the requirement.
//
// THE RULE
//
// Evidence requires BOTH:
//
//  1. the provider's own error MESSAGE — not the whole body. The message
//     is read from the shapes providers actually use (`error.message`,
//     `error` as a string, a top-level `message`, MiniMax's
//     `base_resp.status_msg`). An HTML error page from a front-end proxy
//     carries no provider evidence, and a JSON document that names a
//     header in some other field carries no provider evidence either.
//
//  2. the header named AS THE HEADER THE PROVIDER REQUIRES — a
//     requirement phrase adjacent to the header name ("missing",
//     "requires", "must be", "expected", "carry the … in the … field",
//     "is required"). A bare noun-adjective ("401 Authorization
//     Required") is a front-end page's title, not a requirement, and is
//     not accepted.
//
// The "must differ from what was sent" rule is unchanged: a provider that
// names the header that was already used proposes nothing.
package synth

import (
	"encoding/json"
	"regexp"
	"strings"
)

// evidenceWindow is how far from the header name a requirement phrase may
// sit and still count as evidence.
//
// It is deliberately small. A requirement phrase is a short sentence
// fragment; a wide window would let a requirement aimed at one header
// authorise a retry with another.
const evidenceWindow = 48

// headerRequirementPhrases are the phrases that turn a header MENTION
// into a header REQUIREMENT.
//
// The bare adjective "required" is deliberately NOT here: "401
// Authorization Required" is a front-end page's title, not a statement
// about which header to send. The copula form ("is required", "are
// mandatory") IS accepted — that is a provider stating a requirement.
var headerRequirementPhrases = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bmiss(?:ing|es)\b`),
	regexp.MustCompile(`(?i)\brequir(?:es|e)\b`),
	regexp.MustCompile(`(?i)\bmust\b`),
	regexp.MustCompile(`(?i)\bshould\b`),
	regexp.MustCompile(`(?i)\bexpect(?:s|ed)?\b`),
	regexp.MustCompile(`(?i)\bcarry\b`),
	regexp.MustCompile(`(?i)\babsent\b`),
	regexp.MustCompile(`(?i)\bnot\s+present\b`),
	regexp.MustCompile(`(?i)\binstead\s+of\b`),
	regexp.MustCompile(`(?i)\b(?:is|are|was|were)\s+(?:required|mandatory)\b`),
}

// The two header names a provider can name. Both are matched as whole
// words, so "headers" (a JSON key) and "authorisation" (a misspelling) do
// not match.
var (
	authorizationHeaderPattern = regexp.MustCompile(`(?i)\bauthorization\b`)
	xAPIKeyHeaderPattern       = regexp.MustCompile(`(?i)\bx[-_]api[-_]key\b`)
)

// authMismatchEvidence inspects an authentication-refusal body for
// evidence that the endpoint expected a DIFFERENT authentication header
// than the one that was sent.
//
// Evidence must be EXPLICIT: the provider has to name the header it
// wants, as a requirement. A generic "invalid api key" / "unauthorized"
// body is not evidence — that is an invalid credential, and repeating the
// request with another header would neither help nor be honest.
//
// The proposed scheme is always different from the one that was sent;
// when the body names the header that was already used, no alternative
// is proposed.
func authMismatchEvidence(body []byte, sent AuthScheme) (AuthScheme, bool) {
	message := providerErrorMessage(body)
	if message == "" {
		return "", false
	}
	switch {
	case sent != AuthSchemeBearer && namesHeaderAsRequirement(message, authorizationHeaderPattern):
		return AuthSchemeBearer, true
	case sent != AuthSchemeXAPIKey && namesHeaderAsRequirement(message, xAPIKeyHeaderPattern):
		return AuthSchemeXAPIKey, true
	}
	return "", false
}

// namesHeaderAsRequirement reports whether the message names the header
// AND frames it as something the provider requires.
//
// The header name must be present, and a requirement phrase must sit
// within evidenceWindow characters of it.
func namesHeaderAsRequirement(message string, header *regexp.Regexp) bool {
	locations := header.FindAllStringIndex(message, -1)
	for _, loc := range locations {
		start := loc[0] - evidenceWindow
		if start < 0 {
			start = 0
		}
		end := loc[1] + evidenceWindow
		if end > len(message) {
			end = len(message)
		}
		window := message[start:end]
		for _, phrase := range headerRequirementPhrases {
			if phrase.MatchString(window) {
				return true
			}
		}
	}
	return false
}

// providerErrorMessage extracts the provider's own error message from a
// refusal body.
//
// It returns "" when the body carries no recognisable provider message:
// an HTML error page from a front-end proxy, an echoed request, a JSON
// document whose fields MPM does not recognise, or an empty body.
//
// An empty result is the SAFE result. It means "no evidence", so no
// credential-bearing retry is authorised.
func providerErrorMessage(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ""
	}
	// An HTML error page is a front-end proxy's answer, not the
	// provider's. It carries no provider evidence — and its title is
	// exactly the shape ("401 Authorization Required") that a bare
	// substring match used to misread as a header requirement.
	if looksLikeHTMLPage(trimmed) {
		return ""
	}
	var doc map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &doc); err != nil {
		// Not JSON: the body is the message.
		return trimmed
	}
	return messageFromDocument(doc)
}

// looksLikeHTMLPage reports whether the body is an HTML document.
func looksLikeHTMLPage(body string) bool {
	lower := strings.ToLower(body)
	return strings.HasPrefix(lower, "<!doctype html") ||
		strings.HasPrefix(lower, "<html") ||
		strings.HasPrefix(lower, "<head") ||
		strings.HasPrefix(lower, "<body")
}

// messageFromDocument reads the provider's message out of a decoded JSON
// object.
//
// The recognised shapes are the ones providers actually use:
//
//	{"error":{"message":"..."}}          OpenAI, Anthropic, most gateways
//	{"error":"..."}                      a flat error string
//	{"message":"..."}                    a top-level message
//	{"base_resp":{"status_msg":"..."}}   MiniMax
//
// Anything else — including a document that names a header in some
// unrelated field — yields no message, and therefore no evidence.
func messageFromDocument(doc map[string]interface{}) string {
	if msg := messageFromValue(doc["error"]); msg != "" {
		return msg
	}
	if msg, ok := doc["message"].(string); ok {
		if trimmed := strings.TrimSpace(msg); trimmed != "" {
			return trimmed
		}
	}
	if base, ok := doc["base_resp"].(map[string]interface{}); ok {
		if msg, ok := base["status_msg"].(string); ok {
			if trimmed := strings.TrimSpace(msg); trimmed != "" {
				return trimmed
			}
		}
	}
	if msg, ok := doc["status_msg"].(string); ok {
		if trimmed := strings.TrimSpace(msg); trimmed != "" {
			return trimmed
		}
	}
	if msg, ok := doc["detail"].(string); ok {
		if trimmed := strings.TrimSpace(msg); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// messageFromValue reads a message out of an `error` field, which may be
// a string or an object.
func messageFromValue(value interface{}) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case map[string]interface{}:
		for _, key := range []string{"message", "status_msg", "detail"} {
			if msg, ok := typed[key].(string); ok {
				if trimmed := strings.TrimSpace(msg); trimmed != "" {
					return trimmed
				}
			}
		}
	}
	return ""
}
