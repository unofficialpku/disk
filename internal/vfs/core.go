// Package vfs shares remote namespace, cached reads/writes and recovery between
// go-fuse on Unix and WinFsp on Windows.
package vfs

import (
	"container/list"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/unofficialpku/disk/internal/diagnostic"
	"github.com/unofficialpku/disk/internal/localfile"
	"hash/fnv"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// errorCode classifies core, remote and local errors once. Platform adapters
// only translate the resulting errno to their driver's ABI.
func errorCode(err error) syscall.Errno {
	if err == nil {
		return 0
	}
	switch {
	case errors.Is(err, anyshare.ErrNotFound), errors.Is(err, os.ErrNotExist):
		return syscall.ENOENT
	case errors.Is(err, anyshare.ErrExists), errors.Is(err, os.ErrExist):
		return syscall.EEXIST
	case errors.Is(err, anyshare.ErrPermission), errors.Is(err, anyshare.ErrPending), errors.Is(err, os.ErrPermission):
		return syscall.EACCES
	case errors.Is(err, anyshare.ErrConflict):
		return syscall.EBUSY
	case errors.Is(err, anyshare.ErrQuota):
		return syscall.ENOSPC
	case errors.Is(err, anyshare.ErrSizeLimit):
		return syscall.EFBIG
	case errors.Is(err, context.Canceled):
		return syscall.EINTR
	case errors.Is(err, context.DeadlineExceeded):
		return syscall.ETIMEDOUT
	}
	var code syscall.Errno
	if errors.As(err, &code) {
		return code
	}
	return syscall.EIO
}

type Backend interface {
	Roots(context.Context) ([]anyshare.Entry, error)
	List(context.Context, string) ([]anyshare.Entry, error)
	Stat(context.Context, anyshare.Entry) (anyshare.Entry, error)
	Can(context.Context, string, string) (bool, error)
	ReadAt(context.Context, anyshare.Entry, []byte, int64) (int, error)
	ReadRange(context.Context, anyshare.Entry, int64, int64, io.Writer) error
	Download(context.Context, anyshare.Entry, io.Writer) error
	Upload(context.Context, string, string, *anyshare.Entry, io.ReaderAt, int64, time.Time) (anyshare.Entry, error)
	Mkdir(context.Context, string, string) (anyshare.Entry, error)
	Delete(context.Context, anyshare.Entry) error
	Rename(context.Context, anyshare.Entry, string, bool) (anyshare.Entry, error)
	Move(context.Context, anyshare.Entry, string) (anyshare.Entry, error)
}

type quotaBackend interface {
	StorageQuota(context.Context, string, bool) (anyshare.Quota, error)
}

type Options struct {
	Root       *anyshare.Entry
	ReadOnly   bool
	CacheDir   string
	CacheLimit int64
	Read       *ReadOptions
	TTL        time.Duration
	IOTimeout  time.Duration
	Logger     *log.Logger
	Trace      *diagnostic.Logger
}
type cachedList struct {
	entries []anyshare.Entry
	expires time.Time
}

type listFlight struct {
	done    chan struct{}
	entries []anyshare.Entry
	err     error
}

type statFlight struct {
	done chan struct{}
	err  error
}

type quotaFlight struct {
	done  chan struct{}
	quota anyshare.Quota
	err   error
}

type FileSystem struct {
	backend     Backend
	opts        Options
	cacheDir    string
	opMu        sync.Mutex
	listMu      sync.Mutex
	listCache   map[string]cachedList
	listFlights map[string]*listFlight
	budgetMu    sync.Mutex
	cacheBytes  int64
	readBytes   int64
	readSlots   chan struct{}
	readFiles   map[cacheKey]*list.Element
	readLRU     list.List
	readFlights map[cacheKey]*readFlight
	readCtx     context.Context
	cancelReads context.CancelFunc
	readWG      sync.WaitGroup
	quotaMu     sync.Mutex
	quota       anyshare.Quota
	quotaAt     time.Time
	quotaFlight *quotaFlight
	closed      bool // Protected by budgetMu, alongside flight registration.
	nodes       sync.Map
	root        *Node
}

type Node struct {
	inodeBase
	traceName  string // Initial name, readable before taking the node lock.
	f          *FileSystem
	mu         sync.Mutex
	entry      anyshare.Entry
	synthetic  bool
	cache      *os.File
	cachePath  string
	charged    int64
	dirty      bool
	unlinked   bool
	refs       int
	statAt     time.Time
	statFlight *statFlight
	mode       uint32
	readEnd    int64
	readSize   int64
	readRev    string
}

func New(backend Backend, opts Options) (*FileSystem, error) {
	if backend == nil {
		return nil, errors.New("missing backend")
	}
	if opts.TTL < 0 || opts.CacheLimit < 0 {
		return nil, errors.New("cache limits must be nonnegative")
	}
	read := DefaultReadOptions()
	if opts.Read != nil {
		read = *opts.Read
	}
	if err := read.Validate(); err != nil {
		return nil, err
	}
	opts.Read = &read
	if opts.IOTimeout <= 0 {
		opts.IOTimeout = 10 * time.Minute
	}
	if opts.Logger == nil {
		opts.Logger = log.New(os.Stderr, "pkudisk: ", log.LstdFlags)
	}
	if opts.CacheDir == "" {
		return nil, errors.New("a private cache directory is required")
	}
	if err := localfile.EnsurePrivateDir(opts.CacheDir); err != nil {
		return nil, err
	}
	cache, err := os.MkdirTemp(opts.CacheDir, "mount-")
	if err != nil {
		return nil, err
	}
	f := &FileSystem{backend: backend, opts: opts, cacheDir: cache, listCache: map[string]cachedList{}, listFlights: make(map[string]*listFlight), readFiles: make(map[cacheKey]*list.Element), readFlights: make(map[cacheKey]*readFlight), readSlots: make(chan struct{}, read.Concurrency)}
	f.readCtx, f.cancelReads = context.WithCancel(context.Background())
	opts.Trace.Printf(context.Background(), "mount policy: whole-file=%s block=%s read-ahead=%s concurrency=%d disk-budget=%s metadata-TTL=%s I/O-timeout=%s read-only=%t", opts.Trace.Size(read.WholeFile), opts.Trace.Size(read.BlockSize), opts.Trace.Size(read.ReadAhead), read.Concurrency, opts.Trace.Size(opts.CacheLimit), opts.TTL, opts.IOTimeout, opts.ReadOnly)
	if opts.Root != nil {
		if !opts.Root.Dir {
			f.cancelReads()
			os.Remove(cache)
			return nil, syscall.ENOTDIR
		}
		f.root = f.makeNode(*opts.Root)
	} else {
		f.root = f.makeNode(anyshare.Entry{Name: "PKUDisk", Dir: true, Size: -1})
		f.root.synthetic = true
	}
	return f, nil
}
func (f *FileSystem) Root() *Node       { return f.root }
func (f *FileSystem) CachePath() string { return f.cacheDir }
func (f *FileSystem) makeNode(e anyshare.Entry) *Node {
	n := &Node{f: f, entry: e, statAt: time.Now(), traceName: e.Name}
	if backend, ok := f.backend.(interface{ VirtualDirectory(string) bool }); ok {
		n.synthetic = backend.VirtualDirectory(e.ID)
	}
	if e.Dir {
		n.mode = 0700
	} else {
		n.mode = 0600
	}
	f.nodes.Store(n, true)
	return n
}
func (n *Node) snapshot() anyshare.Entry { n.mu.Lock(); defer n.mu.Unlock(); return n.entry }

func (f *FileSystem) list(ctx context.Context, n *Node, fresh bool) (result []anyshare.Entry, resultErr error) {
	ctx, finish := f.opts.Trace.Begin(ctx, "list directory %q", n.traceName)
	defer func() { finish("entries=%d; %s", len(result), diagnostic.Status(resultErr)) }()
	e := n.snapshot()
	f.listMu.Lock()
	hit, ok := f.listCache[e.ID]
	if !fresh && ok && time.Now().Before(hit.expires) {
		f.listMu.Unlock()
		f.opts.Trace.Printf(ctx, "directory cache hit: entries=%d", len(hit.entries))
		return append([]anyshare.Entry(nil), hit.entries...), nil
	}
	if flight := f.listFlights[e.ID]; flight != nil {
		f.listMu.Unlock()
		f.opts.Trace.Printf(ctx, "waiting for shared directory listing")
		select {
		case <-flight.done:
			return append([]anyshare.Entry(nil), flight.entries...), flight.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	flight := &listFlight{done: make(chan struct{})}
	f.listFlights[e.ID] = flight
	f.listMu.Unlock()
	// Invalidating the namespace detaches in-flight listings. They still finish
	// for their callers, but cannot repopulate a cache from before the mutation.
	defer func() {
		f.listMu.Lock()
		if f.listFlights[e.ID] == flight {
			delete(f.listFlights, e.ID)
			if resultErr == nil {
				f.listCache[e.ID] = cachedList{append([]anyshare.Entry(nil), result...), time.Now().Add(f.opts.TTL)}
			}
		}
		flight.entries, flight.err = result, resultErr
		close(flight.done)
		f.listMu.Unlock()
	}()
	var entries []anyshare.Entry
	var err error
	f.opts.Trace.Printf(ctx, "directory cache miss; fetching remote listing (forced=%t)", fresh)
	if n.synthetic && e.ID == "" {
		entries, err = f.backend.Roots(ctx)
	} else {
		entries, err = f.backend.List(ctx, e.ID)
	}
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !anyshare.ValidName(e.Name) || e.ID == "" {
			return nil, errors.New("server returned an unsupported directory entry")
		}
	}
	return entries, nil
}

func (f *FileSystem) listedAt(id string) time.Time {
	f.listMu.Lock()
	defer f.listMu.Unlock()
	if list, ok := f.listCache[id]; ok {
		return list.expires.Add(-f.opts.TTL)
	}
	return time.Time{}
}
func (f *FileSystem) invalidate() {
	f.listMu.Lock()
	f.listCache = map[string]cachedList{}
	f.listFlights = make(map[string]*listFlight)
	f.listMu.Unlock()
	f.quotaMu.Lock()
	f.quotaAt = time.Time{}
	f.quotaFlight = nil
	f.quotaMu.Unlock()
}

const quotaCacheTTL = 5 * time.Second

func (f *FileSystem) quotaSnapshot(ctx context.Context) (anyshare.Quota, error) {
	provider, ok := f.backend.(quotaBackend)
	if !ok {
		return anyshare.Quota{}, errors.New("backend does not expose server quota")
	}
	f.quotaMu.Lock()
	if !f.quotaAt.IsZero() && time.Since(f.quotaAt) < quotaCacheTTL {
		quota := f.quota
		f.quotaMu.Unlock()
		return quota, nil
	}
	if flight := f.quotaFlight; flight != nil {
		f.quotaMu.Unlock()
		select {
		case <-ctx.Done():
			return anyshare.Quota{}, ctx.Err()
		case <-flight.done:
			return flight.quota, flight.err
		}
	}
	flight := &quotaFlight{done: make(chan struct{})}
	f.quotaFlight = flight
	f.quotaMu.Unlock()
	root := ""
	if f.opts.Root != nil {
		root = f.opts.Root.ID
	}
	ctx, cancel := context.WithTimeout(ctx, f.opts.IOTimeout)
	defer cancel()
	quota, err := provider.StorageQuota(ctx, root, f.opts.ReadOnly)
	f.quotaMu.Lock()
	defer f.quotaMu.Unlock()
	if f.quotaFlight == flight {
		f.quotaFlight = nil
		if err == nil {
			f.quota, f.quotaAt = quota, time.Now()
		}
	}
	flight.quota, flight.err = quota, err
	close(flight.done)
	return quota, err
}

func quotaBlocks(bytes, block int64) uint64 {
	if bytes <= 0 {
		return 0
	}
	blocks := uint64(bytes / block)
	if bytes%block != 0 {
		blocks++
	}
	return blocks
}

func (f *FileSystem) quotaSpace(ctx context.Context) (total, available int64, err error) {
	quota, err := f.quotaSnapshot(ctx)
	if err != nil {
		return 0, 0, err
	}
	return quota.Allocated, quota.Available(), nil
}

func (n *Node) reserveLocked(size int64) error {
	if size < 0 {
		return syscall.EFBIG
	}
	f := n.f
	f.budgetMu.Lock()
	defer f.budgetMu.Unlock()
	delta := size - n.charged
	if delta > 0 && !f.cacheRoomLocked(delta) {
		f.opts.Trace.Printf(context.Background(), "write cache allocation denied: file=%q requested=%s budget=%s used=%s", n.entry.Name, f.opts.Trace.Size(size), f.opts.Trace.Size(f.opts.CacheLimit), f.opts.Trace.Size(f.cacheBytes))
		return syscall.ENOSPC
	}
	f.cacheBytes += delta
	n.charged = size
	return nil
}

func (n *Node) ensureCacheLocked(ctx context.Context, truncate bool) (result error) {
	ctx, finish := n.f.opts.Trace.Begin(ctx, "prepare write cache file=%q truncate=%t", n.entry.Name, truncate)
	defer func() { finish("%s", diagnostic.Status(result)) }()
	if n.cache != nil {
		n.f.opts.Trace.Printf(ctx, "write cache already present; no download")
		return nil
	}
	if !truncate {
		if file, size := n.f.takeCachedFile(n.entry); file != nil {
			n.f.opts.Trace.Printf(ctx, "promoted complete clean read cache; reused=%s; no download", n.f.opts.Trace.Size(size))
			n.cache, n.cachePath, n.charged = file, file.Name(), size
			return nil
		}
	}
	size := n.entry.Size
	if truncate {
		size = 0
	}
	if err := n.reserveLocked(size); err != nil {
		return err
	}
	file, err := os.CreateTemp(n.f.cacheDir, "file-*.data")
	if err != nil {
		n.reserveLocked(0)
		return err
	}
	if !truncate && size > 0 {
		n.f.opts.Trace.Printf(ctx, "downloading existing file into write cache: size=%s", n.f.opts.Trace.Size(size))
		err = n.f.backend.Download(ctx, n.entry, file)
	}
	if err != nil {
		file.Close()
		os.Remove(file.Name())
		n.reserveLocked(0)
		return err
	}
	n.cache = file
	n.cachePath = file.Name()
	return nil
}

func (n *Node) journalLocked() error {
	record := RecoveryRecord{Entry: n.entry, Parent: parentID(n.entry.ID), Data: filepath.Base(n.cachePath), Updated: time.Now()}
	if backend, ok := n.f.backend.(*AccountBackend); ok {
		record = backend.recoveryRecord(record)
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(n.f.cacheDir, "journal-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = localfile.Install(tmp.Name(), n.cachePath+".json", true)
	}
	return err
}

func (n *Node) resizeLocked(size int64) error {
	if err := n.reserveLocked(size); err != nil {
		return err
	}
	if err := n.cache.Truncate(size); err != nil {
		return err
	}
	n.entry.Size = size
	n.entry.Modified = time.Now()
	n.dirty = true
	return n.journalLocked()
}

func (n *Node) flushLocked(ctx context.Context) (result error) {
	ctx, finish := n.f.opts.Trace.Begin(ctx, "flush file=%q size=%s", n.entry.Name, n.f.opts.Trace.Size(n.entry.Size))
	defer func() { finish("%s", diagnostic.Status(result)) }()
	if !n.dirty {
		n.f.opts.Trace.Printf(ctx, "file is clean; no upload")
		return nil
	}
	if n.unlinked {
		n.dirty = false
		os.Remove(n.cachePath + ".json")
		return nil
	}
	if err := n.cache.Sync(); err != nil {
		return err
	}
	if err := n.journalLocked(); err != nil {
		return err
	}
	old := n.entry
	n.f.opts.Trace.Printf(ctx, "uploading pending write: id=%q revision=%q", old.ID, old.Rev)
	entry, err := n.f.backend.Upload(ctx, parentID(old.ID), old.Name, &old, n.cache, old.Size, old.Modified)
	if err != nil {
		n.f.opts.Logger.Printf("uncommitted write retained at %s", n.cachePath)
		return err
	}
	entry.Created, entry.Modified = old.Created, old.Modified
	n.entry = entry
	n.dirty = false
	n.statAt = time.Now()
	os.Remove(n.cachePath + ".json")
	n.f.invalidate()
	n.f.opts.Trace.Printf(ctx, "write published: revision=%q; recovery journal removed", entry.Rev)
	return nil
}

func (n *Node) disposeLocked() {
	if n.cache == nil {
		return
	}
	n.cache.Close()
	n.cache = nil
	if !n.dirty {
		n.f.opts.Trace.Printf(context.Background(), "write cache released: file=%q clean=true unlinked=%t size=%s", n.entry.Name, n.unlinked, n.f.opts.Trace.Size(n.charged))
		os.Remove(n.cachePath + ".json")
		if !n.unlinked && n.entry.Rev != "" {
			n.f.retainCachedFile(n.entry, n.cachePath, n.charged)
			n.charged = 0 // The clean-cache LRU now owns this allocation.
			return
		}
		os.Remove(n.cachePath)
	}
	n.reserveLocked(0)
}

func (f *FileSystem) Close() (result error) {
	_, finish := f.opts.Trace.Begin(context.Background(), "close mount cache")
	defer func() { finish("%s", diagnostic.Status(result)) }()
	f.budgetMu.Lock()
	f.closed = true
	f.cancelReads()
	f.budgetMu.Unlock()
	// Readers may have returned while their shared range continues filling.
	// Stop those transfers before disposing nodes or deleting the mount cache.
	f.readWG.Wait()
	dirty := 0
	f.nodes.Range(func(key, value any) bool {
		n := key.(*Node)
		n.mu.Lock()
		if n.dirty {
			dirty++
		}
		n.disposeLocked()
		n.mu.Unlock()
		return true
	})
	f.clearReadCache()
	os.Remove(f.cacheDir)
	if dirty > 0 {
		return fmtErrorRecovery(dirty, f.cacheDir)
	}
	return nil
}
func fmtErrorRecovery(count int, path string) error {
	return errors.New("uncommitted cached writes remain in " + path + "; use the recovery command before removing the cache")
}

func RandomName() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return ".pkudisk-move-" + hex.EncodeToString(b[:])
}

// errno maps protocol and context failures once; driver adapters only translate
// the resulting platform errno into their ABI's representation.
func errno(err error) syscall.Errno { return errorCode(err) }

type nodeAttributes struct {
	Inode             uint64
	Mode, Links       uint32
	Size, Blocks      int64
	Modified, Created time.Time
}

func entryAttributes(entry anyshare.Entry, mode uint32) nodeAttributes {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(entry.ID))
	attr := nodeAttributes{Inode: max(hash.Sum64(), 2), Mode: 0100000 | mode, Links: 1, Size: max(entry.Size, 0), Modified: entry.Modified, Created: entry.Created}
	if entry.Dir {
		attr.Mode, attr.Links, attr.Size = 0040000|mode, 2, 0
	}
	attr.Blocks = int64(quotaBlocks(attr.Size, 512))
	if attr.Modified.IsZero() {
		attr.Modified = time.Unix(0, 0)
	}
	if attr.Created.IsZero() {
		attr.Created = attr.Modified
	}
	return attr
}

// Caller holds the node lock. Synthetic account directories are always read
// only, with the same permissions on both filesystem drivers.
func (n *Node) effectiveModeLocked() uint32 {
	mode := n.mode
	if n.synthetic {
		mode = 0500
	}
	if n.f.opts.ReadOnly {
		mode &^= 0222
	}
	return mode
}
