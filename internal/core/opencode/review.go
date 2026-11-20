// review.go — the REVIEWER half of the OpenCode boundary.
//
// # WHAT THIS IS
//
// The worker path (`runtime.go`) runs a file-edit-only agent inside a mount
// namespace. The REVIEW path is deliberately different, because Charlie's
// isolation is different and was accepted on its own terms: `mpm-reviewer`
// is default-deny, its ONE execution path is the host-controlled
// `sandbox_exec` tool that the MPM OpenCode plugin registers, and that tool
// establishes a bubblewrap namespace of its own.
//
// So this file does NOT wrap the reviewer in the worker's containment. Doing
// so would nest a namespace inside one that refuses nesting
// (`--disable-userns`) and would WEAKEN the accepted boundary rather than
// preserve it. What it does instead is:
//
//   - refuse unless the reviewer definition and the plugin are actually
//     available, so a missing sandbox is a refusal rather than a review
//     that quietly ran with a wider surface;
//   - run the reviewer in a DISPOSABLE configuration, so nothing it does
//     touches the operator's live OpenCode installation;
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
	"time"
)

// ErrReviewerUnavailable says the reviewer's configuration is not usable.
//
// It is a REFUSAL, not a fallback: there is no less restricted reviewer to
// fall back to, so a review that cannot start says so.
var ErrReviewerUnavailable = errors.New("opencode: the reviewer's configuration is unavailable")

// ReviewerAgentName is the installed reviewer definition.
const ReviewerAgentName = "mpm-reviewer"

// ReviewConfig configures the reviewer path.
type ReviewConfig struct {
	// Root is a DISPOSABLE directory for this review's configuration and
	// captured evidence. It must not be the operator's live config.
	Root string
	// RepoPath is the repository under review. It becomes the runtime's
	// working directory, which is what the reviewer's sandbox is built
	// from.
	RepoPath string
	// OpenCodeBin is the runtime.
	OpenCodeBin string
	// PluginPath is the MPM OpenCode plugin the reviewer's `sandbox_exec`
	// comes from. Empty means the reviewer has no execution path, and the
	// review is refused.
	PluginPath string
	// Model is the reviewer's model.
	Model string
	// Timeout bounds the review.
	Timeout time.Duration
}

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
	if cfg.PluginPath == "" {
		return nil, fmt.Errorf("%w: no plugin path, so `sandbox_exec` would not exist and the "+
			"reviewer would have no confined execution path", ErrReviewerUnavailable)
	}
	if _, err := os.Stat(filepath.Join(cfg.PluginPath, "dist", "index.v2.js")); err != nil {
		return nil, fmt.Errorf("%w: the plugin at %s is not built: %v",
			ErrReviewerUnavailable, cfg.PluginPath, err)
	}
	// ORDER MATTERS, for the same reason the worker path's does: the
	// check asks the ISOLATED runtime what the definition's effective
	// permissions are, and the runtime can only answer about a definition
	// it has been given. Writing the configuration first is what makes the
	// check mean anything.
	if err := r.writeReviewConfig(); err != nil {
		return nil, err
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
	cmd.Dir = cfg.RepoPath
	cmd.Env = IsolatedEnv(cfg)
	var buf boundedBuffer
	buf.limit = maxCapturedOutput
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	runErr := cmd.Run()

	// THE NATIVE MESSAGE STREAM, not the process's stdout. The stream is
	// what the runtime recorded, and it is what a later reader can check.
	findings, state, err := r.captureMessages(ctx, req.SessionID)
	if err != nil {
		return nil, err
	}
	if state == nil {
		return nil, fmt.Errorf("%w: the reviewer produced no session state (%v)",
			ErrReviewerUnavailable, runErr)
	}
	if len(state.Permissions) > 0 {
		return nil, fmt.Errorf(
			"%w: the reviewer session carries a SESSION-LEVEL permission grant, which "+
				"overrides the agent definition's denies", ErrReviewerUnavailable)
	}

	digest := sha256.Sum256([]byte(findings))
	res := &ReviewResult{
		SessionID:      req.SessionID,
		FindingsSHA256: hex.EncodeToString(digest[:]),
		Findings:       findings,
		Outcome:        state.Outcome,
		Agent:          state.Agent,
		Model:          state.Model.ProviderID + "/" + state.Model.ID,
		Permissions:    state.Permissions,
	}

	// THE REVISION IS READ BY THE HOST.
	rev, err := observeRevision(cfg.RepoPath)
	if err != nil {
		return nil, fmt.Errorf("%w: the reviewed revision could not be observed: %v",
			ErrReviewerUnavailable, err)
	}
	if req.ExpectedRevision != "" && rev != req.ExpectedRevision {
		return nil, fmt.Errorf(
			"%w: the repository is at %s but the review was requested for %s",
			ErrReviewerUnavailable, rev, req.ExpectedRevision)
	}
	res.ReviewedRevision = rev

	// THE CAPTURE IS WRITTEN BY THE HOST, beside the disposable config.
	path := filepath.Join(cfg.Root, "findings", "review-"+req.SessionID+".json")
	payload := map[string]any{
		"session_id":        res.SessionID,
		"reviewed_revision": res.ReviewedRevision,
		"findings_sha256":   res.FindingsSHA256,
		"outcome":           res.Outcome,
		"agent":             res.Agent,
		"model":             res.Model,
		"findings":          res.Findings,
	}
	body, _ := json.MarshalIndent(payload, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("opencode: create the evidence directory: %w", err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return nil, fmt.Errorf("opencode: write the review evidence: %w", err)
	}
	res.EvidencePath = path
	return res, nil
}

// verifyReviewer proves the definition exists and is default-deny with
// `sandbox_exec` as its only allowed action.
func (r *Runtime) verifyReviewer(ctx context.Context) error {
	agents, err := r.listAgents(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrReviewerUnavailable, err)
	}
	var found *agentInfo
	for i := range agents {
		if agents[i].ID == ReviewerAgentName {
			found = &agents[i]
			break
		}
	}
	if found == nil {
		return fmt.Errorf("%w: the runtime does not know an agent named %q",
			ErrReviewerUnavailable, ReviewerAgentName)
	}
	// THE HOST PREPENDS A WILDCARD ALLOW, so "no wildcard allow exists"
	// is impossible to assert. What CAN be asserted is what the host
	// actually evaluates under last-match-wins: the LAST rule matching
	// action "*" must be a DENY, and no sensitive action may be allowed.
	lastWildcardDeny := false
	for i := len(found.Permissions) - 1; i >= 0; i-- {
		p := found.Permissions[i]
		if p.Action != "*" {
			continue
		}
		lastWildcardDeny = p.Effect == "deny"
		break
	}
	if !lastWildcardDeny {
		return fmt.Errorf("%w: %q's resolved rules do not END in a wildcard deny, so "+
			"every action it does not name explicitly is open",
			ErrReviewerUnavailable, ReviewerAgentName)
	}

	hasSandbox := false
	for _, p := range found.Permissions {
		if p.Effect != "allow" || p.Action == "*" {
			continue
		}
		if p.Action == "sandbox_exec" {
			hasSandbox = true
			continue
		}
		return fmt.Errorf("%w: %q allows %q beside `sandbox_exec`",
			ErrReviewerUnavailable, ReviewerAgentName, p.Action)
	}
	if !hasSandbox {
		return fmt.Errorf("%w: %q does not allow `sandbox_exec`, so it has no confined "+
			"execution path", ErrReviewerUnavailable, ReviewerAgentName)
	}
	return nil
}

