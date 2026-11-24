// runtime.go — the only code in MPM that talks to OpenCode.
//
// # THE ARCHITECTURAL RULE
//
// This is a TRANSLATOR between MPM assignment state and OpenCode runtime
// state. It does not implement inference, model selection, tool execution
// or a conversation loop — OpenCode owns all of that. What lives here is:
//
//   - which agent definition may be used, and what permissions it really
//     has (agents.go's checks, below);
//   - how a session is created or resumed (the caller-minted id);
//   - what the runtime actually did (its own `outcome`, not an inference
//     from the existence of a session);
//   - whether the runtime ran where it was told to.
//
// THE COMMANDS, AND WHY THEY ARE THE ONES MEASURED
//
//	opencode run --session <id> --agent <a> --model <m> --title <t> <prompt>
//
//	  `--session <id>` is the idempotency lever: it creates the session
//	  with the CALLER's id if it does not exist, and CONTINUES it if it
//	  does. `session.create` always mints its own id and is not
//	  idempotent, so it is not used.
//
//	opencode --standalone api session.list
//
//	  The read path for the runtime's OWN report: `outcome` (succeeded /
//	  failed), `location.directory`, `agent`, `model` and the session's
//	  `permissions`. The adapter verifies all four before accepting a
//	  result.
//
//	opencode --standalone debug agents
//
//	  The only reliable way to read an agent definition's EFFECTIVE
//	  permission set. `agent.get` answers 404 even for a known agent, so
//	  the debug surface is what this package uses.
package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// IsolatedEnv is the environment a runtime process receives OUTSIDE the
// containment (for the read-only inspection commands). Inside the
// containment the environment is Spec.Env, which is shorter still.
//
// A WORKER carries no provider credential: inference goes through the
// sidecar, so it authenticates to a socket, not to a provider. A REVIEWER
// carries its own dedicated provider credential (Config.ProviderKey), and
// that pair is the only provider credential this function ever adds.
func IsolatedEnv(cfg Config) []string {
	env := []string{
		"HOME=" + filepath.Join(cfg.Root, "home"),
		"PATH=" + sandboxPath,
		"XDG_CONFIG_HOME=" + filepath.Join(cfg.Root, "ocfg"),
		"XDG_DATA_HOME=" + filepath.Join(cfg.Root, "odata"),
		"XDG_CACHE_HOME=" + filepath.Join(cfg.Root, "ocache"),
		"XDG_STATE_HOME=" + filepath.Join(cfg.Root, "ostate"),
		"LANG=C.UTF-8",
		"TERM=dumb",
	}
	// A REVIEWER's provider credential is the one exception. It is passed
	// to the OpenCode PROCESS only; `sandbox_exec` scrubs it from the
	// child environment and bubblewrap clears the command environment.
	// The worker's sidecar credential never reaches this function.
	if cfg.ProviderKey != "" && cfg.ProviderKeyEnv != "" {
		env = append(env, cfg.ProviderKeyEnv+"="+cfg.ProviderKey)
	}
	return env
}

