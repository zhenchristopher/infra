package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	rootBuild   = "00000000-0000-0000-0000-000000000001"
	sharedBuild = "00000000-0000-0000-0000-000000000002"
	orphanBuild = "00000000-0000-0000-0000-000000000003"
)

func TestComputeReachablePreservesSharedLayersAndCycles(t *testing.T) {
	t.Parallel()
	edges := map[string][]string{
		rootBuild:   {sharedBuild},
		sharedBuild: {rootBuild},
	}
	reachable, err := computeReachable(t.Context(), []string{rootBuild}, 10, func(_ context.Context, buildID string) ([]string, error) {
		return edges[buildID], nil
	})
	require.NoError(t, err)
	assert.Equal(t, map[string]struct{}{rootBuild: {}, sharedBuild: {}}, reachable)
}

func TestComputeReachableFailsClosedOnUnreadableHeader(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("corrupt header")
	_, err := computeReachable(t.Context(), []string{rootBuild}, 10, func(_ context.Context, _ string) ([]string, error) {
		return nil, sentinel
	})
	require.ErrorIs(t, err, sentinel)
}

func TestComputeReachableEnforcesGraphBound(t *testing.T) {
	t.Parallel()
	_, err := computeReachable(t.Context(), []string{rootBuild}, 1, func(_ context.Context, _ string) ([]string, error) {
		return []string{sharedBuild}, nil
	})
	require.ErrorContains(t, err, "reachable build count exceeded limit")
}

func TestMakePlanRequiresRetentionAndUnreferencedAgeBeforeDelete(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 18, 0, 0, 0, 0, time.UTC)
	scanned := inventory{
		builds: map[string]buildState{
			rootBuild:   {newestUpdated: now.Add(-90 * 24 * time.Hour)},
			sharedBuild: {newestUpdated: now.Add(-90 * 24 * time.Hour)},
			orphanBuild: {newestUpdated: now.Add(-8 * 24 * time.Hour)},
		},
		markers: map[string]markerState{
			orphanBuild: {created: now.Add(-2 * 24 * time.Hour)},
		},
	}
	reachable := map[string]struct{}{rootBuild: {}, sharedBuild: {}}
	planned := makePlan(now, minimumRetention, minimumUnreferencedAge, scanned, reachable)
	assert.Equal(t, []action{{buildID: orphanBuild, kind: actionDelete}}, planned.actions)
}

func TestMakePlanKeepsRecentUnreferencedBuild(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 18, 0, 0, 0, 0, time.UTC)
	scanned := inventory{
		builds: map[string]buildState{
			orphanBuild: {newestUpdated: now.Add(-6 * 24 * time.Hour)},
		},
		markers: map[string]markerState{
			orphanBuild: {created: now.Add(-2 * 24 * time.Hour)},
		},
	}
	planned := makePlan(now, minimumRetention, minimumUnreferencedAge, scanned, map[string]struct{}{})
	assert.Empty(t, planned.actions)
}

func TestMakePlanMarksNewCandidateWithoutDeleting(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 18, 0, 0, 0, 0, time.UTC)
	scanned := inventory{
		builds:  map[string]buildState{orphanBuild: {newestUpdated: now.Add(-365 * 24 * time.Hour)}},
		markers: map[string]markerState{},
	}
	planned := makePlan(now, minimumRetention, minimumUnreferencedAge, scanned, map[string]struct{}{})
	assert.Equal(t, []action{{buildID: orphanBuild, kind: actionMark}}, planned.actions)
}

func TestMakePlanResetsGraceWhenCandidateChanges(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 18, 0, 0, 0, 0, time.UTC)
	markerCreated := now.Add(-2 * 24 * time.Hour)
	scanned := inventory{
		builds: map[string]buildState{
			orphanBuild: {newestUpdated: markerCreated.Add(time.Hour)},
		},
		markers: map[string]markerState{
			orphanBuild: {created: markerCreated},
		},
	}
	planned := makePlan(now, minimumRetention, minimumUnreferencedAge, scanned, map[string]struct{}{})
	assert.Equal(t, []action{{buildID: orphanBuild, kind: actionReset}}, planned.actions)
}

func TestMakePlanRemovesMarkerWhenBuildBecomesReachable(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 18, 0, 0, 0, 0, time.UTC)
	scanned := inventory{
		builds: map[string]buildState{
			sharedBuild: {newestUpdated: now.Add(-90 * 24 * time.Hour)},
		},
		markers: map[string]markerState{
			sharedBuild: {created: now.Add(-31 * 24 * time.Hour)},
		},
	}
	planned := makePlan(now, minimumRetention, minimumUnreferencedAge, scanned, map[string]struct{}{sharedBuild: {}})
	assert.Equal(t, []action{{buildID: sharedBuild, kind: actionUnmark}}, planned.actions)
}

func TestValidateApplySafetyRejectsEmptyOrUndersizedRoots(t *testing.T) {
	t.Parallel()
	builds := map[string]buildState{rootBuild: {}, sharedBuild: {}}
	reachable := map[string]struct{}{rootBuild: {}}
	require.ErrorContains(t, validateApplySafety(nil, builds, reachable, 1, 0.1), "root manifest has 0 builds")
	require.ErrorContains(t, validateApplySafety([]string{rootBuild}, builds, reachable, 2, 0.1), "below minimum 2")
}

func TestValidateApplySafetyRejectsMissingStoredRootsAndCoverageCollapse(t *testing.T) {
	t.Parallel()
	missingRoot := "00000000-0000-0000-0000-000000000004"
	builds := map[string]buildState{rootBuild: {}, sharedBuild: {}, orphanBuild: {}}
	require.ErrorContains(
		t,
		validateApplySafety([]string{missingRoot}, builds, map[string]struct{}{missingRoot: {}}, 1, 0.1),
		"only 0 roots have stored objects",
	)
	require.ErrorContains(
		t,
		validateApplySafety([]string{rootBuild}, builds, map[string]struct{}{rootBuild: {}}, 1, 0.5),
		"below minimum ratio",
	)
}

func TestValidateApplySafetyAcceptsGroundedReachability(t *testing.T) {
	t.Parallel()
	builds := map[string]buildState{rootBuild: {}, sharedBuild: {}, orphanBuild: {}}
	reachable := map[string]struct{}{rootBuild: {}, sharedBuild: {}}
	require.NoError(t, validateApplySafety([]string{rootBuild}, builds, reachable, 1, 0.5))
}

func TestObjectAndMarkerBuildIDRejectNonBuildPrefixes(t *testing.T) {
	t.Parallel()
	_, ok := objectBuildID("metadata/file")
	assert.False(t, ok)
	id, ok := objectBuildID(rootBuild + "/rootfs.ext4")
	assert.True(t, ok)
	assert.Equal(t, rootBuild, id)
	_, ok = markerBuildID(markerPrefix + "not-a-uuid.json")
	assert.False(t, ok)
	id, ok = markerBuildID(markerPrefix + rootBuild + ".json")
	assert.True(t, ok)
	assert.Equal(t, rootBuild, id)
}
