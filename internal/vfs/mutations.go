//go:build linux || darwin

package vfs

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/unofficialpku/disk/internal/diagnostic"
)

func (n *Node) find(ctx context.Context, name string) (*Node, error) {
	entries, err := n.f.list(ctx, n, true)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.Name == name {
			return n.child(ctx, e).Operations().(*Node), nil
		}
	}
	return nil, anyshare.ErrNotFound
}

func (n *Node) mutable() syscall.Errno {
	if n.f.opts.ReadOnly {
		return syscall.EROFS
	}
	if n.synthetic {
		return syscall.EPERM
	}
	return 0
}

func (n *Node) Create(ctx context.Context, name string, flags, mode uint32, out *fuse.EntryOut) (inode *fs.Inode, handle fs.FileHandle, openFlags uint32, result syscall.Errno) {
	ctx, finish := n.f.opts.Trace.Begin(ctx, "create directory=%q name=%q flags=%#x", n.traceName, name, flags)
	defer func() { finish("%s (errno=%d)", diagnostic.Errno(result), result) }()
	if code := n.mutable(); code != 0 {
		return nil, nil, 0, code
	}
	if !anyshare.ValidName(name) {
		return nil, nil, 0, syscall.EINVAL
	}
	n.f.opMu.Lock()
	defer n.f.opMu.Unlock()
	parent := n.snapshot()
	ok, err := n.f.backend.Can(ctx, parent.ID, "create")
	if err != nil {
		return nil, nil, 0, n.failure("check create permission", err)
	}
	if !ok {
		return nil, nil, 0, syscall.EACCES
	}
	entry, err := n.f.backend.Upload(ctx, parent.ID, name, nil, bytes.NewReader(nil), 0, time.Now())
	if err != nil {
		return nil, nil, 0, n.failure("create file", err)
	}
	entry.Modified = time.Now()
	entry.Created = entry.Modified
	child := n.child(ctx, entry)
	node := child.Operations().(*Node)
	node.mu.Lock()
	defer node.mu.Unlock()
	node.mode = mode & 0777
	if err = node.ensureCacheLocked(ctx, true); err != nil {
		return nil, nil, 0, n.failure("create cache", err)
	}
	node.refs++
	node.fillLocked(&out.Attr)
	n.f.invalidate()
	return child, &Handle{node: node, writable: flags&syscall.O_ACCMODE != syscall.O_RDONLY, append: flags&syscall.O_APPEND != 0}, 0, 0
}

func (n *Node) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (inode *fs.Inode, result syscall.Errno) {
	ctx, finish := n.f.opts.Trace.Begin(ctx, "mkdir directory=%q name=%q", n.traceName, name)
	defer func() { finish("%s (errno=%d)", diagnostic.Errno(result), result) }()
	if code := n.mutable(); code != 0 {
		return nil, code
	}
	if !anyshare.ValidName(name) {
		return nil, syscall.EINVAL
	}
	n.f.opMu.Lock()
	defer n.f.opMu.Unlock()
	entry, err := n.f.backend.Mkdir(ctx, n.snapshot().ID, name)
	if err != nil {
		return nil, n.failure("mkdir", err)
	}
	entry.Modified = time.Now()
	entry.Created = entry.Modified
	child := n.child(ctx, entry)
	node := child.Operations().(*Node)
	node.mu.Lock()
	node.mode = mode & 0777
	node.fillLocked(&out.Attr)
	node.mu.Unlock()
	n.f.invalidate()
	return child, 0
}

