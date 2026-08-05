package capability

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// =============================================================================
// forge_payload.go — propose_skill payload shape (spec §3.1)
//
// ForgePayload is the JSON shape an agent sends to `mpm skill propose`.
// The Forge layer parses it (FG-1), validates every field per the
// contracts in §3.1.1, then converts to the Store's Proposal struct
// (which only sees the validated, structurally-sound subset).
//
// Validation is field-level so rejections can point to the exact
// offending field with a structured error (mirrors spec §3.1.2).
// =============================================================================

// ForgePayload matches the spec §3.1 JSON shape. Field-level
// validation happens in Payload.Validate(); conversion to Store.Proposal
// happens in Payload.ToProposal().
type ForgePayload struct {
	Name           string   `json:"name"`
	Purpose        string   `json:"purpose"`
	SourceCode     string   `json:"source_code"`
	SourceLanguage string   `json:"source_language"`
	RequestedDomain string  `json:"requested_domain"`
	Tags           []string `json:"tags,omitempty"`

	DependsOn     []string `json:"depends_on,omitempty"`
	AuthorTheoryID *string `json:"author_theory_id,omitempty"`

	// ReplacesID is the fork-flow escape hatch from §3.6. When set,
	// the dedup check (FG-5) bypasses similarity matching against
	// this specific capability only — all others still match.
	// The Forge sets created_from_id = replaces_id so the lineage
	// walker can revive the replaced tool on rollback.
	ReplacesID *string `json:"replaces_id,omitempty"`

	// ExpectedIO is an optional contract declaration. Not enforced
	// by the Forge today (no runtime contract tester yet); recorded
	// in metadata for the Probation window's empirical comparison.
	ExpectedIO *ExpectedIO `json:"expected_io,omitempty"`

	// Metadata is free-form JSON. Survives round-trip on the
	// capabilities row.
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

// ExpectedIO is the optional I/O contract declaration. Captured at
// proposal; future probation tooling can compare actual stdout shape
// and exit codes against the declared contract.
type ExpectedIO struct {
	Args        string            `json:"args,omitempty"`
	StdoutShape string            `json:"stdout_shape,omitempty"`
	ExitCodes   map[string]string `json:"exit_codes,omitempty"`
}

// Allowed source languages. Adding a new language requires a new
// linter integration (FG-3) and a new dry-run wrapper (FG-7) — the
// set is deliberately small.
var allowedLanguages = map[string]bool{
	"bash":   true,
	"python": true,
	"jq":     true,
}

// Allowed requested domains. Per spec §3.1.1 row 4, agents may NEVER
// self-propose operator — operator domain is reserved for human
// promotion via `mpm skill elevate`.
var allowedRequestableDomains = map[ExecutionDomain]bool{
	DomainSandbox:    true,
	DomainRestricted: true,
	DomainTrusted:    true, // allowed at proposal; rejected by draft policy below
	DomainOperator:   false,
}

// nameSlug matches the slug pattern in spec §3.1.1 row 1:
// `^[a-z][a-z0-9_]{2,63}$` (3-64 chars total).
var nameSlug = regexp.MustCompile(`^[a-z][a-z0-9_]{2,63}$`)

// FieldError is a structured rejection (spec §3.1.2). The Forge
// accumulates these during Validate(); the CLI/MCP layer renders
// them as a `reasons` array.
type FieldError struct {
	Step    string `json:"step"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
	Line    int    `json:"line,omitempty"`
	Snippet string `json:"snippet,omitempty"`
}

func (e FieldError) Error() string {
	if e.Field == "" {
		return fmt.Sprintf("%s: %s", e.Step, e.Message)
	}
	return fmt.Sprintf("%s: field=%s: %s", e.Step, e.Field, e.Message)
}

// PayloadValidationError bundles multiple FieldErrors. Implements
// the errors.Is / errors.As patterns so handlers can branch on the
// aggregate type.
type PayloadValidationError struct {
	Errors []FieldError
}

func (e *PayloadValidationError) Error() string {
	if len(e.Errors) == 1 {
		return e.Errors[0].Error()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "proposal rejected with %d field errors:", len(e.Errors))
	for _, fe := range e.Errors {
		fmt.Fprintf(&b, "\n  - %s", fe.Error())
	}
	return b.String()
}

// Validate runs the §3.1.1 field contracts in order. Returns nil for
// a valid payload, *PayloadValidationError otherwise. Always inspects
// every field (not fail-fast) so the agent sees ALL problems at once.
func (p *ForgePayload) Validate() error {
	var errs []FieldError

	// name
	if strings.TrimSpace(p.Name) == "" {
		errs = append(errs, FieldError{Step: "schema", Field: "name", Message: "name is required"})
	} else if !nameSlug.MatchString(p.Name) {
		errs = append(errs, FieldError{
			Step:    "schema",
			Field:   "name",
			Message: "name must match ^[a-z][a-z0-9_]{2,63}$",
		})
	}

	// purpose
	if l := len(p.Purpose); l < 20 {
		errs = append(errs, FieldError{
			Step: "schema", Field: "purpose",
			Message: fmt.Sprintf("purpose is too short (%d chars, minimum 20)", l),
		})
	} else if l > 500 {
		errs = append(errs, FieldError{
			Step: "schema", Field: "purpose",
			Message: fmt.Sprintf("purpose is too long (%d chars, maximum 500)", l),
		})
	}

	// source_code
	if p.SourceCode == "" {
		errs = append(errs, FieldError{Step: "schema", Field: "source_code", Message: "source_code is required"})
	} else if int64(len(p.SourceCode)) > MaxSourceBytes {
		errs = append(errs, FieldError{
			Step: "schema", Field: "source_code",
			Message: fmt.Sprintf("source_code exceeds %d bytes", MaxSourceBytes),
		})
	} else if lines := strings.Count(p.SourceCode, "\n") + 1; lines > MaxSourceLines {
		errs = append(errs, FieldError{
			Step: "schema", Field: "source_code",
			Message: fmt.Sprintf("source_code exceeds %d lines", MaxSourceLines),
		})
	}

	// source_language
	if p.SourceLanguage == "" {
		errs = append(errs, FieldError{Step: "schema", Field: "source_language", Message: "source_language is required"})
	} else if !allowedLanguages[p.SourceLanguage] {
		errs = append(errs, FieldError{
			Step: "schema", Field: "source_language",
			Message: fmt.Sprintf("source_language %q not supported (allowed: bash, python, jq)", p.SourceLanguage),
		})
	}

	// requested_domain
	if p.RequestedDomain == "" {
		errs = append(errs, FieldError{Step: "schema", Field: "requested_domain", Message: "requested_domain is required"})
	} else {
		dom := ExecutionDomain(p.RequestedDomain)
		if err := dom.Validate(); err != nil {
			errs = append(errs, FieldError{Step: "schema", Field: "requested_domain", Message: err.Error()})
		} else if !allowedRequestableDomains[dom] {
			// Operator domain cannot be self-proposed.
			errs = append(errs, FieldError{
				Step: "schema", Field: "requested_domain",
				Message: "agents may not self-propose operator domain (use mpm skill elevate)",
			})
		}
	}

	// depends_on — basic structural check. Liveness is the Store's
	// concern (FG-6), not the Forge payload validator.
	if len(p.DependsOn) > 64 {
		errs = append(errs, FieldError{
			Step: "schema", Field: "depends_on",
			Message: fmt.Sprintf("too many dependencies (%d, max 64)", len(p.DependsOn)),
		})
	}

	// tags
	if len(p.Tags) > 32 {
		errs = append(errs, FieldError{
			Step: "schema", Field: "tags",
			Message: fmt.Sprintf("too many tags (%d, max 32)", len(p.Tags)),
		})
	}

	if len(errs) > 0 {
		return &PayloadValidationError{Errors: errs}
	}
	return nil
}

// ToProposal converts a validated payload to the Store's Proposal
// shape. Callers MUST call Validate() first; this method assumes
// every field has already passed its contract.
//
// The conversion applies the fork-flow default from §3.6 step 2:
// when replaces_id is set, created_from_id is automatically populated
// to the same value so the lineage walker can revive the predecessor
// on rollback.
func (p *ForgePayload) ToProposal() *Proposal {
	prop := &Proposal{
		Name:           p.Name,
		Purpose:        p.Purpose,
		SourceCode:     p.SourceCode,
		SourceLanguage: p.SourceLanguage,
		RequestedDomain: ExecutionDomain(p.RequestedDomain),
		Tags:           StringSlice(p.Tags),
		DependsOn:      append([]string(nil), p.DependsOn...),
		AuthorAgent:    "forge",
	}

	if p.AuthorTheoryID != nil {
		prop.AuthorTheoryID = p.AuthorTheoryID
	}

	// Fork flow: created_from_id inherits replaces_id so the
	// rollback cascade walker can find the predecessor.
	if p.ReplacesID != nil {
		id := *p.ReplacesID
		prop.CreatedFromID = &id
	}

	// If the payload carries metadata, parse it into the Store's
	// CapabilityMetadata map. Errors are silent — the payload's
	// metadata is advisory, not load-bearing.
	if len(p.Metadata) > 0 {
		meta := make(CapabilityMetadata)
		if err := json.Unmarshal(p.Metadata, &meta); err == nil {
			prop.Metadata = meta
		}
	}

	return prop
}

// ParsePayload decodes a JSON byte slice into a ForgePayload. Used
// by the CLI/MCP entry points; pure parsing (no validation). Returns
// a typed error on malformed JSON so handlers can distinguish
// "couldn't parse" from "parsed but invalid."
func ParsePayload(raw []byte) (*ForgePayload, error) {
	var p ForgePayload
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("capability: parse payload: %w", err)
	}
	return &p, nil
}
