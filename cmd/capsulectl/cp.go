package main

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	capsulev1 "github.com/geekgonecrazy/capsule/models/capsule/v1"
)

// runCp is the dispatcher for `capsulectl cp <src> <dst>`. Exactly one
// of src/dst must be of the form "<workload>:<path>"; the other is a
// local path (file or directory).
//
// Path semantics follow `cp` / `scp` conventions:
//   - <dst> with a trailing "/" means "place the source INSIDE this
//     directory" (the resulting top-level entry keeps the source's
//     basename).
//   - <dst> without a trailing "/" means "the source becomes EXACTLY
//     this path" (rename if basenames differ).
//
// Wire format is always tar — single regular files and directory trees
// flow through the same plumbing. The workload image must include
// /bin/sh, mkdir, and tar (universal except in `scratch` images).
func runCp(addr string, args []string) error {
	if len(args) != 2 {
		return errors.New("cp requires exactly two args: <src> <dst>")
	}
	srcArg, dstArg := args[0], args[1]
	srcWl, srcPath, srcIsRemote := parseCpArg(srcArg)
	dstWl, dstPath, dstIsRemote := parseCpArg(dstArg)

	switch {
	case srcIsRemote && dstIsRemote:
		return errors.New("workload-to-workload copy not supported; copy via your workstation")
	case !srcIsRemote && !dstIsRemote:
		return errors.New("one of <src> or <dst> must be of the form <workload>:<path>")
	case dstIsRemote:
		return cpToWorkload(addr, srcArg /*local src*/, dstWl, dstPath)
	default: // srcIsRemote
		return cpFromWorkload(addr, srcWl, srcPath, dstArg /*local dst*/)
	}
}

// parseCpArg splits a cp arg into (workload, remote_path, is_remote).
// An arg is "remote" iff it contains a ':' AND the part before the
// first ':' contains no '/' (so absolute Windows-ish paths can't be
// confused for workload-prefixed paths). Matches `scp` rules.
func parseCpArg(s string) (workload, remotePath string, isRemote bool) {
	i := strings.IndexByte(s, ':')
	if i < 0 {
		return "", s, false
	}
	if strings.ContainsAny(s[:i], "/") || i == 0 {
		return "", s, false
	}
	return s[:i], s[i+1:], true
}

// cpToWorkload streams localSrc → workload at remotePath. The wire
// always carries one tar archive whose top-level entry is named with
// the source's natural basename; the server-side helper picks
// "extract-into" vs "extract-with-rename" based on dest_path. That way
// `cp file wl:/tmp` and `cp file wl:/tmp/` both Do The Right Thing
// when /tmp already exists as a directory inside the workload.
func cpToWorkload(addr, localSrc, wl, remotePath string) error {
	info, err := os.Lstat(localSrc)
	if err != nil {
		return fmt.Errorf("stat %s: %w", localSrc, err)
	}
	if remotePath == "" {
		return errors.New("remote path is empty")
	}
	if !path.IsAbs(remotePath) {
		return fmt.Errorf("remote path %q must be absolute", remotePath)
	}

	rootName := filepath.Base(strings.TrimRight(localSrc, string(os.PathSeparator)))
	if rootName == "." || rootName == ".." || rootName == "/" || rootName == "" {
		return fmt.Errorf("cannot derive a top-level name from local source %q (try a real file or directory path)", localSrc)
	}

	conn, err := dial(addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := capsulev1.NewWorkloadServiceClient(conn)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := client.CopyTo(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&capsulev1.WorkloadCopyToRequest{
		Payload: &capsulev1.WorkloadCopyToRequest_Metadata{
			Metadata: &capsulev1.WorkloadCopyToMetadata{
				Name:     wl,
				DestPath: remotePath,
			},
		},
	}); err != nil {
		return err
	}

	pw := &grpcChunkWriter{stream: stream}
	if err := tarPack(pw, localSrc, rootName, info); err != nil {
		// Best-effort close so the server sees end-of-stream and unwinds.
		_, _ = stream.CloseAndRecv()
		return fmt.Errorf("pack: %w", err)
	}
	resp, err := stream.CloseAndRecv()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "copied %d byte(s) to %s:%s\n", resp.GetBytesReceived(), wl, remotePath)
	return nil
}

