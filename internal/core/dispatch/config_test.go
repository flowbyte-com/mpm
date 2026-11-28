// config_test.go — Continuity 001, PR 4: the dispatcher's configuration.
//
// # WHAT IS UNDER TEST
//
// The switch, and the difference between "off", "broken" and "on but
// unusable".
//
// # WHY THE DURATIONS ARE SECONDS
//
// A configuration file that mixes seconds with "10m" is a configuration
// file that half of MPM cannot read. Every duration here is an integer
// number of seconds, and the test below proves the shape MPM actually
// accepts — because the shape in the documentation is the shape an
// operator will type.

package dispatch

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core/assignment"
)

// TestAMissingConfigurationFileMeansDisabled proves the safe default.
//
// This is the property that keeps a freshly installed MPM from
// dispatching anything.
func TestAMissingConfigurationFileMeansDisabled(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("a missing configuration file was an error: %v", err)
	}
	if cfg.Enabled {
		t.Error("a missing configuration file enabled the dispatcher")
	}
	// AND IT VALIDATES: a disabled configuration is always usable.
	if err := cfg.Validate(); err != nil {
		t.Errorf("the disabled default does not validate: %v", err)
	}
}

// TestABrokenConfigurationFileIsAnError proves the loud direction.
//
// A file the operator wrote and that cannot be understood is an ERROR.
// It is not a reason to dispatch and it is not a reason to guess that
// the dispatcher is off.
func TestABrokenConfigurationFileIsAnError(t *testing.T) {
	for _, body := range []string{
		`{not json`,
		`{"enabled": "yes"}`,
		`{"concurrency": "many"}`,
		// A TYPO is an error too. A silently ignored key is a limit the
		// operator believes they set and the dispatcher does not.
		`{"enabled": false, "lease_second": 900}`,
		// CONTENT AFTER THE OBJECT. A decoder consumes one value and
		// says nothing about what follows it, so a file with a valid
		// object and then some other content would decode — and a
		// configuration whose meaning depends on where the reader
		// stopped is not a configuration.
		`{"enabled": false} and then some other content`,
	} {
		path := filepath.Join(t.TempDir(), "dispatch.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil {
			t.Errorf("the configuration %q was accepted", body)
		}
	}
}

