// review.go — the spaced-repetition review surface.
//
// WHAT THIS COMMAND IS
// --------------------
// `mpm ops review` is the operator's triage surface for the review
// backlog that `mpm doctor` reports. It has three jobs, and it used to
// do only one of them honestly:
//
//   1. LIST the memories whose review is due.
//   2. RECORD the operator's review of one — the acknowledgment that
//      actually clears the backlog.
//   3. Say the same thing about "due" that the doctor says.
//
// WHAT WAS WRONG
// --------------
//   - The threshold was the CLI's own (30 days) while the doctor used
//     14, so the operator was told to look at a different set than the
//     one the doctor had counted.
//   - --limit was parsed and then IGNORED: the query was called with a
//     hardcoded 20.
//   - A never-accessed memory rendered as "106751d ago" — 292 years —
//     because the NULL last_accessed_at was passed through a formatter
//     that did not understand the *int64 shape.
//   - The reinforcement count was printed as "0x" and the weight was
//     printed as a DELTA from a threshold of 10 that nothing explained.
//   - The doctor said "run this to clear the backlog", and the command
//     only listed. Nothing was acknowledged, so the backlog did not
//     move and the instruction was a lie.
//
// WHAT IT IS NOW
// --------------
// The eligibility rule lives in the substrate (internal/core/review.go)
// and every surface uses it. This file is the presentation layer and the
// acknowledgment action.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/usererror"
)

// reviewOpts is the parsed option set for one review invocation.
type reviewOpts struct {
	stale    bool
	promoted bool
	days     int
	limit    int
	jsonOut  bool
	ack      string
	ackAll   bool
	decision string
	note     string
}

// reviewUsage is the usage line the review command advertises.
const reviewUsage = "mpm ops review [options]"

// reviewProse is the part of the help page that is not an option list.
//
// The combinations are facts about the command's SEMANTICS, so they
// belong here rather than being encoded in the option list — an option
// list that mixes "an option" with "a valid combination of two options"
// is exactly what the doctor's old rejection message did wrong.
const reviewProse = `The default view lists the memories whose review is DUE. The
"promoted" view lists the recently elevated memories instead — a different
question, kept for continuity.

  mpm ops review --stale                 list what is due (window: 14 days)
  mpm ops review --ack <id>              record the review of one memory
  mpm ops review --ack-all               record the review of everything due

A review is recorded durably. It is not a side effect of displaying a
memory: listing never marks anything reviewed, and only --ack / --ack-all
write.`

// reviewOptions declares every option the review command accepts.
func reviewOptions(o *reviewOpts) []OptionSpec {
	return []OptionSpec{
		HelpOption,
		{Name: "stale", Usage: "List the memories whose review is due (the default view)", Kind: OptBool, Bool: &o.stale},
		{Name: "promoted", Usage: "List the recently elevated memories instead", Kind: OptBool, Bool: &o.promoted},
		{Name: "days", Usage: "The review window, in days", Kind: OptInt, Int: &o.days, ValueName: "N", DefaultText: "default 14"},
		{Name: "limit", Usage: "The maximum number of memories to list", Kind: OptInt, Int: &o.limit, ValueName: "N", DefaultText: "default 20"},
		{Name: "ack", Usage: "Record the operator's review of one memory", Kind: OptString, String: &o.ack, ValueName: "ID"},
		{Name: "ack-all", Usage: "Record the operator's review of every memory due now", Kind: OptBool, Bool: &o.ackAll},
		{Name: "decision", Usage: "The decision to record: keep or weaken", Kind: OptString, String: &o.decision, ValueName: "D", DefaultText: "default keep"},
		{Name: "note", Usage: "An optional note recorded with the review", Kind: OptString, String: &o.note, ValueName: "TEXT"},
		{Name: "json", Aliases: []string{"j"}, Usage: "Output JSON for tool integration", Kind: OptBool, Bool: &o.jsonOut},
	}
}

// printReviewHelp renders the review command's own help page.
func printReviewHelp() int {
	o := &reviewOpts{}
	return PrintHelpPage(reviewUsage, reviewOptions(o), reviewProse)
}

