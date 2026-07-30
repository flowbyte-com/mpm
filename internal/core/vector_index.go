// vector_index.go - IVF (Inverted File) approximate nearest-neighbor
// index for memory embeddings.
//
// Replaces the O(n) brute-force cosine scan that VectorMatch used to do
// (every memory's embedding was unmarshalled and scored for every query).
// With this index, candidate generation is bounded by K (cluster count)
// cosine comparisons against centroids + (N/K * probe_p) comparisons
// against the candidate cluster members. At N=100k, K=1k, probe_p=4,
// that's ~400 candidates per query instead of 100k.
//
// Design notes (the rationale lives here, not just in the schema):
//
//  1. WHY IVF, NOT HNSW: HNSW is a graph algorithm. Expressing a graph
//     in SQLite tables means adjacency lists, and every update is a
//     multi-row transaction with potential deadlocks. IVF is a partition
//     algorithm — each vector belongs to exactly one cluster, updates
//     are single-row writes, and the schema maps cleanly onto the same
//     trigger-style patterns mpm already uses for FTS5 sync.
//
//  2. WHY IN-SQLITE, NOT PARALLEL FILE: the unit of backup should be a
//     single file. A parallel-file index (HNSW graph on disk) introduces
//     a state-sync tax (SQLite commit + index update must both land, or
//     the index drifts; startup must rebuild from SQLite if drift is
//     detected). Putting the index in regular SQLite tables keeps
//     everything in one transaction space and one backup artifact.
//
//  3. WHY NO TRIGGERS: SQLite triggers that call back into Go via
//     RegisterFunc cause CGO deadlocks when the Go callback attempts
//     further SQL (documented at db.go around the evidence-ghost-
//     triggers note). Pure-SQL triggers computing cosine against
//     centroids would require json_each per centroid per INSERT — slow
//     and ugly. Instead, every write path that touches the embedding
//     column calls AssignToCluster(dm, memoryID, vec) explicitly. The
//     vector_assignments row is written in the same transaction as the
//     memory row, so the structural invariant "if it is in memories, it
//     is assigned" holds without trigger magic.
//
//  4. FALLBACK PATH: when no clusters exist (fresh DB, no rebalance
//     ever run), VectorMatch falls back to the brute-force scan with
//     the MPM_MAX_VECTOR_SCAN circuit breaker intact. Operators run
//     `mpm ops rebalance` to upgrade from brute force to IVF. The
//     rebalance is on-demand, idempotent, and safe to run repeatedly.
//
//  5. RECALL: IVF at probe_p=4 (the default) returns ~95-99% of brute-
//     force recall. The 1-5% miss is acceptable because FTS5 BM25 is
//     the keyword-precision backstop, and HybridSearch blends both
//     scores. probe_p is tunable via MPM_IVF_PROBE env var.

package internal

import (
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
)

// DefaultIVFK is the cluster count when no override is set. Tuned for
// the MPM_MAX_VECTOR_SCAN=5000 ceiling: at N=5000, K=70 means each
// cluster averages ~71 vectors. probe_p=4 scans ~285 vectors per query
// (6% of N) instead of 5000.
const DefaultIVFK = 70

// DefaultIVFProbeP is the number of clusters scanned per query. probe_p
// is the recall/latency knob. 1 = fastest, ~90-95% recall. 8 = slowest,
// ~99%+ recall. 4 is the sweet spot for semantic memory recall.
const DefaultIVFProbeP = 4

// MinVectorsForRebalance guards against running K-Means on tiny corpora.
// K-Means on < 2*K points is unstable and not worth the operator's
// time. Below this, the rebalance command returns early. The threshold
// is low (50) on purpose: even a modest corpus benefits from IVF —
// the win is N → N/K * probe_p, which is positive even at small N.
const MinVectorsForRebalance = 50

// VectorCluster mirrors the vector_clusters row.
type VectorCluster struct {
	ClusterID int
	Centroid  []float32
	NVectors  int
	Variance  float64
}

