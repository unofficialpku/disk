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

type tuiShareBackend struct {
	tuiBackend
	create  func(context.Context, anyshare.Entry, anyshare.ShareOptions) (anyshare.ShareLink, error)
	grant   func(context.Context, anyshare.Entry, []anyshare.Recipient, anyshare.ShareOptions) error
	removes int
}

func (b *tuiShareBackend) InternalLink(context.Context, anyshare.Entry) (anyshare.ShareLink, error) {
	return anyshare.ShareLink{ID: "inner", Type: "realname", URL: "https://disk.pku.edu.cn/link/inner"}, nil
}
func (b *tuiShareBackend) CreateExternalLink(ctx context.Context, entry anyshare.Entry, options anyshare.ShareOptions) (anyshare.ShareLink, error) {
	return b.create(ctx, entry, options)
}
func (b *tuiShareBackend) ShareLinks(_ context.Context, _ anyshare.Entry, external bool) ([]anyshare.ShareLink, error) {
	return []anyshare.ShareLink{{ID: "outer", Type: "anonymous", Title: "test", URL: "https://disk.pku.edu.cn/link/outer"}}, nil
}
func (b *tuiShareBackend) RemoveExternalLink(context.Context, string) error { b.removes++; return nil }
func (b *tuiShareBackend) GrantShare(ctx context.Context, entry anyshare.Entry, recipients []anyshare.Recipient, options anyshare.ShareOptions) error {
	return b.grant(ctx, entry, recipients, options)
}
func (b *tuiShareBackend) SharePermissions(context.Context, string) (anyshare.ShareACL, error) {
	return anyshare.ShareACL{Inherit: true, Permissions: []anyshare.SharePermission{
		{ID: "inherited", Type: "user", Name: "Inherited", Allow: []string{"download"}, InheritedFrom: "gns://library"},
		{ID: "u", Type: "user", Name: "Recipient", Allow: []string{"download"}},
	}}, nil
}
func (b *tuiShareBackend) RevokeShare(context.Context, string, []anyshare.Recipient) (int, error) {
	b.removes++
	return 1, nil
}
func (b *tuiShareBackend) SearchRecipients(context.Context, string, int) ([]anyshare.Recipient, error) {
	return []anyshare.Recipient{{ID: "u", Type: "user", Name: "Recipient", Account: "directory-user"}}, nil
}

func sharingModel(t *testing.T, backend *tuiShareBackend) *tuiModel {
	t.Helper()
	backend.entries = []anyshare.Entry{{ID: "gns://library/file", Name: "test.txt", Size: 5}}
	model := newTUIModel(context.Background(), backend, "account", "server", time.Second)
	finishNetworkCommand(t, model, model.Init())
	model.Update(tea.KeyPressMsg{Code: 's', Text: "s"})
	if model.mode != sharingMode || model.sharing.stage != "menu" {
		t.Fatal("s did not open sharing")
	}
	return model
}

func pressEnter(m *tuiModel) tea.Cmd {
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	return cmd
}

func TestTUISharingRequiresReviewAndPreservesInputKeys(t *testing.T) {
	creates := 0
	backend := &tuiShareBackend{create: func(_ context.Context, entry anyshare.Entry, options anyshare.ShareOptions) (anyshare.ShareLink, error) {
		creates++
		if options.Code != "Code1234" || options.ExpiresIn == nil || *options.ExpiresIn != time.Hour || options.Limit == nil || *options.Limit != 3 || strings.Join(options.Permissions, ",") != "download" {
			t.Error("TUI lost link settings", options)
		}
		return anyshare.ShareLink{ID: "outer", Type: "anonymous", Title: entry.Name, URL: "https://disk.pku.edu.cn/link/outer", Code: options.Code}, nil
	}}
	model := sharingModel(t, backend)
	model.sharing.choices.Select(3)
	pressEnter(model)
	model.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if model.sharing.fields[0].Value() != "q" || strings.Contains(model.View().Content, " > q") {
		t.Fatal("q quit a form or pickup code leaked")
	}
	for i, value := range []string{"Code1234", "1h", "download", "3"} {
		model.sharing.fields[i].SetValue(value)
		pressEnter(model)
	}
	if model.sharing.stage != "confirm" || creates != 0 || !strings.Contains(model.View().Content, "download") {
		t.Fatal("link created without review")
	}
	_, cmd := model.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	finishNetworkCommand(t, model, cmd)
	if model.sharing.stage != "result" || creates != 1 || !strings.Contains(model.View().Content, "Code1234") {
		t.Fatal("link result is missing")
	}
	_, cmd = model.Update(tea.KeyPressMsg{Code: 'c', Text: "c"})
	if cmd == nil || !strings.Contains(model.status, "OSC52") {
		t.Fatal("copy URL command unavailable")
	}
}