// cpFromWorkload streams workload:remotePath → localDst. Server sends a
// tar archive whose top-level entry is named basename(remotePath); the
// client extracts under localDst and (optionally) rewrites that root
// name to honour the user's "rename" intent.
func cpFromWorkload(addr, wl, remotePath, localDst string) error {
	if remotePath == "" {
		return errors.New("remote path is empty")
	}
	if !path.IsAbs(remotePath) {
		return fmt.Errorf("remote path %q must be absolute", remotePath)
	}

	srcRoot := path.Base(remotePath)
	if srcRoot == "" || srcRoot == "/" || srcRoot == "." {
		return fmt.Errorf("cannot derive a source name from %q", remotePath)
	}

	// Decide where to extract and whether to rename the tar root.
	//   ./dst/  or existing-dir ./dst → extract INTO ./dst (no rename)
	//   ./dst                          → rename root to basename(./dst)
	var destDir, newRoot string
	switch {
	case strings.HasSuffix(localDst, "/") || strings.HasSuffix(localDst, string(os.PathSeparator)):
		destDir = strings.TrimRight(localDst, "/"+string(os.PathSeparator))
		if destDir == "" {
			destDir = "."
		}
		newRoot = "" // keep srcRoot
	default:
		if info, err := os.Stat(localDst); err == nil && info.IsDir() {
			destDir = localDst
			newRoot = "" // keep srcRoot
		} else {
			destDir = filepath.Dir(localDst)
			newRoot = filepath.Base(localDst)
		}
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", destDir, err)
	}

	conn, err := dial(addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := capsulev1.NewWorkloadServiceClient(conn)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := client.CopyFrom(ctx, &capsulev1.WorkloadCopyFromRequest{
		Name:    wl,
		SrcPath: remotePath,
	})
	if err != nil {
		return err
	}

	pr := &grpcChunkReader{stream: stream}
	n, err := tarUnpack(pr, destDir, srcRoot, newRoot)
	if err != nil {
		return fmt.Errorf("unpack: %w", err)
	}
	finalName := srcRoot
	if newRoot != "" {
		finalName = newRoot
	}
	fmt.Fprintf(os.Stderr, "copied %d byte(s) from %s:%s to %s\n", n, wl, remotePath, filepath.Join(destDir, finalName))
	return nil
}

// --- tar pack (local → wire) -----------------------------------------------

// tarPack writes a tar archive of src under the in-archive name
// rootName. If src is a regular file, the archive contains exactly one
// entry named rootName. If src is a directory, the archive contains
// rootName/ and entries beneath it mirroring the on-disk tree.
func tarPack(w io.Writer, src, rootName string, info fs.FileInfo) error {
	tw := tar.NewWriter(w)
	defer tw.Close()
	if !info.IsDir() {
		return tarWriteOne(tw, src, rootName, info)
	}
	return filepath.Walk(src, func(p string, fi fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		name := rootName
		if rel != "." {
			name = rootName + "/" + rel
		}
		return tarWriteOne(tw, p, name, fi)
	})
}

func tarWriteOne(tw *tar.Writer, srcPath, archiveName string, info fs.FileInfo) error {
	var link string
	if info.Mode()&os.ModeSymlink != 0 {
		l, err := os.Readlink(srcPath)
		if err != nil {
			return err
		}
		link = l
	}
	hdr, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return err
	}
	hdr.Name = archiveName
	if info.IsDir() {
		// tar convention: directory names end in "/".
		hdr.Name = strings.TrimSuffix(hdr.Name, "/") + "/"
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(tw, f)
	return err
}

// --- tar unpack (wire → local) ---------------------------------------------

// tarUnpack reads a tar archive from r and extracts entries under
// destDir. If newRoot is non-empty, every entry whose path's first
// segment equals oldRoot has that segment rewritten to newRoot —
// honouring the rename half of cp semantics. Returns total bytes of
// regular-file content written.
func tarUnpack(r io.Reader, destDir, oldRoot, newRoot string) (int64, error) {
	var written int64
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return written, nil
		}
		if err != nil {
			return written, err
		}
		name := strings.TrimLeft(hdr.Name, "/")
		if newRoot != "" {
			name = rewriteRoot(name, oldRoot, newRoot)
		}
		// Reject path-escape attempts after the rewrite.
		if strings.Contains(name, "..") {
			return written, fmt.Errorf("tar entry %q contains '..'", hdr.Name)
		}
		full := filepath.Join(destDir, filepath.FromSlash(name))
		// Belt-and-braces: ensure full lives under destDir even after Join.
		absDest, _ := filepath.Abs(destDir)
		absFull, _ := filepath.Abs(full)
		if absDest != "" && !strings.HasPrefix(absFull, absDest+string(os.PathSeparator)) && absFull != absDest {
			return written, fmt.Errorf("tar entry %q escapes destination", hdr.Name)
		}

		mode := fs.FileMode(hdr.Mode) & 0o7777
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(full, mode|0o700); err != nil {
				return written, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return written, err
			}
			f, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
			if err != nil {
				return written, err
			}
			n, err := io.Copy(f, tr)
			written += n
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return written, err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return written, err
			}
			_ = os.Remove(full)
			if err := os.Symlink(hdr.Linkname, full); err != nil {
				return written, err
			}
		case tar.TypeXGlobalHeader:
			// pax metadata; ignore.
		default:
			// Skip unsupported types (devices, fifos, hardlinks, etc).
			// They're unusual inside workload paths; warn so the user
			// knows we silently dropped something.
			fmt.Fprintf(os.Stderr, "skipping unsupported tar entry %q (type %c)\n", hdr.Name, hdr.Typeflag)
		}
	}
}

// rewriteRoot replaces the first path segment of name (== oldRoot) with
// newRoot. If name doesn't start with oldRoot, it's returned unchanged.
func rewriteRoot(name, oldRoot, newRoot string) string {
	if name == oldRoot {
		return newRoot
	}
	if strings.HasPrefix(name, oldRoot+"/") {
		return newRoot + name[len(oldRoot):]
	}
	return name
}

// --- gRPC stream <-> io.Reader/Writer adapters -----------------------------

// grpcChunkWriter adapts a CopyTo client stream to an io.Writer so the
// tar packer can emit bytes naturally.
type grpcChunkWriter struct {
	stream capsulev1.WorkloadService_CopyToClient
}

func (w *grpcChunkWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := w.stream.Send(&capsulev1.WorkloadCopyToRequest{
		Payload: &capsulev1.WorkloadCopyToRequest_Chunk{Chunk: append([]byte(nil), p...)},
	}); err != nil {
		return 0, err
	}
	return len(p), nil
}

// grpcChunkReader adapts a CopyFrom server-streaming response to an
// io.Reader so the tar reader can pull bytes naturally. Buffers leftover
// bytes between Read calls.
type grpcChunkReader struct {
	stream capsulev1.WorkloadService_CopyFromClient
	buf    []byte
}

func (r *grpcChunkReader) Read(p []byte) (int, error) {
	if len(r.buf) == 0 {
		msg, err := r.stream.Recv()
		if err != nil {
			return 0, err // includes io.EOF
		}
		r.buf = msg.GetData()
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}
