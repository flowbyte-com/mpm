package opencode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// realReviewerRules is the ordered permission list OpenCode v2.3.2 resolves
// for the pinned mpm-reviewer definition. It was captured from
// `opencode --standalone debug agents` in the isolated reviewer configuration;
// it contains no secrets.
func realReviewerRules(t *testing.T) []SessionPerm {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "reviewer-permissions-v2.3.2.json"))
	if err != nil {
		t.Fatalf("read the resolved-rule fixture: %v", err)
	}
	var a agentInfo
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatalf("parse the resolved-rule fixture: %v", err)
	}
	if a.ID != ReviewerAgentName {
		t.Fatalf("the fixture is for %q, want %q", a.ID, ReviewerAgentName)
	}
	if len(a.Permissions) != 47 {
		t.Fatalf("the fixture has %d permissions, want the measured 47", len(a.Permissions))
	}
	if p := a.Permissions[3]; p.Action != "read" || p.Resource != "*.env.example" || p.Effect != "allow" {
		t.Fatalf("the fixture's .env.example allow moved: %+v", p)
	}
	if p := a.Permissions[23]; p.Action != "mpm_assignment" || p.Effect != "deny" {
		t.Fatalf("the fixture's mpm_assignment deny moved: %+v", p)
	}
	return a.Permissions
}

// TestReviewerPermissionPolicyAcceptsRealResolvedRules is the regression:
// the host's earlier `.env.example` allow is overridden by the agent's later
// `read * deny`, and sandbox_exec remains effective.
func TestReviewerPermissionPolicyAcceptsRealResolvedRules(t *testing.T) {
	if violation := reviewerPermissionViolation(realReviewerRules(t)); violation != "" {
		t.Fatalf("the real resolved rules were refused: %s", violation)
	}
}

func TestReviewerPermissionPolicyRejectsEffectiveReadAllow(t *testing.T) {
	perms := append(realReviewerRules(t), SessionPerm{
		Action: "read", Resource: "*", Effect: "allow",
	})
	if violation := reviewerPermissionViolation(perms); !strings.Contains(violation, `"read"`) {
		t.Fatalf("an effective read allow was not named: %q", violation)
	}
}

func TestReviewerPermissionPolicyRejectsEffectiveSensitiveAllows(t *testing.T) {
	for _, action := range []string{
		"shell", "execute", "browser", "webfetch", "websearch",
		"subagent", "skill", "mpm_assignment", "mpm_skills",
	} {
		action := action
		t.Run(action, func(t *testing.T) {
			perms := append(realReviewerRules(t), SessionPerm{
				Action: action, Resource: "*", Effect: "allow",
			})
			if violation := reviewerPermissionViolation(perms); !strings.Contains(violation, `"`+action+`"`) {
				t.Fatalf("an effective %s allow was not named: %q", action, violation)
			}
		})
	}
}

func TestReviewerPermissionPolicyRequiresAnEffectiveWildcardDeny(t *testing.T) {
	perms := realReviewerRules(t)
	for i := len(perms) - 1; i >= 0; i-- {
		if perms[i].Action != "*" {
			continue
		}
		perms[i].Effect = "allow"
		break
	}
	if violation := reviewerPermissionViolation(perms); !strings.Contains(violation, "wildcard deny") {
		t.Fatalf("a later wildcard allow was not refused: %q", violation)
	}

	perms = realReviewerRules(t)
	for i := len(perms) - 1; i >= 0; i-- {
		if perms[i].Action != "*" {
			continue
		}
		perms[i].Resource = "secret*"
		break
	}
	if violation := reviewerPermissionViolation(perms); !strings.Contains(violation, "wildcard deny") {
		t.Fatalf("a resource-specific wildcard deny was accepted: %q", violation)
	}
}

func TestReviewerPermissionPolicyRequiresEffectiveSandboxExec(t *testing.T) {
	// No sandbox_exec allow at all.
	perms := make([]SessionPerm, 0)
	for _, p := range realReviewerRules(t) {
		if p.Action == "sandbox_exec" {
			continue
		}
		perms = append(perms, p)
	}
	if violation := reviewerPermissionViolation(perms); !strings.Contains(violation, "sandbox_exec") {
		t.Fatalf("a reviewer without sandbox_exec was accepted: %q", violation)
	}

	// An earlier sandbox_exec allow is overridden by a later wildcard deny.
	perms = append(realReviewerRules(t), SessionPerm{Action: "*", Resource: "*", Effect: "deny"})
	if violation := reviewerPermissionViolation(perms); !strings.Contains(violation, "sandbox_exec") {
		t.Fatalf("an overridden sandbox_exec allow was accepted: %q", violation)
	}
}

