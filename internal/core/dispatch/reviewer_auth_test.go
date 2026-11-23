package dispatch

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core/assignment"
	oc "github.com/flowbyte-com/mpm-core/opencode"
)

func reviewerConfig(t *testing.T) Config {
	t.Helper()
	cfg := testConfig(t)
	cfg.ReviewerModel = DefaultReviewModel
	return cfg
}

func reviewerAssignment() *assignment.Assignment {
	return &assignment.Assignment{ID: "at-reviewer-auth", RootID: "at-root", Role: assignment.RoleReviewer}
}

func TestReviewerRuntimeRequiresItsOwnCredential(t *testing.T) {
	t.Setenv("MPM_OPENCODE_BIN", stubOpenCode(t))
	t.Setenv("MPM_OPENCODE_SIDECAR_KEY", "marker-worker-sidecar")
	t.Setenv("MPM_OPENCODE_REVIEWER_KEY", "")
	t.Setenv("MINIMAX_API_KEY", "marker-host-minimax")
	t.Setenv("DEEPSEEK_API_KEY", "marker-host-deepseek")
	t.Setenv("MPM_WORKSPACE", t.TempDir())

	factory, err := NewOpenCodeFactory(reviewerConfig(t))
	if err != nil {
		t.Fatalf("build the factory: %v", err)
	}
	defer factory.Close()
	if _, err := factory.reviewerRuntime(reviewerAssignment()); err == nil {
		t.Fatal("the reviewer runtime was built without MPM_OPENCODE_REVIEWER_KEY")
	} else if !strings.Contains(err.Error(), EnvReviewerKey) {
		t.Fatalf("the refusal does not name %s: %v", EnvReviewerKey, err)
	}
}

func TestWorkerSidecarCredentialCannotAuthenticateTheReviewer(t *testing.T) {
	t.Setenv("MPM_OPENCODE_BIN", stubOpenCode(t))
	t.Setenv("MPM_OPENCODE_SIDECAR_KEY", "marker-worker-sidecar")
	t.Setenv("MPM_OPENCODE_REVIEWER_KEY", "")
	t.Setenv("MPM_WORKSPACE", t.TempDir())

	factory, err := NewOpenCodeFactory(reviewerConfig(t))
	if err != nil {
		t.Fatalf("build the factory: %v", err)
	}
	defer factory.Close()
	_, err = factory.reviewerRuntime(reviewerAssignment())
	if err == nil || !strings.Contains(err.Error(), EnvReviewerKey) {
		t.Fatalf("the sidecar key was accepted as a reviewer credential: %v", err)
	}
}

func TestReviewerCredentialCannotAuthenticateTheWorker(t *testing.T) {
	t.Setenv("MPM_OPENCODE_BIN", stubOpenCode(t))
	t.Setenv("MPM_OPENCODE_SIDECAR_KEY", "")
	t.Setenv("MPM_OPENCODE_REVIEWER_KEY", "marker-reviewer")
	t.Setenv("MPM_WORKSPACE", t.TempDir())

	if _, err := NewOpenCodeFactory(reviewerConfig(t)); err == nil {
		t.Fatal("the reviewer credential was accepted as a worker sidecar credential")
	}
}

func TestReviewerRuntimeCarriesTheDedicatedKeyAndProviderMapping(t *testing.T) {
	t.Setenv("MPM_OPENCODE_BIN", stubOpenCode(t))
	t.Setenv("MPM_OPENCODE_SIDECAR_KEY", "marker-worker-sidecar")
	t.Setenv("MPM_OPENCODE_REVIEWER_KEY", "marker-reviewer")
	t.Setenv("MINIMAX_API_KEY", "marker-host-minimax")
	t.Setenv("MPM_WORKSPACE", t.TempDir())

	factory, err := NewOpenCodeFactory(reviewerConfig(t))
	if err != nil {
		t.Fatalf("build the factory: %v", err)
	}
	defer factory.Close()
	rt, err := factory.reviewerRuntime(reviewerAssignment())
	if err != nil {
		t.Fatalf("build the reviewer runtime: %v", err)
	}
	cfg := rt.Config()
	if cfg.ProviderKey != "marker-reviewer" {
		t.Fatalf("the reviewer runtime carries %q, want the dedicated key", cfg.ProviderKey)
	}
	if cfg.ProviderKeyEnv != "MINIMAX_API_KEY" {
		t.Fatalf("the provider environment is %q, want MINIMAX_API_KEY", cfg.ProviderKeyEnv)
	}
	if cfg.ProviderKey == os.Getenv("MINIMAX_API_KEY") {
		t.Fatal("the host MiniMax key was used instead of the dedicated reviewer key")
	}
}

