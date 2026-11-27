// runtime.go — the only code in MPM that talks to OpenCode.
//
// # THE ARCHITECTURAL RULE
//
// This is a TRANSLATOR between MPM assignment state and OpenCode runtime
// state. It does not implement inference, model selection, tool execution
// or a conversation loop — OpenCode owns all of that. What lives here is:
//
//   - which agent definition may be used (and that it exists);
//   - how a session is created or resumed (the caller-minted id);
//   - what the runtime actually did (its own `outcome`, not an inference
//     from the existence of a session);
//   - whether the runtime ran where it was told to.
//
// # NO OPERATING-SYSTEM CONTAINMENT
//
// MPM is an agent orchestration, memory and continuity system. It does not
// create namespaces, mount filesystems, isolate credentials or run an
// inference relay. The process below is an ordinary child of the MPM
// process: it inherits the operator's environment and the runtime's own
// configuration, and the runtime's tool permissions and the deployment
// environment provide whatever isolation the operator wants.
//
// That is deliberate. An agent with the operator's operating-system
// permissions can reach what the operator can reach; MPM does not pretend
// otherwise, and an external container, dedicated user account or virtual
// machine is where that boundary belongs.
//
// THE COMMANDS
//
//	opencode --standalone run --session <id> --agent <a> [--model <m>] \
//	    --title <t> <prompt>
//
//	  `--session <id>` is the idempotency lever: it creates the session
//	  with the CALLER's id if it does not exist, and CONTINUES it if it
//	  does. `session.create` always mints its own id and is not
//	  idempotent, so it is not used.
//
//	opencode --standalone api session.list
//
//	  The read path for the runtime's OWN report: `outcome` (succeeded /
//	  failed), `location.directory`, `agent` and `model`.
//
//	opencode --standalone debug agents
//
//	  The only reliable way to read the agent definitions the runtime
//	  knows. `agent.get` answers 404 even for a known agent, so the debug
//	  surface is what this package uses.
package opencode

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Config is one OpenCode integration's fixed configuration.
type Config struct {
	// Root is MPM's own directory for this runtime: the review snapshot
	// and the captured evidence live here. It is NOT a sandbox root; it
	// is MPM state.
	Root string
	// OpenCodeBin is the runtime binary.
	OpenCodeBin string
	// Model is the runtime model identifier, "provider/model". Empty
	// means the framework's own configured default is used.
	Model string
	// RepoPath is the repository a REVIEW examines. Unused by the worker
	// path, whose workspace comes from the launch request.
	RepoPath string
	// AllowedAgents is the explicit allow-list. An agent not in it is
	// refused BEFORE any session is created, regardless of whether the
	// runtime would have failed anyway.
	AllowedAgents []string
	// Timeout bounds one whole launch.
	Timeout time.Duration
}

// LaunchRequest is one bounded execution.
type LaunchRequest struct {
	// SessionID is CALLER-MINTED and is what recovery reconciles on.
	SessionID string
	// Agent is an allow-listed agent definition name.
	Agent string
	// Workspace is the authorised worktree, absolute.
	Workspace string
	// Title is a bounded human label for the session.
	Title string
	// Prompt is the bounded objective plus its references.
	Prompt string
}

// SessionState is the runtime's own report about a session.
type SessionState struct {
	ID          string          `json:"id"`
	Agent       string          `json:"agent"`
	Model       SessionModel    `json:"model"`
	Outcome     string          `json:"outcome"`
	Location    SessionLocation `json:"location"`
	Permissions []SessionPerm   `json:"permissions"`
}

// SessionModel is the model the session actually used.
type SessionModel struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
}

// SessionLocation is where the session ran.
type SessionLocation struct {
	Directory string `json:"directory"`
}

// SessionPerm is one session-level permission override.
type SessionPerm struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Effect   string `json:"effect"`
}

// LaunchResult is what one execution actually did.
type LaunchResult struct {
	SessionID  string
	ExitCode   int
	Output     string
	State      *SessionState
	InferredOK bool
}

