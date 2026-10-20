package vfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"syscall"
	"time"

	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/unofficialpku/disk/internal/transfer"
)

// WithAccount binds operations without a document ID (recipient lookup, for
// example) to the selected account. Document IDs remain transparently scoped.
func (b *AccountBackend) WithAccount(id string) (*AccountBackend, error) {
	if _, _, err := b.remote(id); err != nil {
		return nil, err
	}
	view := *b
	view.selected, _ = splitAccountID(id)
	return &view, nil
}

// LibraryID and ParentID preserve account identity when deriving ancestors.
func LibraryID(id string) string { return libraryID(id) }
func ParentID(id string) string  { return parentID(id) }

func operationBackend[T any](b *AccountBackend, id string) (backend T, remote string, err error) {
	account, remote, err := b.remote(id)
	if err != nil {
		return backend, "", err
	}
	backend, ok := account.Backend.(T)
	if !ok {
		return backend, "", fmt.Errorf("account %q does not support this operation", account.Name)
	}
	return backend, remote, nil
}

func (b *AccountBackend) operationError(id string, err error) error {
	if err != nil {
		if account, _, lookupErr := b.lookup(id); lookupErr == nil {
			return fmt.Errorf("account %q: %w", account.Name, err)
		}
	}
	return err
}

func (b *AccountBackend) Resolve(ctx context.Context, selector string) (anyshare.Entry, error) {
	prefix, remote := splitAccountID(selector)
	if prefix == "" {
		if strings.HasPrefix(selector, "gns://") {
			prefix, remote = b.selected, selector
		} else {
			name, tail, _ := strings.Cut(strings.TrimPrefix(selector, "/"), "/")
			for _, root := range b.roots {
				if root.Name == name {
					prefix, remote = root.ID, "/"+tail
					break
				}
			}
		}
	}
	if prefix == "" {
		return anyshare.Entry{}, fmt.Errorf("%w: select a path under an account directory", anyshare.ErrNotFound)
	}
	if remote == "" || remote == "/" {
		return b.Stat(ctx, anyshare.Entry{ID: prefix, Dir: true})
	}
	backend, _, err := operationBackend[interface {
		Resolve(context.Context, string) (anyshare.Entry, error)
	}](b, prefix+remote)
	if err != nil {
		return anyshare.Entry{}, err
	}
	entry, err := backend.Resolve(ctx, remote)
	return scopedEntry(prefix, entry), b.operationError(prefix, err)
}

func (b *AccountBackend) DownloadArchive(ctx context.Context, entry anyshare.Entry, dest io.Writer, observe func(anyshare.ArchiveProgress)) (int64, error) {
	backend, remote, err := operationBackend[transfer.Backend](b, entry.ID)
	if err != nil {
		return 0, err
	}
	id := entry.ID
	entry.ID = remote
	n, err := backend.DownloadArchive(ctx, entry, dest, observe)
	return n, b.operationError(id, err)
}

func (b *AccountBackend) Extract(ctx context.Context, entry anyshare.Entry, parent, name string, observe func(int)) (anyshare.Entry, error) {
	backend, remote, err := operationBackend[transfer.Backend](b, entry.ID)
	if err != nil {
		return anyshare.Entry{}, err
	}
	prefix, _ := splitAccountID(entry.ID)
	parentPrefix, destination := splitAccountID(parent)
	if prefix != parentPrefix {
		return anyshare.Entry{}, fmt.Errorf("cross-account extraction is not supported: %w", syscall.EXDEV)
	}
	entry.ID = remote
	result, err := backend.Extract(ctx, entry, destination, name, observe)
	return scopedEntry(prefix, result), b.operationError(prefix, err)
}

func (b *AccountBackend) FastUpload(ctx context.Context, parent, name string, data io.ReaderAt, size int64, modified time.Time, observe func(int64)) (bool, anyshare.Entry, error) {
	backend, remote, err := operationBackend[interface {
		FastUpload(context.Context, string, string, io.ReaderAt, int64, time.Time, func(int64)) (bool, anyshare.Entry, error)
	}](b, parent)
	if err != nil {
		return false, anyshare.Entry{}, err
	}
	matched, entry, err := backend.FastUpload(ctx, remote, name, data, size, modified, observe)
	return matched, scopedEntry(parent, entry), b.operationError(parent, err)
}

type managementBackend interface {
	RecycleList(context.Context, string) ([]anyshare.RecycleEntry, error)
	RecycleRestore(context.Context, string, bool) (anyshare.Entry, error)
	RecycleDelete(context.Context, string) error
	Revisions(context.Context, anyshare.Entry) ([]anyshare.Revision, error)
	RestoreRevision(context.Context, anyshare.Entry, string) error
}

