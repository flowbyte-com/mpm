//go:build !linux

package main

import (
	"os/exec"
	"strconv"
	"strings"
)

// readProcComm returns the short process name for pid by shelling out
// to `ps -p <pid> -o comm=`. The trailing `=` suppresses the header
// row so output is just the comm field. macOS and the BSDs ship this
// exact `ps` syntax; this is the portable POSIX fallback for the
// /proc path that only exists on Linux.
func readProcComm(pid int) (string, error) {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}