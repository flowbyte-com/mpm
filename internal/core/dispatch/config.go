package dispatch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// The dispatcher's configuration.
//
// # DISABLED BY DEFAULT
//
// Autonomous dispatch is OFF unless an operator explicitly enables it.
// There is no environment variable that turns it on by accident, no
// "installed means enabled" rule, and no default that is generous.
//
// A MISSING configuration file means the dispatcher is off. A PRESENT but
// INVALID configuration file is an ERROR, because a file that says
// "enabled" and cannot be understood is not a reason to dispatch
// something, and it is not a reason to guess either.
//
// # WHAT THE LIMITS MEAN
//
//	Concurrency        how many assignments ONE run may dispatch
//	LeaseSeconds      how long the dispatcher's claim lasts
//	RuntimeTimeoutSeconds  the longest the runtime may run
//	LeaseMarginSeconds    the headroom the lease keeps beyond that timeout
//	MaxDispatchAttempts    how many attempts ONE assignment may consume
//	RetryBackoffSeconds   how long a failed assignment waits before a retry
//	CandidateLimit        how many assignments ONE run will even look at
//	Owner                 the identity the dispatcher claims under
//
// EVERY DURATION IS IN SECONDS. A configuration file that mixes seconds
// with "10m" is a configuration file that half of MPM cannot read, and a
// duration this file cannot parse is a duration this file does not
// have.
//
// Concurrency is a RUN limit, and the tree's own max_concurrency is the
// DURABLE one: the service enforces the latter atomically, so a second
// dispatcher cannot exceed it no matter what this says.
type Config struct {
	// Enabled turns autonomous dispatch on. It is false by default.
	Enabled bool `json:"enabled"`

	// Concurrency is how many assignments one run dispatches. It must be
	// at least 1. The tree's own max_concurrency is the DURABLE limit,
	// and the service enforces it atomically.
	Concurrency int `json:"concurrency"`

	// Owner is the identity the dispatcher claims under. It names the
	// dispatcher, not an agent: the dispatcher is MPM, and the agent
	// definition is chosen by the ROLE.
	Owner string `json:"owner"`

	// LeaseSeconds is the lease the dispatcher takes.
	LeaseSeconds int64 `json:"lease_seconds"`

	// RuntimeTimeoutSeconds bounds one runtime invocation.
	RuntimeTimeoutSeconds int64 `json:"runtime_timeout_seconds"`

	// LeaseMarginSeconds is the headroom the lease keeps beyond the
	// runtime timeout, so the adapter can observe the outcome and
	// record it.
	LeaseMarginSeconds int64 `json:"lease_margin_seconds"`

	// MaxDispatchAttempts bounds how many attempts one assignment may
	// consume, so a repeatedly failing assignment cannot monopolise the
	// dispatcher. Zero means the dispatcher does not bound it.
	MaxDispatchAttempts int `json:"max_dispatch_attempts"`

	// RetryBackoffSeconds is how long a failed assignment waits before
	// the dispatcher considers it again.
	RetryBackoffSeconds int64 `json:"retry_backoff_seconds"`

	// CandidateLimit is how many assignments one run will look at.
	CandidateLimit int `json:"candidate_limit"`

	// DrainTimeoutSeconds bounds the wait for a running session to
	// report a terminal outcome.
	DrainTimeoutSeconds int64 `json:"drain_timeout_seconds"`

	// DrainPollIntervalSeconds is how often that wait looks.
	DrainPollIntervalSeconds int64 `json:"drain_poll_interval_seconds"`

	// WorkspaceRoot is the directory the dispatcher creates per-assignment
	// worktrees under. It is operator-configured: the dispatcher does not
	// choose a workspace it was not told about.
	WorkspaceRoot string `json:"workspace_root"`

	// WorkerAgent is the agent definition an IMPLEMENTER runs under. It
	// is the file-edit-only worker definition, and nothing else.
	WorkerAgent string `json:"worker_agent"`

	// ReviewerAgent is the agent definition a REVIEWER runs under. It is
	// the pinned, default-deny `mpm-reviewer`, and nothing else.
	//
	// It is recorded here so an operator can SEE that the reviewer is not
	// the worker. It is not free to be anything else: the runtime's
	// allow-list is pinned to the two definitions MPM ships, so a
	// configuration naming a third one would be refused by the runtime
	// AFTER the claim — and the assignment would be retried forever,
	// because a refusal that creates no attempt never trips the
	// dispatch-attempt cap.
	ReviewerAgent string `json:"reviewer_agent"`

	// ReviewerModel is the model a REVIEWER runs under.
	//
	// It is SEPARATE from the worker's model, and it has to be: the
	// worker's model is the sidecar's local id ("mpm-sidecar/…"), which
	// only means anything inside the worker's namespace with the
	// sidecar's socket. The reviewer's sandbox has no provider and no
	// sidecar, so it names a real provider/model — and naming the
	// worker's id there would ask the reviewer for a model its own
	// configuration does not define.
	ReviewerModel string `json:"reviewer_model"`

	// ReviewRepoPath is the repository the reviewer examines. It must be
	// a CLEAN tree, because the review path pins an exact revision and
	// refuses a worktree with uncommitted changes: there is no revision
	// to describe otherwise.
	ReviewRepoPath string `json:"review_repo_path"`

	// Model is the runtime model the dispatch records on its attempts.
	Model string `json:"model"`
}

