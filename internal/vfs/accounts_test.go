package vfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/unofficialpku/disk/internal/anyshare"
)

type accountTestBackend struct {
	*readProbe
	quotas   map[string]anyshare.Quota
	writable map[string]bool
	quotaErr error
}

func (b *accountTestBackend) LibraryQuota(_ context.Context, id string) (anyshare.Quota, error) {
	return b.quotas[id], b.quotaErr
}

func (b *accountTestBackend) Writable(_ context.Context, id string) (bool, error) {
	return b.writable[anyshare.LibraryID(id)], nil
}

func (b *accountTestBackend) Can(ctx context.Context, id, permission string) (bool, error) {
	if permission == "read" || permission == "display" {
		return true, nil
	}
	return b.Writable(ctx, id)
}

type accountFixture struct {
	backend *AccountBackend
	users   [2]*accountTestBackend
	files   [2]anyshare.Entry
	roots   []anyshare.Entry
}

func newAccountFixture(t *testing.T) *accountFixture {
	t.Helper()
	fixture := new(accountFixture)
	var accounts []MountAccount
	for i, name := range []string{"alpha", "bravo"} {
		memory := newMemory()
		fixture.users[i] = &accountTestBackend{readProbe: &readProbe{memoryBackend: memory},
			quotas:   map[string]anyshare.Quota{memory.roots[0].ID: {Allocated: 200 << 20, Used: 73 << 20}},
			writable: map[string]bool{memory.roots[0].ID: true}}
		fixture.files[i] = seedFile(t, memory, "same.txt", []byte(name))
		accounts = append(accounts, MountAccount{Name: name, Server: "https://disk.pku.edu.cn", Backend: fixture.users[i]})
	}
	// Both accounts deliberately expose identical raw IDs, revisions and sizes
	// but distinct contents, making accidental cache/inode sharing observable.
	if fixture.files[0].ID != fixture.files[1].ID || fixture.files[0].Rev != fixture.files[1].Rev {
		t.Fatal("fixture does not reproduce colliding remote identities")
	}
	var err error
	fixture.backend, err = NewAccountBackend([]MountAccount{accounts[1], accounts[0]})
	if err != nil {
		t.Fatal(err)
	}
	fixture.roots, err = fixture.backend.Roots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i := range fixture.files {
		fixture.files[i] = scopedEntry(fixture.roots[i].ID, fixture.files[i])
	}
	return fixture
}

