package main

// lesson_proposals_cli_test.go — the operator's side of the lesson
// trust boundary, end to end through the real `mpm` binary.
//
// The tool-surface test proves the agent cannot write a trusted lesson.
// This test proves the OTHER half: the operator CAN, and the whole
// path works from a terminal — propose (over `mpm call`, the agent's
// interface), review, promote.
//
// It runs the real binary rather than calling the handlers in-process,
// because the boundary is precisely about which INTERFACE a write
// arrives on.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runMpmInWorkspace runs the mpm binary against a workspace and
// returns its combined output.
func runMpmInWorkspace(t *testing.T, bin, ws string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "MPM_WORKSPACE="+ws)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mpm %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// TestLessonProposalCLI_ProposeThenPromote is the operator's end-to-end
// path.
//
// An agent proposes over `mpm call`; the operator sees it in
// `mpm lesson proposals`; the operator promotes it; and the promoted
// lesson is in the trusted surface.
func TestLessonProposalCLI_ProposeThenPromote(t *testing.T) {
	bin := buildMpmBinary(t)
	ws := t.TempDir()

	const content = "the release checklist must be run before every deploy"

	// 1. The agent proposes, over the agent's own interface.
	proposeOut := runMpmInWorkspace(t, bin, ws, "call", "mpm_lessons", "--payload",
		`{"action":"propose","params":{"fact":"`+content+`","type":"warning"}}`)
	var proposeEnvelope map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(lastJSONLine(proposeOut))), &proposeEnvelope); err != nil {
		t.Fatalf("decode the propose envelope %q: %v", proposeOut, err)
	}
	if success, _ := proposeEnvelope["success"].(bool); !success {
		t.Fatalf("the proposal failed: %v", proposeEnvelope)
	}
	proposalID, _ := proposeEnvelope["id"].(string)
	if proposalID == "" {
		t.Fatalf("the proposal returned no id: %v", proposeEnvelope)
	}
	if proposeEnvelope["status"] != "pending" {
		t.Errorf("status = %v, want \"pending\"", proposeEnvelope["status"])
	}
	if proposeEnvelope["trusted"] != false {
		t.Errorf("trusted = %v, want false", proposeEnvelope["trusted"])
	}

	// 2. The proposal is NOT in the trusted surface.
	searchOut := runMpmInWorkspace(t, bin, ws, "call", "mpm_lessons", "--payload",
		`{"action":"search","params":{"query":"release checklist"}}`)
	if strings.Contains(searchOut, content) {
		t.Errorf("the proposal is in the lesson SEARCH results before it was promoted:\n%s", searchOut)
	}
	listOut := runMpmInWorkspace(t, bin, ws, "lesson", "list")
	if strings.Contains(listOut, content) {
		t.Errorf("the proposal is in the lesson LIST before it was promoted:\n%s", listOut)
	}

	// 3. The operator sees it in the review queue.
	proposalsOut := runMpmInWorkspace(t, bin, ws, "lesson", "proposals")
	if !strings.Contains(proposalsOut, proposalID) {
		t.Errorf("the proposal is not in `mpm lesson proposals`:\n%s", proposalsOut)
	}
	if !strings.Contains(proposalsOut, content) {
		t.Errorf("the proposal's content is not in `mpm lesson proposals`:\n%s", proposalsOut)
	}

	// 4. The operator promotes it.
	promoteOut := runMpmInWorkspace(t, bin, ws, "lesson", "promote", proposalID)
	if !strings.Contains(promoteOut, proposalID) {
		t.Errorf("the promotion did not report the proposal:\n%s", promoteOut)
	}

	// 5. Now it IS in the trusted surface.
	searchOut = runMpmInWorkspace(t, bin, ws, "call", "mpm_lessons", "--payload",
		`{"action":"search","params":{"query":"release checklist"}}`)
	if !strings.Contains(searchOut, content) {
		t.Errorf("the promoted lesson is NOT in the lesson SEARCH results:\n%s", searchOut)
	}
	listOut = runMpmInWorkspace(t, bin, ws, "lesson", "list")
	if !strings.Contains(listOut, content) {
		t.Errorf("the promoted lesson is NOT in the lesson LIST:\n%s", listOut)
	}

	// 6. And it is no longer pending.
	proposalsOut = runMpmInWorkspace(t, bin, ws, "lesson", "proposals")
	if strings.Contains(proposalsOut, proposalID) {
		t.Errorf("the promoted proposal is still in the pending queue:\n%s", proposalsOut)
	}
	allOut := runMpmInWorkspace(t, bin, ws, "lesson", "proposals", "--status", "all")
	if !strings.Contains(allOut, proposalID) {
		t.Errorf("the promoted proposal is not in the full history:\n%s", allOut)
	}
}

