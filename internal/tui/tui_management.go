package tui

import (
	"context"
	"errors"
	"fmt"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/unofficialpku/disk/internal/vfs"
)

type managementBackend interface {
	Delete(context.Context, anyshare.Entry) error
	RecycleList(context.Context, string) ([]anyshare.RecycleEntry, error)
	RecycleRestore(context.Context, string, bool) (anyshare.Entry, error)
	RecycleDelete(context.Context, string) error
	Revisions(context.Context, anyshare.Entry) ([]anyshare.Revision, error)
	RestoreRevision(context.Context, anyshare.Entry, string) error
}

type recycleItem struct {
	anyshare.RecycleEntry
	sizes sizeFormat
}

func (i recycleItem) Title() string { return (remoteItem{entry: i.Entry}).Title() }
func (i recycleItem) Description() string {
	size := "-"
	if !i.Dir && i.Size >= 0 {
		size = i.sizes.Value(i.Size)
	}
	return tuiText(size + " • deleted " + displayTime(i.Modified) + " • " + i.Path)
}
func (i recycleItem) FilterValue() string { return i.Name + " " + i.Path }

type revisionItem struct {
	anyshare.Revision
	sizes sizeFormat
}

func (i revisionItem) Title() string { return tuiText(displayTime(i.Modified) + " • " + i.Name) }
func (i revisionItem) Description() string {
	return tuiText(i.sizes.Value(i.Size) + " • " + i.Editor + " • " + i.Rev)
}
func (i revisionItem) FilterValue() string { return i.Rev + " " + i.Name + " " + i.Editor }

type tuiManagement struct {
	kind, stage, action string
	entry               anyshare.Entry
	recycled            anyshare.RecycleEntry
	revision            anyshare.Revision
	snapshot            []anyshare.RecycleEntry
	choices             list.Model
	backend             managementBackend
	dirty               bool
}
type managementMsg struct {
	id       int
	action   string
	entries  []anyshare.RecycleEntry
	versions []anyshare.Revision
	err      error
}

func (m *tuiModel) openManagement(kind string) tea.Cmd {
	entry := m.selected()
	if kind == "recycle" && m.location.entry != nil && !m.virtual(m.location.entry.ID) {
		entry = m.location.entry
	}
	if entry == nil {
		m.status = "Select a library or file first"
		return nil
	}
	if m.virtual(entry.ID) {
		m.status = "Open an account's library before file operations"
		return nil
	}
	target, err := m.target(entry.ID)
	if err != nil {
		m.setError("Select account", err)
		return nil
	}
	selected := *entry
	selected.ID = target.id
	if kind == "delete" && vfs.ParentID(selected.ID) == "" {
		m.status = "Library roots cannot be deleted"
		return nil
	}
	if kind == "revision" && selected.Dir {
		m.status = "Select a file to inspect its versions"
		return nil
	}
	backend, ok := target.backend.(managementBackend)
	if !ok {
		m.status = "File management is unavailable for this backend"
		return nil
	}
	s := &tuiManagement{kind: kind, stage: "list", entry: selected, backend: backend, choices: list.New(nil, list.NewDefaultDelegate(), 76, 14)}
	s.choices.DisableQuitKeybindings()
	if kind == "recycle" {
		s.choices.Title = "Recycle bin • " + tuiText(m.location.path)
	} else {
		s.choices.Title = "Versions • " + tuiText(selected.Name)
	}
	m.management, m.mode = s, managementMode
	m.status, m.statusError = "", false
	if kind == "delete" {
		s.stage, s.action = "confirm", "delete"
		return nil
	}
	return m.managementOperation("ls")
}

func (m *tuiModel) managementOperation(action string) tea.Cmd {
	task := m.beginOperation("File management: " + action)
	s := *m.management
	return tea.Batch(m.spinner.Tick, task.command(func(ctx context.Context, id int) tea.Msg {
		msg := managementMsg{id: id, action: action}
		switch action {
		case "ls":
			if s.kind == "recycle" {
				msg.entries, msg.err = s.backend.RecycleList(ctx, vfs.LibraryID(s.entry.ID))
			} else {
				msg.versions, msg.err = s.backend.Revisions(ctx, s.entry)
			}
		case "delete":
			msg.err = s.backend.Delete(ctx, s.entry)
		case "restore", "restore-rename":
			_, msg.err = s.backend.RecycleRestore(ctx, s.recycled.ID, action == "restore-rename")
		case "remove":
			msg.err = s.backend.RecycleDelete(ctx, s.recycled.ID)
		case "empty":
			_, msg.err = anyshare.RecycleEmpty(ctx, s.backend, s.snapshot, nil)
		case "restore-revision":
			msg.err = s.backend.RestoreRevision(ctx, s.entry, s.revision.Rev)
		}
		return msg
	}))
}

