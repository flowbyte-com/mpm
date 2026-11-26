package internal

// Restore historical semantic and temporal validation (contract §4.6/B3,
// C1/C2, legacy shapes, T92–T93, T105–T109, T121, T123–T129).
//
// The validator concludes only authorisation/admission ordering from
// durable lineage and claim records. It never infers provider invocation
// time, never rewrites timestamps, attempts, lineage or evidence rows,
// and fails closed with a precise diagnostic wherever ordering is
// missing, equal where strictness is required, or internally
// inconsistent.

import (
	"fmt"
	"strconv"
	"strings"
)

// restoreTask is the replay-relevant projection of an agent_tasks row.
type restoreTask struct {
	id       string
	rootID   string
	parentID string
	state    string
	role     string
	epoch    int64
}

// restoreAttempt is the replay-relevant projection of a session row.
type restoreAttempt struct {
	id        string
	taskID    string
	epoch     int64
	state     string
	outcome   string
	createdAt *int64
	updatedAt *int64
}

// restoreLineage is one lineage row with its authorisation time.
type restoreLineage struct {
	id          string
	rootID      string
	superseded  string
	replacement string
	reviewer    string
	attemptID   string
	createdAt   *int64
}

// restoreClaim is one claimed event (task, epoch) -> created_at.
type restoreClaim struct {
	taskID    string
	epoch     int64
	createdAt *int64
}

// restoreGraph is the validated model of one dump's managed rows.
type restoreGraph struct {
	tasks     map[string]*restoreTask
	attempts  map[string]*restoreAttempt
	byTask    map[string][]*restoreAttempt
	lineages  []*restoreLineage
	claims    map[string]map[int64]*restoreClaim // task -> epoch -> claim
	roots     map[string]bool
	ambiguous map[string][]*restoreAttempt // tree root -> ambiguous attempts
	named     map[string]bool              // attempt id -> named by a lineage
}

