package orchestrator

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/e2b-dev/infra/packages/api/internal/sandbox"
	orchestratorgrpc "github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
)

func TestGetMaxTTLNormal(t *testing.T) {
	t.Parallel()
	now := time.Now()
	ttl := getMaxAllowedTTL(now, now, 2*time.Hour, 3*time.Hour)
	if ttl != 2*time.Hour {
		t.Fatalf("expected 2 hours, got %v", ttl)
	}
}

func TestGetMaxTTLMax(t *testing.T) {
	t.Parallel()
	now := time.Now()
	ttl := getMaxAllowedTTL(now, now, 4*time.Hour, 3*time.Hour)
	if ttl != 3*time.Hour {
		t.Fatalf("expected 3 hours, got %v", ttl)
	}
}

func TestGetMaxTTLExpired(t *testing.T) {
	t.Parallel()
	now := time.Now()
	ttl := getMaxAllowedTTL(now, now.Add(-2*time.Hour), 4*time.Hour, time.Hour)
	if ttl != 0 {
		t.Fatalf("expected 0 hours, got %v", ttl)
	}
}

type sandboxUpdateNotFoundClient struct {
	orchestratorgrpc.SandboxServiceClient
}

func (sandboxUpdateNotFoundClient) Update(_ context.Context, _ *orchestratorgrpc.SandboxUpdateRequest, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	return nil, status.Error(codes.NotFound, "sandbox missing on node")
}

func TestKeepAliveForRemovesStoreRecordWhenNodeSandboxIsMissing(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	o := newCreateSandboxTestOrchestrator(t)
	node := o.GetNode(uuid.Nil, "node-1")
	require.NotNil(t, node)
	node.SetSandboxClient(sandboxUpdateNotFoundClient{})

	teamID := uuid.New()
	sandboxID := "sbx-stale-" + uuid.NewString()[:8]
	startTime := time.Now().Add(-time.Minute)
	sbx := sandbox.Sandbox{
		SandboxID:         sandboxID,
		TemplateID:        "template-test",
		ClientID:          "client-test",
		ExecutionID:       "exec-test",
		TeamID:            teamID,
		BaseTemplateID:    "base-template-test",
		Metadata:          map[string]string{"scaffold": "true"},
		MaxInstanceLength: 24 * time.Hour,
		StartTime:         startTime,
		EndTime:           startTime.Add(time.Hour),
		NodeID:            node.ID,
		ClusterID:         node.ClusterID,
		State:             sandbox.StateRunning,
	}
	require.NoError(t, o.sandboxStore.Add(ctx, sbx, nil))

	_, apiErr := o.KeepAliveFor(ctx, teamID, sandboxID, 2*time.Hour, false)
	require.NotNil(t, apiErr)
	require.Equal(t, http.StatusNotFound, apiErr.Code)
	require.True(t, errors.Is(apiErr.Err, sandbox.ErrNotFound))
	require.True(t, errors.Is(apiErr.Err, ErrSandboxNotFound))

	_, err := o.sandboxStore.Get(ctx, teamID, sandboxID)
	require.ErrorIs(t, err, sandbox.ErrNotFound)
}
