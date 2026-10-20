package gui

import (
	"context"

	"github.com/unofficialpku/disk/internal/accounts"
	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/unofficialpku/disk/internal/auth"
	"github.com/unofficialpku/disk/internal/session"
)

// NewConnector adapts browser login input to the same account/session core used
// by CLI commands. Desktop and mobile differ only in their store location.
func NewConnector(store *accounts.Store) func(context.Context, LoginInput, auth.ChallengeFunc) (*Connection, error) {
	connector := session.Connector{Store: store}
	return func(ctx context.Context, input LoginInput, challenge auth.ChallengeFunc) (*Connection, error) {
		connected, err := connector.Connect(ctx, session.Input{
			Account: input.Account, Name: input.Name, Server: input.Server,
			Explicit: input.Account == "", ForceLogin: input.Account == "", Replace: input.Replace, Default: input.Default,
			Credentials: func(context.Context) (auth.Credentials, error) {
				return auth.Credentials{Username: input.Username, Password: []byte(input.Password)}, nil
			}, Challenge: challenge,
		})
		if err != nil {
			return nil, err
		}
		client, err := anyshare.New(connected.Server, connected.Session)
		if err != nil {
			connected.Session.Close()
			return nil, err
		}
		return &Connection{Name: connected.Name, Server: connected.Server, Username: connected.Username, Client: client, Session: connected.Session}, nil
	}
}