// handleReview implements `mpm ops review` and `mpm review`.
//
//	--promoted (default): show recently elevated memories, ordered by
//	last_accessed_at DESC.
//	--stale: show the memories whose review is due.
//	--ack <id> / --ack-all: record the operator's review.
//	--json: output JSON for tool integration.
func handleReview(args []string) int {
	// A help request must never list, and must never acknowledge. The
	// router intercepts the top-level `mpm review --help`, but the
	// nested `mpm ops review --help` reaches this handler with the
	// router's rewritten "help" token — so the handler short-circuits
	// on it too.
	if requireHelpShortCircuit(args, func() { printReviewHelp() }) {
		return 0
	}

	var o reviewOpts
	opts := reviewOptions(&o)

	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	fs.SetOutput(discardWriter{})
	RegisterOptions(fs, opts)

	// The router has already consumed -h/--help.
	if rc := RejectUnknownOptionInArgs("review", reviewUsage, opts, args[1:]); rc != 0 {
		return rc
	}

	if err := fs.Parse(args[1:]); err != nil {
		return usererror.Error("review: %v", err)
	}

	// The window and the limit are honoured, not re-defaulted by the
	// handler. The old code parsed --limit and then called the query
	// with a hardcoded 20.
	if o.days <= 0 {
		o.days = defaultReviewDays()
	}
	if o.limit <= 0 {
		o.limit = 20
	}
	if o.decision == "" {
		o.decision = "keep"
	}
	switch o.decision {
	case "keep", "weaken":
	default:
		return usererror.Error("review: unknown decision %q (expected keep or weaken)", o.decision)
	}

	// The acknowledgment is a separate mode from the listing. It is
	// never implicit: listing a memory does not review it.
	if o.ack != "" && o.ackAll {
		return usererror.Error("review: --ack and --ack-all are exclusive; name one memory or all of them")
	}
	if o.ack != "" || o.ackAll {
		return handleReviewAcknowledge(&o)
	}

	dm := getDB()
	if dm == nil {
		return 1
	}

	// The "promoted" view is a different question from "what is due",
	// and it is kept for continuity. It is NOT the due list, and the
	// doctor never points at it.
	if o.promoted && !o.stale {
		return reviewShowPromoted(dm, &o)
	}

	candidates, err := dm.MemoriesDueForReview(o.days, o.limit)
	if err != nil {
		return usererror.Error("review: %v", err)
	}

	if o.jsonOut {
		return reviewJSONDue(&o, candidates)
	}

	if len(candidates) == 0 {
		fmt.Printf("No memories are due for review (window: %d days).\n", o.days)
		return 0
	}

	fmt.Printf("Spaced review — %d memor%s due (window: %d days)\n",
		len(candidates), pluralY(len(candidates)), o.days)
	fmt.Println("A memory is due when it is long-term or its weight is 5 or more, and it has not been")
	fmt.Println("reviewed, accessed, or — failing both — created within the window.")
	fmt.Println()

	for i, c := range candidates {
		printReviewCandidate(i+1, c)
	}

	fmt.Printf("%d memories due for review. Record each review with:\n", len(candidates))
	fmt.Println("  mpm ops review --ack <id>            one memory")
	fmt.Println("  mpm ops review --ack-all             every memory due now")
	return 0
}

