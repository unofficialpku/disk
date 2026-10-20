// Package session connects saved accounts without imposing a user interface.
// Surfaces supply credentials and verification callbacks; session restoration,
// cross-process refresh coordination and persistence are shared here.
package session

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/unofficialpku/disk/internal/accounts"
	"github.com/unofficialpku/disk/internal/auth"
	"github.com/unofficialpku/disk/internal/diagnostic"
)

// Connector uses an explicit store. A nil Store performs a one-off login and
// never reads or writes saved accounts.
type Connector struct {
	Store *accounts.Store
	// NewSession permits protocol fixtures without changing the production
	// authentication endpoints. A nil factory uses auth.New.
	NewSession func(auth.Options) (*auth.Session, error)
}

type Identity struct {
	Name, Server, Username string
}

type Input struct {
	Account, Name, Server string
	// Explicit selects supplied credentials instead of the default account.
	Explicit bool
	// ForceLogin requires Replace before overwriting an existing account. It is
	// used by explicit account creation; normal credential logins can refresh a
	// matching saved account.
	ForceLogin, Replace, Default bool
	// Credentials is called only when no saved account was selected. Ownership
	// of the returned password bytes transfers to Connect, which clears them.
	Credentials func(context.Context) (auth.Credentials, error)
	Challenge   auth.ChallengeFunc
	Trace       *diagnostic.Logger
	Selected    func(Identity)
}

type Connection struct {
	Identity
	Session *auth.Session
	Saved   bool
	Default bool
}

func (c Connector) Connect(ctx context.Context, input Input) (*Connection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var config accounts.Config
	if c.Store != nil {
		var err error
		config, err = c.Store.Load()
		if err != nil {
			return nil, fmt.Errorf("load saved accounts: %w", err)
		}
	}
	var saved *accounts.Account
	if !input.Explicit && !input.ForceLogin && c.Store != nil {
		name := input.Account
		if name == "" {
			name = config.Default
		}
		if name != "" {
			record, ok := config.Find(name)
			if !ok {
				return nil, fmt.Errorf("%w: %s", accounts.ErrNotFound, name)
			}
			if input.Server != "" && strings.TrimRight(input.Server, "/") != strings.TrimRight(record.Server, "/") {
				return nil, errors.New("server differs from the saved account's server; provide explicit credentials for another server")
			}
			saved, input.Server = &record, record.Server
		}
	}
	if input.Server == "" {
		input.Server = auth.DefaultServer
	}
	input.Server = strings.TrimRight(input.Server, "/")
	var credentials auth.Credentials
	if saved != nil {
		credentials = auth.Credentials{Username: saved.Username, Password: []byte(saved.Password)}
	} else {
		if input.Credentials == nil {
			return nil, errors.New("username and password are required")
		}
		var err error
		credentials, err = input.Credentials(ctx)
		if err != nil {
			clear(credentials.Password)
			return nil, err
		}
	}
	defer clear(credentials.Password)
	if credentials.Username == "" || len(credentials.Password) == 0 || len(credentials.Password) > 8192 {
		return nil, errors.New("username and a password of at most 8192 bytes are required")
	}
	name := input.Name
	if name == "" {
		name = credentials.Username
	}
	if saved != nil {
		name = saved.Name
	}
	result := &Connection{Identity: Identity{Name: name, Server: input.Server, Username: credentials.Username}}
	options := auth.Options{Server: input.Server, Trace: input.Trace, Challenge: input.Challenge}
	saveNew := c.Store != nil && saved == nil
	if c.Store != nil {
		options.RefreshLock = c.Store.LockSession
		options.LoadState = func() (auth.State, error) {
			latest, err := c.Store.Load()
			if err != nil {
				return auth.State{}, err
			}
			account, ok := latest.Find(name)
			if !ok || account.Username != result.Username || strings.TrimRight(account.Server, "/") != result.Server {
				return auth.State{}, fmt.Errorf("saved account %q changed while the session was refreshing", name)
			}
			return account.Session, nil
		}
	}
	if saveNew {
		if !accounts.ValidName(name) {
			return nil, errors.New("invalid account name")
		}
		replace := input.Replace
		if existing, exists := config.Find(name); exists {
			if strings.TrimRight(existing.Server, "/") != input.Server {
				return nil, fmt.Errorf("account %q already belongs to %s; choose another account name", name, existing.Server)
			}
			if input.ForceLogin && !input.Replace {
				return nil, accounts.ErrExists
			}
			replace = true
		}
		record := accounts.Account{Name: name, Username: credentials.Username, Password: string(credentials.Password), Server: input.Server}
		created := false
		result.Default = input.Default || config.Default == ""
		options.OnState = func(state auth.State) error {
			if created {
				return c.Store.UpdateState(name, record.Server, record.Username, state)
			}
			record.Session = state
			if err := c.Store.Put(record, result.Default, replace); err != nil {
				return err
			}
			created = true
			record.Password = ""
			return nil
		}
	} else if saved != nil {
		options.OnState = func(state auth.State) error {
			return c.Store.UpdateState(saved.Name, saved.Server, saved.Username, state)
		}
	}
	if input.Selected != nil {
		input.Selected(result.Identity)
	}
	newSession := c.NewSession
	if newSession == nil {
		newSession = auth.New
	}
	s, err := newSession(options)
	if err != nil {
		return nil, err
	}
	result.Session = s
	if saved != nil && saved.Session.RefreshToken != "" {
		err = s.Restore(ctx, saved.Session)
		if err == nil {
			return result, nil
		}
		s.Close()
		if !errors.Is(err, auth.ErrSessionExpired) {
			return nil, fmt.Errorf("restore account %q: %w", saved.Name, err)
		}
	}
	if err := s.Login(ctx, credentials); err != nil {
		s.Close()
		return nil, err
	}
	result.Saved = saveNew
	return result, nil
}
