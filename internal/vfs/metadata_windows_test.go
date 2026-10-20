package vfs

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/winfsp/cgofuse/fuse"
	"golang.org/x/sys/windows"
)

func photoNamespace(t *testing.T) (*WinFsp, *metadataProbe, []string) {
	t.Helper()
	f, b := metadataTest(t)
	f.root = f.makeNode(anyshare.Entry{Name: "PKUDisk", Dir: true, Size: -1})
	f.root.synthetic = true
	dirs := []string{"/", "/" + b.roots[0].Name}
	parent := b.roots[0]
	for _, name := range []string{"archive", "photos"} {
		entry, err := b.Mkdir(context.Background(), parent.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, dirs[len(dirs)-1]+"/"+name)
		parent = entry
	}
	if _, err := b.Upload(context.Background(), parent.ID, "photo.jpg", nil, bytes.NewReader([]byte("photo")), 5, time.Now()); err != nil {
		t.Fatal(err)
	}
	return NewWinFsp(context.Background(), f), b, dirs
}

func listWindowsDirectory(t *testing.T, adapter *WinFsp, value string) []string {
	t.Helper()
	code, handle := adapter.Opendir(value)
	if code != 0 {
		t.Fatalf("open %s returned %d", value, code)
	}
	defer adapter.Release(value, handle)
	var names []string
	if code := adapter.Readdir(value, func(name string, _ *fuse.Stat_t, _ int64) bool {
		names = append(names, name)
		return true
	}, 0, handle); code != 0 {
		t.Fatalf("read %s returned %d", value, code)
	}
	return names
}

func TestWinFspPhotoDirectoryProbesDoNotFetchListings(t *testing.T) {
	adapter, b, dirs := photoNamespace(t)
	for _, dir := range dirs {
		listWindowsDirectory(t, adapter, dir)
	}
	// Match the capture: many opens/closes, but only actual enumeration needs
	// contents. Expired listings must not put these probes back on the network.
	adapter.core.invalidate()
	b.rootCalls.Store(0)
	b.listCalls.Store(0)
	b.statCalls.Store(0)
	b.list = func(context.Context, string) ([]anyshare.Entry, error) {
		return nil, fmt.Errorf("unexpected listing during directory probe")
	}
	for i := 0; i < 6648; i++ {
		value := dirs[i%len(dirs)]
		var stat fuse.Stat_t
		if code := adapter.Getattr(value, &stat, ^uint64(0)); code != 0 {
			t.Fatalf("directory metadata probe returned %d", code)
		}
		code, handle := adapter.Opendir(value)
		if code != 0 || adapter.Release(value, handle) != 0 {
			t.Fatal("directory open/close probe failed", code)
		}
	}
	photo := dirs[len(dirs)-1] + "/photo.jpg"
	code, handle := adapter.Open(photo, fuse.O_RDONLY)
	if code != 0 {
		t.Fatal("known photo path walked expired ancestors", code)
	}
	defer adapter.Release(photo, handle)
	got := make([]byte, 5)
	if n := adapter.Read(photo, got, 0, handle); n != 5 || string(got) != "photo" {
		t.Fatal("photo read failed after directory probes", n)
	}
	if b.rootCalls.Load() != 0 || b.listCalls.Load() != 0 || b.statCalls.Load() != 0 {
		t.Fatalf("6648 known-directory probes requested roots=%d lists=%d stats=%d", b.rootCalls.Load(), b.listCalls.Load(), b.statCalls.Load())
	}
}

func TestWinFspDirectoryListingIsLazyAndUsesOneHandleSnapshot(t *testing.T) {
	adapter, b, dirs := photoNamespace(t)
	for _, dir := range dirs {
		listWindowsDirectory(t, adapter, dir)
	}
	adapter.core.invalidate()
	value := dirs[len(dirs)-1]
	code, handle := adapter.Opendir(value)
	if code != 0 {
		t.Fatal(code)
	}
	defer adapter.Release(value, handle)
	before := b.listCalls.Load()
	names := make([]string, 0)
	fill := func(name string, _ *fuse.Stat_t, _ int64) bool { names = append(names, name); return true }
	if code := adapter.Readdir(value, fill, 0, handle); code != 0 {
		t.Fatal(code)
	}
	if b.listCalls.Load() != before+1 || len(names) != 3 || names[2] != "photo.jpg" {
		t.Fatal("first enumeration did not load contents exactly once")
	}
	adapter.core.invalidate()
	names = nil
	if code := adapter.Readdir(value, fill, 0, handle); code != 0 || b.listCalls.Load() != before+1 || len(names) != 3 {
		t.Fatal("directory handle lost its enumeration snapshot", code)
	}
}