func TestReviewerPermissionPolicyDoesNotCollapseResources(t *testing.T) {
	base := []SessionPerm{
		{Action: "*", Resource: "*", Effect: "deny"},
		{Action: "sandbox_exec", Resource: "*", Effect: "allow"},
	}

	// A broad allow is NOT overridden by a narrower deny.
	broad := append(append([]SessionPerm{}, base...),
		SessionPerm{Action: "read", Resource: "*", Effect: "allow"},
		SessionPerm{Action: "read", Resource: "*.env.example", Effect: "deny"},
	)
	if violation := reviewerPermissionViolation(broad); !strings.Contains(violation, `"read"`) {
		t.Fatalf("a broad read allow was treated as action-only denied: %q", violation)
	}

	// A resource-specific allow IS overridden by an exact later deny.
	exact := append(append([]SessionPerm{}, base...),
		SessionPerm{Action: "read", Resource: "*.env.example", Effect: "allow"},
		SessionPerm{Action: "read", Resource: "*.env.example", Effect: "deny"},
	)
	if violation := reviewerPermissionViolation(exact); violation != "" {
		t.Fatalf("an exact resource deny did not override the allow: %q", violation)
	}

	// A resource-specific allow IS overridden by a universal later deny.
	universal := append(append([]SessionPerm{}, base...),
		SessionPerm{Action: "read", Resource: "*.env.example", Effect: "allow"},
		SessionPerm{Action: "read", Resource: "*", Effect: "deny"},
	)
	if violation := reviewerPermissionViolation(universal); violation != "" {
		t.Fatalf("a universal resource deny did not override the allow: %q", violation)
	}

	// A broader glob later deny covers the narrower allow.
	broader := append(append([]SessionPerm{}, base...),
		SessionPerm{Action: "read", Resource: "*.env.example", Effect: "allow"},
		SessionPerm{Action: "read", Resource: "*.env.*", Effect: "deny"},
	)
	if violation := reviewerPermissionViolation(broader); violation != "" {
		t.Fatalf("a broader resource glob did not override the allow: %q", violation)
	}

	// A star does not cross a path separator, so `dir/*` does not cover
	// `dir/sub/*`.
	slash := append(append([]SessionPerm{}, base...),
		SessionPerm{Action: "read", Resource: "dir/sub/*", Effect: "allow"},
		SessionPerm{Action: "read", Resource: "dir/*", Effect: "deny"},
	)
	if violation := reviewerPermissionViolation(slash); !strings.Contains(violation, `"read"`) {
		t.Fatalf("a non-crossing star was treated as covering a nested path: %q", violation)
	}

	// The same rule on the `*middle*` shape.
	nested := append(append([]SessionPerm{}, base...),
		SessionPerm{Action: "read", Resource: "dir/*.env.example", Effect: "allow"},
		SessionPerm{Action: "read", Resource: "*.env.*", Effect: "deny"},
	)
	if violation := reviewerPermissionViolation(nested); !strings.Contains(violation, `"read"`) {
		t.Fatalf("a nested resource was treated as covered by a non-crossing glob: %q", violation)
	}
}

func TestReviewerPermissionPolicyMatchesActionGlobs(t *testing.T) {
	base := []SessionPerm{
		{Action: "*", Resource: "*", Effect: "deny"},
		{Action: "sandbox_exec", Resource: "*", Effect: "allow"},
	}

	overridden := append(append([]SessionPerm{}, base...),
		SessionPerm{Action: "opencode_foo", Resource: "*", Effect: "allow"},
		SessionPerm{Action: "opencode_*", Resource: "*", Effect: "deny"},
	)
	if violation := reviewerPermissionViolation(overridden); violation != "" {
		t.Fatalf("an action-specific allow was not overridden by a later action glob: %q", violation)
	}

	broad := append(append([]SessionPerm{}, base...),
		SessionPerm{Action: "opencode_*", Resource: "*", Effect: "allow"},
		SessionPerm{Action: "opencode_foo", Resource: "*", Effect: "deny"},
	)
	if violation := reviewerPermissionViolation(broad); !strings.Contains(violation, "opencode_*") {
		t.Fatalf("a broad action allow was accepted after a narrow deny: %q", violation)
	}
}

func TestReviewerSessionOverridesRemainRefused(t *testing.T) {
	if violation := reviewerSessionOverrideViolation(nil); violation != "" {
		t.Fatalf("an empty override list was refused: %q", violation)
	}
	violation := reviewerSessionOverrideViolation([]SessionPerm{{
		Action: "read", Resource: "*", Effect: "allow",
	}})
	if !strings.Contains(violation, "SESSION-LEVEL") {
		t.Fatalf("a nonempty session override was not refused: %q", violation)
	}
}

