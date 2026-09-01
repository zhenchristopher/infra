package nodemanager

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	orchestratorinfo "github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator-info"
)

type hostAdmissionInfoClient struct {
	orchestratorinfo.InfoServiceClient

	snapshot *orchestratorinfo.HostAdmissionSnapshot
	drainID  string
	readyGen uint64
}

func (c *hostAdmissionInfoClient) HostAdmission(context.Context, *emptypb.Empty, ...grpc.CallOption) (*orchestratorinfo.HostAdmissionSnapshot, error) {
	return c.snapshot, nil
}

func (c *hostAdmissionInfoClient) HostDrain(_ context.Context, request *orchestratorinfo.HostDrainRequest, _ ...grpc.CallOption) (*orchestratorinfo.HostAdmissionSnapshot, error) {
	c.drainID = request.GetRequestId()

	return c.snapshot, nil
}

func (c *hostAdmissionInfoClient) HostReady(_ context.Context, request *orchestratorinfo.HostReadyRequest, _ ...grpc.CallOption) (*orchestratorinfo.HostAdmissionSnapshot, error) {
	c.readyGen = request.GetExpectedDrainGeneration()

	return c.snapshot, nil
}

func TestHostAdmissionAdminBridge(t *testing.T) {
	t.Parallel()

	client := &hostAdmissionInfoClient{
		snapshot: &orchestratorinfo.HostAdmissionSnapshot{
			NodeId:                  "node-a",
			ServiceId:               "service-a",
			ServiceStatus:           orchestratorinfo.ServiceInfoStatus_Draining,
			DrainRequestId:          "drain-a",
			DrainGeneration:         3,
			AdmissionClosed:         true,
			MetricSandboxesRunning:  0,
			MetricSandboxesStarting: 0,
			RecoveryComplete:        true,
			RecoveryEpoch:           7,
			Quiescent:               true,
		},
	}
	node := NewTestNode("node-a", api.NodeStatusDraining, 0, 16)
	node.client.Info = client

	admission, err := node.HostAdmission(t.Context())
	require.NoError(t, err)
	require.Equal(t, "service-a", admission.ServiceInstanceID)
	require.Equal(t, uint64(3), admission.DrainGeneration)
	require.True(t, admission.AdmissionClosed)
	require.True(t, admission.RecoveryComplete)
	require.True(t, admission.Quiescent)

	_, err = node.HostDrain(t.Context(), "drain-a")
	require.NoError(t, err)
	require.Equal(t, "drain-a", client.drainID)

	_, err = node.HostReady(t.Context(), 3)
	require.NoError(t, err)
	require.Equal(t, uint64(3), client.readyGen)
}

func TestHostAdmissionAdminBridgeRejectsMissingIdentity(t *testing.T) {
	t.Parallel()

	node := NewTestNode("node-a", api.NodeStatusDraining, 0, 16)
	node.client.Info = &hostAdmissionInfoClient{
		snapshot: &orchestratorinfo.HostAdmissionSnapshot{
			ServiceStatus: orchestratorinfo.ServiceInfoStatus_Draining,
		},
	}

	_, err := node.HostAdmission(t.Context())
	require.ErrorContains(t, err, "omitted stable identity")
}