// Config is one OpenCode integration's fixed configuration.
type Config struct {
	// Root is the sandbox root.
	Root string
	// OpenCodeBin is the runtime binary.
	OpenCodeBin string
	// MPMBin is this MPM binary, used for the relay.
	MPMBin string
	// Model is the runtime model identifier, "provider/model".
	Model string
	// RepoPath is the repository a REVIEW examines. Unused by the worker
	// path, whose workspace comes from the launch request.
	RepoPath string
	// PluginPath is the MPM OpenCode plugin build. The REVIEWER's
	// `sandbox_exec` comes from it, so an empty value means a review
	// cannot start.
	PluginPath string
	// AllowedAgents is the explicit allow-list. An agent not in it is
	// refused BEFORE any session is created, regardless of whether the
	// runtime would have failed anyway.
	AllowedAgents []string
	// Sidecar is the WORKER's sidecar configuration. Its APIKey stays in
	// the sidecar process; a reviewer runtime leaves Sidecar empty and
	// carries its own ProviderKey instead.
	Sidecar SidecarConfig
	// ProviderKey and ProviderKeyEnv are the REVIEWER's dedicated
	// provider credential. They are set only for the reviewer runtime;
	// IsolatedEnv passes the pair to the OpenCode process. The
	// `sandbox_exec` plugin scrubs the variable before spawning its
	// helper, and bubblewrap clears the command environment.
	ProviderKey    string
	ProviderKeyEnv string
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
		return nil, errors.New("opencode: the runtime needs a root and a binary")
	}
	// The MPM binary is the RELAY, which only the WORKER path needs. A
	// review does not run the worker containment, so it has no relay —
	// requiring one made `execute-review` impossible to construct.
	if cfg.RepoPath == "" && cfg.MPMBin == "" {
		return nil, errors.New("opencode: the worker runtime needs the MPM binary for its relay")
	}
	if cfg.Model == "" {
		return nil, errors.New("opencode: the runtime needs a model")
	}
	if len(cfg.AllowedAgents) == 0 {
		return nil, errors.New("opencode: the runtime has an empty agent allow-list")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Minute
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

// VerifyAgent asks the RUNTIME what an agent definition's effective
// permissions are, and refuses a definition whose surface is wider than
// this worker is allowed to have.
//
// It is deliberately a live query rather than a parse of the markdown: the
// host prepends its own global rules and resolves last-match-wins, so the
// definition's text is not the effective set. A definition that says "deny
// shell" can still end up with shell allowed if a later rule re-allows it,
// and only the resolved list shows that.
func (r *Runtime) VerifyAgent(ctx context.Context, name string) error {
	if !r.AgentAllowed(name) {
		return fmt.Errorf("opencode: agent %q is not on the allow-list", name)
	}
	agents, err := r.listAgents(ctx)
	if err != nil {
		return err
	}
	var found *agentInfo
	for i := range agents {
		if agents[i].ID == name {
			found = &agents[i]
			break
		}
	}
	if found == nil {
		return fmt.Errorf("opencode: the runtime does not know an agent named %q", name)
	}
	// The worker's effective surface must not re-open execution or
	// egress. A deny that a later rule overrides is not a deny.
	for _, p := range found.Permissions {
		if p.Effect != "allow" {
			continue
		}
		switch p.Action {
		case "shell", "execute", "browser", "webfetch", "websearch", "subagent":
			return fmt.Errorf(
				"opencode: agent %q has an EFFECTIVE allow for %q, which this worker may not have",
				name, p.Action)
		}
	}
	return nil
}

type agentInfo struct {
	ID          string        `json:"id"`
	Mode        string        `json:"mode"`
	Permissions []SessionPerm `json:"permissions"`
}

func (r *Runtime) listAgents(ctx context.Context) ([]agentInfo, error) {
	out, err := r.runIsolated(ctx, r.cfg.OpenCodeBin, "--standalone", "debug", "agents")
	if err != nil {
		return nil, fmt.Errorf("opencode: list the agent definitions: %w", err)
	}
	var agents []agentInfo
	if err := json.Unmarshal([]byte(out), &agents); err != nil {
		return nil, fmt.Errorf("opencode: parse the agent definitions: %w", err)
	}
	return agents, nil
}

// Launch runs the objective in the containment and returns what the runtime
// reported. It NEVER derives success from the existence of a session.
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

	// ORDER MATTERS. The isolated configuration — including the agent
	// definition — is written FIRST, because VerifyAgent asks the RUNTIME
	// what the definition's effective permissions are, and the runtime can
	// only answer about a definition it has been given.
	if err := r.writeSandboxConfig(req.Agent); err != nil {
		return nil, err
	}
	if err := r.VerifyAgent(ctx, req.Agent); err != nil {
		return nil, err
	}

	spec := Spec{
		Root:           r.cfg.Root,
		Workspace:      req.Workspace,
		OpenCodeBin:    r.cfg.OpenCodeBin,
		MPMBin:         r.cfg.MPMBin,
		SidecarSocket:  r.cfg.Sidecar.SocketPath,
		SidecarBaseURL: "http://" + RelayAddr + "/v1",
		Env: map[string]string{
			"HOME":            PrefixHome,
			"TMPDIR":          PrefixTmp,
			"PATH":            sandboxPath,
			"XDG_CONFIG_HOME": PrefixOCfg,
			"XDG_DATA_HOME":   PrefixOData,
			"XDG_CACHE_HOME":  PrefixOCache,
			"XDG_STATE_HOME":  PrefixOState,
			"LANG":            "C.UTF-8",
			"TERM":            "dumb",
			// The relay is started by the launcher, not by this
			// command; the variable is what makes the MPM binary in
			// the namespace run the relay.
			RelayEnvVar: PrefixSocket,
		},
		RelayCommand: []string{"/opt/mpm/mpm"},
		Command: []string{
			"/opt/opencode/opencode", "--standalone", "run",
			"--session", req.SessionID,
			"--agent", req.Agent,
			"--model", r.cfg.Model,
			"--title", req.Title,
			req.Prompt,
		},
	}

	argv, err := spec.Argv()
	if err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	// THE ENVIRONMENT IS EXPLICIT. This is the fix for the measured leak:
	// bubblewrap's own init inherits this, and `--clearenv` would
	// otherwise leave the adapter's whole environment readable at
	// /proc/1/environ inside the namespace.
	cmd.Env = []string{
		"PATH=" + sandboxPath,
		"HOME=" + filepath.Join(r.cfg.Root, "home"),
	}
	// A BOUNDED capture. The output is used only for a size and a digest,
	// so an unbounded buffer would let a looping runtime drive the
	// adapter's memory for no benefit.
	var buf boundedBuffer
	buf.limit = maxCapturedOutput
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	runErr := cmd.Run()
	out := buf.Bytes()

	res := &LaunchResult{
		SessionID: req.SessionID,
		Output:    string(out),
	}
	if cmd.ProcessState != nil {
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
		if runErr == nil {
			return res, fmt.Errorf("opencode: the run finished but its state could not be read: %w", inspectErr)
		}
		return res, nil
	}
	res.State = state
	res.InferredOK = state.Outcome == "succeeded"

	if runErr != nil && res.ExitCode != 0 {
		return res, nil
	}
	return res, nil
}

