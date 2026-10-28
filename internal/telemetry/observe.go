// internal/telemetry/observe.go — HighTokenNoArtifactHunt.
//
// Per spec §10: this hunt is an anomaly detector, NOT an MPM ROI metric.
// A session with high token burn and zero new artifacts may still have
// been valuable (retrieval-driven work, bug fixes without new memory).
// The finding is observation; human arbitration decides.
//
// The cross-DB lookup (artifact count per session) is parameterized via
// ArtifactCountFn. The production implementation computes the count
// IN-PROCESS, read-only, against the substrate's own attribution
// (internal/attribution). It used to shell out to `mpm call
// mpm_provenance --payload {"action":"count_by_session"}` — a tool that
// does not exist in the registry, and a subprocess that could not have
// been read-only in any case: every `mpm call` writes a
// tool_invocations row, even a read.

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

// Observation is the READ-ONLY DIAGNOSTIC rendering of a Finding.
//
// WHY A DIFFERENT SHAPE FROM THE OLD LESSON PAYLOAD. The finding used
// to be rendered as the payload of `mpm call mpm_lessons save` — a
// TRUSTED lesson, trusted on arrival, because lessons_base has no
// status column. A read-only diagnostic must not be shaped like the
// write it is not allowed to perform: an "action": "save" field and a
// "type" that names a lesson both advertise a write that the trust
// boundary forbids. The shape below says what it is — an unverified
// observation, with nothing written.
//
// This is the same category of output the critic's telemetry hunt
// produces since #138: a reported finding with no substrate row
// behind it.
type Observation struct {
	// Observation names the detector that produced the finding.
	Observation string `json:"observation"`
	// Status states the epistemic standing of the finding. It is an
	// observation, not evidence: the token counts live in telemetry.db,
	// which no citation type can address, and the finding is about an
	// ABSENCE, so there is nothing in mpm.db to cite.
	Status string `json:"status"`
	// SessionID is the framework session the finding is about.
	SessionID string `json:"session_id"`
	// InvocationCount is how many invocations the session recorded.
	InvocationCount int `json:"invocations"`
	// InputTokens and OutputTokens are the session's aggregate token
	// counts over the window.
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	// TotalTokens is InputTokens + OutputTokens — the quantity the
	// threshold is applied to.
	TotalTokens int64 `json:"total_tokens"`
	// Artifacts is the number of durable artifacts the substrate
	// attributed to the session. It is measured, not assumed: the
	// finding is only emitted when the measurement is zero.
	Artifacts int `json:"artifacts"`
	// Detail is the human-readable reason.
	Detail string `json:"detail"`
	// Wrote states what the diagnostic did, which is nothing.
	Wrote string `json:"wrote"`
}

// ObservationStatus is the status an Observation carries. It is
// deliberately not a lesson type: the observation has not been
// arbitrated by a human, and no trusted artifact claims it.
const ObservationStatus = "unverified-observation"

// RenderObservation renders a finding as a read-only diagnostic
// observation.
func RenderObservation(f Finding) Observation {
	return Observation{
		Observation:     "high-token-no-artifact",
		Status:          ObservationStatus,
		SessionID:       f.SessionID,
		InvocationCount: f.InvocationCount,
		InputTokens:     f.TotalInputTokens,
		OutputTokens:    f.TotalOutputTokens,
		TotalTokens:     f.TotalInputTokens + f.TotalOutputTokens,
		Artifacts:       0,
		Detail:          f.Reason,
		Wrote:           "nothing",
	}
}
