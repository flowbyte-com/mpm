// retention.go — the registry-membership model for directive retention.
//
// THE POLICY (issue #59, Option B)
// --------------------------------
// An active registry-backed bootstrap directive is STANDING SYSTEM POLICY,
// not an ordinary memory. Its retention follows REGISTRY MEMBERSHIP rather
// than age, access frequency or embedding status.
//
// Three consequences, and the third is the one that is easy to get wrong:
//
//  1. An active registry directive is protected from generic pruning.
//     Every automatic reclamation path in the substrate excludes directive
//     rows (see internal.DirectiveExclusionClause). This is not a
//     special case bolted onto one prune — it is the policy, applied
//     uniformly.
//
//  2. A never-accessed directive never becomes an automatic deletion
//     candidate. The directive read path does not write `last_accessed_at`,
//     so every directive is permanently "never accessed"; an access-based
//     rule would select all of them, forever.
//
//  3. A directive REMOVED FROM THE REGISTRY becomes ELIGIBLE for explicit,
//     auditable cleanup — and ELIGIBLE IS NOT THE SAME AS AUTHORISED.
//
// THE THIRD POINT, STATED PRECISELY
// ---------------------------------
// "Eligible" means: the substrate can tell you that this row is a former
// standing policy that is no longer in the registry, and you may act on
// that information.
//
// "Authorised" means: something actually deletes it.
//
// MPM never conflates the two. There is NO automatic cleanup mechanism for
// directives, and this file deliberately does not add one. A directive that
// has left the registry is not reclaimed by age, by access, by expiry, by
// consolidation or by any future sweep; it stays until an operator acts.
//
// The operator's explicit routes already exist and are unaffected by
// registry membership: `mpm call mpm_memory action=delete` (reversible) and
// the operator-only shred (irreversible). This file's job is to make the
// CANDIDATES visible, so the operator can choose.
//
// WHY A ROW CAN BE AN ORPHAN AT ALL
// ---------------------------------
// Removing an entry from `SeedDirectives` is a CODE change, not a runtime
// event, so there is no moment at which the substrate can observe "this
// directive was just de-registered". By the time the new build runs, the
// row is simply there and the registry no longer mentions it.
//
// The row still carries the marker the canonical seeder wrote —
// `metadata.provenance.source = 'baseline_cognitive_bootstrap'` — so the
// substrate CAN tell a former baseline row from an operator-authored one.
//
// WHY THE MARKER MATTERS
// ----------------------
// A user-authored directive (written through `mpm_memory` with
// `collection='directives'`) is ALSO a directive, and it is also policy the
// operator wants. It must never be reported as an orphan and never be
// eligible for cleanup, because registry membership says nothing about it:
// it was never in the registry.
//
// Only rows the canonical seeder wrote carry the marker. That is the
// difference, and it is the whole difference.
//
// WHAT IS DELIBERATELY NOT HERE
// ------------------------------
//   - No automatic deletion of orphans. Eligibility is reported; the
//     decision is the operator's.
//   - No `--prune` flag on `mpm ops init directives`. The flag is
//     mentioned in that command's source as a possible future; adding it
//     here would be introducing an automatic cleanup mechanism to close a
//     policy issue, which the issue explicitly forbids.
//   - No judgement about whether a de-registered row was CUSTOMISED. After
//     removal there is no registry text left to compare against, so
//     "pristine" and "customised" are indistinguishable. The operator can
//     only tell by reading the row, so the reading is available —
//     EXPLICITLY, through FormatOrphanedDirectiveWithContent. The default
//     report does not print the content, because a report is a log and the
//     policy text should not land in one the operator did not ask for.

package seed

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// DirectiveProvenanceSource is the provenance marker the canonical seeder
// writes into a seeded row's metadata.
//
// It is the ONLY reliable way to tell a row the baseline seeder wrote from
// a row the operator wrote. Both are directives; only one of them was ever
// in the registry, and only that one can become an orphan.
//
// The value is used in three places — the seeder that writes it, the
// reader that looks for it, and the SPEC — so it cannot drift.
const DirectiveProvenanceSource = "baseline_cognitive_bootstrap"

