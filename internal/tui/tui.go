package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/filepicker"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/unofficialpku/disk/internal/localfile"
	"github.com/unofficialpku/disk/internal/transfer"
	"golang.org/x/term"
)

type browserBackend interface {
	Roots(context.Context) ([]anyshare.Entry, error)
	List(context.Context, string) ([]anyshare.Entry, error)
}
type remoteItem struct {
	entry anyshare.Entry
	sizes sizeFormat
}

func (i remoteItem) Title() string {
	if i.entry.Dir {
		return "[DIR] " + tuiText(i.entry.Name)
	}
	return tuiText(i.entry.Name)
}
func (i remoteItem) Description() string {
	if i.entry.Dir {
		return "Folder"
	}
	text := i.sizes.Value(i.entry.Size)
	if !i.entry.Modified.IsZero() {
		text += "  " + i.entry.Modified.Local().Format("2006-01-02 15:04")
	}
	return text
}
func (i remoteItem) FilterValue() string { return i.entry.Name }

type browserLocation struct {
	entry    *anyshare.Entry
	path     string
	selected string
}
type browserMode int

const (
	browseMode browserMode = iota
	pickerMode
	pathMode
	confirmMode
	sharingMode
	managementMode
)

type directoryMsg struct {
	id       int
	location browserLocation
	history  []browserLocation
	entries  []anyshare.Entry
	err      error
}
type transferMsg struct {
	id        int
	direction string
	result    transfer.Result
	err       error
}
type progressTickMsg struct{ id int }
type progressMeter struct {
	mu    sync.Mutex
	value transfer.Progress
}

func (m *progressMeter) Update(p transfer.Progress) { m.mu.Lock(); m.value = p; m.mu.Unlock() }
func (m *progressMeter) Value() transfer.Progress   { m.mu.Lock(); defer m.mu.Unlock(); return m.value }

type tuiModel struct {
	ctx                 context.Context
	backend             browserBackend
	service             transfer.Service
	account, server     string
	sizes               sizeFormat
	timeout             time.Duration
	width, height       int
	remote              list.Model
	picker              filepicker.Model
	input               textinput.Model
	spinner             spinner.Model
	progress            progress.Model
	location            browserLocation
	history             []browserLocation
	mode                browserMode
	inputDirection      string
	source, destination string
	archive, viaArchive bool
	status              string
	statusError         bool
	busy, quitting      bool
	asyncTasks
	meter         *progressMeter
	progressState transfer.Progress
	started       time.Time
	activity      string
	conflict      string
	sharing       *tuiSharing
	management    *tuiManagement
}

