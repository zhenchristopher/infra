//go:build linux

package block

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/google/uuid"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

const (
	rootfsMaxFrames       = 1024
	rootfsMaxFrameBytes   = 8 << 20
	rootfsMaxEncodedBytes = 512 << 20
	rootfsMaxDecodedBytes = 1 << 30
)

// RootfsPrefetchMapping names virtual pages, never provider paths. The immutable
// snapshot header is the only authority for backing objects and frame geometry.
type RootfsPrefetchMapping struct {
	BuildID uuid.UUID `json:"build_id"`
	Offsets []uint64  `json:"offsets"`
}

func (m *RootfsPrefetchMapping) UnmarshalJSON(data []byte) error {
	if len(data) > 32<<10 {
		return fmt.Errorf("rootfs prefetch metadata exceeds 32 KiB")
	}
	type plain RootfsPrefetchMapping
	var value plain
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	if len(value.Offsets) > rootfsMaxFrames {
		return fmt.Errorf("rootfs prefetch exceeds %d frames", rootfsMaxFrames)
	}
	*m = RootfsPrefetchMapping(value)
	return nil
}

// RootfsPrefetchFrame is a complete backing range, including bytes outside the
// virtual mapping. Reading only the requested page would leave an unbounded tail
// of asynchronous chunk fetches and would not establish a pre-resume barrier.
type RootfsPrefetchFrame struct {
	BuildID      uuid.UUID
	Offset       int64
	Length       int64
	EncodedBytes int64
	FrameTable   *storage.FrameTable
}

type rootfsFrameKey struct {
	buildID uuid.UUID
	offset  int64
}
type rootfsPlan struct {
	seen             map[rootfsFrameKey]struct{}
	frames           []RootfsPrefetchFrame
	offsets          []uint64
	encoded, decoded int64
}

func (p *rootfsPlan) add(frame RootfsPrefetchFrame, offset uint64) error {
	if frame.BuildID == uuid.Nil {
		return nil
	}
	key := rootfsFrameKey{frame.BuildID, frame.Offset}
	if _, ok := p.seen[key]; ok {
		return nil
	}
	if len(p.frames) >= rootfsMaxFrames || frame.Length <= 0 || frame.Length > rootfsMaxFrameBytes || frame.EncodedBytes <= 0 || frame.EncodedBytes > rootfsMaxFrameBytes || frame.Length > rootfsMaxDecodedBytes-p.decoded || frame.EncodedBytes > rootfsMaxEncodedBytes-p.encoded {
		return fmt.Errorf("rootfs prefetch exceeds frame or byte budget")
	}
	if p.seen == nil {
		p.seen = make(map[rootfsFrameKey]struct{})
	}
	p.seen[key] = struct{}{}
	p.frames = append(p.frames, frame)
	p.offsets = append(p.offsets, offset)
	p.decoded += frame.Length
	p.encoded += frame.EncodedBytes
	return nil
}

