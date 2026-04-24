// Package container is the containerd-backed ContainerDriver implementation.
package container

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/oci"
	gocni "github.com/containerd/go-cni"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/geekgonecrazy/capsule/boot"

	capsulev1 "github.com/geekgonecrazy/capsule/models/capsule/v1"
	"github.com/geekgonecrazy/capsule/runtime"
)

// execSeq uniquifies exec IDs within a capsuled process. capsuled itself is PID 1
// so we can't rely on os.Getpid() for uniqueness. Combining a nanosecond
// boot-time prefix with a monotonic counter keeps IDs unique across an
// instance's lifetime.
var execSeq atomic.Uint64

// LogDir is where per-workload combined stdout+stderr logs live on the
// capsule. Ephemeral (tmpfs); the containerd image cache on /perm is the
// persistent part. Phase-2 volume mounts will share space with this dir.
const LogDir = "/run/capsule/workloads"

// Namespace is the containerd namespace capsule uses for every container it
// manages. Keeping a dedicated namespace avoids clashing with any other
// tool a human might run on the capsule for debugging.
const Namespace = "capsule"

// Driver implements runtime.ContainerDriver by talking to containerd over
// its Unix socket.
type Driver struct {
	socket string
	client *containerd.Client

	cniOnce sync.Once
	cni     gocni.CNI
	cniErr  error
}

// BridgeLabel is a containerd label marking which workloads were set up
// with CNI bridge networking. We set it on create so Remove can tell
// whether to run CNI teardown without needing external state.
const BridgeLabel = "capsule.network/mode"

// New dials containerd at socket and returns a Driver. Caller must Close
// when done.
func New(socket string) (*Driver, error) {
	if socket == "" {
		socket = "/run/containerd/containerd.sock"
	}
	c, err := containerd.New(socket)
	if err != nil {
		return nil, fmt.Errorf("containerd dial %s: %w", socket, err)
	}
	return &Driver{socket: socket, client: c}, nil
}

// Close releases the containerd connection.
func (d *Driver) Close() error { return d.client.Close() }

func (d *Driver) ctx(parent context.Context) context.Context {
	return namespaces.WithNamespace(parent, Namespace)
}

