//go:build linux

package firecracker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/namespaces"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/geekgonecrazy/capsule/boot"
	capsulev1 "github.com/geekgonecrazy/capsule/models/capsule/v1"
)

// Paths baked into Capsule's rootfs.
//   - SharedKernel: Firecracker-compatible vmlinux, loaded into every VM.
//   - SharedRootfs: the shared ext4 used as /dev/vda for every capsule VM.
//     Contains capsule-guest as /sbin/init. Attached read-only so a single
//     image can back every running VM without copy-on-write overhead.
const (
	SharedKernel = "/usr/share/capsule/vmlinux"
	SharedRootfs = "/usr/share/capsule/vm-shared.ext4"
)

// VMImageNamespace is the containerd namespace capsule uses for pulled OCI
// images destined for microVMs. Kept distinct from "capsule" (the container
// namespace) so a container image cache doesn't get mixed with VM images.
const VMImageNamespace = "capsule-vm"

// OCIPayload is the metadata capsule-guest needs to exec the image's
// entrypoint after /dev/vdb is mounted. Sent over vsock as part of
// StartPayloadRequest.
type OCIPayload struct {
	Command []string
	Env     []string
	Cwd     string
}

// buildPayloadDisk pulls the OCI image (if needed), flattens its layers
// into a directory, and packs the directory into an ext4 image at
// /run/capsule/vms/<name>/payload.ext4. The shared VM rootfs (/dev/vda)
// handles init; this disk carries only the user's application payload
// and is mounted by capsule-guest at /oci inside the VM.
func (d *Driver) buildPayloadDisk(parentCtx context.Context, w *capsulev1.Workload, vmDir string) (string, *OCIPayload, error) {
	spec := w.GetMicroVm()
	ref := spec.GetImage()
	if ref == "" {
		return "", nil, fmt.Errorf("image is required for image-mode microVM")
	}

	client, err := d.containerd()
	if err != nil {
		return "", nil, err
	}
	ctx := namespaces.WithNamespace(parentCtx, VMImageNamespace)

	img, err := client.GetImage(ctx, ref)
	if err != nil {
		slog.Info("pulling vm image", "workload", w.GetName(), "image", ref)
		img, err = client.Pull(ctx, ref, containerd.WithPullUnpack)
		if err != nil {
			return "", nil, fmt.Errorf("pull %q: %w", ref, err)
		}
	}

	payloadDir := filepath.Join(vmDir, "payload")
	if err := os.RemoveAll(payloadDir); err != nil {
		return "", nil, fmt.Errorf("clean %s: %w", payloadDir, err)
	}
	if err := os.MkdirAll(payloadDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("mkdir %s: %w", payloadDir, err)
	}
	if err := unpackImageRootfs(ctx, client, img, payloadDir); err != nil {
		return "", nil, fmt.Errorf("unpack image: %w", err)
	}

	payload, err := buildOCIPayload(ctx, client, img, spec)
	if err != nil {
		return "", nil, fmt.Errorf("resolve image config: %w", err)
	}

	ext4Path := filepath.Join(vmDir, "payload.ext4")
	if err := makeExt4FromDir(payloadDir, ext4Path); err != nil {
		return "", nil, fmt.Errorf("build ext4: %w", err)
	}
	_ = os.RemoveAll(payloadDir)

	return ext4Path, payload, nil
}

// containerd returns a lazily-dialled containerd client used for VM image pulls.
func (d *Driver) containerd() (*containerd.Client, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ctrd == nil {
		c, err := containerd.New("/run/containerd/containerd.sock")
		if err != nil {
			return nil, fmt.Errorf("containerd dial: %w", err)
		}
		d.ctrd = c
	}
	return d.ctrd, nil
}

// unpackImageRootfs creates a read-only view of the image's rootfs via
// containerd's snapshotter and copies it into dst using Go (so we don't
// depend on /bin/cp semantics at all).
func unpackImageRootfs(ctx context.Context, client *containerd.Client, img containerd.Image, dst string) error {
	diffIDs, err := img.RootFS(ctx)
	if err != nil {
		return fmt.Errorf("image rootfs diff ids: %w", err)
	}
	chainID := identChainID(diffIDs)

	snapshotter := client.SnapshotService("overlayfs")
	key := "capsule-vm-unpack-" + img.Name()

	_ = snapshotter.Remove(ctx, key)

	mounts, err := snapshotter.View(ctx, key, chainID.String())
	if err != nil {
		return fmt.Errorf("snapshot view (chain=%s): %w", chainID.String(), err)
	}
	defer snapshotter.Remove(ctx, key) //nolint:errcheck

	mountDir, err := os.MkdirTemp("", "capsule-vm-mnt-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(mountDir)

	for _, m := range mounts {
		if err := m.Mount(mountDir); err != nil {
			return fmt.Errorf("mount snapshot: %w", err)
		}
	}
	defer func() {
		boot.ExecMu.Lock()
		defer boot.ExecMu.Unlock()
		_ = exec.Command("/bin/umount", mountDir).Run()
	}()

	if err := copyTree(mountDir, dst); err != nil {
		return fmt.Errorf("copy snapshot: %w", err)
	}
	return nil
}

// copyTree recursively copies everything under src into dst, preserving
// symlinks, permissions, and owners where possible.
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return err
			}
			_ = os.Remove(target)
			return os.Symlink(linkTarget, target)
		case info.IsDir():
			return os.MkdirAll(target, info.Mode()&os.ModePerm)
		case info.Mode().IsRegular():
			in, err := os.Open(path)
			if err != nil {
				return err
			}
			defer in.Close()
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode()&os.ModePerm)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, in); err != nil {
				out.Close()
				return err
			}
			return out.Close()
		default:
			return nil
		}
	})
}

