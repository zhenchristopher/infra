//go:build linux

package service

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox"
	orchestratorinfo "github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator-info"
)

func newTestAdmission(t *testing.T) (*AdmissionController, *ServiceInfo, string) {
	t.Helper()

	statePath := filepath.Join(t.TempDir(), "host-admission.json")
	info := &ServiceInfo{ClientId: "node-1", ServiceId: "service-1"}
	info.SetStatus(t.Context(), orchestratorinfo.ServiceInfoStatus_Healthy)
	controller, err := NewAdmissionController(t.Context(), info, sandbox.NewSandboxesMap(), AdmissionConfig{
		StatePath: statePath,
		Enabled:   func(context.Context) bool { return true },
		Ceiling:   func(context.Context) int { return 8 },
	})
	require.NoError(t, err)

	return controller, info, statePath
}

func TestAdmissionDrainIsIdempotentAndReadyRequiresGeneration(t *testing.T) {
	t.Parallel()

	controller, info, _ := newTestAdmission(t)

	first, err := controller.Drain(t.Context(), "drain-1")
	require.NoError(t, err)
	assert.Equal(t, uint64(1), first.DrainGeneration)
	assert.True(t, first.AdmissionClosed)
	assert.True(t, first.Quiescent)
	assert.Equal(t, orchestratorinfo.ServiceInfoStatus_Draining, info.GetStatus().Status)

	replayed, err := controller.Drain(t.Context(), "drain-1")
	require.NoError(t, err)
	assert.Equal(t, first, replayed)

	_, err = controller.Ready(t.Context(), first.DrainGeneration+1)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))

	ready, err := controller.Ready(t.Context(), first.DrainGeneration)
	require.NoError(t, err)
	assert.False(t, ready.AdmissionClosed)
	assert.Equal(t, orchestratorinfo.ServiceInfoStatus_Healthy, info.GetStatus().Status)

	second, err := controller.Drain(t.Context(), "drain-2")
	require.NoError(t, err)
	assert.Equal(t, uint64(2), second.DrainGeneration)
}

func TestAdmissionRejectsCreateAfterDrainAndEnforcesCeiling(t *testing.T) {
	t.Parallel()

	controller, _, _ := newTestAdmission(t)
	for index := range 8 {
		reserved, err := controller.BeginCreate(t.Context(), string(rune('a'+index)))
		require.NoError(t, err)
		assert.True(t, reserved)
	}

	_, err := controller.BeginCreate(t.Context(), "ninth")
	assert.Equal(t, codes.ResourceExhausted, status.Code(err))

	for index := range 8 {
		require.NoError(t, controller.EndCreate(t.Context(), string(rune('a'+index))))
	}
	_, err = controller.Drain(t.Context(), "drain-1")
	require.NoError(t, err)

	_, err = controller.BeginCreate(t.Context(), "late")
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
}

func TestAdmissionRestartWithOutstandingReservationFailsClosed(t *testing.T) {
	t.Parallel()

	controller, info, statePath := newTestAdmission(t)
	reserved, err := controller.BeginCreate(t.Context(), "starting")
	require.NoError(t, err)
	require.True(t, reserved)

	restartedInfo := &ServiceInfo{ClientId: info.ClientId, ServiceId: info.ServiceId}
	restartedInfo.SetStatus(t.Context(), orchestratorinfo.ServiceInfoStatus_Healthy)
	restarted, err := NewAdmissionController(t.Context(), restartedInfo, sandbox.NewSandboxesMap(), AdmissionConfig{
		StatePath: statePath,
		Enabled:   func(context.Context) bool { return true },
		Ceiling:   func(context.Context) int { return 8 },
	})
	require.NoError(t, err)

	snapshot := restarted.Snapshot()
	assert.False(t, snapshot.RecoveryComplete)
	assert.True(t, snapshot.AdmissionClosed)
	assert.False(t, snapshot.Quiescent)
	assert.Equal(t, uint32(1), snapshot.StartingCount)
	assert.Equal(t, uint64(2), snapshot.RecoveryEpoch)

	_, err = restarted.BeginCreate(t.Context(), "late")
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
}

func TestAdmissionRestartReconstructsRunningReservationBeforeCompletingRecovery(t *testing.T) {
	t.Parallel()

	controller, info, statePath := newTestAdmission(t)
	running := &sandbox.Sandbox{
		Metadata: &sandbox.Metadata{
			Runtime: sandbox.RuntimeMetadata{SandboxID: "running"},
		},
	}
	reserved, err := controller.BeginCreate(t.Context(), running.Runtime.SandboxID)
	require.NoError(t, err)
	require.True(t, reserved)
	controller.OnInsert(t.Context(), running)
	require.NoError(t, controller.EndCreate(t.Context(), running.Runtime.SandboxID))

	restartedInfo := &ServiceInfo{ClientId: info.ClientId, ServiceId: info.ServiceId}
	restartedInfo.SetStatus(t.Context(), orchestratorinfo.ServiceInfoStatus_Healthy)
	restarted, err := NewAdmissionController(t.Context(), restartedInfo, sandbox.NewSandboxesMap(), AdmissionConfig{
		StatePath: statePath,
		Enabled:   func(context.Context) bool { return true },
		Ceiling:   func(context.Context) int { return 8 },
	})
	require.NoError(t, err)

	beforeRecovery := restarted.Snapshot()
	assert.False(t, beforeRecovery.RecoveryComplete)
	assert.True(t, beforeRecovery.AdmissionClosed)
	assert.Equal(t, uint32(1), beforeRecovery.RunningCount)
	assert.False(t, beforeRecovery.Quiescent)

	restarted.OnInsert(t.Context(), running)
	afterRecovery := restarted.Snapshot()
	assert.True(t, afterRecovery.RecoveryComplete)
	assert.False(t, afterRecovery.AdmissionClosed)
	assert.Equal(t, uint32(1), afterRecovery.RunningCount)
	assert.False(t, afterRecovery.Quiescent)
	assert.Equal(t, orchestratorinfo.ServiceInfoStatus_Healthy, restartedInfo.GetStatus().Status)
}

func TestAdmissionChangedServiceInstanceFailsClosed(t *testing.T) {
	t.Parallel()

	_, _, statePath := newTestAdmission(t)
	changedInfo := &ServiceInfo{ClientId: "node-1", ServiceId: "different-service"}
	changedInfo.SetStatus(t.Context(), orchestratorinfo.ServiceInfoStatus_Healthy)

	_, err := NewAdmissionController(t.Context(), changedInfo, sandbox.NewSandboxesMap(), AdmissionConfig{
		StatePath: statePath,
		Enabled:   func(context.Context) bool { return true },
		Ceiling:   func(context.Context) int { return 8 },
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "belongs to service instance")
}

func TestStableServiceInstanceIDPersistsAcrossRestart(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "service-instance.json")
	first, err := LoadOrCreateServiceInstanceID(path)
	require.NoError(t, err)
	second, err := LoadOrCreateServiceInstanceID(path)
	require.NoError(t, err)

	assert.NotEmpty(t, first)
	assert.Equal(t, first, second)
}
