package update

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// HistoryEntry is one line of update-history.jsonl: the outcome of one
// update or rollback attempt.
//
// The schema fingerprints are the rollback safety mechanism. pre_fp is
// the live database's shape BEFORE the update; post_fp is the shape
// after the smoke step. A rollback to `from_rev` is safe only while the
// live database still has the pre-update shape — if a migration (or any
// later write) changed it, restoring the old binaries without restoring
// the old data would strand the operator on a schema the old runtime
// never knew. See rollback.go.
type HistoryEntry struct {
	TS            time.Time `json:"ts"`
	Action        string    `json:"action"` // "update" or "rollback"
	FromRev       string    `json:"from_rev,omitempty"`
	ToRev         string    `json:"to_rev,omitempty"`
	Source        string    `json:"source,omitempty"` // github | git | local
	SourceRef     string    `json:"source_ref,omitempty"`
	Commit        string    `json:"commit,omitempty"`
	Outcome       string    `json:"outcome"` // "success" | "refused" | "failed"
	Detail        string    `json:"detail,omitempty"`
	BackupDir     string    `json:"backup_dir,omitempty"`
	PreFP         string    `json:"pre_fp,omitempty"`
	PostFP        string    `json:"post_fp,omitempty"`
	SchemaChanged bool      `json:"schema_changed,omitempty"`
}

// AppendHistory appends one entry to update-history.jsonl. History is
// append-only: a failed update is recorded, not erased, so the next
// operator can see what was tried.
func AppendHistory(dataRoot string, e HistoryEntry) error {
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	path := NewLayout(dataRoot).History()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("could not record the update outcome: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(raw, '\n')); err != nil {
		return fmt.Errorf("could not record the update outcome: %w", err)
	}
	return nil
}

// ReadHistory returns every recorded entry, oldest first. A missing
// file is not an error — it means no update has ever been recorded.
func ReadHistory(dataRoot string) ([]HistoryEntry, error) {
	path := NewLayout(dataRoot).History()
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []HistoryEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e HistoryEntry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("the update history at %s is corrupt: %w", path, err)
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// LastSuccessfulUpdateTo returns the most recent successful update that
// LANDED on toRev, or nil. Rollback planning uses it to find the
// pre-update fingerprint of the current state.
func LastSuccessfulUpdateTo(hist []HistoryEntry, toRev string) *HistoryEntry {
	for i := len(hist) - 1; i >= 0; i-- {
		if hist[i].Action == "update" && hist[i].Outcome == "success" && hist[i].ToRev == toRev {
			e := hist[i]
			return &e
		}
	}
	return nil
}

// RecordPreviousRelease writes .previous-release, the pointer target
// before activation. Same name install.sh uses, so either tool reads
// what the other wrote.
func RecordPreviousRelease(dataRoot, target string) error {
	return os.WriteFile(NewLayout(dataRoot).PreviousRelease(), []byte(target+"\n"), 0600)
}

// ReadPreviousRelease returns the recorded previous pointer target, or
// "" when none was recorded.
func ReadPreviousRelease(dataRoot string) string {
	raw, err := os.ReadFile(NewLayout(dataRoot).PreviousRelease())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}