// EnsureRunning — idempotently pull, create, start.
func (d *Driver) EnsureRunning(parentCtx context.Context, w *capsulev1.Workload) error {
	if w.GetContainer() == nil {
		return fmt.Errorf("workload %q has no container spec", w.GetName())
	}
	ctx := d.ctx(parentCtx)

	name := w.GetName()
	spec := w.GetContainer()

	// 1. Already running?
	if existing, err := d.client.LoadContainer(ctx, name); err == nil {
		task, err := existing.Task(ctx, nil)
		if err == nil {
			status, err := task.Status(ctx)
			if err == nil && status.Status == containerd.Running {
				return nil // nothing to do
			}
			// Task exists but is stopped — tear it down so we recreate.
			_, _ = task.Delete(ctx, containerd.WithProcessKill)
		}
		// Container exists, task doesn't — delete and recreate to be safe.
		if err := existing.Delete(ctx, containerd.WithSnapshotCleanup); err != nil {
			return fmt.Errorf("delete stale container %q: %w", name, err)
		}
	} else if !errdefs.IsNotFound(err) {
		return fmt.Errorf("load container %q: %w", name, err)
	}

	// 2. Pull image if needed.
	image, err := d.client.GetImage(ctx, spec.GetImage())
	if err != nil {
		if !errdefs.IsNotFound(err) {
			return fmt.Errorf("get image %q: %w", spec.GetImage(), err)
		}
		slog.Info("pulling image", "workload", name, "image", spec.GetImage())
		image, err = d.client.Pull(ctx, spec.GetImage(), containerd.WithPullUnpack)
		if err != nil {
			return fmt.Errorf("pull image %q: %w", spec.GetImage(), err)
		}
	}

	// 3. Build OCI runtime spec.
	specOpts := []oci.SpecOpts{
		oci.WithImageConfig(image),
	}
	if args := processArgs(spec); len(args) > 0 {
		specOpts = append(specOpts, oci.WithProcessArgs(args...))
	}
	if env := envSlice(spec); len(env) > 0 {
		specOpts = append(specOpts, oci.WithEnv(env))
	}
	// Loop-mount each declared volume's ext4 file on the capsule so we can
	// bind-mount it into the container. If anything fails here we roll
	// back any mounts we already did, else they'd leak.
	mountedVols, err := mountContainerVolumes(name, spec.GetMounts())
	if err != nil {
		unmountContainerVolumes(name, mountedVols)
		return fmt.Errorf("mount volumes: %w", err)
	}
	if mounts := bindMounts(spec, mountedVols); len(mounts) > 0 {
		specOpts = append(specOpts, oci.WithMounts(mounts))
	}

	switch spec.GetNetworkMode() {
	case capsulev1.NetworkMode_NETWORK_MODE_HOST:
		// Share the host network namespace and network config files so
		// the container can bind to the capsule's interfaces directly.
		specOpts = append(specOpts,
			oci.WithHostNamespace(specs.NetworkNamespace),
			oci.WithHostHostsFile,
			oci.WithHostResolvconf,
		)
	case capsulev1.NetworkMode_NETWORK_MODE_BRIDGE:
		// Bridge mode uses the default new-netns behavior; CNI configures
		// the netns after the task is created but before it starts.
	default:
		// Leave containerd's default: private netns with only loopback.
	}

	// Attach a label indicating network mode so Remove can make the right
	// teardown decisions without consulting external state.
	containerOpts := []containerd.NewContainerOpts{
		containerd.WithImage(image),
		containerd.WithNewSnapshot(name+"-snapshot", image),
		containerd.WithNewSpec(specOpts...),
		containerd.WithContainerLabels(map[string]string{
			BridgeLabel: networkModeLabel(spec.GetNetworkMode()),
		}),
	}
	_ = containerOpts // used below in the NewContainer call

	// 4. Create container.
	container, err := d.client.NewContainer(ctx, name, containerOpts...)
	if err != nil {
		return fmt.Errorf("create container %q: %w", name, err)
	}

	// 5. Create + start task. Combined stdout+stderr goes to a per-workload
	// log file under /run/capsule/workloads/<name>/combined.log. cio.LogFile
	// manages the FIFO setup for us.
	logPath := d.LogPath(name)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		_ = container.Delete(ctx, containerd.WithSnapshotCleanup)
		return fmt.Errorf("mkdir logs %q: %w", filepath.Dir(logPath), err)
	}
	task, err := container.NewTask(ctx, cio.LogFile(logPath))
	if err != nil {
		// Clean up the container we just made.
		_ = container.Delete(ctx, containerd.WithSnapshotCleanup)
		return fmt.Errorf("new task %q: %w", name, err)
	}

	// 6. For BRIDGE mode, invoke CNI against the task's netns BEFORE
	// starting the process. This creates a veth pair, attaches it to
	// the host bridge, installs routes, and sets up portmap DNAT rules.
	if spec.GetNetworkMode() == capsulev1.NetworkMode_NETWORK_MODE_BRIDGE {
		if err := d.cniSetup(ctx, name, task.Pid(), spec.GetPorts()); err != nil {
			_, _ = task.Delete(ctx, containerd.WithProcessKill)
			_ = container.Delete(ctx, containerd.WithSnapshotCleanup)
			return fmt.Errorf("cni setup %q: %w", name, err)
		}
	}

	if err := task.Start(ctx); err != nil {
		// Teardown bridge networking too, if we set it up.
		if spec.GetNetworkMode() == capsulev1.NetworkMode_NETWORK_MODE_BRIDGE {
			_ = d.cniTeardown(ctx, name, task.Pid())
		}
		_, _ = task.Delete(ctx, containerd.WithProcessKill)
		_ = container.Delete(ctx, containerd.WithSnapshotCleanup)
		return fmt.Errorf("start task %q: %w", name, err)
	}
	slog.Info("container started", "workload", name, "network_mode", spec.GetNetworkMode().String())
	return nil
}

// networkModeLabel flattens the proto enum to a label-friendly string.
func networkModeLabel(m capsulev1.NetworkMode) string {
	switch m {
	case capsulev1.NetworkMode_NETWORK_MODE_HOST:
		return "host"
	case capsulev1.NetworkMode_NETWORK_MODE_BRIDGE:
		return "bridge"
	default:
		return "none"
	}
}

// ensureCNI lazy-initialises the CNI client the first time bridge mode
// is used. Errors are cached so subsequent requests don't re-try the
// expensive init every time.
func (d *Driver) ensureCNI() (gocni.CNI, error) {
	d.cniOnce.Do(func() {
		cn, err := gocni.New(
			gocni.WithMinNetworkCount(1),
			gocni.WithPluginConfDir("/etc/cni/net.d"),
			gocni.WithPluginDir([]string{"/opt/cni/bin", "/usr/libexec/cni"}),
			gocni.WithInterfacePrefix("eth"),
		)
		if err != nil {
			d.cniErr = err
			return
		}
		if err := cn.Load(gocni.WithLoNetwork, gocni.WithDefaultConf); err != nil {
			d.cniErr = err
			return
		}
		d.cni = cn
	})
	return d.cni, d.cniErr
}

