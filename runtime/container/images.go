package container

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/containerd/containerd"

	"github.com/geekgonecrazy/capsule/runtime"
)

// List enumerates every image cached in containerd's `capsule` namespace.
// Images appear here whether they were registry-pulled (by EnsureRunning)
// or operator-pushed (by Import).
func (d *Driver) List(parentCtx context.Context) ([]runtime.Image, error) {
	ctx := d.ctx(parentCtx)
	imgs, err := d.client.ListImages(ctx)
	if err != nil {
		return nil, fmt.Errorf("list images: %w", err)
	}
	out := make([]runtime.Image, 0, len(imgs))
	for _, img := range imgs {
		// Per-image Size can fail (missing content, partial pull); skip
		// the size rather than dropping the whole image — operators still
		// want to see the ref.
		size, err := img.Size(ctx)
		if err != nil {
			slog.Debug("image size", "ref", img.Name(), "err", err)
		}
		md := img.Metadata()
		out = append(out, runtime.Image{
			Name:      img.Name(),
			Digest:    img.Target().Digest.String(),
			Size:      size,
			CreatedAt: md.UpdatedAt,
		})
	}
	return out, nil
}

// Import streams an OCI / docker-save tarball from r into containerd's
// content store, then unpacks each discovered image into the default
// snapshotter so it's immediately ready to back a container.
func (d *Driver) Import(parentCtx context.Context, r io.Reader) ([]string, error) {
	ctx := d.ctx(parentCtx)
	imgs, err := d.client.Import(ctx, r)
	if err != nil {
		return nil, fmt.Errorf("import: %w", err)
	}
	refs := make([]string, 0, len(imgs))
	for _, m := range imgs {
		refs = append(refs, m.Name)
		img := containerd.NewImage(d.client, m)
		if err := img.Unpack(ctx, ""); err != nil {
			// Don't hard-fail the import — non-runnable refs (e.g. a
			// digest-only manifest entry from a multi-arch index) won't
			// unpack cleanly but are still legitimately stored.
			slog.Warn("image unpack", "ref", m.Name, "err", err)
			continue
		}
		slog.Info("image imported", "ref", m.Name)
	}
	return refs, nil
}
