//go:build linux

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox"
	orchestratorinfo "github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator-info"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

const admissionStateVersion = 1

type AdmissionConfig struct {
	StatePath string
	Enabled   func(context.Context) bool
	Ceiling   func(context.Context) int
}

type admissionDiskState struct {
	Version                     int      `json:"version"`
	ServiceInstanceID           string   `json:"serviceInstanceId"`
	DrainRequestID              string   `json:"drainRequestId,omitempty"`
	DrainGeneration             uint64   `json:"drainGeneration"`
	AdmissionClosed             bool     `json:"admissionClosed"`
	StatusOverrideClosed        bool     `json:"statusOverrideClosed"`
	RecoveryClosed              bool     `json:"recoveryClosed"`
	RecoveryComplete            bool     `json:"recoveryComplete"`
	RecoveryEpoch               uint64   `json:"recoveryEpoch"`
	RecoveryPendingReservations []string `json:"recoveryPendingReservations,omitempty"`
	RunningReservations         []string `json:"runningReservations,omitempty"`
	StartingReservations        []string `json:"startingReservations,omitempty"`
}

type AdmissionSnapshot struct {
	NodeID           string
	ServiceID        string
	ServiceStatus    orchestratorinfo.ServiceInfoStatus
	DrainRequestID   string
	DrainGeneration  uint64
	AdmissionClosed  bool
	RunningCount     uint32
	StartingCount    uint32
	RecoveryComplete bool
	RecoveryEpoch    uint64
	Quiescent        bool
}

type AdmissionController struct {
	mu        sync.Mutex
	info      *ServiceInfo
	sandboxes *sandbox.Map
	config    AdmissionConfig
	state     admissionDiskState
}

func LoadOrCreateServiceInstanceID(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		var persisted struct {
			ServiceInstanceID string `json:"serviceInstanceId"`
		}
		if jsonErr := json.Unmarshal(data, &persisted); jsonErr != nil {
			return "", fmt.Errorf("decode persisted service instance ID: %w", jsonErr)
		}
		if persisted.ServiceInstanceID == "" {
			return "", errors.New("persisted service instance ID is empty")
		}

		return persisted.ServiceInstanceID, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read persisted service instance ID: %w", err)
	}

	serviceInstanceID := uuid.NewString()
	data, err = json.Marshal(struct {
		ServiceInstanceID string `json:"serviceInstanceId"`
	}{ServiceInstanceID: serviceInstanceID})
	if err != nil {
		return "", fmt.Errorf("encode service instance ID: %w", err)
	}
	if err := persistAtomic(path, data); err != nil {
		return "", fmt.Errorf("persist service instance ID: %w", err)
	}

	return serviceInstanceID, nil
}

func NewAdmissionController(ctx context.Context, info *ServiceInfo, sandboxes *sandbox.Map, config AdmissionConfig) (*AdmissionController, error) {
	if info == nil {
		return nil, errors.New("service info is required")
	}
	if sandboxes == nil {
		return nil, errors.New("sandbox map is required")
	}
	if config.StatePath == "" {
		return nil, errors.New("host admission state path is required")
	}

	controller := &AdmissionController{
		info:      info,
		sandboxes: sandboxes,
		config:    config,
	}

	state, err := loadAdmissionState(config.StatePath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		state = admissionDiskState{
			Version:             admissionStateVersion,
			ServiceInstanceID:   info.ServiceId,
			RecoveryComplete:    true,
			RecoveryEpoch:       1,
			RunningReservations: sandboxIDs(sandboxes.Items()),
		}
	case err != nil:
		return nil, fmt.Errorf("load host admission state: %w", err)
	case state.Version != admissionStateVersion:
		return nil, fmt.Errorf("unsupported host admission state version %d", state.Version)
	case state.ServiceInstanceID != info.ServiceId:
		return nil, fmt.Errorf("host admission state belongs to service instance %q, current instance is %q", state.ServiceInstanceID, info.ServiceId)
	default:
		state.RecoveryEpoch++
		state.RecoveryPendingReservations = mergeIDs(state.RunningReservations, state.StartingReservations)
		state.RecoveryComplete = len(state.RecoveryPendingReservations) == 0
		state.RecoveryClosed = !state.RecoveryComplete
		state.AdmissionClosed = state.DrainRequestID != "" || state.StatusOverrideClosed || state.RecoveryClosed
	}

	controller.state = state
	if err := controller.persistLocked(); err != nil {
		return nil, fmt.Errorf("persist host admission recovery state: %w", err)
	}
	if state.AdmissionClosed {
		controller.info.SetStatus(ctx, orchestratorinfo.ServiceInfoStatus_Draining)
	}
	sandboxes.Subscribe(controller)

	return controller, nil
}

