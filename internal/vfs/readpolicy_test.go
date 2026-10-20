package vfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/unofficialpku/disk/internal/anyshare"
)

func generatedRead(_ context.Context, _ anyshare.Entry, data []byte, off int64) (int, error) {
	for i := range data {
		data[i] = byte((off + int64(i)) % 251)
	}
	return len(data), nil
}

func checkGeneratedRead(t *testing.T, node *Node, data []byte, off int64) {
	t.Helper()
	count, err := node.readAt(context.Background(), data, off)
	if err != nil || count != len(data) {
		t.Fatal("range read failed", count, err)
	}
	for i, value := range data {
		if value != byte((off+int64(i))%251) {
			t.Fatalf("wrong byte at offset %d", off+int64(i))
		}
	}
}

func TestReadPolicyWholePhotoWithOneFetch(t *testing.T) {
	f, b := readCacheTest(t, 16<<20)
	data := bytes.Repeat([]byte("photo"), 1<<20)
	entry := seedFile(t, b.memoryBackend, "photo.jpg", data)
	var fetched int
	b.read = func(ctx context.Context, entry anyshare.Entry, data []byte, off int64) (int, error) {
		if off != 0 {
			t.Error("small file was fetched as a tail range")
		}
		fetched = len(data)
		return b.memoryBackend.ReadAt(ctx, entry, data, off)
	}
	for _, off := range []int64{entry.Size - 32, 0, 4 << 20, 511} {
		got := make([]byte, 32)
		if count, err := f.makeNode(entry).readAt(context.Background(), got, off); err != nil || count != 32 || !bytes.Equal(got, data[off:off+32]) {
			t.Fatal("photo header/tail/reopen read failed", count, err)
		}
	}
	if b.reads.Load() != 1 || fetched != 5<<20 || f.cacheBytes != 5<<20 {
		t.Fatalf("photo used %d fetches of %d bytes, cached %d", b.reads.Load(), fetched, f.cacheBytes)
	}
}

func TestReadPolicySequentialGrowthAndSeekReset(t *testing.T) {
	policy := ReadOptions{BlockSize: 1024, ReadAhead: 4096, Concurrency: 2}
	f, b := readCacheTest(t, 1<<20, policy)
	var sizes []int
	b.read = func(ctx context.Context, e anyshare.Entry, data []byte, off int64) (int, error) {
		sizes = append(sizes, len(data))
		return generatedRead(ctx, e, data, off)
	}
	node := f.makeNode(anyshare.Entry{ID: "gns://library/sequential", Rev: "1", Size: 64 << 10})
	for off := int64(0); off < node.entry.Size; off += 512 {
		checkGeneratedRead(t, node, make([]byte, 512), off)
	}
	if b.reads.Load() != 17 || sizes[0] != 1024 || sizes[1] != 4096 {
		t.Fatalf("sequential fetches=%d, sizes=%v; want 17 instead of 64 fixed-block fetches", b.reads.Load(), sizes)
	}
	// Large seeks get one base block; the next sequential reads can grow again.
	random := f.makeNode(anyshare.Entry{ID: "gns://library/random", Rev: "1", Size: 20 << 30})
	for _, off := range []int64{0, 32 << 20, 16 << 20} {
		checkGeneratedRead(t, random, make([]byte, 512), off)
		if sizes[len(sizes)-1] != 1024 {
			t.Fatal("a seek retained the sequential fetch size")
		}
	}
	checkGeneratedRead(t, random, make([]byte, 512), (16<<20)+512)
	checkGeneratedRead(t, random, make([]byte, 512), (16<<20)+1024)
	if sizes[len(sizes)-1] != 4096 {
		t.Fatal("sequential fetches did not grow after a seek")
	}

	policy.ReadAhead = 0
	fixed, probe := readCacheTest(t, 1<<20, policy)
	probe.read = generatedRead
	for off := int64(0); off < node.entry.Size; off += 512 {
		checkGeneratedRead(t, fixed.makeNode(node.entry), make([]byte, 512), off)
	}
	if probe.reads.Load() != 64 {
		t.Fatal("disabling read-ahead did not preserve fixed-block reads")
	}
}

