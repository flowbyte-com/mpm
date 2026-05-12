package core

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// executeCmdWithTimeout runs a shell command with a strict timeout (max 300s).
// Input: { "cmd": "go test ./...", "timeout_seconds": 60 }
// Output: "[elapsed] Exit: N\nstderr\nstdout"
func executeCmdWithTimeout(args map[string]interface{}) (string, error) {
	cmdStr, _ := args["cmd"].(string)
	timeoutSec, ok := args["timeout_seconds"].(float64)
	if !ok || timeoutSec <= 0 {
		timeoutSec = 60
	}
	if timeoutSec > 300 {
		return "", fmt.Errorf("execute_cmd_with_timeout: timeout cannot exceed 300 seconds (security limit)")
	}

	if cmdStr == "" {
		return "", fmt.Errorf("execute_cmd_with_timeout requires 'cmd' field")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
	defer cancel()

	start := time.Now()
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	cmd.Dir, _ = os.Getwd()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	elapsed := time.Since(start)

	result := fmt.Sprintf("[%s] Exit: %d\n%s%s",
		elapsed.Round(time.Millisecond),
		exitCodeFromError(err),
		stderr.String(),
		stdout.String())

	if ctx.Err() == context.DeadlineExceeded {
		return result + "\n⏱ TIMEOUT", fmt.Errorf("command exceeded %v timeout", time.Duration(timeoutSec)*time.Second)
	}
	return result, err
}

func exitCodeFromError(err error) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}
	return -1
}