// pluralY is the pluralising suffix for a count.
func pluralY(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// printReviewCandidate renders one review candidate.
//
// The three facts the old rendering got wrong are the three this one is
// careful about:
//
//   - a NULL last_accessed_at says "never accessed", and the row says
//     so — it does not invent an age;
//   - the weight is the STORED weight, not a delta from an unexplained
//     threshold;
//   - the reinforcement count is labelled as a count, not as "0x".
func printReviewCandidate(n int, c mpminternal.ReviewCandidate) {
	fmt.Printf("%d. [%s]  weight %d  reinforcement count %d\n",
		n, shortID(c.ID), c.Weight, c.ReinforcementCount)
	fmt.Printf("   %s\n", reviewAgeLine(c))
	snippet := stripMarkdown(c.Content)
	if len(snippet) > 120 {
		snippet = snippet[:120] + "..."
	}
	fmt.Printf("   %s\n", snippet)
	fmt.Printf("   → review: mpm ops review --ack %s\n", c.ID)
	fmt.Println()
}

// reviewAgeLine describes WHEN the memory was last touched, honestly.
//
// The three cases are distinct facts and are rendered distinctly:
//
//   - reviewed:    an explicit operator review — the strongest signal;
//   - accessed:    the last retrieval — a proxy, not a review;
//   - never:       neither, so the creation date anchors the window.
func reviewAgeLine(c mpminternal.ReviewCandidate) string {
	var when string
	switch {
	case c.ReviewedAt != nil:
		when = fmt.Sprintf("last reviewed %s", humanDaysAgo(*c.ReviewedAt))
	case c.LastAccessedAt != nil:
		when = fmt.Sprintf("last accessed %s", humanDaysAgo(*c.LastAccessedAt))
	default:
		when = fmt.Sprintf("never accessed (created %s)", humanDaysAgo(c.CreatedAt))
	}
	return fmt.Sprintf("%s — %d days past the window", when, c.DaysOverdue)
}

// humanDaysAgo renders a Unix-epoch timestamp as an age in days.
//
// A timestamp of 0 or less has no meaningful age, and rendering it as
// "106751d ago" is exactly the defect this replaces — 292 years is the
// distance from year 1 to now, and it is what every NULL timestamp used
// to display as.
func humanDaysAgo(ts int64) string {
	if ts <= 0 {
		return "unknown"
	}
	d := int(time.Since(time.Unix(ts, 0)).Hours() / 24)
	if d < 0 {
		d = 0
	}
	if d == 0 {
		return "today"
	}
	return fmt.Sprintf("%dd ago", d)
}

// reviewShowPromoted renders the "recently elevated" view.
//
// This is a DIFFERENT question from "what is due" — it asks which
// memories have been reinforced or carry weight, most recently accessed
// first — and it keeps its OWN query. It is never the answer to the
// doctor's backlog, so the page says so.
func reviewShowPromoted(dm mpminternal.CoreDB, o *reviewOpts) int {
	rows, err := dm.SQLDB().Query(`
		SELECT id, collection, content,
		       COALESCE(reinforcement_count, 0) as reinforcement_count,
		       COALESCE(weight, 1) as weight,
		       last_accessed_at,
		       created_at
		FROM memories
		WHERE deleted_at IS NULL
		  AND last_accessed_at IS NOT NULL
		  AND (reinforcement_count > 0 OR weight > 1)
		ORDER BY last_accessed_at DESC
		LIMIT ?
	`, o.limit)
	if err != nil {
		return usererror.Error("review: %v", err)
	}
	defer rows.Close()

	type promoted struct {
		id         string
		collection string
		content    string
		weight     int
		reinforce  int
		lastSeen   *int64
	}
	var items []promoted
	for rows.Next() {
		var p promoted
		var collection, content string
		var reinforcement int64
		var weight float64
		var lastAccess int64
		var createdAt int64
		if err := rows.Scan(&p.id, &collection, &content, &reinforcement, &weight,
			&lastAccess, &createdAt); err != nil {
			return usererror.Error("review: %v", err)
		}
		p.collection = collection
		p.content = content
		p.reinforce = int(reinforcement)
		p.weight = int(weight)
		la := lastAccess
		p.lastSeen = &la
		items = append(items, p)
	}
	if err := rows.Err(); err != nil {
		return usererror.Error("review: %v", err)
	}

	if len(items) == 0 {
		fmt.Println("No promoted memories found.")
		return 0
	}
	if o.jsonOut {
		records := make([]map[string]interface{}, 0, len(items))
		for _, p := range items {
			records = append(records, map[string]interface{}{
				"id":                  p.id,
				"collection":          p.collection,
				"content":             p.content,
				"weight":              p.weight,
				"reinforcement_count": p.reinforce,
				"last_accessed_at":    *p.lastSeen,
			})
		}
		data, _ := json.Marshal(map[string]interface{}{
			"success":  true,
			"mode":     "promoted",
			"days":     o.days,
			"count":    len(records),
			"memories": records,
		})
		fmt.Println(string(data))
		return 0
	}
	fmt.Printf("Spaced reinforcement review — %d recently elevated memor%s\n",
		len(items), pluralY(len(items)))
	fmt.Println("This view is NOT the review backlog; `mpm ops review --stale` lists what is due.")
	fmt.Println()
	for i, p := range items {
		snippet := stripMarkdown(p.content)
		if len(snippet) > 120 {
			snippet = snippet[:120] + "..."
		}
		fmt.Printf("%d. [%s]  weight %d  reinforcement count %d\n", i+1, shortID(p.id), p.weight, p.reinforce)
		fmt.Printf("   last accessed %s\n", humanDaysAgo(*p.lastSeen))
		fmt.Printf("   %s\n", snippet)
		fmt.Println()
	}
	return 0
}

// reviewJSONDue renders the due list as JSON.
//
// The envelope keeps the shape the command already had — mode, days,
// count, memories — so a consumer of the JSON does not break. The
// per-memory records gain the honest fields: last_accessed_at is NULL
// for a memory that was never accessed, rather than a fabricated age.
func reviewJSONDue(o *reviewOpts, candidates []mpminternal.ReviewCandidate) int {
	items := make([]map[string]interface{}, 0, len(candidates))
	for _, c := range candidates {
		item := map[string]interface{}{
			"id":                  c.ID,
			"collection":          c.Collection,
			"content":             c.Content,
			"weight":              c.Weight,
			"reinforcement_count": c.ReinforcementCount,
			"created_at":          c.CreatedAt,
			"review_clock":        c.ReviewClock,
			"days_overdue":        c.DaysOverdue,
		}
		if c.LastAccessedAt == nil {
			item["last_accessed_at"] = nil
		} else {
			item["last_accessed_at"] = *c.LastAccessedAt
		}
		if c.ReviewedAt == nil {
			item["reviewed_at"] = nil
		} else {
			item["reviewed_at"] = *c.ReviewedAt
		}
		items = append(items, item)
	}
	data, err := json.Marshal(map[string]interface{}{
		"success":  true,
		"mode":     "stale",
		"days":     o.days,
		"count":    len(items),
		"memories": items,
	})
	if err != nil {
		return usererror.Error("review: %v", err)
	}
	fmt.Println(string(data))
	return 0
}

// handleReviewAcknowledge records the operator's review.
//
// This is the action that makes the doctor's instruction TRUE. It:
//
//   - records the review durably (the memory_reviews ledger), so the
//     memory leaves the backlog;
//   - applies a "weaken" decision through the EXISTING WeakenMemory
//     path, so the weight change goes through the separately-audited
//     code rather than a second implementation;
//   - never touches the weight, the reinforcement count or the content
//     on its own.
//
// Merely LISTING a memory does not reach this function: it is reachable
// only through an explicit --ack or --ack-all.
func handleReviewAcknowledge(o *reviewOpts) int {
	dm := getDB()
	if dm == nil {
		return 1
	}

	var targets []string
	if o.ackAll {
		candidates, err := dm.MemoriesDueForReview(o.days, 0)
		if err != nil {
			return usererror.Error("review: %v", err)
		}
		for _, c := range candidates {
			targets = append(targets, c.ID)
		}
		if len(targets) == 0 {
			if o.jsonOut {
				fmt.Println(`{"success":true,"reviewed":[],"count":0}`)
				return 0
			}
			fmt.Printf("No memories are due for review (window: %d days); nothing to review.\n", o.days)
			return 0
		}
	} else {
		targets = []string{o.ack}
	}

	reviewed := make([]map[string]interface{}, 0, len(targets))
	for _, id := range targets {
		if err := dm.RecordMemoryReview(id, o.decision, o.note); err != nil {
			return usererror.Error("review: %v", err)
		}
		// A "weaken" decision is an explicit instruction. It goes
		// through the existing WeakenMemory path, so the weight change
		// is the same audited operation `mpm weaken` performs.
		if o.decision == "weaken" {
			if err := dm.WeakenMemory(id, 1); err != nil {
				return usererror.Error("review: recorded the review of %s but could not weaken it: %v", id, err)
			}
		}
		reviewed = append(reviewed, map[string]interface{}{
			"id":       id,
			"decision": o.decision,
		})
	}

	if o.jsonOut {
		data, _ := json.Marshal(map[string]interface{}{
			"success":  true,
			"reviewed": reviewed,
			"count":    len(reviewed),
			"decision": o.decision,
		})
		fmt.Println(string(data))
		return 0
	}

	if len(reviewed) == 1 {
		fmt.Printf("✓ Reviewed %s (%s)\n", reviewed[0]["id"], o.decision)
	} else {
		fmt.Printf("✓ Reviewed %d memories (%s)\n", len(reviewed), o.decision)
	}
	return 0
}

// defaultReviewDays is the review window the command uses when none is
// given.
//
// It is the substrate's canonical window, the same one the doctor uses.
// The command used to default to 30 while the doctor used 14, which is
// how the doctor's instruction came to name a command that showed a
// different set than the one the doctor had counted.
func defaultReviewDays() int {
	return mpminternal.ReviewDueAfterDays
}

// stripMarkdown removes the markdown emphasis a memory's content may
// carry, so the review list shows the text rather than its formatting.
//
// It lives in recall.go and is shared: the review surface and the
// recall surface must render the same content the same way.

// shortID returns a shortened memory ID (first 8 chars)
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// toInt safely extracts an int from interface{} handling both int and int64.
func toInt(v interface{}) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return 0
	}
}

