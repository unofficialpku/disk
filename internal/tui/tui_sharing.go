package tui

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/unofficialpku/disk/internal/anyshare"
)

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

type shareAction struct{ action, title, description string }

func (i shareAction) Title() string       { return i.title }
func (i shareAction) Description() string { return i.description }
func (i shareAction) FilterValue() string { return i.title }

type shareLinkItem struct{ anyshare.ShareLink }

func (i shareLinkItem) Title() string { return tuiText(i.ShareLink.Title) }
func (i shareLinkItem) Description() string {
	return tuiText(strings.Join(i.Permissions, ",") + " • " + expiration(i.ExpiresAt) + " • " + i.ID)
}
func (i shareLinkItem) FilterValue() string { return i.ShareLink.Title + " " + i.ID }

type sharePermissionItem struct{ anyshare.SharePermission }

func (i sharePermissionItem) Title() string { return tuiText(i.Type + " " + i.Name) }
func (i sharePermissionItem) Description() string {
	source := "direct"
	if i.InheritedFrom != "" {
		source = "inherited: " + i.InheritedPath
	}
	return tuiText(strings.Join(i.Allow, ",") + " • " + expiration(i.ExpiresAt) + " • " + source)
}
func (i sharePermissionItem) FilterValue() string { return i.Name + " " + i.ID }

type tuiSharing struct {
	entry   anyshare.Entry
	stage   string
	choices list.Model
	inputFields
	recipient anyshare.Recipient
	options   anyshare.ShareOptions
	link      anyshare.ShareLink
	confirm   string
	backend   sharingBackend
}

type sharingMsg struct {
	id         int
	action     string
	link       anyshare.ShareLink
	links      []anyshare.ShareLink
	acl        anyshare.ShareACL
	recipients []anyshare.Recipient
	err        error
}

func (m *tuiModel) openSharing() tea.Cmd {
	entry := m.selected()
	if entry == nil {
		return nil
	}
	target, err := m.target(entry.ID)
	if err != nil {
		m.setError("Select sharing account", err)
		return nil
	}
	backend, ok := target.backend.(sharingBackend)
	if !ok {
		m.status = "Sharing is unavailable for this backend"
		return nil
	}
	selected := *entry
	selected.ID = target.id
	m.sharing = &tuiSharing{entry: selected, backend: backend, choices: list.New(nil, list.NewDefaultDelegate(), 76, 14)}
	m.sharing.choices.DisableQuitKeybindings()
	m.mode = sharingMode
	return m.sharingMenu()
}

func (m *tuiModel) sharingMenu() tea.Cmd {
	s := m.sharing
	s.stage, s.fields, s.field = "menu", nil, 0
	s.choices.ResetFilter()
	s.choices.Select(0)
	s.choices.Title = "Share " + tuiText(s.entry.Name)
	m.status, m.statusError = "", false
	return s.choices.SetItems([]list.Item{
		shareAction{"internal", "Internal link", "Requires login and existing permissions; creates no access grant"},
		shareAction{"grant", "Grant internal access", "Find a user/department, choose permissions, then review"},
		shareAction{"permissions", "Internal permissions / revoke", "Inspect direct and inherited access; remove a direct grant"},
		shareAction{"external", "Create external link", "Anonymous link with optional pickup code, validity and visit limit"},
		shareAction{"links", "External links / revoke", "Inspect links, copy URLs or revoke a selected link"},
	})
}

func (m *tuiModel) sharingForm(stage string, labels ...string) tea.Cmd {
	s := m.sharing
	s.stage, s.field, s.fields = stage, 0, nil
	for _, label := range labels {
		input := newTextInput(label+" > ", max(m.width-8, 12), 1024, false)
		s.fields = append(s.fields, input)
	}
	if stage == "external" {
		s.fields[0].EchoMode, s.fields[0].EchoCharacter = textinput.EchoPassword, '•'
	}
	return s.focus(0)
}

func (m *tuiModel) sharingOperation(action, query string) tea.Cmd {
	task := m.beginOperation("Sharing: " + action)
	s := *m.sharing
	backend := s.backend
	return tea.Batch(m.spinner.Tick, task.command(func(ctx context.Context, id int) tea.Msg {
		msg := sharingMsg{id: id, action: action}
		switch action {
		case "internal":
			msg.link, msg.err = backend.InternalLink(ctx, s.entry)
		case "search":
			msg.recipients, msg.err = backend.SearchRecipients(ctx, query, 50)
		case "grant":
			msg.err = backend.GrantShare(ctx, s.entry, []anyshare.Recipient{s.recipient}, s.options)
			if msg.err == nil {
				msg.link, msg.err = backend.InternalLink(ctx, s.entry)
				if msg.err != nil {
					msg.err = fmt.Errorf("permissions granted; internal link failed (retry Internal link): %w", msg.err)
				}
			}
		case "permissions":
			msg.acl, msg.err = backend.SharePermissions(ctx, s.entry.ID)
		case "revoke-permission":
			_, msg.err = backend.RevokeShare(ctx, s.entry.ID, []anyshare.Recipient{s.recipient})
		case "external":
			msg.link, msg.err = backend.CreateExternalLink(ctx, s.entry, s.options)
		case "links":
			msg.links, msg.err = backend.ShareLinks(ctx, s.entry, true)
		case "revoke-link":
			msg.err = backend.RemoveExternalLink(ctx, s.link.ID)
		}
		return msg
	}))
}

