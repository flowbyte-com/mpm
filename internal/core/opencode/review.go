// review.go — the REVIEWER half of the OpenCode boundary.
//
// # WHAT THIS IS
//
// The REVIEW path runs the configured reviewer agent and captures what it
// produced. There is no operating-system sandbox: the reviewer's tool
// permissions come from the agent framework's configuration, and any
// deployment isolation is the operator's.
//
// What MPM does here is ORCHESTRATION, and it is unchanged:
//
//   - refuse unless the reviewer definition exists, so a missing agent is
//     named before an attempt is recorded;
//   - pin the exact revision and review a disposable snapshot of it;
//   - read the NATIVE message stream afterwards; and
//   - derive the reviewed revision and the findings digest from what the
//     HOST captured.
//
// # THE POINT OF THE LAST ONE
//
// A caller-supplied digest is not evidence. The digest this package records
// is computed over the bytes the runtime actually produced, and the revision
// is read from the repository the host handed it — neither is a parameter the
// implementer can choose.
package opencode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrReviewerUnavailable says the reviewer's configuration is not usable.
//
// It is a REFUSAL, not a fallback: there is no less restricted reviewer to
// fall back to, so a review that cannot start says so.
var ErrReviewerUnavailable = errors.New("opencode: the reviewer's configuration is unavailable")

// ErrReviewIncomplete says the reviewer process STARTED, but its result could
// not be captured faithfully: the message stream failed to read, the session
// state was missing, a session-level permission grant was present, the
// session's model was not the configured one, or the evidence file could not
// be written.
//
// It is NOT a pre-launch refusal. The model may already have run and produced
// billable output, so the assignment attempt must be treated as AMBIGUOUS:
// retrying could duplicate inference. Callers distinguish it from
// ErrReviewerUnavailable, which means nothing ran.
var ErrReviewIncomplete = errors.New("opencode: the reviewer started but its result is incomplete")

// ReviewerAgentName is the installed reviewer definition.
const ReviewerAgentName = "mpm-reviewer"

// ReviewConfig configures the reviewer path.

// ReviewRequest is one review.
type ReviewRequest struct {
	// SessionID is CALLER-MINTED, for the same reason the worker's is: a
	// retry must reconcile on an id that already exists.
	SessionID string
	// Brief is the bounded review brief.
	Brief string
	// Title labels the session.
	Title string
	// ExpectedRevision is the revision the operator believes is under
	// review. It is CHECKED against the revision the host observes; a
	// mismatch is a refusal, because a review of an unexpected revision is
	// not the review that was asked for.
	ExpectedRevision string
}

// ReviewResult is what a review actually produced, as OBSERVED by the host.
type ReviewResult struct {
	SessionID string
	// ReviewedRevision is read from the repository by the HOST, at capture
	// time. It is never taken from the caller.
	ReviewedRevision string
	// FindingsSHA256 is the SHA-256 of the captured message stream.
	FindingsSHA256 string
	// EvidencePath is where the host wrote the capture.
	EvidencePath string
	// Findings is the bounded text the reviewer produced.
	Findings string
	// AssistantFindings is the reviewer's OWN TEXT, with the user
	// prompt, tool activity and diagnostics removed. It is the only
	// source from which the host may transcribe a verdict.
	AssistantFindings string
	// Outcome is the runtime's own report.
	Outcome string
	// Agent and Model are what the session actually used.
	Agent string
	Model string
	// Permissions is the session's own permission overrides; a non-empty
	// list is a widening and is refused.
	Permissions []SessionPerm
}