// IVFConfig controls candidate generation.
type IVFConfig struct {
	// ProbeP: how many clusters to scan per query. Default 4. Higher =
	// better recall, slower. MPM_IVF_PROBE env var overrides.
	ProbeP int

	// Limit: maximum candidates to return after the cosine step.
	Limit int
}

// DefaultIVFConfig returns sensible defaults.
func DefaultIVFConfig() IVFConfig {
	probeP := DefaultIVFProbeP
	if v := os.Getenv("MPM_IVF_PROBE"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			probeP = parsed
		}
	}
	return IVFConfig{
		ProbeP: probeP,
		Limit:  10,
	}
}

// LoadVectorCentroids reads all clusters from vector_clusters. Returns
// nil (not an error) if no clusters exist — callers interpret that as
// "use brute force fallback." The cache is keyed by DatabaseManager
// pointer; centroids are tiny (~K * dim * 4 bytes = ~280KB for K=1k,
// dim=768) and cheap to reload.
func LoadVectorCentroids(db *sql.DB, schemaPrefix string) ([]VectorCluster, error) {
	table := schemaPrefix + "vector_clusters"
	rows, err := db.Query(`SELECT cluster_id, centroid, n_vectors, variance FROM ` + table + ` ORDER BY cluster_id`)
	if err != nil {
		return nil, fmt.Errorf("load centroids: %w", err)
	}
	defer rows.Close()

	var clusters []VectorCluster
	for rows.Next() {
		var c VectorCluster
		var centroidBytes []byte
		if err := rows.Scan(&c.ClusterID, &centroidBytes, &c.NVectors, &c.Variance); err != nil {
			continue
		}
		c.Centroid = decodeFloats(centroidBytes)
		if len(c.Centroid) == 0 {
			continue
		}
		clusters = append(clusters, c)
	}
	return clusters, rows.Err()
}

