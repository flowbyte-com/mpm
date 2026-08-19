// provenance_db.go — DB-writing half of artifact provenance.
//
// Spec: docs/superpowers/specs/2026-08-08-artifact-provenance-design.md
//
// Critical invariants this file preserves:
//
//   1. The artifact tx is NEVER poisoned. The provenance INSERT runs
//      inside a SAVEPOINT prov_rec; on failure, ROLLBACK TO SAVEPOINT
//      and RELEASE SAVEPOINT keep the artifact write committed.
//
//   2. The function NEVER returns `error`. Failure is observable via
//      ProvenanceRecordResult; the artifact insert (the caller's
//      primary objective) is not affected by telemetry outcome.
//
//   3. Validation runs BEFORE the SAVEPOINT. A bad APIEndpoint or
//      malformed provider_metadata is rejected with an audit row and
//      no INSERT attempt — the SAVEPOINT is not opened and the tx
//      is untouched.
//
//   4. The UNIQUE (artifact_id, artifact_type) constraint prevents
//      duplicate recording. A second call for the same artifact
//      returns Recorded=false with SQLError describing the violation.
package internal

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ProvenanceRecordResult communicates the outcome of a provenance
// recording attempt. The function never returns an error; this struct
// is the only signal, so future observability layers can track
// telemetry degradation without changing the artifact-tx contract.
type ProvenanceRecordResult struct {
	Recorded         bool
	ValidationReason string
	SQLError         string
}

// RecordArtifactProvenance writes an artifact_provenance row inside
// the caller's transaction. The artifact write has already landed
// at this point; the function guarantees that the artifact tx is
// not poisoned regardless of outcome.
//
// Failure modes (all return Recorded=false):
//   - Validation rejection (bad APIEndpoint, malformed metadata)
//   - SQLite INSERT failure (constraint, schema, connection)
//
// The SQLite failure path uses SAVEPOINT isolation so the artifact's
// INSERT is unaffected.
func (dm *DatabaseManager) RecordArtifactProvenance(
	tx *sql.Tx,
	artifactID, artifactType string,
	prov *EffectiveProvenance,
) ProvenanceRecordResult {
	// Pre-tx validation. Rejections never touch the tx.
	if reason := validateProvenance(prov); reason != "" {
		dm.LogAudit(AuditWarn, "provenance", "validation rejected: "+reason, "", AuditContext{
			"artifact_id": artifactID,
		})
		return ProvenanceRecordResult{ValidationReason: reason}
	}

	// Open SAVEPOINT. If even the SAVEPOINT fails, return early — the
	// artifact tx may be in a degraded state already; we don't try to
	// operate on it further.
	if _, err := tx.Exec("SAVEPOINT prov_rec"); err != nil {
		dm.LogAudit(AuditError, "provenance", "savepoint failed: "+err.Error(), "", AuditContext{
			"artifact_id": artifactID,
		})
		return ProvenanceRecordResult{SQLError: err.Error()}
	}

	// Build SQL. NULL semantics are explicit: pointer fields that are
	// nil or string fields that are empty become NULL.
	args := []interface{}{
		GenerateID(),
		artifactID,
		artifactType,
		time.Now().Unix(),
		nilOrString(prov.ActorKind),
		nilOrString(prov.ActorID),
		nilOrString(prov.FrameworkName),
		nilOrString(prov.FrameworkVersion),
		nilOrString(prov.FrameworkAdapter),
		nilOrString(prov.ProviderName),
		nilOrString(prov.ModelName),
		nilOrString(prov.ModelRevision),
		nilOrString(prov.APIEndpoint),
		nilOrFloat(prov.Temperature),
		nilOrInt(prov.MaxTokens),
		nilOrString(prov.ReasoningMode),
		nilOrFloat(prov.ReasoningEffort),
		nilOrStringPtr(prov.ThinkingLevel),
		nilOrInt64(prov.ThinkingTokens),
		nilOrBool(prov.ThinkingVisible),
		nilOrString(prov.SessionID),
		nilOrString(prov.InvocationID),
		nilOrString(prov.ParentArtifactID),
		nilOrString(prov.ParentInvocationID),
		nilOrString(prov.ProviderMetadata),
	}

	_, err := tx.Exec(`INSERT INTO artifact_provenance (
		id, artifact_id, artifact_type, created_at,
		actor_kind, actor_id,
		framework_name, framework_version, framework_adapter,
		provider_name, model_name, model_revision, api_endpoint,
		temperature, max_tokens,
		reasoning_mode, reasoning_effort,
		thinking_level, thinking_tokens, thinking_visible,
		session_id, invocation_id, parent_artifact_id,
		parent_invocation_id,
		provider_metadata
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		args...,
	)
	if err != nil {
		// Rollback the SAVEPOINT only — the artifact tx is preserved.
		tx.Exec("ROLLBACK TO SAVEPOINT prov_rec")
		tx.Exec("RELEASE SAVEPOINT prov_rec")
		dm.LogAudit(AuditError, "provenance", "insert failed: "+err.Error(), "", AuditContext{
			"artifact_id": artifactID,
		})
		return ProvenanceRecordResult{SQLError: err.Error()}
	}

	if _, err := tx.Exec("RELEASE SAVEPOINT prov_rec"); err != nil {
		// Unusual — the SAVEPOINT was opened and the INSERT succeeded,
		// but RELEASE failed. Don't fail the result; log it.
		dm.LogAudit(AuditWarn, "provenance", "release savepoint failed: "+err.Error(), "", AuditContext{
			"artifact_id": artifactID,
		})
	}
	return ProvenanceRecordResult{Recorded: true}
}

func validateProvenance(prov *EffectiveProvenance) string {
	if prov == nil {
		return "nil provenance"
	}
	if prov.APIEndpoint != "" {
		// Hard rule: no query string, no userinfo.
		if strings.Contains(prov.APIEndpoint, "?") {
			return fmt.Sprintf("api_endpoint contains query string: %q", prov.APIEndpoint)
		}
		if strings.Contains(prov.APIEndpoint, "@") {
			return fmt.Sprintf("api_endpoint contains userinfo: %q", prov.APIEndpoint)
		}
	}
	if prov.ProviderMetadata != "" {
		// Must be a valid JSON object. Empty is fine (maps to NULL).
		var v map[string]interface{}
		if err := json.Unmarshal([]byte(prov.ProviderMetadata), &v); err != nil {
			return fmt.Sprintf("provider_metadata is not a JSON object: %v", err)
		}
	}
	return ""
}

// NULL-conversion helpers. Empty strings and nil pointers map to nil
// for sql.Exec (which writes NULL). Non-empty values are passed as-is.
func nilOrString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func nilOrStringPtr(s *string) interface{} {
	if s == nil {
		return nil
	}
	return *s
}

func nilOrFloat(f *float64) interface{} {
	if f == nil {
		return nil
	}
	return *f
}

func nilOrInt(i *int) interface{} {
	if i == nil {
		return nil
	}
	return int64(*i)
}

func nilOrInt64(i *int64) interface{} {
	if i == nil {
		return nil
	}
	return *i
}

func nilOrBool(b *bool) interface{} {
	if b == nil {
		return nil
	}
	if *b {
		return 1
	}
	return 0
}