// Inspect reads a session's OWN report.
func (r *Runtime) Inspect(ctx context.Context, sessionID string) (*SessionState, error) {
	out, err := r.runIsolated(ctx, r.cfg.OpenCodeBin, "--standalone", "api", "session.list")
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

// runIsolated runs one read-only runtime command in the isolated data tree.
func (r *Runtime) runIsolated(ctx context.Context, bin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = IsolatedEnv(r.cfg)
	cmd.Dir = filepath.Join(r.cfg.Root, "home")
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

// writeSandboxConfig writes the isolated OpenCode configuration: the
// allowed agent definitions, and ONE custom provider that points at the
// relay. There is no provider credential here — the runtime authenticates
// to a socket.
func (r *Runtime) writeSandboxConfig(agent string) error {
	dir := filepath.Join(r.cfg.Root, "ocfg", "opencode")
	if err := os.MkdirAll(filepath.Join(dir, "agents"), 0o700); err != nil {
		return fmt.Errorf("opencode: create the config directory: %w", err)
	}

	cfg := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"providers": map[string]any{
			"mpm-sidecar": map[string]any{
				"name":    "MPM Sidecar",
				"package": "@opencode/ai/providers/openai-compatible",
				"settings": map[string]any{
					"baseURL": "http://" + RelayAddr + "/v1",
				},
				"models": map[string]any{
					r.modelID(): map[string]any{
						"name":  r.modelID(),
						"limit": map[string]any{"context": 200000},
					},
				},
			},
		},
	}
	body, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("opencode: encode the sandbox config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "opencode.json"), body, 0o600); err != nil {
		return fmt.Errorf("opencode: write the sandbox config: %w", err)
	}
	// The agent definition the sandbox receives is the one COMPILED INTO
	// this binary. A definition read from the host would be an input the
	// adapter never reviewed.
	if agent != WorkerAgentName {
		return fmt.Errorf("opencode: %q is not the worker definition this adapter installs", agent)
	}
	if err := os.WriteFile(
		filepath.Join(dir, "agents", WorkerAgentName+".md"),
		WorkerAgentDefinition, 0o600); err != nil {
		return fmt.Errorf("opencode: write the worker definition: %w", err)
	}
	return nil
}

// modelID is the part of "provider/model" after the slash, which is what
// the sidecar's allow-list is keyed on.
func (r *Runtime) modelID() string {
	if i := strings.LastIndex(r.cfg.Model, "/"); i >= 0 {
		return r.cfg.Model[i+1:]
	}
	return r.cfg.Model
}

// sandboxPath is the PATH the containment and the isolated read commands
// receive.
//
// IT INCLUDES /usr/sbin AND /sbin. The first version did not, and `ip` lives
// in /usr/sbin on Debian, Ubuntu and RHEL-family hosts — so on a
// non-merged-/usr host the launcher's `ip link set lo up` would not be found
// and the containment would exit before bubblewrap ever ran. (On a
// merged-/usr host /usr/bin/ip exists, which is why the omission was
// invisible here.)
const sandboxPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

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
