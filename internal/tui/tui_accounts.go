package tui

import (
	"errors"

	"github.com/unofficialpku/disk/internal/transfer"
	"github.com/unofficialpku/disk/internal/vfs"
)

type tuiTarget struct {
	backend transfer.Backend
	id      string
}

// Account routing stays in the backend; UI selections keep scoped identities.
func (m *tuiModel) target(id string) (tuiTarget, error) {
	target := tuiTarget{backend: m.service.Backend, id: id}
	if namespace, ok := m.backend.(*vfs.AccountBackend); ok {
		view, err := namespace.WithAccount(id)
		if err != nil {
			return tuiTarget{}, err
		}
		target.backend = view
	}
	if target.backend == nil {
		return tuiTarget{}, errors.New("this backend does not support file operations")
	}
	return target, nil
}

func (t tuiTarget) extractionDestination(selector string) (string, error) {
	if backend, ok := t.backend.(interface{ ExtractionDestination(string) (string, error) }); ok {
		return backend.ExtractionDestination(selector)
	}
	return selector, nil
}

func (m *tuiModel) virtual(id string) bool {
	if namespace, ok := m.backend.(*vfs.AccountBackend); ok {
		return namespace.VirtualDirectory(id)
	}
	return false
}

func (m *tuiModel) accountContext() (string, string) {
	if namespace, ok := m.backend.(*vfs.AccountBackend); ok && m.mode == managementMode && m.management != nil {
		if account, _, err := namespace.Account(m.management.entry.ID); err == nil {
			return account.Name, account.Server
		}
	}
	if namespace, ok := m.backend.(*vfs.AccountBackend); ok && m.location.entry != nil {
		if account, _, err := namespace.Account(m.location.entry.ID); err == nil {
			return account.Name, account.Server
		}
	}
	return m.account, m.server
}