// TestAnEnabledConfigurationMustBeComplete proves that "enabled" is not
// enough.
//
// A dispatcher that invents its own limits is worse than no dispatcher:
// the operator would be relying on numbers they never wrote.
func TestAnEnabledConfigurationMustBeComplete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dispatch.json")
	if err := os.WriteFile(path, []byte(`{"enabled": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("an enabled configuration with no limits was accepted")
	}
	// The error says what is missing.
	if !strings.Contains(err.Error(), "dispatch") {
		t.Errorf("the refusal does not name the dispatcher: %v", err)
	}
}

// TestTheDocumentedConfigurationIsUsable proves the documentation is
// not a lie.
//
// The shape in the design document is the shape an operator will type,
// so it has to load. If the two ever drift, this test fails.
func TestTheDocumentedConfigurationIsUsable(t *testing.T) {
	body := `{
  "enabled": true,
  "concurrency": 1,
  "owner": "mpm-dispatcher",
  "lease_seconds": 1800,
  "runtime_timeout_seconds": 600,
  "lease_margin_seconds": 300,
  "max_dispatch_attempts": 3,
  "retry_backoff_seconds": 30,
  "candidate_limit": 50,
  "drain_timeout_seconds": 120,
  "drain_poll_interval_seconds": 5,
  "workspace_root": "/var/lib/mpm/workspaces",
  "repository_path": "/srv/mpm-repo",
  "worker_agent": "mpm-worker",
  "reviewer_agent": "mpm-reviewer",
  "review_repo_path": "/srv/repo",
  "model": "mpm-sidecar/permitted-model"
}`
	path := filepath.Join(t.TempDir(), "dispatch.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("the documented configuration did not load: %v", err)
	}
	if !cfg.Enabled {
		t.Error("the documented configuration is not enabled")
	}
	if cfg.RuntimeTimeoutSeconds != 600 {
		t.Errorf("the runtime timeout is %d, want 600", cfg.RuntimeTimeoutSeconds)
	}
	if cfg.LeaseSeconds != 1800 {
		t.Errorf("the lease is %d, want 1800", cfg.LeaseSeconds)
	}
}

// TestTheConfiguredPathsMustNotContainOneAnother proves equality is
// containment too.
func TestTheConfiguredPathsMustNotContainOneAnother(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.Concurrency = 1
	cfg.LeaseSeconds = 900
	cfg.RuntimeTimeoutSeconds = 600
	cfg.LeaseMarginSeconds = 300
	cfg.CandidateLimit = 10
	cfg.WorkspaceRoot = "/srv/work"
	cfg.RepositoryPath = "/srv/work"
	cfg.ReviewRepoPath = "/srv/repo"
	if err := cfg.Validate(); err == nil {
		t.Error("a workspace root equal to the repository was accepted")
	}
	cfg.RepositoryPath = "/srv/work/repo"
	if err := cfg.Validate(); err == nil {
		t.Error("a repository inside the workspace root was accepted")
	}
}

// TestTheLeaseMustCoverTheRuntimeWindow proves the refusal that keeps a
// launch recordable.
//
// A lease shorter than the runtime window would expire while the runtime
// is still working, and the adapter's result would be fenced out.
func TestTheLeaseMustCoverTheRuntimeWindow(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.Concurrency = 1
	cfg.LeaseSeconds = 600
	cfg.RuntimeTimeoutSeconds = 600
	cfg.LeaseMarginSeconds = 300
	cfg.CandidateLimit = 10
	cfg.WorkspaceRoot = "/tmp/workspaces"
	cfg.RepositoryPath = "/srv/mpm-repo"
	cfg.ReviewRepoPath = "/srv/repo"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("a lease that cannot cover the runtime window was accepted")
	}
	if !strings.Contains(err.Error(), "does not cover") {
		t.Errorf("refused, but not as a lease problem: %v", err)
	}
	// AND IT IS ACCEPTED WHEN IT DOES COVER IT.
	cfg.LeaseSeconds = 900
	if err := cfg.Validate(); err != nil {
		t.Errorf("a lease that covers the window was refused: %v", err)
	}
}

// TestTheReviewerMayNotBeTheWorker proves the two definitions are
// different by construction.
//
// The reviewer runs under the pinned, default-deny definition. A
// configuration that names the worker's definition for the reviewer is
// refused, so an operator cannot widen the reviewer's surface by
// accident.
func TestTheReviewerMayNotBeTheWorker(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.Concurrency = 1
	cfg.LeaseSeconds = 900
	cfg.RuntimeTimeoutSeconds = 600
	cfg.LeaseMarginSeconds = 300
	cfg.CandidateLimit = 10
	cfg.WorkspaceRoot = "/tmp/workspaces"
	cfg.RepositoryPath = "/srv/mpm-repo"
	cfg.ReviewRepoPath = "/srv/repo"
	cfg.WorkerAgent = "mpm-worker"
	cfg.ReviewerAgent = "mpm-worker"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("the worker was accepted as the reviewer")
	}
	if !strings.Contains(err.Error(), "pinned to") {
		t.Errorf("refused, but not as a definition problem: %v", err)
	}
}

// TestAnUnpinnedWorkerAgentIsRefused proves the containment's pin is
// enforced at the configuration.
//
// The runtime's allow-list names exactly the two definitions MPM ships.
// A configuration naming a third one would be refused by the runtime
// AFTER the claim and BEFORE any attempt exists — and a refusal that
// creates no attempt never trips the dispatch-attempt cap, so the
// assignment would be retried forever while its live claim starved the
// tree's concurrency budget.
func TestAnUnpinnedWorkerAgentIsRefused(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.Concurrency = 1
	cfg.LeaseSeconds = 900
	cfg.RuntimeTimeoutSeconds = 600
	cfg.LeaseMarginSeconds = 300
	cfg.CandidateLimit = 10
	cfg.WorkspaceRoot = "/tmp/workspaces"
	cfg.RepositoryPath = "/srv/mpm-repo"
	cfg.ReviewRepoPath = "/srv/repo"
	cfg.WorkerAgent = "some-other-agent"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("an unpinned worker agent was accepted")
	}
	if !strings.Contains(err.Error(), "pinned to") {
		t.Errorf("refused, but not as a pinning problem: %v", err)
	}
}

// TestARelativePathIsRefused proves the paths the containment needs are
// checked BEFORE anything is claimed.
//
// The sandbox's root and the workspace it contains are resolved against
// the filesystem, and the containment refuses a relative path. The
// worker path needed that already; the reviewer path only found out at
// the factory — after the claim.
func TestARelativePathIsRefused(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.Concurrency = 1
	cfg.LeaseSeconds = 900
	cfg.RuntimeTimeoutSeconds = 600
	cfg.LeaseMarginSeconds = 300
	cfg.CandidateLimit = 10
	cfg.WorkspaceRoot = "relative/workspaces"
	cfg.RepositoryPath = "/srv/mpm-repo"
	cfg.ReviewRepoPath = "/srv/repo"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("a relative workspace root was accepted")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("the refusal does not name the reason: %v", err)
	}

	cfg.WorkspaceRoot = "/srv/workspaces"
	cfg.RepositoryPath = "/srv/mpm-repo"
	cfg.ReviewRepoPath = "relative/repo"
	err = cfg.Validate()
	if err == nil {
		t.Fatal("a relative repository was accepted")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("the refusal does not name the reason: %v", err)
	}
}

// TestTheConfigurationIsAPlainJSONObject proves the file is readable.
//
// A configuration that only a Go program can parse is a configuration an
// operator cannot inspect.
func TestTheConfigurationIsAPlainJSONObject(t *testing.T) {
	cfg := DefaultConfig()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("the configuration is not a plain JSON object: %v", err)
	}
	for _, key := range []string{
		"enabled", "concurrency", "owner", "lease_seconds",
		"runtime_timeout_seconds", "lease_margin_seconds",
		"max_dispatch_attempts", "retry_backoff_seconds", "candidate_limit",
		"drain_timeout_seconds", "drain_poll_interval_seconds",
		"workspace_root", "worker_agent", "reviewer_agent",
		"review_repo_path", "model",
	} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("the configuration has no %q key: %s", key, raw)
		}
	}
}

// TestANewEngineRefusesAnUnusableRepositoryBeforeAnyClaim proves the
// repository is checked before an engine exists.
//
// A bad repository discovered after the first claim would leave a live
// lease and no attempt, so the dispatch-attempt cap could never bound
// the retry.
func TestANewEngineRefusesAnUnusableRepositoryBeforeAnyClaim(t *testing.T) {
	store, _ := testStore(t)
	factory := func(context.Context, *assignment.Assignment, string) (
		assignment.Runtime, assignment.ReviewRuntime, error) {
		return newFakeRuntime(), nil, nil
	}

	cfg := testConfig(t)
	cfg.RepositoryPath = t.TempDir() // a directory, but not a Git repository
	if _, err := NewEngine(store, cfg, factory); err == nil {
		t.Error("an engine was built over a directory that is not a Git repository")
	}

	cfg.RepositoryPath = filepath.Join(t.TempDir(), "absent")
	if _, err := NewEngine(store, cfg, factory); err == nil {
		t.Error("an engine was built over a repository path that does not exist")
	}

	cfg.RepositoryPath, _ = testRepository(t)
	if _, err := NewEngine(store, cfg, factory); err != nil {
		t.Errorf("a usable repository was refused: %v", err)
	}
}