func (c *AdmissionController) BeginCreate(ctx context.Context, sandboxID string) (bool, error) {
	if !c.enabled(ctx) {
		return false, nil
	}
	if sandboxID == "" {
		return false, status.Error(codes.InvalidArgument, "sandbox ID is required")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.info.GetStatus().Status != orchestratorinfo.ServiceInfoStatus_Healthy {
		return false, status.Error(codes.FailedPrecondition, "host is not healthy")
	}
	if c.state.AdmissionClosed || !c.state.RecoveryComplete {
		return false, status.Error(codes.FailedPrecondition, "host admission is closed")
	}

	ceiling := c.config.Ceiling(ctx)
	if ceiling <= 0 {
		return false, status.Error(codes.FailedPrecondition, "host admission ceiling is invalid")
	}
	if int(c.sandboxes.Count())+len(c.state.StartingReservations) >= ceiling {
		return false, status.Errorf(codes.ResourceExhausted, "host running plus starting sandbox ceiling reached (%d)", ceiling)
	}
	if slices.Contains(c.state.StartingReservations, sandboxID) {
		return false, status.Errorf(codes.AlreadyExists, "sandbox %q already has a starting reservation", sandboxID)
	}

	next := c.state
	next.StartingReservations = append(slices.Clone(next.StartingReservations), sandboxID)
	slices.Sort(next.StartingReservations)
	if err := c.persistState(next); err != nil {
		return false, status.Errorf(codes.Internal, "persist starting reservation: %s", err)
	}
	c.state = next

	return true, nil
}

func (c *AdmissionController) EndCreate(ctx context.Context, sandboxID string) error {
	if c == nil {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if _, running := c.sandboxes.Get(sandboxID); running {
		return c.markRunningLocked(ctx, sandboxID)
	}

	index := slices.Index(c.state.StartingReservations, sandboxID)
	if index < 0 {
		return nil
	}

	next := c.state
	next.StartingReservations = slices.Delete(slices.Clone(next.StartingReservations), index, index+1)
	if err := c.persistState(next); err != nil {
		c.failClosedLocked(ctx, err)

		return fmt.Errorf("persist released starting reservation: %w", err)
	}
	c.state = next

	return nil
}

func (c *AdmissionController) Drain(ctx context.Context, requestID string) (AdmissionSnapshot, error) {
	if !c.enabled(ctx) {
		return AdmissionSnapshot{}, status.Error(codes.FailedPrecondition, "host admission contract is disabled")
	}
	if requestID == "" {
		return AdmissionSnapshot{}, status.Error(codes.InvalidArgument, "drain request ID is required")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.state.DrainRequestID == requestID {
		return c.snapshotLocked(), nil
	}
	if c.state.DrainRequestID != "" {
		return AdmissionSnapshot{}, status.Errorf(codes.FailedPrecondition, "host already has active drain request %q", c.state.DrainRequestID)
	}

	next := c.state
	next.DrainRequestID = requestID
	next.DrainGeneration++
	next.AdmissionClosed = true
	if err := c.persistState(next); err != nil {
		return AdmissionSnapshot{}, status.Errorf(codes.Internal, "persist drain generation: %s", err)
	}
	c.state = next
	c.info.SetStatus(ctx, orchestratorinfo.ServiceInfoStatus_Draining)

	return c.snapshotLocked(), nil
}

func (c *AdmissionController) Ready(ctx context.Context, expectedGeneration uint64) (AdmissionSnapshot, error) {
	if !c.enabled(ctx) {
		return AdmissionSnapshot{}, status.Error(codes.FailedPrecondition, "host admission contract is disabled")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.state.DrainRequestID == "" || !c.state.AdmissionClosed {
		return AdmissionSnapshot{}, status.Error(codes.FailedPrecondition, "host is not under a managed drain")
	}
	if c.state.DrainGeneration != expectedGeneration {
		return AdmissionSnapshot{}, status.Errorf(codes.FailedPrecondition, "expected drain generation %d, current generation is %d", expectedGeneration, c.state.DrainGeneration)
	}
	if !c.state.RecoveryComplete {
		return AdmissionSnapshot{}, status.Error(codes.FailedPrecondition, "host recovery is incomplete")
	}
	if c.state.StatusOverrideClosed {
		return AdmissionSnapshot{}, status.Error(codes.FailedPrecondition, "host has an active service status override")
	}

	next := c.state
	next.DrainRequestID = ""
	next.AdmissionClosed = next.RecoveryClosed
	if err := c.persistState(next); err != nil {
		return AdmissionSnapshot{}, status.Errorf(codes.Internal, "persist host ready state: %s", err)
	}
	c.state = next
	c.info.SetStatus(ctx, orchestratorinfo.ServiceInfoStatus_Healthy)

	return c.snapshotLocked(), nil
}

func (c *AdmissionController) CloseForStatus(ctx context.Context, serviceStatus orchestratorinfo.ServiceInfoStatus) error {
	if c == nil {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	next := c.state
	next.AdmissionClosed = true
	next.StatusOverrideClosed = true
	if err := c.persistState(next); err != nil {
		return fmt.Errorf("persist closed admission: %w", err)
	}
	c.state = next
	c.info.SetStatus(ctx, serviceStatus)

	return nil
}

func (c *AdmissionController) Snapshot() AdmissionSnapshot {
	if c == nil {
		return AdmissionSnapshot{}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	return c.snapshotLocked()
}

func (c *AdmissionController) StartingCount() uint32 {
	if c == nil {
		return 0
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	return uint32(len(c.state.StartingReservations))
}

func (c *AdmissionController) enabled(ctx context.Context) bool {
	return c != nil && c.config.Enabled != nil && c.config.Enabled(ctx)
}

func (c *AdmissionController) snapshotLocked() AdmissionSnapshot {
	running := uint32(max(c.sandboxes.Count(), len(c.state.RunningReservations)))
	starting := uint32(len(c.state.StartingReservations))

	return AdmissionSnapshot{
		NodeID:           c.info.ClientId,
		ServiceID:        c.info.ServiceId,
		ServiceStatus:    c.info.GetStatus().Status,
		DrainRequestID:   c.state.DrainRequestID,
		DrainGeneration:  c.state.DrainGeneration,
		AdmissionClosed:  c.state.AdmissionClosed,
		RunningCount:     running,
		StartingCount:    starting,
		RecoveryComplete: c.state.RecoveryComplete,
		RecoveryEpoch:    c.state.RecoveryEpoch,
		Quiescent:        c.state.RecoveryComplete && c.state.AdmissionClosed && running+starting == 0,
	}
}

func (c *AdmissionController) OnInsert(ctx context.Context, sbx *sandbox.Sandbox) {
	if c == nil || sbx == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.markRunningLocked(ctx, sbx.Runtime.SandboxID); err != nil {
		logger.L().Error(ctx, "failed to persist running sandbox admission state", zap.Error(err), logger.WithSandboxID(sbx.Runtime.SandboxID))
	}
}

func (c *AdmissionController) OnNetworkRelease(ctx context.Context, sbx *sandbox.Sandbox) {
	if c == nil || sbx == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	next := cloneAdmissionState(c.state)
	next.RunningReservations = removeID(next.RunningReservations, sbx.Runtime.SandboxID)
	next.RecoveryPendingReservations = removeID(next.RecoveryPendingReservations, sbx.Runtime.SandboxID)
	c.finishRecovery(&next)
	if err := c.persistState(next); err != nil {
		c.failClosedLocked(ctx, err)
		logger.L().Error(ctx, "failed to persist released running sandbox admission state", zap.Error(err), logger.WithSandboxID(sbx.Runtime.SandboxID))

		return
	}
	c.state = next
	c.applyRecoveredStatus(ctx)
}

func (c *AdmissionController) markRunningLocked(ctx context.Context, sandboxID string) error {
	next := cloneAdmissionState(c.state)
	next.StartingReservations = removeID(next.StartingReservations, sandboxID)
	next.RunningReservations = addID(next.RunningReservations, sandboxID)
	next.RecoveryPendingReservations = removeID(next.RecoveryPendingReservations, sandboxID)
	c.finishRecovery(&next)
	if slices.Equal(next.StartingReservations, c.state.StartingReservations) &&
		slices.Equal(next.RunningReservations, c.state.RunningReservations) &&
		slices.Equal(next.RecoveryPendingReservations, c.state.RecoveryPendingReservations) &&
		next.RecoveryComplete == c.state.RecoveryComplete &&
		next.RecoveryClosed == c.state.RecoveryClosed &&
		next.AdmissionClosed == c.state.AdmissionClosed {
		return nil
	}
	if err := c.persistState(next); err != nil {
		c.failClosedLocked(ctx, err)

		return err
	}
	c.state = next
	c.applyRecoveredStatus(ctx)

	return nil
}

func (c *AdmissionController) finishRecovery(state *admissionDiskState) {
	if len(state.RecoveryPendingReservations) != 0 {
		return
	}

	state.RecoveryComplete = true
	state.RecoveryClosed = false
	state.AdmissionClosed = state.DrainRequestID != "" || state.StatusOverrideClosed
}

func (c *AdmissionController) applyRecoveredStatus(ctx context.Context) {
	if c.state.RecoveryComplete && !c.state.AdmissionClosed && c.info.GetStatus().Status == orchestratorinfo.ServiceInfoStatus_Draining {
		c.info.SetStatus(ctx, orchestratorinfo.ServiceInfoStatus_Healthy)
	}
}

func (c *AdmissionController) failClosedLocked(ctx context.Context, err error) {
	c.state.AdmissionClosed = true
	c.state.RecoveryClosed = true
	c.state.RecoveryComplete = false
	c.info.SetStatus(ctx, orchestratorinfo.ServiceInfoStatus_Draining)
	logger.L().Error(ctx, "host admission failed closed", zap.Error(err))
}

func cloneAdmissionState(state admissionDiskState) admissionDiskState {
	state.RecoveryPendingReservations = slices.Clone(state.RecoveryPendingReservations)
	state.RunningReservations = slices.Clone(state.RunningReservations)
	state.StartingReservations = slices.Clone(state.StartingReservations)

	return state
}

func addID(ids []string, id string) []string {
	if id == "" || slices.Contains(ids, id) {
		return ids
	}

	ids = append(slices.Clone(ids), id)
	slices.Sort(ids)

	return ids
}

func removeID(ids []string, id string) []string {
	index := slices.Index(ids, id)
	if index < 0 {
		return ids
	}

	return slices.Delete(slices.Clone(ids), index, index+1)
}

func mergeIDs(groups ...[]string) []string {
	merged := make([]string, 0)
	for _, group := range groups {
		for _, id := range group {
			merged = addID(merged, id)
		}
	}

	return merged
}

func sandboxIDs(items map[string]*sandbox.Sandbox) []string {
	ids := make([]string, 0, len(items))
	for id := range items {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	return ids
}

func loadAdmissionState(path string) (admissionDiskState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return admissionDiskState{}, err
	}

	var state admissionDiskState
	if err := json.Unmarshal(data, &state); err != nil {
		return admissionDiskState{}, fmt.Errorf("decode admission state: %w", err)
	}

	return state, nil
}

func (c *AdmissionController) persistLocked() error {
	return c.persistState(c.state)
}

func (c *AdmissionController) persistState(state admissionDiskState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode admission state: %w", err)
	}

	return persistAtomic(c.config.StatePath, data)
}

func persistAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	temp, err := os.CreateTemp(filepath.Dir(path), ".admission-state-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() {
		_ = os.Remove(tempPath)
	}()

	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()

		return err
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()

		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()

		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}

	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := dir.Close(); closeErr != nil {
			logger.L().Warn(context.Background(), "failed to close admission state directory", zap.Error(closeErr))
		}
	}()

	return dir.Sync()
}
