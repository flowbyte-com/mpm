// handlers_assignment.go — mpm_assignment, the agent-facing surface over
// the assignment coordination service.
//
// # WHAT THIS SURFACE IS, AND WHAT IT IS NOT
//
// This is the surface an AGENT uses: it delivers work, acknowledges it,
// accepts it, claims it, renews the claim, starts it, completes it,
// delegates it, records a review verdict, hands it to the operator's
// gate, cancels it, and reads its inbox, its row and its ledger.
//
// TWO OPERATIONS ARE NOT HERE, AND THEIR ABSENCE IS THE POINT:
//
//   - creating a ROOT assignment, and
//   - recording an OPERATOR APPROVAL.
//
// Neither appears as an action, neither appears in the schema, and
// neither can be reached through this handler. They are not "refused at
// call time by a flag" — they are ABSENT, so there is nothing to
// dispatch to. The shred precedent refuses at the execution point
// because the action had to keep working for the operator; here the
// operator path is a DIFFERENT entry point (`mpm assignment create-root`
// and `mpm assignment record-approval`), so the agent surface can
// simply not have the capability at all.
//
// A confirmation flag would not have been a boundary: a boolean the
// agent itself sets cannot distinguish "the operator asked for this"
// from "the agent decided this".
//
// # ATTRIBUTION IS NOT IDENTITY
//
// Every write here records WHO the caller says it is. Nothing in this
// package branches on that attribution to grant authority, and the
// recorded address is never treated as a credential.

package tools

import (
	"encoding/json"
	"fmt"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/assignment"
)

// handleMpmAssignment implements the mpm_assignment tool.
//
// The handler is a thin adapter: it extracts the parameters and calls
// the service. All of the behaviour — the state machine, the fence, the
// delegation bounds, the review's independence — lives in
// internal/core/assignment, so the CLI and this surface share ONE
// implementation.
func handleMpmAssignment(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
	params, err := extractParamsOrFail("mpm_assignment", payload)
	if err != nil {
		return nil, err
	}
	action, _ := payload["action"].(string)

	store := assignment.NewStore(dm)

	switch action {
	case "inbox":
		return assignmentInbox(store, params)
	case "show":
		return assignmentShow(store, params)
	case "history":
		return assignmentHistory(store, params)
	case "tree":
		return assignmentTree(store, params)
	case "deliver":
		return assignmentDeliver(store, params)
	case "acknowledge":
		return assignmentAcknowledge(store, params)
	case "accept":
		return assignmentAccept(store, params)
	case "refuse":
		return assignmentRefuse(store, params)
	case "claim":
		return assignmentClaim(store, params)
	case "renew":
		return assignmentRenew(store, params)
	case "start":
		return assignmentStart(store, params)
	case "complete":
		return assignmentComplete(store, params)
	case "delegate":
		return assignmentDelegate(store, params)
	case "cancel":
		return assignmentCancel(store, params)
	case "note":
		return assignmentNote(store, params)
	case "record-review":
		return assignmentRecordReview(store, params)
	case "handoff":
		return assignmentHandoff(store, params)
	default:
		return nil, fmt.Errorf(
			"mpm_assignment: unknown action %q. Valid actions include inbox, show, "+
				"history, tree, deliver, acknowledge, accept, refuse, claim, renew, "+
				"start, complete, delegate, cancel, note, record-review, handoff", action)
	}
}

// ── parameter helpers ─────────────────────────────────────────────────────

// assignmentActor reads the attribution from the parameters.
//
// The address is RECORDED, never verified. An absent address is not an
// error: the service attributes the write to the assignment's own
// recipient, which is a structural fact rather than a claim.
func assignmentActor(p map[string]interface{}) assignment.Actor {
	address := getString(p, "actor")
	kind := getString(p, "actor_kind")
	if kind == "" {
		kind = "agent"
	}
	return assignment.Actor{Address: address, Kind: kind}
}

// assignmentInt reads an integer parameter.
func assignmentInt(p map[string]interface{}, key string) int64 {
	switch v := p[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0
		}
		return n
	}
	return 0
}

