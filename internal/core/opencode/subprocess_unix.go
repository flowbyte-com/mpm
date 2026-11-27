//go:build unix

package opencode

// Process-group lifecycle for unix-like hosts (Linux, macOS, BSDs).
//
// The child is started as a PROCESS-GROUP LEADER, so cancellation can
// terminate the whole owned group — the ordinary way to bound a child
// whose descendants inherited its output pipes. This is subprocess
// accounting, not a sandbox: a process that calls setsid itself, or
// otherwise leaves the group, is outside it, and MPM does not claim
// otherwise.

import (
	"os/exec"
	"syscall"
)

// The signals used for the two-step termination.
const (
	syscallSIGTERM = syscall.SIGTERM
	syscallSIGKILL = syscall.SIGKILL
)

// setProcessGroup puts the child in its own process group.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// signalProcessGroup signals the child's OWN group. The child is its
// group leader (Setpgid), so the group id is the child's pid; signalling
// a non-existent group is ESRCH and harmless. Nothing outside that group
// is ever signalled.
func signalProcessGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, sig)
}

// terminateProcessGroup force-terminates the child's group.
func terminateProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
