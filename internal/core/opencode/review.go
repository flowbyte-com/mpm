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
	cmd.Env = IsolatedEnv(cfg)
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
		return nil, err
	}
	if state == nil {
		return nil, fmt.Errorf("%w: the reviewer produced no session state (%v)",
			ErrReviewerUnavailable, runErr)
	}
	if violation := reviewerSessionOverrideViolation(state.Permissions); violation != "" {
		return nil, fmt.Errorf("%w: %s", ErrReviewerUnavailable, violation)
	}

	digest := sha256.Sum256([]byte(findings))
	res := &ReviewResult{
		SessionID:         req.SessionID,
		FindingsSHA256:    hex.EncodeToString(digest[:]),
		Findings:          findings,
		AssistantFindings: assistantFindings,
		Outcome:           state.Outcome,
		Agent:             state.Agent,
		Model:             state.Model.ProviderID + "/" + state.Model.ID,
		Permissions:       state.Permissions,
	}

	// THE REVISION IS THE PINNED ONE, not a fresh observation.
	res.ReviewedRevision = pinned

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
	if violation := reviewerPermissionViolation(found.Permissions); violation != "" {
		return fmt.Errorf("%w: %s", ErrReviewerUnavailable, violation)
	}
	return nil
}

// reviewerPermissionViolation evaluates the ORDERED permission rules the
// host resolved for the reviewer.
//
// # WHY ORDER MATTERS
//
// The host prepends its own rules and appends the agent definition, and
// evaluates the result with last-match-wins. A rule that ALLOWS an action is
// therefore not effective merely because it appears; a later matching DENY
// overrides it. OpenCode 2.3.2 supplies, in order, a `* *` allow, read rules
// for `.env` files (including `read *.env.example` allow), a `* *` deny,
// `sandbox_exec *` allow, `read *` deny, and the remaining explicit action
// denies.
//
// The old verifier scanned forward and rejected every explicit allow, so the
// host's `.env.example` allow — already overridden by the final `read * deny`
// — made the reviewer unstartable.
//
// # WHAT "COVERS" MEANS
//
// A later deny overrides an allow only when the deny's action and resource
// patterns cover the allow's whole pattern. The proof is deliberately
// conservative: universal patterns, exact equality, a literal allow matched
// by a deny glob, a one-star deny whose prefix and suffix contain the allow's,
// and a `*middle*` deny whose literal is wholly inside one of the allow's
// literal parts. Shapes it cannot prove are refused rather than assumed safe.
// A broad allow is therefore never collapsed into an action-wide permission
// because a narrower later rule denied one example.
func reviewerPermissionViolation(perms []SessionPerm) string {
	// UNMEASURED SYNTAX IS REFUSED OUTRIGHT. A later deny using it cannot
	// be proved to cover anything, and a sandbox allow using it cannot be
	// proved to provide a usable execution path. Treating "cannot prove"
	// as "does not cover" would accept an unproven sandbox.
	for _, p := range perms {
		switch p.Effect {
		case "allow", "deny", "ask":
		default:
			return fmt.Sprintf(
				"%q's resolved rules carry an unknown permission effect %q",
				ReviewerAgentName, p.Effect)
		}
		if permissionHasUnprovenSyntax(p.Action, false, p.Effect == "allow") ||
			permissionHasUnprovenSyntax(p.Resource, true, p.Effect == "allow") {
			return fmt.Sprintf(
				"%q's resolved rules contain permission syntax this verifier cannot prove",
				ReviewerAgentName)
		}
	}

	// THE WILDCARD MUST END IN A DENY THAT COVERS EVERY RESOURCE.
	//
	// The host prepends a wildcard allow, so "no wildcard allow exists" is
	// impossible to assert. The last wildcard-action rule decides the
	// actions the definition does not name, and it must deny everything; a
	// deny narrowed to `secret*` leaves the rest open.
	lastWildcard := -1
	for i, p := range perms {
		if p.Action == "*" {
			lastWildcard = i
		}
	}
	if lastWildcard < 0 ||
		perms[lastWildcard].Effect != "deny" ||
		!permissionResourceIsUniversal(perms[lastWildcard].Resource) {
		return fmt.Sprintf(
			"%q's resolved rules do not END in an all-resource wildcard deny, so "+
				"every action it does not name explicitly is open", ReviewerAgentName)
	}

	hasSandbox := false
	for i, p := range perms {
		if p.Effect != "allow" || p.Action == "*" {
			continue
		}
		if permissionAllowOverridden(perms, i) {
			continue
		}
		if p.Action == "sandbox_exec" {
			hasSandbox = true
			continue
		}
		return fmt.Sprintf("%q allows %q beside `sandbox_exec`",
			ReviewerAgentName, p.Action)
	}

	// AN EFFECTIVE ASK IS A REQUESTABLE ACTION, NOT A DENY. It is unsafe
	// unless a later allow or deny fully covers it. The original host
	// `.env` asks are covered by the later `read * deny`.
	for i, p := range perms {
		if p.Effect != "ask" {
			continue
		}
		overridden := false
		for j := i + 1; j < len(perms); j++ {
			q := perms[j]
			if q.Effect != "allow" && q.Effect != "deny" {
				continue
			}
			if !permissionActionCovers(q.Action, p.Action) ||
				!permissionResourceCovers(q.Resource, p.Resource) {
				continue
			}
			overridden = true
			break
		}
		if !overridden {
			return fmt.Sprintf("%q leaves %q requestable by an effective `ask`",
				ReviewerAgentName, p.Action)
		}
	}

	if !hasSandbox {
		return fmt.Sprintf("%q does not allow `sandbox_exec` effectively, so it has no "+
			"confined execution path", ReviewerAgentName)
	}
	return ""
}

