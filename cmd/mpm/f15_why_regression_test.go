// f15_why_regression_test.go — F15 regression.
//
// Audit finding F15: `mpm why` could not resolve work artifacts ("no
// artifact found ... probed N standard collections") and showed
// "(unknown)" timestamps on memories.
//
// Root causes:
//  1. detectKind only probed the `memories` table; works live in their
//     own first-class table, so every probe missed.
//  2. After the timestamps_unified_v1 migration the columns hold INTEGER
//     Unix-epoch seconds, which parseSQLiteTime (text formats only)
//     rejected → zero time → "(unknown)" display.
package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestF15_WhyResolvesWorkArtifact(t *testing.T) {
	dm := newTestDMForCmd(t)

	w, err := dm.AddWork("f15 work artifact", "work body content", "")
	require.NoError(t, err)

	svc := NewWhyService(dm)
	require.NotNil(t, svc)
	report, err := svc.Explain(w.ID)
	require.NoError(t, err)
	assert.Empty(t, report.SkipReason, "work artifact must resolve")
	assert.Equal(t, "work", report.ArtifactKind)
	require.NotNil(t, report.Identity)
	assert.Contains(t, report.Identity.Content, "work body content")

	// Timestamps must be real, not "(unknown)".
	require.NotNil(t, report.Provenance)
	assert.False(t, report.Provenance.CreatedAt.IsZero(),
		"created_at parsed from INTEGER epoch must render as a real timestamp")
}

func TestF15_WhyMemoryTimestampsFromIntegerEpoch(t *testing.T) {
	dm := newTestDMForCmd(t)

	id, err := dm.SaveMemory("memories", "f15 memory timestamp target", "", nil, nil, nil, false, 3)
	require.NoError(t, err)

	svc := NewWhyService(dm)
	report, err := svc.Explain(id)
	require.NoError(t, err)
	assert.Empty(t, report.SkipReason)
	require.NotNil(t, report.Provenance)
	assert.False(t, report.Provenance.CreatedAt.IsZero(),
		"memory created_at must parse from the INTEGER epoch column")
	assert.WithinDuration(t, time.Now(), report.Provenance.CreatedAt, 24*time.Hour)
}

func TestF15_WhyMissingReferenceExplicit(t *testing.T) {
	dm := newTestDMForCmd(t)

	svc := NewWhyService(dm)
	report, err := svc.Explain("no-such-artifact-f15")
	require.NoError(t, err)
	assert.NotEmpty(t, report.SkipReason,
		"a missing artifact must say so explicitly instead of rendering empty panels")
}
