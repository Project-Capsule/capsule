//go:build linux

package firecracker

import (
	"fmt"
	"os"

	fc "github.com/firecracker-microvm/firecracker-go-sdk"
	fcmodels "github.com/firecracker-microvm/firecracker-go-sdk/client/models"

	corevolume "github.com/geekgonecrazy/capsule/core/volume"
	capsulev1 "github.com/geekgonecrazy/capsule/models/capsule/v1"
)

// sanitizeID maps a Capsule volume name to a Firecracker-legal DriveID.
// Firecracker allows [a-zA-Z0-9_] only; everything else becomes "_".
func sanitizeID(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}

// prepareVolumeDrives takes the workload's declared volume mounts,
// verifies each volume's LV block device exists, appends it to the
// Firecracker drive list as /dev/vdN, and returns the matching guest-side
// VolumeMount list (device + mount_path + fstype) for capsule-guest's
// StartPayloadRequest.
//
// Drive-letter mapping:
//
//	vda = shared rootfs (index 0)
//	vdb = payload (index 1)
//	vdc, vdd, ... = user volumes, in spec order
//
// Volumes must already exist (created via `capsulectl volume create`).
// A missing LV is a hard error — unlike the pre-LVM world, we don't
// silently materialize volumes here; that's the volume service's job and
// hiding it caused mount-time surprises.
func (d *Driver) prepareVolumeDrives(mounts []*capsulev1.VolumeMount, drives *[]fcmodels.Drive) ([]*capsulev1.GuestVolumeMount, error) {
	if len(mounts) == 0 {
		return nil, nil
	}
	out := make([]*capsulev1.GuestVolumeMount, 0, len(mounts))
	letter := byte('c')
	for i, m := range mounts {
		name := m.GetVolumeName()
		mountPath := m.GetMountPath()
		if name == "" || mountPath == "" {
			return nil, fmt.Errorf("mount[%d]: volume_name and mount_path are required", i)
		}
		dev := corevolume.HostPath(name)
		if _, err := os.Stat(dev); err != nil {
			return nil, fmt.Errorf("volume %q: %w (run `capsulectl volume create %s`)", name, err, name)
		}
		*drives = append(*drives, fcmodels.Drive{
			DriveID:      fc.String("vol_" + sanitizeID(name)),
			PathOnHost:   fc.String(dev),
			IsRootDevice: fc.Bool(false),
			IsReadOnly:   fc.Bool(m.GetReadOnly()),
		})
		out = append(out, &capsulev1.GuestVolumeMount{
			Device:    "/dev/vd" + string(letter),
			MountPath: mountPath,
			Fstype:    "ext4",
			ReadOnly:  m.GetReadOnly(),
		})
		letter++
	}
	return out, nil
}
