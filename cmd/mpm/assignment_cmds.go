// cmd/mpm/assignment_cmds.go — the `mpm assignment` namespace.
//
// # ONE IMPLEMENTATION, TWO DOORS
//
// The CLI and the MCP/`mpm call` surface are two doors into the SAME
// business logic: internal/core/assignment. Every verb here calls the
// service directly, and the tool handler calls the same service. There
// is no second copy of the state machine, the fence, the delegation
// bounds or the review's independence check.
//
// # TWO OPERATOR-ONLY VERBS
//
//	`mpm assignment create-root`      — creates the top-level work order
//	`mpm assignment record-approval`  — records an externally verified approval
//
// Neither verb is an MCP action and neither is reachable through
// `mpm call`. They are absent from the registry entirely, so no agent
// dispatch path can reach them: there is no action to dispatch TO.
//
// They are not hidden behind a confirmation flag either. A boolean the
// caller sets is not a credential — it cannot distinguish "the operator
// asked for this" from "the caller decided this". A DEDICATED VERB is
// the boundary: the capability exists, and it exists only in the
// operator's own front door.
//
// # WHAT NONE OF THESE VERBS DO
//
// None of them merges a pull request, deploys, or changes any state
// outside the assignment ledger. The operator's approval RECORDS that
// the operator authorised something; it does not perform it. The
// authority to merge or deploy stays where it was: with the operator,
// in whatever tool they already use.

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/flowbyte-com/mpm-core/assignment"
	"github.com/flowbyte-com/mpm-core/usererror"
)

// assignmentFlags is the parsed flag set of one `mpm assignment` verb.
type assignmentFlags struct {
	// identity
	id        string
	rootID    string
	parentID  string
	projectID string

	// addressing
	recipient string
	actor     string
	actorKind string

	// content
	title          string
	objective      string
	role           string
	note           string
	verdict        string
	awaitingAction string

	// the claim
	epoch        int64
	owner        string
	leaseSeconds int64

	// creation
	idempotencyKey   string
	permitted        []string
	eligible         []string
	maxDepth         int64
	maxTasks         int64
	maxConcurrency   int64
	maxResourceUnits int64
	resourceUnits    int64

	// evidence
	issueRef       string
	prRef          string
	baseSHA        string
	headSHA        string
	reviewedSHA    string
	findingsSHA256 string
	evidenceRef    string

	// query
	states []string
	limit  int

	// approval
	action   string
	revision string
	source   string
	evidence string

	// payload is the structured instruction the assignment carries —
	// a reviewer's target, for instance. It is set by the delegation
	// that creates the assignment, because that is the only place
	// that can fix it.
	payload string

	// output
	jsonOutput bool
}

// handleAssignment is the entry point for `mpm assignment <verb>`.
func handleAssignment(args []string) int {
	if len(args) == 0 {
		printAssignmentHelp()
		return 0
	}
	sub := args[0]
	rest := args[1:]

	switch sub {
	case "inbox":
		return assignmentVerb(rest, assignmentVerbInbox)
	case "show":
		return assignmentVerb(rest, assignmentVerbShow)
	case "history":
		return assignmentVerb(rest, assignmentVerbHistory)
	case "tree":
		return assignmentVerb(rest, assignmentVerbTree)
	case "deliver":
		return assignmentVerb(rest, assignmentVerbDeliver)
	case "acknowledge":
		return assignmentVerb(rest, assignmentVerbAcknowledge)
	case "accept":
		return assignmentVerb(rest, assignmentVerbAccept)
	case "refuse":
		return assignmentVerb(rest, assignmentVerbRefuse)
	case "claim":
		return assignmentVerb(rest, assignmentVerbClaim)
	case "renew":
		return assignmentVerb(rest, assignmentVerbRenew)
	case "start":
		return assignmentVerb(rest, assignmentVerbStart)
	case "complete":
		return assignmentVerb(rest, assignmentVerbComplete)
	case "delegate":
		return assignmentVerb(rest, assignmentVerbDelegate)
	case "cancel":
		return assignmentVerb(rest, assignmentVerbCancel)
	case "note":
		return assignmentVerb(rest, assignmentVerbNote)
	case "record-review":
		return assignmentVerb(rest, assignmentVerbRecordReview)
	case "handoff":
		return assignmentVerb(rest, assignmentVerbHandoff)
	case "create-root":
		return assignmentVerb(rest, assignmentVerbCreateRoot)
	case "record-approval":
		return assignmentVerb(rest, assignmentVerbRecordApproval)
	case "help", "-h", "--help":
		printAssignmentHelp()
		return 0
	default:
		usererror.Error("mpm assignment: unknown verb %q\n"+
			"available verbs: inbox, show, history, tree, deliver, acknowledge, accept,\n"+
			"  refuse, claim, renew, start, complete, delegate, cancel, note,\n"+
			"  record-review, handoff, create-root, record-approval", sub)
		return 1
	}
}

