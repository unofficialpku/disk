package vfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/unofficialpku/disk/internal/localfile"
	"github.com/winfsp/cgofuse/fuse"
	"golang.org/x/sys/windows"
)

// inodeBase is supplied by go-fuse on Unix. WinFsp addresses the same cached
// Node state through paths and file handles rather than kernel inode objects.
type inodeBase struct{}

type winHandle struct {
	node     *Node
	writable bool
	append   bool
	listMu   sync.Mutex
	listed   bool
	listedAt time.Time
	entries  []anyshare.Entry
}

type winResult int

func (r winResult) String() string {
	if r < 0 {
		return "failed: " + fuse.Error(r).Error()
	}
	return fmt.Sprintf("success (result=%d)", r)
}

type WinFsp struct {
	fuse.FileSystemBase
	core    *FileSystem
	ctx     context.Context
	mu      sync.Mutex
	paths   map[string]*Node
	handles map[uint64]*winHandle
	next    uint64
	Ready   chan struct{}
}

func NewWinFsp(ctx context.Context, core *FileSystem) *WinFsp {
	return &WinFsp{core: core, ctx: ctx, paths: map[string]*Node{"/": core.root}, handles: map[uint64]*winHandle{}, Ready: make(chan struct{})}
}

// WinFspMountOptions binds all exposed POSIX owners to the mounting Windows
// user's SID. WinFsp documents -1 as the mounting user's UID/GID.
func WinFspMountOptions() []string { return []string{"-o", "uid=-1,gid=-1"} }

func (w *WinFsp) Init() {
	w.core.opts.Trace.Printf(w.ctx, "WinFsp initialized; mount ready")
	close(w.Ready)
}

func (w *WinFsp) operation(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, w.core.opts.IOTimeout)
}

func winError(err error) int {
	if errors.Is(err, windows.ERROR_DISK_FULL) {
		return -fuse.ENOSPC
	}
	// Windows' syscall errno values do not share cgofuse's POSIX numbering.
	codes := map[syscall.Errno]int{
		0: 0, syscall.ENOENT: fuse.ENOENT, syscall.EEXIST: fuse.EEXIST,
		syscall.EACCES: fuse.EACCES, syscall.EPERM: fuse.EPERM,
		syscall.EBUSY: fuse.EBUSY, syscall.ENOSPC: fuse.ENOSPC,
		syscall.EFBIG: fuse.EFBIG, syscall.EXDEV: fuse.EXDEV,
		syscall.EINTR: fuse.EINTR, syscall.ETIMEDOUT: fuse.ETIMEDOUT,
		syscall.EINVAL: fuse.EINVAL, syscall.ENOTDIR: fuse.ENOTDIR,
		syscall.EISDIR: fuse.EISDIR, syscall.ENOTEMPTY: fuse.ENOTEMPTY,
		syscall.EROFS: fuse.EROFS, syscall.EBADF: fuse.EBADF,
	}
	if code, ok := codes[errno(err)]; ok {
		return -code
	}
	return -fuse.EIO
}

func (w *WinFsp) failure(operation string, err error) int {
	if err != nil {
		w.core.opts.Logger.Printf("%s: %v", operation, err)
	}
	return winError(err)
}

func winPath(value string) (string, bool) {
	if !strings.HasPrefix(value, "/") || strings.ContainsRune(value, '\x00') {
		return "", false
	}
	for _, part := range strings.Split(strings.Trim(value, "/"), "/") {
		if part == "" {
			continue
		}
		if !localfile.ValidName(part) {
			return "", false
		}
	}
	return path.Clean(value), true
}

func (w *WinFsp) remember(value string, entry anyshare.Entry, checked time.Time) *Node {
	w.mu.Lock()
	node := w.paths[value]
	w.mu.Unlock()
	if node != nil {
		node.mu.Lock()
		if !node.unlinked && checked.Before(node.statAt) {
			node.mu.Unlock()
			return node
		}
		if !node.unlinked && node.entry.ID == entry.ID {
			if !node.dirty && node.refs == 0 {
				node.entry = entry
				node.statAt = checked
			}
			node.mu.Unlock()
			return node
		}
		node.mu.Unlock()
	}
	fresh := w.core.makeNode(entry)
	fresh.statAt = checked
	w.mu.Lock()
	if w.paths[value] != node {
		w.mu.Unlock()
		return w.remember(value, entry, checked)
	}
	w.paths[value] = fresh
	w.mu.Unlock()
	return fresh
}