func rootfsFrameAt(ctx context.Context, h *header.Header, offset uint64) (RootfsPrefetchFrame, uint64, error) {
	if err := ctx.Err(); err != nil {
		return RootfsPrefetchFrame{}, 0, err
	}
	if h == nil || h.Metadata == nil || h.IncompletePendingUpload || offset%header.PageSize != 0 || offset >= h.Metadata.Size {
		return RootfsPrefetchFrame{}, 0, fmt.Errorf("invalid rootfs prefetch offset or incomplete header")
	}
	m, err := h.GetShiftedMapping(ctx, int64(offset))
	if err != nil {
		return RootfsPrefetchFrame{}, 0, err
	}
	if m.Length == 0 {
		return RootfsPrefetchFrame{}, 0, fmt.Errorf("empty rootfs mapping")
	}
	if m.BuildId == uuid.Nil {
		return RootfsPrefetchFrame{}, offset + min(m.Length, h.Metadata.Size-offset), nil
	}
	bd, ok := h.Builds[m.BuildId]
	if !ok || bd.Size <= 0 || m.Offset >= uint64(bd.Size) {
		return RootfsPrefetchFrame{}, 0, fmt.Errorf("rootfs backing size unavailable or out of range")
	}
	ft := h.GetBuildFrameData(m.BuildId)
	start := int64(m.Offset) / storage.MemoryChunkSize * storage.MemoryChunkSize
	length := min(int64(storage.MemoryChunkSize), bd.Size-start)
	encoded := length
	if ft.IsCompressed() {
		u, err := ft.LocateUncompressed(int64(m.Offset))
		if err != nil {
			return RootfsPrefetchFrame{}, 0, err
		}
		c, err := ft.LocateCompressed(u.Offset)
		if err != nil {
			return RootfsPrefetchFrame{}, 0, err
		}
		start, length, encoded = u.Offset, int64(u.Length), int64(c.Length)
	}
	if start < 0 || length <= 0 || start > bd.Size-length || int64(m.Offset) < start || int64(m.Offset)-start >= length {
		return RootfsPrefetchFrame{}, 0, fmt.Errorf("invalid rootfs backing frame")
	}
	next := offset + min(m.Length, h.Metadata.Size-offset, uint64(start+length)-m.Offset)
	return RootfsPrefetchFrame{BuildID: m.BuildId, Offset: start, Length: length, EncodedBytes: encoded, FrameTable: ft}, next, nil
}

// Frames validates the entire plan before its caller performs any I/O.
func (m *RootfsPrefetchMapping) Frames(ctx context.Context, h *header.Header) ([]RootfsPrefetchFrame, error) {
	if m == nil {
		return nil, nil
	}
	if h == nil || h.Metadata == nil || m.BuildID == uuid.Nil || m.BuildID != h.Metadata.BuildId || len(m.Offsets) > rootfsMaxFrames {
		return nil, fmt.Errorf("rootfs prefetch snapshot identity or count mismatch")
	}
	var p rootfsPlan
	for _, offset := range m.Offsets {
		frame, _, err := rootfsFrameAt(ctx, h, offset)
		if err != nil {
			return nil, err
		}
		if err := p.add(frame, offset); err != nil {
			return nil, err
		}
	}
	return p.frames, nil
}

// RootfsRecorder is scoped to build qualification, above all provider caches.
// It observes read demand without retaining payloads or changing guest I/O errors.
type RootfsRecorder struct {
	ReadonlyDevice
	header *header.Header
	mu     sync.Mutex
	plan   rootfsPlan
	err    error
	sealed bool
}

func NewRootfsRecorder(device ReadonlyDevice, h *header.Header) *RootfsRecorder {
	return &RootfsRecorder{ReadonlyDevice: device, header: h}
}

func (r *RootfsRecorder) record(ctx context.Context, offset, length int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sealed || r.err != nil || length == 0 {
		return
	}
	if offset < 0 || length < 0 || r.header == nil || r.header.Metadata == nil || uint64(offset) > r.header.Metadata.Size || uint64(length) > r.header.Metadata.Size-uint64(offset) {
		r.err = fmt.Errorf("rootfs capture range out of bounds")
		return
	}
	for off, end := uint64(offset), uint64(offset)+uint64(length); off < end; {
		frame, next, err := rootfsFrameAt(ctx, r.header, off)
		if err == nil {
			err = r.plan.add(frame, off)
		}
		if err != nil {
			r.err = err
			return
		}
		off = next
	}
}

func (r *RootfsRecorder) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	r.record(ctx, off, int64(len(p)))
	return r.ReadonlyDevice.ReadAt(ctx, p, off)
}

func (r *RootfsRecorder) Slice(ctx context.Context, off, length int64) ([]byte, error) {
	r.record(ctx, off, length)
	return r.ReadonlyDevice.Slice(ctx, off, length)
}

// Mapping seals collection. No partial working set is advertised after overflow.
func (r *RootfsRecorder) Mapping() (*RootfsPrefetchMapping, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sealed = true
	if r.err != nil {
		return nil, r.err
	}
	if len(r.plan.offsets) == 0 {
		return nil, nil
	}
	return &RootfsPrefetchMapping{BuildID: r.header.Metadata.BuildId, Offsets: r.plan.offsets}, nil
}