func TestWinFspMountedDirectoryAttributeProbes(t *testing.T) {
	adapter, b, dirs := photoNamespace(t)
	root := mountWinFspTest(t, adapter)
	for _, dir := range dirs {
		if _, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(dir))); err != nil {
			t.Fatal("native directory enumeration failed", err)
		}
	}
	adapter.core.invalidate()
	b.rootCalls.Store(0)
	b.listCalls.Store(0)
	for i := 0; i < 100; i++ {
		name := filepath.Join(root, filepath.FromSlash(dirs[i%len(dirs)]))
		wide, err := windows.UTF16PtrFromString(name)
		if err != nil {
			t.Fatal(err)
		}
		handle, err := windows.CreateFile(wide, windows.FILE_READ_ATTRIBUTES,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
		if err != nil {
			t.Fatal("native directory attribute open failed", err)
		}
		var info windows.ByHandleFileInformation
		err = windows.GetFileInformationByHandle(handle, &info)
		windows.CloseHandle(handle)
		if err != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
			t.Fatal("native directory attribute query failed", err)
		}
	}
	if b.rootCalls.Load() != 0 || b.listCalls.Load() != 0 {
		t.Fatalf("attribute-only opens fetched roots=%d listings=%d", b.rootCalls.Load(), b.listCalls.Load())
	}
}

func TestWinFspOldDirectorySnapshotCannotReviveRemovedPath(t *testing.T) {
	adapter, _, dirs := photoNamespace(t)
	for _, dir := range dirs {
		listWindowsDirectory(t, adapter, dir)
	}
	value := dirs[len(dirs)-1]
	code, handle := adapter.Opendir(value)
	if code != 0 {
		t.Fatal(code)
	}
	defer adapter.Release(value, handle)
	fill := func(string, *fuse.Stat_t, int64) bool { return true }
	if code := adapter.Readdir(value, fill, 0, handle); code != 0 {
		t.Fatal(code)
	}
	photo := value + "/photo.jpg"
	if code := adapter.Unlink(photo); code != 0 {
		t.Fatal(code)
	}
	if code := adapter.Readdir(value, fill, 0, handle); code != 0 {
		t.Fatal(code)
	}
	if adapter.knownPath(photo) != nil {
		t.Fatal("old enumeration revived a deleted path in the namespace index")
	}
	if code, _ := adapter.Open(photo, fuse.O_RDONLY); code != -fuse.ENOENT {
		t.Fatal("old snapshot reopened a deleted file", code)
	}
}

func TestWinFspExternalRenameAndReplacementInvalidateKnownPath(t *testing.T) {
	adapter, b, dirs := photoNamespace(t)
	for _, dir := range dirs {
		listWindowsDirectory(t, adapter, dir)
	}
	oldPath := dirs[len(dirs)-1] + "/photo.jpg"
	node := adapter.knownPath(oldPath)
	entry := node.snapshot()
	if _, err := b.Rename(context.Background(), entry, "renamed.jpg", false); err != nil {
		t.Fatal(err)
	}
	node.statAt = time.Time{}
	var stat fuse.Stat_t
	if code := adapter.Getattr(oldPath, &stat, ^uint64(0)); code != -fuse.ENOENT {
		t.Fatal("external rename left the old known path usable", code)
	}
	if code, _ := adapter.Open(oldPath, fuse.O_RDONLY); code != -fuse.ENOENT {
		t.Fatal("obsolete parent listing restored a renamed path", code)
	}
	newPath := dirs[len(dirs)-1] + "/renamed.jpg"
	code, handle := adapter.Open(newPath, fuse.O_RDONLY)
	if code != 0 {
		t.Fatal("new external name could not be resolved", code)
	}
	adapter.Release(newPath, handle)
	node = adapter.knownPath(newPath)
	entry = node.snapshot()
	if err := b.Delete(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Upload(context.Background(), anyshare.ParentID(entry.ID), entry.Name, nil, bytes.NewReader([]byte("newer")), 5, time.Now()); err != nil {
		t.Fatal(err)
	}
	node.statAt = time.Time{}
	if code := adapter.Getattr(newPath, &stat, ^uint64(0)); code != -fuse.ENOENT {
		t.Fatal("deleted entry was not detected", code)
	}
	code, handle = adapter.Open(newPath, fuse.O_RDONLY)
	if code != 0 {
		t.Fatal("replacement at the same path kept the obsolete document ID", code)
	}
	defer adapter.Release(newPath, handle)
	got := make([]byte, 5)
	if n := adapter.Read(newPath, got, 0, handle); n != 5 || string(got) != "newer" {
		t.Fatal("replacement served obsolete bytes", n)
	}
}
