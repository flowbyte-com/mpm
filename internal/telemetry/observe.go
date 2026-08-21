// internal/telemetry/observe.go — HighTokenNoArtifactHunt.
//
// Per spec §10: this hunt is an anomaly detector, NOT an MPM ROI metric.
// A session with high token burn and zero new artifacts may still have
// been valuable (retrieval-driven work, bug fixes without new memory).
// The finding is observation; human arbitration decides.
//
// The cross-DB lookup (artifact count per session) is parameterized via
// ArtifactCountFn. The production implementation shells out to
// `mpm call mpm_provenance`. Tests inject a fake.

package telemetry

import (
	"context"
	"fmt"
)

type HuntConfig struct {
	Since             int64 // Unix epoch seconds; rows older than this are ignored
	HighTokenThreshold int64 // descriptive aggregate threshold (input + output)
	MinInvocations    int   // sessions below this invocation count are ignored
}

type Finding struct {
	SessionID         string
	InvocationCount   int
	TotalInputTokens  int64
	TotalOutputTokens int64
	Reason            string // human-readable; tag-friendly
}

type ArtifactCountFn func(ctx context.Context, sessionID string) (int, error)

func Hunt(ctx context.Context, store *Store, cfg HuntConfig, countFn ArtifactCountFn) ([]Finding, error) {
	rows, err := store.QuerySince(ctx, cfg.Since)
	if err != nil {
		return nil, fmt.Errorf("query telemetry: %w", err)
	}

	type agg struct {
		inputSum, outputSum int64
		count               int
	}
	bySession := make(map[string]*agg)
	for _, f := range rows {
		sid := ""
		if f.SessionID != nil {
			sid = *f.SessionID
		}
		if sid == "" {
			continue
		}
		a, ok := bySession[sid]
		if !ok {
			a = &agg{}
			bySession[sid] = a
		}
		if f.InputTokens != nil {
			a.inputSum += *f.InputTokens
		}
		if f.OutputTokens != nil {
			a.outputSum += *f.OutputTokens
		}
		a.count++
	}

	var findings []Finding
	for sid, a := range bySession {
		if a.count < cfg.MinInvocations {
			continue
		}
		totalTokens := a.inputSum + a.outputSum
		if totalTokens < cfg.HighTokenThreshold {
			continue
		}
		artifactCount, err := countFn(ctx, sid)
		if err != nil {
			return nil, fmt.Errorf("artifact_count for session %s: %w", sid, err)
		}
		if artifactCount > 0 {
			continue
		}
		findings = append(findings, Finding{
			SessionID:         sid,
			InvocationCount:   a.count,
			TotalInputTokens:  a.inputSum,
			TotalOutputTokens: a.outputSum,
			Reason:            fmt.Sprintf("high-token-no-artifact: session %s burned %d tokens across %d invocations with zero artifacts", sid, totalTokens, a.count),
		})
	}
	return findings, nil
}

// FindingLessonPayload renders a finding as the JSON payload for
// `mpm call mpm_lessons --payload {...}`. Per spec §10 the type is
// "observation" (not "warning") so the critic's existing triage
// treats it as informational.
func FindingLessonPayload(f Finding, cycleTag string) map[string]any {
	return map[string]any{
		"action": "save",
		"fact":   f.Reason,
		"type":   "observation",
		"tags":   []string{"telemetry", "high-token-no-artifact", "auto", cycleTag},
	}
}
