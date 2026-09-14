// lifecycle_help.go — discoverable lifecycle asymmetry notes.
//
// Operators reasonably expect destructive verbs (delete, shred) on
// every artifact family. Several families intentionally omit them
// to preserve audit history. This handler renders those asymmetries
// as concise human-facing explanations so users do not have to read
// docs/final-pass-lifecycle-matrix.md to learn why an expected verb
// does not exist.
//
// Usage:
//   mpm lifecycle            — all families
//   mpm lifecycle theory     — single family
//   mpm lifecycle decision
//   mpm lifecycle evidence
//   mpm lifecycle wake
//   mpm lifecycle skill
//   mpm lifecycle handoff
//
// Routes through the shared render package — heading, hint, label.

package main

import (
	"fmt"
	"os"

	"github.com/flowbyte-com/mpm-core/usererror"

	"github.com/flowbyte-com/mpm/cmd/mpm/render"
)

// lifecycleNote is one asymmetry description.
type lifecycleNote struct {
	Heading string
	Body    string
	Hint    string
}

// lifecycleNotesByFamily maps each artifact family to its notes.
var lifecycleNotesByFamily = map[string][]lifecycleNote{
	"theory": {
		{
			Heading: "Terminal: resolve or invalidate",
			Body:    "A theory is finalized via mpm theories resolve <id> (proven / disproven) or mpm theories invalidate <id>. Once resolved, the validation history is preserved.",
			Hint:    "Use mpm theories status <id> to view current state.",
		},
		{
			Heading: "No delete / shred",
			Body:    "The validation_criteria + history chain is the audit trail. Removing the row would erase the evidence downstream decisions cite.",
		},
	},
	"decision": {
		{
			Heading: "Terminal: supersede or invalidate",
			Body:    "A decision is updated by recording a new one (mpm decide supersedes <old-id>) or by marking the old one invalid (mpm decisions invalidate <id>). The supersede-by link is preserved.",
			Hint:    "Use mpm decisions show <id> to see supersede chain.",
		},
		{
			Heading: "No delete / shred",
			Body:    "Decisions are audit artifacts. Reversing a decision means recording an invalidation, not erasing the record.",
		},
	},
	"evidence": {
		{
			Heading: "Append-only",
			Body:    "Evidence rows are never modified or deleted. To correct a wrong observation, attach a counter-evidence row with negative strength.",
			Hint:    "Use mpm evidence add --counter-to <id> for explicit counter-evidence.",
		},
		{
			Heading: "Expiry is automatic",
			Body:    "Rows with expires_at < now are filtered out of queries by ListEvidence. The row itself remains for audit.",
		},
	},
	"wake": {
		{
			Heading: "Substrate primitive, not first-class human object",
			Body:    "Raw wakes are scheduler-internal signals. The human-facing abstraction is scheduled tasks (mpm wake upsert_task / list_tasks).",
			Hint:    "Use mpm wake upsert-task <name> --cron <expr> for recurring work.",
		},
		{
			Heading: "Snooze",
			Body:    "mpm wake snooze <cluster> <duration> defers a wake backlog cluster. Snoozes expire automatically.",
		},
	},
	"skill": {
		{
			Heading: "Versioned, not deleted",
			Body:    "Skills are identified by skill:<name>-v<version>. Deprecation means publishing a new version; the old version remains for canonical references.",
			Hint:    "Use mpm skill save with the same name + bumped version.",
		},
		{
			Heading: "Shred",
			Body:    "mpm skill shred removes the skill entirely. Use sparingly — references in handoffs and directives become orphans.",
		},
	},
	"handoff": {
		{
			Heading: "Write / read / list / shred",
			Body:    "Handoffs persist across sessions until read. Use mpm handoff write to record, mpm handoff read to retrieve, mpm handoff list to enumerate, mpm handoff shred to hard-delete.",
		},
		{
			Heading: "No update",
			Body:    "Handoffs are immutable once written. To revise, write a new handoff and shred the old one.",
		},
	},
}

// handleLifecycleHelp prints the lifecycle asymmetry notes for one
// or all families.
func handleLifecycleHelp(args []string) int {
	if len(args) == 0 {
		// All families, sentence-case label order.
		for _, family := range []string{"theory", "decision", "evidence", "wake", "skill", "handoff"} {
			renderFamilyNotes(family)
			fmt.Println()
		}
		return 0
	}
	family := args[0]
	if _, ok := lifecycleNotesByFamily[family]; !ok {
		usererror.Error("lifecycle: no notes for family %q. Known: theory, decision, evidence, wake, skill, handoff.", family)
		return 1
	}
	renderFamilyNotes(family)
	return 0
}

// renderFamilyNotes prints the heading + notes for one family through
// the shared renderer.
func renderFamilyNotes(family string) {
	render.Heading(os.Stdout, "Lifecycle · "+family)
	notes := lifecycleNotesByFamily[family]
	for _, n := range notes {
		fmt.Println()
		render.Section(os.Stdout, n.Heading)
		render.Plain(os.Stdout, "  "+n.Body)
		if n.Hint != "" {
			render.Hint(os.Stdout, n.Hint)
		}
	}
}
