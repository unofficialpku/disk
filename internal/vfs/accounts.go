package vfs

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/unofficialpku/disk/internal/localfile"
)

// MountAccount owns one remote namespace. IDs include both its saved name and
// server so identical remote IDs never share inodes, cached data or permissions.
type MountAccount struct {
	Name, Server string
	Backend      Backend
}

type AccountBackend struct {
	selected string
	accounts map[string]MountAccount
	roots    []anyshare.Entry
}

func NewAccountBackend(accounts []MountAccount) (*AccountBackend, error) {
	if len(accounts) == 0 {
		return nil, errors.New("no saved accounts; run 'pkudisk account add' first")
	}
	b := &AccountBackend{accounts: make(map[string]MountAccount)}
	names := make(map[string]bool)
	for _, account := range accounts {
		name := account.Name
		if !anyshare.ValidName(name) || !localfile.ValidName(name) {
			return nil, fmt.Errorf("account name %q cannot be used as a mount directory; save it under a filesystem-safe name with 'account add NAME'", name)
		}
		if runtime.GOOS == "windows" {
			name = strings.ToLower(name)
		}
		if names[name] {
			return nil, fmt.Errorf("account name %q collides with another mount directory", account.Name)
		}
		if account.Backend == nil || account.Server == "" {
			return nil, errors.New("missing account backend or server")
		}
		names[name] = true
		account.Server = canonicalServer(account.Server)
		id := "account://" + base64.RawURLEncoding.EncodeToString([]byte(account.Name+"\x00"+account.Server)) + "/"
		b.accounts[id] = account
		b.roots = append(b.roots, anyshare.Entry{ID: id, Name: account.Name, Dir: true, Size: -1})
	}
	sort.Slice(b.roots, func(i, j int) bool { return b.roots[i].Name < b.roots[j].Name })
	return b, nil
}

func canonicalServer(server string) string {
	u, err := url.Parse(server)
	if err != nil {
		return strings.TrimRight(server, "/")
	}
	u.Host = strings.ToLower(u.Host)
	if u.Scheme == "https" && u.Port() == "443" || u.Scheme == "http" && u.Port() == "80" {
		u.Host = u.Hostname()
		if strings.Contains(u.Host, ":") {
			u.Host = "[" + u.Host + "]"
		}
	}
	return strings.TrimRight(u.String(), "/")
}

func splitAccountID(id string) (prefix, remote string) {
	if tail, ok := strings.CutPrefix(id, "account://"); ok {
		if account, remote, ok := strings.Cut(tail, "/"); ok {
			return "account://" + account + "/", remote
		}
	}
	return "", id
}

func libraryID(id string) string {
	prefix, remote := splitAccountID(id)
	if library := anyshare.LibraryID(remote); library != "" {
		return prefix + library
	}
	return ""
}

func parentID(id string) string {
	prefix, remote := splitAccountID(id)
	if remote == "" {
		return ""
	}
	parent := anyshare.ParentID(remote)
	if parent == "" {
		return ""
	}
	return prefix + parent
}

func (b *AccountBackend) lookup(id string) (MountAccount, string, error) {
	prefix, remote := splitAccountID(id)
	account, ok := b.accounts[prefix]
	if !ok {
		return MountAccount{}, "", anyshare.ErrNotFound
	}
	return account, remote, nil
}

// Account resolves the owning session and original server ID for a scoped
// entry. Browsers can use that backend's transfer/share APIs directly.
func (b *AccountBackend) Account(id string) (MountAccount, string, error) {
	return b.lookup(id)
}

// Scope restores an account identity on metadata returned by a direct API.
func (b *AccountBackend) Scope(id string, entry anyshare.Entry) anyshare.Entry {
	return scopedEntry(id, entry)
}

func (b *AccountBackend) remote(id string) (MountAccount, string, error) {
	account, remote, err := b.lookup(id)
	if err == nil && remote == "" {
		err = syscall.EPERM
	}
	return account, remote, err
}

func scopedEntry(id string, entry anyshare.Entry) anyshare.Entry {
	if entry.ID != "" {
		prefix, _ := splitAccountID(id)
		entry.ID = prefix + entry.ID
	}
	return entry
}

