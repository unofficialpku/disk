package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unofficialpku/disk/internal/vfs"
)

func selectTUIEntry(t *testing.T, m *tuiModel, id string) {
	t.Helper()
	for i, item := range m.remote.Items() {
		if e, ok := item.(remoteItem); ok && e.entry.ID == id {
			m.remote.Select(i)
			return
		}
	}
	t.Fatal("TUI entry absent", id)
}

func TestTUIAllAccountsFileManagementRoutesCollidingIDs(t *testing.T) {
	m, clients, roots := accountTUI(t)
	ns := m.backend.(*vfs.AccountBackend)
	for i, root := range roots {
		enterTUIAccount(t, m, root)
		file := ns.Scope(root.ID, clients[i].objects[tuiAccountLibrary+"/same"])
		selectTUIEntry(t, m, file.ID)
		finishAccountOperation(t, m, tuiKey(m, "H"))
		if !strings.Contains(m.View().Content, root.Name) {
			t.Fatal("history lost owning account")
		}
		tuiKey(m, "enter")
		finishAccountOperation(t, m, tuiKey(m, "y"))
		if clients[i].revisionRestores != 1 {
			t.Fatal("wrong revision account")
		}
		finishAccountOperation(t, m, tuiKey(m, "esc"))
		selectTUIEntry(t, m, file.ID)
		tuiKey(m, "delete")
		finishAccountOperation(t, m, tuiKey(m, "y"))
		if clients[i].deletions != 1 {
			t.Fatal("wrong deletion account")
		}
		finishAccountOperation(t, m, tuiKey(m, "b"))
		if len(m.management.choices.Items()) != 1 {
			t.Fatal("wrong recycle bin")
		}
		tuiKey(m, "enter")
		finishAccountOperation(t, m, tuiKey(m, "y"))
		if clients[i].restores != 1 {
			t.Fatal("wrong restore account")
		}
		finishAccountOperation(t, m, tuiKey(m, "esc"))
		selectTUIEntry(t, m, file.ID)
		tuiKey(m, "delete")
		finishAccountOperation(t, m, tuiKey(m, "y"))
		finishAccountOperation(t, m, tuiKey(m, "b"))
		tuiKey(m, "delete")
		finishAccountOperation(t, m, tuiKey(m, "y"))
		if clients[i].removals != 1 {
			t.Fatal("wrong permanent removal account")
		}
		finishAccountOperation(t, m, tuiKey(m, "esc"))
		local := filepath.Join(t.TempDir(), "instant.txt")
		os.WriteFile(local, []byte(root.Name), 0600)
		m.service.Fast = true
		finishAccountOperation(t, m, m.startTransfer("upload", local, m.location.entry.ID, false))
		if clients[i].instantUploads != 1 || string(clients[i].contents[tuiAccountLibrary+"/new-file"]) != root.Name {
			t.Fatal("instant upload used wrong account")
		}
	}
	for _, client := range clients {
		if client.deletions != 2 || client.restores != 1 || client.removals != 1 || client.revisionRestores != 1 || client.instantUploads != 1 {
			t.Fatal("colliding account IDs crossed operation boundaries")
		}
	}
}