// buildRestoreGraph indexes parsed managed rows. Malformed rows
// (missing id references, bad integers) fail closed here.
func buildRestoreGraph(m *restoreModel) (*restoreGraph, error) {
	g := &restoreGraph{
		tasks:     map[string]*restoreTask{},
		attempts:  map[string]*restoreAttempt{},
		byTask:    map[string][]*restoreAttempt{},
		claims:    map[string]map[int64]*restoreClaim{},
		roots:     map[string]bool{},
		ambiguous: map[string][]*restoreAttempt{},
		named:     map[string]bool{},
	}
	get := func(rm map[string]restoreValue, col string) (string, bool) {
		v, ok := rm[strings.ToLower(col)]
		if !ok || v.isNull() {
			return "", false
		}
		return v.text, true
	}
	getInt := func(rm map[string]restoreValue, col string) (int64, bool) {
		s, ok := get(rm, col)
		if !ok {
			return 0, false
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	for _, r := range m.managed["agent_tasks"] {
		rm := r.rowMap()
		id, ok := get(rm, "id")
		if !ok || id == "" {
			return nil, fmt.Errorf("restore: agent_tasks row without an id")
		}
		t := &restoreTask{id: id}
		if v, ok := get(rm, "root_id"); ok {
			t.rootID = v
		}
		if v, ok := get(rm, "parent_id"); ok {
			t.parentID = v
		}
		if v, ok := get(rm, "state"); ok {
			t.state = v
		}
		if v, ok := get(rm, "role"); ok {
			t.role = v
		}
		if n, ok := getInt(rm, "claim_epoch"); ok {
			t.epoch = n
		}
		if _, dup := g.tasks[id]; dup {
			return nil, fmt.Errorf("restore: duplicate agent_tasks id %q", id)
		}
		g.tasks[id] = t
		if t.parentID == "" {
			g.roots[id] = true
		}
	}
	for _, r := range m.managed["agent_task_sessions"] {
		rm := r.rowMap()
		id, ok := get(rm, "id")
		if !ok || id == "" {
			return nil, fmt.Errorf("restore: agent_task_sessions row without an id")
		}
		taskID, ok := get(rm, "task_id")
		if !ok || taskID == "" {
			return nil, fmt.Errorf("restore: attempt %q without a task", id)
		}
		a := &restoreAttempt{id: id, taskID: taskID}
		if n, ok := getInt(rm, "claim_epoch"); ok {
			a.epoch = n
		}
		if v, ok := get(rm, "launch_state"); ok {
			a.state = v
		}
		if v, ok := get(rm, "runtime_outcome"); ok {
			a.outcome = v
		}
		if n, ok := getInt(rm, "created_at"); ok {
			n := n
			a.createdAt = &n
		}
		if n, ok := getInt(rm, "updated_at"); ok {
			n := n
			a.updatedAt = &n
		}
		if _, dup := g.attempts[id]; dup {
			return nil, fmt.Errorf("restore: duplicate attempt id %q", id)
		}
		g.attempts[id] = a
		g.byTask[taskID] = append(g.byTask[taskID], a)
		if a.state == "ambiguous" {
			if t, ok := g.tasks[taskID]; ok {
				g.ambiguous[t.rootID] = append(g.ambiguous[t.rootID], a)
			}
		}
	}
	for _, r := range m.managed["agent_task_supersessions"] {
		rm := r.rowMap()
		id, _ := get(rm, "id")
		l := &restoreLineage{id: id}
		if v, ok := get(rm, "root_id"); ok {
			l.rootID = v
		}
		if v, ok := get(rm, "superseded_task_id"); ok {
			l.superseded = v
		}
		if v, ok := get(rm, "replacement_task_id"); ok {
			l.replacement = v
		}
		if v, ok := get(rm, "reviewer_task_id"); ok {
			l.reviewer = v
		}
		if v, ok := get(rm, "attempt_id"); ok {
			l.attemptID = v
			g.named[v] = true
		}
		if n, ok := getInt(rm, "created_at"); ok {
			n := n
			l.createdAt = &n
		}
		g.lineages = append(g.lineages, l)
	}
	for _, r := range m.managed["agent_task_events"] {
		rm := r.rowMap()
		typ, _ := get(rm, "event_type")
		if typ != "claimed" {
			continue
		}
		taskID, ok := get(rm, "task_id")
		if !ok || taskID == "" {
			continue
		}
		var epoch int64
		if n, ok := getInt(rm, "claim_epoch"); ok {
			epoch = n
		} else {
			continue
		}
		var created *int64
		if n, ok := getInt(rm, "created_at"); ok {
			n := n
			created = &n
		}
		if g.claims[taskID] == nil {
			g.claims[taskID] = map[int64]*restoreClaim{}
		}
		g.claims[taskID][epoch] = &restoreClaim{taskID: taskID, epoch: epoch, createdAt: created}
	}
	return g, nil
}

// validateRestoreHistory runs the pre-destructive-window semantic and
// temporal checks. It returns the first refusal.
func validateRestoreHistory(m *restoreModel, class restoreClass) error {
	g, err := buildRestoreGraph(m)
	if err != nil {
		return err
	}
	if err := checkRestoreReferences(g); err != nil {
		return err
	}
	if err := checkReservedKeys(m, g); err != nil {
		return err
	}
	if err := checkLegacyAttemptShapes(g); err != nil {
		return err
	}
	if err := checkTemporalBounds(g); err != nil {
		return err
	}
	return nil
}

// checkRestoreReferences enforces T92: every managed reference resolves
// and no delegation cycle exists.
func checkRestoreReferences(g *restoreGraph) error {
	for id, t := range g.tasks {
		if t.rootID == "" {
			return fmt.Errorf("restore: task %q has no root", id)
		}
		if _, ok := g.tasks[t.rootID]; !ok {
			return fmt.Errorf("restore: task %q names a missing root %q", id, t.rootID)
		}
		if t.parentID != "" {
			p, ok := g.tasks[t.parentID]
			if !ok {
				return fmt.Errorf("restore: task %q names a missing parent %q", id, t.parentID)
			}
			if p.rootID != t.rootID {
				return fmt.Errorf("restore: task %q crosses a tree boundary", id)
			}
		}
		// Cycle check: walk the parent chain.
		seen := map[string]bool{id: true}
		cur := t.parentID
		for cur != "" {
			if seen[cur] {
				return fmt.Errorf("restore: delegation cycle at %q", cur)
			}
			seen[cur] = true
			p, ok := g.tasks[cur]
			if !ok {
				break
			}
			cur = p.parentID
		}
	}
	for id, a := range g.attempts {
		if _, ok := g.tasks[a.taskID]; !ok {
			return fmt.Errorf("restore: attempt %q names a missing task %q", id, a.taskID)
		}
	}
	for _, l := range g.lineages {
		for _, ref := range []struct {
			what, id string
		}{{"superseded task", l.superseded}, {"replacement", l.replacement}, {"reviewer", l.reviewer}} {
			if ref.id == "" || g.tasks[ref.id] == nil {
				return fmt.Errorf("restore: lineage %q names a missing %s %q", l.id, ref.what, ref.id)
			}
		}
		if l.attemptID == "" || g.attempts[l.attemptID] == nil {
			return fmt.Errorf("restore: lineage %q names a missing attempt %q", l.id, l.attemptID)
		}
	}
	return nil
}

// checkReservedKeys enforces T93/T121: a supersede: key without a naming
// lineage is a legacy collision and is refused. The dump rows carry the
// keys; the graph carries the lineage names.
func checkReservedKeys(m *restoreModel, g *restoreGraph) error {
	namedTasks := map[string]bool{}
	for _, l := range g.lineages {
		namedTasks[l.replacement] = true
		namedTasks[l.reviewer] = true
	}
	for _, r := range m.managed["agent_tasks"] {
		rm := r.rowMap()
		key, ok := rm["idempotency_key"]
		if !ok || key.isNull() {
			continue
		}
		if !strings.HasPrefix(key.text, "supersede:") {
			continue
		}
		id, _ := rm["id"]
		if namedTasks[id.text] {
			continue
		}
		// The contract names this refusal ErrReservedKeyCollision
		// (T121): a reserved key with no naming lineage is a legacy
		// collision, never adopted or rewritten.
		return fmt.Errorf("restore: ErrReservedKeyCollision: reserved key %q has no naming lineage",
			key.text)
	}
	return nil
}

// checkLegacyAttemptShapes enforces T106 (completed without success) and
// T107 (ambiguity conflict) on stored rows.
func checkLegacyAttemptShapes(g *restoreGraph) error {
	for _, a := range g.attempts {
		if a.state == "completed" && a.outcome != "succeeded" {
			return fmt.Errorf("restore: completed_without_success: attempt %q reports %q", a.id, a.outcome)
		}
	}
	for taskID, atts := range g.byTask {
		var amb, live int
		for _, a := range atts {
			switch a.state {
			case "ambiguous":
				amb++
			case "intent", "launched", "running":
				live++
			}
		}
		if amb > 1 {
			return fmt.Errorf("restore: ambiguity_conflict: task %q carries %d ambiguous attempts", taskID, amb)
		}
		if amb == 1 && live > 0 {
			return fmt.Errorf("restore: ambiguity_conflict: task %q pairs an ambiguity with a live attempt", taskID)
		}
	}
	return nil
}

// scopedRole reports whether the task is the replacement or reviewer of
// a live lineage (live = replacement not superseded by a later lineage).
func (g *restoreGraph) scopedRole(taskID string) (isReplacement, isReviewer bool, lineage *restoreLineage) {
	for _, l := range g.lineages {
		live := true
		for _, l2 := range g.lineages {
			if l2.superseded == l.replacement {
				live = false
				break
			}
		}
		if !live {
			continue
		}
		if l.replacement == taskID {
			return true, false, l
		}
		if l.reviewer == taskID {
			return false, true, l
		}
	}
	return false, false, nil
}

// supersedingLineage returns the earliest lineage superseding the task,
// or nil.
func (g *restoreGraph) supersedingLineage(taskID string) *restoreLineage {
	var best *restoreLineage
	for _, l := range g.lineages {
		if l.superseded != taskID {
			continue
		}
		if l.createdAt == nil {
			continue
		}
		if best == nil || (best.createdAt != nil && *l.createdAt < *best.createdAt) {
			best = l
		}
	}
	return best
}

// isHistoricalTask mirrors the live predicate for replay rows.
func (g *restoreGraph) isHistoricalTask(taskID string) bool {
	for _, l := range g.lineages {
		if l.superseded == taskID {
			return true
		}
	}
	t, ok := g.tasks[taskID]
	if !ok {
		return false
	}
	cur := t.parentID
	for cur != "" {
		for _, l := range g.lineages {
			if l.superseded == cur {
				return true
			}
		}
		p, ok := g.tasks[cur]
		if !ok {
			break
		}
		cur = p.parentID
	}
	if t, ok := g.tasks[taskID]; ok && t.role == "reviewer" {
		for _, l := range g.lineages {
			if l.reviewer == taskID && g.supersedingLineage(l.replacement) != nil {
				return true
			}
		}
	}
	return false
}

// checkTemporalBounds enforces the scoped/unscoped temporal rules with
// C1/C2 (T105, T108, T123–T129).
func checkTemporalBounds(g *restoreGraph) error {
	// t_amb per tree: earliest ambiguous updated_at. NULL is
	// indeterminate only when the tree needs it (ambiguous tree with
	// unscoped attempts to judge).
	tAmb := map[string]*int64{}
	for root, atts := range g.ambiguous {
		var best *int64
		for _, a := range atts {
			if a.updatedAt == nil {
				continue
			}
			if best == nil || *a.updatedAt < *best {
				n := *a.updatedAt
				best = &n
			}
		}
		tAmb[root] = best
	}
	// Scoped attempts: lower and upper bounds, judged per lineage so
	// that reviewers of non-live lineages are obsolete (T124) rather
	// than unscoped, and replacements of superseded lineages keep
	// their post-supersession bound (T123).
	for _, l := range g.lineages {
		if l.createdAt == nil {
			continue
		}
		upper := g.supersedingLineage(l.replacement)
		var upperAt *int64
		if upper != nil {
			upperAt = upper.createdAt
			if upperAt == nil || *l.createdAt >= *upperAt {
				return fmt.Errorf("restore: indeterminate_temporal_order: lineage %q and superseding lineage %q are inconsistent", l.id, upper.id)
			}
		}
		for _, role := range []struct {
			taskID string
			kind   string
		}{{l.replacement, "replacement"}, {l.reviewer, "reviewer"}} {
			for _, a := range g.byTask[role.taskID] {
				if a.createdAt == nil {
					return fmt.Errorf("restore: indeterminate_temporal_order: attempt %q has no created_at", a.id)
				}
				if *a.createdAt < *l.createdAt {
					return fmt.Errorf("restore: attempt_before_authorisation: attempt %q predates lineage %q", a.id, l.id)
				}
				if upperAt != nil && *a.createdAt >= *upperAt {
					if role.kind == "replacement" {
						return fmt.Errorf("restore: post_supersession_activity: attempt %q of superseded replacement %q", a.id, a.taskID)
					}
					return fmt.Errorf("restore: obsolete_reviewer_activity: attempt %q of obsolete reviewer %q", a.id, a.taskID)
				}
			}
		}
	}
	// Historical descendants: tasks below a superseded task.
	for id, t := range g.tasks {
		cur := t.parentID
		var naming *restoreLineage
		for cur != "" {
			if l := g.supersedingLineage(cur); l != nil {
				if naming == nil || (l.createdAt != nil && naming.createdAt != nil && *l.createdAt < *naming.createdAt) {
					naming = l
				}
			}
			p, ok := g.tasks[cur]
			if !ok {
				break
			}
			cur = p.parentID
		}
		if naming == nil || naming.createdAt == nil {
			continue
		}
		for _, a := range g.byTask[id] {
			if a.createdAt == nil {
				return fmt.Errorf("restore: indeterminate_temporal_order: attempt %q has no created_at", a.id)
			}
			if *a.createdAt >= *naming.createdAt {
				return fmt.Errorf("restore: historical_descendant_activity: attempt %q under a superseded ancestor of %q", a.id, id)
			}
		}
	}
	// C2: non-ambiguous attempts on a superseded task itself.
	for _, l := range g.lineages {
		if l.createdAt == nil {
			continue
		}
		for _, a := range g.byTask[l.superseded] {
			if a.state == "ambiguous" {
				continue
			}
			if a.createdAt == nil {
				return fmt.Errorf("restore: indeterminate_temporal_order: attempt %q has no created_at", a.id)
			}
			if *a.createdAt >= *l.createdAt {
				return fmt.Errorf("restore: post_supersession_activity: attempt %q on superseded task %q", a.id, l.superseded)
			}
		}
	}
	// Unscoped attempts in ambiguous trees (C1 + T105/T108).
	// Tasks covered by any lineage role or by the historical predicate
	// are judged by the scoped, descendant and C2 arms above — never
	// here. Only genuinely unscoped, non-historical work faces the
	// claim-versus-t_amb ordering.
	lineageScoped := map[string]bool{}
	for _, l := range g.lineages {
		lineageScoped[l.replacement] = true
		lineageScoped[l.reviewer] = true
	}
	for root := range g.ambiguous {
		amb := tAmb[root]
		for taskID, list := range g.byTask {
			t, ok := g.tasks[taskID]
			if !ok || t.rootID != root {
				continue
			}
			if lineageScoped[taskID] || g.isHistoricalTask(taskID) {
				continue
			}
			if isRep, isRev, _ := g.scopedRole(taskID); isRep || isRev {
				continue
			}
			for _, a := range list {
				if a.state == "ambiguous" {
					continue
				}
				cl, ok := g.claims[taskID]
				if !ok {
					return fmt.Errorf("restore: indeterminate_temporal_order: no claimed event for %q epoch %d", taskID, a.epoch)
				}
				ev, ok := cl[a.epoch]
				if !ok || ev.createdAt == nil {
					return fmt.Errorf("restore: indeterminate_temporal_order: no claimed event for %q epoch %d", taskID, a.epoch)
				}
				if a.createdAt == nil {
					return fmt.Errorf("restore: indeterminate_temporal_order: attempt %q has no created_at", a.id)
				}
				if *ev.createdAt > *a.createdAt {
					return fmt.Errorf("restore: indeterminate_temporal_order: attempt %q predates its claim", a.id)
				}
				if amb == nil {
					return fmt.Errorf("restore: indeterminate_temporal_order: tree %q has an ambiguity without a recorded time", root)
				}
				if *a.createdAt >= *amb || *ev.createdAt >= *amb {
					return fmt.Errorf("restore: post_ambiguity_unscoped_session: attempt %q of %q", a.id, taskID)
				}
			}
		}
	}
	return nil
}

// restoreErrorCode extracts the diagnostic code prefix.
func restoreErrorCode(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if i := strings.Index(msg, ":"); i > 0 {
		return msg[:i]
	}
	return msg
}
