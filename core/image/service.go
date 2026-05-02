// Package image is the business logic for image-store operations:
// listing what's cached on this capsule and importing a streamed
// OCI/docker-save tarball into it. The runtime adapter does the actual
// containerd work; this layer translates between gRPC-shaped types and
// the runtime.ImageStore port.
package image

import (
	"context"
	"errors"
	"io"

	capsulev1 "github.com/geekgonecrazy/capsule/models/capsule/v1"
	"github.com/geekgonecrazy/capsule/runtime"
)

// ErrNoRuntime is returned when the service has no underlying ImageStore
// (e.g. dev-mode capsuled with no containerd reachable). Distinct from
// transport errors so the controller can surface a helpful message.
var ErrNoRuntime = errors.New("image runtime not available")

// Service is the image-store façade exposed to controllers.
type Service struct {
	store runtime.ImageStore
}

// New returns a Service backed by store. store may be nil; in that case
// every method returns ErrNoRuntime.
func New(store runtime.ImageStore) *Service {
	return &Service{store: store}
}

// List returns the capsule's cached images as proto records.
func (s *Service) List(ctx context.Context) ([]*capsulev1.Image, error) {
	if s.store == nil {
		return nil, ErrNoRuntime
	}
	imgs, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*capsulev1.Image, 0, len(imgs))
	for _, img := range imgs {
		out = append(out, &capsulev1.Image{
			Name:        img.Name,
			Digest:      img.Digest,
			SizeBytes:   img.Size,
			CreatedUnix: img.CreatedAt.Unix(),
		})
	}
	return out, nil
}

// Import streams a tar archive into the runtime's image store and
// returns the refs that were registered.
func (s *Service) Import(ctx context.Context, r io.Reader) ([]string, error) {
	if s.store == nil {
		return nil, ErrNoRuntime
	}
	return s.store.Import(ctx, r)
}