// DefaultConfig is the configuration of a dispatcher that does nothing.
//
// It is what a missing configuration file yields, and it is SAFE: the
// dispatcher is off, and turning it on requires an explicit edit.
func DefaultConfig() Config {
	return Config{
		Enabled:                  false,
		Concurrency:              1,
		Owner:                    "mpm-dispatcher",
		LeaseSeconds:             0,
		RuntimeTimeoutSeconds:    0,
		LeaseMarginSeconds:       0,
		MaxDispatchAttempts:      0,
		RetryBackoffSeconds:      0,
		CandidateLimit:           50,
		DrainTimeoutSeconds:      0,
		DrainPollIntervalSeconds: 0,
		WorkspaceRoot:            "",
		WorkerAgent:              "mpm-worker",
		ReviewerAgent:            "mpm-reviewer",
		ReviewerModel:            DefaultReviewModel,
		ReviewRepoPath:           "",
		Model:                    "",
	}
}

// Validate checks the configuration.
//
// A DISABLED configuration is always valid: it does nothing, so there is
// nothing to be inconsistent about.
//
// An ENABLED configuration must be COMPLETE. Every limit it needs is
// stated, so there is no value the dispatcher can invent for itself.
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.Concurrency < 1 {
		return fmt.Errorf("dispatch: the configuration is enabled but the concurrency is %d; it must be at least 1",
			c.Concurrency)
	}
	if c.Owner == "" {
		return fmt.Errorf("dispatch: the configuration is enabled but names no owner to claim under")
	}
	if c.LeaseSeconds <= 0 {
		return fmt.Errorf("dispatch: the configuration is enabled but the lease is %d seconds; it must be positive",
			c.LeaseSeconds)
	}
	if c.RuntimeTimeoutSeconds <= 0 {
		return fmt.Errorf("dispatch: the configuration is enabled but the runtime timeout is %ds; it must be positive",
			c.RuntimeTimeoutSeconds)
	}
	if c.LeaseMarginSeconds < 0 {
		return fmt.Errorf("dispatch: the configuration is enabled but the lease margin is %ds",
			c.LeaseMarginSeconds)
	}
	// THE LEASE MUST COVER THE RUNTIME WINDOW.
	//
	// The dispatcher never renews during a run — PR 2's Renew advances
	// the claim epoch, and PR 3's attempt writes are fenced on the epoch,
	// so a renewal mid-run would make the running attempt unable to
	// record its result and the next dispatch would duplicate the work.
	// So the lease is sized ONCE, and it has to cover the whole window.
	need := c.RuntimeTimeoutSeconds + c.LeaseMarginSeconds
	if int64(c.LeaseSeconds) < need {
		return fmt.Errorf(
			"dispatch: the configuration is enabled but the lease of %ds does not cover the %ds runtime window plus %ds of margin",
			c.LeaseSeconds, c.RuntimeTimeoutSeconds, c.LeaseMarginSeconds)
	}
	if c.MaxDispatchAttempts < 0 {
		return fmt.Errorf("dispatch: the configuration is enabled but the dispatch-attempt limit is %d",
			c.MaxDispatchAttempts)
	}
	if c.RetryBackoffSeconds < 0 {
		return fmt.Errorf("dispatch: the configuration is enabled but the retry backoff is %ds",
			c.RetryBackoffSeconds)
	}
	if c.CandidateLimit <= 0 {
		return fmt.Errorf("dispatch: the configuration is enabled but the candidate limit is %d; it must be positive",
			c.CandidateLimit)
	}
	if c.DrainTimeoutSeconds < 0 || c.DrainPollIntervalSeconds < 0 {
		return fmt.Errorf("dispatch: the configuration is enabled but the drain window is negative")
	}
	if c.DrainTimeoutSeconds > 0 && c.DrainPollIntervalSeconds <= 0 {
		return fmt.Errorf("dispatch: the configuration is enabled with a drain window but no poll interval")
	}
	if c.WorkspaceRoot == "" {
		return fmt.Errorf("dispatch: the configuration is enabled but names no workspace root")
	}
	if c.WorkerAgent == "" {
		return fmt.Errorf("dispatch: the configuration is enabled but names no worker agent definition")
	}
	if c.ReviewerAgent == "" {
		return fmt.Errorf("dispatch: the configuration is enabled but names no reviewer agent definition")
	}
	// THE TWO DEFINITIONS ARE PINNED, AND THE PIN IS ENFORCED HERE.
	//
	// The runtime's allow-list names exactly the two definitions MPM
	// ships, so a configuration naming a third one would be REFUSED by
	// the runtime — after the claim, and before any attempt exists. That
	// refusal never trips the dispatch-attempt cap, so the assignment
	// would be retried forever and its live claim would starve the
	// tree's concurrency budget. Refusing the configuration is the same
	// decision, made before anything is claimed.
	if c.WorkerAgent != DefaultWorkerAgent {
		return fmt.Errorf(
			"dispatch: the worker agent is %q, and the containment's allow-list is pinned to %q",
			c.WorkerAgent, DefaultWorkerAgent)
	}
	if c.ReviewerAgent != DefaultReviewerAgent {
		return fmt.Errorf(
			"dispatch: the reviewer agent is %q, and the review path is pinned to %q",
			c.ReviewerAgent, DefaultReviewerAgent)
	}
	// THE REVIEWER CANNOT BE THE WORKER, and the PIN is what proves it.
	//
	// There is no separate check for "the same definition for both",
	// because the pin above already makes it impossible: the worker is
	// `mpm-worker` and the reviewer is `mpm-reviewer`, and a
	// configuration that names the same definition for both names
	// neither of them — so it is refused by the pin, with a message that
	// says what the pin is.
	if c.ReviewRepoPath == "" {
		return fmt.Errorf("dispatch: the configuration is enabled but names no repository to review")
	}
	// THE PATHS MUST BE ABSOLUTE, because the CONTAINMENT requires it.
	//
	// The sandbox's root and the workspace it contains are resolved
	// against the filesystem, and a relative path would make the
	// containment's own guarantee depend on the working directory. The
	// worker path needs it too — the adapter refuses a relative
	// workspace — but the reviewer path only found out at the factory,
	// after the claim. Refusing it here is the same decision, made
	// before anything is claimed.
	if !filepath.IsAbs(c.WorkspaceRoot) {
		return fmt.Errorf(
			"dispatch: the workspace root %q is not an absolute path", c.WorkspaceRoot)
	}
	if !filepath.IsAbs(c.ReviewRepoPath) {
		return fmt.Errorf(
			"dispatch: the repository to review %q is not an absolute path", c.ReviewRepoPath)
	}
	if c.ReviewerModel == "" {
		return fmt.Errorf("dispatch: the configuration is enabled but names no reviewer model")
	}
	return nil
}