func (b *AccountBackend) VirtualDirectory(id string) bool {
	_, remote, err := b.lookup(id)
	return err == nil && remote == ""
}

func (b *AccountBackend) Roots(context.Context) ([]anyshare.Entry, error) {
	return append([]anyshare.Entry(nil), b.roots...), nil
}

func (b *AccountBackend) List(ctx context.Context, id string) ([]anyshare.Entry, error) {
	account, remote, err := b.lookup(id)
	if err != nil {
		return nil, err
	}
	var entries []anyshare.Entry
	if remote == "" {
		entries, err = account.Backend.Roots(ctx)
	} else {
		entries, err = account.Backend.List(ctx, remote)
	}
	if err != nil {
		return nil, fmt.Errorf("account %q: %w", account.Name, err)
	}
	// Backends may retain their returned slice; do not modify its IDs in place.
	result := make([]anyshare.Entry, len(entries))
	for i, entry := range entries {
		result[i] = scopedEntry(id, entry)
	}
	return result, nil
}

func (b *AccountBackend) Stat(ctx context.Context, entry anyshare.Entry) (anyshare.Entry, error) {
	account, remote, err := b.lookup(entry.ID)
	if err != nil {
		return anyshare.Entry{}, err
	}
	if remote == "" {
		return anyshare.Entry{ID: entry.ID, Name: account.Name, Dir: true, Size: -1}, nil
	}
	id := entry.ID
	entry.ID = remote
	entry, err = account.Backend.Stat(ctx, entry)
	return scopedEntry(id, entry), err
}

func (b *AccountBackend) Can(ctx context.Context, id, permission string) (bool, error) {
	account, remote, err := b.lookup(id)
	if err != nil {
		return false, err
	}
	if remote == "" {
		return permission == "read" || permission == "display", nil
	}
	return account.Backend.Can(ctx, remote, permission)
}

func (b *AccountBackend) ReadAt(ctx context.Context, entry anyshare.Entry, data []byte, offset int64) (int, error) {
	account, remote, err := b.remote(entry.ID)
	if err != nil {
		return 0, err
	}
	entry.ID = remote
	return account.Backend.ReadAt(ctx, entry, data, offset)
}

func (b *AccountBackend) ReadRange(ctx context.Context, entry anyshare.Entry, offset, length int64, dest io.Writer) error {
	account, remote, err := b.remote(entry.ID)
	if err != nil {
		return err
	}
	entry.ID = remote
	return account.Backend.ReadRange(ctx, entry, offset, length, dest)
}

func (b *AccountBackend) Download(ctx context.Context, entry anyshare.Entry, dest io.Writer) error {
	account, remote, err := b.remote(entry.ID)
	if err != nil {
		return err
	}
	id := entry.ID
	entry.ID = remote
	return b.operationError(id, account.Backend.Download(ctx, entry, dest))
}

func (b *AccountBackend) Upload(ctx context.Context, parent, name string, existing *anyshare.Entry, data io.ReaderAt, size int64, mtime time.Time) (anyshare.Entry, error) {
	account, remote, err := b.remote(parent)
	if err != nil {
		return anyshare.Entry{}, err
	}
	if existing != nil {
		prefix, id := splitAccountID(existing.ID)
		parentPrefix, _ := splitAccountID(parent)
		if prefix != parentPrefix {
			return anyshare.Entry{}, syscall.EXDEV
		}
		copy := *existing
		copy.ID = id
		existing = &copy
	}
	entry, err := account.Backend.Upload(ctx, remote, name, existing, data, size, mtime)
	return scopedEntry(parent, entry), err
}

func (b *AccountBackend) Mkdir(ctx context.Context, parent, name string) (anyshare.Entry, error) {
	account, remote, err := b.remote(parent)
	if err != nil {
		return anyshare.Entry{}, err
	}
	entry, err := account.Backend.Mkdir(ctx, remote, name)
	return scopedEntry(parent, entry), err
}