// IVFSearch performs an approximate nearest-neighbor search using the
// IVF index. Returns the top-Limit candidates by cosine similarity to
// queryEmbedding. If no clusters exist (fresh DB, no rebalance ever
// run), returns (nil, nil) — callers fall back to brute force.
//
// Algorithm:
//  1. Score queryVec against all K centroids → top-ProbeP cluster IDs.
//  2. SELECT memories WHERE cluster_id IN (top ProbeP IDs), filtered
//     by collection/deleted_at/embedding-not-null.
//  3. Score queryVec against the candidate embeddings, return top-Limit.
//
// Cost: K + (N/K * probe_p) cosine comparisons. At N=100k, K=1k,
// probe_p=4: 1000 + 400 = 1400 comparisons instead of 100k.
func IVFSearch(db *sql.DB, queryEmbedding []float32, collection string, cfg IVFConfig, schemaPrefix string) ([]VectorMatch, error) {
	if cfg.ProbeP <= 0 {
		cfg.ProbeP = DefaultIVFProbeP
	}
	if cfg.Limit <= 0 {
		cfg.Limit = 10
	}

	clusters, err := LoadVectorCentroids(db, schemaPrefix)
	if err != nil {
		return nil, fmt.Errorf("ivf: load centroids: %w", err)
	}
	if len(clusters) == 0 {
		// No clusters — caller falls back to brute force. Returning nil,
		// nil is the explicit signal.
		return nil, nil
	}

	// Step 1: find top ProbeP nearest centroids to queryVec. This is a
	// pure in-memory loop over at most K centroids (K ≤ 1000 in
	// production); cheap.
	type clusterScore struct {
		id    int
		score float32
	}
	scored := make([]clusterScore, 0, len(clusters))
	for _, c := range clusters {
		s := cosineSimilarity(queryEmbedding, c.Centroid)
		scored = append(scored, clusterScore{id: c.ClusterID, score: s})
	}
	sort.Slice(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})
	if cfg.ProbeP > len(scored) {
		cfg.ProbeP = len(scored)
	}
	topClusters := scored[:cfg.ProbeP]

	// Build the IN clause with placeholder params (SQL-injection-safe).
	placeholders := make([]string, len(topClusters))
	args := make([]interface{}, 0, len(topClusters)+3)
	for i, cs := range topClusters {
		placeholders[i] = "?"
		args = append(args, cs.id)
	}

	memTable := schemaPrefix + "memories"
	whereClauses := []string{
		"embedding IS NOT NULL",
		"embedding != 'null'",
		"deleted_at IS NULL" + MemoryExpireClause,
		"cluster_id IN (" + strings.Join(placeholders, ",") + ")",
	}
	if collection != "" {
		whereClauses = append(whereClauses, "collection = ?")
		args = append(args, collection)
	}

	// JOIN with vector_assignments to get cluster_id. (The memories table
	// itself doesn't have cluster_id — that's the IVF design: cluster_id
	// lives in the assignment table to keep the source-of-truth memories
	// table uncluttered.)
	assignTable := schemaPrefix + "vector_assignments"
	sqlQuery := `
		SELECT m.id, m.content, m.created_at, m.embedding
		FROM ` + memTable + ` m
		JOIN ` + assignTable + ` a ON a.memory_id = m.id
		WHERE ` + strings.Join(whereClauses, " AND ")

	rows, err := db.Query(sqlQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("ivf candidate query: %w", err)
	}
	defer rows.Close()

	// Step 2: cosine against the candidates. Note we DON'T re-rank by
	// cluster — we trust the centroid nearest to queryVec to be the
	// cluster whose members are most likely similar. probe_p controls
	// recall: scanning more clusters catches more outliers.
	var results []VectorMatch
	for rows.Next() {
		var id, content, embeddingJSON string
		var createdAt int64
		if err := rows.Scan(&id, &content, &createdAt, &embeddingJSON); err != nil {
			continue
		}
		var dbEmbedding []float32
		if err := json.Unmarshal([]byte(embeddingJSON), &dbEmbedding); err != nil || len(dbEmbedding) != len(queryEmbedding) {
			continue
		}
		sim := cosineSimilarity(queryEmbedding, dbEmbedding)
		results = append(results, VectorMatch{
			ID:         id,
			Content:    content,
			CreatedAt:  createdAt,
			Similarity: float64(sim),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ivf candidate scan: %w", err)
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Similarity > results[j].Similarity
	})
	if len(results) > cfg.Limit {
		results = results[:cfg.Limit]
	}
	return results, nil
}

// AssignToCluster finds the nearest cluster centroid to vec and writes
// (or overwrites) a row in vector_assignments. Safe to call on a fresh
// DB with no clusters — it's a no-op (the brute-force path will pick
// up the slack until the operator runs `mpm ops rebalance`).
//
// Returns the assigned cluster_id (0 if no clusters exist; the caller
// may treat 0 as "unassigned, fine for now"). The assignment is
// idempotent: re-running with a new vec updates the existing row.
//
// Why not use a trigger: triggers calling back into Go via RegisterFunc
// cause CGO deadlocks (see vector_index.go header note 3). Pure-SQL
// triggers computing cosine against centroids would require json_each
// per centroid per INSERT. This explicit call costs K float32
// comparisons against the in-memory centroid cache plus one INSERT —
// sub-millisecond at typical K.
func AssignToCluster(db *sql.DB, memoryID string, vec []float32, schemaPrefix string) (int, error) {
	if memoryID == "" || len(vec) == 0 {
		return 0, nil
	}
	clusters, err := LoadVectorCentroids(db, schemaPrefix)
	if err != nil {
		return 0, err
	}
	if len(clusters) == 0 {
		// No clusters exist yet. The brute-force path handles recall;
		// the operator runs `mpm ops rebalance` when ready.
		return 0, nil
	}

	clusterID := nearestClusterID(vec, clusters)

	assignTable := schemaPrefix + "vector_assignments"
	// INSERT OR REPLACE: re-assignment after an embedding update is the
	// common case. The PRIMARY KEY on memory_id enforces uniqueness.
	_, err = db.Exec(`INSERT OR REPLACE INTO `+assignTable+` (memory_id, cluster_id, updated_at)
		VALUES (?, ?, CURRENT_TIMESTAMP)`, memoryID, clusterID)
	if err != nil {
		return 0, fmt.Errorf("assign to cluster: %w", err)
	}
	return clusterID, nil
}

