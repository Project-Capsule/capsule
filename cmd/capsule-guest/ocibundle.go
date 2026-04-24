//go:build linux

package main

import (
	"encoding/json"
	"os"
	"path/filepath"

	rspec "github.com/opencontainers/runtime-spec/specs-go"
)

// extraMount describes a host-side path already mounted inside the
// payload rootfs (/oci/<dst>) that should be bind-mounted into the
// container at <dst>.
type extraMount struct {
	src      string // absolute path inside capsule-guest's namespace (e.g. /oci/data)
	dst      string // path inside the container (e.g. /data)
	readOnly bool
}

// writeOCIConfig synthesizes a minimal OCI runtime config (config.json)
// for the payload container and writes it to bundleDir/config.json.
//
// The container runs with:
//   - rootfs pointing at /oci (already-mounted OCI image filesystem)
//   - pid/ipc/uts/mount namespaces (network is shared with the VM so the
//     payload sees eth0 directly; the VM is the network boundary, not the
//     container)
//   - Docker-default capabilities set
//   - Standard OCI mounts (proc/sys/dev/pts/shm/mqueue)
//   - Any `extra` mounts bound in (volume ext4s already mounted under /oci)
func writeOCIConfig(bundleDir string, command, env []string, cwd string, extra []extraMount) error {
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		return err
	}

	caps := defaultCaps()
	spec := &rspec.Spec{
		Version: "1.0.2-dev",
		Process: &rspec.Process{
			Terminal: false,
			User:     rspec.User{UID: 0, GID: 0},
			Args:     command,
			Env:      ensurePath(env),
			Cwd:      cwd,
			Capabilities: &rspec.LinuxCapabilities{
				Bounding:    caps,
				Effective:   caps,
				Permitted:   caps,
				Ambient:     caps,
				Inheritable: caps,
			},
			NoNewPrivileges: true,
			Rlimits: []rspec.POSIXRlimit{
				{Type: "RLIMIT_NOFILE", Hard: 1048576, Soft: 1048576},
			},
		},
		Root: &rspec.Root{
			Path:     "/oci",
			Readonly: false,
		},
		Hostname: "payload",
		Mounts: []rspec.Mount{
			{Destination: "/proc", Type: "proc", Source: "proc"},
			{
				Destination: "/dev",
				Type:        "tmpfs",
				Source:      "tmpfs",
				Options:     []string{"nosuid", "strictatime", "mode=755", "size=65536k"},
			},
			{
				Destination: "/dev/pts",
				Type:        "devpts",
				Source:      "devpts",
				Options:     []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620", "gid=5"},
			},
			{
				Destination: "/dev/shm",
				Type:        "tmpfs",
				Source:      "shm",
				Options:     []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"},
			},
			{
				Destination: "/dev/mqueue",
				Type:        "mqueue",
				Source:      "mqueue",
				Options:     []string{"nosuid", "noexec", "nodev"},
			},
			{
				Destination: "/sys",
				Type:        "sysfs",
				Source:      "sysfs",
				Options:     []string{"nosuid", "noexec", "nodev", "ro"},
			},
		},
		Linux: &rspec.Linux{
			Namespaces: []rspec.LinuxNamespace{
				{Type: rspec.PIDNamespace},
				{Type: rspec.IPCNamespace},
				{Type: rspec.UTSNamespace},
				{Type: rspec.MountNamespace},
				// Network namespace deliberately omitted — payload shares
				// eth0 with the VM. VM boundary is the network boundary.
			},
			MaskedPaths: []string{
				"/proc/asound",
				"/proc/acpi",
				"/proc/kcore",
				"/proc/keys",
				"/proc/latency_stats",
				"/proc/timer_list",
				"/proc/timer_stats",
				"/proc/sched_debug",
				"/sys/firmware",
				"/proc/scsi",
			},
			ReadonlyPaths: []string{
				"/proc/bus",
				"/proc/fs",
				"/proc/irq",
				"/proc/sys",
				"/proc/sysrq-trigger",
			},
		},
	}

	// Append each extra mount as a rbind from capsule-guest's namespace
	// (where the block device is already mounted) into the container.
	for _, m := range extra {
		opts := []string{"rbind", "rprivate"}
		if m.readOnly {
			opts = append(opts, "ro")
		} else {
			opts = append(opts, "rw")
		}
		spec.Mounts = append(spec.Mounts, rspec.Mount{
			Destination: m.dst,
			Type:        "bind",
			Source:      m.src,
			Options:     opts,
		})
	}

	f, err := os.Create(filepath.Join(bundleDir, "config.json"))
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(spec)
}

// defaultCaps mirrors Docker's default bounding capability set — the
// payload is a regular app, not a host tool.
func defaultCaps() []string {
	return []string{
		"CAP_AUDIT_WRITE",
		"CAP_CHOWN",
		"CAP_DAC_OVERRIDE",
		"CAP_FOWNER",
		"CAP_FSETID",
		"CAP_KILL",
		"CAP_MKNOD",
		"CAP_NET_BIND_SERVICE",
		"CAP_NET_RAW",
		"CAP_SETFCAP",
		"CAP_SETGID",
		"CAP_SETPCAP",
		"CAP_SETUID",
		"CAP_SYS_CHROOT",
	}
}

// ensurePath makes sure the env list contains a PATH — some OCI images
// ship entrypoints that don't set it, and runc doesn't inject a default.
func ensurePath(env []string) []string {
	for _, e := range env {
		if len(e) >= 5 && e[:5] == "PATH=" {
			return env
		}
	}
	return append(env, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
}
