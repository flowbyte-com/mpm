package opencode

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestReviewerProviderCredentialExposesTheMeasuredModel is OPTIONAL: it
// needs a real OpenCode v2.3.2 binary. It performs NO inference. It pins the
// mechanism the dispatcher uses: the dedicated reviewer key is passed as
// MINIMAX_API_KEY to the isolated process, and only then does OpenCode list
// the measured MiniMax model.
func TestReviewerProviderCredentialExposesTheMeasuredModel(t *testing.T) {
	bin := os.Getenv("MPM_OPENCODE_BIN")
	if bin == "" {
		t.Skip("MPM_OPENCODE_BIN is not set")
	}
	root := filepath.Join(t.TempDir(), "reviewer-auth")
	for _, dir := range []string{"home", "ocfg", "odata", "ocache", "ostate"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	const measuredModel = "minimax-coding-plan/MiniMax-M3.1-Flash"
	withKey := Config{
		Root: root, OpenCodeBin: bin, Model: measuredModel,
		ProviderKey: "marker-not-a-real-credential", ProviderKeyEnv: "MINIMAX_API_KEY",
	}
	cmd := exec.Command(bin, "--standalone", "models")
	cmd.Env = IsolatedEnv(withKey)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("opencode models with the marker reviewer key: %v", err)
	}
	if !strings.Contains(string(out), measuredModel) {
		t.Fatalf("the measured reviewer model was not listed with its credential:\n%s", out)
	}
	withoutKey := withKey
	withoutKey.ProviderKey = ""
	cmd = exec.Command(bin, "--standalone", "models")
	cmd.Env = IsolatedEnv(withoutKey)
	out, err = cmd.Output()
	if err != nil {
		t.Fatalf("opencode models without a credential: %v", err)
	}
	if strings.Contains(string(out), measuredModel) {
		t.Fatalf("the reviewer model was listed WITHOUT its credential:\n%s", out)
	}
}
