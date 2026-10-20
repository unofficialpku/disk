package tui

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/unofficialpku/disk/internal/auth"
	"github.com/unofficialpku/disk/internal/vfs"
)

const tuiAccountLibrary = "gns://shared"

type tuiAccountClient struct {
	tuiShareBackend
	t                                                               *testing.T
	owner                                                           string
	objects                                                         map[string]anyshare.Entry
	contents                                                        map[string][]byte
	shares, uploads                                                 int
	extractions                                                     int
	failDownload                                                    bool
	recycled                                                        map[string]anyshare.RecycleEntry
	deletions, restores, removals, revisionRestores, instantUploads int
}

func (b *tuiAccountClient) Delete(_ context.Context, entry anyshare.Entry) error {
	b.checkID(entry.ID)
	b.deletions++
	delete(b.objects, entry.ID)
	if b.recycled == nil {
		b.recycled = map[string]anyshare.RecycleEntry{}
	}
	entry.ID = tuiAccountLibrary + "/bin/recycled"
	b.recycled[entry.ID] = anyshare.RecycleEntry{Entry: entry, Path: "gns://Library"}
	return nil
}
func (b *tuiAccountClient) RecycleList(_ context.Context, id string) ([]anyshare.RecycleEntry, error) {
	b.checkID(id)
	items := []anyshare.RecycleEntry{}
	for _, entry := range b.recycled {
		items = append(items, entry)
	}
	return items, nil
}
func (b *tuiAccountClient) RecycleRestore(_ context.Context, id string, _ bool) (anyshare.Entry, error) {
	b.checkID(id)
	b.restores++
	entry := b.recycled[id].Entry
	delete(b.recycled, id)
	entry.ID = tuiAccountLibrary + "/same"
	b.objects[entry.ID] = entry
	return entry, nil
}
func (b *tuiAccountClient) RecycleDelete(_ context.Context, id string) error {
	b.checkID(id)
	b.removals++
	delete(b.recycled, id)
	return nil
}
func (b *tuiAccountClient) Revisions(_ context.Context, entry anyshare.Entry) ([]anyshare.Revision, error) {
	b.checkID(entry.ID)
	return []anyshare.Revision{{Rev: "old", Name: entry.Name, Editor: b.owner, Size: 5}}, nil
}
func (b *tuiAccountClient) RestoreRevision(_ context.Context, entry anyshare.Entry, rev string) error {
	b.checkID(entry.ID)
	b.revisionRestores++
	return nil
}
func (b *tuiAccountClient) FastUpload(ctx context.Context, parent, name string, data io.ReaderAt, size int64, mtime time.Time, observe func(int64)) (bool, anyshare.Entry, error) {
	b.checkID(parent)
	if observe != nil {
		observe(size)
	}
	b.instantUploads++
	entry, err := b.Upload(ctx, parent, name, nil, data, size, mtime)
	return true, entry, err
}