// ── the flag parser ───────────────────────────────────────────────────────

// assignmentSplitList splits a comma-separated flag value.
//
// Go's flag package has no repeatable flag, and three of this
// namespace's flags carry a list: the operations a root permits, the
// recipients a root allows, and the states an inbox is filtered by.
// The comma-separated form is the CLI's existing convention for a
// list-valued flag (`mpm handoff write --tag a,b,c`).
func assignmentSplitList(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// assignmentParseFlags parses the flags of one verb.
//
// Every verb accepts every flag and IGNORES the ones that do not apply
// to it, rather than each verb declaring its own set. A flag set that
// differs per verb is a flag set that surprises: the operator types
// `--json` on a verb that does not take it and gets an error about a
// flag they cannot see.
//
// The registrations below are the canonical vocabulary: the CLI's
// option registry derives its accepted names from them, so an option
// added here without being registered there is caught by the drift
// test rather than by an operator.
func assignmentParseFlags(args []string) (*assignmentFlags, error) {
	fs := flag.NewFlagSet("mpm assignment", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}

	// The registrations below are the canonical vocabulary: the CLI's
	// option registry derives its accepted names from them, so an
	// option added here without being registered there is caught by
	// the drift test rather than by an operator.
	id := fs.String("id", "", "the assignment id")
	rootID := fs.String("root-id", "", "the root of the delegation tree")
	parentID := fs.String("parent-id", "", "the assignment being delegated from")
	projectID := fs.String("project-id", "", "the project the work belongs to")
	recipient := fs.String("recipient", "", "the address the work is addressed to")
	actor := fs.String("actor", "", "who is writing the row (attribution, not identity)")
	actorKind := fs.String("actor-kind", "", "the kind of the writer (agent, operator, runtime)")
	title := fs.String("title", "", "the title of the work")
	objective := fs.String("objective", "", "the objective of the work")
	role := fs.String("role", "", "implementer or reviewer")
	note := fs.String("note", "", "a note recorded on the ledger")
	verdict := fs.String("verdict", "", "passed or changes_requested")
	awaitingAction := fs.String("awaiting-action", "", "the EXACT action the operator is asked to authorise")
	epoch := fs.Int64("epoch", 0, "the claim epoch the caller holds (the fence)")
	owner := fs.String("owner", "", "the holder of the lease")
	leaseSeconds := fs.Int64("lease-seconds", 0, "the length of the lease")
	idempotencyKey := fs.String("idempotency-key", "", "the key that identifies the logical assignment")
	permitted := fs.String("permitted-operation", "", "the operations the root permits (comma-separated)")
	eligible := fs.String("eligible-recipient", "", "the recipients the root allows (comma-separated)")
	maxDepth := fs.Int64("max-depth", 0, "the root's depth budget")
	maxTasks := fs.Int64("max-tasks", 0, "the root's task budget")
	maxConcurrency := fs.Int64("max-concurrency", 0, "the root's concurrency budget")
	maxResourceUnits := fs.Int64("max-resource-units", 0, "the root's resource budget")
	resourceUnits := fs.Int64("resource-units", 0, "the resource units this assignment costs")
	issueRef := fs.String("issue-ref", "", "the issue this work answers")
	prRef := fs.String("pr-ref", "", "the pull request this work produces")
	baseSHA := fs.String("base-sha", "", "the base revision of the work")
	headSHA := fs.String("head-sha", "", "the head revision of the work")
	reviewedSHA := fs.String("reviewed-sha", "", "the revision that was reviewed")
	findingsSHA256 := fs.String("findings-sha256", "", "the digest of the review's findings")
	evidenceRef := fs.String("evidence-ref", "", "a pointer to the evidence")
	states := fs.String("state", "", "the states to filter the inbox by (comma-separated)")
	limit := fs.Int("limit", 0, "the row limit")
	action := fs.String("action", "", "the action that was authorised")
	revision := fs.String("revision", "", "the revision the authorisation names")
	source := fs.String("source", "", "where the authorisation came from")
	evidence := fs.String("evidence", "", "the evidence pointer behind the authorisation")
	payload := fs.String("payload", "", "a JSON object stored on the assignment (e.g. the reviewer's target)")
	jsonOutput := fs.Bool("json", false, "emit JSON")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	return &assignmentFlags{
		id:               *id,
		rootID:           *rootID,
		parentID:         *parentID,
		projectID:        *projectID,
		recipient:        *recipient,
		actor:            *actor,
		actorKind:        *actorKind,
		title:            *title,
		objective:        *objective,
		role:             *role,
		note:             *note,
		verdict:          *verdict,
		awaitingAction:   *awaitingAction,
		epoch:            *epoch,
		owner:            *owner,
		leaseSeconds:     *leaseSeconds,
		idempotencyKey:   *idempotencyKey,
		permitted:        assignmentSplitList(*permitted),
		eligible:         assignmentSplitList(*eligible),
		maxDepth:         *maxDepth,
		maxTasks:         *maxTasks,
		maxConcurrency:   *maxConcurrency,
		maxResourceUnits: *maxResourceUnits,
		resourceUnits:    *resourceUnits,
		issueRef:         *issueRef,
		prRef:            *prRef,
		baseSHA:          *baseSHA,
		headSHA:          *headSHA,
		reviewedSHA:      *reviewedSHA,
		findingsSHA256:   *findingsSHA256,
		evidenceRef:      *evidenceRef,
		states:           assignmentSplitList(*states),
		limit:            *limit,
		action:           *action,
		revision:         *revision,
		source:           *source,
		evidence:         *evidence,
		payload:          *payload,
		jsonOutput:       *jsonOutput,
	}, nil
}

// assignmentParsePayload reads the --payload flag.
//
// The payload is the one part of an assignment that carries a STRUCTURED
// instruction rather than a field: a reviewer's target, for instance, is
// fixed by the delegation that creates the reviewer, and the delegation
// is the only place that can name it.
//
// A malformed payload is an error, not a silent empty object: a caller
// that meant to name a review target and spelled it wrong must not end
// up with an assignment that reviews nothing.
func assignmentParsePayload(raw string) (map[string]interface{}, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("--payload must be a JSON object: %v", err)
	}
	if out == nil {
		return nil, fmt.Errorf("--payload must be a JSON object, not null")
	}
	return out, nil
}