func (w *WinFsp) knownPath(value string) *Node {
	w.mu.Lock()
	node := w.paths[value]
	w.mu.Unlock()
	if node == nil {
		return nil
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	if node.unlinked || value != "/" && node.entry.Name != path.Base(value) {
		return nil
	}
	return node
}

func (w *WinFsp) resolve(ctx context.Context, value string) (*Node, error) {
	clean, ok := winPath(value)
	if !ok {
		return nil, anyshare.ErrNotFound
	}
	if clean == "/" {
		return w.core.root, nil
	}
	if known := w.knownPath(clean); known != nil {
		w.core.opts.Trace.Printf(ctx, "known path reused: %q; no ancestor directory walks", clean)
		return known, nil
	}
	current, prefix := w.core.root, ""
	for _, name := range strings.Split(strings.TrimPrefix(clean, "/"), "/") {
		if !current.snapshot().Dir {
			return nil, anyshare.ErrNotFound
		}
		next := prefix + "/" + name
		if known := w.knownPath(next); known != nil {
			current, prefix = known, next
			continue
		}
		entries, err := w.core.list(ctx, current, false)
		if err != nil {
			return nil, err
		}
		found := false
		for _, entry := range entries {
			if entry.Name == name {
				prefix += "/" + name
				current = w.remember(prefix, entry, w.core.listedAt(current.snapshot().ID))
				found = true
				break
			}
		}
		if !found {
			return nil, anyshare.ErrNotFound
		}
	}
	return current, nil
}

func (w *WinFsp) handle(fh uint64) *winHandle {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.handles[fh]
}

func (w *WinFsp) addHandle(handle *winHandle) uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.next++
	w.handles[w.next] = handle
	return w.next
}

func (w *WinFsp) node(ctx context.Context, value string, fh uint64) (*Node, error) {
	if fh != ^uint64(0) {
		if handle := w.handle(fh); handle != nil {
			return handle.node, nil
		}
	}
	return w.resolve(ctx, value)
}

func winStat(entry anyshare.Entry, mode uint32) fuse.Stat_t {
	attr := entryAttributes(entry, mode)
	return fuse.Stat_t{Ino: attr.Inode, Mode: attr.Mode, Nlink: attr.Links, Size: attr.Size,
		Blksize: 4096, Blocks: attr.Blocks, Atim: fuse.NewTimespec(attr.Modified),
		Mtim: fuse.NewTimespec(attr.Modified), Ctim: fuse.NewTimespec(attr.Modified), Birthtim: fuse.NewTimespec(attr.Created)}
}

func (w *WinFsp) Getattr(value string, stat *fuse.Stat_t, fh uint64) (result int) {
	ctx, finish := w.core.opts.Trace.Begin(w.ctx, "WinFsp getattr path=%q handle=%d", value, fh)
	defer func() { finish("%s", winResult(result)) }()
	ctx, cancel := w.operation(ctx)
	defer cancel()
	node, err := w.node(ctx, value, fh)
	if err != nil {
		return winError(err)
	}
	if err := node.refreshMetadata(ctx); err != nil {
		return winError(err)
	}
	w.core.opts.Trace.Printf(ctx, "waiting for file metadata lock")
	node.mu.Lock()
	defer node.mu.Unlock()
	if fh == ^uint64(0) && value != "/" && node.entry.Name != path.Base(value) {
		return -fuse.ENOENT
	}
	*stat = winStat(node.entry, node.effectiveModeLocked())
	w.core.opts.Trace.Printf(ctx, "attributes returned: size=%s mode=%#o", w.core.opts.Trace.Size(stat.Size), stat.Mode)
	return 0
}

