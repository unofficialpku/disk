package main

import (
	"context"
	"github.com/unofficialpku/disk/internal/accounts"
	mountcore "github.com/unofficialpku/disk/internal/mount"
	"github.com/unofficialpku/disk/internal/session"
)

// CLI credentials and announcements are the only surface-specific part of selection.
func prepareAccounts(ctx context.Context, credentials authFlags, opts mountcore.SourceOptions) (*mountcore.Source, error) {
	var store *accounts.Store
	if opts.AllAccounts {
		var err error
		store, err = accounts.Default()
		if err != nil {
			return nil, err
		}
	}
	return mountcore.PrepareSource(ctx, store, credentials.account, opts, func(ctx context.Context, name string) (*session.Connection, error) {
		credentials.account = name
		active, server, label, err := authenticate(ctx, credentials)
		if err != nil {
			return nil, err
		}
		return &session.Connection{Identity: session.Identity{Name: label, Server: server}, Session: active}, nil
	})
}
