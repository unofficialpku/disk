package main

import (
	"context"
	"os"

	"github.com/unofficialpku/disk/internal/accounts"
	"github.com/unofficialpku/disk/internal/diagnostic"
	"github.com/unofficialpku/disk/internal/gui"
	mountcore "github.com/unofficialpku/disk/internal/mount"
	"github.com/unofficialpku/disk/internal/session"
)

func newGUIApplication(ctx context.Context) (*gui.Application, error) {
	store, err := accounts.Default()
	if err != nil {
		return nil, err
	}
	return gui.NewApplication(ctx, gui.ApplicationOptions{
		Store: store, CacheDirectory: defaultCache(),
		Connect: gui.NewConnector(store), CheckMount: mountcore.CheckSupport, Mount: guiMount,
		ValidateMount: func(input gui.MountInput) error { _, err := guiMountOptions(input); return err },
	}), nil
}

func guiMount(ctx context.Context, input gui.MountInput, connections []*gui.Connection, ready func(), unmount <-chan chan error) error {
	opts, err := guiMountOptions(input)
	if err != nil {
		return err
	}
	opts.Ready, opts.Unmount = ready, unmount
	active := make([]*session.Connection, 0, len(connections))
	for _, connected := range connections {
		active = append(active, &session.Connection{Identity: session.Identity{Name: connected.Name, Server: connected.Server, Username: connected.Username}, Session: connected.Session})
	}
	source, err := mountcore.NewSource(ctx, active, mountcore.SourceOptions{AllAccounts: input.AllAccounts, Root: opts.Root, PartSize: opts.PartSize, Timeout: opts.Timeout, Trace: opts.Trace})
	if err != nil {
		return err
	}
	return mountcore.Run(ctx, source, opts, os.Stderr)
}

func guiMountOptions(input gui.MountInput) (mountcore.Options, error) {
	opts, err := mountcore.ParseOptions(mountcore.Input{
		Point: input.Point, Root: input.Root, Cache: input.Cache, ReadOnly: input.ReadOnly, Debug: input.Debug, Verbose: input.Verbose,
		CacheLimit: input.CacheLimit, PartSize: input.PartSize, WholeFile: input.WholeFile, BlockSize: input.BlockSize, ReadAhead: input.ReadAhead,
		Concurrency: input.Concurrency, TimeoutSeconds: input.TimeoutSeconds, TTLSeconds: input.TTLSeconds, RefreshSeconds: input.RefreshSeconds,
	})
	if err == nil && input.Verbose {
		opts.Trace = diagnostic.New(os.Stderr)
	}
	return opts, err
}
