// ops_rebalance_cmds.go — `mpm ops rebalance` engine-room command.
//
// Builds (or rebuilds) the IVF vector index. After this command runs,
// VectorMatch switches from the brute-force O(n) scan to the IVFSearch
// fast path (K + N/K * probe_p cosine comparisons instead of N).
//
// Idempotent. Safe to run repeatedly. The previous cluster/assignment
// rows are wiped in a single transaction before the new ones are
// inserted, so a crash mid-rebalance leaves the database in either the
// pre-rebalance state (clusters absent, brute force active) or the
// post-rebalance state (clusters fresh, IVF active). No half-applied
// state ever lands.
//
// Triggers: the next memory write after a rebalance will hit
// AssignToCluster and land in vector_assignments. Existing writes that
// happened between rebalance runs are picked up automatically by the
// rebalance itself (Rebalance reads all memories with non-null
// embeddings, not just recent ones).
//
// Operator's-eye view:
//   mpm ops rebalance                # run K-Means, write clusters
//   mpm ops rebalance --stats        # just report cluster variance; don't rebalance
//
// Implementation note: K-Means is in Go (internal/core/vector_index.go).
// No CGO, no parallel file. The whole index lives in two standard
// SQLite tables (vector_clusters, vector_assignments) that are part of
// the regular backup. The K-Means run is sub-second for typical N; the
// cost scales with N*K*iterations which is fine for the operator's
// "run it occasionally" cadence.

package main

import (
	"fmt"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/usererror"
)

// handleOpsRebalance runs `mpm ops rebalance [--stats]`.
//
// Without --stats: executes K-Means on all memories with non-null
// embeddings, writes the centroids and per-memory cluster assignments,
// and reports the cluster-quality diagnostics (per-cluster variance,
// iteration count, convergence flag).
//
// With --stats: reports the current cluster state without re-running
// K-Means. Useful for checking whether a rebalance is overdue
// (high variance = some clusters absorbing too much).
func handleOpsRebalance(args []string) int {
	statsOnly := false
	for _, a := range args {
		if a == "--stats" {
			statsOnly = true
		} else if a == "--help" || a == "-h" || a == "help" {
			// router.parseFlags rewrites --help to "help" globally before
			// dispatch, so the rewritten form is what reaches us.
			fmt.Println("Usage: mpm ops rebalance [--stats]")
			fmt.Println("")
			fmt.Println("Builds the IVF vector index by running K-Means on memory embeddings.")
			fmt.Println("After this, VectorMatch switches from brute-force O(n) scan to IVF")
			fmt.Println("(K + N/K * probe_p comparisons per query).")
			fmt.Println("")
			fmt.Println("Flags:")
			fmt.Println("  --stats   Report current cluster state without re-running K-Means")
			return 0
		}
	}

	dm := getDBConcrete()
	if dm == nil {
		usererror.Error("could not open database")
		return 1
	}

	if statsOnly {
		return rebalanceStats(dm)
	}

	fmt.Println("⚡ Running K-Means vector index rebalance...")
	fmt.Println("   This may take a few seconds for large corpora.")

	result, n, err := mpminternal.Rebalance(dm.SQLDB(), "")
	if err != nil {
		usererror.Error("rebalance failed: %v", err)
		return 1
	}

	fmt.Printf("   ✓ Rebuilt vector_clusters (%d clusters) and vector_assignments (%d memories)\n\n",
		len(result.Centroids), n)
	fmt.Printf("   Converged: %v (after %d iterations)\n", result.Converged, result.Iterations)

	// Report per-cluster variance distribution so the operator can see
	// cluster quality. High variance in one cluster = that cluster
	// is absorbing too much and the K choice may need adjustment.
	maxVar, sumVar := 0.0, 0.0
	for _, v := range result.Variances {
		if v > maxVar {
			maxVar = v
		}
		sumVar += v
	}
	if len(result.Variances) > 0 {
		avgVar := sumVar / float64(len(result.Variances))
		fmt.Printf("   Variance: avg=%.4f, max=%.4f (lower = tighter clusters)\n", avgVar, maxVar)
		if maxVar > avgVar*3 {
			fmt.Println("   ⚠ High variance cluster detected — consider rebalance with --probe-p higher,")
			fmt.Println("     or check for outlier embeddings (e.g., mismatched embedding model).")
		}
	}
	fmt.Println("\n✓ IVF index live. VectorMatch now uses IVFSearch (fast path).")
	return 0
}

// rebalanceStats reports the current cluster state without re-running K-Means.
func rebalanceStats(dm *mpminternal.DatabaseManager) int {
	clusters, err := mpminternal.LoadVectorCentroids(dm.SQLDB(), "")
	if err != nil {
		usererror.Error("failed to load centroids: %v", err)
		return 1
	}
	if len(clusters) == 0 {
		fmt.Println("No IVF clusters exist. Run `mpm ops rebalance` to build the index.")
		fmt.Println("VectorMatch is currently using the brute-force O(n) path with MPM_MAX_VECTOR_SCAN cap.")
		return 0
	}

	fmt.Printf("IVF cluster state (%d clusters):\n\n", len(clusters))
	fmt.Printf("  %-8s  %-12s  %-12s\n", "ID", "n_vectors", "variance")
	fmt.Printf("  %-8s  %-12s  %-12s\n", "--", "--------", "--------")

	maxVar, sumVar := 0.0, 0.0
	totalVecs := 0
	for _, c := range clusters {
		fmt.Printf("  %-8d  %-12d  %-12.4f\n", c.ClusterID, c.NVectors, c.Variance)
		if c.Variance > maxVar {
			maxVar = c.Variance
		}
		sumVar += c.Variance
		totalVecs += c.NVectors
	}

	fmt.Printf("\n  Total vectors assigned: %d\n", totalVecs)
	if len(clusters) > 0 {
		avgVar := sumVar / float64(len(clusters))
		fmt.Printf("  Variance: avg=%.4f, max=%.4f\n", avgVar, maxVar)
		if maxVar > avgVar*3 {
			fmt.Println("\n  ⚠ High variance cluster — rebalance overdue.")
		}
	}
	return 0
}