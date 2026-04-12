package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// IdentityBranch represents a branch in the identity versioning system.
type IdentityBranch struct {
	Name      string `json:"name"`
	Parent    string `json:"parent"`
	CreatedAt string `json:"created_at"`
	Status    string `json:"status"` // active, promoted, deprecated
}

// ForkIdentity creates a new branch from the current IDENTITY.md.
func ForkIdentity(binaryDir, branchName string) error {
	// Read current IDENTITY.md
	identityPath := filepath.Join(binaryDir, "IDENTITY.md")
	identityData, err := os.ReadFile(identityPath)
	if err != nil {
		return fmt.Errorf("read identity: %w", err)
	}

	// Ensure IDENTITIES/ directory exists
	identitiesDir := filepath.Join(binaryDir, "IDENTITIES")
	if err := os.MkdirAll(identitiesDir, 0755); err != nil {
		return fmt.Errorf("create identities dir: %w", err)
	}

	// Write branch file
	branchPath := filepath.Join(identitiesDir, branchName+".md")
	if err := os.WriteFile(branchPath, identityData, 0644); err != nil {
		return fmt.Errorf("write branch file: %w", err)
	}

	// Update branches.json
	branchesPath := filepath.Join(identitiesDir, "branches.json")
	var branches []IdentityBranch

	// Read existing branches.json if it exists
	if data, err := os.ReadFile(branchesPath); err == nil {
		if err := json.Unmarshal(data, &branches); err != nil {
			return fmt.Errorf("parse branches.json: %w", err)
		}
	}

	// Add new branch
	branches = append(branches, IdentityBranch{
		Name:      branchName,
		Parent:    "",
		CreatedAt: time.Now().Format(time.RFC3339),
		Status:    "active",
	})

	// Sort branches by CreatedAt
	sort.Slice(branches, func(i, j int) bool {
		return branches[i].CreatedAt < branches[j].CreatedAt
	})

	// Write updated branches.json
	data, err := json.MarshalIndent(branches, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal branches: %w", err)
	}
	if err := os.WriteFile(branchesPath, data, 0644); err != nil {
		return fmt.Errorf("write branches.json: %w", err)
	}

	return nil
}

// ListIdentityBranches returns all identity branches.
func ListIdentityBranches(binaryDir string) ([]IdentityBranch, error) {
	branchesPath := filepath.Join(binaryDir, "IDENTITIES", "branches.json")

	data, err := os.ReadFile(branchesPath)
	if err != nil {
		if os.IsNotExist(err) {
			return []IdentityBranch{}, nil
		}
		return nil, fmt.Errorf("read branches.json: %w", err)
	}

	var branches []IdentityBranch
	if err := json.Unmarshal(data, &branches); err != nil {
		return nil, fmt.Errorf("parse branches.json: %w", err)
	}

	// Sort by CreatedAt
	sort.Slice(branches, func(i, j int) bool {
		return branches[i].CreatedAt < branches[j].CreatedAt
	})

	return branches, nil
}

// SwitchIdentityBranch switches to a different identity branch.
func SwitchIdentityBranch(binaryDir, branchName string) error {
	// Read branch file
	branchPath := filepath.Join(binaryDir, "IDENTITIES", branchName+".md")
	branchData, err := os.ReadFile(branchPath)
	if err != nil {
		return fmt.Errorf("read branch file: %w", err)
	}

	// Write to IDENTITY.md
	identityPath := filepath.Join(binaryDir, "IDENTITY.md")
	if err := os.WriteFile(identityPath, branchData, 0644); err != nil {
		return fmt.Errorf("write identity: %w", err)
	}

	return nil
}

// PromoteIdentityBranch promotes a branch to be the new main identity.
func PromoteIdentityBranch(binaryDir, branchName string) error {
	// Read branch file
	branchPath := filepath.Join(binaryDir, "IDENTITIES", branchName+".md")
	branchData, err := os.ReadFile(branchPath)
	if err != nil {
		return fmt.Errorf("read branch file: %w", err)
	}

	// Backup current IDENTITY.md
	identityPath := filepath.Join(binaryDir, "IDENTITY.md")
	if currentData, err := os.ReadFile(identityPath); err == nil && len(currentData) > 0 {
		backupPath := identityPath + ".backup"
		if err := os.WriteFile(backupPath, currentData, 0644); err != nil {
			return fmt.Errorf("write backup: %w", err)
		}
	}

	// Overwrite IDENTITY.md with branch content
	if err := os.WriteFile(identityPath, branchData, 0644); err != nil {
		return fmt.Errorf("write identity: %w", err)
	}

	// Update branch status in branches.json
	branchesPath := filepath.Join(binaryDir, "IDENTITIES", "branches.json")
	var branches []IdentityBranch

	if data, err := os.ReadFile(branchesPath); err == nil {
		if err := json.Unmarshal(data, &branches); err != nil {
			return fmt.Errorf("parse branches.json: %w", err)
		}
	}

	for i := range branches {
		if branches[i].Name == branchName {
			branches[i].Status = "promoted"
			break
		}
	}

	data, err := json.MarshalIndent(branches, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal branches: %w", err)
	}
	if err := os.WriteFile(branchesPath, data, 0644); err != nil {
		return fmt.Errorf("write branches.json: %w", err)
	}

	return nil
}

// CompareIdentityBranches returns a diff summary between two branches.
func CompareIdentityBranches(binaryDir, branchA, branchB string) (string, error) {
	// Read branch A
	branchAPath := filepath.Join(binaryDir, "IDENTITIES", branchA+".md")
	branchAData, err := os.ReadFile(branchAPath)
	if err != nil {
		return "", fmt.Errorf("read branch A: %w", err)
	}

	// Read branch B
	branchBPath := filepath.Join(binaryDir, "IDENTITIES", branchB+".md")
	branchBData, err := os.ReadFile(branchBPath)
	if err != nil {
		return "", fmt.Errorf("read branch B: %w", err)
	}

	// Simple string comparison - find differences
	linesA := strings.Split(string(branchAData), "\n")
	linesB := strings.Split(string(branchBData), "\n")

	var diff []string
	maxLen := len(linesA)
	if len(linesB) > maxLen {
		maxLen = len(linesB)
	}

	for i := 0; i < maxLen; i++ {
		lineA := ""
		lineB := ""
		if i < len(linesA) {
			lineA = linesA[i]
		}
		if i < len(linesB) {
			lineB = linesB[i]
		}
		if lineA != lineB {
			diff = append(diff, fmt.Sprintf("Line %d:\n  A: %s\n  B: %s", i+1, lineA, lineB))
		}
	}

	if len(diff) == 0 {
		return "", nil
	}

	return strings.Join(diff, "\n"), nil
}
