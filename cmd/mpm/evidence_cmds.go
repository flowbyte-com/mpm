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
func parseEvidenceAddArgs(args []string) (map[string]interface{}, error) {
	fs := flag.NewFlagSet("evidence-add", flag.ContinueOnError)
	artifactID := fs.String("artifact", "", "artifact id (required)")
	artifactType := fs.String("artifact-type", "memory", "artifact type (memory/theory/decision/lesson)")
	evType := fs.String("type", "", "evidence type (required)")
	source := fs.String("source", "", "source group (required)")
	strength := fs.String("strength", "", "strength in [-1, 1]; defaults to type's registry value")
	independence := fs.String("independence", "1.0", "independence factor; defaults to 1.0")
	createdBy := fs.String("by", "", "creator (required)")
	notes := fs.String("notes", "", "optional notes")
	if err := fs.Parse(args); err != nil {
		return nil, err
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
	return map[string]interface{}{
		"artifact_id":         *artifactID,
		"artifact_type":       *artifactType,
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
	in := mpminternal.EvidenceInput{
		ArtifactID:         payload["artifact_id"].(string),
		ArtifactType:       payload["artifact_type"].(string),
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
	out, _ := json.Marshal(map[string]interface{}{"success": true, "artifact_id": in.ArtifactID, "type": in.Type})
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
	artifactID := fs.String("artifact", "", "artifact id (required)")
	artifactType := fs.String("artifact-type", "memory", "artifact type")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if *artifactID == "" {
		return nil, fmt.Errorf("--artifact is required")
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
	rows, err := mpminternal.ListEvidenceForArtifact(dm, artifactID, artifactType)
	if err != nil {
		printError("list evidence: %v", err)
		return 1
	}
	out, _ := json.Marshal(rows)
	respond(string(out), "", 0)
	return 0
}
