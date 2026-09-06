// handlers_provenance.go — CLI surface for artifact provenance.
//
// Three commands form the coherent alpha surface:
//
//   mpm provenance <artifact_id>            show provenance for one artifact
//   mpm provenance inspect --invocation <id>  show all artifacts under one invocation
//   mpm provenance model-yield [--days N]   show model lifecycle analytics
//
// Spec: docs/archive/2026-08-08-artifact-provenance-design.md
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// handleProvenance is the entry point for `mpm provenance <id>`.
// Returns the artifact_provenance row for the given artifact, or
// a "no provenance" message if none exists.
func handleProvenance(args []string) int {
	if len(args) < 1 {
		return respond("", "Usage: mpm provenance <artifact_id>\n", 1)
	}
	artifactID := args[0]
	// Stage S4 of the CLI refactor (2026-09-06): --json is now
	// extracted via the canonical ExtractJSONFlag helper. The
	// previous per-handler hasFlag helper is deleted as dead
	// code (no remaining callers after this migration).
	asJSON, _ := ExtractJSONFlag(args)

	dm := getDB()
	if dm == nil {
		return respond("", "database unavailable\n", 1)
	}

	row, err := dm.SQLDB().Query(`
		SELECT actor_kind, actor_id, framework_name, framework_version,
		       framework_adapter, provider_name, model_name, model_revision,
		       api_endpoint, temperature, max_tokens, reasoning_mode,
		       reasoning_effort, thinking_level, thinking_tokens,
		       thinking_visible, session_id, invocation_id, parent_artifact_id,
		       provider_metadata, schema_version, created_at
		FROM artifact_provenance
		WHERE artifact_id = ?
		LIMIT 1`, artifactID)
	if err != nil {
		return respond("", fmt.Sprintf("query: %v\n", err), 1)
	}
	defer row.Close()

	if !row.Next() {
		return respond("", fmt.Sprintf("no provenance recorded for artifact %s\n", artifactID), 1)
	}

	// Scan into a map for clean JSON output.
	var (
		actorKind, actorID, fwName, fwVersion, fwAdapter sql.NullString
		provider, model, revision, apiEP, reasoningMode sql.NullString
		thinkingLevel                                     sql.NullString
		sessionID, invocationID, parent, providerMetadata sql.NullString
		schemaVersion                                    sql.NullString
		temperature, reasoningEffort                     sql.NullFloat64
		maxTokens, thinkingTokens                        sql.NullInt64
		thinkingVisible                                  sql.NullInt64
		createdAt                                        int64
	)
	if err := row.Scan(
		&actorKind, &actorID, &fwName, &fwVersion, &fwAdapter,
		&provider, &model, &revision, &apiEP,
		&temperature, &maxTokens, &reasoningMode,
		&reasoningEffort, &thinkingLevel, &thinkingTokens,
		&thinkingVisible, &sessionID, &invocationID, &parent,
		&providerMetadata, &schemaVersion, &createdAt,
	); err != nil {
		return respond("", fmt.Sprintf("scan: %v\n", err), 1)
	}

	// Build the wire-format map.
	out := map[string]interface{}{
		"artifact_id":       artifactID,
		"schema_version":    nullStringToString(schemaVersion),
		"actor_kind":        nullStringToString(actorKind),
		"actor_id":          nullStringToString(actorID),
		"framework":         nullStringToString(fwName),
		"framework_version": nullStringToString(fwVersion),
		"framework_adapter": nullStringToString(fwAdapter),
		"provider":          nullStringToString(provider),
		"model":             nullStringToString(model),
		"model_revision":    nullStringToString(revision),
		"api_endpoint":      nullStringToString(apiEP),
		"temperature":       nullFloatToFloat(temperature),
		"max_tokens":        nullInt64ToInt(maxTokens),
		"reasoning_mode":    nullStringToString(reasoningMode),
		"reasoning_effort":  nullFloatToFloat(reasoningEffort),
		"thinking_level":    nullStringToString(thinkingLevel),
		"thinking_tokens":   nullInt64ToInt(thinkingTokens),
		"thinking_visible":  nullInt64ToInt(thinkingVisible),
		"session_id":        nullStringToString(sessionID),
		"invocation_id":     nullStringToString(invocationID),
		"parent_artifact_id": nullStringToString(parent),
		"provider_metadata": nullStringToString(providerMetadata),
		"created_at":        createdAt,
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return respond("", fmt.Sprintf("json encode: %v\n", err), 1)
		}
		return 0
	}

	// Human-readable output.
	fmt.Printf("Artifact:       %s\n", artifactID)
	fmt.Printf("schema_version: %s\n", nullStringToString(schemaVersion))
	fmt.Printf("actor_kind:     %s\n", nullStringToString(actorKind))
	for _, kv := range []struct{ k, v string }{
		{"framework", nullStringToString(fwName)},
		{"framework_version", nullStringToString(fwVersion)},
		{"framework_adapter", nullStringToString(fwAdapter)},
		{"provider", nullStringToString(provider)},
		{"model", nullStringToString(model)},
		{"model_revision", nullStringToString(revision)},
		{"thinking_level", nullStringToString(thinkingLevel)},
		{"thinking_tokens", fmt.Sprintf("%v", nullInt64ToInt(thinkingTokens))},
		{"session_id", nullStringToString(sessionID)},
		{"invocation_id", nullStringToString(invocationID)},
		{"parent_artifact_id", nullStringToString(parent)},
	} {
		if kv.v != "" {
			fmt.Printf("  %s: %s\n", kv.k, kv.v)
		}
	}
	return 0
}