// RegistryDirectiveIDs returns the stable IDs currently in the registry.
//
// This is REGISTRY MEMBERSHIP — the authority for "is this row still
// standing policy?". It is deliberately a function of the SeedDirectives
// slice and nothing else: no database, no environment, no clock.
func RegistryDirectiveIDs() []string {
	ids := make([]string, 0, len(SeedDirectives))
	for _, sd := range SeedDirectives {
		ids = append(ids, sd.StableID)
	}
	sort.Strings(ids)
	return ids
}

// IsRegistryDirectiveID reports whether a stable ID is a current registry
// member.
//
// It reads the SeedDirectives slice LIVE rather than a snapshot taken at
// init. The slice is the single source of truth for registry membership,
// and a snapshot would be a second copy of it that could disagree —
// which is exactly the drift the retention policy cannot tolerate. The
// slice has a handful of entries, so the linear scan is free.
func IsRegistryDirectiveID(id string) bool {
	for _, sd := range SeedDirectives {
		if sd.StableID == id {
			return true
		}
	}
	return false
}

// IsRegistryBackedDirective reports whether a row's metadata marks it as a
// row the canonical baseline seeder wrote.
//
// The check is on the PROVENANCE MARKER, not on the collection and not on
// the stable ID:
//   - `collection='directives'` alone does not distinguish a seeded row
//     from an operator-authored one;
//   - the stable-ID prefix is a convention, not a contract.
//
// A row that carries the marker was written by the seeder, so it was in
// the registry at some point. Whether it is STILL in the registry is a
// separate question, answered by IsRegistryDirectiveID.
func IsRegistryBackedDirective(metadata map[string]interface{}) bool {
	if metadata == nil {
		return false
	}
	prov, ok := metadata["provenance"].(map[string]interface{})
	if !ok {
		return false
	}
	source, _ := prov["source"].(string)
	return source == DirectiveProvenanceSource
}

// OrphanedDirective is a registry-backed directive row whose stable ID is
// no longer in the registry.
//
// It is a CANDIDATE for explicit cleanup. It is not a row MPM will delete,
// and nothing about it is urgent: it is inert policy text that no agent
// reads as standing policy any more.
type OrphanedDirective struct {
	// ID is the row's stable ID — the registry entry that used to own it.
	ID string

	// Content is the row's content, so the operator can SEE what would be
	// removed before deciding. It is the operator's own policy text (or the
	// baseline's, if they never edited it).
	Content string

	// CreatedAt is the row's creation timestamp, in Unix-epoch seconds.
	CreatedAt int64

	// Tags is the row's tag array, as stored.
	Tags []string

	// ReferencedBy names the seeded scheduled tasks whose DirectiveID
	// points at this directive.
	//
	// The registry has internal cross-references: the baseline nightly
	// compaction task names mpm-seed-epistemic-compaction-policy as the
	// directive the agent reads when the wake fires. De-registering a
	// directive that a seeded task still points at does NOT break the
	// task on an existing database (the row is still there) — but on a
	// FRESH install the task seed would find no directive and skip the
	// task entirely.
	//
	// That is a consequence of a registry change that the operator
	// should see before they act on an orphan, so the report carries it.
	ReferencedBy []string
}

// registryReferencesTo returns the seeded scheduled tasks that name the
// given directive ID.
func registryReferencesTo(directiveID string) []string {
	var refs []string
	for _, st := range SeedScheduledTasks {
		if st.DirectiveID == directiveID {
			refs = append(refs, st.StableID)
		}
	}
	return refs
}

