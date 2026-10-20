package tui

import (
	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"context"
	"errors"
	"github.com/unofficialpku/disk/internal/anyshare"
)

type recipientItem struct{ anyshare.Recipient }

func (i recipientItem) Title() string { return tuiText(i.Name) }
func (i recipientItem) Description() string {
	return tuiText(i.Type + " " + i.Account + " " + i.Path + " (" + i.ID + ")")
}
func (i recipientItem) FilterValue() string { return i.Name + " " + i.Account + " " + i.ID }

type recipientPicker struct {
	list   list.Model
	chosen *anyshare.Recipient
}

func (p *recipientPicker) Init() tea.Cmd { return nil }
func (p *recipientPicker) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if size, ok := msg.(tea.WindowSizeMsg); ok {
		p.list.SetSize(max(20, size.Width-4), max(6, size.Height-4))
	}
	if key, ok := msg.(tea.KeyPressMsg); ok && !p.list.SettingFilter() {
		switch key.String() {
		case "enter":
			if item, ok := p.list.SelectedItem().(recipientItem); ok {
				p.chosen = &item.Recipient
				return p, tea.Quit
			}
		case "esc", "q", "ctrl+c":
			return p, tea.Quit
		}
	}
	var cmd tea.Cmd
	p.list, cmd = p.list.Update(msg)
	return p, cmd
}
func (p *recipientPicker) View() tea.View { return tea.NewView(p.list.View()) }

func PickRecipient(ctx context.Context, recipients []anyshare.Recipient) (anyshare.Recipient, error) {
	if len(recipients) == 0 {
		return anyshare.Recipient{}, errors.New("no matching recipients; find an explicit ID with 'share search NAME'")
	}
	input, output, err := openTerminal()
	if err != nil {
		return anyshare.Recipient{}, errors.New("interactive terminal unavailable; provide --user ID or --department ID")
	}
	defer closeTerminal(input, output)
	items := make([]list.Item, len(recipients))
	for i, recipient := range recipients {
		items[i] = recipientItem{recipient}
	}
	picker := &recipientPicker{list: list.New(items, list.NewDefaultDelegate(), 80, 16)}
	picker.list.Title = "Select sharing recipient (Enter selects, Esc cancels)"
	_, err = tea.NewProgram(picker, tea.WithContext(ctx), tea.WithInput(input), tea.WithOutput(output)).Run()
	if err != nil {
		return anyshare.Recipient{}, err
	}
	if picker.chosen == nil {
		return anyshare.Recipient{}, context.Canceled
	}
	return *picker.chosen, nil
}
