package core

import (
	"os"
	"path/filepath"
	"strings"
)

// Identity represents the bot's stable core identity.
type Identity struct {
	Name       string
	Version    string
	Type       string
	Domain     string
	Traits     string
	Boundaries string
}

// LoadIdentity reads IDENTITY.md from a specific directory.
// Returns empty Identity if file doesn't exist.
func LoadIdentity(binaryDir string) (*Identity, error) {
	path := filepath.Join(binaryDir, "IDENTITY.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	content := string(data)
	id := &Identity{}

	lines := strings.Split(content, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			// Extract name and version from first header line
			if id.Name == "" {
				header := strings.TrimSpace(strings.TrimPrefix(line, "#"))
				parts := strings.Split(header, " v")
				if len(parts) >= 1 {
					id.Name = strings.TrimSpace(parts[0])
				}
				if len(parts) >= 2 {
					id.Version = strings.TrimSpace(parts[1])
				}
			}
			continue
		}
		if strings.HasPrefix(line, "Type:") {
			id.Type = strings.TrimSpace(strings.TrimPrefix(line, "Type:"))
		} else if strings.HasPrefix(line, "Domain:") {
			id.Domain = strings.TrimSpace(strings.TrimPrefix(line, "Domain:"))
		} else if strings.HasPrefix(line, "Core traits:") {
			id.Traits = strings.TrimSpace(strings.TrimPrefix(line, "Core traits:"))
		} else if strings.HasPrefix(line, "Boundaries:") {
			id.Boundaries = strings.TrimSpace(strings.TrimPrefix(line, "Boundaries:"))
		}
	}

	return id, nil
}

// GetBinaryDir returns the directory containing the running executable.
func GetBinaryDir() string {
	execPath, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Dir(execPath)
}

// readIdentityFileFrom reads IDENTITY.md from a specific directory.
func readIdentityFileFrom(dir string) string {
	path := filepath.Join(dir, "IDENTITY.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}