// handleProvenanceInspect is the entry point for
// `mpm provenance inspect --invocation <id>`.
func handleProvenanceInspect(args []string) int {
	inv := flagValue(args, "--invocation")
	if inv == "" {
		return respond("", "Usage: mpm provenance inspect --invocation <id>\n", 1)
	}

	dm := getDB()
	if dm == nil {
		return respond("", "database unavailable\n", 1)
	}

	rows, err := dm.SQLDB().Query(`
		SELECT artifact_id, artifact_type, created_at
		FROM artifact_provenance
		WHERE invocation_id = ?
		ORDER BY created_at ASC`, inv)
	if err != nil {
		return respond("", fmt.Sprintf("query: %v\n", err), 1)
	}
	defer rows.Close()

	fmt.Printf("Invocation: %s\n", inv)
	count := 0
	for rows.Next() {
		var id, typ string
		var createdAt int64
		if err := rows.Scan(&id, &typ, &createdAt); err != nil {
			return respond("", fmt.Sprintf("scan: %v\n", err), 1)
		}
		fmt.Printf("  %s  %s  %s\n", id, typ, formatUnix(createdAt))
		count++
	}
	if count == 0 {
		fmt.Printf("  (no artifacts under this invocation)\n")
	}
	return 0
}

// handleProvenanceModelYield is the entry point for
// `mpm provenance model-yield [--days N]`.
func handleProvenanceModelYield(args []string) int {
	days := 30
	if d := flagValue(args, "--days"); d != "" {
		if n, err := fmt.Sscanf(d, "%d", &days); err != nil || n != 1 {
			return respond("", "--days must be an integer\n", 1)
		}
	}

	dm := getDB()
	if dm == nil {
		return respond("", "database unavailable\n", 1)
	}

	rows, err := dm.SQLDB().Query(`
		SELECT model_spec, framework_name, total_created,
		       survived_30d, survival_30d_pct,
		       total_reinforcements, total_challenged
		FROM v_model_memory_yield
		WHERE total_created > 0
		ORDER BY total_created DESC`)
	if err != nil {
		return respond("", fmt.Sprintf("query: %v\n", err), 1)
	}
	defer rows.Close()

	fmt.Printf("\nLifecycle measure for artifacts created in the last %d days.\n", days)
	fmt.Printf("Survival is descriptive, not a quality score.\n\n")
	fmt.Printf("%-32s  %-16s  %8s  %8s  %12s  %12s  %10s\n",
		"model", "framework", "created", "survived", "survive_%", "reinforced", "challenged")
	for rows.Next() {
		var (
			modelSpec, fwName sql.NullString
			totalCreated, survived int64
			survivalPct          sql.NullFloat64
			reinforced, challenged sql.NullInt64
		)
		if err := rows.Scan(&modelSpec, &fwName, &totalCreated,
			&survived, &survivalPct, &reinforced, &challenged); err != nil {
			return respond("", fmt.Sprintf("scan: %v\n", err), 1)
		}
		fmt.Printf("%-32s  %-16s  %8d  %8d  %12s  %12s  %10s\n",
			modelSpec.String, fwName.String, totalCreated, survived,
			formatFloatNull(survivalPct),
			formatInt64Null(reinforced),
			formatInt64Null(challenged))
	}
	return 0
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func nullStringToString(s sql.NullString) string {
	if !s.Valid {
		return ""
	}
	return s.String
}

func nullFloatToFloat(f sql.NullFloat64) interface{} {
	if !f.Valid {
		return nil
	}
	return f.Float64
}

func nullInt64ToInt(i sql.NullInt64) interface{} {
	if !i.Valid {
		return nil
	}
	return i.Int64
}

func formatFloatNull(f sql.NullFloat64) string {
	if !f.Valid {
		return "—"
	}
	return fmt.Sprintf("%.1f%%", f.Float64)
}

func formatInt64Null(i sql.NullInt64) string {
	if !i.Valid {
		return "0"
	}
	return fmt.Sprintf("%d", i.Int64)
}

func flagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func formatUnix(unix int64) string {
	return time.Unix(unix, 0).UTC().Format("2006-01-02 15:04:05 UTC")
}
