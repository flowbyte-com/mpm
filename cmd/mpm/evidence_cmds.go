package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"strconv"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// parseEvidenceAddArgs converts a CLI args slice into a map with keys matching
// the EvidenceInput struct fields. Kept separate so it's testable without
// touching the DB.
//
// M3 audit D-023: --note (singular) is canonical; --notes (plural) is the
// deprecated alias kept for backwards compatibility with callers who picked
// up the original spelling. Both flags populate the same Notes field.
func parseEvidenceAddArgs(args []string) (map[string]interface{}, error) {
	fs := flag.NewFlagSet("evidence-add", flag.ContinueOnError)
	artifactID := fs.String("artifact", "", "artifact id (required)")
	// Defect G (2026-09-13 acceptance): the pre-fix default was
	// "memory", which silently wrote every evidence row with
	// artifact_type=memory regardless of whether the artifact was a
	// theory, decision, lesson, etc. The why flow then queried with
	// the public kind and found nothing. The fix leaves the default
	// empty so the caller can pass an explicit override or rely on
	// the handler-level auto-resolution (which calls
	// ResolveArtifactType on the artifact_id and stores the
	// canonical kind on the row).
	artifactType := fs.String("artifact-type", "", "artifact type (default: auto-resolve from artifact id)")
	evType := fs.String("type", "", "evidence type (required)")
	source := fs.String("source", "", "source group (required)")
	strength := fs.String("strength", "", "strength in [-1, 1]; defaults to type's registry value")
	independence := fs.String("independence", "1.0", "independence factor; defaults to 1.0")
	createdBy := fs.String("by", "", "creator (required)")
	notes := fs.String("note", "", "optional notes")
	notesAlias := fs.String("notes", "", "deprecated alias for --note")
	fs.Usage = func() {
		fmt.Println("Usage: mpm evidence add --artifact <id> --type <t> --source <s> --by <c> [--artifact-type <kind>] [--note <text>]")
		fmt.Println()
		fmt.Println("Flags:")
		fs.PrintDefaults()
		fmt.Println()
		fmt.Println("Note: --notes (plural) is a deprecated alias for --note.")
		fmt.Println("Note: --artifact-type defaults to auto-resolve from --artifact.")
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	// M3 audit D-023: --notes (plural) wins when set, --note (singular)
	// is the canonical form. Operator who types both sees the
	// plural-supplied value.
	if *notesAlias != "" && *notes == "" {
		*notes = *notesAlias
	}
	if *artifactID == "" {
		return nil, fmt.Errorf("--artifact is required")
	}
	if *evType == "" {
		return nil, fmt.Errorf("--type is required")
	}
	if !mpminternal.IsValidEvidenceType(*evType) {
		return nil, fmt.Errorf("invalid evidence type: %q", *evType)
	}
	if *source == "" {
		return nil, fmt.Errorf("--source is required")
	}
	if *createdBy == "" {
		return nil, fmt.Errorf("--by is required")
	}
	var s float64
	if *strength == "" {
		def, _ := mpminternal.DefaultStrength(*evType)
		s = def
	} else {
		parsed, err := strconv.ParseFloat(*strength, 64)
		if err != nil {
			return nil, fmt.Errorf("--strength: %w", err)
		}
		s = parsed
	}
	ind, err := strconv.ParseFloat(*independence, 64)
	if err != nil {
		return nil, fmt.Errorf("--independence: %w", err)
	}
	// Resolve artifact_type at the parser boundary so the handler
	// receives a non-empty kind. If the caller passed --artifact-type,
	// honor it (validated below). Otherwise leave empty and let the
	// handler call ResolveArtifactType.
	resolvedType := *artifactType
	return map[string]interface{}{
		"artifact_id":         *artifactID,
		"artifact_type":       resolvedType,
		"artifact_type_set":   *artifactType != "", // distinguishes explicit from auto
		"type":                *evType,
		"source_group":        *source,
		"strength":            s,
		"independence_factor": ind,
		"created_by":          *createdBy,
		"notes":               *notes,
	}, nil
}

// handleEvidenceAdd dispatches `mpm evidence add ...`. Calls internal.AddEvidence
// directly. Task 10 will add a parallel `mpm call mpm_evidence` tool that goes
// through the call layer.
func handleEvidenceAdd(args []string) int {
	payload, err := parseEvidenceAddArgs(args)
	if err != nil {
		printError("%v", err)
		return 1
	}
	if getDB() == nil {
		return 1
	}
	// AddEvidence takes a concrete *DatabaseManager, not the CoreDB
	// interface. The singleton is always a *DatabaseManager, so the
	// type assertion is safe; the nil check above guards it.
	dm := getDB().(*mpminternal.DatabaseManager)

	artifactType, _ := payload["artifact_type"].(string)
	// Defect G (2026-09-13 acceptance): the pre-fix default of
	// "memory" silently wrote the wrong artifact_type for any
	// typed artifact (theory, decision, lesson, work). The why
	// flow then queried with the public kind and returned empty
	// evidence/confidence rows. The fix auto-resolves the type from
	// the artifact_id when the caller didn't pass --artifact-type
	// explicitly, so theory evidence is stored as type=theory,
	// decision evidence as type=decision, etc. Backward compat:
	// the resolver's collection-to-type mapping matches what the
	// schema migration already uses, so old explicit overrides
	// continue to work.
	if payload["artifact_type_set"] == false {
		resolved, resolveErr := dm.ResolveArtifactType(payload["artifact_id"].(string))
		if resolveErr != nil {
			printError("--artifact-type is required when --artifact <id> cannot be resolved to a known kind (id=%q not found)", payload["artifact_id"].(string))
			return 1
		}
		artifactType = resolved
	}

	in := mpminternal.EvidenceInput{
		ArtifactID:         payload["artifact_id"].(string),
		ArtifactType:       artifactType,
		Type:               payload["type"].(string),
		SourceGroup:        payload["source_group"].(string),
		Strength:           payload["strength"].(float64),
		IndependenceFactor: payload["independence_factor"].(float64),
		CreatedBy:          payload["created_by"].(string),
		Notes:              payload["notes"].(string),
	}
	if err := mpminternal.AddEvidence(dm, in); err != nil {
		printError("add evidence: %v", err)
		return 1
	}
	out, _ := json.Marshal(map[string]interface{}{"success": true, "artifact_id": in.ArtifactID, "artifact_type": in.ArtifactType, "type": in.Type})
	respond(string(out), "", 0)
	return 0
}

// handleEvidence dispatches `mpm evidence ...` to subcommands. Task 8 adds
// the `list` subcommand.
func handleEvidence(args []string) int {
	if len(args) < 1 {
		// RECOMMENDED 7: when invoked with no subcommand, surface the
		// evidence-type documentation so operators can see the v1
		// vocabulary without grepping the source. Source of truth
		// remains internal/core/evidence.go.
		printError("usage: mpm evidence <add|list|types> ...\n\n%s", mpminternal.EvidenceTypeHelp())
		return 1
	}
	switch args[0] {
	case "add":
		return handleEvidenceAdd(args[1:])
	case "list":
		return handleEvidenceList(args[1:])
	case "types", "help", "-h", "--help":
		// The `types` subcommand prints the same canonical
		// evidence-type documentation as the empty-args path,
		// exposed as a first-class verb so scripts can pipe it.
		fmt.Println(mpminternal.EvidenceTypeHelp())
		return 0
	default:
		printError("unknown evidence subcommand: %s", args[0])
		return 1
	}
}

func parseEvidenceListArgs(args []string) (map[string]interface{}, error) {
	fs := flag.NewFlagSet("evidence-list", flag.ContinueOnError)
	artifactID := fs.String("artifact", "", "optional artifact id (omit for unfiltered list)")
	artifactType := fs.String("artifact-type", "memory", "artifact type (used only with --artifact)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"artifact_id":   *artifactID,
		"artifact_type": *artifactType,
	}, nil
}

func handleEvidenceList(args []string) int {
	payload, err := parseEvidenceListArgs(args)
	if err != nil {
		printError("%v", err)
		return 1
	}
	artifactID, _ := payload["artifact_id"].(string)
	artifactType, _ := payload["artifact_type"].(string)
	if getDB() == nil {
		return 1
	}
	dm := getDB().(*mpminternal.DatabaseManager)

	// 2026-09-10 regression repair (T45): bare `mpm evidence list`
	// (no --artifact) now returns all evidence; `--artifact <id>`
	// narrows to a single artifact. Both apply the same expiry
	// filter via the substrate helpers, so parity holds.
	var rows []mpminternal.Evidence
	if artifactID == "" {
		rows, err = mpminternal.ListEvidence(dm)
	} else {
		rows, err = mpminternal.ListEvidenceForArtifact(dm, artifactID, artifactType)
	}
	if err != nil {
		printError("list evidence: %v", err)
		return 1
	}
	// M3 audit D-019: the CLI evidence list returned a bare JSON
	// array. The MCP `mpm_evidence` action wraps the same payload in
	// `{success, count, evidence}` so the two surfaces disagree on
	// shape. Normalize on the wrapped envelope here too — callers
	// parsing the CLI output get the same keys as the call interface.
	if rows == nil {
		rows = []mpminternal.Evidence{}
	}
	resp := map[string]interface{}{
		"success": true,
		"count":   len(rows),
		"evidence": rows,
	}
	if artifactID != "" {
		resp["artifact_id"] = artifactID
		resp["artifact_type"] = artifactType
	}
	out, _ := json.Marshal(resp)
	respond(string(out), "", 0)
	return 0
}