// TestReviewerPermissionPolicyAcceptsTheLiveOpenCodeConfig is OPTIONAL in CI:
// it needs a real opencode binary and the built MPM plugin. When both are
// supplied it proves the PERMISSION VERIFIER accepts the live resolved rules;
// it does not authenticate a provider or run a model.
func TestReviewerPermissionPolicyAcceptsTheLiveOpenCodeConfig(t *testing.T) {
	bin := os.Getenv("MPM_OPENCODE_BIN")
	plugin := os.Getenv("MPM_OPENCODE_PLUGIN")
	if bin == "" || plugin == "" {
		t.Skip("MPM_OPENCODE_BIN and MPM_OPENCODE_PLUGIN are not set")
	}
	if _, err := os.Stat(filepath.Join(plugin, "dist", "index.v2.js")); err != nil {
		t.Skipf("the plugin payload is not built: %v", err)
	}
	root := filepath.Join(t.TempDir(), "review-sandbox")
	if err := BuildTree(Spec{Root: root, Workspace: t.TempDir()}); err != nil {
		t.Fatalf("build the review tree: %v", err)
	}
	r := &Runtime{cfg: Config{
		Root: root, OpenCodeBin: bin, Model: "minimax-coding-plan/MiniMax-M3.1-Flash",
		PluginPath: plugin, AllowedAgents: []string{ReviewerAgentName},
		Timeout: 60 * time.Second,
	}, Now: time.Now}
	if err := r.writeReviewConfig(); err != nil {
		t.Fatalf("write the review config: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := r.verifyReviewer(ctx); err != nil {
		t.Fatalf("the actual isolated reviewer configuration was refused: %v", err)
	}
}

func TestReviewerPermissionPolicyRejectsUnprovenPatternSyntax(t *testing.T) {
	// A LITERAL allow makes the old verifier's filepath.Match path accept
	// these denies; the list-level guard must refuse the list first.
	for _, tc := range []struct {
		name string
		deny string
	}{
		{"escape", `a\b`},
		{"question", "a?"},
		{"class", "a[bc]"},
		{"doublestar", "a**"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			perms := []SessionPerm{
				{Action: "*", Resource: "*", Effect: "deny"},
				{Action: "sandbox_exec", Resource: "*", Effect: "allow"},
				{Action: "read", Resource: "ab", Effect: "allow"},
				{Action: "read", Resource: tc.deny, Effect: "deny"},
			}
			if violation := reviewerPermissionViolation(perms); !strings.Contains(violation, "cannot prove") {
				t.Fatalf("unproven deny syntax %q was not refused: %q", tc.deny, violation)
			}
		})
	}
}

func TestReviewerPermissionPolicyRejectsUnprovenSandboxRules(t *testing.T) {
	// An unproven sandbox allow must not count as an execution path.
	allowResource := []SessionPerm{
		{Action: "*", Resource: "*", Effect: "deny"},
		{Action: "sandbox_exec", Resource: "**", Effect: "allow"},
	}
	if violation := reviewerPermissionViolation(allowResource); !strings.Contains(violation, "cannot prove") {
		t.Fatalf("an unproven sandbox allow resource was accepted: %q", violation)
	}

	// An unproven later deny could cover the sandbox; it must not be read
	// as "does not cover" and thereby leave the sandbox effective.
	laterDeny := []SessionPerm{
		{Action: "*", Resource: "*", Effect: "deny"},
		{Action: "sandbox_exec", Resource: "*", Effect: "allow"},
		{Action: "sandbox_exec", Resource: "**", Effect: "deny"},
	}
	if violation := reviewerPermissionViolation(laterDeny); !strings.Contains(violation, "cannot prove") {
		t.Fatalf("an unproven later sandbox deny was treated as non-covering: %q", violation)
	}
}

func TestReviewerPermissionPolicyDoesNotTreatDoubleStarAsWildcard(t *testing.T) {
	perms := []SessionPerm{
		{Action: "**", Resource: "*", Effect: "deny"},
		{Action: "sandbox_exec", Resource: "*", Effect: "allow"},
	}
	if violation := reviewerPermissionViolation(perms); !strings.Contains(violation, "cannot prove") {
		t.Fatalf("a `**` action was accepted as the wildcard deny: %q", violation)
	}

	base := []SessionPerm{
		{Action: "*", Resource: "*", Effect: "deny"},
		{Action: "sandbox_exec", Resource: "*", Effect: "allow"},
	}
	resource := append(append([]SessionPerm{}, base...),
		SessionPerm{Action: "read", Resource: "*", Effect: "allow"},
		SessionPerm{Action: "read", Resource: "**", Effect: "deny"},
	)
	if violation := reviewerPermissionViolation(resource); !strings.Contains(violation, "cannot prove") {
		t.Fatalf("a `**` resource deny was treated as universal: %q", violation)
	}
}