func (w *WinFsp) Open(value string, flags int) (result int, fhOut uint64) {
	ctx, finish := w.core.opts.Trace.Begin(w.ctx, "WinFsp open path=%q flags=%#x", value, flags)
	defer func() { finish("%s handle=%d", winResult(result), fhOut) }()
	ctx, cancel := w.operation(ctx)
	defer cancel()
	node, err := w.resolve(ctx, value)
	if err != nil {
		return winError(err), ^uint64(0)
	}
	writable := flags&fuse.O_ACCMODE != fuse.O_RDONLY
	if writable && w.core.opts.ReadOnly {
		return -fuse.EROFS, ^uint64(0)
	}
	if err := node.refreshMetadata(ctx); err != nil {
		return winError(err), ^uint64(0)
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	if node.entry.Dir {
		return -fuse.EISDIR, ^uint64(0)
	}
	if node.entry.Name != path.Base(value) {
		return -fuse.ENOENT, ^uint64(0)
	}
	if writable {
		allowed, err := w.core.backend.Can(ctx, node.entry.ID, "modify")
		if err != nil || !allowed {
			if err == nil {
				err = anyshare.ErrPermission
			}
			return winError(err), ^uint64(0)
		}
		if err := node.ensureCacheLocked(ctx, flags&fuse.O_TRUNC != 0); err != nil {
			return winError(err), ^uint64(0)
		}
		if flags&fuse.O_TRUNC != 0 {
			if err := node.resizeLocked(0); err != nil {
				return winError(err), ^uint64(0)
			}
		}
	}
	node.refs++
	return 0, w.addHandle(&winHandle{node: node, writable: writable, append: flags&fuse.O_APPEND != 0})
}

func (w *WinFsp) Read(value string, data []byte, off int64, fh uint64) (result int) {
	ctx, finish := w.core.opts.Trace.Begin(w.ctx, "WinFsp read path=%q handle=%d offset=%d requested=%s", value, fh, off, w.core.opts.Trace.Size(int64(len(data))))
	defer func() { finish("%s", winResult(result)) }()
	handle := w.handle(fh)
	if handle == nil {
		return -fuse.EBADF
	}
	node := handle.node
	ctx, cancel := w.operation(ctx)
	defer cancel()
	n, err := node.readAt(ctx, data, off)
	if err != nil && err != io.EOF {
		return w.failure("read", err)
	}
	return n
}

func (w *WinFsp) Write(value string, data []byte, off int64, fh uint64) (result int) {
	_, finish := w.core.opts.Trace.Begin(w.ctx, "WinFsp write path=%q handle=%d offset=%d requested=%s", value, fh, off, w.core.opts.Trace.Size(int64(len(data))))
	defer func() { finish("%s", winResult(result)) }()
	handle := w.handle(fh)
	if handle == nil || !handle.writable {
		return -fuse.EBADF
	}
	node := handle.node
	node.mu.Lock()
	defer node.mu.Unlock()
	if handle.append {
		off = node.entry.Size
	}
	if off < 0 || off > int64(^uint64(0)>>1)-int64(len(data)) {
		return -fuse.EFBIG
	}
	if err := node.reserveLocked(max(node.entry.Size, off+int64(len(data)))); err != nil {
		return winError(err)
	}
	n, err := node.cache.WriteAt(data, off)
	if n > 0 {
		node.entry.Size = max(node.entry.Size, off+int64(n))
		node.entry.Modified, node.dirty = time.Now(), true
		if journalErr := node.journalLocked(); err == nil {
			err = journalErr
		}
	}
	if err != nil {
		return w.failure("write", err)
	}
	return n
}

func (w *WinFsp) Flush(value string, fh uint64) (result int) {
	ctx, finish := w.core.opts.Trace.Begin(w.ctx, "WinFsp flush path=%q handle=%d", value, fh)
	defer func() { finish("%s", winResult(result)) }()
	handle := w.handle(fh)
	if handle == nil {
		return -fuse.EBADF
	}
	if !handle.writable {
		return 0
	}
	ctx, cancel := w.operation(ctx)
	defer cancel()
	handle.node.mu.Lock()
	defer handle.node.mu.Unlock()
	return w.failure("upload on close", handle.node.flushLocked(ctx))
}

func (w *WinFsp) Fsync(value string, _ bool, fh uint64) int { return w.Flush(value, fh) }

func (w *WinFsp) Release(value string, fh uint64) (result int) {
	_, finish := w.core.opts.Trace.Begin(w.ctx, "WinFsp release path=%q handle=%d", value, fh)
	defer func() { finish("%s", winResult(result)) }()
	w.mu.Lock()
	handle := w.handles[fh]
	delete(w.handles, fh)
	w.mu.Unlock()
	if handle == nil {
		return -fuse.EBADF
	}
	handle.node.mu.Lock()
	defer handle.node.mu.Unlock()
	handle.node.refs--
	if handle.node.refs == 0 && !handle.node.dirty {
		handle.node.disposeLocked()
	}
	return 0
}

func (w *WinFsp) parent(ctx context.Context, value string) (*Node, string, int) {
	clean, ok := winPath(value)
	if !ok || clean == "/" {
		return nil, "", -fuse.EINVAL
	}
	node, err := w.resolve(ctx, path.Dir(clean))
	if err != nil {
		return nil, "", winError(err)
	}
	if !node.snapshot().Dir {
		return nil, "", -fuse.ENOTDIR
	}
	if w.core.opts.ReadOnly {
		return nil, "", -fuse.EROFS
	}
	if node.synthetic {
		return nil, "", -fuse.EPERM
	}
	return node, path.Base(clean), 0
}

func (w *WinFsp) Create(value string, flags int, mode uint32) (result int, fhOut uint64) {
	ctx, finish := w.core.opts.Trace.Begin(w.ctx, "WinFsp create path=%q flags=%#x", value, flags)
	defer func() { finish("%s handle=%d", winResult(result), fhOut) }()
	ctx, cancel := w.operation(ctx)
	defer cancel()
	w.core.opMu.Lock()
	defer w.core.opMu.Unlock()
	parent, name, code := w.parent(ctx, value)
	if code != 0 {
		return code, ^uint64(0)
	}
	allowed, err := w.core.backend.Can(ctx, parent.snapshot().ID, "create")
	if err != nil || !allowed {
		if err == nil {
			err = anyshare.ErrPermission
		}
		return winError(err), ^uint64(0)
	}
	entry, err := w.core.backend.Upload(ctx, parent.snapshot().ID, name, nil, bytes.NewReader(nil), 0, time.Now())
	if err != nil {
		return w.failure("create file", err), ^uint64(0)
	}
	entry.Modified, entry.Created = time.Now(), time.Now()
	node := w.remember(path.Clean(value), entry, time.Now())
	node.mu.Lock()
	defer node.mu.Unlock()
	node.mode = mode & 0777
	if err := node.ensureCacheLocked(ctx, true); err != nil {
		return winError(err), ^uint64(0)
	}
	node.refs++
	w.core.invalidate()
	return 0, w.addHandle(&winHandle{node: node, writable: flags&fuse.O_ACCMODE != fuse.O_RDONLY, append: flags&fuse.O_APPEND != 0})
}

func (w *WinFsp) Mkdir(value string, mode uint32) (result int) {
	ctx, finish := w.core.opts.Trace.Begin(w.ctx, "WinFsp mkdir path=%q", value)
	defer func() { finish("%s", winResult(result)) }()
	ctx, cancel := w.operation(ctx)
	defer cancel()
	w.core.opMu.Lock()
	defer w.core.opMu.Unlock()
	parent, name, code := w.parent(ctx, value)
	if code != 0 {
		return code
	}
	entry, err := w.core.backend.Mkdir(ctx, parent.snapshot().ID, name)
	if err != nil {
		return winError(err)
	}
	node := w.remember(path.Clean(value), entry, time.Now())
	node.mu.Lock()
	node.mode = mode & 0777
	node.mu.Unlock()
	w.core.invalidate()
	return 0
}

func (w *WinFsp) Truncate(value string, size int64, fh uint64) (result int) {
	ctx, finish := w.core.opts.Trace.Begin(w.ctx, "WinFsp truncate path=%q size=%d handle=%d", value, size, fh)
	defer func() { finish("%s", winResult(result)) }()
	if w.core.opts.ReadOnly {
		return -fuse.EROFS
	}
	if size < 0 {
		return -fuse.EINVAL
	}
	if handle := w.handle(fh); handle != nil && !handle.writable {
		return -fuse.EBADF
	}
	ctx, cancel := w.operation(ctx)
	defer cancel()
	node, err := w.node(ctx, value, fh)
	if err != nil {
		return winError(err)
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	if node.entry.Dir {
		return -fuse.EISDIR
	}
	if !node.unlinked {
		allowed, err := w.core.backend.Can(ctx, node.entry.ID, "modify")
		if err != nil || !allowed {
			if err == nil {
				err = anyshare.ErrPermission
			}
			return winError(err)
		}
	}
	if err := node.ensureCacheLocked(ctx, size == 0); err != nil {
		return winError(err)
	}
	if err := node.resizeLocked(size); err != nil {
		return winError(err)
	}
	if node.refs == 0 {
		if err := node.flushLocked(ctx); err != nil {
			return w.failure("commit truncate", err)
		}
		node.disposeLocked()
	}
	return 0
}

func (w *WinFsp) remove(value string, directory bool) (result int) {
	ctx, finish := w.core.opts.Trace.Begin(w.ctx, "WinFsp remove path=%q directory=%t", value, directory)
	defer func() { finish("%s", winResult(result)) }()
	ctx, cancel := w.operation(ctx)
	defer cancel()
	w.core.opMu.Lock()
	defer w.core.opMu.Unlock()
	if _, _, code := w.parent(ctx, value); code != 0 {
		return code
	}
	node, err := w.resolve(ctx, value)
	if err != nil {
		return winError(err)
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	if node.entry.Dir != directory {
		if directory {
			return -fuse.ENOTDIR
		}
		return -fuse.EISDIR
	}
	if directory {
		entries, err := w.core.backend.List(ctx, node.entry.ID)
		if err != nil {
			return winError(err)
		}
		if len(entries) > 0 {
			return -fuse.ENOTEMPTY
		}
	} else if node.refs > 0 {
		if err := node.ensureCacheLocked(ctx, false); err != nil {
			return winError(err)
		}
	}
	if err := w.core.backend.Delete(ctx, node.entry); err != nil {
		return winError(err)
	}
	node.unlinked = true
	w.core.invalidate()
	return 0
}

func (w *WinFsp) Unlink(value string) int { return w.remove(value, false) }
func (w *WinFsp) Rmdir(value string) int  { return w.remove(value, true) }

func (w *WinFsp) Rename(oldPath, newPath string) (result int) {
	ctx, finish := w.core.opts.Trace.Begin(w.ctx, "WinFsp rename from=%q to=%q", oldPath, newPath)
	defer func() { finish("%s", winResult(result)) }()
	ctx, cancel := w.operation(ctx)
	defer cancel()
	w.core.opMu.Lock()
	defer w.core.opMu.Unlock()
	oldParent, _, code := w.parent(ctx, oldPath)
	if code != 0 {
		return code
	}
	newParent, newName, code := w.parent(ctx, newPath)
	if code != 0 {
		return code
	}
	if path.Clean(oldPath) == path.Clean(newPath) {
		return 0
	}
	oldParentEntry, newParentEntry := oldParent.snapshot(), newParent.snapshot()
	if libraryID(oldParentEntry.ID) != libraryID(newParentEntry.ID) {
		return -fuse.EXDEV
	}
	source, err := w.resolve(ctx, oldPath)
	if err != nil {
		return winError(err)
	}
	target, err := w.resolve(ctx, newPath)
	if err != nil && !errors.Is(err, anyshare.ErrNotFound) {
		return winError(err)
	}
	if target == source {
		return 0
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	old := source.entry
	if old.Dir && (newParentEntry.ID == old.ID || strings.HasPrefix(newParentEntry.ID, old.ID+"/")) {
		return -fuse.EINVAL
	}
	if target != nil && target != source {
		target.mu.Lock()
		defer target.mu.Unlock()
		if target.entry.Dir != old.Dir {
			if target.entry.Dir {
				return -fuse.EISDIR
			}
			return -fuse.ENOTDIR
		}
		if target.entry.Dir {
			entries, err := w.core.backend.List(ctx, target.entry.ID)
			if err != nil {
				return winError(err)
			}
			if len(entries) > 0 {
				return -fuse.ENOTEMPTY
			}
		} else if target.refs > 0 {
			if err := target.ensureCacheLocked(ctx, false); err != nil {
				return winError(err)
			}
		}
	}
	if err := source.flushLocked(ctx); err != nil {
		return winError(err)
	}
	old = source.entry
	var updated anyshare.Entry
	if oldParentEntry.ID == newParentEntry.ID {
		updated, err = w.core.backend.Rename(ctx, old, newName, target != nil)
	} else {
		updated, err = w.core.moveAndRename(ctx, old, oldParentEntry.ID, newParentEntry.ID, newName, target != nil)
	}
	if err != nil {
		w.core.invalidate()
		return w.failure("rename", err)
	}
	updated.Created, updated.Modified = old.Created, old.Modified
	source.entry, source.statAt = updated, time.Now()
	if target != nil && target != source {
		target.unlinked = true
	}
	w.mu.Lock()
	oldPath, newPath = path.Clean(oldPath), path.Clean(newPath)
	children := map[string]*Node{}
	for value, node := range w.paths {
		if strings.HasPrefix(value, oldPath+"/") {
			children[value] = node
		}
	}
	for value, node := range children {
		delete(w.paths, value)
		w.paths[newPath+strings.TrimPrefix(value, oldPath)] = node
	}
	delete(w.paths, oldPath)
	w.paths[newPath] = source
	w.mu.Unlock()
	for _, node := range children {
		if old.ID != updated.ID {
			node.mu.Lock()
			node.entry.ID = updated.ID + strings.TrimPrefix(node.entry.ID, old.ID)
			if node.dirty {
				if err := node.journalLocked(); err != nil {
					w.failure("rebase write journal", err)
				}
			}
			node.mu.Unlock()
		}
	}
	w.core.invalidate()
	return 0
}

func (w *WinFsp) Opendir(value string) (result int, fhOut uint64) {
	ctx, finish := w.core.opts.Trace.Begin(w.ctx, "WinFsp opendir path=%q", value)
	defer func() { finish("%s handle=%d", winResult(result), fhOut) }()
	ctx, cancel := w.operation(ctx)
	defer cancel()
	node, err := w.resolve(ctx, value)
	if err != nil {
		return winError(err), ^uint64(0)
	}
	if !node.snapshot().Dir {
		return -fuse.ENOTDIR, ^uint64(0)
	}
	w.core.opts.Trace.Printf(ctx, "directory handle opened; contents will be fetched only on readdir")
	node.mu.Lock()
	node.refs++
	node.mu.Unlock()
	return 0, w.addHandle(&winHandle{node: node})
}

func (w *WinFsp) Readdir(value string, fill func(string, *fuse.Stat_t, int64) bool, off int64, fh uint64) (result int) {
	ctx, finish := w.core.opts.Trace.Begin(w.ctx, "WinFsp readdir path=%q handle=%d offset=%d", value, fh, off)
	defer func() { finish("%s", winResult(result)) }()
	handle := w.handle(fh)
	if handle == nil || off < 0 {
		return -fuse.EBADF
	}
	ctx, cancel := w.operation(ctx)
	defer cancel()
	handle.listMu.Lock()
	defer handle.listMu.Unlock()
	if !handle.listed {
		entries, err := w.core.list(ctx, handle.node, false)
		if err != nil {
			return winError(err)
		}
		handle.entries, handle.listed = entries, true
		handle.listedAt = w.core.listedAt(handle.node.snapshot().ID)
	}
	if !fill(".", nil, 0) || !fill("..", nil, 0) {
		return 0
	}
	// An open stream may enumerate its old snapshot after a mutation. Return
	// that snapshot without resurrecting names in the current path index.
	indexSnapshot := !handle.listedAt.IsZero() && handle.listedAt == w.core.listedAt(handle.node.snapshot().ID)
	for i, entry := range handle.entries {
		if int64(i) < off || !localfile.ValidName(entry.Name) {
			continue
		}
		mode := uint32(0600)
		if entry.Dir {
			mode = 0700
		}
		if w.core.opts.ReadOnly {
			mode &= 0555
		}
		stat := winStat(entry, mode)
		if indexSnapshot {
			w.remember(path.Join(value, entry.Name), entry, handle.listedAt)
		}
		if !fill(entry.Name, &stat, int64(i+1)) {
			break
		}
	}
	return 0
}

func (w *WinFsp) Releasedir(value string, fh uint64) int { return w.Release(value, fh) }
func (w *WinFsp) Fsyncdir(string, bool, uint64) int      { return 0 }

func (w *WinFsp) Statfs(value string, stat *fuse.Statfs_t) (result int) {
	ctx, finish := w.core.opts.Trace.Begin(w.ctx, "WinFsp statfs path=%q", value)
	defer func() { finish("%s", winResult(result)) }()
	ctx, cancel := w.operation(ctx)
	defer cancel()
	total, available, err := w.core.quotaSpace(ctx)
	if err != nil {
		return winError(err)
	}
	const blockSize int64 = 4096
	*stat = fuse.Statfs_t{
		Bsize: uint64(blockSize), Frsize: uint64(blockSize),
		Blocks: quotaBlocks(total, blockSize),
		Bfree:  uint64(max(available/blockSize, 0)),
		Bavail: uint64(max(available/blockSize, 0)),
		Files:  1 << 48, Ffree: 1 << 48, Namemax: 255,
	}
	return 0
}

func (w *WinFsp) Access(value string, mask uint32) (result int) {
	ctx, finish := w.core.opts.Trace.Begin(w.ctx, "WinFsp access path=%q mask=%#x", value, mask)
	defer func() { finish("%s", winResult(result)) }()
	if mask&2 != 0 && w.core.opts.ReadOnly {
		return -fuse.EROFS
	}
	ctx, cancel := w.operation(ctx)
	defer cancel()
	node, err := w.resolve(ctx, value)
	if err != nil {
		return winError(err)
	}
	if mask&2 != 0 && node.synthetic {
		return -fuse.EACCES
	}
	return 0
}

func (w *WinFsp) Chmod(value string, mode uint32) (result int) {
	ctx, finish := w.core.opts.Trace.Begin(w.ctx, "WinFsp chmod path=%q mode=%#o", value, mode)
	defer func() { finish("%s", winResult(result)) }()
	if w.core.opts.ReadOnly {
		return -fuse.EROFS
	}
	ctx, cancel := w.operation(ctx)
	defer cancel()
	node, err := w.resolve(ctx, value)
	if err != nil {
		return winError(err)
	}
	if node.synthetic {
		return -fuse.EPERM
	}
	node.mu.Lock()
	node.mode = mode & 0777
	node.mu.Unlock()
	return 0
}

func (w *WinFsp) Chflags(value string, _ uint32) int {
	// DOS archive/hidden flags have no matching AnyShare metadata.
	return w.Access(value, 2)
}

func (w *WinFsp) Utimens(value string, times []fuse.Timespec) (result int) {
	ctx, finish := w.core.opts.Trace.Begin(w.ctx, "WinFsp set timestamps path=%q", value)
	defer func() { finish("%s", winResult(result)) }()
	if w.core.opts.ReadOnly {
		return -fuse.EROFS
	}
	ctx, cancel := w.operation(ctx)
	defer cancel()
	node, err := w.resolve(ctx, value)
	if err != nil {
		return winError(err)
	}
	if node.synthetic {
		return -fuse.EPERM
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	mtime := time.Now()
	if len(times) > 1 {
		mtime = times[1].Time()
	}
	if node.entry.Dir {
		node.entry.Modified = mtime
		return 0
	}
	if err := node.ensureCacheLocked(ctx, false); err != nil {
		return winError(err)
	}
	node.entry.Modified, node.dirty = mtime, true
	if err := node.journalLocked(); err != nil {
		return winError(err)
	}
	if node.refs == 0 {
		if err := node.flushLocked(ctx); err != nil {
			return w.failure("commit timestamp", err)
		}
		node.disposeLocked()
	}
	return 0
}

var _ fuse.FileSystemInterface = (*WinFsp)(nil)
