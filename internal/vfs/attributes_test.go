package vfs

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/unofficialpku/disk/internal/anyshare"
)

func TestSharedErrorCodeNormalizesWrappedFailures(t *testing.T) {
	notDirectory := syscall.ENOTDIR
	if runtime.GOOS == "windows" {
		// Go aliases ENOTDIR to Win32 ERROR_PATH_NOT_FOUND. Like other native
		// missing-path errors, it is normalized through os.ErrNotExist.
		notDirectory = syscall.ENOENT
	}
	for _, test := range []struct {
		err  error
		want syscall.Errno
	}{
		{anyshare.ErrNotFound, syscall.ENOENT}, {os.ErrNotExist, syscall.ENOENT},
		{anyshare.ErrExists, syscall.EEXIST}, {os.ErrExist, syscall.EEXIST},
		{anyshare.ErrPermission, syscall.EACCES}, {os.ErrPermission, syscall.EACCES},
		{anyshare.ErrPending, syscall.EACCES}, {anyshare.ErrConflict, syscall.EBUSY},
		{anyshare.ErrQuota, syscall.ENOSPC}, {anyshare.ErrSizeLimit, syscall.EFBIG},
		{context.Canceled, syscall.EINTR}, {context.DeadlineExceeded, syscall.ETIMEDOUT},
		{syscall.EXDEV, syscall.EXDEV}, {syscall.ENOTDIR, notDirectory},
		{errors.New("unexpected remote response"), syscall.EIO},
	} {
		if got := errorCode(fmt.Errorf("operation: %w", test.err)); got != test.want {
			t.Errorf("%v mapped to %v; want %v", test.err, got, test.want)
		}
	}
	if errorCode(nil) != 0 {
		t.Fatal("successful operation returned an error")
	}
}

func TestSharedAttributesNormalizeRemoteMetadata(t *testing.T) {
	modified := time.Unix(100, 0)
	entry := anyshare.Entry{ID: "gns://library/file", Size: 513, Modified: modified}
	a := entryAttributes(entry, 0640)
	if a.Inode < 2 || a.Mode != syscall.S_IFREG|0640 || a.Links != 1 || a.Size != 513 || a.Blocks != 2 || a.Modified != modified || a.Created != modified {
		t.Fatalf("invalid file attributes: %+v", a)
	}
	entry.Size = math.MaxInt64
	if got := entryAttributes(entry, 0600).Blocks; got != math.MaxInt64/512+1 {
		t.Fatalf("block count overflowed: %d", got)
	}
	entry.Dir, entry.Size, entry.Modified = true, -1, time.Time{}
	dir := entryAttributes(entry, 0500)
	if dir.Inode != a.Inode || dir.Mode != syscall.S_IFDIR|0500 || dir.Links != 2 || dir.Size != 0 || dir.Blocks != 0 || !dir.Modified.Equal(time.Unix(0, 0)) || dir.Created != dir.Modified {
		t.Fatalf("invalid directory attributes: %+v", dir)
	}
	node := &Node{f: &FileSystem{opts: Options{ReadOnly: true}}, mode: 0760}
	if node.effectiveModeLocked() != 0540 {
		t.Fatal("read-only mount retained writes or changed read/execute bits")
	}
	node.synthetic = true
	if node.effectiveModeLocked() != 0500 {
		t.Fatal("virtual root is writable")
	}
}
