package gui

import (
	"context"
	"errors"
	"github.com/unofficialpku/disk/internal/accounts"
	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/unofficialpku/disk/internal/anyshare/mockserver"
	"github.com/unofficialpku/disk/internal/auth"
	"os"
	"path/filepath"
	"testing"
)

const fixtureLibrary = mockserver.Library

type businessFixture struct {
	server *mockserver.Server
	store  *accounts.Store
	root   string
}

func newBusinessFixture(t *testing.T) *businessFixture {
	t.Helper()
	f := &businessFixture{root: t.TempDir(), server: mockserver.New("campus", "work")}
	t.Cleanup(func() {
		f.server.Close()
		for _, failure := range f.server.Failures() {
			t.Error(failure)
		}
	})
	f.store = &accounts.Store{Path: filepath.Join(f.root, "config", "accounts.json")}
	for _, name := range []string{"campus", "work"} {
		if err := f.store.Put(f.server.Account(name), name == "campus", false); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(f.root, "upload.txt"), []byte("uploaded through the GUI\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *businessFixture) options() ApplicationOptions {
	return ApplicationOptions{
		Store: f.store, CacheDirectory: filepath.Join(f.root, "cache"),
		Connect:    f.connect,
		CheckMount: func() error { return nil },
		Mount: func(ctx context.Context, _ MountInput, _ []*Connection, ready func(), stop <-chan chan error) error {
			ready()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case reply := <-stop:
				reply <- nil
				return nil
			}
		},
	}
}

func (f *businessFixture) connect(ctx context.Context, input LoginInput, challenge auth.ChallengeFunc) (*Connection, error) {
	name := input.Account
	if name == "" {
		name = input.Name
		if name == "" {
			name = input.Username
		}
		if input.Username == "verify" {
			code, err := challenge(ctx, auth.Challenge{Kind: "OTP"})
			if err != nil {
				return nil, err
			}
			if code != "123456" {
				return nil, errors.New("incorrect verification code")
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := f.store.Put(accounts.Account{Name: name, Username: input.Username, Password: input.Password, Server: f.server.URL}, input.Default, input.Replace); err != nil {
			return nil, err
		}
		f.server.AddAccount(name)
	}
	config, err := f.store.Load()
	if err != nil {
		return nil, err
	}
	saved, ok := config.Find(name)
	if !ok {
		return nil, accounts.ErrNotFound
	}
	client, err := anyshare.New(f.server.URL, mockserver.Token(name))
	if err != nil {
		return nil, err
	}
	return &Connection{Name: name, Server: f.server.URL, Username: saved.Username, Client: client}, nil
}