// ── the verb runner ───────────────────────────────────────────────────────

// assignmentVerbFn is one verb.
type assignmentVerbFn func(store *assignment.Store, f *assignmentFlags) (interface{}, error)

// assignmentVerb parses the flags, opens the store and runs the verb.
func assignmentVerb(args []string, fn assignmentVerbFn) int {
	f, err := assignmentParseFlags(args)
	if err != nil {
		usererror.Error("mpm assignment: %v", err)
		return 1
	}
	dm := getDBConcrete()
	if dm == nil {
		usererror.Error("database unavailable")
		return 1
	}
	out, err := fn(assignment.NewStore(dm), f)
	if err != nil {
		usererror.Error("mpm assignment: %v", err)
		return 1
	}
	return assignmentRender(out, f.jsonOutput)
}

// assignmentRender prints the result.
//
// The JSON shape is the ASSIGNMENT itself, so a machine consumer reads
// the same fields the service produced. The human shape is a compact
// summary; the full row is one `--json` away.
func assignmentRender(out interface{}, asJSON bool) int {
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			usererror.Error("json encode failed: %v", err)
			return 1
		}
		return 0
	}
	switch v := out.(type) {
	case *assignment.Assignment:
		fmt.Printf("%s  %s\n", v.ID, v.State)
		fmt.Printf("  title     : %s\n", v.Title)
		fmt.Printf("  recipient : %s (%s)\n", v.Recipient, v.Role)
		fmt.Printf("  epoch     : %d", v.ClaimEpoch)
		if v.Owner != "" {
			fmt.Printf("  owner: %s", v.Owner)
		}
		fmt.Println()
		if v.AwaitingAction != "" {
			fmt.Printf("  awaiting  : %s\n", v.AwaitingAction)
		}
		if v.ApprovalAction != "" {
			fmt.Printf("  approved  : %s @ %s\n", v.ApprovalAction, v.ApprovalRevision)
		}
	case assignment.Assignment:
		return assignmentRender(&v, asJSON)
	case *assignment.AssignmentList:
		for _, a := range v.Assignments {
			fmt.Printf("%s  %-18s %-12s %s\n", a.ID, a.State, a.Recipient, a.Title)
		}
		fmt.Printf("\n%d assignment(s)\n", len(v.Assignments))
	case *assignment.EventList:
		for _, e := range v.Events {
			to := e.ToState
			if to == "" {
				to = "-"
			}
			fmt.Printf("%3d  %-18s %-18s %s\n", e.EventIndex, e.EventType, to, e.Note)
		}
		fmt.Printf("\n%d event(s)\n", len(v.Events))
	default:
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			usererror.Error("json encode failed: %v", err)
			return 1
		}
	}
	return 0
}