func (n *Node) Unlink(ctx context.Context, name string) (result syscall.Errno) {
	ctx, finish := n.f.opts.Trace.Begin(ctx, "unlink directory=%q name=%q", n.traceName, name)
	defer func() { finish("%s (errno=%d)", diagnostic.Errno(result), result) }()
	if code := n.mutable(); code != 0 {
		return code
	}
	n.f.opMu.Lock()
	defer n.f.opMu.Unlock()
	node, err := n.find(ctx, name)
	if err != nil {
		return n.failure("unlink lookup", err)
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	if node.entry.Dir {
		return syscall.EISDIR
	}
	if node.refs > 0 {
		if err = node.ensureCacheLocked(ctx, false); err != nil {
			return n.failure("preserve open unlinked file", err)
		}
	}
	if err = n.f.backend.Delete(ctx, node.entry); err != nil {
		return n.failure("unlink", err)
	}
	node.unlinked = true
	n.f.invalidate()
	return 0
}

func (n *Node) Rmdir(ctx context.Context, name string) (result syscall.Errno) {
	ctx, finish := n.f.opts.Trace.Begin(ctx, "rmdir directory=%q name=%q", n.traceName, name)
	defer func() { finish("%s (errno=%d)", diagnostic.Errno(result), result) }()
	if code := n.mutable(); code != 0 {
		return code
	}
	n.f.opMu.Lock()
	defer n.f.opMu.Unlock()
	node, err := n.find(ctx, name)
	if err != nil {
		return n.failure("rmdir lookup", err)
	}
	e := node.snapshot()
	if !e.Dir {
		return syscall.ENOTDIR
	}
	entries, err := n.f.backend.List(ctx, e.ID)
	if err != nil {
		return n.failure("rmdir list", err)
	}
	if len(entries) > 0 {
		return syscall.ENOTEMPTY
	}
	if err = n.f.backend.Delete(ctx, e); err != nil {
		return n.failure("rmdir", err)
	}
	n.f.invalidate()
	return 0
}

func (n *Node) Setattr(ctx context.Context, h fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) (result syscall.Errno) {
	ctx, finish := n.f.opts.Trace.Begin(ctx, "setattr file=%q", n.traceName)
	defer func() { finish("%s (errno=%d)", diagnostic.Errno(result), result) }()
	if code := n.mutable(); code != 0 {
		return code
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if uid, ok := in.GetUID(); ok && uid != uint32(syscall.Getuid()) {
		return syscall.EPERM
	}
	if gid, ok := in.GetGID(); ok && gid != uint32(syscall.Getgid()) {
		return syscall.EPERM
	}
	if mode, ok := in.GetMode(); ok {
		if mode&07000 != 0 {
			return syscall.ENOTSUP
		}
		n.mode = mode & 0777
	}
	size, sizeSet := in.GetSize()
	mtime, mtimeSet := in.GetMTime()
	if sizeSet || mtimeSet && !n.entry.Dir {
		if n.entry.Dir {
			return syscall.EISDIR
		}
		if size > uint64(^uint64(0)>>1) {
			return syscall.EFBIG
		}
		if !n.unlinked {
			ok, err := n.f.backend.Can(ctx, n.entry.ID, "modify")
			if err != nil {
				return n.failure("check setattr permission", err)
			}
			if !ok {
				return syscall.EACCES
			}
		}
		if err := n.ensureCacheLocked(ctx, sizeSet && size == 0); err != nil {
			return n.failure("setattr cache", err)
		}
		if sizeSet {
			if err := n.resizeLocked(int64(size)); err != nil {
				return n.failure("truncate", err)
			}
		}
		if mtimeSet {
			n.entry.Modified = mtime
			n.dirty = true
			if err := n.journalLocked(); err != nil {
				return n.failure("record mtime", err)
			}
		}
		if h == nil && n.refs == 0 {
			if err := n.flushLocked(ctx); err != nil {
				return n.failure("commit setattr", err)
			}
			n.disposeLocked()
		}
	} else if mtimeSet {
		n.entry.Modified = mtime
	}
	n.fillLocked(&out.Attr)
	return 0
}

func (n *Node) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) (result syscall.Errno) {
	ctx, finish := n.f.opts.Trace.Begin(ctx, "rename directory=%q from=%q to=%q", n.traceName, name, newName)
	defer func() { finish("%s (errno=%d)", diagnostic.Errno(result), result) }()
	parent, ok := newParent.(*Node)
	if !ok || parent.f != n.f {
		return syscall.EXDEV
	}
	if code := n.mutable(); code != 0 {
		return code
	}
	if code := parent.mutable(); code != 0 {
		return code
	}
	if flags & ^uint32(1) != 0 {
		return syscall.ENOTSUP
	}
	if !anyshare.ValidName(newName) {
		return syscall.EINVAL
	}
	n.f.opMu.Lock()
	defer n.f.opMu.Unlock()
	oldParent, newParentEntry := n.snapshot(), parent.snapshot()
	if oldParent.ID == newParentEntry.ID && name == newName {
		return 0
	}
	if libraryID(oldParent.ID) != libraryID(newParentEntry.ID) {
		return syscall.EXDEV
	}
	source, err := n.find(ctx, name)
	if err != nil {
		return n.failure("rename source", err)
	}
	target, err := parent.find(ctx, newName)
	if err != nil && !errors.Is(err, anyshare.ErrNotFound) {
		return n.failure("rename destination", err)
	}
	if target == source {
		return 0
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	old := source.entry
	if old.Dir && (newParentEntry.ID == old.ID || strings.HasPrefix(newParentEntry.ID, old.ID+"/")) {
		return syscall.EINVAL
	}
	if target != nil {
		if flags&1 != 0 {
			return syscall.EEXIST
		}
		target.mu.Lock()
		defer target.mu.Unlock()
		if target.entry.Dir && !old.Dir {
			return syscall.EISDIR
		}
		if !target.entry.Dir && old.Dir {
			return syscall.ENOTDIR
		}
		if target.entry.Dir {
			entries, err := n.f.backend.List(ctx, target.entry.ID)
			if err != nil {
				return n.failure("rename directory destination", err)
			}
			if len(entries) > 0 {
				return syscall.ENOTEMPTY
			}
		}
		if target.refs > 0 {
			if err := target.ensureCacheLocked(ctx, false); err != nil {
				return n.failure("preserve replaced file", err)
			}
		}
	}
	if err := source.flushLocked(ctx); err != nil {
		return n.failure("commit before rename", err)
	}
	old = source.entry
	var updated anyshare.Entry
	if oldParent.ID == newParentEntry.ID {
		updated, err = n.f.backend.Rename(ctx, old, newName, target != nil)
	} else {
		updated, err = n.f.moveAndRename(ctx, old, oldParent.ID, newParentEntry.ID, newName, target != nil)
	}
	if err != nil {
		n.f.invalidate()
		return n.failure("rename", err)
	}
	updated.Created, updated.Modified = old.Created, old.Modified
	source.entry = updated
	source.statAt = time.Now()
	if old.ID != updated.ID {
		source.rebaseChildren(old.ID, updated.ID)
	}
	if target != nil {
		target.unlinked = true
	}
	n.f.invalidate()
	return 0
}

func (n *Node) rebaseChildren(old, new string) {
	for _, inode := range n.Children() {
		child, ok := inode.Operations().(*Node)
		if !ok {
			continue
		}
		child.mu.Lock()
		if strings.HasPrefix(child.entry.ID, old+"/") {
			child.entry.ID = new + strings.TrimPrefix(child.entry.ID, old)
			if child.dirty {
				if err := child.journalLocked(); err != nil {
					child.failure("rebase write journal", err)
				}
			}
		}
		child.rebaseChildren(old, new)
		child.mu.Unlock()
	}
}

var _ fs.NodeGetattrer = (*Node)(nil)
var _ fs.NodeLookuper = (*Node)(nil)
var _ fs.NodeReaddirer = (*Node)(nil)
var _ fs.NodeOpener = (*Node)(nil)
var _ fs.NodeCreater = (*Node)(nil)
var _ fs.NodeMkdirer = (*Node)(nil)
var _ fs.NodeUnlinker = (*Node)(nil)
var _ fs.NodeRmdirer = (*Node)(nil)
var _ fs.NodeRenamer = (*Node)(nil)
var _ fs.NodeSetattrer = (*Node)(nil)
