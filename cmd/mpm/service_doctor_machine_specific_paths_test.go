// service_doctor_machine_specific_paths_test.go — regression for the
// doctor-Database FAIL path.
//
// Background: before this fix, checkDatabase's FAIL branch returned
// a hardcoded "/home/v/.mpm/src/db/mpm.db" example path in its
// Details. On any host other than the original author's, that path
// did not exist, so the operator was instructed to "inspect" a
// non-existent file. The fix replaces the literal with s.dm.DBPath(),
// which is the canonical resolved path this doctor's *DatabaseManager
// is actually bound to — operators see a path that exists on their
// own machine.
//
// This test pins that contract: a doctor whose HealthCheck fails
// surfaces the resolved dm.DBPath() in its Details and never
// contains a hardcoded author-machine literal.

package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	internal "github.com/flowbyte-com/mpm-core"
)

// TestDoctorService_checkDatabase_FailSurfacesActualDBPath pins the
// "FAIL details must point at the real resolved DB path" contract.
// Pre-fix this returned a literal "/home/v/.mpm/src/db/mpm.db"
// example path regardless of which host the doctor was running on;
// post-fix the Details contain s.dm.DBPath() so the operator is told
// to inspect a path that actually exists on their machine.
//
// Uses internal.NewTestSharedDM(t) (not newTestDMForCmd) because the
// in-memory helper produces a DatabaseManager with an empty dbPath —
// the test needs a dm whose DBPath() resolves to a real on-disk
// location the doctor can surface in its Details.
func TestDoctorService_checkDatabase_FailSurfacesActualDBPath(t *testing.T) {
	dm := internal.NewTestSharedDM(t)

	// Resolve and remember the canonical DB path this dm is bound
	// to so we can assert the doctor surfaces the SAME value.
	want := dm.DBPath()
	require.NotEmpty(t, want, "test setup: dm.DBPath() must be resolvable")

	// Force HealthCheck to fail by closing the underlying DB. The
	// doctor's checkDatabase() takes the err != nil branch, which is
	// the exact branch whose Details message was the defect.
	require.NoError(t, dm.Close())

	svc := NewDoctorService(dm)
	check := svc.checkDatabase()

	// Sanity: we exercised the FAIL branch.
	require.Equal(t, "FAIL", check.Status, "expected FAIL status after closing the dm")
	require.NotEmpty(t, check.Details, "FAIL branch must surface actionable details")

	// Assertion 1: the resolved DB path is present.
	joined := strings.Join(check.Details, "\n")
	require.Contains(t, joined, want,
		"doctor FAIL Details must contain the actual dm.DBPath() (%q) so "+
			"operators see a path that exists on their host; got: %s",
		want, joined)

	// Assertion 2: no hardcoded author-machine literal remains.
	// The original defect was /home/v/.mpm/src/db/mpm.db; the
	// resolution is portable so we forbid that literal at the
	// output surface regardless of which host runs the doctor.
	const authorLiteral = "/home/v/"
	require.NotContains(t, joined, authorLiteral,
		"doctor FAIL Details must not hardcode an author-machine path; "+
			"got: %s", joined)
}

// TestDoctorService_checkDatabase_FailSurfacesDBPath_NotHardcodedRoot
// is a stricter sibling of the above: even if the resolved DB path
// happens to live under /home/v on the original author's host, the
// doctor's Details MUST contain the dm.DBPath() verbatim (not just
// any /home/v fragment). This protects against a regression where
// someone reverts the fix but only on a branch where the operator
// happens to be the original author.
func TestDoctorService_checkDatabase_FailSurfacesDBPath_NotHardcodedRoot(t *testing.T) {
	dm := internal.NewTestSharedDM(t)
	want := dm.DBPath()
	require.NotEmpty(t, want, "test setup: dm.DBPath() must be resolvable")
	require.NoError(t, dm.Close())

	svc := NewDoctorService(dm)
	check := svc.checkDatabase()

	joined := strings.Join(check.Details, "\n")
	require.True(t, strings.Contains(joined, want),
		"doctor FAIL Details must surface the verbatim dm.DBPath(); got %q want substring %q",
		joined, want)
}