func (m *tuiModel) sharingResult(msg sharingMsg) tea.Cmd {
	if msg.id != m.operation {
		return nil
	}
	m.stopOperation()
	if m.quitting {
		return tea.Quit
	}
	s := m.sharing
	if msg.err != nil {
		if errors.Is(msg.err, anyshare.ErrPending) {
			cmd := m.sharingMenu()
			m.status = "Sharing request submitted for approval; changes are not active yet"
			return cmd
		}
		m.setError(m.activity, msg.err)
		if errors.Is(msg.err, anyshare.ErrPickupCodeRequired) {
			s.stage, s.field = "external", 0
			return s.focus(0)
		}
		return nil
	}
	items := []list.Item{}
	switch msg.action {
	case "search":
		for _, recipient := range msg.recipients {
			items = append(items, recipientItem{recipient})
		}
		s.stage, s.choices.Title = "recipients", "Choose a recipient"
	case "permissions":
		for _, permission := range msg.acl.Permissions {
			items = append(items, sharePermissionItem{permission})
		}
		s.stage, s.choices.Title = "permissions", "Direct and inherited permissions"
	case "links":
		for _, link := range msg.links {
			items = append(items, shareLinkItem{link})
		}
		s.stage, s.choices.Title = "links", "External links (Enter views, Delete revokes)"
	case "revoke-permission", "revoke-link":
		cmd := m.sharingMenu()
		m.status = "Direct grant removed; inherited access remains"
		if msg.action == "revoke-link" {
			m.status = "External link revoked"
		}
		return cmd
	default:
		return m.showSharingLink(msg.link)
	}
	s.choices.ResetFilter()
	s.choices.Select(0)
	return s.choices.SetItems(items)
}

func (m *tuiModel) showSharingLink(link anyshare.ShareLink) tea.Cmd {
	s := m.sharing
	s.link, s.stage = link, "result"
	input := textinput.New()
	input.Prompt = "URL > "
	input.SetWidth(max(m.width-14, 12))
	input.SetValue(link.URL)
	input.CursorStart()
	s.fields = []textinput.Model{input}
	return s.focus(0)
}

func (m *tuiModel) sharingKey(msg tea.KeyPressMsg) tea.Cmd {
	s := m.sharing
	k := msg.String()
	listStage := s.stage == "menu" || s.stage == "recipients" || s.stage == "permissions" || s.stage == "links"
	if k == "q" && listStage && !s.choices.SettingFilter() {
		return m.quit()
	}
	if k == "esc" && (!listStage || s.choices.FilterState() == list.Unfiltered) {
		if s.stage == "menu" {
			m.mode = browseMode
			return nil
		}
		return m.sharingMenu()
	}
	if s.stage == "query" || s.stage == "external" || s.stage == "grant-form" {
		action, cmd := s.inputFields.update(msg)
		if action == formSubmitted {
			if s.stage == "query" {
				return m.sharingOperation("search", strings.TrimSpace(s.fields[0].Value()))
			}
			if err := s.readOptions(); err != nil {
				m.setError("Sharing settings", err)
				return nil
			}
			s.confirm = "external"
			if s.stage == "grant-form" {
				s.confirm = "grant"
			}
			s.stage = "confirm"
		}
		return cmd
	}
	if s.stage == "confirm" {
		switch confirmKey(k) {
		case confirmationAccepted:
			return m.sharingOperation(s.confirm, "")
		case confirmationCancelled:
			return m.sharingMenu()
		}
		return nil
	}
	if s.stage == "result" {
		if k == "c" {
			m.status = "Copy requested through terminal clipboard (OSC52)"
			return tea.SetClipboard(s.link.URL)
		}
		if k == "left" || k == "right" || k == "home" || k == "end" {
			var cmd tea.Cmd
			s.fields[0], cmd = s.fields[0].Update(msg)
			return cmd
		}
		return nil
	}
	if !s.choices.SettingFilter() && (k == "enter" || k == "delete") {
		switch item := s.choices.SelectedItem().(type) {
		case shareAction:
			switch item.action {
			case "grant":
				return m.sharingForm("query", "Recipient name/account")
			case "external":
				return m.sharingForm("external", "Pickup code (optional)", "Expires in (blank / 168h / 0)", "Permissions (blank / preview,download)", "Visits (blank / -1 / positive)")
			default:
				return m.sharingOperation(item.action, "")
			}
		case recipientItem:
			s.recipient = item.Recipient
			return m.sharingForm("grant-form", "Permissions (blank / preview,download)", "Expires in (blank / 168h / 0)")
		case sharePermissionItem:
			if item.InheritedFrom != "" || item.Type != "user" && item.Type != "department" {
				m.status = "Select a direct user/department rule; inherited access must be changed at its source"
				return nil
			}
			s.recipient = anyshare.Recipient{ID: item.ID, Type: item.Type, Name: item.Name}
			s.confirm, s.stage = "revoke-permission", "confirm"
		case shareLinkItem:
			s.link = item.ShareLink
			if k == "delete" {
				s.confirm, s.stage = "revoke-link", "confirm"
			} else {
				return m.showSharingLink(item.ShareLink)
			}
		}
		return nil
	}
	var cmd tea.Cmd
	s.choices, cmd = s.choices.Update(msg)
	return cmd
}