func (b *tuiAccountClient) checkID(id string) {
	b.t.Helper()
	if !strings.HasPrefix(id, "gns://") {
		b.t.Errorf("account namespace leaked into remote operation: %s", id)
	}
}
func (b *tuiAccountClient) Roots(context.Context) ([]anyshare.Entry, error) {
	return []anyshare.Entry{b.objects[tuiAccountLibrary]}, nil
}
func (b *tuiAccountClient) List(_ context.Context, id string) ([]anyshare.Entry, error) {
	b.checkID(id)
	entries := []anyshare.Entry{}
	for _, entry := range b.objects {
		if anyshare.ParentID(entry.ID) == id {
			entries = append(entries, entry)
		}
	}
	return entries, nil
}
func (b *tuiAccountClient) Resolve(_ context.Context, selector string) (anyshare.Entry, error) {
	if strings.HasPrefix(selector, "account://") {
		b.t.Error("scoped selector leaked into account Resolve")
	}
	if selector == "/Library" {
		selector = tuiAccountLibrary
	}
	for _, entry := range b.objects {
		if entry.ID == selector || selector == "/Library/"+entry.Name {
			return entry, nil
		}
	}
	return anyshare.Entry{}, anyshare.ErrNotFound
}
func (b *tuiAccountClient) Upload(_ context.Context, parent, name string, existing *anyshare.Entry, data io.ReaderAt, size int64, modified time.Time) (anyshare.Entry, error) {
	b.checkID(parent)
	id := parent + "/new-file"
	if existing != nil {
		b.checkID(existing.ID)
		id = existing.ID
	}
	payload, err := io.ReadAll(io.NewSectionReader(data, 0, size))
	if err != nil {
		return anyshare.Entry{}, err
	}
	entry := anyshare.Entry{ID: id, Name: name, Size: size, Rev: "new", Modified: modified}
	b.objects[id], b.contents[id] = entry, payload
	b.uploads++
	return entry, nil
}
func (b *tuiAccountClient) Download(_ context.Context, entry anyshare.Entry, writer io.Writer) error {
	b.checkID(entry.ID)
	if b.failDownload {
		return auth.ErrSessionExpired
	}
	_, err := writer.Write(b.contents[entry.ID])
	return err
}
func (b *tuiAccountClient) DownloadArchive(_ context.Context, entry anyshare.Entry, writer io.Writer, observe func(anyshare.ArchiveProgress)) (int64, error) {
	b.checkID(entry.ID)
	var data bytes.Buffer
	archive := zip.NewWriter(&data)
	file, err := archive.Create(entry.Name + "/inner.txt")
	if err != nil {
		return 0, err
	}
	io.WriteString(file, b.owner)
	if err := archive.Close(); err != nil {
		return 0, err
	}
	n, err := writer.Write(data.Bytes())
	if observe != nil {
		observe(anyshare.ArchiveProgress{Done: int64(n), Total: int64(n)})
	}
	return int64(n), err
}
func (b *tuiAccountClient) Extract(_ context.Context, archive anyshare.Entry, parent, name string, observe func(int)) (anyshare.Entry, error) {
	b.checkID(archive.ID)
	b.checkID(parent)
	b.extractions++
	entry := anyshare.Entry{ID: parent + "/extracted", Name: name, Dir: true, Size: -1}
	b.objects[entry.ID] = entry
	if observe != nil {
		observe(100)
	}
	return entry, nil
}
func (b *tuiAccountClient) InternalLink(_ context.Context, entry anyshare.Entry) (anyshare.ShareLink, error) {
	b.checkID(entry.ID)
	b.shares++
	return anyshare.ShareLink{ID: b.owner, Type: "realname", URL: "https://example.test/link/" + b.owner}, nil
}
func (b *tuiAccountClient) CreateExternalLink(ctx context.Context, entry anyshare.Entry, _ anyshare.ShareOptions) (anyshare.ShareLink, error) {
	link, err := b.InternalLink(ctx, entry)
	link.Type = "anonymous"
	return link, err
}
func (b *tuiAccountClient) GrantShare(_ context.Context, entry anyshare.Entry, _ []anyshare.Recipient, _ anyshare.ShareOptions) error {
	b.checkID(entry.ID)
	b.shares++
	return nil
}
func (b *tuiAccountClient) RemoveExternalLink(_ context.Context, id string) error {
	if id != "outer" {
		b.t.Errorf("scoped or unrelated link ID reached server: %s", id)
	}
	b.removes++
	return nil
}
func (b *tuiAccountClient) SearchRecipients(context.Context, string, int) ([]anyshare.Recipient, error) {
	return []anyshare.Recipient{{ID: b.owner + "-recipient", Type: "user", Name: b.owner}}, nil
}

func accountTUI(t *testing.T) (*tuiModel, [2]*tuiAccountClient, []anyshare.Entry) {
	t.Helper()
	var clients [2]*tuiAccountClient
	var accounts []vfs.MountAccount
	for i, owner := range []string{"alpha", "bravo"} {
		b := &tuiAccountClient{t: t, owner: owner, objects: make(map[string]anyshare.Entry), contents: make(map[string][]byte)}
		for _, entry := range []anyshare.Entry{
			{ID: tuiAccountLibrary, Name: "Library", Dir: true, Size: -1},
			{ID: tuiAccountLibrary + "/same", Name: "same.txt", Size: 5, Rev: "same"},
			{ID: tuiAccountLibrary + "/folder", Name: "folder", Dir: true, Size: -1},
			{ID: tuiAccountLibrary + "/folder/inner", Name: "inner.txt", Size: 5, Rev: "same"},
			{ID: tuiAccountLibrary + "/archive", Name: "archive.zip", Size: 0},
		} {
			b.objects[entry.ID] = entry
			b.contents[entry.ID] = []byte(owner)
		}
		clients[i] = b
		accounts = append(accounts, vfs.MountAccount{Name: owner, Server: "https://" + owner + ".example", Backend: b})
	}
	namespace, err := vfs.NewAccountBackend(accounts)
	if err != nil {
		t.Fatal(err)
	}
	model := newTUIModel(context.Background(), namespace, "All accounts", "2 saved accounts", time.Second)
	finishAccountOperation(t, model, model.Init())
	roots, err := namespace.Roots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return model, clients, roots
}

