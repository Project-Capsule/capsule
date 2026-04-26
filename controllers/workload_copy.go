package controllers

import (
	"bytes"
	"io"
	"path"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	capsulev1 "github.com/geekgonecrazy/capsule/models/capsule/v1"
	"github.com/geekgonecrazy/capsule/runtime"
)

// CopyTo extracts a tar archive streamed by the client into the
// workload at metadata.dest_path. Pipes the bytes into a shell helper
// that picks "extract-into" vs "extract-with-rename" based on whether
// dest_path ends with '/' or is an existing directory inside the
// workload — same UX as cp/scp/docker-cp. Goes through the existing
// exec path so it works uniformly for containers and microVMs.
//
// TODO(cp-architecture): this exec-tar approach is a stopgap. It's
// the same pattern kubectl cp uses, with the same drawbacks: requires
// /bin/sh + tar in the workload (so `scratch`/distroless images don't
// work), depends on stdin-EOF propagation through containerd's cio
// (a recurring source of bugs — see runtime/container/driver.go's
// eofSignalReader), and can't operate on stopped workloads. docker
// and podman do this the right way: native filesystem access via
// the snapshotter's overlay mount, with tar pack/unpack happening
// host-side in the daemon. Plan: rewrite CopyTo/CopyFrom to do
// host-side packing for containers (containerd snapshot mount) and
// add native CopyTo/CopyFrom RPCs to capsule-guest for microVMs.
// Wire format (tar) stays the same so the proto doesn't need to change.
func (c *WorkloadController) CopyTo(stream capsulev1.WorkloadService_CopyToServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	md := first.GetMetadata()
	if md == nil {
		return status.Error(codes.InvalidArgument, "first CopyTo message must carry metadata")
	}
	if md.GetName() == "" {
		return status.Error(codes.InvalidArgument, "metadata.name is required")
	}
	dest := md.GetDestPath()
	if dest == "" || !path.IsAbs(dest) {
		return status.Error(codes.InvalidArgument, "metadata.dest_path must be an absolute path")
	}

	stdinR, stdinW := io.Pipe()
	var outBuf, errBuf bytes.Buffer

	// Pump received chunks → stdin pipe. When the client closes-send,
	// stream.Recv returns EOF, the goroutine closes stdinW, and tar
	// sees EOF and exits. recvDone is closed once the goroutine has
	// applied its final byte count, so we can read bytesIn race-free
	// after waiting on it.
	var bytesIn int64
	recvDone := make(chan error, 1)
	go func() {
		defer stdinW.Close()
		for {
			msg, err := stream.Recv()
			if err == io.EOF {
				recvDone <- nil
				return
			}
			if err != nil {
				recvDone <- err
				return
			}
			chunk := msg.GetChunk()
			if len(chunk) == 0 {
				continue
			}
			n, werr := stdinW.Write(chunk)
			bytesIn += int64(n)
			if werr != nil {
				recvDone <- werr
				return
			}
		}
	}()

	exitCode, execErr := c.Service.Exec(stream.Context(), runtime.ExecRequest{
		Name:    md.GetName(),
		Command: []string{"/bin/sh", "-c", copyToScript},
		Env:     map[string]string{"CAPSULE_CP_DEST": dest},
		Stdin:   stdinR,
		Stdout:  &outBuf,
		Stderr:  &errBuf,
	})
	// Idempotent — if recv already closed, this is a no-op.
	_ = stdinW.Close()

	if execErr != nil {
		return status.Errorf(codes.Internal, "exec tar x: %v", execErr)
	}
	if exitCode != 0 {
		return status.Errorf(codes.Internal, "tar x in %q exited %d: %s",
			md.GetName(), exitCode, copyTrim(&errBuf))
	}
	// Happy path: tar exited 0, which means it consumed EOF, which means
	// the recv goroutine had already closed stdinW. Wait for it so the
	// final bytesIn write is visible.
	if rerr := <-recvDone; rerr != nil {
		return status.Errorf(codes.Internal, "receive: %v", rerr)
	}
	return stream.SendAndClose(&capsulev1.WorkloadCopyToResponse{BytesReceived: bytesIn})
}