func Interactive(in io.Reader, out io.Writer) bool {
	input, inOK := in.(*os.File)
	output, outOK := out.(*os.File)
	return inOK && outOK && term.IsTerminal(int(input.Fd())) && term.IsTerminal(int(output.Fd()))
}
func Run(ctx context.Context, backend browserBackend, account, server string, timeout time.Duration, sizes sizeFormat, in io.Reader, out io.Writer) error {
	if !Interactive(in, out) {
		return errors.New("tui requires an interactive terminal")
	}
	uiCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	model := newTUIModel(uiCtx, backend, account, server, timeout)
	model.sizes = sizes
	_, err := tea.NewProgram(model, tea.WithContext(uiCtx), tea.WithInput(in), tea.WithOutput(out)).Run()
	model.asyncTasks.stop()
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
func newTUIModel(ctx context.Context, backend browserBackend, account, server string, timeout time.Duration) *tuiModel {
	delegate := list.NewDefaultDelegate()
	delegate.SetSpacing(0)
	remote := list.New(nil, delegate, 80, 16)
	remote.Title = "Remote files"
	remote.SetStatusBarItemName("entry", "entries")
	remote.DisableQuitKeybindings()
	remote.AdditionalShortHelpKeys = func() []key.Binding {
		return []key.Binding{
			key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "open")),
			key.NewBinding(key.WithKeys("backspace"), key.WithHelp("backspace", "parent")),
			key.NewBinding(key.WithKeys("u"), key.WithHelp("u", "upload")),
			key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "download")),
			key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "share")),
		}
	}
	remote.AdditionalFullHelpKeys = func() []key.Binding {
		return []key.Binding{
			key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "open folder")),
			key.NewBinding(key.WithKeys("backspace", "h"), key.WithHelp("backspace/h", "parent")),
			key.NewBinding(key.WithKeys("u"), key.WithHelp("u", "upload")),
			key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "download")),
			key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "download folder ZIP")),
			key.NewBinding(key.WithKeys("v"), key.WithHelp("v", "download folder via ZIP")),
			key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "extract remote archive")),
			key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "share / links / revoke")),
			key.NewBinding(key.WithKeys("delete"), key.WithHelp("delete", "delete into recycle bin")),
			key.NewBinding(key.WithKeys("b"), key.WithHelp("b", "library recycle bin")),
			key.NewBinding(key.WithKeys("H"), key.WithHelp("H", "file version history")),
			key.NewBinding(key.WithKeys("F"), key.WithHelp("F", "toggle instant upload")),
			key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
			key.NewBinding(key.WithKeys("g"), key.WithHelp("g", "root")),
			key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel operation")),
			key.NewBinding(key.WithKeys("q"), key.WithHelp("q", "quit")),
		}
	}
	picker := filepicker.New()
	cwd, err := os.Getwd()
	if err == nil {
		picker.CurrentDirectory = cwd
	}
	picker.DirAllowed = true
	picker.KeyMap.Open.SetKeys("l", "right", "enter", "space")
	picker.KeyMap.Select.SetKeys("space")
	picker.ShowHidden = true
	picker.ShowSize = true
	picker.ShowPermissions = false
	picker.SetHeight(12)
	input := textinput.New()
	input.Prompt = "Path > "
	input.SetWidth(70)
	spin := spinner.New()
	spin.Spinner = spinner.Dot
	bar := progress.New(progress.WithDefaultBlend())
	bar.SetWidth(45)
	transfers, _ := backend.(transfer.Backend)
	return &tuiModel{ctx: ctx, backend: backend, service: transfer.Service{Backend: transfers}, account: account, server: server, timeout: timeout,
		width: 80, height: 24, remote: remote, picker: picker, input: input, spinner: spin, progress: bar, location: browserLocation{path: "/"}}
}
func (m *tuiModel) Init() tea.Cmd { return m.load(m.location, nil) }
func (m *tuiModel) load(location browserLocation, history []browserLocation) tea.Cmd {
	if m.busy {
		return nil
	}
	task := m.beginOperation("Loading directory")
	return tea.Batch(m.spinner.Tick, task.command(func(ctx context.Context, id int) tea.Msg {
		var entries []anyshare.Entry
		var err error
		if location.entry == nil {
			entries, err = m.backend.Roots(ctx)
		} else {
			entries, err = m.backend.List(ctx, location.entry.ID)
		}
		return directoryMsg{id, location, history, entries, err}
	}))
}
func (m *tuiModel) selected() *anyshare.Entry {
	if item, ok := m.remote.SelectedItem().(remoteItem); ok {
		entry := item.entry
		return &entry
	}
	return nil
}
func (m *tuiModel) currentLocation() browserLocation {
	location := m.location
	if entry := m.selected(); entry != nil {
		location.selected = entry.ID
	}
	return location
}
func (m *tuiModel) startTransfer(direction, source, destination string, overwrite bool) tea.Cmd {
	selector := source
	if direction == "upload" {
		selector = destination
	}
	target, err := m.target(selector)
	if err != nil {
		m.setError("Select transfer account", err)
		return nil
	}
	remoteSource, remoteDestination := source, destination
	if direction == "upload" {
		remoteDestination = target.id
	} else {
		remoteSource = target.id
	}
	if direction == "extract" {
		remoteDestination, err = target.extractionDestination(destination)
		if err != nil {
			m.setError("Select extraction destination", err)
			return nil
		}
	}
	task := m.beginOperation(strings.ToUpper(direction[:1]) + direction[1:])
	m.source = source
	m.destination = destination
	m.mode = browseMode
	m.started = time.Now()
	meter := &progressMeter{}
	m.meter = meter
	m.progressState = transfer.Progress{}
	service := m.service
	service.Backend = target.backend
	service.Archive, service.ViaArchive = m.archive, m.viaArchive
	return tea.Batch(m.spinner.Tick, progressTick(task.id), task.command(func(ctx context.Context, id int) tea.Msg {
		var result transfer.Result
		var err error
		if direction == "upload" {
			result, err = service.Upload(ctx, source, remoteDestination, overwrite, meter.Update)
		} else if direction == "extract" {
			result, err = service.Extract(ctx, remoteSource, remoteDestination, meter.Update)
		} else {
			result, err = service.Download(ctx, remoteSource, destination, overwrite, meter.Update)
		}
		return transferMsg{id, direction, result, err}
	}))
}
func progressTick(id int) tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg { return progressTickMsg{id} })
}
func (m *tuiModel) stopOperation() {
	m.asyncTasks.stop()
	m.busy = false
}
func (m *tuiModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	defer m.layout()
	switch msg := message.(type) {
	case managementMsg:
		return m, m.managementResult(msg)
	case sharingMsg:
		return m, m.sharingResult(msg)
	case tea.WindowSizeMsg:
		m.width = max(msg.Width, 20)
		m.height = max(msg.Height, 8)
		m.remote.SetSize(max(m.width-4, 16), max(m.height-7, 4))
		m.picker.SetHeight(max(m.height-10, 3))
		m.input.SetWidth(max(m.width-14, 8))
		m.progress.SetWidth(max(min(m.width-8, 60), 12))
	case directoryMsg:
		if msg.id != m.operation {
			return m, nil
		}
		m.stopOperation()
		if m.quitting {
			return m, tea.Quit
		}
		if msg.err != nil {
			m.setError("Load directory", msg.err)
			return m, nil
		}
		m.location = msg.location
		m.history = msg.history
		sort.SliceStable(msg.entries, func(i, j int) bool {
			if msg.entries[i].Dir != msg.entries[j].Dir {
				return msg.entries[i].Dir
			}
			return strings.ToLower(msg.entries[i].Name) < strings.ToLower(msg.entries[j].Name)
		})
		items := make([]list.Item, len(msg.entries))
		selected := 0
		for i, entry := range msg.entries {
			items[i] = remoteItem{entry: entry, sizes: m.sizes}
			if entry.ID == msg.location.selected {
				selected = i
			}
		}
		m.remote.ResetFilter()
		cmd := m.remote.SetItems(items)
		m.remote.Select(selected)
		if m.status == "" {
			m.status = fmt.Sprintf("Loaded %d entries", len(items))
		} else {
			m.status += " • directory refreshed"
		}
		return m, cmd
	case transferMsg:
		if msg.id != m.operation {
			return m, nil
		}
		m.stopOperation()
		if m.meter != nil {
			m.progressState = m.meter.Value()
		}
		if m.quitting {
			return m, tea.Quit
		}
		if msg.err != nil {
			if (errors.Is(msg.err, anyshare.ErrExists) || errors.Is(msg.err, os.ErrExist)) && msg.direction != "extract" {
				m.mode = confirmMode
				m.inputDirection = msg.direction
				m.conflict = msg.err.Error()
				return m, nil
			}
			m.setError(m.activity, msg.err)
			return m, nil
		}
		m.status = fmt.Sprintf("%s complete: %s (%s)", m.activity, tuiText(msg.result.Entry.Name), m.sizes.Value(msg.result.Bytes))
		m.statusError = false
		if msg.result.Entry.Dir {
			m.status += fmt.Sprintf(" • %d files, %d directories", msg.result.Files, msg.result.Dirs)
		}
		if msg.result.FastFiles > 0 {
			m.status += fmt.Sprintf(" • %d instant upload(s)", msg.result.FastFiles)
		}
		if msg.direction == "upload" || msg.direction == "extract" {
			status := m.status
			location := m.currentLocation()
			location.selected = msg.result.Entry.ID
			cmd := m.load(location, m.history)
			m.status = status
			return m, cmd
		}
		m.status += " → " + tuiText(msg.result.Local)
		return m, nil
	case progressTickMsg:
		if m.busy && msg.id == m.operation && m.meter != nil {
			m.progressState = m.meter.Value()
			return m, progressTick(msg.id)
		}
	case spinner.TickMsg:
		if m.busy {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd
		}
	case tea.KeyPressMsg:
		k := msg.String()
		if k == "ctrl+c" {
			return m, m.quit()
		}
		if m.mode == browseMode && m.remote.SettingFilter() {
			var cmd tea.Cmd
			m.remote, cmd = m.remote.Update(msg)
			return m, cmd
		}
		if k == "q" && m.mode != pathMode && (m.mode != sharingMode && m.mode != managementMode || m.busy) {
			return m, m.quit()
		}
		if m.busy {
			if k == "esc" {
				m.asyncTasks.stop()
				m.status = "Cancelling…"
			}
			return m, nil
		}
		if m.mode == sharingMode {
			return m, m.sharingKey(msg)
		}
		if m.mode == managementMode {
			return m, m.managementKey(msg)
		}
		switch m.mode {
		case pickerMode:
			if k == "F" {
				m.toggleFastUpload()
				return m, nil
			}
			if k == "esc" {
				m.mode = browseMode
				return m, nil
			}
			if k == "p" {
				m.inputDirection = "upload"
				m.mode = pathMode
				m.input.SetValue("")
				m.input.Placeholder = "Local file or directory path (supports ~/...)"
				return m, m.input.Focus()
			}
			var cmd tea.Cmd
			m.picker, cmd = m.picker.Update(msg)
			if selected, file := m.picker.DidSelectFile(msg); selected {
				return m, m.startTransfer("upload", file, m.location.entry.ID, false)
			}
			return m, cmd
		case pathMode:
			if k == "ctrl+f" && m.inputDirection == "upload" {
				m.toggleFastUpload()
				return m, nil
			}
			if k == "esc" {
				m.mode = browseMode
				m.input.Blur()
				return m, nil
			}
			if k == "enter" {
				value := strings.TrimSpace(m.input.Value())
				if value == "" {
					m.status = "Enter a destination or source path"
					return m, nil
				}
				if m.inputDirection != "extract" {
					value = expandTUIPath(value)
				}
				m.input.Blur()
				if m.inputDirection == "upload" {
					return m, m.startTransfer("upload", value, m.location.entry.ID, false)
				}
				return m, m.startTransfer(m.inputDirection, m.source, value, false)
			}
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return m, cmd
		case confirmMode:
			switch confirmKey(k) {
			case confirmationAccepted:
				return m, m.startTransfer(m.inputDirection, m.source, m.destination, true)
			case confirmationCancelled:
				m.mode = browseMode
				m.status = "Overwrite cancelled"
				return m, nil
			}
			return m, nil
		default:
			switch k {
			case "delete":
				return m, m.openManagement("delete")
			case "b":
				return m, m.openManagement("recycle")
			case "H":
				return m, m.openManagement("revision")
			case "F":
				m.toggleFastUpload()
				return m, nil
			case "s":
				return m, m.openSharing()
			case "enter", "l":
				entry := m.selected()
				if entry == nil {
					return m, nil
				}
				if !entry.Dir {
					m.status = "Press d to download the selected file"
					return m, nil
				}
				current := m.currentLocation()
				history := append(append([]browserLocation{}, m.history...), current)
				return m, m.load(browserLocation{entry: entry, path: strings.TrimRight(m.location.path, "/") + "/" + entry.Name}, history)
			case "backspace", "h", "left":
				if len(m.history) == 0 {
					m.status = "Already at root"
					return m, nil
				}
				last := len(m.history) - 1
				return m, m.load(m.history[last], m.history[:last])
			case "g":
				return m, m.load(browserLocation{path: "/"}, nil)
			case "r":
				return m, m.load(m.currentLocation(), m.history)
			case "u":
				if m.location.entry == nil || m.virtual(m.location.entry.ID) {
					m.status = "Open a library or folder before uploading"
					return m, nil
				}
				m.mode = pickerMode
				m.archive, m.viaArchive = false, false
				m.status = ""
				return m, m.picker.Init()
			case "d", "a", "v", "x":
				entry := m.selected()
				if entry == nil {
					return m, nil
				}
				if m.virtual(entry.ID) {
					m.status = "Open an account's library before file operations"
					return m, nil
				}
				if (k == "a" || k == "v") && !entry.Dir {
					m.status = "Select a folder for ZIP downloads"
					return m, nil
				}
				m.source = entry.ID
				m.inputDirection = "download"
				m.mode = pathMode
				m.archive, m.viaArchive = k == "a", k == "v"
				value := entry.Name
				if m.archive {
					value += ".zip"
				}
				if k == "x" {
					if entry.Dir || m.location.entry == nil {
						m.status = "Select a remote archive file to extract"
						m.mode = browseMode
						return m, nil
					}
					m.inputDirection = "extract"
					value = m.location.path
				}
				m.input.SetValue(value)
				m.input.CursorEnd()
				m.status = ""
				return m, m.input.Focus()
			}
			var cmd tea.Cmd
			m.remote, cmd = m.remote.Update(msg)
			return m, cmd
		}
	}
	// List filtering and picker directory reads emit their own component messages.
	if m.mode == pickerMode {
		var cmd tea.Cmd
		m.picker, cmd = m.picker.Update(message)
		return m, cmd
	}
	if m.mode == pathMode {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(message)
		return m, cmd
	}
	if m.mode == sharingMode {
		if len(m.sharing.fields) > 0 && m.sharing.stage != "confirm" {
			_, cmd := m.sharing.inputFields.update(message)
			return m, cmd
		}
		var cmd tea.Cmd
		m.sharing.choices, cmd = m.sharing.choices.Update(message)
		return m, cmd
	}
	if m.mode == managementMode {
		var cmd tea.Cmd
		m.management.choices, cmd = m.management.choices.Update(message)
		return m, cmd
	}
	var cmd tea.Cmd
	m.remote, cmd = m.remote.Update(message)
	return m, cmd
}
func (m *tuiModel) quit() tea.Cmd {
	if m.busy {
		m.quitting = true
		m.asyncTasks.stop()
		m.status = "Cancelling before exit…"
		return nil
	}
	return tea.Quit
}

