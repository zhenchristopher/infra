//go:build linux

package block

import (
	"context"
	"errors"
	"testing"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
)

// A page becoming available is not full-frame completion: footer corruption
// must fail replay, and an aborted replay must not start another backing fetch.
func TestRootfsPrefetchFrameCompletion(t *testing.T) {
	data := makeTestData(testFrameSize)
	ft, source := makeCompressedTestData(t, data)
	c := newTestChunker(t, int64(len(data)))
	defer c.Close()
	source.corrupt, source.corruptByte = true, int64(len(source.data)-1)
	if err := c.PrefetchFrame(t.Context(), 0, testFrameSize, source, ft); err == nil {
		t.Fatal("corrupt footer passed the replay barrier")
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	before := source.fetchCount.Load()
	if err := c.PrefetchFrame(canceled, 0, testFrameSize, source, ft); !errors.Is(err, context.Canceled) {
		t.Fatalf("wrong cancellation: %v", err)
	}
	if source.fetchCount.Load() != before {
		t.Fatal("canceled prefetch started I/O")
	}
	if err := c.PrefetchFrame(t.Context(), 0, testFrameSize/2, source, storage.UncompressedFrameTable); err == nil {
		t.Fatal("changed backing geometry accepted")
	}
}