func (d *Driver) cniSetup(ctx context.Context, name string, pid uint32, ports []*capsulev1.PortMapping) error {
	cn, err := d.ensureCNI()
	if err != nil {
		return err
	}
	netnsPath := fmt.Sprintf("/proc/%d/ns/net", pid)

	opts := []gocni.NamespaceOpts{}
	if len(ports) > 0 {
		mappings := make([]gocni.PortMapping, 0, len(ports))
		for _, p := range ports {
			proto := strings.ToLower(p.GetProtocol())
			if proto == "" {
				proto = "tcp"
			}
			mappings = append(mappings, gocni.PortMapping{
				HostPort:      int32(p.GetHostPort()),
				ContainerPort: int32(p.GetContainerPort()),
				Protocol:      proto,
			})
		}
		opts = append(opts, gocni.WithCapabilityPortMap(mappings))
	}

	_, err = cn.Setup(ctx, name, netnsPath, opts...)
	return err
}

func (d *Driver) cniTeardown(ctx context.Context, name string, pid uint32) error {
	cn, err := d.ensureCNI()
	if err != nil {
		return err
	}
	netnsPath := fmt.Sprintf("/proc/%d/ns/net", pid)
	return cn.Remove(ctx, name, netnsPath)
}

// Remove — idempotently stop and delete.
func (d *Driver) Remove(parentCtx context.Context, name string) error {
	ctx := d.ctx(parentCtx)
	c, err := d.client.LoadContainer(ctx, name)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("load container %q: %w", name, err)
	}

	// Check label to decide whether CNI teardown is needed. We have to
	// run teardown BEFORE deleting the task because CNI needs the netns
	// path (derived from the task's PID).
	info, infoErr := c.Info(ctx)
	needsCNI := infoErr == nil && info.Labels[BridgeLabel] == "bridge"

	if task, err := c.Task(ctx, nil); err == nil {
		if needsCNI {
			if terr := d.cniTeardown(ctx, name, task.Pid()); terr != nil {
				slog.Warn("cni teardown", "workload", name, "err", terr)
			}
		}
		if _, derr := task.Delete(ctx, containerd.WithProcessKill); derr != nil {
			slog.Warn("task delete", "workload", name, "err", derr)
		}
	} else if !errdefs.IsNotFound(err) {
		slog.Warn("task lookup", "workload", name, "err", err)
	}
	if err := c.Delete(ctx, containerd.WithSnapshotCleanup); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("delete container %q: %w", name, err)
	}
	// Unmount any per-workload loop-mounted volumes. We don't have the
	// mapping in memory (process may have restarted); pass nil and let
	// unmountContainerVolumes scan /run/capsule/mounts/<name>/ itself.
	unmountContainerVolumes(name, nil)
	return nil
}

// Status — observed phase for a given workload name.
func (d *Driver) Status(parentCtx context.Context, name string) (runtime.Status, error) {
	ctx := d.ctx(parentCtx)
	c, err := d.client.LoadContainer(ctx, name)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return runtime.Status{Phase: runtime.PhaseUnknown}, nil
		}
		return runtime.Status{}, err
	}
	task, err := c.Task(ctx, nil)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return runtime.Status{Phase: runtime.PhaseStopped, Message: "no task"}, nil
		}
		return runtime.Status{}, err
	}
	st, err := task.Status(ctx)
	if err != nil {
		return runtime.Status{}, err
	}
	switch st.Status {
	case containerd.Running:
		return runtime.Status{Phase: runtime.PhaseRunning}, nil
	case containerd.Created, containerd.Pausing, containerd.Paused:
		return runtime.Status{Phase: runtime.PhasePending, Message: string(st.Status)}, nil
	case containerd.Stopped:
		msg := fmt.Sprintf("exited %d", st.ExitStatus)
		if st.ExitStatus != 0 {
			return runtime.Status{Phase: runtime.PhaseFailed, Message: msg}, nil
		}
		return runtime.Status{Phase: runtime.PhaseStopped, Message: msg}, nil
	}
	return runtime.Status{Phase: runtime.PhaseUnknown, Message: string(st.Status)}, nil
}

// LogPath returns the combined-log file path for a workload, regardless
// of whether the workload is currently running.
func (d *Driver) LogPath(name string) string {
	return filepath.Join(LogDir, name, "combined.log")
}