// permissionAllowOverridden reports whether a later rule makes an allow
// ineffective. A later deny denies it; a later ask turns it into a request.
// Either way the allow is not effective.
func permissionAllowOverridden(perms []SessionPerm, allowIndex int) bool {
	p := perms[allowIndex]
	for j := allowIndex + 1; j < len(perms); j++ {
		q := perms[j]
		if q.Effect != "deny" && q.Effect != "ask" {
			continue
		}
		if !permissionActionCovers(q.Action, p.Action) ||
			!permissionResourceCovers(q.Resource, p.Resource) {
			continue
		}
		return true
	}
	return false
}

// permissionActionCovers reports whether a deny rule's action pattern covers
// the whole of an allow rule's action pattern.
//
// A literal allow can be covered by a deny glob (`opencode_*` covers
// `opencode_foo`). When the allow is itself a glob, only equality or a
// universal deny proves coverage.
func permissionActionCovers(deny, allow string) bool {
	if deny == "*" || deny == allow {
		return true
	}
	// Escapes, single-character classes and `**` have no measured host
	// semantics in the resolved list. Refuse them rather than guess.
	if permissionHasUnprovenSyntax(deny, false, false) ||
		permissionHasUnprovenSyntax(allow, false, true) {
		return false
	}
	if !strings.ContainsAny(allow, "*?[\\") {
		return permissionActionMatches(deny, allow)
	}
	return false
}

// permissionResourceCovers reports whether a deny rule's resource pattern
// covers the whole of an allow rule's resource pattern.
//
// The recognisable shapes are the ones the resolved permission list uses:
// a universal deny, an exact match, a literal allow matched by a deny glob,
// a one-star deny whose literal prefix/suffix contain the allow's, and a
// `*middle*` deny whose literal is wholly inside one of the allow's literal
// parts. The span the deny's star must consume is required to be slash-free.
// Everything else — including escapes, `?`, character classes and `**` — is
// refused, not assumed safe.
func permissionResourceCovers(deny, allow string) bool {
	if deny == "*" || deny == allow {
		return true
	}
	// Escapes, single-character classes and `**` have no measured host
	// semantics in the resolved list. Refuse them rather than guess.
	if permissionHasUnprovenSyntax(deny, true, false) ||
		permissionHasUnprovenSyntax(allow, true, true) {
		return false
	}
	if !strings.ContainsAny(allow, "*?[\\") {
		// The allow names one concrete resource; a deny glob that matches
		// it covers it.
		return permissionResourceMatches(deny, allow)
	}
	allowPrefix, allowSuffix, allowSingle := permissionSplitOneStar(allow)
	if !allowSingle {
		return false
	}
	// A one-star deny `prefix*suffix` covers `allowPrefix*allowSuffix`
	// when the deny's prefix is a prefix of the allow's and the deny's
	// suffix is a suffix of the allow's. The deny's `*` cannot cross a
	// path separator, so the covered middle must be slash-free.
	if denyPrefix, denySuffix, denySingle := permissionSplitOneStar(deny); denySingle {
		if !strings.HasPrefix(allowPrefix, denyPrefix) ||
			!strings.HasSuffix(allowSuffix, denySuffix) {
			return false
		}
		middle := allowPrefix[len(denyPrefix):] +
			allowSuffix[:len(allowSuffix)-len(denySuffix)]
		return !strings.Contains(middle, "/")
	}
	// A `*middle*` deny covers a one-star allow when the middle literal
	// lies wholly inside the allow's prefix or suffix, and the literal
	// parts the two stars must span contain no path separator.
	if strings.HasPrefix(deny, "*") && strings.HasSuffix(deny, "*") &&
		strings.Count(deny, "*") == 2 {
		middle := strings.TrimSuffix(strings.TrimPrefix(deny, "*"), "*")
		if middle == "" {
			return false
		}
		var outside string
		switch {
		case strings.Contains(allowPrefix, middle):
			outside = strings.Replace(allowPrefix, middle, "", 1) + allowSuffix
		case strings.Contains(allowSuffix, middle):
			outside = allowPrefix + strings.Replace(allowSuffix, middle, "", 1)
		default:
			return false
		}
		return !strings.Contains(outside, "/")
	}
	return false
}