func (m *tuiModel) layout() {
	if m.sharing != nil {
		m.sharing.layout(m.width, m.height)
	}
	if m.management != nil {
		m.management.layout(m.width, m.height)
	}
	overhead := 8
	if m.busy {
		overhead += 2
		if m.meter != nil {
			overhead += 3
		}
	}
	if m.status != "" {
		overhead += 1
	}
	m.remote.SetSize(max(m.width-4, 16), max(m.height-overhead, 4))
	m.picker.SetHeight(max(m.height-12, 3))
	m.input.SetWidth(max(m.width-14, 8))
	m.progress.SetWidth(max(min(m.width-8, 60), 12))
}
func (m *tuiModel) setError(action string, err error) {
	if errors.Is(err, context.Canceled) {
		m.status = action + " cancelled"
		m.statusError = false
	} else {
		m.status = action + ": " + tuiText(err.Error())
		m.statusError = true
	}
}
func (m *tuiModel) View() tea.View {
	accent := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("63"))
	muted := lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	account, server := m.accountContext()
	header := ansi.Truncate(accent.Render("PKUDisk")+"  Account: "+tuiText(account), m.width-4, "…") + "\n" + muted.Render(ansi.Truncate(tuiText(server), m.width-4, "…")) + "\n" + ansi.Truncate("Remote: "+tuiText(m.location.path), m.width-4, "…") + "\n"
	var content string
	switch m.mode {
	case managementMode:
		content = m.management.view(m.sizes)
	case sharingMode:
		content = m.sharing.view()
	case pickerMode:
		content = accent.Render("Choose a local file or directory to upload") + "\nLocal: " + tuiText(m.picker.CurrentDirectory) + "\nInstant upload: " + fastUploadLabel(m.service.Fast) + "\n\n" + m.picker.View() + "\n\n↑/↓ move • Enter open folder • Space select • Backspace parent\np type path • F toggle instant upload • Esc cancel"
	case pathMode:
		label := "Enter a local path"
		mode := "direct transfer"
		if m.archive {
			mode = "server ZIP download"
		} else if m.viaArchive {
			mode = "ZIP transfer + extraction"
		}
		if m.inputDirection == "extract" {
			label = "Enter a remote destination (new folder)"
			mode = "Server extraction may omit empty folders; cancellation only stops waiting"
		}
		if m.inputDirection == "upload" {
			mode += " • instant upload " + fastUploadLabel(m.service.Fast) + " (Ctrl+F toggles)"
		}
		content = accent.Render(label) + "\nMode: " + mode + "\n" + m.input.View() + "\n\nEnter continue • Esc cancel"
	case confirmMode:
		content = confirmView(accent.Render("Overwrite files / merge directory?")+"\n\n"+tuiText(m.conflict), "replace matching files; keep unrelated entries")
	default:
		content = m.remote.View()
	}
	if m.busy {
		content += "\n" + m.spinner.View() + " " + m.activity + "  Esc: cancel • q: cancel and quit"
		if m.meter != nil {
			state := m.progressState
			percent := 0.0
			if state.Total > 0 {
				percent = float64(state.Done) / float64(state.Total)
			}
			if state.Phase == transfer.Preparing || state.Phase == transfer.Extracting {
				percent = float64(state.Percent) / 100
			}
			if state.Phase == transfer.Complete {
				percent = 1
			} else {
				percent = min(percent, 0.99)
			}
			phase := ""
			if state.Phase == transfer.Finalizing {
				phase = "  finishing..."
			}
			if state.Phase == transfer.Hashing {
				phase = "  hashing locally..."
			}
			detail := m.sizes.Value(state.Done) + " / " + m.sizes.Value(state.Total) + phase
			if state.Total < 0 {
				detail = m.sizes.Value(state.Done) + " (total unknown)"
			}
			if state.Phase == transfer.Preparing || state.Phase == transfer.Extracting {
				detail = fmt.Sprintf("%s (%d%%)", state.Phase, state.Percent)
			}
			content += "\n" + m.progress.ViewAs(percent) + "\n" + detail
			elapsed := time.Since(m.started).Seconds()
			if elapsed > 0 && state.Done > 0 && state.Phase == transfer.Transferring {
				content += "  " + m.sizes.Value(int64(float64(state.Done)/elapsed)) + "/s"
			}
		}
	}
	if m.status != "" {
		style := muted
		if m.statusError {
			style = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
		}
		content += "\n" + style.Render(ansi.Truncate(m.status, m.width-4, "…"))
	}
	// Width constrains long names and errors; widgets own pagination/scrolling.
	view := tea.NewView(lipgloss.NewStyle().Width(max(m.width-4, 16)).Padding(1, 2).Render(header + "\n" + content))
	view.AltScreen = true
	return view
}
func expandTUIPath(value string) string { return localfile.ExpandPath(value) }
