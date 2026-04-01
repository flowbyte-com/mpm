//go:build ignore

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Mode struct {
	SymID        string      `json:"sym_id"`
	Title        string      `json:"title"`
	Name         string      `json:"name"`
}

func main() {
	// Simulate what the ACTUAL mpm binary would see
	// The mpm binary is at /home/v/.openclaw/workspace/bin/mpm
	// GetMPMDir resolves it to /home/v/.openclaw/workspace/projects/mpm
	
	// So use the hardcoded path directly for testing
	jsonDir := "/home/v/.openclaw/workspace/projects/mpm/mode"
	fmt.Println("Testing JSONDir:", jsonDir)
	
	files, err := os.ReadDir(jsonDir)
	if err != nil {
		fmt.Printf("ReadDir ERROR: %v\n", err)
		return
	}
	fmt.Printf("Total entries: %d\n", len(files))
	
	count := 0
	for _, file := range files {
		name := file.Name()
		if file.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		
		jsonPath := filepath.Join(jsonDir, name)
		data, err := os.ReadFile(jsonPath)
		if err != nil {
			fmt.Printf("  READ ERROR %s: %v\n", name, err)
			continue
		}
		
		var m Mode
		if err := json.Unmarshal(data, &m); err != nil {
			fmt.Printf("  JSON ERROR %s: %v\n", name, err)
			continue
		}
		
		if m.SymID != "" {
			count++
			fmt.Printf("  [%d] %s: SymID=%q Name=%q Title=%q\n", count, name, m.SymID, m.Name, m.Title)
		} else {
			fmt.Printf("  [EMPTY SymID] %s: Name=%q Title=%q\n", name, m.Name, m.Title)
		}
	}
	fmt.Printf("Total modes with SymID: %d\n", count)
}