func TestReviewerRuntimeRefusesAnUnmappedModel(t *testing.T) {
	t.Setenv("MPM_OPENCODE_BIN", stubOpenCode(t))
	t.Setenv("MPM_OPENCODE_SIDECAR_KEY", "marker-worker-sidecar")
	t.Setenv("MPM_OPENCODE_REVIEWER_KEY", "marker-reviewer")
	t.Setenv("MPM_WORKSPACE", t.TempDir())

	cfg := reviewerConfig(t)
	cfg.ReviewerModel = "deepseek/deepseek-chat"
	factory, err := NewOpenCodeFactory(cfg)
	if err != nil {
		t.Fatalf("build the factory: %v", err)
	}
	defer factory.Close()
	if _, err := factory.reviewerRuntime(reviewerAssignment()); err == nil {
		t.Fatal("an unmapped reviewer model was accepted")
	} else if !strings.Contains(err.Error(), "dedicated reviewer credential mapping") {
		t.Fatalf("the refusal does not name the mapping: %v", err)
	}
}

func TestReviewerRuntimeIsCachedWithItsCredentialBoundary(t *testing.T) {
	t.Setenv("MPM_OPENCODE_BIN", stubOpenCode(t))
	t.Setenv("MPM_OPENCODE_SIDECAR_KEY", "marker-worker-sidecar")
	t.Setenv("MPM_OPENCODE_REVIEWER_KEY", "marker-reviewer")
	t.Setenv("MPM_WORKSPACE", t.TempDir())

	factory, err := NewOpenCodeFactory(reviewerConfig(t))
	if err != nil {
		t.Fatalf("build the factory: %v", err)
	}
	defer factory.Close()
	build := factory.Factory()
	reviewer := reviewerAssignment()
	_, first, err := build(context.Background(), reviewer, "")
	if err != nil {
		t.Fatalf("first reviewer runtime: %v", err)
	}
	_, second, err := build(context.Background(), reviewer, "")
	if err != nil {
		t.Fatalf("second reviewer runtime: %v", err)
	}
	if first != second {
		t.Fatal("recovery built a different reviewer runtime")
	}
	firstRT, ok := first.(*oc.Runtime)
	if !ok {
		t.Fatalf("the first runtime is %T, want *oc.Runtime", first)
	}
	if firstRT.Config().Model != DefaultReviewModel {
		t.Fatalf("the cached runtime model is %q, want %q",
			firstRT.Config().Model, DefaultReviewModel)
	}
	if firstRT.Config().ProviderKey != "marker-reviewer" ||
		firstRT.Config().ProviderKeyEnv != "MINIMAX_API_KEY" {
		t.Fatal("the cached runtime lost its credential boundary")
	}
}

// TestReviewerProviderEnvMapsOnlyTheMeasuredFamily pins the closed mapping.
// The CN endpoint and other providers were not measured, so they are refused
// rather than guessed at.
func TestReviewerProviderEnvMapsOnlyTheMeasuredFamily(t *testing.T) {
	for _, tc := range []struct {
		model   string
		wantEnv string
		wantErr bool
	}{
		{"minimax-coding-plan/MiniMax-M3.1-Flash", "MINIMAX_API_KEY", false},
		{"minimax-coding-plan/MiniMax-M2.7", "", true},
		{"minimax-cn-coding-plan/MiniMax-M3.1-Flash", "", true},
		{"deepseek/deepseek-chat", "", true},
		{"", "", true},
	} {
		env, err := ReviewerProviderEnv(tc.model)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%q was mapped to %q, want a refusal", tc.model, env)
			}
			continue
		}
		if err != nil || env != tc.wantEnv {
			t.Errorf("%q -> (%q, %v), want (%q, nil)", tc.model, env, err, tc.wantEnv)
		}
	}
}

// TestReviewRefusalReasonsSeparateIncompleteFromUnavailable is the recovery
// distinction: a review that STARTED but could not be captured must require
// an operator, while a review that never started is a plain unavailability.
func TestReviewRefusalReasonsSeparateIncompleteFromUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"post-inference capture", fmt.Errorf("%w: capture", oc.ErrReviewIncomplete), ReasonRecoveryRequired},
		{"ambiguous result", assignment.ErrReviewAmbiguous, ReasonRecoveryRequired},
		{"ambiguous stale transaction", fmt.Errorf("%w: %w",
			assignment.ErrReviewAmbiguous, assignment.ErrStaleClaim), ReasonRecoveryRequired},
		{"missing verdict", assignment.ErrReviewVerdict, ReasonRecoveryRequired},
		{"never started", oc.ErrReviewerUnavailable, ReasonReviewerUnavailable},
	} {
		if got := classifyReviewRefusal(tc.err); got != tc.want {
			t.Errorf("%s: reason %q, want %q", tc.name, got, tc.want)
		}
	}
}