// LoadConfig reads the dispatcher's configuration.
//
// A MISSING file yields the disabled default and no error: that is the
// safe reading, and it is what keeps a freshly installed MPM from
// dispatching anything.
//
// A PRESENT file that cannot be read or understood is an ERROR. The
// operator wrote something and it must be said what is wrong with it,
// rather than the dispatcher deciding that a broken file means "off" or
// "on with sensible defaults".
func LoadConfig(path string) (Config, error) {
	if path == "" {
		return DefaultConfig(), nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultConfig(), nil
		}
		return DefaultConfig(), fmt.Errorf("dispatch: read the configuration at %s: %w", path, err)
	}
	cfg := DefaultConfig()
	// AN UNKNOWN KEY IS AN ERROR.
	//
	// A silently ignored key is a typo the operator cannot see: they
	// wrote "lease_second" and the dispatcher used the default, and the
	// difference between the two is the difference between a lease that
	// covers the runtime window and one that does not. So the file has
	// to say what it means.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return DefaultConfig(), fmt.Errorf(
			"dispatch: the configuration at %s is not usable JSON: %w", path, err)
	}
	// NOTHING AFTER THE OBJECT.
	//
	// A decoder consumes ONE value and says nothing about what follows
	// it, so a file with a valid object and then some other content
	// would decode and be accepted. A configuration file is either one
	// object or it is not a configuration file: a half-read file is a
	// file whose meaning depends on where the reader stopped.
	if dec.More() {
		return DefaultConfig(), fmt.Errorf(
			"dispatch: the configuration at %s has content after the object", path)
	}
	if err := cfg.Validate(); err != nil {
		return DefaultConfig(), fmt.Errorf("dispatch: the configuration at %s is not usable: %w", path, err)
	}
	return cfg, nil
}
