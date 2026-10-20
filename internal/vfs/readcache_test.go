package vfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unofficialpku/disk/internal/anyshare"
)

const readBlockSize int64 = 4 << 20

type readProbe struct {
	*memoryBackend
	reads, downloads atomic.Int64
	read             func(context.Context, anyshare.Entry, []byte, int64) (int, error)
	stream           func(context.Context, anyshare.Entry, int64, int64, io.Writer) error
}

func (b *readProbe) ReadAt(ctx context.Context, e anyshare.Entry, data []byte, off int64) (int, error) {
	b.reads.Add(1)
	if b.read != nil {
		return b.read(ctx, e, data, off)
	}
	return b.memoryBackend.ReadAt(ctx, e, data, off)
}

func (b *readProbe) ReadRange(ctx context.Context, e anyshare.Entry, off, length int64, dest io.Writer) error {
	if b.stream != nil {
		b.reads.Add(1)
		return b.stream(ctx, e, off, length, dest)
	}
	data := make([]byte, length)
	n, err := b.ReadAt(ctx, e, data, off)
	if err == io.EOF && n == len(data) {
		err = nil
	}
	if err == nil && n != len(data) {
		err = io.ErrUnexpectedEOF
	}
	if err == nil {
		_, err = dest.Write(data)
	}
	return err
}

func (b *readProbe) Download(ctx context.Context, e anyshare.Entry, out io.Writer) error {
	b.downloads.Add(1)
	return b.memoryBackend.Download(ctx, e, out)
}

