// skill_workshop.go — MPM Skill Workshop.
//
// The workshop is a structured skill-formation and validation workflow
// that lives above the existing skill persistence and validation
// architecture. See docs/archive/2026-08-28-mpm-skill-workshop-design.md
// for the full contract.
//
// This file holds:
//   - The single-flight cache (workshopCache, claimOrWait, publishResult)
//   - The pipeline stages (input validation, decision model,
//     when_to_use check, duplicate detection, identity check,
//     change_type mapping, validate, publish)
//   - The two new DatabaseManager methods (ValidateSkill,
//     SaveSkillAndDeprecatePrior) are defined in skill_db.go where
//     their non-mutating / transactional primitives live alongside
//     SaveSkill.

package internal

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

const (
	workshopCacheTTL    = 24 * time.Hour
	workshopWaitTimeout = 60 * time.Second
)

var errWorkshopWaitTimeout = errors.New("workshop: cache wait timed out")

// workshopCacheEntry holds the in-flight result of a workshop execution.
// `done` is closed by the first writer when publishResult is called,
// unblocking any concurrent waiters observing the same key.
type workshopCacheEntry struct {
	done     chan struct{}
	response json.RawMessage
	err      error
}

// workshopCache is the in-memory single-flight dedup map.
// Package-level: cleared on daemon restart. The durable identity
// check (see identityCheckStage) provides the cross-restart guarantee;
// this cache is a deduplication convenience, not a correctness
// mechanism.
var workshopCache sync.Map // map[string]*workshopCacheEntry

// claimOrWait atomically claims the workshop_key slot. If the slot is
// unclaimed, the caller is the writer and receives a fresh entry; if
// the slot is already claimed, the caller blocks on entry.done (or
// ctx.Done / workshopWaitTimeout, whichever fires first) and returns
// the same entry.
//
// Returns (entry, isWriter, err). isWriter=true means the caller MUST
// call publishResult on the entry when finished; isWriter=false means
// the caller should consume entry.response / entry.err instead.
func claimOrWait(ctx context.Context, key string) (*workshopCacheEntry, bool, error) {
	newEntry := &workshopCacheEntry{done: make(chan struct{})}
	actual, loaded := workshopCache.LoadOrStore(key, newEntry)
	entry := actual.(*workshopCacheEntry)

	if loaded {
		select {
		case <-entry.done:
			return entry, false, nil
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-time.After(workshopWaitTimeout):
			return nil, false, errWorkshopWaitTimeout
		}
	}

	// Schedule cleanup after TTL. CompareAndDelete ensures we only
	// remove the entry we put in (not a newer claim from a later call
	// that happened to share the slot after expiry).
	time.AfterFunc(workshopCacheTTL, func() {
		workshopCache.CompareAndDelete(key, entry)
	})
	return newEntry, true, nil
}

// publishResult stores the response on the entry and unblocks waiters.
// MUST be called by the writer (the caller for which claimOrWait
// returned isWriter=true).
func publishResult(entry *workshopCacheEntry, response json.RawMessage, err error) {
	entry.response = response
	entry.err = err
	close(entry.done)
}