// permissionHasUnprovenSyntax reports whether a pattern uses syntax whose
// host semantics were not measured. `**` is checked on the pattern itself,
// not on a concatenation: two adjacent single-star patterns must not look
// like a double star.
//
// A resource pattern may use one star, or the measured two-star `*middle*`
// form used by the host's `.env.*` ASK and by a deny that covers a narrower
// allow. A two-star resource ALLOW is refused: the coverage proof cannot
// bound an allow's two-star shape, and an allow is exactly where a later
// deny could be missed. Action patterns get at most one star.
func permissionHasUnprovenSyntax(pattern string, resource, allowEffect bool) bool {
	if pattern == "" {
		return true
	}
	if strings.ContainsAny(pattern, "?[\\") || strings.Contains(pattern, "**") {
		return true
	}
	stars := strings.Count(pattern, "*")
	if stars <= 1 {
		return false
	}
	if !resource || stars != 2 ||
		!strings.HasPrefix(pattern, "*") || !strings.HasSuffix(pattern, "*") {
		return true
	}
	middle := strings.TrimSuffix(strings.TrimPrefix(pattern, "*"), "*")
	if middle == "" {
		return true
	}
	// The measured `.env.*` shape is an ask/deny. A two-star allow is
	// refused rather than treated as effective without a coverage proof.
	return allowEffect
}

// permissionSplitOneStar splits a pattern containing exactly one `*` into its
// literal prefix and suffix. A literal pattern (no star) is not a one-star
// pattern; callers handle that case before calling.
func permissionSplitOneStar(pattern string) (prefix, suffix string, ok bool) {
	if strings.Count(pattern, "*") != 1 || strings.ContainsAny(pattern, "?[\\") {
		return "", "", false
	}
	i := strings.IndexByte(pattern, '*')
	return pattern[:i], pattern[i+1:], true
}

// permissionActionMatches reports whether an action pattern applies to a
// concrete action name. The host's `opencode_*` family is a real glob, not a
// literal name.
func permissionActionMatches(pattern, action string) bool {
	if pattern == "*" || pattern == action {
		return true
	}
	if !strings.ContainsAny(pattern, "*?[") {
		return false
	}
	matched, err := filepath.Match(pattern, action)
	return err == nil && matched
}

// permissionResourceMatches reports whether a resource pattern applies to a
// concrete resource.
//
// A bare `*` is the host's catch-all and matches every resource. Other
// patterns use filepath.Match semantics, so `*.env` does not silently become
// an action-wide permission. It also does not swallow path separators. A
// `**` is not treated as universal here; the coverage proof refuses it unless
// the patterns are exactly equal, because its host semantics were not
// measured.
func permissionResourceMatches(pattern, resource string) bool {
	if pattern == "*" || pattern == resource {
		return true
	}
	if !strings.ContainsAny(pattern, "*?[") {
		return false
	}
	matched, err := filepath.Match(pattern, resource)
	return err == nil && matched
}

// permissionResourceIsUniversal reports whether a wildcard deny covers every
// resource. Only the host's bare catch-alls do; `secret*` does not.
func permissionResourceIsUniversal(pattern string) bool {
	return pattern == "*"
}

// reviewerSessionOverrideViolation refuses a session-level permission grant.
//
// A session grant overrides the agent definition, so even a perfect
// definition check would not describe the session that actually ran.
func reviewerSessionOverrideViolation(perms []SessionPerm) string {
	if len(perms) == 0 {
		return ""
	}
	return "the reviewer session carries a SESSION-LEVEL permission grant, which " +
		"overrides the agent definition's denies"
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
func (r *Runtime) captureMessages(ctx context.Context, sessionID string) (string, string, *SessionState, error) {
	out, err := r.runIsolated(ctx, r.cfg.OpenCodeBin, "--standalone", "api",
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
// tool part renders as "[tool sandbox_exec]", which is not whitespace, so
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
// A tool part is ACTIVITY, not prose: "[tool sandbox_exec]" is not a
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
