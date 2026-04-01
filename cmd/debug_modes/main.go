package main

import (
	"fmt"
	"mpm/internal"
	"mpm/internal/config"
	"os"
	"path/filepath"
)

func main() {
	execPath, _ := os.Executable()
	fmt.Printf("Executable: %s\n", execPath)
	fmt.Printf("GetWorkspace: %s\n", config.GetWorkspace())
	fmt.Printf("GetMPMDir: %s\n", config.GetMPMDir())
	
	// Also trace GetMPMDir manually
	execDir := filepath.Dir(execPath)
	base := filepath.Base(execDir)
	fmt.Printf("execDir=%s base=%s\n", execDir, base)
	
	mm := internal.NewModeManager("")
	fmt.Printf("JSONDir: %s\n", mm.JSONDir)
	
	// Test what os.ReadDir sees
	files, _ := os.ReadDir(mm.JSONDir)
	fmt.Printf("ReadDir entries: %d\n", len(files))
	for _, f := range files {
		if !f.IsDir() {
			fmt.Printf("  file: %s\n", f.Name())
		}
	}
	
	modes, err := mm.List()
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Mode count: %d\n", len(modes))
	for i, m := range modes {
		fmt.Printf("  [%d] Name=%q Title=%q SymID=%q\n", i+1, m.Name, m.Title, m.SymID)
	}
}
