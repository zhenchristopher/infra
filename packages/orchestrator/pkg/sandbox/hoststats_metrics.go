//go:build linux

package sandbox

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/cgroup"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

var (
	hostStatsMeter = otel.Meter("github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/hoststats")

	cgroupMemoryCurrent = utils.Must(hostStatsMeter.Int64Histogram(
		"orchestrator.sandbox.cgroup.memory.current",
		metric.WithDescription("Current host cgroup memory usage for a sandbox."),
		metric.WithUnit("By"),
	))
	cgroupMemoryPeak = utils.Must(hostStatsMeter.Int64Histogram(
		"orchestrator.sandbox.cgroup.memory.peak",
		metric.WithDescription("Peak host cgroup memory usage observed during the sampling interval."),
		metric.WithUnit("By"),
	))
	cgroupMemoryEvents = utils.Must(hostStatsMeter.Int64Counter(
		"orchestrator.sandbox.cgroup.memory.events",
		metric.WithDescription("Host cgroup memory events for a sandbox."),
		metric.WithUnit("{event}"),
	))
	cgroupCPUThrottledPeriods = utils.Must(hostStatsMeter.Int64Counter(
		"orchestrator.sandbox.cgroup.cpu.throttled.periods",
		metric.WithDescription("Host cgroup CPU throttling periods for a sandbox."),
		metric.WithUnit("{period}"),
	))
	cgroupCPUThrottledUsec = utils.Must(hostStatsMeter.Int64Counter(
		"orchestrator.sandbox.cgroup.cpu.throttled.usec",
		metric.WithDescription("Host cgroup CPU throttling time for a sandbox."),
		metric.WithUnit("us"),
	))
	cgroupPressureStalledUsec = utils.Must(hostStatsMeter.Int64Counter(
		"orchestrator.sandbox.cgroup.pressure.stalled.usec",
		metric.WithDescription("Host cgroup CPU, memory, or I/O pressure stall time for a sandbox."),
		metric.WithUnit("us"),
	))
)

func recordHostCgroupMetrics(ctx context.Context, metadata HostStatsMetadata, current, previous *cgroup.Stats) {
	baseAttrs := []attribute.KeyValue{
		attribute.String("sandbox_id", metadata.SandboxID),
		attribute.String("sandbox_type", metadata.SandboxType.String()),
	}
	baseOptions := metric.WithAttributes(baseAttrs...)

	cgroupMemoryCurrent.Record(ctx, int64(current.MemoryUsageBytes), baseOptions)
	cgroupMemoryPeak.Record(ctx, int64(current.MemoryPeakBytes), baseOptions)
	addDelta(ctx, cgroupCPUThrottledPeriods, current.CPUThrottledPeriods, previous.CPUThrottledPeriods, baseAttrs)
	addDelta(ctx, cgroupCPUThrottledUsec, current.CPUThrottledUsec, previous.CPUThrottledUsec, baseAttrs)
	addDelta(ctx, cgroupMemoryEvents, current.MemoryOOMEvents, previous.MemoryOOMEvents, append(baseAttrs, attribute.String("event", "oom")))
	addDelta(ctx, cgroupMemoryEvents, current.MemoryOOMKills, previous.MemoryOOMKills, append(baseAttrs, attribute.String("event", "oom_kill")))

	for _, pressure := range []struct {
		resource string
		scope    string
		current  uint64
		previous uint64
	}{
		{resource: "cpu", scope: "some", current: current.CPUPressureSomeUsec, previous: previous.CPUPressureSomeUsec},
		{resource: "cpu", scope: "full", current: current.CPUPressureFullUsec, previous: previous.CPUPressureFullUsec},
		{resource: "memory", scope: "some", current: current.MemoryPressureSomeUsec, previous: previous.MemoryPressureSomeUsec},
		{resource: "memory", scope: "full", current: current.MemoryPressureFullUsec, previous: previous.MemoryPressureFullUsec},
		{resource: "io", scope: "some", current: current.IOPressureSomeUsec, previous: previous.IOPressureSomeUsec},
		{resource: "io", scope: "full", current: current.IOPressureFullUsec, previous: previous.IOPressureFullUsec},
	} {
		addDelta(ctx, cgroupPressureStalledUsec, pressure.current, pressure.previous, append(
			baseAttrs,
			attribute.String("resource", pressure.resource),
			attribute.String("scope", pressure.scope),
		))
	}
}

func addDelta(ctx context.Context, counter metric.Int64Counter, current, previous uint64, attrs []attribute.KeyValue) {
	delta := saturatingSub(current, previous)
	if delta == 0 {
		return
	}

	counter.Add(ctx, int64(delta), metric.WithAttributes(attrs...))
}