func TestTUISharingRecipientSelectionAndInheritedRevoke(t *testing.T) {
	grants := 0
	backend := &tuiShareBackend{grant: func(_ context.Context, _ anyshare.Entry, recipients []anyshare.Recipient, _ anyshare.ShareOptions) error {
		grants++
		if len(recipients) != 1 || recipients[0].ID != "u" {
			t.Error("TUI granted wrong recipient")
		}
		return nil
	}}
	model := sharingModel(t, backend)
	model.sharing.choices.Select(1)
	pressEnter(model)
	model.sharing.fields[0].SetValue("Recipient")
	finishNetworkCommand(t, model, pressEnter(model))
	if model.sharing.stage != "recipients" {
		t.Fatal("recipient search has no choices")
	}
	pressEnter(model)
	pressEnter(model)
	pressEnter(model)
	if grants != 0 || model.sharing.stage != "confirm" {
		t.Fatal("grant occurred before review")
	}
	_, cmd := model.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	finishNetworkCommand(t, model, cmd)
	if grants != 1 || model.sharing.stage != "result" {
		t.Fatal("grant or internal link failed")
	}
	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	model.sharing.choices.Select(2)
	finishNetworkCommand(t, model, pressEnter(model))
	pressEnter(model)
	if model.sharing.stage != "permissions" || backend.removes != 0 || !strings.Contains(model.status, "inherited") {
		t.Fatal("inherited permission removal was offered")
	}
	model.sharing.choices.Select(1)
	pressEnter(model)
	_, cmd = model.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	finishNetworkCommand(t, model, cmd)
	if backend.removes != 1 {
		t.Fatal("direct grant removal failed")
	}
}

func TestTUISharingCancellationAndApprovalRemainExplicit(t *testing.T) {
	backend := &tuiShareBackend{create: func(ctx context.Context, _ anyshare.Entry, _ anyshare.ShareOptions) (anyshare.ShareLink, error) {
		<-ctx.Done()
		return anyshare.ShareLink{}, ctx.Err()
	}}
	model := sharingModel(t, backend)
	model.sharing.options = anyshare.ShareOptions{}
	cmd := model.sharingOperation("external", "")
	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	finishNetworkCommand(t, model, cmd)
	if model.busy || !strings.Contains(model.status, "cancelled") {
		t.Fatal("sharing cancellation blocked the UI")
	}
	model.busy = true
	model.Update(sharingMsg{id: model.operation, action: "external", err: anyshare.ErrPending})
	if model.sharing.stage != "menu" || !strings.Contains(model.status, "not active") {
		t.Fatal("pending sharing was displayed as active")
	}
	backend.create = func(context.Context, anyshare.Entry, anyshare.ShareOptions) (anyshare.ShareLink, error) {
		return anyshare.ShareLink{}, errors.New("network failed")
	}
	model.Update(sharingMsg{id: model.operation - 1, action: "external", link: anyshare.ShareLink{ID: "stale"}})
	if model.sharing.stage != "menu" {
		t.Fatal("stale sharing operation changed current state")
	}
}

func TestRecipientPickerConfirmsSelectionAndFilters(t *testing.T) {
	picker := &recipientPicker{list: list.New([]list.Item{recipientItem{anyshare.Recipient{ID: "u", Name: "Recipient", Type: "user"}}}, list.NewDefaultDelegate(), 80, 16)}
	picker.Update(tea.KeyPressMsg{Code: '/'})
	picker.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if picker.chosen != nil {
		t.Fatal("filter selected a recipient")
	}
	picker.list.ResetFilter()
	_, cmd := picker.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil || picker.chosen == nil || picker.chosen.ID != "u" {
		t.Fatal("Enter failed to confirm exact recipient")
	}
}
