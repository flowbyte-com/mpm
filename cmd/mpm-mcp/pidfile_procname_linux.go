//go:build linux

package main

import (
	"fmt"
	"os"
	"strings"
)

// readProcComm returns the short process name for pid by reading
// /proc/<pid>/comm. This is the cheapest portable path on Linux —
// no fork, no shell, no parsing. Returns an error if /proc is
// unavailable (containerized namespaces without /proc) or the pid
// is gone.
func readProcComm(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}