// TestLessonProposalCLI_RejectKeepsTheRecord pins the decline path.
func TestLessonProposalCLI_RejectKeepsTheRecord(t *testing.T) {
	bin := buildMpmBinary(t)
	ws := t.TempDir()

	const content = "a proposal the operator will decline"

	proposeOut := runMpmInWorkspace(t, bin, ws, "call", "mpm_lessons", "--payload",
		`{"action":"propose","params":{"fact":"`+content+`","type":"insight"}}`)
	var envelope map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(lastJSONLine(proposeOut))), &envelope); err != nil {
		t.Fatalf("decode the propose envelope %q: %v", proposeOut, err)
	}
	proposalID, _ := envelope["id"].(string)
	if proposalID == "" {
		t.Fatalf("the proposal returned no id: %v", envelope)
	}

	rejectOut := runMpmInWorkspace(t, bin, ws, "lesson", "reject", proposalID)
	if !strings.Contains(rejectOut, proposalID) {
		t.Errorf("the rejection did not report the proposal:\n%s", rejectOut)
	}

	// The record is kept, and it is out of the pending queue.
	allOut := runMpmInWorkspace(t, bin, ws, "lesson", "proposals", "--status", "all")
	if !strings.Contains(allOut, proposalID) || !strings.Contains(allOut, content) {
		t.Errorf("the rejected proposal's record was discarded:\n%s", allOut)
	}
	pendingOut := runMpmInWorkspace(t, bin, ws, "lesson", "proposals")
	if strings.Contains(pendingOut, proposalID) {
		t.Errorf("the rejected proposal is still in the pending queue:\n%s", pendingOut)
	}

	// A rejected proposal never becomes a lesson.
	listOut := runMpmInWorkspace(t, bin, ws, "lesson", "list")
	if strings.Contains(listOut, content) {
		t.Errorf("a REJECTED proposal became a lesson:\n%s", listOut)
	}
}

// TestLessonProposalCLI_AgentSaveIsRefused pins the operator-visible
// half of the refusal: `mpm call` is the agent's interface, and it
// must refuse to write a trusted lesson — with an error that names the
// operator's actual path.
func TestLessonProposalCLI_AgentSaveIsRefused(t *testing.T) {
	bin := buildMpmBinary(t)
	ws := t.TempDir()

	cmd := exec.Command(bin, "call", "mpm_lessons", "--payload",
		`{"action":"save","params":{"fact":"the agent tried to write a trusted lesson"}}`)
	cmd.Env = append(os.Environ(), "MPM_WORKSPACE="+ws)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("mpm call mpm_lessons action=save succeeded:\n%s", out)
	}

	var envelope map[string]interface{}
	if jsonErr := json.Unmarshal([]byte(strings.TrimSpace(lastJSONLine(string(out)))), &envelope); jsonErr != nil {
		t.Fatalf("decode the refusal envelope %q: %v", out, jsonErr)
	}
	if success, _ := envelope["success"].(bool); success {
		t.Errorf("the refusal reported success: %v", envelope)
	}
	msg, _ := envelope["error"].(string)
	for _, fragment := range []string{"operator-only", "action=propose", "mpm lesson add"} {
		if !strings.Contains(msg, fragment) {
			t.Errorf("the refusal is missing %q: %q", fragment, msg)
		}
	}

	// And nothing was written.
	listOut := runMpmInWorkspace(t, bin, ws, "lesson", "list")
	if strings.Contains(listOut, "the agent tried to write a trusted lesson") {
		t.Errorf("the refused write created a lesson:\n%s", listOut)
	}
}

// TestLessonProposalCLI_OperatorAddStillWorks pins that the operator's
// direct path is untouched.
//
// The boundary refuses the AGENT's interface. It must not cost the
// operator their own command.
func TestLessonProposalCLI_OperatorAddStillWorks(t *testing.T) {
	bin := buildMpmBinary(t)
	ws := t.TempDir()

	const content = "a lesson the operator typed by hand"
	addOut := runMpmInWorkspace(t, bin, ws, "lesson", "add", content, "--type", "warning")
	if !strings.Contains(addOut, "added") {
		t.Errorf("`mpm lesson add` did not report success:\n%s", addOut)
	}

	listOut := runMpmInWorkspace(t, bin, ws, "lesson", "list")
	if !strings.Contains(listOut, content) {
		t.Errorf("the operator's lesson is not in the list:\n%s", listOut)
	}
	searchOut := runMpmInWorkspace(t, bin, ws, "call", "mpm_lessons", "--payload",
		`{"action":"search","params":{"query":"typed by hand"}}`)
	if !strings.Contains(searchOut, content) {
		t.Errorf("the operator's lesson is not in the search results:\n%s", searchOut)
	}
}

// lastJSONLine returns the last line of output that parses as a JSON
// object. The CLI prints log noise before the envelope on some paths,
// so the envelope is not always the first line.
func lastJSONLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "{") {
			continue
		}
		if json.Valid([]byte(line)) {
			return line
		}
	}
	return strings.TrimSpace(out)
}

// TestLessonProposalCLI_TheWorkspaceHasNoStrayFiles guards the
// operator's workspace: the review surface is a table, not a file.
func TestLessonProposalCLI_TheWorkspaceHasNoStrayFiles(t *testing.T) {
	bin := buildMpmBinary(t)
	ws := t.TempDir()

	runMpmInWorkspace(t, bin, ws, "call", "mpm_lessons", "--payload",
		`{"action":"propose","params":{"fact":"a proposal that leaves no file behind"}}`)

	entries, err := os.ReadDir(ws)
	if err != nil {
		t.Fatalf("read the workspace: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.Contains(entry.Name(), "proposal") {
			t.Errorf("the proposal left a file in the workspace: %s", filepath.Join(ws, entry.Name()))
		}
	}
}