// Review loads the reviewer definition, runs it and captures the result.
func (r *Runtime) Review(ctx context.Context, req ReviewRequest) (*ReviewResult, error) {
	cfg := r.cfg

	// THE REVISION IS PINNED BEFORE ANYTHING RUNS.
	//
	// Observing HEAD after the run records whatever the tree happened to
	// be at capture time — so a tree that advanced mid-review would be
	// credited with a revision the reviewer never saw. The review runs
	// against an IMMUTABLE SNAPSHOT at an exact SHA instead, and that same
	// SHA is what the evidence names.
	pinned, snapshot, err := r.snapshotRevision()
	if err != nil {
		return nil, err
	}
	if req.ExpectedRevision != "" && pinned != req.ExpectedRevision {
		return nil, fmt.Errorf(
			"%w: the repository is at %s but the review was requested for %s",
			ErrReviewerUnavailable, pinned, req.ExpectedRevision)
	}
	if err := r.verifyReviewer(ctx); err != nil {
		return nil, err
	}

	argv := []string{
		"--standalone", "run",
		"--session", req.SessionID,
		"--agent", ReviewerAgentName,
		"--model", cfg.Model,
		"--title", req.Title,
		req.Brief,
	}
	runCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, cfg.OpenCodeBin, argv...)
	cmd.Dir = snapshot
	// THE FRAMEWORK'S OWN ENVIRONMENT: its configuration, credentials and
	// tool permissions. MPM does not build a sandbox around the reviewer.
	cmd.Env = os.Environ()
	var buf boundedBuffer
	buf.limit = maxCapturedOutput
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	runErr := cmd.Run()

	// THE NATIVE MESSAGE STREAM, not the process's stdout. The stream is
	// what the runtime recorded, and it is what a later reader can check.
	//
	// ONLY ASSISTANT-AUTHORED TEXT COUNTS AS FINDINGS. A session that
	// contains nothing but the submitted prompt echo, an empty assistant
	// turn, tool activity or a diagnostic envelope has produced NO REVIEW —
	// counting it would let a prompt be mistaken for a verdict.
	findings, assistantFindings, state, err := r.captureMessages(ctx, req.SessionID)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrReviewIncomplete, err)
	}
	if state == nil {
		return nil, fmt.Errorf("%w: %w: the reviewer produced no session state (%v)",
			ErrReviewIncomplete, ErrReviewerUnavailable, runErr)
	}
	// THE SESSION'S OWN MODEL IS CHECKED. The configured model only says
	// what was ASKED for; the session state says what ran. A runtime that
	// fell back to another provider (or another model) did not run the
	// review the operator authorised.
	actualModel := state.Model.ProviderID + "/" + state.Model.ID
	if cfg.Model != "" && actualModel != cfg.Model {
		return nil, fmt.Errorf("%w: %w: the reviewer session ran model %q, want %q",
			ErrReviewIncomplete, ErrReviewerUnavailable, actualModel, cfg.Model)
	}

	// THE REVISION IS THE PINNED ONE, not a fresh observation.
	res := reviewResultFromCapture(req.SessionID, pinned, findings, assistantFindings, state)

	// THE CAPTURE IS WRITTEN BY THE HOST, beside the disposable config.
	path := filepath.Join(cfg.Root, "findings", "review-"+req.SessionID+".json")
	payload := map[string]any{
		"session_id":         res.SessionID,
		"reviewed_revision":  res.ReviewedRevision,
		"findings_sha256":    res.FindingsSHA256,
		"outcome":            res.Outcome,
		"agent":              res.Agent,
		"model":              res.Model,
		"findings":           res.Findings,
		"assistant_findings": res.AssistantFindings,
	}
	body, _ := json.MarshalIndent(payload, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("%w: create the evidence directory: %w",
			ErrReviewIncomplete, err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return nil, fmt.Errorf("%w: write the review evidence: %w",
			ErrReviewIncomplete, err)
	}
	res.EvidencePath = path
	return res, nil
}

// reviewResultFromCapture assembles the host-observed result.
//
// It exists as a named function so the PRODUCTION wiring can be tested: the
// transcript and the assistant-authored text are different values, and a
// review that put the transcript in `AssistantFindings` would let a prompt or
// tool line be transcribed as a verdict. A fake runtime cannot catch that
// swap because it supplies both fields itself.
func reviewResultFromCapture(sessionID, pinned, findings, assistantFindings string,
	state *SessionState) *ReviewResult {
	digest := sha256.Sum256([]byte(findings))
	return &ReviewResult{
		SessionID:         sessionID,
		ReviewedRevision:  pinned,
		FindingsSHA256:    hex.EncodeToString(digest[:]),
		Findings:          findings,
		AssistantFindings: assistantFindings,
		Outcome:           state.Outcome,
		Agent:             state.Agent,
		Model:             state.Model.ProviderID + "/" + state.Model.ID,
		Permissions:       state.Permissions,
	}
}