// Exec runs a one-shot process inside the workload's running container.
// Returns the exit code and any error from the exec itself.
func (d *Driver) Exec(parentCtx context.Context, req runtime.ExecRequest) (int, error) {
	if len(req.Command) == 0 {
		return -1, errors.New("exec: command is required")
	}
	ctx := d.ctx(parentCtx)

	c, err := d.client.LoadContainer(ctx, req.Name)
	if err != nil {
		return -1, fmt.Errorf("load container %q: %w", req.Name, err)
	}
	task, err := c.Task(ctx, nil)
	if err != nil {
		return -1, fmt.Errorf("get task %q: %w", req.Name, err)
	}

	// Build the process spec: start from the container's default, then
	// override Args/Env/Terminal.
	info, err := c.Info(ctx)
	if err != nil {
		return -1, fmt.Errorf("container info: %w", err)
	}
	_ = info // reserved for future use (e.g. merging container's default env)

	spec, err := c.Spec(ctx)
	if err != nil {
		return -1, fmt.Errorf("container spec: %w", err)
	}
	pspec := *spec.Process
	pspec.Args = req.Command
	pspec.Terminal = req.TTY
	if len(req.Env) > 0 {
		pspec.Env = mergeEnv(pspec.Env, req.Env)
	}

	// Build cio.Creator wired to the caller's streams.
	creator := execIO(req)

	execID := fmt.Sprintf("capsule-exec-%d-%d", time.Now().UnixNano(), execSeq.Add(1))
	process, err := task.Exec(ctx, execID, &pspec, creator)
	if err != nil {
		return -1, fmt.Errorf("task exec: %w", err)
	}
	defer func() { _, _ = process.Delete(ctx) }()

	statusCh, err := process.Wait(ctx)
	if err != nil {
		return -1, fmt.Errorf("process wait: %w", err)
	}

	if err := process.Start(ctx); err != nil {
		return -1, fmt.Errorf("process start: %w", err)
	}

	// Forward resize events while the process runs.
	if req.TTY && req.ResizeCh != nil {
		go func() {
			for sz := range req.ResizeCh {
				// containerd.ConsoleSize expects (width, height).
				_ = process.Resize(ctx, uint32(sz.Cols), uint32(sz.Rows))
			}
		}()
	}

	select {
	case <-ctx.Done():
		// Best-effort kill; ignore errors.
		_ = process.Kill(ctx, syscall.SIGTERM)
		return -1, ctx.Err()
	case es := <-statusCh:
		code, _, err := es.Result()
		return int(code), err
	}
}

