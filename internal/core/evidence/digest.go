package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// The DIGEST, and why it is over the CONTENT.
//
// An evidence record is identified by the digest of its own bytes. That
// makes the identifier VERIFIABLE: a reader downloads the record, hashes
// it, and compares the hash with the name the index gave it. An
// identifier that only the publisher can check is an identifier the reader
// has to trust.
//
// The digest is computed over the CANONICAL form of the record: the JSON
// encoding of the record as published, with its fields in the order the
// struct declares them and its maps sorted. Two publications of the same
// evidence therefore produce the same bytes, the same digest and the same
// file name — which is what makes republication idempotent rather than
// noisy.
//
// # THE INDEX DOES NOT AUTHENTICATE ITSELF
//
// A record names its own digest only by being NAMED BY it. The index
// names the records; nothing names the index from inside the index. So
// there is no SHA anywhere in the published content that claims to be the
// SHA of the content containing it — the self-reference that would make
// the content impossible to compute in the first place.

// DigestPrefix is the prefix every digest in this format carries.
const DigestPrefix = "sha256:"

// Digest returns the digest of the canonical bytes.
//
// The bytes are the record's JSON encoding. The function is deterministic:
// the same record always produces the same digest.
func Digest(r *Record) (string, error) {
	raw, err := CanonicalJSON(r)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return DigestPrefix + hex.EncodeToString(sum[:]), nil
}

// CanonicalJSON returns the canonical bytes of a record.
//
// "Canonical" here means: the JSON encoding, with the fields in the order
// the struct declares them and every map key sorted. Go's encoder already
// sorts map keys, so the encoding of a record IS its canonical form.
func CanonicalJSON(r *Record) ([]byte, error) {
	if r == nil {
		return nil, fmt.Errorf("evidence: there is no record to encode")
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("evidence: encode the record: %w", err)
	}
	return raw, nil
}

// RecordID returns the record's identifier: its digest, without the
// prefix.
//
// It is the FILE NAME of the published record, and the KEY the index uses
// for it. It is derived from the content, so two publications of the same
// evidence land on the same file.
func RecordID(r *Record) (string, error) {
	digest, err := Digest(r)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(digest, DigestPrefix), nil
}

// SnapshotID returns the identifier of a SET of records.
//
// It is the digest over the canonical list of the records' identities and
// their logical keys. Two publications of the same corpus produce the
// same snapshot id, so a republication does not manufacture a new
// snapshot of an unchanged corpus.
//
// The snapshot id is computed over the LIST, not over the index that
// points at the list: the list is immutable content, and the index is the
// mutable pointer.
func SnapshotID(records []*Record) (string, error) {
	type entry struct {
		ID     string `json:"id"`
		RootID string `json:"root_id"`
		TaskID string `json:"task_id"`
	}
	entries := make([]entry, 0, len(records))
	for _, r := range records {
		id, err := RecordID(r)
		if err != nil {
			return "", err
		}
		entries = append(entries, entry{ID: id, RootID: r.RootID, TaskID: r.TaskID})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].RootID != entries[j].RootID {
			return entries[i].RootID < entries[j].RootID
		}
		return entries[i].TaskID < entries[j].TaskID
	})
	raw, err := json.Marshal(entries)
	if err != nil {
		return "", fmt.Errorf("evidence: encode the snapshot list: %w", err)
	}
	sum := sha256.Sum256(raw)
	return DigestPrefix + hex.EncodeToString(sum[:]), nil
}

// DigestBytes returns the digest of arbitrary bytes.
//
// It is what a reader computes over a downloaded record, and it is the
// same function the publisher used, so the comparison is meaningful.
func DigestBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return DigestPrefix + hex.EncodeToString(sum[:])
}

// SameDigest reports whether two digests name the same bytes.
//
// It is deliberately tolerant of the prefix and of case, because a reader
// may have copied a digest out of a terminal or a web page. It is NOT
// tolerant of a MISSING digest: two unknowns are not the same thing, and
// treating them as equal would make a missing digest verify.
func SameDigest(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	na := strings.ToLower(strings.TrimSpace(a))
	nb := strings.ToLower(strings.TrimSpace(b))
	na = strings.TrimPrefix(na, DigestPrefix)
	nb = strings.TrimPrefix(nb, DigestPrefix)
	if na == "" || nb == "" {
		return false
	}
	if na == Unknown || nb == Unknown {
		return false
	}
	return na == nb
}
