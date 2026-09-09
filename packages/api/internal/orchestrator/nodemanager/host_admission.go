package nodemanager

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	orchestratorinfo "github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator-info"
)

func (n *Node) HostAdmission(ctx context.Context) (*api.HostAdmissionSnapshot, error) {
	client, ctx := n.GetClient(ctx)
	snapshot, err := client.Info.HostAdmission(ctx, &emptypb.Empty{})
	if err != nil {
		return nil, fmt.Errorf("read host admission: %w", err)
	}

	return apiHostAdmissionSnapshot(snapshot)
}

func (n *Node) HostDrain(ctx context.Context, requestID string) (*api.HostAdmissionSnapshot, error) {
	client, ctx := n.GetClient(ctx)
	snapshot, err := client.Info.HostDrain(ctx, &orchestratorinfo.HostDrainRequest{RequestId: requestID})
	if err != nil {
		return nil, fmt.Errorf("drain host: %w", err)
	}

	return apiHostAdmissionSnapshot(snapshot)
}

func (n *Node) HostReady(ctx context.Context, expectedGeneration uint64) (*api.HostAdmissionSnapshot, error) {
	client, ctx := n.GetClient(ctx)
	snapshot, err := client.Info.HostReady(ctx, &orchestratorinfo.HostReadyRequest{ExpectedDrainGeneration: expectedGeneration})
	if err != nil {
		return nil, fmt.Errorf("ready host: %w", err)
	}

	return apiHostAdmissionSnapshot(snapshot)
}

func apiHostAdmissionSnapshot(snapshot *orchestratorinfo.HostAdmissionSnapshot) (*api.HostAdmissionSnapshot, error) {
	status, ok := OrchestratorToApiNodeStateMapper[snapshot.GetServiceStatus()]
	if !ok {
		return nil, fmt.Errorf("unknown host admission service status: %s", snapshot.GetServiceStatus())
	}
	if snapshot.GetNodeId() == "" || snapshot.GetServiceId() == "" {
		return nil, fmt.Errorf("host admission response omitted stable identity")
	}

	result := &api.HostAdmissionSnapshot{
		NodeID:            snapshot.GetNodeId(),
		ServiceInstanceID: snapshot.GetServiceId(),
		Status:            status,
		DrainGeneration:   snapshot.GetDrainGeneration(),
		AdmissionClosed:   snapshot.GetAdmissionClosed(),
		RunningCount:      snapshot.GetMetricSandboxesRunning(),
		StartingCount:     snapshot.GetMetricSandboxesStarting(),
		RecoveryComplete:  snapshot.GetRecoveryComplete(),
		RecoveryEpoch:     snapshot.GetRecoveryEpoch(),
		Quiescent:         snapshot.GetQuiescent(),
	}
	if snapshot.GetDrainRequestId() != "" {
		result.DrainRequestID = &snapshot.DrainRequestId
	}

	return result, nil
}
