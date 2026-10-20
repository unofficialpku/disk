package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"github.com/unofficialpku/disk/internal/anyshare"
)

type managementTUIBackend struct {
	*tuiBackend
	recycled                   []anyshare.RecycleEntry
	versions                   []anyshare.Revision
	deleted, removed           []string
	restored, revisionRestored []string
	restoreCollision           bool
}

func (b *managementTUIBackend) Delete(_ context.Context, e anyshare.Entry) error {
	b.deleted = append(b.deleted, e.ID)
	return nil
}
func (b *managementTUIBackend) RecycleList(context.Context, string) ([]anyshare.RecycleEntry, error) {
	return append([]anyshare.RecycleEntry{}, b.recycled...), nil
}
func (b *managementTUIBackend) RecycleRestore(_ context.Context, id string, rename bool) (anyshare.Entry, error) {
	if b.restoreCollision && !rename {
		return anyshare.Entry{}, anyshare.ErrExists
	}
	b.restored = append(b.restored, id)
	return anyshare.Entry{ID: "gns://lib/new"}, nil
}
func (b *managementTUIBackend) RecycleDelete(_ context.Context, id string) error {
	b.removed = append(b.removed, id)
	return nil
}
func (b *managementTUIBackend) Revisions(context.Context, anyshare.Entry) ([]anyshare.Revision, error) {
	return append([]anyshare.Revision{}, b.versions...), nil
}
func (b *managementTUIBackend) RestoreRevision(_ context.Context, e anyshare.Entry, rev string) error {
	b.revisionRestored = append(b.revisionRestored, e.ID+":"+rev)
	return nil
}

func managementTUI(t *testing.T) (*tuiModel, *managementTUIBackend) {
	t.Helper()
	backend := &managementTUIBackend{tuiBackend: &tuiBackend{entries: []anyshare.Entry{{ID: "gns://lib/file", Name: "file.txt", Size: 4}}}, recycled: []anyshare.RecycleEntry{{Entry: anyshare.Entry{ID: "gns://lib/bin/recycled", Name: "deleted.txt", Size: 1536}, Path: "gns://Library/Original"}}, versions: []anyshare.Revision{{Rev: "old", Name: "file.txt", Size: 1536, Modified: time.Now()}}}
	m := newTUIModel(context.Background(), backend, "work", "https://example.test", time.Second)
	finishNetworkCommand(t, m, m.Init())
	m.location.entry = &anyshare.Entry{ID: "gns://lib", Name: "Library", Dir: true}
	m.location.path = "/Library"
	return m, backend
}
func tuiKey(m *tuiModel, key string) tea.Cmd {
	var msg tea.KeyPressMsg
	switch key {
	case "enter":
		msg.Code = tea.KeyEnter
	case "delete":
		msg.Code = tea.KeyDelete
	case "esc":
		msg.Code = tea.KeyEscape
	default:
		msg.Code = rune(key[0])
		msg.Text = key
	}
	_, cmd := m.Update(msg)
	return cmd
}

func TestTUIDeleteRequiresConfirmationAndGuardsLibraries(t *testing.T) {
	m, b := managementTUI(t)
	tuiKey(m, "delete")
	if m.mode != managementMode || len(b.deleted) > 0 || !strings.Contains(m.View().Content, "recycle bin") {
		t.Fatal("delete was not reviewed")
	}
	tuiKey(m, "enter")
	if len(b.deleted) > 0 || m.mode != browseMode {
		t.Fatal("Enter must default to cancel")
	}
	tuiKey(m, "delete")
	finishAccountOperation(t, m, tuiKey(m, "y"))
	if len(b.deleted) != 1 || b.deleted[0] != "gns://lib/file" || m.mode != browseMode {
		t.Fatal(b.deleted)
	}
	m.remote.SetItems([]list.Item{remoteItem{entry: anyshare.Entry{ID: "gns://lib", Name: "Library", Dir: true}}})
	tuiKey(m, "delete")
	if m.mode != browseMode || !strings.Contains(m.status, "roots cannot") {
		t.Fatal("library deletion guard missing")
	}
}

func TestTUIRecycleRestoreCollisionPermanentRemovalAndEmpty(t *testing.T) {
	m, b := managementTUI(t)
	finishAccountOperation(t, m, tuiKey(m, "b"))
	if m.mode != managementMode || len(m.management.choices.Items()) != 1 || !strings.Contains(m.View().Content, "1.5 KiB") {
		t.Fatal("recycle list missing")
	}
	b.restoreCollision = true
	tuiKey(m, "enter")
	finishAccountOperation(t, m, tuiKey(m, "y"))
	if m.management.action != "restore-rename" || m.management.stage != "confirm" || len(b.restored) != 0 {
		t.Fatal("collision needs a separate review")
	}
	finishAccountOperation(t, m, tuiKey(m, "y"))
	if len(b.restored) != 1 || b.restored[0] != "gns://lib/bin/recycled" {
		t.Fatal("restore used original rather than recycled ID")
	}
	tuiKey(m, "delete")
	if !strings.Contains(m.View().Content, "cannot be undone") {
		t.Fatal("permanent delete is not explained")
	}
	tuiKey(m, "esc")
	if len(b.removed) > 0 {
		t.Fatal("cancelled permanent removal made changes")
	}
	tuiKey(m, "delete")
	finishAccountOperation(t, m, tuiKey(m, "y"))
	if len(b.removed) != 1 {
		t.Fatal(b.removed)
	}
	tuiKey(m, "e")
	if len(m.management.snapshot) != 1 || !strings.Contains(m.View().Content, "all 1") {
		t.Fatal("empty snapshot not reviewed")
	}
	finishAccountOperation(t, m, tuiKey(m, "y"))
	if len(b.removed) != 2 {
		t.Fatal(b.removed)
	}
	finishAccountOperation(t, m, tuiKey(m, "esc"))
	if m.mode != browseMode || m.management != nil {
		t.Fatal("closing a changed bin failed to refresh browser")
	}
}

func TestTUIRevisionRestoreAndFilters(t *testing.T) {
	m, b := managementTUI(t)
	finishAccountOperation(t, m, tuiKey(m, "H"))
	if len(m.management.choices.Items()) != 1 {
		t.Fatal("no versions loaded")
	}
	m.management.choices.SetFilterState(list.Filtering)
	tuiKey(m, "q")
	if m.management.choices.FilterInput.Value() != "q" || m.quitting {
		t.Fatal("filter text quit the program")
	}
	m.management.choices.ResetFilter()
	tuiKey(m, "enter")
	if len(b.revisionRestored) > 0 || !strings.Contains(m.View().Content, "changes the current") {
		t.Fatal("revision restore was not reviewed")
	}
	finishAccountOperation(t, m, tuiKey(m, "y"))
	if len(b.revisionRestored) != 1 || b.revisionRestored[0] != "gns://lib/file:old" {
		t.Fatal(b.revisionRestored)
	}
}

func TestTUIInstantUploadToggleAndStaleResults(t *testing.T) {
	m, _ := managementTUI(t)
	tuiKey(m, "F")
	if !m.service.Fast || !strings.Contains(m.status, "on") {
		t.Fatal("fast toggle failed")
	}
	m.mode = pickerMode
	tuiKey(m, "F")
	if m.service.Fast || !strings.Contains(m.View().Content, "Instant upload: off") {
		t.Fatal("picker toggle failed")
	}
	m.Update(managementMsg{id: m.operation - 1, err: errors.New("stale result")})
	if m.statusError {
		t.Fatal("stale async result changed current state")
	}
}