// ── the verbs ─────────────────────────────────────────────────────────────

func assignmentVerbInbox(store *assignment.Store, f *assignmentFlags) (interface{}, error) {
	items, err := store.Inbox(assignment.InboxQuery{
		Recipient: f.recipient,
		ProjectID: f.projectID,
		States:    f.states,
		Limit:     f.limit,
	})
	if err != nil {
		return nil, err
	}
	return &assignment.AssignmentList{Assignments: items}, nil
}

func assignmentVerbShow(store *assignment.Store, f *assignmentFlags) (interface{}, error) {
	return store.Get(f.id)
}

func assignmentVerbHistory(store *assignment.Store, f *assignmentFlags) (interface{}, error) {
	events, err := store.History(f.id)
	if err != nil {
		return nil, err
	}
	return &assignment.EventList{Events: events}, nil
}

func assignmentVerbTree(store *assignment.Store, f *assignmentFlags) (interface{}, error) {
	rootID := f.rootID
	if rootID == "" {
		rootID = f.id
	}
	items, err := store.ListByRoot(rootID)
	if err != nil {
		return nil, err
	}
	return &assignment.AssignmentList{Assignments: items}, nil
}

func assignmentVerbDeliver(store *assignment.Store, f *assignmentFlags) (interface{}, error) {
	return store.Deliver(assignmentFenced(f))
}

func assignmentVerbAcknowledge(store *assignment.Store, f *assignmentFlags) (interface{}, error) {
	return store.Acknowledge(assignmentFenced(f))
}

func assignmentVerbAccept(store *assignment.Store, f *assignmentFlags) (interface{}, error) {
	return store.Accept(assignmentFenced(f))
}

func assignmentVerbRefuse(store *assignment.Store, f *assignmentFlags) (interface{}, error) {
	return store.Refuse(assignmentFenced(f))
}

