package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Embedding COVERAGE vs embedding INTEGRITY
//
// Origin (2026-10-13). `checkEmbeddings` answers a coverage question —
// "what is the distribution of `embedding_source` across memories?" —
// and classifies the result against a statistical tolerance (it only
// warns when nulls exceed 10% of rows). On the live database that
// tolerance reported PASS while 12 of 144 memories carried a literal
// null vector, and while nothing at all verified the payload:
//
//   - a row labelled `embedding_source='provider'` could hold an empty
//     array, a truncated payload, or a non-numeric body, and doctor
//     would still call it provider-generated;
//   - nothing compared vector DIMENSIONS, so a database carrying both
//     384-dim and 768-dim vectors looked uniform;
//   - nothing checked that components are finite;
//   - `memories` has no `embedding_model` column at all, so model
//     identity for a memory vector is unrecoverable from stored data.
//
// A coverage percentage cannot stand in for validity. "12% of memories
// lack a vector" is a different statement from "the vectors we do have
// are real, finite, and mutually comparable", and only the first is
// what the existing check measures.
//
// So this file adds `checkEmbeddingIntegrity`, which validates the
// stored payloads themselves. It is deliberately ADDITIVE: the existing
// `checkEmbeddings` is left intact (tests pin its name, "Stored
// embeddings", and its four-state behaviour), and its doc comment now
// states that it measures coverage only.
//
// Provider-agnostic by construction: nothing here names a vendor,
// recommends a model, or compares against a hardcoded dimension. It
// reports the dimensions it OBSERVED and, where a fingerprint exists,
// the models it OBSERVED. Interpreting those against a configured
// profile stays the operator's call, consistent with the v0.1-final
// decision to remove the embedding probe.
// ---------------------------------------------------------------------------

// embeddingIntegrityResult accumulates the observations the check renders.
type embeddingIntegrityResult struct {
	// Coverage-adjacent counters, reported for context.
	totalVectors   int
	nullVectors    int
	legacyHashRows int

	// Integrity failures, any of which is disqualifying on its own.
	malformedRows int // present but not decodable as a float array
	emptyRows     int // decoded to a zero-length vector
	nonFiniteRows int // contains NaN or ±Inf

	// dimensionHistogram maps observed dimension -> row count.
	dimensionHistogram map[int]int
	// dominantDimension is the most common dimension, for wording.
	dominantDimension int
	// dominantShare is dominantDimension / totalVectors.
	dominantShare float64

	// Fingerprints.
	referenceModels    map[string]int
	referenceDims      map[int]int
	memoryHasModelCol  bool
	referenceChunkRows int
}

// distinctDimensions returns the observed dimensions in ascending order.
func (r embeddingIntegrityResult) distinctDimensions() []int {
	dims := make([]int, 0, len(r.dimensionHistogram))
	for d := range r.dimensionHistogram {
		dims = append(dims, d)
	}
	sort.Ints(dims)
	return dims
}

// memoryEmbeddingModelColumnExists reports whether `memories` carries a
// model-fingerprint column. It does not today; the probe exists so that
// if a future migration adds one, the check reports the fingerprint
// instead of claiming identity is unrecoverable.
//
// Probed by query rather than by reading sqlite_master, because
// DatabaseManager does not expose its *sql.DB handle.
func memoryEmbeddingModelColumnExists(s *DoctorService) bool {
	if s == nil || s.dm == nil {
		return false
	}
	for _, col := range []string{
		"embedding_model", "embedding_fingerprint", "embedding_provider",
	} {
		rows, err := s.dm.QueryTracked(
			fmt.Sprintf("SELECT %s FROM memories LIMIT 1", col))
		if err != nil {
			continue // column absent
		}
		rows.Close()
		return true
	}
	return false
}

// classifyEmbeddingPayload validates one stored embedding body.
//
// Storage format (db.go:3662-3666): memories.embedding holds the JSON
// encoding of a []float32, or the literal string "null" when no
// embedding was supplied. Validation therefore reduces to: does it
// decode, is it non-empty, and is every component finite?
func classifyEmbeddingPayload(body string) (dim int, malformed, empty, nonFinite bool) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" || trimmed == "null" {
		return 0, false, false, false // counted as a null vector, not malformed
	}
	var vec []float32
	if err := json.Unmarshal([]byte(trimmed), &vec); err != nil {
		return 0, true, false, false
	}
	if len(vec) == 0 {
		return 0, false, true, false
	}
	for _, v := range vec {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return len(vec), false, false, true
		}
	}
	return len(vec), false, false, false
}

