package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestForkIdentity(t *testing.T) {
	dir := t.TempDir()

	// Create initial IDENTITY.md
	initialIdentity := `# MiniBot v1.0
Type: assistant
Core traits: helpful`
	if err := os.WriteFile(filepath.Join(dir, "IDENTITY.md"), []byte(initialIdentity), 0644); err != nil {
		t.Fatal(err)
	}

	// Fork the identity
	branchName := "experimental"
	if err := ForkIdentity(dir, branchName, "main"); err != nil {
		t.Fatalf("ForkIdentity failed: %v", err)
	}

	// Verify branch file exists
	branchPath := filepath.Join(dir, "IDENTITIES", branchName+".md")
	if _, err := os.Stat(branchPath); os.IsNotExist(err) {
		t.Error("branch file was not created")
	}

	// Verify branch content matches original
	branchData, err := os.ReadFile(branchPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(branchData) != initialIdentity {
		t.Errorf("branch content mismatch.\nExpected:\n%s\nGot:\n%s", initialIdentity, string(branchData))
	}

	// Verify branches.json was created/updated
	branchesPath := filepath.Join(dir, "IDENTITIES", "branches.json")
	branchesData, err := os.ReadFile(branchesPath)
	if err != nil {
		t.Fatal(err)
	}

	var branches []IdentityBranch
	if err := json.Unmarshal(branchesData, &branches); err != nil {
		t.Fatalf("failed to parse branches.json: %v", err)
	}

	if len(branches) != 1 {
		t.Errorf("expected 1 branch, got %d", len(branches))
	}

	if branches[0].Name != branchName {
		t.Errorf("expected branch name '%s', got '%s'", branchName, branches[0].Name)
	}

	if branches[0].Status != "active" {
		t.Errorf("expected status 'active', got '%s'", branches[0].Status)
	}
}

func TestForkIdentityMultiple(t *testing.T) {
	dir := t.TempDir()

	// Create initial IDENTITY.md
	initialIdentity := `# MiniBot v1.0
Type: assistant`
	if err := os.WriteFile(filepath.Join(dir, "IDENTITY.md"), []byte(initialIdentity), 0644); err != nil {
		t.Fatal(err)
	}

	// Fork multiple branches
	branches := []string{"feature-a", "feature-b", "hotfix"}
	for _, name := range branches {
		if err := ForkIdentity(dir, name, "main"); err != nil {
			t.Fatalf("ForkIdentity(%s) failed: %v", name, err)
		}
	}

	// List branches
	listed, err := ListIdentityBranches(dir)
	if err != nil {
		t.Fatalf("ListIdentityBranches failed: %v", err)
	}

	if len(listed) != 3 {
		t.Errorf("expected 3 branches, got %d", len(listed))
	}
}

func TestListIdentityBranches(t *testing.T) {
	dir := t.TempDir()

	// Create IDENTITY.md
	if err := os.WriteFile(filepath.Join(dir, "IDENTITY.md"), []byte("# MiniBot"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create IDENTITIES directory with branches.json
	identitiesDir := filepath.Join(dir, "IDENTITIES")
	if err := os.MkdirAll(identitiesDir, 0755); err != nil {
		t.Fatal(err)
	}

	branches := []IdentityBranch{
		{Name: "main", Parent: "", CreatedAt: "2024-01-01T00:00:00Z", Status: "active"},
		{Name: "experimental", Parent: "main", CreatedAt: "2024-01-02T00:00:00Z", Status: "active"},
	}
	branchesJSON, _ := json.Marshal(branches)
	if err := os.WriteFile(filepath.Join(identitiesDir, "branches.json"), branchesJSON, 0644); err != nil {
		t.Fatal(err)
	}

	// List branches
	listed, err := ListIdentityBranches(dir)
	if err != nil {
		t.Fatalf("ListIdentityBranches failed: %v", err)
	}

	if len(listed) != 2 {
		t.Errorf("expected 2 branches, got %d", len(listed))
	}

	// Verify order (main should come first as it's created earlier)
	if listed[0].Name != "main" {
		t.Errorf("expected first branch to be 'main', got '%s'", listed[0].Name)
	}
}

func TestListIdentityBranchesEmpty(t *testing.T) {
	dir := t.TempDir()

	// Create IDENTITY.md but no IDENTITIES directory
	if err := os.WriteFile(filepath.Join(dir, "IDENTITY.md"), []byte("# MiniBot"), 0644); err != nil {
		t.Fatal(err)
	}

	// List branches should return empty slice, not error
	listed, err := ListIdentityBranches(dir)
	if err != nil {
		t.Fatalf("ListIdentityBranches failed unexpectedly: %v", err)
	}

	if len(listed) != 0 {
		t.Errorf("expected 0 branches, got %d", len(listed))
	}
}

func TestSwitchIdentityBranch(t *testing.T) {
	dir := t.TempDir()

	// Create initial IDENTITY.md
	initialIdentity := `# MiniBot v1.0
Type: assistant
Core traits: helpful`
	if err := os.WriteFile(filepath.Join(dir, "IDENTITY.md"), []byte(initialIdentity), 0644); err != nil {
		t.Fatal(err)
	}

	// Fork a branch
	branchName := "experimental"
	if err := ForkIdentity(dir, branchName, "main"); err != nil {
		t.Fatal(err)
	}

	// Modify the branch file
	modifiedIdentity := `# MiniBot v1.0
Type: assistant
Core traits: helpful,concise
Boundaries: no personal advice`
	if err := os.WriteFile(filepath.Join(dir, "IDENTITIES", branchName+".md"), []byte(modifiedIdentity), 0644); err != nil {
		t.Fatal(err)
	}

	// Switch to the branch
	if err := SwitchIdentityBranch(dir, branchName); err != nil {
		t.Fatalf("SwitchIdentityBranch failed: %v", err)
	}

	// Verify IDENTITY.md is now the branch content
	current, err := os.ReadFile(filepath.Join(dir, "IDENTITY.md"))
	if err != nil {
		t.Fatal(err)
	}

	if string(current) != modifiedIdentity {
		t.Errorf("IDENTITY.md was not switched correctly.\nExpected:\n%s\nGot:\n%s", modifiedIdentity, string(current))
	}
}

func TestSwitchIdentityBranchNotFound(t *testing.T) {
	dir := t.TempDir()

	// Create IDENTITY.md
	if err := os.WriteFile(filepath.Join(dir, "IDENTITY.md"), []byte("# MiniBot"), 0644); err != nil {
		t.Fatal(err)
	}

	// Try to switch to non-existent branch
	err := SwitchIdentityBranch(dir, "nonexistent")
	if err == nil {
		t.Error("expected error for non-existent branch")
	}
}

func TestPromoteIdentityBranch(t *testing.T) {
	dir := t.TempDir()

	// Create initial IDENTITY.md
	initialIdentity := `# MiniBot v1.0
Type: assistant`
	if err := os.WriteFile(filepath.Join(dir, "IDENTITY.md"), []byte(initialIdentity), 0644); err != nil {
		t.Fatal(err)
	}

	// Fork a branch
	branchName := "experimental"
	if err := ForkIdentity(dir, branchName, "main"); err != nil {
		t.Fatal(err)
	}

	// Modify the branch file
	promotedContent := `# MiniBot v2.0
Type: assistant
Core traits: improved`
	if err := os.WriteFile(filepath.Join(dir, "IDENTITIES", branchName+".md"), []byte(promotedContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Promote the branch
	if err := PromoteIdentityBranch(dir, branchName); err != nil {
		t.Fatalf("PromoteIdentityBranch failed: %v", err)
	}

	// Verify IDENTITY.md was updated
	current, err := os.ReadFile(filepath.Join(dir, "IDENTITY.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != promotedContent {
		t.Errorf("IDENTITY.md was not promoted.\nExpected:\n%s\nGot:\n%s", promotedContent, string(current))
	}

	// Verify backup was created
	backupPath := filepath.Join(dir, "IDENTITY.md.backup")
	backup, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != initialIdentity {
		t.Errorf("backup mismatch.\nExpected:\n%s\nGot:\n%s", initialIdentity, string(backup))
	}

	// Verify branch status is now "promoted"
	branches, err := ListIdentityBranches(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range branches {
		if b.Name == branchName {
			if b.Status != "promoted" {
				t.Errorf("expected branch status 'promoted', got '%s'", b.Status)
			}
			break
		}
	}
}

func TestCompareIdentityBranches(t *testing.T) {
	dir := t.TempDir()

	// Create initial IDENTITY.md
	initialIdentity := `# MiniBot v1.0
Type: assistant
Core traits: helpful`
	if err := os.WriteFile(filepath.Join(dir, "IDENTITY.md"), []byte(initialIdentity), 0644); err != nil {
		t.Fatal(err)
	}

	// Fork two branches
	if err := ForkIdentity(dir, "branch-a", "main"); err != nil {
		t.Fatal(err)
	}
	if err := ForkIdentity(dir, "branch-b", "main"); err != nil {
		t.Fatal(err)
	}

	// Modify branch-a
	branchAContent := `# MiniBot v1.0
Type: assistant
Core traits: helpful,concise`
	if err := os.WriteFile(filepath.Join(dir, "IDENTITIES", "branch-a.md"), []byte(branchAContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Compare branches
	diff, err := CompareIdentityBranches(dir, "branch-a", "branch-b")
	if err != nil {
		t.Fatalf("CompareIdentityBranches failed: %v", err)
	}

	// The diff should indicate the differences
	if diff == "" {
		t.Error("expected non-empty diff")
	}
}

func TestCompareIdentityBranchesSame(t *testing.T) {
	dir := t.TempDir()

	// Create initial IDENTITY.md
	initialIdentity := `# MiniBot v1.0
Type: assistant`
	if err := os.WriteFile(filepath.Join(dir, "IDENTITY.md"), []byte(initialIdentity), 0644); err != nil {
		t.Fatal(err)
	}

	// Fork two branches with same content
	if err := ForkIdentity(dir, "branch-a", "main"); err != nil {
		t.Fatal(err)
	}
	if err := ForkIdentity(dir, "branch-b", "main"); err != nil {
		t.Fatal(err)
	}

	// Compare - should indicate no differences
	diff, err := CompareIdentityBranches(dir, "branch-a", "branch-b")
	if err != nil {
		t.Fatalf("CompareIdentityBranches failed: %v", err)
	}

	// Empty diff means identical
	if diff != "" {
		t.Errorf("expected empty diff for identical branches, got: %s", diff)
	}
}