// assignmentStrings reads a list of strings from the parameters.
func assignmentStrings(p map[string]interface{}, key string) []string {
	raw, ok := p[key].([]interface{})
	if !ok {
		if list, ok := p[key].([]string); ok {
			return list
		}
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// assignmentPayload reads the free-form payload object.
func assignmentPayload(p map[string]interface{}) map[string]interface{} {
	if v, ok := p["payload"].(map[string]interface{}); ok {
		return v
	}
	return nil
}

// assignmentID reads the assignment id, which every action needs.
func assignmentID(p map[string]interface{}) string { return getString(p, "id") }

// ── the reads ─────────────────────────────────────────────────────────────

func assignmentInbox(store *assignment.Store, p map[string]interface{}) (interface{}, error) {
	q := assignment.InboxQuery{
		Recipient: getString(p, "recipient"),
		ProjectID: getString(p, "project_id"),
		States:    assignmentStrings(p, "states"),
		Limit:     int(assignmentInt(p, "limit")),
	}
	items, err := store.Inbox(q)
	if err != nil {
		return nil, err
	}
	return &assignment.AssignmentList{Assignments: items, Count: len(items)}, nil
}

func assignmentShow(store *assignment.Store, p map[string]interface{}) (interface{}, error) {
	a, err := store.Get(assignmentID(p))
	if err != nil {
		return nil, err
	}
	return a, nil
}

func assignmentHistory(store *assignment.Store, p map[string]interface{}) (interface{}, error) {
	events, err := store.History(assignmentID(p))
	if err != nil {
		return nil, err
	}
	return &assignment.EventList{Events: events, Count: len(events)}, nil
}

func assignmentTree(store *assignment.Store, p map[string]interface{}) (interface{}, error) {
	rootID := getString(p, "root_id")
	if rootID == "" {
		rootID = assignmentID(p)
	}
	items, err := store.ListByRoot(rootID)
	if err != nil {
		return nil, err
	}
	return &assignment.AssignmentList{Assignments: items, Count: len(items)}, nil
}

// ── the transitions ───────────────────────────────────────────────────────

func assignmentDeliver(store *assignment.Store, p map[string]interface{}) (interface{}, error) {
	return store.Deliver(assignment.FencedParams{
		ID:    assignmentID(p),
		Epoch: assignmentInt(p, "epoch"),
		Actor: assignmentActor(p),
		Note:  getString(p, "note"),
	})
}

func assignmentAcknowledge(store *assignment.Store, p map[string]interface{}) (interface{}, error) {
	return store.Acknowledge(assignment.FencedParams{
		ID:    assignmentID(p),
		Epoch: assignmentInt(p, "epoch"),
		Actor: assignmentActor(p),
		Note:  getString(p, "note"),
	})
}

func assignmentAccept(store *assignment.Store, p map[string]interface{}) (interface{}, error) {
	return store.Accept(assignment.FencedParams{
		ID:    assignmentID(p),
		Epoch: assignmentInt(p, "epoch"),
		Actor: assignmentActor(p),
		Note:  getString(p, "note"),
	})
}

func assignmentRefuse(store *assignment.Store, p map[string]interface{}) (interface{}, error) {
	return store.Refuse(assignment.FencedParams{
		ID:    assignmentID(p),
		Epoch: assignmentInt(p, "epoch"),
		Actor: assignmentActor(p),
		Note:  getString(p, "note"),
	})
}

func assignmentClaim(store *assignment.Store, p map[string]interface{}) (interface{}, error) {
	return store.Claim(assignment.ClaimParams{
		ID:            assignmentID(p),
		ExpectedEpoch: assignmentInt(p, "epoch"),
		Owner:         getString(p, "owner"),
		LeaseSeconds:  assignmentInt(p, "lease_seconds"),
		Actor:         assignmentActor(p),
	})
}

func assignmentRenew(store *assignment.Store, p map[string]interface{}) (interface{}, error) {
	return store.Renew(assignment.RenewParams{
		ID:            assignmentID(p),
		ExpectedEpoch: assignmentInt(p, "epoch"),
		Owner:         getString(p, "owner"),
		LeaseSeconds:  assignmentInt(p, "lease_seconds"),
		Actor:         assignmentActor(p),
	})
}

func assignmentStart(store *assignment.Store, p map[string]interface{}) (interface{}, error) {
	return store.Start(assignment.FencedParams{
		ID:    assignmentID(p),
		Epoch: assignmentInt(p, "epoch"),
		Actor: assignmentActor(p),
		Note:  getString(p, "note"),
	})
}

func assignmentComplete(store *assignment.Store, p map[string]interface{}) (interface{}, error) {
	return store.Complete(assignment.FencedParams{
		ID:    assignmentID(p),
		Epoch: assignmentInt(p, "epoch"),
		Actor: assignmentActor(p),
		Note:  getString(p, "note"),
	})
}

func assignmentCancel(store *assignment.Store, p map[string]interface{}) (interface{}, error) {
	return store.Cancel(assignment.CancelParams{
		ID:    assignmentID(p),
		Epoch: assignmentInt(p, "epoch"),
		Actor: assignmentActor(p),
		Note:  getString(p, "note"),
	})
}

func assignmentDelegate(store *assignment.Store, p map[string]interface{}) (interface{}, error) {
	a, _, err := store.Delegate(assignment.DelegateParams{
		ParentID:       getString(p, "parent_id"),
		Title:          getString(p, "title"),
		Objective:      getString(p, "objective"),
		Recipient:      getString(p, "recipient"),
		Role:           getString(p, "role"),
		IdempotencyKey: getString(p, "idempotency_key"),
		Actor:          assignmentActor(p),
		ResourceUnits:  assignmentInt(p, "resource_units"),
		IssueRef:       getString(p, "issue_ref"),
		PRRef:          getString(p, "pr_ref"),
		BaseSHA:        getString(p, "base_sha"),
		HeadSHA:        getString(p, "head_sha"),
		ReviewedSHA:    getString(p, "reviewed_sha"),
		FindingsSHA256: getString(p, "findings_sha256"),
		EvidenceRef:    getString(p, "evidence_ref"),
		Payload:        assignmentPayload(p),
	})
	if err != nil {
		return nil, err
	}
	return a, nil
}

func assignmentNote(store *assignment.Store, p map[string]interface{}) (interface{}, error) {
	return store.Note(assignment.NoteParams{
		ID:      assignmentID(p),
		Actor:   assignmentActor(p),
		Note:    getString(p, "note"),
		Payload: assignmentPayload(p),
	})
}

func assignmentRecordReview(store *assignment.Store, p map[string]interface{}) (interface{}, error) {
	return store.RecordReviewResult(assignment.ReviewResultParams{
		ReviewerID: assignmentID(p),
		Verdict:    getString(p, "verdict"),
		Epoch:      assignmentInt(p, "epoch"),
		Actor:      assignmentActor(p),
		Note:       getString(p, "note"),
	})
}

func assignmentHandoff(store *assignment.Store, p map[string]interface{}) (interface{}, error) {
	return store.HandoffToOperator(assignment.HandoffParams{
		ID:             assignmentID(p),
		Epoch:          assignmentInt(p, "epoch"),
		AwaitingAction: getString(p, "awaiting_action"),
		Actor:          assignmentActor(p),
		Note:           getString(p, "note"),
	})
}
