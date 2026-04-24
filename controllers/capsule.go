package controllers

import (
	"context"
	"os"

	capsulev1 "github.com/geekgonecrazy/capsule/models/capsule/v1"
)

// CapsuleController implements capsule.v1.CapsuleServiceServer.
// For phase 0 it reads live info directly from the kernel; later phases
// will move the data-source bits into core/ and accept them via fields.
type CapsuleController struct {
	capsulev1.UnimplementedCapsuleServiceServer

	// CapsuleVersion is the build-time version string baked into the binary.
	CapsuleVersion string
	// ActiveSlot is the currently active A/B slot identifier. Empty until
	// A/B updates ship (phase 3).
	ActiveSlot string
}

func (c *CapsuleController) GetInfo(ctx context.Context, _ *capsulev1.GetInfoRequest) (*capsulev1.GetInfoResponse, error) {
	hostname, _ := os.Hostname()
	u := uname()
	uptime := uptimeSeconds()

	return &capsulev1.GetInfoResponse{
		Hostname:      hostname,
		KernelRelease: u.release,
		KernelVersion: u.version,
		Architecture:  u.machine,
		UptimeSeconds: uptime,
		CapsuleVersion:   c.CapsuleVersion,
		ActiveSlot:    c.ActiveSlot,
	}, nil
}

type unameInfo struct {
	release string
	version string
	machine string
}
