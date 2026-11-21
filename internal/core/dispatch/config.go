package dispatch

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
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
//	RuntimeTimeout    the longest the runtime may run
//	LeaseMargin       the headroom the lease keeps beyond that timeout
//	MaxDispatchAttempts  how many attempts ONE assignment may consume
//	RetryBackoff      how long a failed assignment waits before it is retried
//	CandidateLimit    how many assignments ONE run will even look at
//	Owner             the identity the dispatcher claims under
//
// Concurrency is a RUN limit, and the tree's own max_concurrency is the
// DURABLE one: the service enforces the latter atomically, so a second
// dispatcher cannot exceed it no matter what this says.
type Config struct {
	// Enabled turns autonomous dispatch on. It is false by default.
	Enabled bool `json:"enabled"`

	// Concurrency is how many assignments one run dispatches. It must be
	// at least 1 and at most MaxConcurrency.
	Concurrency int `json:"concurrency"`

	// Owner is the identity the dispatcher claims under. It names the
	// dispatcher, not an agent: the dispatcher is MPM, and the agent
	// definition is chosen by the ROLE.
	Owner string `json:"owner"`

	// LeaseSeconds is the lease the dispatcher takes.
	LeaseSeconds int64 `json:"lease_seconds"`

	// RuntimeTimeout bounds one runtime invocation.
	RuntimeTimeout time.Duration `json:"runtime_timeout"`

	// LeaseMargin is the headroom the lease keeps beyond the runtime
	// timeout, so the adapter can observe the outcome and record it.
	LeaseMargin time.Duration `json:"lease_margin"`

	// MaxDispatchAttempts bounds how many attempts one assignment may
	// consume, so a repeatedly failing assignment cannot monopolise the
	// dispatcher. Zero means the dispatcher does not bound it.
	MaxDispatchAttempts int `json:"max_dispatch_attempts"`

	// RetryBackoff is how long a failed assignment waits before the
	// dispatcher considers it again.
	RetryBackoff time.Duration `json:"retry_backoff"`

	// CandidateLimit is how many assignments one run will look at.
	CandidateLimit int `json:"candidate_limit"`

	// DrainTimeout bounds the wait for a running session to report a
	// terminal outcome.
	DrainTimeout time.Duration `json:"drain_timeout"`

	// DrainPollInterval is how often that wait looks.
	DrainPollInterval time.Duration `json:"drain_poll_interval"`

	// WorkspaceRoot is the directory the dispatcher creates per-assignment
	// worktrees under. It is operator-configured: the dispatcher does not
	// choose a workspace it was not told about.
	WorkspaceRoot string `json:"workspace_root"`

	// WorkerAgent is the agent definition an IMPLEMENTER runs under. It
	// is the file-edit-only worker definition, and nothing else.
	WorkerAgent string `json:"worker_agent"`

	// ReviewerAgent is the agent definition a REVIEWER runs under. It is
	// the pinned, default-deny `mpm-reviewer`, and nothing else. It is
	// recorded here so an operator can SEE that the reviewer is not the
	// worker; the value is not free to be the worker, because the
	// review path ignores it and uses the pinned definition.
	ReviewerAgent string `json:"reviewer_agent"`

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
		Enabled:             false,
		Concurrency:         1,
		Owner:               "mpm-dispatcher",
		LeaseSeconds:        0,
		RuntimeTimeout:      0,
		LeaseMargin:         0,
		MaxDispatchAttempts: 0,
		RetryBackoff:        0,
		CandidateLimit:      50,
		DrainTimeout:        0,
		DrainPollInterval:   0,
		WorkspaceRoot:       "",
		WorkerAgent:         "mpm-worker",
		ReviewerAgent:       "mpm-reviewer",
		ReviewRepoPath:      "",
		Model:               "",
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
	if c.RuntimeTimeout <= 0 {
		return fmt.Errorf("dispatch: the configuration is enabled but the runtime timeout is %s; it must be positive",
			c.RuntimeTimeout)
	}
	if c.LeaseMargin < 0 {
		return fmt.Errorf("dispatch: the configuration is enabled but the lease margin is %s", c.LeaseMargin)
	}
	// THE LEASE MUST COVER THE RUNTIME WINDOW.
	//
	// The dispatcher never renews during a run — PR 2's Renew advances
	// the claim epoch, and PR 3's attempt writes are fenced on the epoch,
	// so a renewal mid-run would make the running attempt unable to
	// record its result and the next dispatch would duplicate the work.
	// So the lease is sized ONCE, and it has to cover the whole window.
	need := int64(c.RuntimeTimeout.Seconds()) + int64(c.LeaseMargin.Seconds())
	if int64(c.LeaseSeconds) < need {
		return fmt.Errorf(
			"dispatch: the configuration is enabled but the lease of %ds does not cover the %ds runtime window plus %ds of margin",
			c.LeaseSeconds, int64(c.RuntimeTimeout.Seconds()), int64(c.LeaseMargin.Seconds()))
	}
	if c.MaxDispatchAttempts < 0 {
		return fmt.Errorf("dispatch: the configuration is enabled but the dispatch-attempt limit is %d",
			c.MaxDispatchAttempts)
	}
	if c.RetryBackoff < 0 {
		return fmt.Errorf("dispatch: the configuration is enabled but the retry backoff is %s", c.RetryBackoff)
	}
	if c.CandidateLimit <= 0 {
		return fmt.Errorf("dispatch: the configuration is enabled but the candidate limit is %d; it must be positive",
			c.CandidateLimit)
	}
	if c.DrainTimeout < 0 || c.DrainPollInterval < 0 {
		return fmt.Errorf("dispatch: the configuration is enabled but the drain window is negative")
	}
	if c.DrainTimeout > 0 && c.DrainPollInterval <= 0 {
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
	if c.WorkerAgent == c.ReviewerAgent {
		return fmt.Errorf(
			"dispatch: the worker agent %q and the reviewer agent %q are the same definition; "+
				"the reviewer runs under its own default-deny definition and never the worker's",
			c.WorkerAgent, c.ReviewerAgent)
	}
	if c.ReviewRepoPath == "" {
		return fmt.Errorf("dispatch: the configuration is enabled but names no repository to review")
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
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return DefaultConfig(), fmt.Errorf(
			"dispatch: the configuration at %s is not valid JSON: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return DefaultConfig(), fmt.Errorf("dispatch: the configuration at %s is not usable: %w", path, err)
	}
	return cfg, nil
}
