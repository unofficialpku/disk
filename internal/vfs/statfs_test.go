//go:build linux || darwin

package vfs

import (
	"context"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"log"
	"path/filepath"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/unofficialpku/disk/internal/anyshare"
)

func TestStatfsReportsWritableServerQuota(t *testing.T) {
	backend := newMemory()
	backend.quota = anyshare.Quota{Allocated: 200 << 20, Used: 73 << 20}
	core, err := New(backend, Options{Root: &backend.roots[0], CacheDir: filepath.Join(t.TempDir(), "private"), CacheLimit: 1 << 20, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	var stat fuse.StatfsOut
	if code := core.root.Statfs(context.Background(), &stat); code != 0 {
		t.Fatal(code)
	}
	const block = int64(4096)
	if stat.Blocks != quotaBlocks(200<<20, block) || stat.Bavail != uint64((127<<20)/block) || stat.Bfree != stat.Bavail {
		t.Fatalf("statfs=%+v", stat)
	}
	backend.mu.Lock()
	backend.quota.Used = 199 << 20
	backend.mu.Unlock()
	// The five-second cache is intentional; a namespace mutation invalidates it
	// immediately, matching the point at which the mount publishes a write.
	core.invalidate()
	if code := core.root.Statfs(context.Background(), &stat); code != 0 || stat.Bavail != uint64((1<<20)/block) {
		t.Fatalf("refreshed statfs=%+v code=%d", stat, code)
	}
}

func TestKernelStatfsReportsServerCapacity(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		t.Run(fmt.Sprint(readOnly), func(t *testing.T) {
			backend := newMemory()
			backend.quota = anyshare.Quota{Allocated: 200 << 20, Used: 73 << 20}
			point, _ := mountTest(t, backend, Options{Root: &backend.roots[0], ReadOnly: readOnly})
			var stat unix.Statfs_t
			if err := unix.Statfs(point, &stat); err != nil {
				t.Fatal(err)
			}
			wantTotal, wantFree := int64(200<<20), int64(127<<20)
			if readOnly {
				wantTotal, wantFree = 73<<20, 0
			}
			if int64(stat.Blocks)*int64(stat.Bsize) != wantTotal || int64(stat.Bavail)*int64(stat.Bsize) != wantFree {
				t.Fatalf("kernel statfs total=%d available=%d", stat.Blocks*uint64(stat.Bsize), stat.Bavail*uint64(stat.Bsize))
			}
		})
	}
}
