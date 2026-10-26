package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	mpminternal "github.com/flowbyte-com/mpm-core"
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
	// Coverage-adjacent counters, reported for context. These count
	// ORDINARY memories only — directive rows are accounted for
	// separately, below.
	totalVectors   int
	nullVectors    int
	legacyHashRows int

	// Bootstrap directive rows.
	//
	// 2026-10-23 (HAL-9001 Finding 5): a directive is policy text, not
	// a searchable memory — `mpm recall` searches collection='memories'
	// and the hybrid search filters on the same collection — so its
	// missing vector is an EXPECTED POLICY CHOICE rather than an
	// indexing gap. Directive rows are therefore excluded from the
	// vector statistics (a directive's vector is never ranked against
	// anything) and disclosed separately instead of being silently
	// dropped or counted as a defect.
	directiveRows        int // live directive rows
	directiveNullVectors int // directive rows holding no vector
	directiveDefects     int // directive rows whose body is malformed, empty or non-finite

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
	// memoryUnlabelledRows counts live memory vectors that carry no
	// embedding_model. Distinct from memoryHasModelCol: the column can
	// exist while every row predates it, which is the state the live
	// database was in when the column was added on 2026-10-13 (all 149).
	memoryUnlabelledRows int

	// Partial-migration split (2026-10-25, Hermes review, priority 3).
	//
	// A store can be HALF migrated: some vectors carry the active model's
	// fingerprint, some carry another model's, and some carry none. The
	// retrieval filter accepts only the first group, so a partially
	// migrated database silently ranks a subset of the corpus. These
	// counters make the split visible, and memoryActiveModel records the
	// fingerprint "compatible" was measured against.
	memoryActiveModel      string
	memoryCompatibleRows   int // labelled with memoryActiveModel
	memoryIncompatibleRows int // labelled with a different model
	memoryLabelledRows     int // any non-empty fingerprint
	// memoryUnlabelledVectors counts embedded rows (real vector body) with
	// no fingerprint. Distinct from memoryUnlabelledRows, which keeps the
	// legacy predicate (embedding IS NOT NULL, including the literal
	// 'null' body) behind the pre-existing disclosure message.
	memoryUnlabelledVectors int

	// Vector retrieval mode, as it would actually run right now:
	// zero clusters means IVFSearch returns nil and VectorMatch
	// brute-forces every compatible row.
	ivfClusterCount int
	vectorMaxScan   int
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

// hasVectorBodySQL is the SQL form of classifyEmbeddingPayload's
// "this row holds no vector" definition: the column is SQL NULL, empty
// after trimming whitespace, or the literal string "null".
//
// The trim set mirrors Go's unicode.IsSpace ASCII subset so the SQL
// predicate and the Go classifier agree on every body —
// TestHasVectorBodySQL_MatchesTheGoClassifier pins that agreement.
//
// 2026-10-23 (HAL-9001 Finding 5): the coverage check used to infer
// coverage from the `embedding_source` LABEL, whose column default is
// 'provider'. A row that held no vector at all was therefore counted
// as provider-covered.
const hasVectorBodySQL = `embedding IS NOT NULL AND TRIM(CAST(embedding AS TEXT), ' ' || char(9) || char(10) || char(11) || char(12) || char(13)) NOT IN ('', 'null')`

// activeEmbeddingModelName returns the configured provider's model name
// WITHOUT probing it.
//
// This mirrors the model half of CurrentEmbeddingFingerprint (which is
// what VectorMatch's compatibility filter compares against) but
// deliberately skips the dimension half: ActiveEmbeddingDimension issues
// a sample embed, and `mpm doctor` is read-only — a diagnostic must not
// make a provider call, allocate a vector, or depend on provider
// reachability to report local state.
func activeEmbeddingModelName() string {
	cfg := mpminternal.DefaultEmbeddingConfig()
	if cfg == nil || cfg.Provider == nil || cfg.Provider.Name() == "null" {
		return ""
	}
	return cfg.Provider.Name()
}