func readCacheTest(t *testing.T, limit int64, read ...ReadOptions) (*FileSystem, *readProbe) {
	t.Helper()
	b := &readProbe{memoryBackend: newMemory()}
	var policy *ReadOptions
	if len(read) > 0 {
		policy = &read[0]
	}
	f, err := New(b, Options{Root: &b.roots[0], CacheDir: filepath.Join(t.TempDir(), "cache"), CacheLimit: limit, Read: policy, IOTimeout: 5 * time.Second, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f, b
}

func seedFile(t *testing.T, b *memoryBackend, name string, data []byte) anyshare.Entry {
	t.Helper()
	entry, err := b.Upload(context.Background(), b.roots[0].ID, name, nil, bytes.NewReader(data), int64(len(data)), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func TestReadCacheDecoderReadsAndReopen(t *testing.T) {
	f, b := readCacheTest(t, 1<<20)
	data := bytes.Repeat([]byte("photo bytes"), 1000)
	entry := seedFile(t, b.memoryBackend, "photo.jpg", data)
	for open := 0; open < 3; open++ {
		node := f.makeNode(entry)
		for off := 0; off+32 <= len(data); off += 31 {
			got := make([]byte, 32)
			count, err := node.readAt(context.Background(), got, int64(off))
			if err != nil || count != len(got) || !bytes.Equal(got, data[off:off+32]) {
				t.Fatal("decoder read differed", count, err)
			}
		}
	}
	f.readWG.Wait()
	if b.reads.Load() != 1 || f.cacheBytes != int64(len(data)) {
		t.Fatalf("small reads/reopens fetched %d ranges, charged %d bytes", b.reads.Load(), f.cacheBytes)
	}
}

func TestReadCacheCoalescesWithoutBlockingMetadataOrWarmReads(t *testing.T) {
	policy := DefaultReadOptions()
	policy.WholeFile, policy.ReadAhead = 0, 0
	f, b := readCacheTest(t, 3*readBlockSize, policy)
	data := bytes.Repeat([]byte{0x7b}, int(2*readBlockSize))
	node := f.makeNode(seedFile(t, b.memoryBackend, "photo.jpg", data))
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	b.read = func(ctx context.Context, e anyshare.Entry, buf []byte, off int64) (int, error) {
		if off > 0 {
			once.Do(func() { close(started) })
			select {
			case <-release:
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}
		return b.memoryBackend.ReadAt(ctx, e, buf, off)
	}
	if _, err := node.readAt(context.Background(), make([]byte, 1), 0); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		group.Go(func() {
			got := make([]byte, 32)
			n, err := node.readAt(context.Background(), got, readBlockSize+7)
			if err == nil && (n != 32 || !bytes.Equal(got, data[:32])) {
				err = errors.New("concurrent read returned incorrect bytes")
			}
			errs <- err
		})
	}
	<-started
	responsive := make(chan bool, 1)
	go func() {
		entry := node.snapshot()
		_, err := node.readAt(context.Background(), make([]byte, 16), 0)
		responsive <- err == nil && entry.Size == int64(len(data))
	}()
	select {
	case ok := <-responsive:
		if !ok {
			t.Error("metadata or warm read failed")
		}
	case <-time.After(time.Second):
		t.Error("network read blocked metadata or warm cached reads")
	}
	close(release)
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if b.reads.Load() != 2 {
		t.Fatalf("coalesced readers fetched %d ranges, want two distinct blocks", b.reads.Load())
	}
}

func TestReadCacheSharesBudgetAndNeverEvictsPendingWrites(t *testing.T) {
	f, b := readCacheTest(t, 12)
	first := f.makeNode(seedFile(t, b.memoryBackend, "first", []byte("12345678")))
	second := f.makeNode(seedFile(t, b.memoryBackend, "second", []byte("abcdefgh")))
	if _, err := first.readAt(context.Background(), make([]byte, 8), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := second.readAt(context.Background(), make([]byte, 8), 0); err != nil {
		t.Fatal(err)
	}
	if f.cacheBytes != 8 {
		t.Fatal("clean blocks exceeded the shared budget")
	}
	writer := f.makeNode(seedFile(t, b.memoryBackend, "pending", []byte("pending-data")))
	writer.mu.Lock()
	err := writer.ensureCacheLocked(context.Background(), false)
	if err == nil {
		writer.dirty = true
		err = writer.journalLocked()
	}
	writer.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 8)
	if _, err := second.readAt(context.Background(), got, 0); err != nil || string(got) != "abcdefgh" {
		t.Fatal("read failed with budget occupied by pending writes", err)
	}
	if f.cacheBytes != 12 || len(f.readFiles) != 0 {
		t.Fatal("reads consumed pending-write budget")
	}
	if data, err := os.ReadFile(writer.cachePath); err != nil || string(data) != "pending-data" {
		t.Fatal("pending data evicted", err)
	}
}

func TestReadCachePromotesWritesAndRetainsCommittedData(t *testing.T) {
	f, b := readCacheTest(t, 8)
	entry := seedFile(t, b.memoryBackend, "photo.jpg", []byte("original"))
	node := f.makeNode(entry)
	if _, err := node.readAt(context.Background(), make([]byte, 8), 0); err != nil {
		t.Fatal(err)
	}
	node.mu.Lock()
	err := node.ensureCacheLocked(context.Background(), false)
	if err == nil {
		_, err = node.cache.WriteAt([]byte("modified"), 0)
		node.dirty = true
	}
	if err == nil {
		err = node.flushLocked(context.Background())
	}
	updated := node.entry
	node.disposeLocked()
	node.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if b.downloads.Load() != 0 || f.cacheBytes != 8 {
		t.Fatal("promotion downloaded or double-charged a complete read cache")
	}
	got := make([]byte, 8)
	if _, err := f.makeNode(updated).readAt(context.Background(), got, 0); err != nil || string(got) != "modified" {
		t.Fatal("committed cache was not reused", err)
	}
	if b.reads.Load() != 1 {
		t.Fatal("post-write read fetched another remote range")
	}
}

func TestReadCacheRevisionAndRangeBoundaries(t *testing.T) {
	f, b := readCacheTest(t, 3*readBlockSize)
	data := bytes.Repeat([]byte("abcd"), int(readBlockSize/4)+8)
	entry := seedFile(t, b.memoryBackend, "photo.jpg", data)
	node := f.makeNode(entry)
	got := make([]byte, 16)
	if count, err := node.readAt(context.Background(), got, readBlockSize-8); err != nil || count != 16 || !bytes.Equal(got, data[readBlockSize-8:readBlockSize+8]) {
		t.Fatal("read across block boundary failed", count, err)
	}
	if count, err := node.readAt(context.Background(), got, entry.Size-4); err != nil || count != 4 {
		t.Fatal("read across EOF failed", count, err)
	}
	if count, err := node.readAt(context.Background(), got, entry.Size); err != nil || count != 0 {
		t.Fatal("EOF read failed", count, err)
	}
	b.mu.Lock()
	entry.Rev = "new-revision"
	b.entries[entry.ID] = entry
	b.data[entry.ID][0] = 'z'
	b.mu.Unlock()
	node.mu.Lock()
	node.entry = entry
	node.mu.Unlock()
	if _, err := node.readAt(context.Background(), got, 0); err != nil || got[0] != 'z' {
		t.Fatal("read cache served an old revision", err)
	}
}

func TestReadCacheLargeFileIsBoundedAndFailuresAreNotCached(t *testing.T) {
	f, b := readCacheTest(t, 2*readBlockSize)
	entry := anyshare.Entry{ID: b.roots[0].ID + "/large", Rev: "1", Size: 20 << 30}
	b.read = func(_ context.Context, _ anyshare.Entry, buf []byte, off int64) (int, error) {
		if b.reads.Load() == 1 {
			return 1, nil
		}
		for i := range buf {
			buf[i] = byte((off + int64(i)) % 251)
		}
		return len(buf), nil
	}
	node := f.makeNode(entry)
	got := make([]byte, 64)
	if _, err := node.readAt(context.Background(), got, entry.Size-64); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("short range was accepted", err)
	}
	if _, err := node.readAt(context.Background(), got, entry.Size-64); err != nil {
		t.Fatal(err)
	}
	for i, value := range got {
		if value != byte((entry.Size-64+int64(i))%251) {
			t.Fatal("large file bytes differ")
		}
	}
	if b.reads.Load() != 2 || f.cacheBytes != readBlockSize {
		t.Fatal("large-file reads cached more than a single block or retained a failed range")
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.CachePath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("clean cache survived unmount", err)
	}
}

func TestReadCacheCancellationAndConcurrentLocalWrite(t *testing.T) {
	f, b := readCacheTest(t, 1<<20)
	entry := seedFile(t, b.memoryBackend, "photo.jpg", []byte("original"))
	node := f.makeNode(entry)
	started, release := make(chan struct{}), make(chan struct{})
	b.read = func(ctx context.Context, e anyshare.Entry, buf []byte, off int64) (int, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
		return b.memoryBackend.ReadAt(ctx, e, buf, off)
	}
	done := make(chan error, 1)
	go func() {
		got := make([]byte, 8)
		_, err := node.readAt(context.Background(), got, 0)
		if err == nil && string(got) != "modified" {
			err = errors.New("reader ignored a concurrent local write")
		}
		done <- err
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := node.readAt(ctx, make([]byte, 8), 0); !errors.Is(err, context.Canceled) {
		t.Error("waiting reader did not honor cancellation", err)
	}
	node.mu.Lock()
	err := node.ensureCacheLocked(context.Background(), false)
	if err == nil {
		_, err = node.cache.WriteAt([]byte("modified"), 0)
		node.dirty = true
	}
	node.mu.Unlock()
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
