package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// round 9 T46-T48: extend the `mpm ops confidence` subcommand to
// expose `explain` and `history` as thin CLI adapters over the
// underlying mpm_confidence functionality (the substrate tool at
// internal/core/tools handles action=explain|history directly). The
// pre-fix surface offered only show/recompute/changes/trend — a
// smoke test running `mpm ops confidence explain <id>` got "unknown
// subcommand" rather than the parity-with-mpm_call surface. The fix
// routes explain/history through the same actions the MCP/CALL
// surface accepts and updates the usage strings/help to advertise
// the full surface.
func parseOpsConfidenceArgs(args []string) (map[string]interface{}, string, error) {
	if len(args) == 0 {
		return nil, "", fmt.Errorf("subcommand required: show|recompute|changes|trend|explain|history")
	}
	sub := args[0]
	switch sub {
	case "show", "recompute", "changes", "trend", "explain", "history":
	default:
		return nil, "", fmt.Errorf("unknown subcommand: %q (want show|recompute|changes|trend|explain|history)", sub)
	}
	fs := flag.NewFlagSet("ops-confidence-"+sub, flag.ContinueOnError)
	artifactID := fs.String("artifact", "", "artifact id (required)")
	artifactType := fs.String("artifact-type", "memory", "artifact type")
	limit := fs.Int("limit", 10, "history rows to show (show|history only)")
	windowDays := fs.Int("window-days", 30, "trend window in days (trend only)")
	if err := fs.Parse(args[1:]); err != nil {
		return nil, "", err
	}
	if *artifactID == "" {
		return nil, "", fmt.Errorf("--artifact is required")
	}
	payload := map[string]interface{}{
		"artifact_id":   *artifactID,
		"artifact_type": *artifactType,
		"limit":         *limit,
		"window_days":   *windowDays,
	}
	return payload, sub, nil
}

func handleOpsConfidence(args []string) int {
	payload, sub, err := parseOpsConfidenceArgs(args)
	if err != nil {
		printError("%v", err)
		return 1
	}
	dm := getDBConcrete()
	if dm == nil {
		return 1
	}

	artifactID, _ := payload["artifact_id"].(string)
	artifactType, _ := payload["artifact_type"].(string)

	var res interface{}
	switch sub {
	case "show":
		limit, _ := payload["limit"].(int)
		snap, err := mpminternal.GetConfidenceForArtifact(dm, artifactID, artifactType, limit)
		if err != nil {
			printError("ops confidence show: %v", err)
			return 1
		}
		res = snap
	case "recompute":
		if err := mpminternal.RecomputeConfidence(dm, artifactID, artifactType, mpminternal.RecomputeReasonManual); err != nil {
			printError("ops confidence recompute: %v", err)
			return 1
		}
		res = map[string]interface{}{"success": true, "artifact_id": artifactID}
	case "changes":
		limit, _ := payload["limit"].(int)
		filter := mpminternal.ConfidenceChangesFilter{
			Since:        time.Now().Add(-24 * time.Hour),
			Limit:        limit,
			ArtifactID:   artifactID,
			ArtifactType: artifactType,
		}
		changes, err := mpminternal.QueryConfidenceChanges(dm, filter)
		if err != nil {
			printError("ops confidence changes: %v", err)
			return 1
		}
		res = map[string]interface{}{"changes": changes, "count": len(changes)}
	case "trend":
		windowDays, _ := payload["window_days"].(int)
		trend, err := mpminternal.QueryConfidenceTrend(dm, artifactID, artifactType, windowDays)
		if err != nil {
			printError("ops confidence trend: %v", err)
			return 1
		}
		res = trend
	case "explain":
		// T47: route through the substrate confidence engine so the
		// CLI surface stays in parity with `mpm call mpm_confidence
		// --payload '{"action":"explain",...}'`. The handler returns
		// the reasoning trace (evidence breakdown + recompute trace)
		// for the artifact.
		explain, err := dm.ExplainConfidence(artifactID, artifactType)
		if err != nil {
			printError("ops confidence explain: %v", err)
			return 1
		}
		res = explain
	case "history":
		// T48: history is the full confidence_history timeline. The
		// underlying tool accepts `action=history`; we go through
		// the canonical QueryConfidenceHistory so a CLI call and
		// `mpm call mpm_confidence --payload '{"action":"history"}'`
		// return identical shapes.
		limit, _ := payload["limit"].(int)
		history, err := dm.QueryConfidenceHistory(artifactID, artifactType, limit)
		if err != nil {
			printError("ops confidence history: %v", err)
			return 1
		}
		res = history
	}
	out, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(out))
	return 0
}