// collectEmbeddingIntegrity reads every live memory vector and every
// reference-chunk fingerprint. Read-only.
func collectEmbeddingIntegrity(s *DoctorService) (embeddingIntegrityResult, error) {
	res := embeddingIntegrityResult{
		dimensionHistogram: map[int]int{},
		referenceModels:    map[string]int{},
		referenceDims:      map[int]int{},
		memoryHasModelCol:  memoryEmbeddingModelColumnExists(s),
	}

	if s == nil || s.dm == nil {
		return res, fmt.Errorf("no database")
	}

	rows, err := s.dm.QueryTracked(
		`SELECT embedding FROM memories WHERE deleted_at IS NULL`)
	if err != nil {
		return res, fmt.Errorf("query memory embeddings: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var body sql.NullString
		if err := rows.Scan(&body); err != nil {
			return res, fmt.Errorf("scan memory embedding: %w", err)
		}
		text := ""
		if body.Valid {
			text = body.String
		}
		dim, malformed, empty, nonFinite := classifyEmbeddingPayload(text)
		switch {
		case malformed:
			res.malformedRows++
			continue
		case empty:
			res.emptyRows++
			continue
		case nonFinite:
			res.nonFiniteRows++
			// still counted: a non-finite vector has a real dimension
		case dim == 0:
			res.nullVectors++
			continue
		}
		res.totalVectors++
		res.dimensionHistogram[dim]++
	}

	// reference_chunks is the only table carrying a real
	// (embedding_model, embedding_dimension) fingerprint, and it has a
	// working mismatch detector (referenceEmbeddingNeedsRefresh,
	// reference_db.go:495). Report what it holds.
	refRows, err := s.dm.QueryTracked(
		`SELECT embedding_model, embedding_dimension FROM reference_chunks`)
	if err == nil {
		defer refRows.Close()
		for refRows.Next() {
			var model sql.NullString
			var dim sql.NullInt64
			if err := refRows.Scan(&model, &dim); err != nil {
				continue
			}
			res.referenceChunkRows++
			if model.Valid && strings.TrimSpace(model.String) != "" {
				res.referenceModels[model.String]++
			}
			if dim.Valid && dim.Int64 > 0 {
				res.referenceDims[int(dim.Int64)]++
			}
		}
	}

	best, bestN := 0, -1
	for d, n := range res.dimensionHistogram {
		if n > bestN {
			best, bestN = d, n
		}
	}
	res.dominantDimension = best
	if bestN > 0 && res.totalVectors > 0 {
		res.dominantShare = float64(bestN) / float64(res.totalVectors)
	}
	return res, nil
}

// checkEmbeddingIntegrity validates stored embedding payloads.
//
// FAIL conditions (any disqualifies the stored vectors, and none is
// subject to a statistical tolerance — a single malformed or
// dimension-inconsistent vector means the vector index cannot be
// trusted for similarity ranking):
//
//   - a malformed or empty payload;
//   - a non-finite component;
//   - more than one observed dimension among live memory vectors.
//
// WARN conditions:
//
//   - null vectors present at any count. This is the explicit
//     counterpart to the coverage check's ">10%" tolerance: coverage
//     tolerates a small shortfall, integrity does not tolerate a row
//     that claims a source it does not have.
//   - a fingerprint split across reference chunks (more than one
//     distinct embedding_model), which means references were embedded
//     by more than one model generation.
//
// The check never asserts that the observed model or dimension MATCHES
// the operator's configured profile. Doing so would require a live
// provider call, which the removed embedding probe performed and which
// this design does not restore. The report states what was observed and
// leaves the comparison to the operator.
func (s *DoctorService) checkEmbeddingIntegrity() DoctorCheck {
	check := DoctorCheck{Name: "Embedding integrity"}

	res, err := collectEmbeddingIntegrity(s)
	if err != nil {
		check.Status = "WARN"
		check.Message = fmt.Sprintf("could not validate stored vectors: %v", err)
		return check
	}

	dims := res.distinctDimensions()
	var details []string

	// -- dimension distribution ------------------------------------------
	switch len(dims) {
	case 0:
		if res.totalVectors == 0 {
			details = append(details,
				"No decodable memory vectors stored; nothing to validate.")
		}
	case 1:
		details = append(details,
			fmt.Sprintf("All %d stored memory vectors are %d-dimensional.",
				res.totalVectors, dims[0]))
	default:
		detailParts := make([]string, 0, len(dims))
		for _, d := range dims {
			detailParts = append(detailParts,
				fmt.Sprintf("%d×%d-dim", res.dimensionHistogram[d], d))
		}
		details = append(details,
			fmt.Sprintf("Mixed vector dimensions across live memories: %s. Vectors of "+
				"different dimensions are not comparable, so cosine similarity across "+
				"them is meaningless and hybrid ranking degrades silently.",
				strings.Join(detailParts, ", ")))
	}

	// -- malformed / empty / non-finite ---------------------------------
	switch {
	case res.malformedRows > 0:
		details = append(details,
			fmt.Sprintf("%d memory rows carry an embedding that is not a decodable "+
				"float array.", res.malformedRows))
	case res.emptyRows > 0:
		details = append(details,
			fmt.Sprintf("%d memory rows carry a zero-length vector.", res.emptyRows))
	}
	if res.nonFiniteRows > 0 {
		details = append(details,
			fmt.Sprintf("%d memory rows carry a non-finite component (NaN or ±Inf). "+
				"A NaN makes every comparison false, so guards written as "+
				"`score < threshold` PASS on it rather than rejecting it.",
				res.nonFiniteRows))
	}

	// -- nulls: integrity FAILs, coverage WARNs -------------------------
	if res.nullVectors > 0 {
		details = append(details,
			fmt.Sprintf("%d live memories have no vector. This is reported here as a "+
				"count, not a percentage: a null vector is an absent vector, and no "+
				"threshold makes it valid.", res.nullVectors))
	}

	// -- model fingerprints ---------------------------------------------
	if res.memoryHasModelCol {
		details = append(details,
			"memories now carries a model-fingerprint column; per-row identity is available.")
	} else {
		details = append(details,
			"Model identity for memory vectors is NOT recoverable: `memories` has no "+
				"embedding_model column, so a stored vector cannot be attributed to the "+
				"model that produced it. If the embedding profile is changed, existing "+
				"memory vectors are retained unlabelled and cannot be detected as "+
				"stale. Re-embed with `mpm ops backfill-embeddings --force` (memories "+
				"backfill is otherwise NULL-only) and rebuild the vector index.")
	}
	switch len(res.referenceModels) {
	case 0:
		if res.referenceChunkRows > 0 {
			details = append(details,
				fmt.Sprintf("%d reference chunks carry no model fingerprint.",
					res.referenceChunkRows))
		}
	case 1:
		model := ""
		for m := range res.referenceModels {
			model = m
		}
		dimNote := ""
		if len(res.referenceDims) == 1 {
			for d := range res.referenceDims {
				dimNote = fmt.Sprintf(" at %d dimensions", d)
			}
		} else if len(res.referenceDims) > 1 {
			dimNote = fmt.Sprintf(" across MIXED dimensions (%d distinct)",
				len(res.referenceDims))
		}
		details = append(details,
			fmt.Sprintf("All %d reference chunks fingerprint to %q%s.",
				res.referenceChunkRows, model, dimNote))
	default:
		parts := make([]string, 0, len(res.referenceModels))
		for m, n := range res.referenceModels {
			parts = append(parts, fmt.Sprintf("%q×%d", m, n))
		}
		sort.Strings(parts)
		details = append(details,
			fmt.Sprintf("Reference chunks span %d distinct embedding models (%s). "+
				"Vectors from different model generations are not interchangeable.",
				len(res.referenceModels), strings.Join(parts, ", ")))
	}

	check.Details = details

	// -- verdict --------------------------------------------------------
	fatal := res.malformedRows > 0 || res.emptyRows > 0 ||
		res.nonFiniteRows > 0 || len(dims) > 1 || len(res.referenceModels) > 1
	switch {
	case fatal:
		check.Status = "FAIL"
		check.Message = fmt.Sprintf(
			"%d stored vectors are malformed, non-finite, or dimension-inconsistent; "+
				"vector-based similarity ranking cannot be trusted",
			res.malformedRows+res.emptyRows+res.nonFiniteRows)
	case res.nullVectors > 0 && res.totalVectors == 0:
		check.Status = "WARN"
		check.Message = fmt.Sprintf(
			"%d live memories have no vector; none are stored", res.nullVectors)
	case res.nullVectors > 0:
		check.Status = "WARN"
		check.Message = fmt.Sprintf(
			"%d live memories have no vector; the %d that are stored are well-formed",
			res.nullVectors, res.totalVectors)
	case res.totalVectors == 0:
		check.Status = "INFO"
		check.Message = "no memory vectors stored — semantic recall is unavailable, " +
			"BM25/FTS5 recall is unaffected"
	default:
		check.Status = "PASS"
		check.Message = fmt.Sprintf(
			"%d stored vectors, all well-formed and uniformly %d-dimensional",
			res.totalVectors, res.dominantDimension)
	}
	return check
}
