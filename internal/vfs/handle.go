//go:build linux || darwin

package vfs

import (
	"context"
	"io"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/unofficialpku/disk/internal/diagnostic"
)

type Handle struct {
	node     *Node
	writable bool
	append   bool
	released bool
}

func (h *Handle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	n := h.node
	count, err := n.readAt(ctx, dest, off)
	if err != nil && err != io.EOF {
		return nil, n.failure("read", err)
	}
	return fuse.ReadResultData(dest[:count]), 0
}

func (h *Handle) Write(ctx context.Context, data []byte, off int64) (countOut uint32, result syscall.Errno) {
	ctx, finish := h.node.f.opts.Trace.Begin(ctx, "write file=%q offset=%d length=%d", h.node.traceName, off, len(data))
	defer func() { finish("%s (errno=%d)", diagnostic.Errno(result), result) }()
	if !h.writable {
		return 0, syscall.EBADF
	}
	n := h.node
	n.mu.Lock()
	defer n.mu.Unlock()
	if h.append {
		off = n.entry.Size
	}
	if off < 0 || off > int64(^uint64(0)>>1)-int64(len(data)) {
		return 0, syscall.EFBIG
	}
	size := max(n.entry.Size, off+int64(len(data)))
	if err := n.reserveLocked(size); err != nil {
		return 0, n.failure("reserve write cache", err)
	}
	count, err := n.cache.WriteAt(data, off)
	if count > 0 {
		n.entry.Size = max(n.entry.Size, off+int64(count))
		n.entry.Modified = time.Now()
		n.dirty = true
		je := n.journalLocked()
		if err == nil {
			err = je
		}
	}
	return uint32(count), n.failure("write", err)
}

func (h *Handle) Flush(ctx context.Context) (result syscall.Errno) {
	ctx, finish := h.node.f.opts.Trace.Begin(ctx, "flush handle file=%q", h.node.traceName)
	defer func() { finish("%s (errno=%d)", diagnostic.Errno(result), result) }()
	if !h.writable {
		return 0
	}
	n := h.node
	n.mu.Lock()
	defer n.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, n.f.opts.IOTimeout)
	defer cancel()
	return n.failure("upload on close", n.flushLocked(ctx))
}
func (h *Handle) Fsync(ctx context.Context, flags uint32) syscall.Errno { return h.Flush(ctx) }

func (h *Handle) Release(ctx context.Context) (result syscall.Errno) {
	ctx, finish := h.node.f.opts.Trace.Begin(ctx, "release handle file=%q", h.node.traceName)
	defer func() { finish("%s (errno=%d)", diagnostic.Errno(result), result) }()
	n := h.node
	n.mu.Lock()
	defer n.mu.Unlock()
	if h.released {
		return 0
	}
	h.released = true
	// Flush reports failures to close(2). Release must keep failed uploads on disk,
	// rather than retrying a potentially successful but unacknowledged publish.
	n.refs--
	if n.refs == 0 && !n.dirty {
		n.disposeLocked()
	}
	return 0
}

func (h *Handle) Getattr(ctx context.Context, out *fuse.AttrOut) syscall.Errno {
	return h.node.Getattr(ctx, h, out)
}
func (h *Handle) Setattr(ctx context.Context, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	return h.node.Setattr(ctx, h, in, out)
}

var _ fs.FileReader = (*Handle)(nil)
var _ fs.FileWriter = (*Handle)(nil)
var _ fs.FileFlusher = (*Handle)(nil)
var _ fs.FileFsyncer = (*Handle)(nil)
var _ fs.FileReleaser = (*Handle)(nil)