func (m *tuiModel) managementResult(msg managementMsg) tea.Cmd {
	if msg.id != m.operation {
		return nil
	}
	m.stopOperation()
	if m.quitting {
		return tea.Quit
	}
	s := m.management
	if msg.err != nil {
		if msg.action == "restore" && errors.Is(msg.err, anyshare.ErrExists) {
			s.stage, s.action = "confirm", "restore-rename"
			m.status = "The original name is occupied. Review restoring under a new name."
			return nil
		}
		m.setError(m.activity, msg.err)
		if msg.action == "delete" {
			m.mode, m.management = browseMode, nil
		} else {
			s.stage = "list"
		}
		// Empty can fail after some entries were deleted; a later refresh must
		// keep the directory metadata consistent when leaving this dialog.
		if msg.action == "empty" {
			s.dirty = true
		}
		return nil
	}
	if msg.action == "delete" {
		m.mode, m.management = browseMode, nil
		status := "Deleted " + tuiText(s.entry.Name) + " into the recycle bin"
		cmd := m.load(m.currentLocation(), m.history)
		m.status = status
		return cmd
	}
	if msg.action != "ls" {
		s.stage, s.dirty = "list", true
		cmd := m.managementOperation("ls")
		m.status = "Completed " + msg.action
		return cmd
	}
	items := []list.Item{}
	for _, entry := range msg.entries {
		items = append(items, recycleItem{entry, m.sizes})
	}
	for _, revision := range msg.versions {
		items = append(items, revisionItem{revision, m.sizes})
	}
	s.choices.ResetFilter()
	s.choices.Select(0)
	s.stage = "list"
	if m.status == "" {
		m.status = fmt.Sprintf("Loaded %d entries", len(items))
	}
	return s.choices.SetItems(items)
}

func (m *tuiModel) closeManagement() tea.Cmd {
	dirty := m.management.dirty
	m.mode, m.management = browseMode, nil
	if dirty {
		return m.load(m.currentLocation(), m.history)
	}
	return nil
}

func (m *tuiModel) managementKey(msg tea.KeyPressMsg) tea.Cmd {
	s := m.management
	k := msg.String()
	if s.stage == "confirm" {
		if k == "q" {
			return m.quit()
		}
		switch confirmKey(k) {
		case confirmationAccepted:
			return m.managementOperation(s.action)
		case confirmationCancelled:
			if s.kind == "delete" {
				return m.closeManagement()
			}
			s.stage = "list"
			m.status = "Operation cancelled"
		}
		return nil
	}
	if s.choices.SettingFilter() {
		var cmd tea.Cmd
		s.choices, cmd = s.choices.Update(msg)
		return cmd
	}
	if k == "q" {
		return m.quit()
	}
	if k == "esc" && s.choices.FilterState() == list.Unfiltered {
		return m.closeManagement()
	}
	if k == "r" {
		return m.managementOperation("ls")
	}
	if k == "e" && s.kind == "recycle" {
		s.snapshot = nil
		// Empty the entire loaded snapshot, including rows hidden by a filter.
		for _, item := range s.choices.Items() {
			if entry, ok := item.(recycleItem); ok {
				s.snapshot = append(s.snapshot, entry.RecycleEntry)
			}
		}
		if len(s.snapshot) == 0 {
			m.status = "Recycle bin is empty"
			return nil
		}
		s.stage, s.action = "confirm", "empty"
		return nil
	}
	if k == "enter" || k == "delete" {
		switch item := s.choices.SelectedItem().(type) {
		case recycleItem:
			s.recycled = item.RecycleEntry
			s.stage, s.action = "confirm", "restore"
			if k == "delete" {
				s.action = "remove"
			}
		case revisionItem:
			if k == "enter" {
				s.revision = item.Revision
				s.stage, s.action = "confirm", "restore-revision"
			}
		}
		return nil
	}
	var cmd tea.Cmd
	s.choices, cmd = s.choices.Update(msg)
	return cmd
}

func (s *tuiManagement) view(sizes sizeFormat) string {
	if s.stage != "confirm" {
		keys := "Enter restore version • r refresh • / filter • Esc back • q quit"
		if s.kind == "recycle" {
			keys = "Enter restore • Delete permanently remove • e empty all listed objects\nr refresh • / filter • Esc back • q quit"
		}
		return s.choices.View() + "\n" + keys
	}
	text := ""
	switch s.action {
	case "delete":
		text = "Delete " + tuiText(s.entry.Name) + " into the recycle bin?\n" + tuiText(s.entry.ID)
		if s.entry.Dir {
			text += "\nThis deletes the entire directory subtree."
		}
	case "restore", "restore-rename":
		text = "Restore " + tuiText(s.recycled.Name) + " to " + tuiText(s.recycled.Path) + "?"
		if s.action == "restore-rename" {
			text += "\nA new name will be chosen by the server; both objects will be kept."
		}
	case "remove":
		text = "Permanently remove " + tuiText(s.recycled.Name) + "?\n" + tuiText(s.recycled.ID) + "\nThis cannot be undone, including any directory contents."
	case "empty":
		text = fmt.Sprintf("Permanently remove all %d loaded recycle objects?\nIncludes entries hidden by filtering. This cannot be undone.", len(s.snapshot))
	case "restore-revision":
		text = "Restore " + tuiText(s.entry.Name) + " to version " + tuiText(s.revision.Rev) + "?\n" + displayTime(s.revision.Modified) + " • " + sizes.Value(s.revision.Size) + "\nThis changes the current remote file contents."
	}
	return confirmView(text, "confirm")
}

func (s *tuiManagement) layout(width, height int) {
	s.choices.SetSize(max(width-4, 16), max(height-12, 4))
}

func (m *tuiModel) toggleFastUpload() {
	m.service.Fast = !m.service.Fast
	state := "off"
	if m.service.Fast {
		state = "on"
	}
	m.status = "Instant upload " + state + ": new files only; non-matches use ordinary upload"
}

func fastUploadLabel(enabled bool) string {
	if enabled {
		return "on (new files only)"
	}
	return "off"
}