// LaunchFailureClass is the COMMAND half of a launch failure. It is a
// classification, not a copy of the command's error.
type LaunchFailureClass string

const (
	// LaunchStartFailed says the command could not be started at all.
	LaunchStartFailed LaunchFailureClass = "command_start_failed"
	// LaunchExitFailure says the command started and exited unsuccessfully.
	LaunchExitFailure LaunchFailureClass = "command_exit_failure"
)

// InspectFailureClass is the SESSION half of a launch failure.
type InspectFailureClass string

const (
	// InspectNotFound says the runtime does not know the session.
	InspectNotFound InspectFailureClass = "session_not_found"
	// InspectUnavailable says the runtime could not be asked.
	InspectUnavailable InspectFailureClass = "inspection_failed"
)

// LaunchError preserves the SAFE, structured diagnostics of a launch whose
// native session state could not be read.
//
// IT DELIBERATELY CARRIES NO RAW OUTPUT. stdout and stderr may contain the
// prompt, model output or a credential a misconfigured runtime printed. The
// bytes and their SHA-256 are retained as evidence; the text is not. A
// caller can say WHERE the launch failed without reproducing WHAT it printed.
//
// StateObserved is false unless the runtime actually returned a state.
// InferenceUnknown is true whenever no trustworthy terminal state was
// observed: a failed command does NOT prove the provider was never called.
type LaunchError struct {
	RunFailure       LaunchFailureClass
	ExitCode         int
	InspectFailure   InspectFailureClass
	StateObserved    bool
	InferenceUnknown bool
	OutputBytes      int
	OutputSHA256     string
}

// Error is the bounded, credential-free explanation recorded by the adapter.
func (e *LaunchError) Error() string {
	parts := make([]string, 0, 4)
	switch e.RunFailure {
	case LaunchStartFailed:
		parts = append(parts, "the command could not start")
	case LaunchExitFailure:
		parts = append(parts, fmt.Sprintf("the command exited unsuccessfully (exit %d)", e.ExitCode))
	}
	switch e.InspectFailure {
	case InspectNotFound:
		parts = append(parts, "the native session was not found")
	case InspectUnavailable:
		parts = append(parts, "the native session could not be inspected")
	}
	if !e.StateObserved {
		parts = append(parts, "no session state was observed; inference is unknown")
	} else if e.InferenceUnknown {
		parts = append(parts, "the session state has no terminal outcome; inference is unknown")
	}
	if e.OutputBytes > 0 {
		parts = append(parts, fmt.Sprintf(
			"bounded output %d bytes sha256:%s", e.OutputBytes, e.OutputSHA256))
	}
	if len(parts) == 0 {
		parts = append(parts, "the launch produced no usable session state")
	}
	return "opencode: launch incomplete: " + strings.Join(parts, "; ")
}

// newLaunchError classifies a launch failure without retaining raw output.
func newLaunchError(runErr error, started bool, exitCode int,
	inspectErr error, res *LaunchResult) *LaunchError {

	le := &LaunchError{ExitCode: exitCode}
	if res != nil {
		le.StateObserved = res.State != nil
		le.InferenceUnknown = res.State == nil || res.State.Outcome == ""
		if res.Output != "" {
			sum := sha256.Sum256([]byte(res.Output))
			le.OutputBytes = len(res.Output)
			le.OutputSHA256 = fmt.Sprintf("%x", sum)
		}
	} else {
		le.InferenceUnknown = true
	}
	if runErr != nil {
		if !started {
			le.RunFailure = LaunchStartFailed
		} else {
			le.RunFailure = LaunchExitFailure
		}
	}
	if inspectErr != nil {
		if errors.Is(inspectErr, ErrSessionNotFound) {
			le.InspectFailure = InspectNotFound
		} else {
			le.InspectFailure = InspectUnavailable
		}
	}
	return le
}