func (s *tuiSharing) readOptions() error {
	opts := anyshare.ShareOptions{}
	permission, expiry := 0, 1
	if s.stage == "external" {
		opts.Code = s.fields[0].Value()
		permission, expiry = 2, 1
		if text := strings.TrimSpace(s.fields[3].Value()); text != "" {
			limit, err := strconv.ParseInt(text, 10, 64)
			if err != nil {
				return errors.New("visits must be -1 or a positive integer")
			}
			opts.Limit = &limit
		}
	}
	if text := strings.TrimSpace(s.fields[permission].Value()); text != "" {
		for _, value := range strings.Split(text, ",") {
			opts.Permissions = append(opts.Permissions, strings.TrimSpace(value))
		}
	}
	if text := strings.TrimSpace(s.fields[expiry].Value()); text != "" {
		duration, err := time.ParseDuration(text)
		if err != nil {
			return errors.New("expiry must be 0 or a duration such as 24h or 168h")
		}
		opts.ExpiresIn = &duration
	}
	if err := opts.Validate(); err != nil {
		return err
	}
	s.options = opts
	return nil
}

func (s *tuiSharing) layout(width, height int) {
	s.choices.SetSize(max(width-4, 16), max(height-10, 5))
	for i := range s.fields {
		s.fields[i].SetWidth(max(width-lipgloss.Width(s.fields[i].Prompt)-8, 8))
	}
}

func (s *tuiSharing) view() string {
	if s.stage == "result" {
		text := "Share " + tuiText(s.entry.Name) + "\n" + s.fields[0].View()
		if s.link.Type == "anonymous" {
			text += "\nAllow: " + strings.Join(s.link.Permissions, ",") + "\nExpires: " + expiration(s.link.ExpiresAt)
			visits := "unlimited"
			if s.link.LimitedTimes != -1 {
				visits = fmt.Sprint(s.link.LimitedTimes)
			}
			text += "\nVisits: " + visits + " • Phone verification: " + fmt.Sprint(s.link.VerifyMobile)
			if s.link.Code != "" {
				text += "\nPickup code: " + tuiText(s.link.Code)
			}
		} else {
			text += "\nInternal link: requires login and file permissions"
		}
		return text + "\n\nc copy URL (terminal must support OSC52) • ←/→ scroll URL • Esc back"
	}
	if s.stage == "confirm" {
		text := "Review " + s.confirm + ": " + tuiText(s.entry.Name)
		if s.confirm == "grant" || s.confirm == "revoke-permission" {
			text += "\nRecipient: " + tuiText(s.recipient.Name+" "+s.recipient.Type+" "+s.recipient.ID)
		}
		if s.confirm == "grant" || s.confirm == "external" {
			permissions := "server default"
			if len(s.options.Permissions) > 0 {
				permissions = strings.Join(s.options.Permissions, ",")
			}
			expires := "server default"
			if s.options.ExpiresIn != nil {
				expires = s.options.ExpiresIn.String()
				if *s.options.ExpiresIn == 0 {
					expires = "never (subject to server policy)"
				}
			}
			text += "\nPermissions: " + permissions + "\nExpiry: " + expires
			if s.confirm == "external" {
				visits, code := "server default", "none (server may require one)"
				if s.options.Limit != nil {
					visits = fmt.Sprint(*s.options.Limit) + " (-1 = unlimited)"
				}
				if s.options.Code != "" {
					code = "set"
				}
				text += "\nVisits: " + visits + "\nPickup code: " + code
			}
		}
		if s.confirm == "revoke-link" {
			text += "\nLink: " + tuiText(s.link.ID)
		}
		return confirmView(text, "apply")
	}
	if s.stage == "query" || s.stage == "external" || s.stage == "grant-form" {
		return "Sharing settings for " + tuiText(s.entry.Name) + "\nBlank settings use server defaults.\n\n" + s.inputFields.view() + "\n\nTab next • Enter continue/review • Esc back"
	}
	return s.choices.View() + "\nEnter select • Esc back"
}