func finishAccountOperation(t *testing.T, model *tuiModel, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("operation did not start", model.status)
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatal("network operation is not asynchronous")
	}
	_, next := model.Update(batch[len(batch)-1]())
	if model.busy && next != nil {
		finishAccountOperation(t, model, next)
	}
}

func enterTUIAccount(t *testing.T, model *tuiModel, root anyshare.Entry) anyshare.Entry {
	t.Helper()
	finishAccountOperation(t, model, model.load(browserLocation{entry: &root, path: "/" + root.Name}, nil))
	entry := model.selected()
	if entry == nil || entry.Name != "Library" {
		t.Fatal("account does not contain its libraries")
	}
	finishAccountOperation(t, model, model.load(browserLocation{entry: entry, path: "/" + root.Name + "/Library"}, []browserLocation{{path: "/"}, {entry: &root, path: "/" + root.Name}}))
	return *entry
}

func TestTUIAllAccountsNavigationContextAndVirtualDirectories(t *testing.T) {
	model, _, roots := accountTUI(t)
	if !strings.Contains(model.View().Content, "Account: All accounts") || len(model.remote.Items()) != 2 {
		t.Fatal("aggregate root missing accounts")
	}
	for _, action := range []rune{'d', 'a', 'v', 's'} {
		model.Update(tea.KeyPressMsg{Code: action, Text: string(action)})
		if model.mode != browseMode || model.busy {
			t.Fatal("virtual account directory allows an operation", string(action))
		}
	}
	_, cmd := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	finishAccountOperation(t, model, cmd)
	model.Update(tea.KeyPressMsg{Code: 'u', Text: "u"})
	if model.mode != browseMode {
		t.Fatal("upload picker opened in a virtual account directory")
	}
	var ids [2]string
	for i, root := range roots {
		library := enterTUIAccount(t, model, root)
		ids[i] = library.ID
		if !strings.Contains(model.View().Content, "Account: "+root.Name) || !strings.Contains(model.View().Content, root.Name+".example") {
			t.Fatal("header lost the owning account/server")
		}
	}
	if ids[0] == ids[1] {
		t.Fatal("identical remote libraries lost their account identities")
	}
	_, cmd = model.Update(tea.KeyPressMsg{Code: 'g', Text: "g"})
	finishAccountOperation(t, model, cmd)
	if !strings.Contains(model.View().Content, "Account: All accounts") {
		t.Fatal("g did not restore the aggregate account root")
	}
}

func TestTUIAllAccountsTransfersUseRawOwningBackend(t *testing.T) {
	model, clients, roots := accountTUI(t)
	namespace := model.backend.(*vfs.AccountBackend)
	for i, root := range roots {
		library := enterTUIAccount(t, model, root)
		file := namespace.Scope(root.ID, clients[i].objects[tuiAccountLibrary+"/same"])
		local := filepath.Join(t.TempDir(), "same.txt")
		finishAccountOperation(t, model, model.startTransfer("download", file.ID, local, false))
		if data, err := os.ReadFile(local); err != nil || string(data) != root.Name {
			t.Fatal("download used another account", string(data), err)
		}
		payload := []byte("upload for " + root.Name)
		if err := os.WriteFile(local, payload, 0600); err != nil {
			t.Fatal(err)
		}
		finishAccountOperation(t, model, model.startTransfer("upload", local, library.ID, true))
		if clients[i].uploads != 1 || !bytes.Equal(clients[i].contents[tuiAccountLibrary+"/same"], payload) {
			t.Fatal("overwrite did not use its own account")
		}
		// Upload completion refreshes the directory with the scoped result ID.
		if selected := model.selected(); selected == nil || selected.ID != file.ID {
			t.Fatal("upload refresh lost the account-scoped selection")
		}
	}
	for i, root := range roots {
		if string(clients[i].contents[tuiAccountLibrary+"/same"]) != "upload for "+root.Name {
			t.Fatal("upload leaked into another account")
		}
	}
}

