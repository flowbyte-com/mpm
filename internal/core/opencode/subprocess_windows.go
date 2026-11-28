//go:build !unix

package opencode

// Windows fallback: no POSIX process groups. The DIRECT child is
// terminated by exec's cancellation (Process.Kill) and WaitDelay bounds
// the pipe copy; descendant accounting beyond that is not attempted here.

import (
	"os/exec"
	"time"
)

const (
	syscallSIGTERM = 0
	syscallSIGKILL = 0
)

func setProcessGroup(cmd *exec.Cmd) {}

func signalProcessGroup(cmd *exec.Cmd, sig int) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

func terminateProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

var _ = time.Second
