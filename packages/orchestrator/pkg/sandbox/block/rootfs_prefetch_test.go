//go:build linux

package block

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

// Failure modes: warm caches hide demand; alias mappings duplicate backing frames;
// multi-frame reads lose tails; concurrent collection races; stale or oversized
// metadata initiates unbounded reads; failed collection advertises a partial set.
func rootfsTestHeader(t *testing.T, frames int, decoded, encoded int32) *header.Header {
	t.Helper()
	id := uuid.New()
	h, err := header.NewHeader(&header.Metadata{Version: 4, BuildId: id, Size: uint64(frames) * uint64(decoded), BlockSize: header.PageSize}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sizes := make([]storage.FrameSize, frames)
	for i := range sizes {
		sizes[i] = storage.FrameSize{U: decoded, C: encoded}
	}
	h.SetBuild(id, header.BuildData{Size: int64(h.Metadata.Size), FrameData: storage.NewFullFrameTable(storage.CompressionLZ4, sizes).Table()})
	return h
}

// Already-resident bytes: recording must happen above the provider/cache layer.
type residentRootfs struct{ ReadonlyDevice }

func (residentRootfs) ReadAt(_ context.Context, p []byte, _ int64) (int, error) {
	clear(p)
	return len(p), nil
}
func (residentRootfs) Slice(_ context.Context, _, length int64) ([]byte, error) {
	return make([]byte, length), nil
}

func TestRootfsPrefetchCapture(t *testing.T) {
	h := rootfsTestHeader(t, 3, 2*header.PageSize, header.PageSize)
	// The last virtual page aliases the first source frame.
	mapping := []header.BuildMap{{Offset: 0, Length: 5 * header.PageSize, BuildId: h.Metadata.BuildId}, {Offset: 5 * header.PageSize, Length: header.PageSize, BuildId: h.Metadata.BuildId}}
	var err error
	h.Mapping, err = header.NewMapping(header.PageSize, mapping)
	if err != nil {
		t.Fatal(err)
	}
	r := NewRootfsRecorder(residentRootfs{}, h)
	ctx := context.Background()
	if _, err := r.ReadAt(ctx, make([]byte, 4*header.PageSize), header.PageSize); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() { _, _ = r.Slice(ctx, 5*header.PageSize, header.PageSize) })
	}
	wg.Wait()
	m, err := r.Mapping()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m.Offsets, []uint64{header.PageSize, 2 * header.PageSize, 4 * header.PageSize}) {
		t.Fatalf("lost frame or duplicate alias: %v", m.Offsets)
	}
	frames, err := m.Frames(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 3 || frames[0].Offset != 0 || frames[2].Offset != 4*header.PageSize || frames[2].Length != 2*header.PageSize {
		t.Fatalf("wrong backing ranges: %+v", frames)
	}
	// Mapping seals collection; subsequent guest reads cannot mutate published data.
	_, _ = r.ReadAt(ctx, make([]byte, header.PageSize), 0)
	if len(m.Offsets) != 3 {
		t.Fatal("published mapping mutated")
	}
}

func TestRootfsPrefetchRejectsUnsafePlans(t *testing.T) {
	ctx := context.Background()
	h := rootfsTestHeader(t, 2, 2*header.PageSize, header.PageSize)
	for name, m := range map[string]*RootfsPrefetchMapping{
		"stale":        {BuildID: uuid.New(), Offsets: []uint64{0}},
		"unaligned":    {BuildID: h.Metadata.BuildId, Offsets: []uint64{1}},
		"out of range": {BuildID: h.Metadata.BuildId, Offsets: []uint64{h.Metadata.Size}},
		"overflow":     {BuildID: h.Metadata.BuildId, Offsets: []uint64{math.MaxUint64}},
		"count":        {BuildID: h.Metadata.BuildId, Offsets: make([]uint64, rootfsMaxFrames+1)},
	} {
		t.Run(name, func(t *testing.T) {
			if frames, err := m.Frames(ctx, h); err == nil || frames != nil {
				t.Fatalf("unsafe plan accepted: %v, %v", frames, err)
			}
		})
	}
	for _, shape := range []struct {
		frames           int
		decoded, encoded int32
	}{
		{1, rootfsMaxFrameBytes + header.PageSize, header.PageSize},
		{1, header.PageSize, rootfsMaxFrameBytes + 1},
		{rootfsMaxDecodedBytes/(2<<20) + 1, 2 << 20, 4096},
		{rootfsMaxEncodedBytes/(2<<20) + 1, 2 << 20, 2 << 20},
	} {
		h := rootfsTestHeader(t, shape.frames, shape.decoded, shape.encoded)
		m := &RootfsPrefetchMapping{BuildID: h.Metadata.BuildId}
		for i := range shape.frames {
			m.Offsets = append(m.Offsets, uint64(i)*uint64(shape.decoded))
		}
		if frames, err := m.Frames(ctx, h); err == nil || frames != nil {
			t.Fatalf("byte cap not enforced for %+v: %v", shape, err)
		}
	}
	// Exact byte boundary remains usable, and cancellation prevents even planning.
	h = rootfsTestHeader(t, rootfsMaxDecodedBytes/(2<<20), 2<<20, 4096)
	m := &RootfsPrefetchMapping{BuildID: h.Metadata.BuildId}
	for i := range rootfsMaxDecodedBytes / (2 << 20) {
		m.Offsets = append(m.Offsets, uint64(i)*(2<<20))
	}
	if _, err := m.Frames(ctx, h); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := m.Frames(canceled, h); err == nil {
		t.Fatal("ignored cancellation")
	}
}

func TestRootfsPrefetchCollectionFailsClosed(t *testing.T) {
	h := rootfsTestHeader(t, rootfsMaxFrames+1, header.PageSize, 1)
	r := NewRootfsRecorder(residentRootfs{}, h)
	if _, err := r.ReadAt(context.Background(), make([]byte, int(h.Metadata.Size)), 0); err != nil {
		t.Fatalf("collection broke guest read: %v", err)
	}
	if m, err := r.Mapping(); err == nil || m != nil {
		t.Fatalf("partial capture published: %v, %v", m, err)
	}
}

func TestRootfsPrefetchMetadataBound(t *testing.T) {
	var m RootfsPrefetchMapping
	body := `{"build_id":"` + uuid.NewString() + `","offsets":[` + strings.Repeat("0,", rootfsMaxFrames) + `0]}`
	if err := json.Unmarshal([]byte(body), &m); err == nil {
		t.Fatal("oversized rootfs map accepted")
	}
}