// ErrSessionNotFound says the runtime has NEVER HEARD of a session.
//
// It is deliberately distinct from "the runtime could not be asked". The
// adapter's recovery depends on the difference: a session the runtime does
// not know is the SAFE case (nothing was created, so reusing the recorded
// id cannot duplicate anything), whereas an unreachable runtime is the
// AMBIGUOUS case and must stop the retry.
var ErrSessionNotFound = errors.New("opencode: the runtime does not know that session")

// Runtime is the real OpenCode boundary.
type Runtime struct {
	cfg Config
	Now func() time.Time
}

// NewRuntime validates and returns a Runtime.
func NewRuntime(cfg Config) (*Runtime, error) {
	if cfg.Root == "" || cfg.OpenCodeBin == "" {
		return nil, errors.New("opencode: the runtime needs a state directory and a binary")
	}
	if len(cfg.AllowedAgents) == 0 {
		return nil, errors.New("opencode: the runtime has an empty agent allow-list")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Minute
	}
	if err := os.MkdirAll(cfg.Root, 0o700); err != nil {
		return nil, fmt.Errorf("opencode: create the runtime's state directory: %w", err)
	}
	return &Runtime{cfg: cfg, Now: func() time.Time { return time.Now().UTC() }}, nil
}

// Config returns the runtime's configuration.
func (r *Runtime) Config() Config { return r.cfg }

// AgentAllowed reports whether the name is on the explicit allow-list.
//
// This is checked BEFORE a session exists. The measured host behaviour is a
// RUNTIME failure (`AgentNotFoundError`) rather than a create-time refusal,
// so relying on the host would mean creating a session — and burning a run
// — before learning the name was wrong.
func (r *Runtime) AgentAllowed(name string) bool {
	for _, a := range r.cfg.AllowedAgents {
		if a == name {
			return true
		}
	}
	return false
}

// VerifyAgent proves the runtime knows the definition.
//
// THE PERMISSIONS ARE THE FRAMEWORK'S. MPM used to reject a definition whose
// resolved permissions re-opened execution or egress, because the contained
// worker's sandbox was MPM's own and the check was part of that boundary.
// Without that boundary the definition, its permissions and the operator's
// framework configuration are the authority — MPM asking the runtime for an
// effective permission set and overriding it would be exactly the kind of
// application-level substitute for operating-system privilege separation
// this design rejects.
//
// Existence is still checked, because a missing definition would otherwise
// fail AFTER the attempt was recorded, and an operator cannot act on that.
func (r *Runtime) VerifyAgent(ctx context.Context, name string) error {
	if !r.AgentAllowed(name) {
		return fmt.Errorf("opencode: agent %q is not on the allow-list", name)
	}
	agents, err := r.listAgents(ctx)
	if err != nil {
		return err
	}
	for i := range agents {
		if agents[i].ID == name {
			return nil
		}
	}
	return fmt.Errorf("opencode: the runtime does not know an agent named %q", name)
}

type agentInfo struct {
	ID          string        `json:"id"`
	Mode        string        `json:"mode"`
	Permissions []SessionPerm `json:"permissions"`
}

func (r *Runtime) listAgents(ctx context.Context) ([]agentInfo, error) {
	out, err := r.runRead(ctx, r.cfg.OpenCodeBin, "--standalone", "debug", "agents")
	if err != nil {
		return nil, fmt.Errorf("opencode: list the agent definitions: %w", err)
	}
	var agents []agentInfo
	if err := json.Unmarshal([]byte(out), &agents); err != nil {
		return nil, fmt.Errorf("opencode: parse the agent definitions: %w", err)
	}
	return agents, nil
}

