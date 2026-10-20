//go:build linux || darwin

package vfs

import (
	"context"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/unofficialpku/disk/internal/diagnostic"
	"os"
	"syscall"
)

type inodeBase = fs.Inode

func stable(e anyshare.Entry) fs.StableAttr {
	attr := entryAttributes(e, 0)
	return fs.StableAttr{Ino: attr.Inode, Mode: attr.Mode}
}

func (n *Node) failure(operation string, err error) syscall.Errno {
	if err != nil {
		n.f.opts.Logger.Printf("%s: %v", operation, err)
	}
	return errno(err)
}

func (n *Node) fillLocked(a *fuse.Attr) {
	attr := entryAttributes(n.entry, n.effectiveModeLocked())
	a.Mode, a.Nlink = attr.Mode, attr.Links
	a.Uid, a.Gid = uint32(os.Getuid()), uint32(os.Getgid())
	a.Size, a.Blksize, a.Blocks = uint64(attr.Size), 4096, uint64(attr.Blocks)
	a.SetTimes(&attr.Modified, &attr.Modified, &attr.Modified)
}

func (n *Node) Getattr(ctx context.Context, h fs.FileHandle, out *fuse.AttrOut) (result syscall.Errno) {
	ctx, finish := n.f.opts.Trace.Begin(ctx, "getattr file=%q", n.traceName)
	defer func() { finish("%s (errno=%d)", diagnostic.Errno(result), result) }()
	if err := n.refreshMetadata(ctx); err != nil {
		return n.failure("stat", err)
	}
	n.f.opts.Trace.Printf(ctx, "waiting for file metadata lock")
	n.mu.Lock()
	defer n.mu.Unlock()
	n.fillLocked(&out.Attr)
	n.f.opts.Trace.Printf(ctx, "attributes returned: size=%s mode=%#o", n.f.opts.Trace.Size(int64(out.Size)), out.Mode)
	return 0
}

// Statfs reports server allocation and the bytes still writable by the
// account. The local cache volume is intentionally not used here: it is an
// implementation detail and can be larger or smaller than the remote quota.
func (n *Node) Statfs(ctx context.Context, out *fuse.StatfsOut) (result syscall.Errno) {
	ctx, finish := n.f.opts.Trace.Begin(ctx, "statfs path=%q", n.traceName)
	defer func() { finish("%s (errno=%d)", diagnostic.Errno(result), result) }()
	total, available, err := n.f.quotaSpace(ctx)
	if err != nil {
		return n.failure("statfs quota", err)
	}
	const blockSize int64 = 4096
	*out = fuse.StatfsOut{
		Blocks:  quotaBlocks(total, blockSize),
		Bfree:   uint64(max(available/blockSize, 0)),
		Bavail:  uint64(max(available/blockSize, 0)),
		Files:   1 << 48,
		Ffree:   1 << 48,
		Bsize:   uint32(blockSize),
		NameLen: 255,
		Frsize:  uint32(blockSize),
	}
	return 0
}

func (n *Node) child(ctx context.Context, e anyshare.Entry) *fs.Inode {
	if old := n.GetChild(e.Name); old != nil {
		if child, ok := old.Operations().(*Node); ok && child.snapshot().ID == e.ID {
			return old
		}
	}
	return n.NewInode(ctx, n.f.makeNode(e), stable(e))
}

func (n *Node) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (inode *fs.Inode, result syscall.Errno) {
	ctx, finish := n.f.opts.Trace.Begin(ctx, "lookup directory=%q name=%q", n.traceName, name)
	defer func() { finish("%s (errno=%d)", diagnostic.Errno(result), result) }()
	entries, err := n.f.list(ctx, n, false)
	if err != nil {
		return nil, n.failure("lookup", err)
	}
	for _, e := range entries {
		if e.Name == name {
			child := n.child(ctx, e)
			node := child.Operations().(*Node)
			node.mu.Lock()
			node.fillLocked(&out.Attr)
			node.mu.Unlock()
			return child, 0
		}
	}
	return nil, syscall.ENOENT
}

func (n *Node) Readdir(ctx context.Context) (streamOut fs.DirStream, result syscall.Errno) {
	ctx, finish := n.f.opts.Trace.Begin(ctx, "readdir directory=%q", n.traceName)
	defer func() { finish("%s (errno=%d)", diagnostic.Errno(result), result) }()
	entries, err := n.f.list(ctx, n, false)
	if err != nil {
		return nil, n.failure("readdir", err)
	}
	stream := make([]fuse.DirEntry, 0, len(entries))
	for _, e := range entries {
		attr := stable(e)
		stream = append(stream, fuse.DirEntry{Name: e.Name, Mode: attr.Mode, Ino: attr.Ino})
	}
	return fs.NewListDirStream(stream), 0
}

func (n *Node) Open(ctx context.Context, flags uint32) (handle fs.FileHandle, openFlags uint32, result syscall.Errno) {
	ctx, finish := n.f.opts.Trace.Begin(ctx, "open file=%q flags=%#x", n.traceName, flags)
	defer func() { finish("%s (errno=%d)", diagnostic.Errno(result), result) }()
	writable := flags&syscall.O_ACCMODE != syscall.O_RDONLY
	if writable && n.f.opts.ReadOnly {
		return nil, 0, syscall.EROFS
	}
	if err := n.refreshMetadata(ctx); err != nil {
		return nil, 0, n.failure("open", err)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.entry.Dir {
		return nil, 0, syscall.EISDIR
	}
	if writable && !n.unlinked {
		ok, err := n.f.backend.Can(ctx, n.entry.ID, "modify")
		if err != nil {
			return nil, 0, n.failure("check modify permission", err)
		}
		if !ok {
			return nil, 0, syscall.EACCES
		}
	}
	if writable {
		if err := n.ensureCacheLocked(ctx, flags&syscall.O_TRUNC != 0); err != nil {
			return nil, 0, n.failure("prepare write cache", err)
		}
		if flags&syscall.O_TRUNC != 0 {
			if err := n.resizeLocked(0); err != nil {
				return nil, 0, n.failure("truncate", err)
			}
		}
	}
	n.refs++
	return &Handle{node: n, writable: writable, append: flags&syscall.O_APPEND != 0}, 0, 0
}
