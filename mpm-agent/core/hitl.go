package core

import (
	"fmt"
	"sync"
	"time"
)

// ApprovalResult is sent back from the callback handler.
type ApprovalResult struct {
	Approved bool
	Output   string
}

// pendingApproval is stored in sync.Map while waiting for user response.
type pendingApproval struct {
	Action    string                 // "write_file: path" or "execute_shell: command"
	Tool      string                 // tool name
	Args      map[string]interface{} // tool arguments
	Channel   chan *ApprovalResult   // channel to receive approval result
	Timestamp time.Time
}

// pendingApprovals is the sync.Map of execution_id → *pendingApproval.
var pendingApprovals sync.Map

// generateExecID returns an 8-char hex ID for an approval request.
func generateExecID() string {
	id := fmt.Sprintf("%x", time.Now().UnixNano())
	return id[:8]
}

// RequestApproval stores a pending action and returns the approval channel.
// The caller MUST select on the channel, HITL timeout (60s), and ctx.Done().
func RequestApproval(tool string, action string, args map[string]interface{}) (execID string, approvalChan chan *ApprovalResult) {
	execID = generateExecID()
	approvalChan = make(chan *ApprovalResult, 1)

	pendingApprovals.Store(execID, &pendingApproval{
		Action:    action,
		Tool:      tool,
		Args:      args,
		Channel:   approvalChan,
		Timestamp: time.Now(),
	})

	return execID, approvalChan
}

// Approve resolves a pending approval with "approved".
func Approve(execID string, output string) {
	if p, ok := pendingApprovals.LoadAndDelete(execID); ok {
		pa := p.(*pendingApproval)
		pa.Channel <- &ApprovalResult{Approved: true, Output: output}
		close(pa.Channel)
	}
}

// Deny resolves a pending approval with "denied".
func Deny(execID string) {
	if p, ok := pendingApprovals.LoadAndDelete(execID); ok {
		pa := p.(*pendingApproval)
		pa.Channel <- &ApprovalResult{Approved: false, Output: "User denied permission"}
		close(pa.Channel)
	}
}

// GetPendingApproval retrieves a pending approval by execID.
func GetPendingApproval(execID string) (*pendingApproval, bool) {
	if p, ok := pendingApprovals.Load(execID); ok {
		return p.(*pendingApproval), true
	}
	return nil, false
}