// execIO wires an ExecRequest's Stdin/Stdout/Stderr into a cio.Creator.
func execIO(req runtime.ExecRequest) cio.Creator {
	opts := []cio.Opt{}
	if req.TTY {
		opts = append(opts, cio.WithTerminal)
	}
	stdin := req.Stdin
	stdout := req.Stdout
	stderr := req.Stderr
	if stderr == nil {
		// cio.WithStreams requires non-nil writers. Use discard for unused.
		stderr = nopWriter{}
	}
	if stdout == nil {
		stdout = nopWriter{}
	}
	opts = append(opts, cio.WithStreams(stdin, stdout, stderr))
	return cio.NewCreator(opts...)
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// Volume paths on the capsule:
//   - backing ext4 file: /perm/volumes/<name>.ext4
//   - per-workload mount point: /run/capsule/mounts/<workload>/<name>
//
// Each container workload loop-mounts its declared volume ext4 files at
// the per-workload mount points; the OCI runtime spec then bind-mounts
// that capsule path into the container at the requested mount_path.
// Mirrors the VM-side path (same .ext4 file, attached as virtio-blk).
const (
	volumeBackingDir = "/perm/volumes"
	volumeMountDir   = "/run/capsule/mounts"
)

// mountContainerVolumes loop-mounts each declared ext4 volume at
// /run/capsule/mounts/<workload>/<volumeName>. Returns the list of
// (volumeName → mount path) entries for bindMounts to consume.
// Caller must call unmountContainerVolumes on error or container teardown.
func mountContainerVolumes(workload string, declared []*capsulev1.VolumeMount) (map[string]string, error) {
	if len(declared) == 0 {
		return nil, nil
	}
	mounted := map[string]string{}
	for _, m := range declared {
		vol := m.GetVolumeName()
		if vol == "" {
			continue
		}
		ext4 := filepath.Join(volumeBackingDir, vol+".ext4")
		if _, err := os.Stat(ext4); err != nil {
			return mounted, fmt.Errorf("volume %q: %w (run `capsulectl volume create %s`)", vol, err, vol)
		}
		mp := filepath.Join(volumeMountDir, workload, vol)
		if err := os.MkdirAll(mp, 0o755); err != nil {
			return mounted, fmt.Errorf("mkdir %s: %w", mp, err)
		}
		opts := []string{"loop"}
		if m.GetReadOnly() {
			opts = append(opts, "ro")
		}
		if err := runMount("-o", strings.Join(opts, ","), ext4, mp); err != nil {
			return mounted, fmt.Errorf("loop-mount %s at %s: %w", ext4, mp, err)
		}
		mounted[vol] = mp
	}
	return mounted, nil
}

// unmountContainerVolumes reverses mountContainerVolumes. Idempotent:
// skips mounts that aren't actually mounted. Called on container delete
// and on EnsureRunning failure paths.
func unmountContainerVolumes(workload string, mounted map[string]string) {
	if len(mounted) == 0 {
		// Best-effort recovery if we have no in-memory state: scan the
		// workload's mount dir and unmount everything under it.
		root := filepath.Join(volumeMountDir, workload)
		entries, err := os.ReadDir(root)
		if err != nil {
			return
		}
		for _, e := range entries {
			_ = runUmount(filepath.Join(root, e.Name()))
			_ = os.Remove(filepath.Join(root, e.Name()))
		}
		_ = os.Remove(root)
		return
	}
	for _, mp := range mounted {
		_ = runUmount(mp)
		_ = os.Remove(mp)
	}
	_ = os.Remove(filepath.Join(volumeMountDir, workload))
}

// bindMounts translates workload VolumeMounts into OCI bind-mount specs.
// Source is the per-workload loop-mount point the driver already set up
// via mountContainerVolumes; destination is the path inside the container.
func bindMounts(spec *capsulev1.ContainerSpec, mounted map[string]string) []specs.Mount {
	var out []specs.Mount
	for _, m := range spec.GetMounts() {
		vol := m.GetVolumeName()
		src, ok := mounted[vol]
		if !ok || m.GetMountPath() == "" {
			continue
		}
		opts := []string{"rbind"}
		if m.GetReadOnly() {
			opts = append(opts, "ro")
		} else {
			opts = append(opts, "rw")
		}
		out = append(out, specs.Mount{
			Type:        "bind",
			Source:      src,
			Destination: m.GetMountPath(),
			Options:     opts,
		})
	}
	return out
}

func runMount(args ...string) error {
	boot.ExecMu.Lock()
	defer boot.ExecMu.Unlock()
	out, err := exec.Command("/bin/mount", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mount %v: %w: %s", args, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func runUmount(mountPath string) error {
	boot.ExecMu.Lock()
	defer boot.ExecMu.Unlock()
	out, err := exec.Command("/bin/umount", mountPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("umount %s: %w: %s", mountPath, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func mergeEnv(base []string, extra map[string]string) []string {
	out := make([]string, 0, len(base)+len(extra))
	seen := map[string]bool{}
	for k := range extra {
		seen[k] = false
	}
	for _, e := range base {
		if i := strings.IndexByte(e, '='); i > 0 {
			k := e[:i]
			if _, ok := extra[k]; ok {
				out = append(out, k+"="+extra[k])
				seen[k] = true
				continue
			}
		}
		out = append(out, e)
	}
	for k, v := range extra {
		if !seen[k] {
			out = append(out, k+"="+v)
		}
	}
	return out
}

func processArgs(spec *capsulev1.ContainerSpec) []string {
	// If Command is set, it overrides the image entrypoint. If only Args
	// is set, those replace the image's default args. This mirrors
	// Kubernetes' conventions (Command == entrypoint, Args == cmd).
	if len(spec.GetCommand()) > 0 {
		return append(append([]string{}, spec.GetCommand()...), spec.GetArgs()...)
	}
	return spec.GetArgs()
}

func envSlice(spec *capsulev1.ContainerSpec) []string {
	if len(spec.GetEnv()) == 0 {
		return nil
	}
	out := make([]string, 0, len(spec.GetEnv()))
	for k, v := range spec.GetEnv() {
		out = append(out, k+"="+v)
	}
	return out
}

// Silence unused import warnings when strings isn't needed; kept for
// forward use (env parsing, name validation).
var _ = strings.ToLower

// Sentinel for callers that want to distinguish "runtime has no record of
// this workload" from other errors; currently used internally.
var errNotFound = errors.New("container not found") //nolint:unused