// verifyReviewer proves the runtime knows the reviewer definition.
func (r *Runtime) verifyReviewer(ctx context.Context) error {
	agents, err := r.listAgents(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrReviewerUnavailable, err)
	}
	for i := range agents {
		if agents[i].ID == ReviewerAgentName {
			return nil
		}
	}
	return fmt.Errorf("%w: the runtime does not know an agent named %q",
		ErrReviewerUnavailable, ReviewerAgentName)
}

func (r *Runtime) captureMessages(ctx context.Context, sessionID string) (string, string, *SessionState, error) {
	out, err := r.runRead(ctx, r.cfg.OpenCodeBin, "--standalone", "api",
		"session.message.list", "--param", "sessionID="+sessionID)
	if err != nil {
		return "", "", nil, fmt.Errorf("opencode: read the reviewer's messages: %w", err)
	}
	var env struct {
		Data []reviewMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		return "", "", nil, fmt.Errorf("opencode: parse the reviewer's messages: %w", err)
	}

	// THE COMPLETION GUARD, applied where the stream is read. The
	// transcript survives a refusal — it is returned alongside the error
	// — but a session that produced no assistant-authored TEXT is not a
	// review.
	transcript, err := captureReviewFindings(env.Data)
	if err != nil {
		return transcript, assistantAuthoredFindings(env.Data), nil, err
	}
	state, err := r.Inspect(ctx, sessionID)
	if err != nil {
		return "", "", nil, err
	}
	return transcript, assistantAuthoredFindings(env.Data), state, nil
}

// assistantAuthoredFindings is the reviewer's own prose, with prompts, tool
// activity and diagnostics removed. A verdict may be transcribed only from
// this text.
func assistantAuthoredFindings(msgs []reviewMessage) string {
	var b strings.Builder
	for _, m := range msgs {
		if m.Type != "assistant" {
			continue
		}
		// Keep message boundaries: a verdict in the next assistant turn
		// must start on its own line, not be concatenated onto prose.
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(assistantAuthoredText(m.Content))
	}
	return b.String()
}

// reviewMessage is the shape findingsFromMessages consumes.
type reviewMessage struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Content json.RawMessage `json:"content"`
	Error   json.RawMessage `json:"error"`
}

// findingsFromMessages separates the TRANSCRIPT from the AUTHORSHIP.
//
// The transcript is a faithful record: the submitted prompt, the tool
// activity and the runtime's diagnostics all appear, because a reader
// needs the whole exchange — a review that refused to run is not a review
// that found nothing.
//
// `authored` answers a different question: did the ASSISTANT write TEXT? A
// tool part renders as "[tool bash]", which is not whitespace, so
// counting the rendered transcript would let a session that only RAN TOOLS
// pass as a review. The gate therefore looks at the assistant's own words
// only.
func findingsFromMessages(msgs []reviewMessage) (transcript string, authored bool) {
	var all, assistant strings.Builder
	for _, m := range msgs {
		switch m.Type {
		case "assistant":
			// The TRANSCRIPT keeps the tool markers; the AUTHORSHIP gate
			// sees the assistant's text only.
			all.WriteString(assistantText(m.Content))
			all.WriteString("\n")
			assistant.WriteString(assistantAuthoredText(m.Content))
		case "user":
			all.WriteString("[user] " + m.Text + "\n")
		case "idle":
			if len(m.Error) > 0 && string(m.Error) != "null" {
				all.WriteString("[idle] " + string(m.Error) + "\n")
			}
		}
	}
	return all.String(), strings.TrimSpace(assistant.String()) != ""
}