func (b *AccountBackend) RecycleList(ctx context.Context, id string) ([]anyshare.RecycleEntry, error) {
	backend, remote, err := operationBackend[managementBackend](b, id)
	if err != nil {
		return nil, err
	}
	entries, err := backend.RecycleList(ctx, remote)
	result := make([]anyshare.RecycleEntry, len(entries))
	for i, entry := range entries {
		result[i] = entry
		result[i].Entry = scopedEntry(id, entry.Entry)
	}
	return result, b.operationError(id, err)
}
func (b *AccountBackend) RecycleRestore(ctx context.Context, id string, rename bool) (anyshare.Entry, error) {
	backend, remote, err := operationBackend[managementBackend](b, id)
	if err != nil {
		return anyshare.Entry{}, err
	}
	entry, err := backend.RecycleRestore(ctx, remote, rename)
	return scopedEntry(id, entry), b.operationError(id, err)
}
func (b *AccountBackend) RecycleDelete(ctx context.Context, id string) error {
	backend, remote, err := operationBackend[managementBackend](b, id)
	if err != nil {
		return err
	}
	return b.operationError(id, backend.RecycleDelete(ctx, remote))
}
func (b *AccountBackend) RecycleEmpty(ctx context.Context, snapshot []anyshare.RecycleEntry, observe func(int, int)) (int, error) {
	return anyshare.RecycleEmpty(ctx, b, snapshot, observe)
}
func (b *AccountBackend) Revisions(ctx context.Context, entry anyshare.Entry) ([]anyshare.Revision, error) {
	backend, remote, err := operationBackend[managementBackend](b, entry.ID)
	if err != nil {
		return nil, err
	}
	id := entry.ID
	entry.ID = remote
	versions, err := backend.Revisions(ctx, entry)
	return versions, b.operationError(id, err)
}
func (b *AccountBackend) RestoreRevision(ctx context.Context, entry anyshare.Entry, rev string) error {
	backend, remote, err := operationBackend[managementBackend](b, entry.ID)
	if err != nil {
		return err
	}
	id := entry.ID
	entry.ID = remote
	return b.operationError(id, backend.RestoreRevision(ctx, entry, rev))
}

type sharingBackend interface {
	InternalLink(context.Context, anyshare.Entry) (anyshare.ShareLink, error)
	CreateExternalLink(context.Context, anyshare.Entry, anyshare.ShareOptions) (anyshare.ShareLink, error)
	ShareLinks(context.Context, anyshare.Entry, bool) ([]anyshare.ShareLink, error)
	RemoveExternalLink(context.Context, string) error
	GrantShare(context.Context, anyshare.Entry, []anyshare.Recipient, anyshare.ShareOptions) error
	SharePermissions(context.Context, string) (anyshare.ShareACL, error)
	RevokeShare(context.Context, string, []anyshare.Recipient) (int, error)
	SearchRecipients(context.Context, string, int) ([]anyshare.Recipient, error)
}

func scopedLink(id string, link anyshare.ShareLink) anyshare.ShareLink {
	if link.ID != "" {
		prefix, _ := splitAccountID(id)
		link.ID = prefix + link.ID
	}
	return link
}
func (b *AccountBackend) InternalLink(ctx context.Context, entry anyshare.Entry) (anyshare.ShareLink, error) {
	backend, remote, err := operationBackend[sharingBackend](b, entry.ID)
	if err != nil {
		return anyshare.ShareLink{}, err
	}
	id := entry.ID
	entry.ID = remote
	link, err := backend.InternalLink(ctx, entry)
	return scopedLink(id, link), b.operationError(id, err)
}
func (b *AccountBackend) CreateExternalLink(ctx context.Context, entry anyshare.Entry, opts anyshare.ShareOptions) (anyshare.ShareLink, error) {
	backend, remote, err := operationBackend[sharingBackend](b, entry.ID)
	if err != nil {
		return anyshare.ShareLink{}, err
	}
	id := entry.ID
	entry.ID = remote
	link, err := backend.CreateExternalLink(ctx, entry, opts)
	return scopedLink(id, link), b.operationError(id, err)
}
func (b *AccountBackend) ShareLinks(ctx context.Context, entry anyshare.Entry, external bool) ([]anyshare.ShareLink, error) {
	backend, remote, err := operationBackend[sharingBackend](b, entry.ID)
	if err != nil {
		return nil, err
	}
	id := entry.ID
	entry.ID = remote
	links, err := backend.ShareLinks(ctx, entry, external)
	result := make([]anyshare.ShareLink, len(links))
	for i, link := range links {
		result[i] = scopedLink(id, link)
	}
	return result, b.operationError(id, err)
}
func (b *AccountBackend) RemoveExternalLink(ctx context.Context, id string) error {
	backend, remote, err := operationBackend[sharingBackend](b, id)
	if err != nil {
		return err
	}
	return b.operationError(id, backend.RemoveExternalLink(ctx, remote))
}
func (b *AccountBackend) GrantShare(ctx context.Context, entry anyshare.Entry, recipients []anyshare.Recipient, opts anyshare.ShareOptions) error {
	backend, remote, err := operationBackend[sharingBackend](b, entry.ID)
	if err != nil {
		return err
	}
	id := entry.ID
	entry.ID = remote
	return b.operationError(id, backend.GrantShare(ctx, entry, recipients, opts))
}
func (b *AccountBackend) SharePermissions(ctx context.Context, id string) (anyshare.ShareACL, error) {
	backend, remote, err := operationBackend[sharingBackend](b, id)
	if err != nil {
		return anyshare.ShareACL{}, err
	}
	acl, err := backend.SharePermissions(ctx, remote)
	return acl, b.operationError(id, err)
}
func (b *AccountBackend) RevokeShare(ctx context.Context, id string, recipients []anyshare.Recipient) (int, error) {
	backend, remote, err := operationBackend[sharingBackend](b, id)
	if err != nil {
		return 0, err
	}
	n, err := backend.RevokeShare(ctx, remote, recipients)
	return n, b.operationError(id, err)
}
func (b *AccountBackend) SearchRecipients(ctx context.Context, keyword string, limit int) ([]anyshare.Recipient, error) {
	account, ok := b.accounts[b.selected]
	if !ok {
		return nil, errors.New("select an account before searching recipients")
	}
	backend, ok := account.Backend.(sharingBackend)
	if !ok {
		return nil, errors.New("account does not support sharing")
	}
	rows, err := backend.SearchRecipients(ctx, keyword, limit)
	return rows, b.operationError(b.selected, err)
}