// collectEmbeddingIntegrity reads every live memory vector and every
// reference-chunk fingerprint. Read-only.
func collectEmbeddingIntegrity(s *DoctorService) (embeddingIntegrityResult, error) {
	res := embeddingIntegrityResult{
		dimensionHistogram: map[int]int{},
		referenceModels:    map[string]int{},
		referenceDims:      map[int]int{},
	}
	if s == nil || s.dm == nil {
		return res, fmt.Errorf("no database")
	}
	res.memoryHasModelCol = memoryEmbeddingModelColumnExists(s)

	// Count unlabelled vectors separately from the column's existence.
	// Only meaningful once the column is present; before the migration
	// every row is unlabelled by construction and saying so would be noise.
	if res.memoryHasModelCol {
		if rows, err := s.dm.QueryTracked(
			`SELECT COUNT(*) FROM memories
			 WHERE deleted_at IS NULL AND embedding IS NOT NULL
			   AND (embedding_model IS NULL OR TRIM(embedding_model) = '')`); err == nil {
			if rows.Next() {
				var n int
				if err := rows.Scan(&n); err == nil {
					res.memoryUnlabelledRows = n
				}
			}
			rows.Close()
		}
	}

	// The fingerprint split. Read-only and provider-agnostic: the active
	// model NAME comes from local configuration (no probe, no network),
	// and the groups come from the stored column. Directive rows and
	// vectorless rows are excluded for the same reasons they are
	// excluded from the dimension statistics above.
	res.memoryActiveModel = activeEmbeddingModelName()
	if res.memoryHasModelCol {
		if rows, err := s.dm.QueryTracked(
			`SELECT COALESCE(embedding_model, ''), COUNT(*)
			 FROM memories
			 WHERE deleted_at IS NULL AND ` + hasVectorBodySQL + `
			   AND COALESCE(is_prime_directive, 0) = 0
			   AND COALESCE(collection, '') <> 'directives'
			 GROUP BY 1`); err == nil {
			for rows.Next() {
				var model string
				var n int
				if err := rows.Scan(&model, &n); err != nil {
					rows.Close()
					return res, fmt.Errorf("scan embedding-model counts: %w", err)
				}
				switch {
				case strings.TrimSpace(model) == "":
					// Real vector, no fingerprint. Counted separately from
					// memoryUnlabelledRows (legacy predicate, which also
					// counts rows whose body is the literal 'null').
					res.memoryUnlabelledVectors += n
				case res.memoryActiveModel != "" && model == res.memoryActiveModel:
					res.memoryCompatibleRows += n
					res.memoryLabelledRows += n
				default:
					res.memoryIncompatibleRows += n
					res.memoryLabelledRows += n
				}
			}
			rows.Close()
		}
	}

	if err := s.dm.QueryRowTracked(`SELECT COUNT(*) FROM vector_clusters`).Scan(&res.ivfClusterCount); err != nil {
		// Index table absent (pre-IVF database) or unreadable: report the
		// state we can prove rather than failing the whole check.
		res.ivfClusterCount = 0
	}
	res.vectorMaxScan = s.dm.GetConfigInt("vector.max_scan", 5000)

	rows, err := s.dm.QueryTracked(
		`SELECT embedding, collection, COALESCE(is_prime_directive, 0) FROM memories WHERE deleted_at IS NULL`)
	if err != nil {
		return res, fmt.Errorf("query memory embeddings: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var body sql.NullString
		var collection sql.NullString
		var primeDirective int
		if err := rows.Scan(&body, &collection, &primeDirective); err != nil {
			return res, fmt.Errorf("scan memory embedding: %w", err)
		}
		text := ""
		if body.Valid {
			text = body.String
		}
		dim, malformed, empty, nonFinite := classifyEmbeddingPayload(text)

		// Directive rows are policy text, not ranked memories. Route
		// them into their own counters so their missing vector is
		// disclosed rather than counted as an indexing gap — and so a
		// directive's vector (if it ever has one) cannot skew the
		// dimension histogram of the vectors that ARE ranked.
		isDirective := primeDirective == 1 ||
			(collection.Valid && collection.String == "directives")
		if isDirective {
			res.directiveRows++
			switch {
			case malformed, empty, nonFinite:
				res.directiveDefects++
			case dim == 0:
				res.directiveNullVectors++
			}
			continue
		}

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
//
// 2026-10-23 (HAL-9001 Finding 5): bootstrap directive rows are routed
// into their own counters rather than the vector statistics. A directive
// is standing policy, never a vector-search candidate, so its absent
// vector is an expected policy choice and not an indexing gap — but it
// is DISCLOSED in the details rather than dropped, and a genuine gap in
// an ordinary memory still WARNs exactly as before.
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

	// -- bootstrap directives ------------------------------------------
	//
	// Disclosed, not counted as gaps. A directive is policy text read
	// at wake time; it is never a vector-search candidate, so its
	// missing vector is a policy choice rather than an indexing gap.
	// The row is still reported so the operator can see why the
	// database holds rows without vectors.
	if res.directiveRows > 0 {
		msg := fmt.Sprintf(
			"%d bootstrap directive rows are excluded from these statistics. A directive "+
				"is standing policy read at wake time, not a searchable memory, so it "+
				"carries no vector by design — this is not an indexing gap. %d of them "+
				"hold no vector.", res.directiveRows, res.directiveNullVectors)
		if res.directiveDefects > 0 {
			msg += fmt.Sprintf(" %d directive rows carry a malformed, empty or non-finite "+
				"body; that IS a data defect, but it does not affect ranking because a "+
				"directive is never ranked.", res.directiveDefects)
		}
		details = append(details, msg)
	}

	// -- model fingerprints ---------------------------------------------
	//
	// The column's presence and its population are different facts. As of
	// the 2026-10-13 migration `memories` HAS the column, but every row
	// written before it still carries NULL — on the live database that was
	// all 149. Reporting only "the column exists" would read as "identity is
	// available" when in practice no memory vector is yet attributable, so
	// the unlabelled count is stated separately.
	if res.memoryHasModelCol {
		msg := "memories carries a model-fingerprint column; per-row identity is recorded as of the 2026-10-13 migration."
		if res.memoryUnlabelledRows > 0 {
			msg += fmt.Sprintf(" %d memory rows predate it and still have no recorded model: "+
				"their vectors cannot be attributed to the model that produced them, and "+
				"retrieval excludes them until they are re-embedded. Re-embed with "+
				"`mpm ops backfill-embeddings`.", res.memoryUnlabelledRows)
		}
		details = append(details, msg)

		// Partial-migration split. A half-migrated store is the silent
		// case: retrieval keeps working, on whatever subset happens to
		// match the active model, and nothing above says how large that
		// subset is.
		totalLabelled := res.memoryLabelledRows
		excluded := res.memoryIncompatibleRows + res.memoryUnlabelledVectors
		switch {
		case totalLabelled == 0 && res.memoryUnlabelledVectors == 0:
			// Nothing embedded; already reported by the dimension section.
		case res.memoryActiveModel == "":
			details = append(details, fmt.Sprintf(
				"No embedding model is configured, so no stored vector can be attributed to "+
					"the model producing queries: %d vectors carry a fingerprint (%d other model(s), "+
					"%d active) and %d carry none. Retrieval compares raw vectors without a "+
					"model-compatibility filter in this state. Configure the embedding profile "+
					"(`mpm config`) and re-embed with `mpm ops backfill-embeddings --force-reembed`.",
				res.memoryLabelledRows, res.memoryIncompatibleRows, res.memoryCompatibleRows,
				res.memoryUnlabelledVectors))
		default:
			details = append(details, fmt.Sprintf(
				"Active embedding model %q: %d of %d embedded memories are compatible and "+
					"eligible for vector matching; %d were embedded by a different model and %d "+
					"carry no fingerprint, so they are excluded (never compared on dimension "+
					"alone). Retrieval ranks only the compatible subset until they are "+
					"re-embedded: `mpm ops backfill-embeddings`.",
				res.memoryActiveModel, res.memoryCompatibleRows,
				res.memoryCompatibleRows+excluded,
				res.memoryIncompatibleRows, res.memoryUnlabelledVectors))
		}
	} else {
		details = append(details,
			"Model identity for memory vectors is NOT recoverable: `memories` has no "+
				"embedding_model column, so a stored vector cannot be attributed to the "+
				"model that produced it. If the embedding profile is changed, existing "+
				"memory vectors are retained unlabelled and cannot be detected as "+
				"stale. Re-embed with `mpm ops backfill-embeddings --force-reembed` (memories "+
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

	// -- retrieval mode --------------------------------------------------
	//
	// Report the path VectorMatch would actually take, not the path the
	// operator might assume. An empty IVF index means every semantic
	// query falls through to a brute-force scan of all compatible rows,
	// bounded only by vector.max_scan.
	mode := "brute force"
	if res.ivfClusterCount > 0 {
		mode = fmt.Sprintf("IVF (%d clusters, probe_p=%d)",
			res.ivfClusterCount, mpminternal.DefaultIVFConfig().ProbeP)
	}
	capNote := "no scan cap"
	if res.vectorMaxScan > 0 {
		capNote = fmt.Sprintf("scan cap %d rows", res.vectorMaxScan)
	}
	modeMsg := fmt.Sprintf("Vector retrieval mode: %s; %s.", mode, capNote)
	if res.ivfClusterCount == 0 {
		modeMsg += " The IVF index holds no clusters, so each semantic query scans every " +
			"compatible vector. Build the index with `mpm ops rebalance`."
	}
	details = append(details, modeMsg)

	check.Details = details

	// -- verdict --------------------------------------------------------
	fatal := res.malformedRows > 0 || res.emptyRows > 0 ||
		res.nonFiniteRows > 0 || len(dims) > 1 || len(res.referenceModels) > 1
	partialExcluded := res.memoryIncompatibleRows + res.memoryUnlabelledVectors
	switch {
	case fatal:
		check.Status = "FAIL"
		check.Message = fmt.Sprintf(
			"%d stored vectors are malformed, non-finite, or dimension-inconsistent; "+
				"vector-based similarity ranking cannot be trusted",
			res.malformedRows+res.emptyRows+res.nonFiniteRows)
	case res.memoryActiveModel != "" && res.memoryCompatibleRows > 0 && partialExcluded > 0:
		// Partially migrated store: retrieval works, but silently on a
		// subset. That is exactly the state that reads as healthy.
		check.Status = "WARN"
		check.Message = fmt.Sprintf(
			"%d of %d embedded memories are excluded from vector matching "+
				"(different model or no fingerprint); semantic recall ranks only the "+
				"compatible subset",
			partialExcluded, res.memoryCompatibleRows+partialExcluded)
	case res.memoryActiveModel != "" && res.memoryCompatibleRows == 0 && res.memoryIncompatibleRows > 0:
		// Totally mismatched store: EVERY stored vector was produced by a
		// different model, so retrieval's compatibility filter excludes
		// all of them and vector matching returns nothing. Strictly
		// worse than the partial case above, which still ranks a subset.
		//
		// The partial-case rule (compatible > 0 AND excluded > 0) cannot
		// reach this state, so it used to fall through to the default
		// verdict — "N stored vectors, all well-formed and uniformly
		// D-dimensional", PASS — for a store whose semantic recall
		// cannot match a single row. The vectors ARE well-formed; they
		// are incomparable with the configured model, which is exactly
		// what the compatibility filter exists to prevent.
		check.Status = "WARN"
		check.Message = fmt.Sprintf(
			"0 of %d embedded memories are compatible with the active embedding model %q: "+
				"every one of them was produced by a different model and is excluded from "+
				"vector matching, so semantic recall cannot use them; re-embed with "+
				"`mpm ops backfill-embeddings --force-reembed`",
			res.memoryIncompatibleRows, res.memoryActiveModel)
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
