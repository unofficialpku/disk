package vfs

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unofficialpku/disk/internal/diagnostic"
)

type mountLog struct {
	mu sync.Mutex
	bytes.Buffer
	waiting chan struct{}
	once    sync.Once
}

func (b *mountLog) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.waiting != nil && bytes.Contains(data, []byte("waiting for file lock")) {
		b.once.Do(func() { close(b.waiting) })
	}
	return b.Buffer.Write(data)
}
func (b *mountLog) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

func TestVerboseReadExplainsFetchHitAndPromotion(t *testing.T) {
	f, b := readCacheTest(t, 16<<20)
	var output mountLog
	f.opts.Trace = diagnostic.New(&output)
	node := f.makeNode(seedFile(t, b.memoryBackend, "photo.jpg", make([]byte, 5<<20)))
	for _, off := range []int64{0, 4 << 20} {
		if _, err := node.readAt(context.Background(), make([]byte, 32), off); err != nil {
			t.Fatal(err)
		}
		f.readWG.Wait() // The next read checks reuse of a retained complete range.
	}
	node.mu.Lock()
	err := node.ensureCacheLocked(context.Background(), false)
	if err == nil {
		err = node.flushLocked(context.Background())
	}
	node.mu.Unlock()
	if err != nil || b.reads.Load() != 1 || b.downloads.Load() != 0 {
		t.Fatal("verbose mode changed cache behavior", err)
	}
	for _, want := range []string{"START read file=\"photo.jpg\"", "read plan: whole-file=true", "cache miss; waiting for read slot", "read slot acquired", "length=5.0 MiB", "remote range finished", "retaining range in disk cache", "disk cache hit", "END read file=", "promoted complete clean read cache", "file is clean; no upload", "elapsed="} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("missing read diagnostic %q", want)
		}
	}
}

func TestVerboseReadReportsBlockedFileLockBeforeNetwork(t *testing.T) {
	f, b := readCacheTest(t, 1024)
	output := mountLog{waiting: make(chan struct{})}
	f.opts.Trace = diagnostic.New(&output)
	node := f.makeNode(seedFile(t, b.memoryBackend, "locked.jpg", []byte("photo")))
	node.mu.Lock()
	done := make(chan error, 1)
	go func() { _, err := node.readAt(context.Background(), make([]byte, 1), 0); done <- err }()
	select {
	case <-output.waiting:
		if strings.Contains(output.String(), "END read") || b.reads.Load() != 0 {
			t.Error("blocked operation was declared complete or started network I/O")
		}
	case <-time.After(time.Second):
		t.Error("blocked operation was not logged before acquiring its lock")
	}
	node.mu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "END read file=\"locked.jpg\"") {
		t.Fatal("operation completion was not logged")
	}
}