func TestTUIAllAccountsRecursiveAndZIPDownloads(t *testing.T) {
	for _, mode := range []string{"direct", "archive", "via-archive"} {
		t.Run(mode, func(t *testing.T) {
			model, clients, roots := accountTUI(t)
			namespace := model.backend.(*vfs.AccountBackend)
			folder := namespace.Scope(roots[1].ID, clients[1].objects[tuiAccountLibrary+"/folder"])
			model.archive, model.viaArchive = mode == "archive", mode == "via-archive"
			local := filepath.Join(t.TempDir(), "download")
			finishAccountOperation(t, model, model.startTransfer("download", folder.ID, local, false))
			if model.statusError {
				t.Fatal(model.status)
			}
			if mode == "archive" {
				archive, err := zip.OpenReader(local)
				if err != nil {
					t.Fatal(err)
				}
				defer archive.Close()
				if len(archive.File) != 1 || archive.File[0].Name != "folder/inner.txt" {
					t.Fatal("archive came from another namespace")
				}
			} else if data, err := os.ReadFile(filepath.Join(local, "inner.txt")); err != nil || string(data) != "bravo" {
				t.Fatal(string(data), err)
			}
		})
	}
}

func TestTUIAllAccountsExtractionRejectsOtherAccount(t *testing.T) {
	model, clients, roots := accountTUI(t)
	enterTUIAccount(t, model, roots[1])
	namespace := model.backend.(*vfs.AccountBackend)
	archive := namespace.Scope(roots[1].ID, clients[1].objects[tuiAccountLibrary+"/archive"])
	if cmd := model.startTransfer("extract", archive.ID, "/alpha/Library/extracted", false); cmd != nil || model.busy || clients[0].extractions != 0 || clients[1].extractions != 0 {
		t.Fatal("cross-account extraction started a mutation")
	}
	finishAccountOperation(t, model, model.startTransfer("extract", archive.ID, "/bravo/Library/extracted", false))
	if model.statusError || clients[1].extractions != 1 || clients[0].extractions != 0 {
		t.Fatal("same-account extraction failed", model.status)
	}
}

func TestTUIAllAccountsSharingAndSessionErrorsNameOwner(t *testing.T) {
	model, clients, roots := accountTUI(t)
	enterTUIAccount(t, model, roots[1])
	for i, item := range model.remote.Items() {
		if item.(remoteItem).entry.Name == "same.txt" {
			model.remote.Select(i)
		}
	}
	model.openSharing()
	finishAccountOperation(t, model, model.sharingOperation("internal", ""))
	if model.sharing.link.URL != "https://example.test/link/bravo" || clients[1].shares != 1 || clients[0].shares != 0 {
		t.Fatal("internal link used another account")
	}
	finishAccountOperation(t, model, model.sharingOperation("search", "name"))
	if item := model.sharing.choices.SelectedItem().(recipientItem); item.ID != "bravo-recipient" {
		t.Fatal("recipient search used another account's directory")
	}
	model.sharing.recipient = anyshare.Recipient{ID: "recipient", Type: "user"}
	finishAccountOperation(t, model, model.sharingOperation("grant", ""))
	finishAccountOperation(t, model, model.sharingOperation("external", ""))
	if clients[0].shares != 0 || clients[1].shares != 4 {
		t.Fatal("share mutation lost the selected account")
	}
	finishAccountOperation(t, model, model.sharingOperation("links", ""))
	link := model.sharing.choices.SelectedItem().(shareLinkItem)
	model.sharing.link = link.ShareLink
	finishAccountOperation(t, model, model.sharingOperation("revoke-link", ""))
	if clients[0].removes != 0 || clients[1].removes != 1 {
		t.Fatal("link removal crossed account boundaries")
	}
	clients[1].failDownload = true
	file := model.backend.(*vfs.AccountBackend).Scope(roots[1].ID, clients[1].objects[tuiAccountLibrary+"/same"])
	finishAccountOperation(t, model, model.startTransfer("download", file.ID, filepath.Join(t.TempDir(), "failed"), false))
	if !model.statusError || !strings.Contains(model.status, `account "bravo"`) || !strings.Contains(model.status, "session expired") {
		t.Fatal("session failure omitted owning account", model.status)
	}
	if clients[0].shares != 0 {
		t.Fatal("session failure changed another account")
	}
}
