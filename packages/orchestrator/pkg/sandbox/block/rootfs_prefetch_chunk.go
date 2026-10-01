//go:build linux

package block

import (
	"context"
	"fmt"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
)

// PrefetchFrame waits through reader close/CRC, not merely the first available
// page. Shared fetches retain their existing cache-owned timeout on cancellation:
// other sandboxes can be waiting on them. The replay stops scheduling immediately.
func (c *Chunker) PrefetchFrame(ctx context.Context, off, length int64, upstream storage.RangeOpener, ft *storage.FrameTable) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	start, size, err := c.locateChunk(off, ft)
	if err != nil {
		return err
	}
	if start != off || size != length || size <= 0 || size > rootfsMaxFrameBytes {
		return fmt.Errorf("rootfs prefetch frame geometry changed")
	}
	s, cached := c.getOrCreateSession(ctx, off, length, upstream, ft)
	if cached {
		return nil
	}
	stop := context.AfterFunc(ctx, func() { s.mu.Lock(); defer s.mu.Unlock(); s.cond.Broadcast() })
	defer stop()
	s.mu.Lock()
	defer s.mu.Unlock()
	for !s.done && ctx.Err() == nil {
		s.cond.Wait()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.fetchErr
}