// CopyFrom runs `tar c` inside the workload and forwards the archive
// bytes to the client. The archive contains a single top-level entry
// named basename(src_path); the client handles any rename/extract.
func (c *WorkloadController) CopyFrom(req *capsulev1.WorkloadCopyFromRequest, stream capsulev1.WorkloadService_CopyFromServer) error {
	if req.GetName() == "" {
		return status.Error(codes.InvalidArgument, "name is required")
	}
	src := req.GetSrcPath()
	if src == "" || !path.IsAbs(src) {
		return status.Error(codes.InvalidArgument, "src_path must be an absolute path")
	}
	parent := path.Dir(src)
	base := path.Base(src)

	stdoutR, stdoutW := io.Pipe()
	var errBuf bytes.Buffer

	// Pump tar's stdout pipe → gRPC stream until Exec closes the writer.
	sendDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 64*1024)
		for {
			n, rerr := stdoutR.Read(buf)
			if n > 0 {
				if serr := stream.Send(&capsulev1.WorkloadCopyFromChunk{Data: append([]byte(nil), buf[:n]...)}); serr != nil {
					sendDone <- serr
					return
				}
			}
			if rerr == io.EOF {
				sendDone <- nil
				return
			}
			if rerr != nil {
				sendDone <- rerr
				return
			}
		}
	}()

	exitCode, execErr := c.Service.Exec(stream.Context(), runtime.ExecRequest{
		Name:    req.GetName(),
		Command: []string{"/bin/sh", "-c", `exec tar c -C "$CAPSULE_CP_PARENT" -- "$CAPSULE_CP_BASE"`},
		Env: map[string]string{
			"CAPSULE_CP_PARENT": parent,
			"CAPSULE_CP_BASE":   base,
		},
		// No stdin; tar c reads from the filesystem.
		Stdout: stdoutW,
		Stderr: &errBuf,
	})
	_ = stdoutW.Close()
	sendErr := <-sendDone

	if execErr != nil {
		return status.Errorf(codes.Internal, "exec tar c: %v", execErr)
	}
	if exitCode != 0 {
		return status.Errorf(codes.Internal, "tar c %s in %q exited %d: %s",
			src, req.GetName(), exitCode, copyTrim(&errBuf))
	}
	if sendErr != nil {
		return sendErr
	}
	return nil
}

// copyToScript is the workload-side shell helper for CopyTo. It picks
// between two extraction modes based on $CAPSULE_CP_DEST:
//   - "into" mode (dest ends in '/' OR is an existing directory): extract
//     entries directly under dest, the same way cp/scp/docker-cp does.
//   - "rename" mode (anything else): extract to a sibling temp dir, then
//     atomically rename the single top-level entry to dest. This is what
//     handles `cp file wl:/etc/newname` and also the case where the
//     destination doesn't exist yet.
//
// Written for busybox sh/tar so it works in alpine + minimal images;
// avoids `tar --transform` (GNU-only). Uses `set -e` so any failed step
// surfaces as a non-zero exit and the controller reports it via the
// captured stderr buffer.
const copyToScript = `set -e
DEST="$CAPSULE_CP_DEST"
case "$DEST" in
    */)
        D="${DEST%/}"
        [ -z "$D" ] && D=/
        mkdir -p "$D"
        exec tar x -C "$D"
        ;;
esac
if [ -d "$DEST" ]; then
    exec tar x -C "$DEST"
fi
parent=$(dirname "$DEST")
mkdir -p "$parent"
tmp=$(mktemp -d "${parent%/}/.capsule-cp.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
tar x -C "$tmp"
nentries=$(ls -A "$tmp" | wc -l)
if [ "$nentries" -ne 1 ]; then
    echo "capsule cp: expected 1 top-level archive entry, got $nentries" >&2
    exit 1
fi
entry=$(ls -A "$tmp")
[ -e "$DEST" ] && rm -rf "$DEST"
mv "$tmp/$entry" "$DEST"
`

// copyTrim returns the buffered tar stderr trimmed of trailing whitespace
// and capped at 1 KiB so a screen of warnings doesn't drown the client.
func copyTrim(buf *bytes.Buffer) string {
	s := strings.TrimSpace(buf.String())
	const cap = 1024
	if len(s) > cap {
		s = s[:cap] + "…"
	}
	return s
}