// writeReviewConfig writes a DISPOSABLE OpenCode configuration that loads the
// MPM plugin (the source of `sandbox_exec`) and nothing of the operator's.
func (r *Runtime) writeReviewConfig() error {
	dir := filepath.Join(r.cfg.Root, "ocfg", "opencode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("opencode: create the review config directory: %w", err)
	}
	cfg := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"plugins": []string{"file://" + r.cfg.PluginPath},
	}
	body, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("opencode: encode the review config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "opencode.json"), body, 0o600); err != nil {
		return fmt.Errorf("opencode: write the review config: %w", err)
	}
	// THE DEFINITION COMES FROM THE BINARY. Reading it from a path would
	// let the party under review supply its reviewer's permissions.
	agents := filepath.Join(dir, "agents")
	if err := os.MkdirAll(agents, 0o700); err != nil {
		return fmt.Errorf("opencode: create the review agent directory: %w", err)
	}
	if err := os.WriteFile(
		filepath.Join(agents, ReviewerAgentName+".md"),
		ReviewerAgentDefinition, 0o600); err != nil {
		return fmt.Errorf("opencode: write the reviewer definition: %w", err)
	}
	return nil
}

// captureMessages reads the session's native message stream and returns the
// reviewer's own text plus the session state.
//
// The errors and the tool results are included: a review that refused to run
// is not a review that found nothing.
func (r *Runtime) captureMessages(ctx context.Context, sessionID string) (string, *SessionState, error) {
	out, err := r.runIsolated(ctx, r.cfg.OpenCodeBin, "--standalone", "api",
		"session.message.list", "--param", "sessionID="+sessionID)
	if err != nil {
		return "", nil, fmt.Errorf("opencode: read the reviewer's messages: %w", err)
	}
	var env struct {
		Data []struct {
			Type    string          `json:"type"`
			Text    string          `json:"text"`
			Content json.RawMessage `json:"content"`
			Error   json.RawMessage `json:"error"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		return "", nil, fmt.Errorf("opencode: parse the reviewer's messages: %w", err)
	}

	var b strings.Builder
	for _, m := range env.Data {
		switch m.Type {
		case "assistant":
			b.WriteString(assistantText(m.Content))
			b.WriteString("\n")
		case "user":
			b.WriteString("[user] " + m.Text + "\n")
		case "idle":
			if len(m.Error) > 0 && string(m.Error) != "null" {
				b.WriteString("[idle] " + string(m.Error) + "\n")
			}
		}
	}
	state, err := r.Inspect(ctx, sessionID)
	if err != nil {
		return "", nil, err
	}
	return b.String(), state, nil
}

// assistantText flattens an assistant message's content parts.
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
