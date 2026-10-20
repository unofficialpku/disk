package vfs

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unofficialpku/disk/internal/anyshare"
)

type metadataProbe struct {
	*readProbe
	rootCalls, listCalls, statCalls atomic.Int64
	list                            func(context.Context, string) ([]anyshare.Entry, error)
	stat                            func(context.Context, anyshare.Entry) (anyshare.Entry, error)
}

func (b *metadataProbe) Roots(ctx context.Context) ([]anyshare.Entry, error) {
	b.rootCalls.Add(1)
	return b.memoryBackend.Roots(ctx)
}

func (b *metadataProbe) List(ctx context.Context, id string) ([]anyshare.Entry, error) {
	b.listCalls.Add(1)
	if b.list != nil {
		return b.list(ctx, id)
	}
	return b.memoryBackend.List(ctx, id)
}

func (b *metadataProbe) Stat(ctx context.Context, entry anyshare.Entry) (anyshare.Entry, error) {
	b.statCalls.Add(1)
	if b.stat != nil {
		return b.stat(ctx, entry)
	}
	return b.memoryBackend.Stat(ctx, entry)
}

func metadataTest(t *testing.T) (*FileSystem, *metadataProbe) {
	t.Helper()
	f, reads := readCacheTest(t, 16<<20)
	f.opts.TTL = time.Hour
	b := &metadataProbe{readProbe: reads}
	f.backend = b
	return f, b
}

func TestMetadataRefreshSharesFetchWithoutBlockingCachedRead(t *testing.T) {
	f, b := metadataTest(t)
	data := []byte("cached photo")
	node := f.makeNode(seedFile(t, b.memoryBackend, "photo.jpg", data))
	if _, err := node.readAt(context.Background(), make([]byte, len(data)), 0); err != nil {
		t.Fatal(err)
	}
	node.statAt = time.Time{}
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	b.stat = func(ctx context.Context, entry anyshare.Entry) (anyshare.Entry, error) {
		once.Do(func() { close(started) })
		select {
		case <-release:
		case <-ctx.Done():
			return anyshare.Entry{}, ctx.Err()
		}
		return b.memoryBackend.Stat(ctx, entry)
	}
	var group sync.WaitGroup
	for i := 0; i < 16; i++ {
		group.Go(func() {
			if err := node.refreshMetadata(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := node.refreshMetadata(ctx); !errors.Is(err, context.Canceled) {
		t.Error("metadata waiter ignored cancellation", err)
	}
	responsive := make(chan error, 1)
	go func() {
		node.snapshot()
		got := make([]byte, len(data))
		_, err := node.readAt(context.Background(), got, 0)
		if err == nil && !bytes.Equal(got, data) {
			err = errors.New("cached bytes changed during metadata fetch")
		}
		responsive <- err
	}()
	select {
	case err := <-responsive:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(time.Second):
		t.Error("metadata HTTP request blocked cached reads or the file lock")
	}
	close(release)
	group.Wait()
	if b.statCalls.Load() != 1 || b.reads.Load() != 1 {
		t.Fatalf("metadata calls=%d data fetches=%d; want one each", b.statCalls.Load(), b.reads.Load())
	}
}

func TestMetadataRefreshCannotReplaceConcurrentWrite(t *testing.T) {
	f, b := metadataTest(t)
	node := f.makeNode(seedFile(t, b.memoryBackend, "photo.jpg", []byte("original")))
	node.statAt = time.Time{}
	started, release := make(chan struct{}), make(chan struct{})
	b.stat = func(ctx context.Context, entry anyshare.Entry) (anyshare.Entry, error) {
		old, err := b.memoryBackend.Stat(ctx, entry)
		close(started)
		<-release
		return old, err
	}
	done := make(chan error, 1)
	go func() { done <- node.refreshMetadata(context.Background()) }()
	<-started
	node.mu.Lock()
	err := node.ensureCacheLocked(context.Background(), false)
	if err == nil {
		err = node.resizeLocked(9)
	}
	if err == nil {
		_, err = node.cache.WriteAt([]byte("modified!"), 0)
	}
	node.mu.Unlock()
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 9)
	if n, err := node.readAt(context.Background(), got, 0); err != nil || n != 9 || string(got) != "modified!" || !node.dirty {
		t.Fatal("metadata response replaced a concurrent local write", n, err)
	}
}

func TestMetadataTTLAndRevisionRefresh(t *testing.T) {
	f, b := metadataTest(t)
	entry := seedFile(t, b.memoryBackend, "photo.jpg", []byte("old"))
	node := f.makeNode(entry)
	if err := node.refreshMetadata(context.Background()); err != nil || b.statCalls.Load() != 0 {
		t.Fatal("fresh listing metadata was not reused", err)
	}
	if _, err := node.readAt(context.Background(), make([]byte, 3), 0); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	entry.Rev = "new-revision"
	b.entries[entry.ID] = entry
	b.data[entry.ID] = []byte("new")
	b.mu.Unlock()
	node.statAt = time.Time{}
	if err := node.refreshMetadata(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := node.refreshMetadata(context.Background()); err != nil || b.statCalls.Load() != 1 {
		t.Fatal("metadata was refetched within its TTL", err)
	}
	got := make([]byte, 3)
	if _, err := node.readAt(context.Background(), got, 0); err != nil || string(got) != "new" || b.reads.Load() != 2 {
		t.Fatal("refreshed revision reused obsolete cached bytes", err)
	}
}

func TestDirectoryListingSharesFetchAndCancellation(t *testing.T) {
	f, b := metadataTest(t)
	seedFile(t, b.memoryBackend, "photo.jpg", []byte("photo"))
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	b.list = func(ctx context.Context, id string) ([]anyshare.Entry, error) {
		once.Do(func() { close(started) })
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return b.memoryBackend.List(ctx, id)
	}
	var group sync.WaitGroup
	for i := 0; i < 16; i++ {
		group.Go(func() {
			entries, err := f.list(context.Background(), f.root, false)
			if err != nil || len(entries) != 1 {
				t.Error("shared listing failed", err)
			}
		})
	}
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.list(ctx, f.root, false); !errors.Is(err, context.Canceled) {
		t.Error("listing waiter ignored cancellation", err)
	}
	close(release)
	group.Wait()
	if b.listCalls.Load() != 1 {
		t.Fatal("concurrent listing requests were duplicated")
	}
}

func TestDirectoryInvalidationDetachesObsoleteListing(t *testing.T) {
	f, b := metadataTest(t)
	entry := seedFile(t, b.memoryBackend, "new-photo.jpg", []byte("photo"))
	started, release := make(chan struct{}), make(chan struct{})
	b.list = func(_ context.Context, _ string) ([]anyshare.Entry, error) {
		if b.listCalls.Load() == 1 {
			close(started)
			<-release
			return nil, nil // Snapshot from before a namespace mutation.
		}
		return []anyshare.Entry{entry}, nil
	}
	done := make(chan error, 1)
	go func() { _, err := f.list(context.Background(), f.root, false); done <- err }()
	<-started
	f.invalidate()
	entries, err := f.list(context.Background(), f.root, false)
	close(release)
	if err != nil || len(entries) != 1 {
		t.Fatal("new listing joined an obsolete fetch", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	entries, err = f.list(context.Background(), f.root, false)
	if err != nil || len(entries) != 1 || entries[0].ID != entry.ID || b.listCalls.Load() != 2 {
		t.Fatal("obsolete listing repopulated the namespace cache", err)
	}
}