func assignmentVerbStart(store *assignment.Store, f *assignmentFlags) (interface{}, error) {
	return store.Start(assignmentFenced(f))
}

func assignmentVerbComplete(store *assignment.Store, f *assignmentFlags) (interface{}, error) {
	return store.Complete(assignmentFenced(f))
}

func assignmentVerbCancel(store *assignment.Store, f *assignmentFlags) (interface{}, error) {
	return store.Cancel(assignment.CancelParams{
		ID:    f.id,
		Epoch: f.epoch,
		Actor: assignmentActorFrom(f),
		Note:  f.note,
	})
}

func assignmentVerbClaim(store *assignment.Store, f *assignmentFlags) (interface{}, error) {
	return store.Claim(assignment.ClaimParams{
		ID:            f.id,
		ExpectedEpoch: f.epoch,
		Owner:         f.owner,
		LeaseSeconds:  f.leaseSeconds,
		Actor:         assignmentActorFrom(f),
	})
}

func assignmentVerbRenew(store *assignment.Store, f *assignmentFlags) (interface{}, error) {
	return store.Renew(assignment.RenewParams{
		ID:            f.id,
		ExpectedEpoch: f.epoch,
		Owner:         f.owner,
		LeaseSeconds:  f.leaseSeconds,
		Actor:         assignmentActorFrom(f),
	})
}

func assignmentVerbDelegate(store *assignment.Store, f *assignmentFlags) (interface{}, error) {
	payload, err := assignmentParsePayload(f.payload)
	if err != nil {
		return nil, err
	}
	a, _, err := store.Delegate(assignment.DelegateParams{
		ParentID:       f.parentID,
		Title:          f.title,
		Objective:      f.objective,
		Recipient:      f.recipient,
		Role:           f.role,
		IdempotencyKey: f.idempotencyKey,
		Actor:          assignmentActorFrom(f),
		ResourceUnits:  f.resourceUnits,
		IssueRef:       f.issueRef,
		PRRef:          f.prRef,
		BaseSHA:        f.baseSHA,
		HeadSHA:        f.headSHA,
		ReviewedSHA:    f.reviewedSHA,
		FindingsSHA256: f.findingsSHA256,
		EvidenceRef:    f.evidenceRef,
		Payload:        payload,
	})
	if err != nil {
		return nil, err
	}
	return a, nil
}

func assignmentVerbNote(store *assignment.Store, f *assignmentFlags) (interface{}, error) {
	return store.Note(assignment.NoteParams{
		ID:    f.id,
		Actor: assignmentActorFrom(f),
		Note:  f.note,
	})
}

func assignmentVerbRecordReview(store *assignment.Store, f *assignmentFlags) (interface{}, error) {
	return store.RecordReviewResult(assignment.ReviewResultParams{
		ReviewerID: f.id,
		Verdict:    f.verdict,
		Epoch:      f.epoch,
		Actor:      assignmentActorFrom(f),
		Note:       f.note,
	})
}

func assignmentVerbHandoff(store *assignment.Store, f *assignmentFlags) (interface{}, error) {
	return store.HandoffToOperator(assignment.HandoffParams{
		ID:             f.id,
		Epoch:          f.epoch,
		AwaitingAction: f.awaitingAction,
		Actor:          assignmentActorFrom(f),
		Note:           f.note,
	})
}

