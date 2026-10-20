package vfs

import (
	"context"
	"errors"
	"io"
	"log"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/unofficialpku/disk/internal/anyshare"
)

type quotaProbe struct {
	*memoryBackend
	calls            atomic.Int32
	started, release chan struct{}
}

func (b *quotaProbe) StorageQuota(ctx context.Context, _ string, _ bool) (anyshare.Quota, error) {
	number := b.calls.Add(1)
	if number == 1 {
		close(b.started)
		select {
		case <-ctx.Done():
			return anyshare.Quota{}, ctx.Err()
		case <-b.release:
		}
	}
	return anyshare.Quota{Allocated: int64(number) * 4096, Used: 0}, nil
}
func TestQuotaQueriesShareRefreshAndRespectInvalidation(t *testing.T) {
	probe := &quotaProbe{memoryBackend: newMemory(), started: make(chan struct{}), release: make(chan struct{})}
	core, err := New(probe, Options{CacheDir: filepath.Join(t.TempDir(), "cache"), Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	old := make(chan anyshare.Quota, 1)
	go func() { quota, _ := core.quotaSnapshot(context.Background()); old <- quota }()
	<-probe.started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := core.quotaSnapshot(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled waiter did not leave shared refresh", err)
	}
	if probe.calls.Load() != 1 {
		t.Fatal("quota waiters started duplicate requests")
	}
	core.invalidate()
	fresh, err := core.quotaSnapshot(context.Background())
	if err != nil || fresh.Allocated != 8192 {
		t.Fatal(fresh, err)
	}
	close(probe.release)
	<-old
	cached, err := core.quotaSnapshot(context.Background())
	if err != nil || cached != fresh || probe.calls.Load() != 2 {
		t.Fatal("older quota request repopulated invalidated cache", cached, err)
	}
}