// identChainID computes the OCI ChainID over a sequence of diff IDs.
func identChainID(diffIDs []digest.Digest) digest.Digest {
	if len(diffIDs) == 0 {
		return ""
	}
	if len(diffIDs) == 1 {
		return diffIDs[0]
	}
	chain := diffIDs[0]
	for _, d := range diffIDs[1:] {
		chain = digest.FromString(chain.String() + " " + d.String())
	}
	return chain
}

// buildOCIPayload reads the image config (entrypoint, cmd, env, workdir)
// and applies any spec-level overrides. Result is the argv+env the host
// sends over vsock in StartPayloadRequest.
func buildOCIPayload(ctx context.Context, client *containerd.Client, img containerd.Image, spec *capsulev1.MicroVMSpec) (*OCIPayload, error) {
	cfgDesc, err := images.Config(ctx, client.ContentStore(), img.Target(), img.Platform())
	if err != nil {
		return nil, fmt.Errorf("image config descriptor: %w", err)
	}
	raw, err := readContent(ctx, client, cfgDesc)
	if err != nil {
		return nil, err
	}

	var imageCfg struct {
		Config struct {
			Cmd        []string `json:"Cmd,omitempty"`
			Entrypoint []string `json:"Entrypoint,omitempty"`
			Env        []string `json:"Env,omitempty"`
			WorkingDir string   `json:"WorkingDir,omitempty"`
		} `json:"config"`
	}
	if err := json.Unmarshal(raw, &imageCfg); err != nil {
		return nil, fmt.Errorf("parse image config: %w", err)
	}

	entrypoint := imageCfg.Config.Entrypoint
	cmd := imageCfg.Config.Cmd
	if len(spec.GetCommand()) > 0 {
		entrypoint = spec.GetCommand()
	}
	if len(spec.GetArgs()) > 0 {
		cmd = spec.GetArgs()
	}
	argv := append([]string{}, entrypoint...)
	argv = append(argv, cmd...)
	if len(argv) == 0 {
		return nil, fmt.Errorf("image %s has no entrypoint or cmd", img.Name())
	}

	env := append([]string{}, imageCfg.Config.Env...)
	for k, v := range spec.GetEnv() {
		env = append(env, k+"="+v)
	}

	return &OCIPayload{
		Command: argv,
		Env:     env,
		Cwd:     imageCfg.Config.WorkingDir,
	}, nil
}

func readContent(ctx context.Context, client *containerd.Client, desc ocispec.Descriptor) ([]byte, error) {
	ra, err := client.ContentStore().ReaderAt(ctx, desc)
	if err != nil {
		return nil, err
	}
	defer ra.Close()
	return io.ReadAll(io.NewSectionReader(ra, 0, ra.Size()))
}

// makeExt4FromDir builds an ext4 filesystem image from dir using
// `mkfs.ext4 -d`. Size = content + max(50 MiB, 20%) of headroom.
func makeExt4FromDir(dir, out string) error {
	used, err := dirSizeBytes(dir)
	if err != nil {
		return err
	}
	hdr := int64(50 * 1024 * 1024)
	if used/5 > hdr {
		hdr = used / 5
	}
	total := used + hdr
	total = ((total + (1024*1024 - 1)) / (1024 * 1024)) * (1024 * 1024)

	if err := os.WriteFile(out, nil, 0o644); err != nil {
		return err
	}
	if err := os.Truncate(out, total); err != nil {
		return err
	}

	boot.ExecMu.Lock()
	defer boot.ExecMu.Unlock()
	cmd := exec.Command("/usr/sbin/mkfs.ext4", "-q", "-F", "-d", dir, out)
	o, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	cmd = exec.Command("/sbin/mkfs.ext4", "-q", "-F", "-d", dir, out)
	o2, err2 := cmd.CombinedOutput()
	if err2 != nil {
		return fmt.Errorf("mkfs.ext4: %w: %s / %s", err, string(o), string(o2))
	}
	return nil
}

func dirSizeBytes(root string) (int64, error) {
	var total int64
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}
