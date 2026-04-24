//go:build linux

package firecracker

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	fc "github.com/firecracker-microvm/firecracker-go-sdk"
	fcmodels "github.com/firecracker-microvm/firecracker-go-sdk/client/models"

	"github.com/geekgonecrazy/capsule/boot"
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

// volumeExt4Root is where per-VM volume ext4 images live. Parallel to
// /perm/volumes/<name>/ (which is the container bind-mount target);
// VMs use a separate ext4 file per volume so virtio-blk can attach it.
const volumeExt4Root = "/perm/volumes"

// defaultVolumeSizeMiB is the size of a freshly-created VM volume ext4.
// Operators can grow later via `resize2fs` on the detached file.
const defaultVolumeSizeMiB = 512

// prepareVolumeDrives takes the workload's declared volume mounts,
// makes sure an ext4 file exists for each, appends it to the
// Firecracker drive list as /dev/vdN, and returns the matching
// guest-side VolumeMount list (device + mount_path + fstype) for
// capsule-guest's StartPayloadRequest.
//
// Drive-letter mapping:
//
//	vda = shared rootfs (index 0)
//	vdb = payload (index 1)
//	vdc, vdd, ... = user volumes, in spec order
func (d *Driver) prepareVolumeDrives(mounts []*capsulev1.VolumeMount, drives *[]fcmodels.Drive) ([]*capsulev1.GuestVolumeMount, error) {
	if len(mounts) == 0 {
		return nil, nil
	}
	out := make([]*capsulev1.GuestVolumeMount, 0, len(mounts))
	// Drive ids/letters start after rootfs+payload: c, d, e...
	letter := byte('c')
	for i, m := range mounts {
		name := m.GetVolumeName()
		mountPath := m.GetMountPath()
		if name == "" || mountPath == "" {
			return nil, fmt.Errorf("mount[%d]: volume_name and mount_path are required", i)
		}
		ext4Path, err := ensureVolumeExt4(name, defaultVolumeSizeMiB)
		if err != nil {
			return nil, fmt.Errorf("volume %q: %w", name, err)
		}
		*drives = append(*drives, fcmodels.Drive{
			// Firecracker constrains DriveID to [a-zA-Z0-9_] — sanitize.
			DriveID:      fc.String("vol_" + sanitizeID(name)),
			PathOnHost:   fc.String(ext4Path),
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

// ensureVolumeExt4 returns the path to the ext4 backing file for the
// named volume, creating + formatting it on first use. The file lives
// at /perm/volumes/<name>.ext4 (distinct from the container bind-mount
// directory at /perm/volumes/<name>/).
func ensureVolumeExt4(name string, sizeMiB int) (string, error) {
	path := filepath.Join(volumeExt4Root, name+".ext4")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}

	if err := os.MkdirAll(volumeExt4Root, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		return "", err
	}
	if err := os.Truncate(path, int64(sizeMiB)*1024*1024); err != nil {
		return "", err
	}
	boot.ExecMu.Lock()
	defer boot.ExecMu.Unlock()
	cmd := exec.Command("/usr/sbin/mkfs.ext4", "-q", "-F", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		cmd = exec.Command("/sbin/mkfs.ext4", "-q", "-F", path)
		if out2, err2 := cmd.CombinedOutput(); err2 != nil {
			_ = os.Remove(path)
			return "", fmt.Errorf("mkfs.ext4 %s: %w: %s / %s", path, err, string(out), string(out2))
		}
	}
	return path, nil
}