// captureReviewFindings is the COMPLETION GUARD.
//
// It builds the transcript and then decides whether the session produced an
// actual review. `Review` calls this exact function, so a test that
// exercises it exercises the production guard: a prompt-only session, an
// empty assistant turn, tool activity alone and a diagnostics-only session
// are all refused, and the transcript is returned either way so the refusal
// can be read.
func captureReviewFindings(msgs []reviewMessage) (string, error) {
	transcript, authored := findingsFromMessages(msgs)
	if !authored {
		return transcript, fmt.Errorf(
			"%w: the reviewer session produced no assistant-authored findings "+
				"(only the submitted prompt, empty output, tool activity or "+
				"diagnostics)", ErrReviewerUnavailable)
	}
	return transcript, nil
}

// assistantText flattens an assistant message's content parts for the
// TRANSCRIPT: its text verbatim, and its tool activity as a marker.
func assistantText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return string(raw)
	}
	var b strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "text":
			b.WriteString(p.Text)
		case "tool":
			b.WriteString("[tool " + p.Name + "]\n")
		}
	}
	return b.String()
}

// assistantAuthoredText returns ONLY the assistant's own words.
//
// A tool part is ACTIVITY, not prose: "[tool bash]" is not a
// finding, and a session whose assistant turns carry nothing but tool
// calls has not written a review. A payload that cannot be parsed into
// content parts is not prose either, so it contributes nothing.
func assistantAuthoredText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type != "text" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(p.Text)
	}
	return b.String()
}

// observeRevision reads the repository's HEAD from the HOST.
func observeRevision(repo string) (string, error) {
	cmd := exec.Command("git", "-C", repo, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	rev := strings.TrimSpace(string(out))
	if len(rev) != 40 && len(rev) != 64 {
		return "", fmt.Errorf("not an object id: %q", rev)
	}
	return rev, nil
}

// snapshotRevision pins the revision a review will examine.
//
// It REFUSES a dirty worktree: uncommitted changes are not a revision, so a
// review of them cannot be described by an object id, and the evidence would
// name a commit that does not contain what was read.
//
// It then materialises an IMMUTABLE snapshot at that exact SHA and returns the
// path the runtime should run in. The snapshot is a separate clone, so the
// tree under review can keep moving without changing what is being examined.
func (r *Runtime) snapshotRevision() (string, string, error) {
	repo := r.cfg.RepoPath

	status, err := exec.Command("git", "-C", repo, "status", "--porcelain", "--untracked-files=normal").Output()
	if err != nil {
		return "", "", fmt.Errorf("%w: the repository could not be inspected: %v",
			ErrReviewerUnavailable, err)
	}
	if strings.TrimSpace(string(status)) != "" {
		return "", "", fmt.Errorf(
			"%w: the worktree has uncommitted changes, so there is no revision to review",
			ErrReviewerUnavailable)
	}

	rev, err := observeRevision(repo)
	if err != nil {
		return "", "", fmt.Errorf("%w: the revision could not be observed: %v",
			ErrReviewerUnavailable, err)
	}

	snapshot := filepath.Join(r.cfg.Root, "snapshot")
	_ = os.RemoveAll(snapshot)
	if out, err := exec.Command("git", "clone", "--quiet", "--no-checkout",
		"--shared", repo, snapshot).CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("%w: the snapshot could not be created: %v: %s",
			ErrReviewerUnavailable, err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("git", "-C", snapshot, "checkout", "--quiet", rev).CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("%w: the snapshot could not be checked out at %s: %v: %s",
			ErrReviewerUnavailable, rev, err, strings.TrimSpace(string(out)))
	}
	// THE SNAPSHOT MUST BE THE PINNED REVISION, verified rather than assumed.
	at, err := observeRevision(snapshot)
	if err != nil || at != rev {
		return "", "", fmt.Errorf("%w: the snapshot is at %q, not the pinned %q",
			ErrReviewerUnavailable, at, rev)
	}
	return rev, snapshot, nil
}