func TestReadPolicyBatchesDemandAndOverlappingReaders(t *testing.T) {
	policy := ReadOptions{BlockSize: 1024, ReadAhead: 4096, Concurrency: 1}
	f, b := readCacheTest(t, 4096, policy)
	node := f.makeNode(anyshare.Entry{ID: "gns://library/batch", Rev: "1", Size: 64 << 10})
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	b.read = func(ctx context.Context, entry anyshare.Entry, data []byte, off int64) (int, error) {
		once.Do(func() { close(started) })
		select {
		case <-release:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
		return generatedRead(ctx, entry, data, off)
	}
	var group sync.WaitGroup
	group.Go(func() { checkGeneratedRead(t, node, make([]byte, 2048), 512) })
	<-started
	group.Go(func() { checkGeneratedRead(t, node, make([]byte, 32), 2048) })
	close(release)
	group.Wait()
	f.readWG.Wait() // Prefix reads finish before the complete batch is retained.
	if b.reads.Load() != 1 || f.cacheBytes != 3072 {
		t.Fatalf("overlapping demand used %d fetches, cached %d bytes", b.reads.Load(), f.cacheBytes)
	}
	files, err := os.ReadDir(f.CachePath())
	if err != nil || len(files) != 1 {
		t.Fatal("batch did not occupy exactly one disk file", err)
	}
	// An impossible write allocation must leave useful clean reads intact.
	writer := f.makeNode(seedFile(t, b.memoryBackend, "too-large", make([]byte, 8192)))
	writer.mu.Lock()
	err = writer.ensureCacheLocked(context.Background(), false)
	writer.mu.Unlock()
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatal("oversized write allocation succeeded", err)
	}
	checkGeneratedRead(t, node, make([]byte, 32), 2048)
	if b.reads.Load() != 1 {
		t.Fatal("impossible write allocation evicted a useful read batch")
	}
	// Force eviction of the batched file, then read one of its indexed blocks.
	checkGeneratedRead(t, node, make([]byte, 32), 32<<10)
	checkGeneratedRead(t, node, make([]byte, 32), 48<<10)
	checkGeneratedRead(t, node, make([]byte, 32), 2048)
	if b.reads.Load() != 4 {
		t.Fatal("evicted batch left a stale block index")
	}
	if err := f.Close(); err != nil || f.cacheBytes != 0 || f.readBytes != 0 || len(f.readFiles) != 0 {
		t.Fatal("batched cache leaked bytes or block indexes", err)
	}
}

func TestReadPolicyCompleteBatchPromotesToWrite(t *testing.T) {
	policy := ReadOptions{BlockSize: 1024, ReadAhead: 4096, Concurrency: 1}
	f, b := readCacheTest(t, 8192, policy)
	data := bytes.Repeat([]byte("ab"), 2048)
	node := f.makeNode(seedFile(t, b.memoryBackend, "complete", data))
	if _, err := node.readAt(context.Background(), make([]byte, len(data)), 0); err != nil {
		t.Fatal(err)
	}
	node.mu.Lock()
	err := node.ensureCacheLocked(context.Background(), false)
	node.mu.Unlock()
	if err != nil || b.downloads.Load() != 0 || f.cacheBytes != 4096 || f.readBytes != 0 || len(f.readFiles) != 0 {
		t.Fatal("whole-batch promotion downloaded or retained stale indexes/charges", err)
	}
}

func TestReadPolicyBoundsConcurrencyWithoutBlockingWarmReads(t *testing.T) {
	policy := ReadOptions{BlockSize: 1024, Concurrency: 2}
	f, b := readCacheTest(t, 64<<10, policy)
	warm := f.makeNode(seedFile(t, b.memoryBackend, "warm", []byte("warm")))
	if _, err := warm.readAt(context.Background(), make([]byte, 4), 0); err != nil {
		t.Fatal(err)
	}
	b.reads.Store(0)
	var active, peak atomic.Int64
	started, release := make(chan struct{}, 6), make(chan struct{})
	b.read = func(ctx context.Context, entry anyshare.Entry, data []byte, off int64) (int, error) {
		now := active.Add(1)
		for old := peak.Load(); now > old && !peak.CompareAndSwap(old, now); old = peak.Load() {
		}
		defer active.Add(-1)
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
		return generatedRead(ctx, entry, data, off)
	}
	var group sync.WaitGroup
	for i := 0; i < 6; i++ {
		node := f.makeNode(anyshare.Entry{ID: fmt.Sprintf("gns://library/cold%d", i), Rev: "1", Size: 64 << 10})
		group.Go(func() { checkGeneratedRead(t, node, make([]byte, 32), 0) })
	}
	<-started
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	queued := f.makeNode(anyshare.Entry{ID: "gns://library/cancel", Rev: "1", Size: 64 << 10})
	done := make(chan error, 1)
	go func() { _, err := queued.readAt(ctx, make([]byte, 32), 0); done <- err }()
	// Cancel after this request actually joins the saturated admission queue.
	ticker := time.NewTicker(time.Millisecond)
	deadline := time.After(time.Second)
waitQueued:
	for {
		f.budgetMu.Lock()
		waiting := f.readFlights[fileKey(queued.entry, 0)] != nil
		f.budgetMu.Unlock()
		if waiting {
			break
		}
		select {
		case <-ticker.C:
		case <-deadline:
			t.Error("read never entered the admission queue")
			break waitQueued
		}
	}
	ticker.Stop()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Error("queued read ignored cancellation", err)
		}
	case <-time.After(time.Second):
		t.Error("queued read did not cancel promptly")
	}
	responsive := make(chan error, 1)
	go func() {
		warm.snapshot()
		_, err := warm.readAt(context.Background(), make([]byte, 4), 0)
		responsive <- err
	}()
	select {
	case err := <-responsive:
		if err != nil {
			t.Error("warm read failed", err)
		}
	case <-time.After(time.Second):
		t.Error("network admission blocked a warm read or metadata")
	}
	if b.reads.Load() != 2 {
		t.Error("too many remote fetches admitted before a slot was released")
	}
	close(release)
	group.Wait()
	if peak.Load() != 2 || b.reads.Load() != 6 {
		t.Fatalf("peak concurrency=%d, fetches=%d; want 2 and 6", peak.Load(), b.reads.Load())
	}
}