var _ transfer.Backend = (*AccountBackend)(nil)

// ExtractionDestination checks account boundaries before a surface starts an
// asynchronous extraction. Extract repeats this check on resolved IDs.
func (b *AccountBackend) ExtractionDestination(selector string) (string, error) {
	account, ok := b.accounts[b.selected]
	if !ok {
		return "", errors.New("select an account before extracting")
	}
	if strings.HasPrefix(selector, "gns://") {
		return selector, nil
	}
	if prefix, _ := splitAccountID(selector); prefix == b.selected {
		return selector, nil
	}
	if strings.HasPrefix(selector, "/"+account.Name+"/") {
		return selector, nil
	}
	return "", fmt.Errorf("extract into a library under /%s/; cross-account extraction is not supported: %w", account.Name, syscall.EXDEV)
}

func (b *AccountBackend) RecycleLibraries(ctx context.Context) ([]anyshare.Entry, error) {
	result := []anyshare.Entry{}
	for _, root := range b.roots {
		if b.selected != "" && b.selected != root.ID {
			continue
		}
		account := b.accounts[root.ID]
		backend, ok := account.Backend.(interface {
			RecycleLibraries(context.Context) ([]anyshare.Entry, error)
		})
		if !ok {
			return nil, fmt.Errorf("account %q does not expose recycle libraries", account.Name)
		}
		libraries, err := backend.RecycleLibraries(ctx)
		if err != nil {
			return nil, b.operationError(root.ID, err)
		}
		for _, library := range libraries {
			result = append(result, scopedEntry(root.ID, library))
		}
	}
	return result, nil
}

func (b *AccountBackend) LibraryQuota(ctx context.Context, id string) (anyshare.Quota, error) {
	backend, remote, err := operationBackend[anyshare.LibraryStatsBackend](b, id)
	if err != nil {
		return anyshare.Quota{}, err
	}
	quota, err := backend.LibraryQuota(ctx, remote)
	return quota, b.operationError(id, err)
}
func (b *AccountBackend) Writable(ctx context.Context, id string) (bool, error) {
	backend, remote, err := operationBackend[anyshare.LibraryStatsBackend](b, id)
	if err != nil {
		return false, err
	}
	writable, err := backend.Writable(ctx, remote)
	return writable, b.operationError(id, err)
}

func (b *AccountBackend) Copy(ctx context.Context, source, destination anyshare.Entry, observe func(anyshare.CopyProgress)) (anyshare.Entry, error) {
	backend, remote, err := operationBackend[interface {
		Copy(context.Context, anyshare.Entry, anyshare.Entry, func(anyshare.CopyProgress)) (anyshare.Entry, error)
	}](b, source.ID)
	if err != nil {
		return anyshare.Entry{}, err
	}
	prefix, _ := splitAccountID(source.ID)
	destinationPrefix, target := splitAccountID(destination.ID)
	if prefix != destinationPrefix {
		return anyshare.Entry{}, syscall.EXDEV
	}
	source.ID, destination.ID = remote, target
	entry, err := backend.Copy(ctx, source, destination, observe)
	return scopedEntry(prefix, entry), b.operationError(prefix, err)
}
