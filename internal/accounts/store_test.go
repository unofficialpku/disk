package accounts

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/unofficialpku/disk/internal/auth"
	"github.com/unofficialpku/disk/internal/localfile"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return &Store{Path: filepath.Join(dir, "nested", "accounts.json")}
}

func testAccount(name string) Account {
	return Account{Name: name, Server: auth.DefaultServer, Username: name + "-user", Password: "test-password"}
}

func TestStorePersistsCredentialsDefaultAndRotatedSession(t *testing.T) {
	s := testStore(t)
	account := testAccount("primary")
	if err := s.Put(account, true, false); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateState(account.Name, account.Server, account.Username, auth.State{AccessToken: "access-1", RefreshToken: "refresh-1"}); err != nil {
		t.Fatal(err)
	}
	c, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := c.Find(account.Name)
	if !ok || c.Default != account.Name || got.Password != account.Password || got.Session.RefreshToken != "refresh-1" {
		t.Fatalf("loaded config = %#v, account = %#v", c, got)
	}
	info, err := os.Stat(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("account store mode = %o, want 600", info.Mode().Perm())
	}
	file, err := localfile.OpenPrivate(s.Path, os.O_RDONLY)
	if err != nil {
		t.Fatal("saved account permissions:", err)
	}
	file.Close()
}

func TestStoreRejectsDuplicateAndRemovesDefault(t *testing.T) {
	s := testStore(t)
	if err := s.Put(testAccount("one"), true, false); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(testAccount("one"), false, false); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate error = %v", err)
	}
	if err := s.Put(testAccount("two"), false, false); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("one"); err != nil {
		t.Fatal(err)
	}
	c, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Default != "two" || len(c.Accounts) != 1 {
		t.Fatalf("after remove = %#v", c)
	}
}

func TestStoreRejectsUnsafeDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode check; Windows ACL rejection is covered in localfile")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	s := &Store{Path: filepath.Join(dir, "accounts.json")}
	if err := s.Put(testAccount("unsafe"), true, false); err == nil {
		t.Fatal("world-readable account directory was accepted")
	}
}

func TestSessionLockHonorsContext(t *testing.T) {
	s := testStore(t)
	first, err := s.LockSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if second, err := s.LockSession(ctx); err == nil {
		second.Close()
		t.Fatal("session lock was reentrant")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("session lock error = %v", err)
	}
}