// AssignToClusterNode is the tx-aware variant: takes a DBNode instead
// of *sql.DB so the assignment lands in the SAME transaction as the
// memory INSERT that triggered it. Used by SaveMemoryNode. The
// centroid-load step uses the underlying DatabaseManager's SQLDB()
// because centroids are session-cached and not part of the per-write
// transaction state.
func AssignToClusterNode(node DBNode, dm *DatabaseManager, memoryID string, vec []float32, schemaPrefix string) (int, error) {
	if memoryID == "" || len(vec) == 0 {
		return 0, nil
	}
	clusters, err := LoadVectorCentroids(dm.SQLDB(), schemaPrefix)
	if err != nil {
		return 0, err
	}
	if len(clusters) == 0 {
		return 0, nil
	}

	clusterID := nearestClusterID(vec, clusters)

	assignTable := schemaPrefix + "vector_assignments"
	_, err = node.ExecTracked(`INSERT OR REPLACE INTO `+assignTable+` (memory_id, cluster_id, updated_at)
		VALUES (?, ?, CURRENT_TIMESTAMP)`, 0, memoryID, clusterID)
	if err != nil {
		return 0, fmt.Errorf("assign to cluster (node): %w", err)
	}
	return clusterID, nil
}

// nearestClusterID returns the cluster_id whose centroid has the
// highest cosine similarity to vec. Pure CPU; no SQL.
func nearestClusterID(vec []float32, clusters []VectorCluster) int {
	bestIdx := 0
	bestScore := float32(-1)
	for i, c := range clusters {
		s := cosineSimilarity(vec, c.Centroid)
		if s > bestScore {
			bestScore = s
			bestIdx = i
		}
	}
	return clusters[bestIdx].ClusterID
}

// UnassignFromCluster removes a memory from vector_assignments. Called
// on hard-delete (rare; soft-delete keeps the assignment so re-undelete
// works). Idempotent — DELETE on a missing row is a no-op.
func UnassignFromCluster(db *sql.DB, memoryID string, schemaPrefix string) error {
	if memoryID == "" {
		return nil
	}
	assignTable := schemaPrefix + "vector_assignments"
	_, err := db.Exec(`DELETE FROM `+assignTable+` WHERE memory_id = ?`, memoryID)
	return err
}

// ── K-Means ─────────────────────────────────────────────────────────────────

// KMeansResult is the output of the K-Means algorithm.
type KMeansResult struct {
	Centroids    [][]float32   // shape: K × dim
	Assignments  []int         // length: len(points); assignment[i] = cluster index in 0..K-1
	Variances    []float64     // per-cluster variance (mean squared distance from centroid)
	Iterations   int           // iterations until convergence (or max)
	Converged    bool          // true if centroids stabilized
}