// assignmentVerbCreateRoot is the OPERATOR's verb.
//
// It is here and not in the registry: the agent-facing surface has no
// action that reaches it, so no dispatch path can reach it either.
func assignmentVerbCreateRoot(store *assignment.Store, f *assignmentFlags) (interface{}, error) {
	payload, err := assignmentParsePayload(f.payload)
	if err != nil {
		return nil, err
	}
	a, _, err := store.CreateRoot(
		assignment.NewLocalOperatorAuthority(assignmentOperatorName(f)),
		assignment.CreateRootParams{
			ProjectID:           f.projectID,
			Title:               f.title,
			Objective:           f.objective,
			Recipient:           f.recipient,
			Role:                f.role,
			IdempotencyKey:      f.idempotencyKey,
			PermittedOperations: f.permitted,
			EligibleRecipients:  f.eligible,
			MaxDepth:            f.maxDepth,
			MaxTasks:            f.maxTasks,
			MaxConcurrency:      f.maxConcurrency,
			MaxResourceUnits:    f.maxResourceUnits,
			ResourceUnits:       f.resourceUnits,
			IssueRef:            f.issueRef,
			PRRef:               f.prRef,
			BaseSHA:             f.baseSHA,
			HeadSHA:             f.headSHA,
			ReviewedSHA:         f.reviewedSHA,
			FindingsSHA256:      f.findingsSHA256,
			EvidenceRef:         f.evidenceRef,
			Payload:             payload,
		})
	if err != nil {
		return nil, err
	}
	return a, nil
}

// assignmentVerbRecordApproval is the OPERATOR's verb.
//
// It RECORDS an approval that the operator obtained elsewhere. It does
// not authenticate it, and it does not perform whatever was approved:
// merging a pull request and deploying stay where they were.
func assignmentVerbRecordApproval(store *assignment.Store, f *assignmentFlags) (interface{}, error) {
	return store.RecordApproval(
		assignment.NewLocalOperatorAuthority(assignmentOperatorName(f)),
		assignment.ApprovalParams{
			ID:       f.id,
			Action:   f.action,
			Revision: f.revision,
			Source:   f.source,
			Evidence: f.evidence,
			Epoch:    f.epoch,
		})
}

// assignmentFenced builds the fenced parameters of a transition.
func assignmentFenced(f *assignmentFlags) assignment.FencedParams {
	return assignment.FencedParams{
		ID:    f.id,
		Epoch: f.epoch,
		Actor: assignmentActorFrom(f),
		Note:  f.note,
	}
}

// assignmentActorFrom reads the attribution from the flags.
//
// The address is RECORDED, never verified. It grants nothing.
func assignmentActorFrom(f *assignmentFlags) assignment.Actor {
	kind := f.actorKind
	if kind == "" {
		kind = "agent"
	}
	return assignment.Actor{Address: f.actor, Kind: kind}
}

// assignmentOperatorName is the attribution of the operator's own verb.
//
// It is the operator's own name when they give one, and the name of the
// account the process runs as otherwise. It is RECORDED — it is not a
// credential, and it is not treated as one.
func assignmentOperatorName(f *assignmentFlags) string {
	if f.actor != "" {
		return f.actor
	}
	if name := os.Getenv("USER"); name != "" {
		return name
	}
	return "operator"
}

// ── the help page ─────────────────────────────────────────────────────────

// printAssignmentHelp renders `mpm assignment --help`.
func printAssignmentHelp() int {
	fmt.Println(`mpm assignment — durable multi-agent assignment coordination

READ
  inbox             list the work addressed to a recipient
  show              show one assignment
  history           show an assignment's ledger
  tree              show a delegation tree

THE WORK
  deliver           hand the work to a runtime
  acknowledge       record that the recipient has taken the work
  accept            commit to the work
  refuse            decline the work
  claim             take the fenced lease
  renew             extend the lease
  start             record that the work is under way
  complete          record that the work is finished
  delegate          create a bounded child inside the root's budgets
  cancel            withdraw the work
  note              append a ledger note
  record-review     record an independent review verdict
  handoff           hand the work to the operator's gate

OPERATOR ONLY
  create-root       create the top-level work order
  record-approval   record an externally verified approval

Common flags:
  --id, --root-id, --parent-id, --project-id, --recipient, --actor
  --epoch, --owner, --lease-seconds, --note, --json

The operator-only verbs are not reachable from ` + "`mpm call`" + ` or from any
MCP tool: they are absent from the tool registry, not hidden behind a
flag. Recording an approval records that the operator authorised
something; it does not merge, deploy, or change anything else.`)
	return 0
}