// OrphanedDirectives returns the registry-backed directive rows that are no
// longer in the registry.
//
// READ-ONLY. It changes nothing, deletes nothing, and is safe to call
// repeatedly — calling it twice returns the same answer.
//
// The query is narrow on purpose. It selects rows that are BOTH directives
// (by the canonical two-armed predicate) AND registry-backed (by the
// provenance marker), and the membership test happens in Go against the
// registry. A user-authored directive is not registry-backed, so it is never
// in the result set — which is what makes "registry membership changed" safe
// for operator content.
//
// Soft-deleted rows are excluded: they are already out of the read path,
// so they are not standing policy any more and there is nothing for the
// operator to decide. MPM does not hard-delete them either — see the
// package doc.
func OrphanedDirectives(dm interface {
	SQLDB() *sql.DB
}) ([]OrphanedDirective, error) {
	db := dm.SQLDB()
	if db == nil {
		return nil, fmt.Errorf("seed: no database")
	}
	rows, err := db.Query(`
		SELECT id, content, COALESCE(created_at, 0), COALESCE(tags, '[]')
		FROM memories
		WHERE deleted_at IS NULL
		  AND (collection = 'directives' OR COALESCE(is_prime_directive, 0) = 1)
		  AND json_extract(metadata, '$.provenance.source') = ?
	`, DirectiveProvenanceSource)
	if err != nil {
		return nil, fmt.Errorf("seed: query orphaned directives: %w", err)
	}
	defer rows.Close()

	var orphans []OrphanedDirective
	for rows.Next() {
		var id, content, tagsJSON string
		var createdAt int64
		if err := rows.Scan(&id, &content, &createdAt, &tagsJSON); err != nil {
			return nil, fmt.Errorf("seed: scan orphaned directive: %w", err)
		}
		if IsRegistryDirectiveID(id) {
			continue
		}
		var tags []string
		if tagsJSON != "" {
			_ = json.Unmarshal([]byte(tagsJSON), &tags)
		}
		orphans = append(orphans, OrphanedDirective{
			ID:           id,
			Content:      content,
			CreatedAt:    createdAt,
			Tags:         tags,
			ReferencedBy: registryReferencesTo(id),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("seed: iterate orphaned directives: %w", err)
	}
	sort.Slice(orphans, func(i, j int) bool { return orphans[i].ID < orphans[j].ID })
	return orphans, nil
}

// FormatOrphanedDirective renders one orphan for the operator report.
//
// IT DOES NOT SHOW THE CONTENT.
//
// A cleanup report is a LOG. It is printed to stdout, so it lands in a
// terminal scrollback, in a CI log, and in whatever the operator pipes
// it into. The default report therefore carries only what an operator
// needs to DECIDE THAT THEY WANT TO LOOK — the row's stable ID, its age,
// and any seeded task that still points at it — and not the directive
// text itself.
//
// The content is available, explicitly, through
// FormatOrphanedDirectiveWithContent. Inspecting a candidate is a
// deliberate act by the operator, not a side effect of running a
// maintenance command.
//
// WHY THE CONTENT IS NOT IN THE DEFAULT LINE
// ------------------------------------------
// The operator cannot be told "this row is eligible for cleanup" and be
// left to work out whether it is their own customised policy. They have
// to read it — but reading it has to be a choice, because the read is
// what puts the policy text into a log the operator did not ask for.
func FormatOrphanedDirective(o OrphanedDirective) string {
	line := o.ID
	if o.CreatedAt > 0 {
		line += fmt.Sprintf(" (seeded %s)", time.Unix(o.CreatedAt, 0).UTC().Format("2006-01-02"))
	}
	if len(o.ReferencedBy) > 0 {
		line += fmt.Sprintf(" [still referenced by: %s]", strings.Join(o.ReferencedBy, ", "))
	}
	return line
}

// FormatOrphanedDirectiveWithContent renders one orphan INCLUDING a
// preview of its content.
//
// This is the EXPLICIT INSPECTION path: the operator asked to see the
// text, so the text is shown. The preview is still truncated, because a
// report line should carry a preview rather than a whole document — but
// the truncation is a courtesy, not the privacy control. The privacy
// control is that this function is not the default.
//
// It exists for a second reason, and the reason is why the default
// report points at it. A de-registered directive cannot be
// distinguished from a CUSTOMISED one: removing an entry from the
// registry is a code change, so there is no registry text left to
// compare the row against, and "pristine" and "customised" are
// unknowable after the fact. The operator can only tell by reading. So
// the reading must be available — and the fact that it is available is
// what makes "eligible" safe to report at all.
func FormatOrphanedDirectiveWithContent(o OrphanedDirective) string {
	preview := strings.TrimSpace(o.Content)
	if len(preview) > 96 {
		preview = preview[:96] + "…"
	}
	line := fmt.Sprintf("%s — %s", o.ID, preview)
	if len(o.ReferencedBy) > 0 {
		line += fmt.Sprintf(" [still referenced by: %s]", strings.Join(o.ReferencedBy, ", "))
	}
	return line
}