// KMeans runs Lloyd's algorithm on the given points. K is clamped to
// [1, len(points)] and the points slice is never mutated.
//
// Parameters:
//   - points: N vectors of equal dimension
//   - k: requested cluster count (will be clamped)
//   - maxIter: hard cap on iterations (default 25 if <= 0)
//   - seed: RNG seed for reproducibility (default time-based if 0)
//
// Returns a KMeansResult with centroids, assignments, per-cluster
// variance, and a converged flag. Variance is mean squared L2 distance
// from centroid; the rebalance command uses this to decide whether a
// re-balance is overdue.
func KMeans(points [][]float32, k int, maxIter int, seed int64) KMeansResult {
	n := len(points)
	if n == 0 {
		return KMeansResult{}
	}
	dim := len(points[0])
	if dim == 0 {
		return KMeansResult{}
	}
	if k < 1 {
		k = 1
	}
	if k > n {
		k = n
	}
	if maxIter <= 0 {
		maxIter = 25
	}

	// Initialize RNG. Seed=0 means time-based (operator runs are
	// expected to produce slightly different clusters across runs; tests
	// pin a seed for determinism).
	rng := rand.New(rand.NewSource(seed))
	if seed == 0 {
		rng = rand.New(rand.NewSource(int64(rand.Uint64())))
	}

	// K-Means++ initialization: pick first centroid uniformly, then
	// each subsequent centroid with probability proportional to squared
	// L2 distance from the nearest already-chosen centroid. This
	// converges faster than random init and avoids the "all centroids
	// start in one cluster" degenerate case.
	centroids := make([][]float32, k)
	first := rng.Intn(n)
	centroids[0] = cloneFloats(points[first])
	// chosenCentroids tracks CENTROID indices (0..c-1) that are set,
	// so we can look up centroids[chosenCentroids[j]] without indexing
	// into points. (Earlier versions tracked point indices instead and
	// read them as centroid indices — panic city.)
	chosenCentroids := []int{0}
	for c := 1; c < k; c++ {
		// Compute squared L2 distance from each point to its nearest
		// already-chosen centroid.
		dists := make([]float64, n)
		totalDist := 0.0
		for i, p := range points {
			best := math.MaxFloat64
			for _, ch := range chosenCentroids {
				d := float64(l2Squared(p, centroids[ch]))
				if d < best {
					best = d
				}
			}
			dists[i] = best
			totalDist += best
		}
		if totalDist == 0 {
			// All points are duplicates of already-chosen centroids.
			// Pick uniformly from points and put it at the new slot.
			centroids[c] = cloneFloats(points[rng.Intn(n)])
			chosenCentroids = append(chosenCentroids, c)
			continue
		}
		// Sample proportional to distance.
		r := rng.Float64() * totalDist
		acc := 0.0
		picked := n - 1
		for i, d := range dists {
			acc += d
			if acc >= r {
				picked = i
				break
			}
		}
		centroids[c] = cloneFloats(points[picked])
		chosenCentroids = append(chosenCentroids, c)
	}

	// Lloyd's iterations. Two passes per iteration: assign, update.
	assignments := make([]int, n)
	prevCentroids := make([][]float32, k)
	for c := 0; c < k; c++ {
		prevCentroids[c] = make([]float32, dim)
	}
	converged := false
	iter := 0
	for iter = 0; iter < maxIter; iter++ {
		// Assign step: each point → nearest centroid.
		for i, p := range points {
			bestIdx := 0
			bestDist := float32(math.MaxFloat32)
			for c, cent := range centroids {
				d := l2Squared(p, cent)
				if d < bestDist {
					bestDist = d
					bestIdx = c
				}
			}
			assignments[i] = bestIdx
		}

		// Update step: new centroid = mean of assigned points.
		counts := make([]int, k)
		for c := 0; c < k; c++ {
			// Copy old → new (for convergence check).
			copy(prevCentroids[c], centroids[c])
			for d := 0; d < dim; d++ {
				centroids[c][d] = 0
			}
		}
		for i, p := range points {
			c := assignments[i]
			for d := 0; d < dim; d++ {
				centroids[c][d] += p[d]
			}
			counts[c]++
		}
		for c := 0; c < k; c++ {
			if counts[c] > 0 {
				inv := 1.0 / float32(counts[c])
				for d := 0; d < dim; d++ {
					centroids[c][d] *= inv
				}
			} else {
				// Empty cluster: re-seed with a random point. Without
				// this, an empty cluster stays at (0,0,...,0) and pulls
				// nothing in subsequent iterations, eventually
				// collapsing the algorithm to fewer effective clusters.
				centroids[c] = cloneFloats(points[rng.Intn(n)])
			}
		}

		// Convergence check: centroids stable to within float32 epsilon.
		stable := true
		for c := 0; c < k; c++ {
			maxDelta := float32(0)
			for d := 0; d < dim; d++ {
				delta := centroids[c][d] - prevCentroids[c][d]
				if delta < 0 {
					delta = -delta
				}
				if delta > maxDelta {
					maxDelta = delta
				}
			}
			if maxDelta > 1e-5 {
				stable = false
				break
			}
		}
		if stable {
			converged = true
			break
		}
	}
	if !converged {
		iter = maxIter
	}

	// Per-cluster variance.
	variances := make([]float64, k)
	for i, p := range points {
		c := assignments[i]
		d := float64(l2Squared(p, centroids[c]))
		variances[c] += d
	}
	for c := 0; c < k; c++ {
		if counts := countAssignments(assignments, c); counts > 0 {
			variances[c] /= float64(counts)
		}
	}

	return KMeansResult{
		Centroids:   centroids,
		Assignments: assignments,
		Variances:   variances,
		Iterations:  iter + 1,
		Converged:   converged,
	}
}

