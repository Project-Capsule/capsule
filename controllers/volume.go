package controllers

import (
	"context"
	stderrors "errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	corevolume "github.com/geekgonecrazy/capsule/core/volume"
	capsulev1 "github.com/geekgonecrazy/capsule/models/capsule/v1"
	"github.com/geekgonecrazy/capsule/store"
)

// VolumeController implements capsule.v1.VolumeServiceServer.
type VolumeController struct {
	capsulev1.UnimplementedVolumeServiceServer
	Service *corevolume.Service
}

func (c *VolumeController) Create(ctx context.Context, req *capsulev1.VolumeCreateRequest) (*capsulev1.Volume, error) {
	v, err := c.Service.Create(ctx, req.GetName())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return v, nil
}

func (c *VolumeController) Get(ctx context.Context, req *capsulev1.VolumeGetRequest) (*capsulev1.Volume, error) {
	v, err := c.Service.Get(ctx, req.GetName())
	if err != nil {
		if stderrors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "volume %q not found", req.GetName())
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	return v, nil
}

func (c *VolumeController) List(ctx context.Context, _ *capsulev1.VolumeListRequest) (*capsulev1.VolumeListResponse, error) {
	vs, err := c.Service.List(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &capsulev1.VolumeListResponse{Volumes: vs}, nil
}

func (c *VolumeController) Delete(ctx context.Context, req *capsulev1.VolumeDeleteRequest) (*capsulev1.VolumeDeleteResponse, error) {
	if err := c.Service.Delete(ctx, req.GetName(), req.GetForce()); err != nil {
		if stderrors.Is(err, corevolume.ErrInUse) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &capsulev1.VolumeDeleteResponse{}, nil
}
