//go:build linux

package build

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block"
)

func (b *File) PrefetchRootfs(ctx context.Context, mapping *block.RootfsPrefetchMapping) error {
	if b.fileType != Rootfs {
		return fmt.Errorf("rootfs prefetch requires a rootfs device")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	frames, err := mapping.Frames(ctx, b.Header())
	if err != nil {
		return err
	}
	g, fetchCtx := errgroup.WithContext(ctx)
	g.SetLimit(8)
	for _, frame := range frames {
		if fetchCtx.Err() != nil {
			break
		}
		g.Go(func() error {
			ctx := fetchCtx
			if err := ctx.Err(); err != nil {
				return err
			}
			d, err := b.getBuild(ctx, frame.BuildID)
			if err != nil {
				return err
			}
			if remote, ok := d.(*StorageDiff); ok {
				up, ft, err := remote.resolve(ctx, frame.FrameTable)
				if err != nil {
					return err
				}
				encoded := frame.Length
				if ft.IsCompressed() {
					c, err := ft.LocateCompressed(frame.Offset)
					if err != nil {
						return err
					}
					encoded = int64(c.Length)
				}
				if encoded > frame.EncodedBytes {
					return fmt.Errorf("rootfs prefetch backing byte budget changed")
				}
				return remote.chunker.PrefetchFrame(ctx, frame.Offset, frame.Length, up, ft)
			}
			// Local build diffs are already resident; Slice neither copies nor fetches.
			_, err = d.Slice(ctx, frame.Offset, frame.Length, frame.FrameTable)
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	return ctx.Err()
}
