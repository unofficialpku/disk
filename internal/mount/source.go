package mount

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/unofficialpku/disk/internal/accounts"
	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/unofficialpku/disk/internal/auth"
	"github.com/unofficialpku/disk/internal/diagnostic"
	"github.com/unofficialpku/disk/internal/session"
	"github.com/unofficialpku/disk/internal/vfs"
)

type Source struct {
	Backend  vfs.Backend
	Root     *anyshare.Entry
	sessions []*session.Connection
}

type SourceOptions struct {
	AllAccounts bool
	Root        string
	PartSize    int64
	Timeout     time.Duration
	Trace       *diagnostic.Logger
}

type ConnectFunc func(context.Context, string) (*session.Connection, error)

// PrepareSource selects the saved accounts, connects them, and validates each
// remote root before exposing a filesystem. A failure closes every new session.
func PrepareSource(ctx context.Context, store *accounts.Store, account string, opts SourceOptions, connect ConnectFunc) (_ *Source, result error) {
	names := []string{account}
	if opts.AllAccounts {
		if store == nil {
			return nil, errors.New("saved account store is required")
		}
		config, err := store.Load()
		if err != nil {
			return nil, fmt.Errorf("load saved accounts: %w", err)
		}
		if len(config.Accounts) == 0 {
			return nil, errors.New("no saved accounts; run 'pkudisk account add' first")
		}
		names = nil
		for _, account := range config.Accounts {
			names = append(names, account.Name)
		}
	}
	var connections []*session.Connection
	defer func() {
		if result != nil {
			for _, connection := range connections {
				if connection.Session != nil {
					connection.Session.Close()
				}
			}
		}
	}()
	for _, name := range names {
		connected, err := connect(ctx, name)
		if err != nil {
			if name == "" {
				return nil, err
			}
			return nil, fmt.Errorf("prepare account %q: %w", name, err)
		}
		connections = append(connections, connected)
	}
	return NewSource(ctx, connections, opts)
}

// NewSource borrows sessions from the caller. GUI connections may continue to
// serve requests after unmount, so neither Run nor NewSource closes them.
func NewSource(ctx context.Context, connections []*session.Connection, opts SourceOptions) (*Source, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(connections) == 0 || (!opts.AllAccounts && len(connections) != 1) {
		return nil, errors.New("select an account")
	}
	if opts.AllAccounts && opts.Root != "" && opts.Root != "/" {
		return nil, errors.New("a remote root cannot be combined with all accounts")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultOptions().Timeout
	}
	source := &Source{}
	var mounted []vfs.MountAccount
	for _, connected := range connections {
		client, err := anyshare.New(connected.Server, connected.Session)
		if err != nil {
			return nil, err
		}
		if opts.PartSize > 0 {
			client.PartSize = opts.PartSize
		}
		client.SetTrace(opts.Trace)
		if err := selectRoot(ctx, source, client, opts); err != nil {
			return nil, fmt.Errorf("prepare account %q: %w", connected.Name, err)
		}
		mounted = append(mounted, vfs.MountAccount{Name: connected.Name, Server: connected.Server, Backend: client})
		source.Backend = client
		if connected.Session != nil {
			source.sessions = append(source.sessions, connected)
		}
	}
	if opts.AllAccounts {
		backend, err := vfs.NewAccountBackend(mounted)
		if err != nil {
			return nil, err
		}
		source.Backend = backend
	}
	return source, nil
}

func selectRoot(ctx context.Context, source *Source, client *anyshare.Client, opts SourceOptions) error {
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	if opts.Root == "" || opts.Root == "/" {
		_, err := client.Roots(ctx)
		return err
	}
	entry, err := client.Resolve(ctx, opts.Root)
	if err != nil {
		return fmt.Errorf("select mount root: %w", err)
	}
	if !entry.Dir {
		return errors.New("mount root must be a remote directory")
	}
	source.Root = &entry
	return nil
}

func (s *Source) AccountCount() int { return len(s.sessions) }

func (s *Source) Close() {
	for _, account := range s.sessions {
		account.Session.Close()
	}
}

// Maintain refreshes all accounts and joins its workers on stop. Expiration
// names the failed account so every mount driver can stop consistently.
func (s *Source) Maintain(ctx context.Context, interval time.Duration, output io.Writer) (<-chan error, func()) {
	ctx, cancel := context.WithCancel(ctx)
	failed := make(chan error, len(s.sessions))
	var workers sync.WaitGroup
	var outputMu sync.Mutex
	for _, account := range s.sessions {
		workers.Go(func() {
			account.Session.KeepAlive(ctx, interval, func(err error) {
				if errors.Is(err, auth.ErrSessionExpired) {
					failed <- fmt.Errorf("account %q: %w", account.Name, err)
					return
				}
				outputMu.Lock()
				defer outputMu.Unlock()
				if err == nil {
					fmt.Fprintf(output, "Account %q: authentication session renewed.\n", account.Name)
				} else {
					fmt.Fprintf(output, "Account %q: session renewal will retry: %v\n", account.Name, err)
				}
			})
		})
	}
	return failed, func() { cancel(); workers.Wait() }
}