// Launch runs the objective and returns what the runtime reported. It NEVER
// derives success from the existence of a session.
//
// The process is an ordinary child of MPM: it runs in the authorised
// workspace, under the operator's environment, with the runtime's own
// configuration and credentials. No namespace, mount or relay is involved.
func (r *Runtime) Launch(ctx context.Context, req LaunchRequest) (*LaunchResult, error) {
	if req.SessionID == "" {
		return nil, errors.New("opencode: a launch needs a caller-minted session id")
	}
	if !r.AgentAllowed(req.Agent) {
		return nil, fmt.Errorf("opencode: agent %q is not on the allow-list", req.Agent)
	}
	if !filepath.IsAbs(req.Workspace) {
		return nil, fmt.Errorf("opencode: the workspace must be absolute, got %q", req.Workspace)
	}

	// The definition must exist before anything is recorded against it.
	if err := r.VerifyAgent(ctx, req.Agent); err != nil {
		return nil, err
	}

	argv := []string{
		"--standalone", "run",
		"--session", req.SessionID,
		"--agent", req.Agent,
	}
	if r.cfg.Model != "" {
		argv = append(argv, "--model", r.cfg.Model)
	}
	argv = append(argv, "--title", req.Title, req.Prompt)

	runCtx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, r.cfg.OpenCodeBin, argv...)
	// THE FRAMEWORK'S OWN ENVIRONMENT. MPM does not scrub, manufacture or
	// isolate it: the operator's configuration, credentials and tool
	// permissions are the contract.
	cmd.Env = os.Environ()
	cmd.Dir = req.Workspace
	// A BOUNDED capture. The output is used only for a size and a digest,
	// so an unbounded buffer would let a looping runtime drive the
	// adapter's memory for no benefit.
	var buf boundedBuffer
	buf.limit = maxCapturedOutput
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	runErr := cmd.Run()
	started := cmd.ProcessState != nil
	out := buf.Bytes()

	res := &LaunchResult{SessionID: req.SessionID, Output: string(out)}
	if started {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	if runErr != nil {
		res.InferredOK = false
	}

	// The runtime's own report is authoritative. A session that exists
	// proves nothing; its `outcome` does.
	state, inspectErr := r.Inspect(ctx, req.SessionID)
	if inspectErr != nil {
		res.InferredOK = false
		// THE FAILURE IS NOT DISCARDED. The command may have failed, the
		// inspection may have failed, or both — and the raw output may
		// contain anything. Preserve the CLASSIFICATION, exit code and
		// bounded-output digest, never the text.
		return res, newLaunchError(runErr, started, res.ExitCode, inspectErr, res)
	}
	if state == nil {
		res.InferredOK = false
		return res, newLaunchError(runErr, started, res.ExitCode,
			errors.New("the runtime returned no state"), res)
	}
	res.State = state
	res.InferredOK = state.Outcome == "succeeded"

	// A COMMAND FAILURE WITH A RECORDED TERMINAL STATE IS NOT A LAUNCH
	// FAILURE: the runtime's own report is authoritative, and the adapter
	// records what it said. It is also not a success unless the state says
	// so.
	return res, nil
}

// Inspect reads a session's OWN report.
func (r *Runtime) Inspect(ctx context.Context, sessionID string) (*SessionState, error) {
	out, err := r.runRead(ctx, r.cfg.OpenCodeBin, "--standalone", "api", "session.list")
	if err != nil {
		return nil, err
	}
	var env struct {
		Data []SessionState `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		return nil, fmt.Errorf("opencode: parse the session list: %w", err)
	}
	for i := range env.Data {
		if env.Data[i].ID == sessionID {
			return &env.Data[i], nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrSessionNotFound, sessionID)
}

// runRead runs one read-only runtime command with MPM's own working
// directory and the framework's environment.
func (r *Runtime) runRead(ctx context.Context, bin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = os.Environ()
	cmd.Dir = r.cfg.Root
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("%s: %w: %s", filepath.Base(bin), err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return string(out), nil
}

// maxCapturedOutput bounds the runtime output the adapter keeps.
const maxCapturedOutput = 1 << 20

// boundedBuffer keeps at most limit bytes and silently discards the rest.
// It is used where the output is only digested, never interpreted.
type boundedBuffer struct {
	buf   []byte
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	room := b.limit - len(b.buf)
	if room > 0 {
		if len(p) < room {
			room = len(p)
		}
		b.buf = append(b.buf, p[:room]...)
	}
	return len(p), nil
}

func (b *boundedBuffer) Bytes() []byte { return b.buf }
