// cache.go — system_config[model_probe_results] read/write.
//
// Cache shape: a single row whose JSON value is an array of CachedProbe
// entries, bounded to DefaultMaxCachedRecords (FIFO eviction). Saving
// through SaveSystemConfig uses the existing content-hash short-circuit
// (db.go), so identical writes are zero-cost.

package probe

import (
	"encoding/json"
	"sort"
	"strings"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// contentHash computes a stable hex digest over an arbitrary payload.
// Used to populate the system_config.content_hash column for
// short-circuit identical-write detection. SHA-256 hex (64 chars).
func contentHash(s string) string {
	// Reuse the same hash family as the fingerprint for code clarity.
	// Different output lengths (64 chars vs 64 chars from fingerprint
	// which is also 64 chars), so DB-side constant-time compare sees
	// the same shape.
	return ComputeFingerprint(fingerprintInput{Provider: s})
}

// LoadCache reads the cached probes row. Returns an empty slice (not an
// error) when the row is absent or unparseable — Doctor and the dashboard
// both treat absence as "no recent state".
func LoadCache(dm *mpminternal.DatabaseManager) ([]CachedProbe, error) {
	if dm == nil {
		return nil, nil
	}
	row, err := dm.GetSystemConfig(SystemConfigKey)
	if err != nil || row == nil {
		// No row yet — that's the normal first-run state.
		return nil, nil
	}
	rawJSON, _ := row["raw_json"].(string)
	if rawJSON == "" {
		return nil, nil
	}
	var entries []CachedProbe
	if err := json.Unmarshal([]byte(rawJSON), &entries); err != nil {
		// Corrupt row — return empty rather than failing the caller.
		return nil, nil
	}
	return entries, nil
}

// SaveCache atomically writes the bounded result set to system_config.
// Uses SaveSystemConfig's content-hash short-circuit so a no-op merge
// (identical content) is zero-cost. Caller is responsible for bounding
// the result set via ApplyAndBound before calling SaveCache.
func SaveCache(dm *mpminternal.DatabaseManager, entries []CachedProbe) error {
	if dm == nil {
		return nil
	}
	bounded := BoundEntries(entries)
	raw, err := json.Marshal(bounded)
	if err != nil {
		return err
	}
	hash := contentHash(string(raw))
	_, err = dm.SaveSystemConfig(SystemConfigKey, string(raw), hash, "[]")
	return err
}

// BoundEntries applies FIFO eviction at DefaultMaxCachedRecords. The
// returned slice is always a copy; input is unchanged.
func BoundEntries(entries []CachedProbe) []CachedProbe {
	if len(entries) <= DefaultMaxCachedRecords {
		return append([]CachedProbe(nil), entries...)
	}
	return append([]CachedProbe(nil), entries[len(entries)-DefaultMaxCachedRecords:]...)
}

// MergeCache merges a fresh result set with the existing cache row.
//
//   - existing rows whose fingerprint is still in `currentFingerprints`
//     AND not being refreshed are preserved
//   - existing rows whose fingerprint is no longer in `currentFingerprints`
//     are dropped (connection fields changed → not reachable by that path)
//   - existing rows being refreshed by a fresh result are skipped here;
//     the fresh version is appended
//   - fresh results are appended (in slice order) so the trailing entries
//     are the most-recent probes
//
// `currentFingerprints` is the union of (1) all fingerprints that were
// actually probed in this Doctor invocation, and (2) all fingerprints
// the live config still resolves to (so routing-label changes don't drop
// otherwise-healthy entries). Caller passes that set in.
//
// Result is bounded via BoundEntries.
//
// This is the single merge function used by both Doctor (multi-result)
// and config-time verification (single-result).
func MergeCache(existing []CachedProbe, fresh []ProbeResult, currentFingerprints map[string]struct{}) []CachedProbe {
	freshByFp := make(map[string]CachedProbe, len(fresh))
	for _, r := range fresh {
		freshByFp[r.Fingerprint] = ToCachedProbe(r)
	}

	out := make([]CachedProbe, 0, len(existing)+len(fresh))
	for _, e := range existing {
		// Skip rows that are being refreshed this round; fresh version wins.
		if _, isFresh := freshByFp[e.Fingerprint]; isFresh {
			continue
		}
		// Drop rows whose fingerprints are no longer reachable from the
		// current effective routing. This handles two cases:
		//   (a) connection fields changed (operator edit) — fingerprint
		//       mismatch naturally drops the row
		//   (b) profile removed from config — fingerprint no longer in
		//       current set
		if _, live := currentFingerprints[e.Fingerprint]; !live {
			continue
		}
		out = append(out, e)
	}

	// Append fresh results so the trailing entries are most-recent.
	for _, r := range fresh {
		out = append(out, freshByFp[r.Fingerprint])
	}

	return BoundEntries(out)
}

// ToCachedProbe converts an in-memory ProbeResult to the persistable
// CachedProbe shape. Routing labels are dropped; only connection-health
// facts are kept.
func ToCachedProbe(r ProbeResult) CachedProbe {
	return CachedProbe{
		Fingerprint:  r.Fingerprint,
		Provider:     r.Provider,
		Model:        r.Model,
		BaseURLSafe:  r.BaseURLSafe,
		Status:       r.Status.String(),
		LatencyMs:    r.LatencyMs,
		ErrorClass:   r.ErrorClass,
		ErrorSummary: r.ErrorSummary,
		CheckedAt:    r.CheckedAt.Unix(),
	}
}

// SortByCheckedAt orders entries oldest-first, newest-last. Used only when
// loadCache output arrives in arbitrary order; production writes are
// append-only.
func SortByCheckedAt(entries []CachedProbe) []CachedProbe {
	out := append([]CachedProbe(nil), entries...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CheckedAt != out[j].CheckedAt {
			return out[i].CheckedAt < out[j].CheckedAt
		}
		return strings.Compare(out[i].Fingerprint, out[j].Fingerprint) < 0
	})
	return out
}