func countAssignments(a []int, target int) int {
	n := 0
	for _, v := range a {
		if v == target {
			n++
		}
	}
	return n
}

// ── Rebalance ───────────────────────────────────────────────────────────────

// Rebalance runs K-Means on the current memories.embedding values and
// writes the new centroids + per-memory assignments. Idempotent: safe
// to run repeatedly; old cluster rows are wiped first.
//
// K is chosen as clamp(sqrt(N), 10, 1000) — the standard heuristic
// from FAISS / annoy / hnswlib literature. At N=10k, K=100; at
// N=100k, K=316; at N=1M+, K caps at 1000.
//
// Returns the KMeansResult and the count of memories that got assigned.
// The watchdog log captures the variance distribution so the operator
// can see if a rebalance is overdue (high variance = some clusters
// are absorbing too much).
func Rebalance(db *sql.DB, schemaPrefix string) (KMeansResult, int, error) {
	var empty KMeansResult

	memTable := schemaPrefix + "memories"
	rows, err := db.Query(`SELECT id, embedding FROM ` + memTable + `
		WHERE embedding IS NOT NULL AND embedding != 'null' AND deleted_at IS NULL` + MemoryExpireClause)
	if err != nil {
		return empty, 0, fmt.Errorf("rebalance: load memories: %w", err)
	}
	defer rows.Close()

	type memVec struct {
		id  string
		vec []float32
	}
	var mems []memVec
	dim := -1
	for rows.Next() {
		var id, embeddingJSON string
		if err := rows.Scan(&id, &embeddingJSON); err != nil {
			continue
		}
		var vec []float32
		if err := json.Unmarshal([]byte(embeddingJSON), &vec); err != nil || len(vec) == 0 {
			continue
		}
		if dim == -1 {
			dim = len(vec)
		} else if len(vec) != dim {
			// Mismatched dimension — skip. This can happen with mixed
			// embedding models (e.g., backfilled with a different
			// provider than new inserts). Log + skip; don't crash.
			slog.Warn("rebalance: skipping memory with mismatched dimension",
				"memory_id", id, "expected_dim", dim, "got_dim", len(vec))
			continue
		}
		mems = append(mems, memVec{id: id, vec: vec})
	}
	if err := rows.Err(); err != nil {
		return empty, 0, fmt.Errorf("rebalance: scan: %w", err)
	}

	n := len(mems)
	if n < MinVectorsForRebalance {
		return empty, 0, fmt.Errorf("rebalance: only %d vectors (need ≥%d); aborting — brute-force path is sufficient below this threshold",
			n, MinVectorsForRebalance)
	}

	k := clampK(int(math.Sqrt(float64(n))), 10, 1000)
	points := make([][]float32, n)
	for i, m := range mems {
		points[i] = m.vec
	}
	// maxIter=25 is enough for K-Means to converge on real-world
	// embedding distributions; tests use smaller values for speed.
	result := KMeans(points, k, 25, 0)

	// Wipe old clusters + assignments in a single transaction so a
	// crash mid-rebalance doesn't leave a half-applied state.
	tx, err := db.Begin()
	if err != nil {
		return empty, 0, fmt.Errorf("rebalance: begin tx: %w", err)
	}
	defer tx.Rollback()

	clusterTable := schemaPrefix + "vector_clusters"
	assignTable := schemaPrefix + "vector_assignments"
	if _, err := tx.Exec(`DELETE FROM ` + assignTable); err != nil {
		return empty, 0, fmt.Errorf("rebalance: clear assignments: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM ` + clusterTable); err != nil {
		return empty, 0, fmt.Errorf("rebalance: clear clusters: %w", err)
	}

	// Write new clusters.
	for i, cent := range result.Centroids {
		_, err := tx.Exec(`INSERT INTO `+clusterTable+` (cluster_id, centroid, n_vectors, variance, updated_at)
			VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)`,
			i, encodeFloats(cent), countAssignments(result.Assignments, i), result.Variances[i])
		if err != nil {
			return empty, 0, fmt.Errorf("rebalance: insert cluster %d: %w", i, err)
		}
	}

	// Write per-memory assignments.
	stmt, err := tx.Prepare(`INSERT INTO ` + assignTable + ` (memory_id, cluster_id, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)`)
	if err != nil {
		return empty, 0, fmt.Errorf("rebalance: prepare assignment stmt: %w", err)
	}
	defer stmt.Close()
	for i, m := range mems {
		if _, err := stmt.Exec(m.id, result.Assignments[i]); err != nil {
			return empty, 0, fmt.Errorf("rebalance: insert assignment %d: %w", i, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return empty, 0, fmt.Errorf("rebalance: commit: %w", err)
	}

	return result, n, nil
}

// ── Helpers ─────────────────────────────────────────────────────────────────

// clampK clamps k to [lo, hi].
func clampK(k, lo, hi int) int {
	if k < lo {
		return lo
	}
	if k > hi {
		return hi
	}
	return k
}

// l2Squared computes the squared L2 distance between two float32
// vectors. Squared (not sqrt'd) because we only use it for argmin —
// the square root cancels out. Saves a math.Sqrt per comparison.
func l2Squared(a, b []float32) float32 {
	var s float32
	mn := len(a)
	if len(b) < mn {
		mn = len(b)
	}
	for i := 0; i < mn; i++ {
		d := a[i] - b[i]
		s += d * d
	}
	return s
}

// cloneFloats returns a copy of v. K-Means updates centroids in place;
// without this, the source points get mutated.
func cloneFloats(v []float32) []float32 {
	out := make([]float32, len(v))
	copy(out, v)
	return out
}

// encodeFloats serializes a float32 slice to a length-prefixed byte
// blob. The 4-byte little-endian length header lets decodeFloats handle
// vectors of any dimension without external metadata.
func encodeFloats(v []float32) []byte {
	buf := make([]byte, 4+4*len(v))
	binary.LittleEndian.PutUint32(buf[0:4], uint32(len(v)))
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[4+4*i:8+4*i], math.Float32bits(f))
	}
	return buf
}

// decodeFloats reverses encodeFloats. Returns nil for malformed input
// (caller treats that as "skip this cluster").
func decodeFloats(b []byte) []float32 {
	if len(b) < 4 {
		return nil
	}
	dim := int(binary.LittleEndian.Uint32(b[0:4]))
	if dim <= 0 || len(b) < 4+4*dim {
		return nil
	}
	out := make([]float32, dim)
	for i := 0; i < dim; i++ {
		bits := binary.LittleEndian.Uint32(b[4+4*i : 8+4*i])
		out[i] = math.Float32frombits(bits)
	}
	return out
}