func (b *AccountBackend) Delete(ctx context.Context, entry anyshare.Entry) error {
	account, remote, err := b.remote(entry.ID)
	if err != nil {
		return err
	}
	entry.ID = remote
	if err := anyshare.ValidateDelete(entry); err != nil {
		return err
	}
	return account.Backend.Delete(ctx, entry)
}

func (b *AccountBackend) Rename(ctx context.Context, entry anyshare.Entry, name string, replace bool) (anyshare.Entry, error) {
	account, remote, err := b.remote(entry.ID)
	if err != nil {
		return anyshare.Entry{}, err
	}
	id := entry.ID
	entry.ID = remote
	entry, err = account.Backend.Rename(ctx, entry, name, replace)
	return scopedEntry(id, entry), err
}

func (b *AccountBackend) Move(ctx context.Context, entry anyshare.Entry, parent string) (anyshare.Entry, error) {
	account, remote, err := b.remote(entry.ID)
	if err != nil {
		return anyshare.Entry{}, err
	}
	if libraryID(entry.ID) != libraryID(parent) {
		return anyshare.Entry{}, syscall.EXDEV
	}
	_, dest := splitAccountID(parent)
	entry.ID = remote
	entry, err = account.Backend.Move(ctx, entry, dest)
	return scopedEntry(parent, entry), err
}

type libraryQuotaBackend interface {
	LibraryQuota(context.Context, string) (anyshare.Quota, error)
	Writable(context.Context, string) (bool, error)
}

// Shared libraries consume physical capacity once. A library is writable if
// any mounted account can write it, regardless of account iteration order.
func (b *AccountBackend) StorageQuota(ctx context.Context, root string, readOnly bool) (anyshare.Quota, error) {
	if root != "" {
		account, remote, err := b.lookup(root)
		if err != nil {
			return anyshare.Quota{}, err
		}
		provider, ok := account.Backend.(quotaBackend)
		if !ok {
			return anyshare.Quota{}, errors.New("account backend does not expose server quota")
		}
		return provider.StorageQuota(ctx, remote, readOnly)
	}
	type libraryKey struct{ server, id string }
	type capacity struct {
		quota    anyshare.Quota
		writable bool
	}
	libraries := make(map[libraryKey]capacity)
	for _, root := range b.roots {
		account := b.accounts[root.ID]
		provider, ok := account.Backend.(libraryQuotaBackend)
		if !ok {
			return anyshare.Quota{}, fmt.Errorf("account %q does not expose library quota", account.Name)
		}
		roots, err := account.Backend.Roots(ctx)
		if err != nil {
			return anyshare.Quota{}, fmt.Errorf("account %q: %w", account.Name, err)
		}
		for _, library := range roots {
			key := libraryKey{account.Server, library.ID}
			current, exists := libraries[key]
			if !exists {
				current.quota, err = provider.LibraryQuota(ctx, library.ID)
			}
			if err == nil && !readOnly && !current.writable {
				current.writable, err = provider.Writable(ctx, library.ID)
			}
			if err != nil {
				return anyshare.Quota{}, fmt.Errorf("account %q, library %q: %w", account.Name, library.Name, err)
			}
			libraries[key] = current
		}
	}
	var total anyshare.Quota
	for _, library := range libraries {
		quota := library.quota
		if !library.writable {
			quota.Allocated = quota.Used
		} else {
			quota.Allocated = max(quota.Allocated, quota.Used)
		}
		if quota.Allocated < 0 || quota.Used < 0 || total.Allocated > math.MaxInt64-quota.Allocated || total.Used > math.MaxInt64-quota.Used {
			return anyshare.Quota{}, errors.New("invalid or overflowing combined library quota")
		}
		total.Allocated += quota.Allocated
		total.Used += quota.Used
	}
	return total, nil
}

func (b *AccountBackend) recoveryRecord(record RecoveryRecord) RecoveryRecord {
	if account, remote, err := b.lookup(record.Entry.ID); err == nil {
		record.Account, record.Server = account.Name, account.Server
		record.Entry.ID, record.Parent = remote, anyshare.ParentID(remote)
	}
	return record
}