func accountCore(t *testing.T, fixture *accountFixture) *FileSystem {
	t.Helper()
	core, err := New(fixture.backend, Options{CacheDir: filepath.Join(t.TempDir(), "cache"), CacheLimit: 1 << 20, IOTimeout: time.Second, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { core.Close() })
	return core
}

func TestAccountNamespaceAndReadCacheIsolation(t *testing.T) {
	fixture := newAccountFixture(t)
	core := accountCore(t, fixture)
	ctx := context.Background()
	roots, err := core.list(ctx, core.root, false)
	if err != nil || len(roots) != 2 || roots[0].Name != "alpha" || roots[1].Name != "bravo" {
		t.Fatal(roots, err)
	}
	fixture.users[0].writable[fixture.users[0].roots[0].ID] = false
	for i, root := range roots {
		node := core.makeNode(root)
		if !node.synthetic || node.refreshMetadata(ctx) != nil {
			t.Fatal("account metadata was treated as a remote file")
		}
		libraries, err := core.list(ctx, node, false)
		if err != nil || len(libraries) != 1 {
			t.Fatal(libraries, err)
		}
		if libraries[0].ID != parentID(fixture.files[i].ID) || libraryID(libraries[0].ID) != libraries[0].ID {
			t.Fatal("namespaced hierarchy lost the library or parent identity")
		}
		files, err := core.list(ctx, core.makeNode(libraries[0]), false)
		if err != nil || len(files) != 1 || files[0].ID != fixture.files[i].ID {
			t.Fatal(files, err)
		}
		allowed, err := fixture.backend.Can(ctx, files[0].ID, "modify")
		if err != nil || allowed != (i == 1) {
			t.Fatal("permissions used another account", allowed, err)
		}
	}
	if libraryID(fixture.files[0].ID) == libraryID(fixture.files[1].ID) {
		t.Fatal("accounts still share a library identity")
	}
	var group sync.WaitGroup
	for i, file := range fixture.files {
		for range 12 {
			group.Go(func() {
				node := core.makeNode(file)
				data := make([]byte, 5)
				if n, err := node.readAt(ctx, data, 0); err != nil || n != 5 || string(data) != roots[i].Name {
					t.Errorf("account %s read %q (%d): %v", roots[i].Name, data, n, err)
				}
			})
		}
	}
	group.Wait()
	for _, user := range fixture.users {
		if user.reads.Load() != 1 {
			t.Fatal("one account's concurrent opens did not share its fetch", user.reads.Load())
		}
	}
}

func TestAccountMutationsAndCrossAccountRejection(t *testing.T) {
	fixture := newAccountFixture(t)
	b := fixture.backend
	ctx := context.Background()
	parent := parentID(fixture.files[0].ID)
	folder, err := b.Mkdir(ctx, parent, "folder")
	if err != nil {
		t.Fatal(err)
	}
	file, err := b.Upload(ctx, folder.ID, "new.txt", nil, bytes.NewReader([]byte("first")), 5, time.Now())
	if err != nil || parentID(file.ID) != folder.ID {
		t.Fatal(file, err)
	}
	file, err = b.Upload(ctx, folder.ID, file.Name, &file, bytes.NewReader([]byte("later")), 5, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	file, err = b.Stat(ctx, file)
	if err != nil || parentID(file.ID) != folder.ID {
		t.Fatal(file, err)
	}
	file, err = b.Move(ctx, file, parent)
	if err != nil || parentID(file.ID) != parent {
		t.Fatal(file, err)
	}
	file, err = b.Rename(ctx, file, "renamed.txt", false)
	if err != nil || file.Name != "renamed.txt" {
		t.Fatal(file, err)
	}
	var data bytes.Buffer
	if err := b.Download(ctx, file, &data); err != nil || data.String() != "later" {
		t.Fatal(data.String(), err)
	}
	got := make([]byte, 5)
	if _, err := b.ReadAt(ctx, file, got, 0); err != nil || string(got) != "later" {
		t.Fatal(string(got), err)
	}
	otherParent := parentID(fixture.files[1].ID)
	if _, err := b.Upload(ctx, parent, "wrong.txt", &fixture.files[1], bytes.NewReader(nil), 0, time.Now()); !errors.Is(err, syscall.EXDEV) {
		t.Fatal("cross-account overwrite was accepted", err)
	}
	if _, err := b.Move(ctx, file, otherParent); !errors.Is(err, syscall.EXDEV) {
		t.Fatal("cross-account move was accepted", err)
	}
	core := accountCore(t, fixture)
	if _, err := core.moveAndRename(ctx, file, parent, otherParent, "different.txt", false); !errors.Is(err, syscall.EXDEV) {
		t.Fatal("cross-account move started its temporary rename", err)
	}
	if actual, err := b.Stat(ctx, file); err != nil || actual.Name != "renamed.txt" {
		t.Fatal("rejected move modified the source", actual, err)
	}
	if fixture.users[1].uploads != 1 {
		t.Fatal("write reached another account")
	}
	if err := b.Delete(ctx, file); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete(ctx, folder); err != nil {
		t.Fatal(err)
	}
	for _, root := range fixture.roots {
		if _, err := b.Mkdir(ctx, root.ID, "bad"); !errors.Is(err, syscall.EPERM) {
			t.Fatal("created inside virtual account directory", err)
		}
		if err := b.Delete(ctx, root); !errors.Is(err, syscall.EPERM) {
			t.Fatal("deleted virtual account directory", err)
		}
		if _, err := b.Rename(ctx, root, "bad", false); !errors.Is(err, syscall.EPERM) {
			t.Fatal("renamed virtual account directory", err)
		}
	}
}

func TestAccountRecoveryRetainsOwnerAndRawRemoteIDs(t *testing.T) {
	fixture := newAccountFixture(t)
	core := accountCore(t, fixture)
	node := core.makeNode(fixture.files[1])
	ctx := context.Background()
	if err := node.ensureCacheLocked(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := node.resizeLocked(9); err != nil {
		t.Fatal(err)
	}
	if _, err := node.cache.WriteAt([]byte("unflushed"), 0); err != nil {
		t.Fatal(err)
	}
	fixture.users[1].failUpload = true
	if err := node.flushLocked(ctx); err == nil {
		t.Fatal("failed upload was acknowledged")
	}
	records, err := RecoveryFiles(core.cacheDir)
	if err != nil || len(records) != 1 {
		t.Fatal(records, err)
	}
	record := records[0]
	_, remote := splitAccountID(fixture.files[1].ID)
	if record.Account != "bravo" || record.Server != "https://disk.pku.edu.cn" || record.Entry.ID != remote || record.Parent != anyshare.ParentID(remote) {
		t.Fatal("recovery lost the owning account or exposed an internal ID", record)
	}
	data, err := os.ReadFile(record.Data)
	if err != nil || string(data) != "unflushed" {
		t.Fatal(string(data), err)
	}
	fixture.users[1].failUpload = false
	if err := node.flushLocked(ctx); err != nil {
		t.Fatal("namespaced recovery could not be retried", err)
	}
	if records, err := RecoveryFiles(core.cacheDir); err != nil || len(records) != 0 {
		t.Fatal("successful retry left a journal", records, err)
	}
}

func TestAccountQuotaDeduplicatesLibrariesAndCombinesWriteAccess(t *testing.T) {
	fixture := newAccountFixture(t)
	shared := fixture.users[0].roots[0].ID
	fixture.users[0].writable[shared] = false
	for i, user := range fixture.users {
		personal := anyshare.Entry{ID: "gns://personal-" + fixture.roots[i].Name, Name: "personal", Dir: true, Size: -1}
		public := anyshare.Entry{ID: "gns://public", Name: "public", Dir: true, Size: -1}
		user.roots = append(user.roots, personal, public)
		user.quotas[personal.ID] = anyshare.Quota{Allocated: 20 << 20, Used: 3 << 20}
		user.writable[personal.ID] = true
		user.quotas[public.ID] = anyshare.Quota{Allocated: 1 << 50, Used: 7 << 20}
	}
	// Different spellings of the same origin cannot double-count shared quota.
	backend, err := NewAccountBackend([]MountAccount{
		{Name: "alpha", Server: "https://DISK.pku.edu.cn:443/", Backend: fixture.users[0]},
		{Name: "bravo", Server: "https://disk.pku.edu.cn", Backend: fixture.users[1]},
	})
	if err != nil {
		t.Fatal(err)
	}
	quota, err := backend.StorageQuota(context.Background(), "", false)
	want := anyshare.Quota{Allocated: 247 << 20, Used: 86 << 20}
	if err != nil || quota != want || quota.Available() != 161<<20 {
		t.Fatal("shared capacity was duplicated or RO allowance included", quota, err)
	}
	quota, err = backend.StorageQuota(context.Background(), "", true)
	if err != nil || quota.Allocated != want.Used || quota.Used != want.Used || quota.Available() != 0 {
		t.Fatal("read-only capacity includes free space", quota, err)
	}
	backend, err = NewAccountBackend([]MountAccount{
		{Name: "alpha", Server: "https://other.example", Backend: fixture.users[0]},
		{Name: "bravo", Server: "https://disk.pku.edu.cn", Backend: fixture.users[1]},
	})
	if err != nil {
		t.Fatal(err)
	}
	quota, err = backend.StorageQuota(context.Background(), "", false)
	if err != nil || quota.Allocated != (247+73+7)<<20 || quota.Used != (86+73+7)<<20 {
		t.Fatal("distinct servers were incorrectly deduplicated", quota, err)
	}
	fixture.users[0].quotaErr = anyshare.ErrPermission
	if _, err := backend.StorageQuota(context.Background(), "", false); !errors.Is(err, anyshare.ErrPermission) || !strings.Contains(err.Error(), "alpha") {
		t.Fatal("quota failure hid the owning account", err)
	}
	fixture.users[0].quotaErr = nil
	fixture.users[0].quotas["gns://personal-alpha"] = anyshare.Quota{Allocated: math.MaxInt64}
	if _, err := backend.StorageQuota(context.Background(), "", false); err == nil {
		t.Fatal("combined quota overflowed")
	}
}

func TestAccountDirectoryNamesAreUsable(t *testing.T) {
	backend := newMemory()
	names := []string{"", ".", "..", "bad/name", "bad\x00name"}
	if runtime.GOOS == "windows" {
		names = append(names, "NUL", "bad*name", "bad.")
	}
	for _, name := range names {
		if _, err := NewAccountBackend([]MountAccount{{Name: name, Server: "https://disk.pku.edu.cn", Backend: backend}}); err == nil {
			t.Fatalf("unusable account directory %q accepted", name)
		}
	}
	if _, err := NewAccountBackend(nil); err == nil {
		t.Fatal("empty account set accepted")
	}
	if runtime.GOOS == "windows" {
		if _, err := NewAccountBackend([]MountAccount{
			{Name: "Work", Server: "https://disk.pku.edu.cn", Backend: backend},
			{Name: "work", Server: "https://disk.pku.edu.cn", Backend: backend},
		}); err == nil {
			t.Fatal("case-insensitive account directory collision accepted")
		}
	}
}

// The same filesystem operations run through a kernel FUSE mount and WinFsp.
func exerciseMountedAccounts(t *testing.T, point string, fixture *accountFixture) {
	t.Helper()
	entries, err := os.ReadDir(point)
	if err != nil || len(entries) != 2 || entries[0].Name() != "alpha" || entries[1].Name() != "bravo" {
		t.Fatal(entries, err)
	}
	var paths [2]string
	for i, account := range fixture.roots {
		paths[i] = filepath.Join(point, account.Name, fixture.users[i].roots[0].Name, "same.txt")
		for range 3 {
			data, err := os.ReadFile(paths[i])
			if err != nil || string(data) != account.Name {
				t.Fatal("mounted accounts returned the same cached contents", string(data), err)
			}
		}
	}
	if err := os.Rename(paths[0], paths[1]); err == nil {
		t.Fatal("cross-account rename succeeded")
	}
	for i, account := range fixture.roots {
		data, err := os.ReadFile(paths[i])
		if err != nil || string(data) != account.Name {
			t.Fatal("rejected rename modified data", string(data), err)
		}
	}
	if err := os.WriteFile(paths[0], []byte("alpha-new"), 0600); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(paths[1]); err != nil || string(data) != "bravo" {
		t.Fatal("write leaked into another account", string(data), err)
	}
	folder := filepath.Join(filepath.Dir(paths[0]), "新目录")
	if err := os.Mkdir(folder, 0700); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(folder, "moved.txt")
	if err := os.Rename(paths[0], moved); err != nil {
		t.Fatal("same-account move failed", err)
	}
	if data, err := os.ReadFile(moved); err != nil || string(data) != "alpha-new" {
		t.Fatal(string(data), err)
	}
	if err := os.Remove(moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(folder); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(point, "alpha", "forbidden"), 0700); err == nil {
		t.Fatal("virtual account directory is writable")
	}
	if err := os.Rename(filepath.Join(point, "alpha"), filepath.Join(point, "renamed")); err == nil {
		t.Fatal("virtual account directory can be renamed")
	}
}