// toTime safely extracts a time.Time from interface{}.
//
// Handles:
//   - time.Time
//   - string (RFC3339)
//   - int / int64 / float64 (Unix-epoch seconds — the canonical
//     INTEGER timestamp shape after the 2026-08 timestamps_unified_v1
//     migration; see internal/core/memory.go)
//   - *int64 (the NULLABLE shape)
//
// D-009 (alpha-4.1.1): the int64 branch is load-bearing. The pre-fix
// code only handled time.Time and string — any integer timestamp fell
// through to the zero-time default, which `time.Since` rendered as
// "106751d ago" (≈292 years, the time from year 1 to now) for every
// newly-created memory in the spaced review report. The fix routes the
// integer shape through time.Unix so the relative-age display reads
// sensibly.
//
// THE *int64 BRANCH (2026-10-30): the same defect survived the first
// fix, because the shape that actually reaches the review renderer is
// the POINTER. GetSpacedReinforcementReview returns
// `last_accessed_at` as a *int64 — nil for a memory that was never
// accessed — and a *int64 matched none of the branches above, so it
// fell through to the zero-time default and rendered as "106751d ago"
// again. A nil pointer now yields the zero time (which the renderers
// treat as "never"), and a non-nil one yields the real instant.
//
// Future timestamp shapes (RFC3339Nano, fractional seconds) are out of
// scope — the substrate standardises on INTEGER Unix-epoch seconds.
func toTime(v interface{}) time.Time {
	switch t := v.(type) {
	case time.Time:
		return t
	case string:
		parsed, _ := time.Parse(time.RFC3339, t)
		return parsed
	case int:
		if t <= 0 {
			return time.Time{}
		}
		return time.Unix(int64(t), 0)
	case int64:
		if t <= 0 {
			return time.Time{}
		}
		return time.Unix(t,  0)
	case float64:
		if t <= 0 {
			return time.Time{}
		}
		return time.Unix(int64(t), 0)
	case *int64:
		if t == nil || *t <= 0 {
			return time.Time{}
		}
		return time.Unix(*t, 0)
	default:
		return time.Time{}
	}
}
