package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/pflag"
	"github.com/unofficialpku/disk/internal/accounts"
	"github.com/unofficialpku/disk/internal/auth"
	"github.com/unofficialpku/disk/internal/diagnostic"
	"github.com/unofficialpku/disk/internal/localfile"
	"github.com/unofficialpku/disk/internal/session"
)

type authFlags struct {
	challenge                                                auth.ChallengeFunc
	trace                                                    *diagnostic.Logger
	server, account, username, passwordFile, credentialsFile string
	stdin, noSave                                            bool
	saveName                                                 string
	forceLogin, makeDefault, replace                         bool
	announce                                                 bool
	nonInteractive                                           bool
	input                                                    io.Reader
	output                                                   io.Writer
}

func (f *authFlags) addCredentials(set *pflag.FlagSet) {
	set.StringVar(&f.server, "server", "", "AnyShare server URL (saved account's server or "+auth.DefaultServer+")")
	set.StringVarP(&f.username, "username", "u", "", "login username (prompted if omitted)")
	set.StringVar(&f.passwordFile, "password-file", "", "read password from an owner-only file")
	set.StringVar(&f.credentialsFile, "credentials-file", "", "read campus username/password from an owner-only JSON file")
	set.BoolVar(&f.stdin, "password-stdin", false, "read password from standard input")
	set.BoolVar(&f.nonInteractive, "non-interactive", false, "fail instead of prompting for credentials, verification or required command inputs")
}

func (f *authFlags) add(set *pflag.FlagSet) {
	f.addCredentials(set)
	set.StringVarP(&f.account, "account", "a", "", "use this saved account instead of the default")
	set.BoolVar(&f.noSave, "no-save", false, "use a one-off login without reading or saving accounts")
}

func (f authFlags) validate() error {
	explicit := f.username != "" || f.passwordFile != "" || f.credentialsFile != "" || f.stdin
	if f.credentialsFile != "" && (f.username != "" || f.passwordFile != "" || f.stdin) {
		return errors.New("--credentials-file cannot be combined with other credential flags")
	}
	if f.passwordFile != "" && f.stdin {
		return errors.New("choose --password-file or --password-stdin, not both")
	}
	if f.account != "" && (explicit || f.noSave) {
		return errors.New("--account cannot be combined with explicit credentials or --no-save")
	}
	return nil
}

func privateFile(path string) ([]byte, error) {
	file, err := localfile.OpenPrivate(localfile.ExpandPath(path), os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	b, err := io.ReadAll(io.LimitReader(file, 8193))
	if err != nil {
		return nil, err
	}
	if len(b) > 8192 {
		clear(b)
		return nil, errors.New("credential file is too large")
	}
	return b, nil
}

func passwordBytes(b []byte) []byte {
	return bytes.TrimSuffix(bytes.TrimSuffix(b, []byte("\n")), []byte("\r"))
}

func readCredentials(ctx context.Context, opts authFlags) (auth.Credentials, error) {
	var c auth.Credentials
	if opts.credentialsFile != "" {
		b, err := privateFile(opts.credentialsFile)
		if err != nil {
			return c, err
		}
		defer clear(b)
		var v struct {
			Username string `json:"username"`
			Password string `json:"password"`
			OTP      string `json:"otp"`
			SMS      string `json:"sms"`
		}
		if err = json.Unmarshal(b, &v); err != nil {
			return c, errors.New("invalid credentials JSON")
		}
		return auth.Credentials{Username: v.Username, Password: []byte(v.Password), OTP: v.OTP, SMS: v.SMS}, nil
	}
	c.Username = opts.username
	if c.Username == "" {
		if opts.nonInteractive {
			return c, errors.New("no saved account or username; run 'account add' interactively, select --account NAME, or provide --username and --password-file")
		}
		b, err := ttyInput(ctx, "Username: ", false)
		if err != nil {
			return c, err
		}
		c.Username = string(b)
	}
	var err error
	switch {
	case opts.passwordFile != "":
		c.Password, err = privateFile(opts.passwordFile)
		c.Password = passwordBytes(c.Password)
	case opts.stdin:
		c.Password, err = io.ReadAll(io.LimitReader(opts.input, 8193))
		c.Password = passwordBytes(c.Password)
	default:
		if opts.nonInteractive {
			return c, errors.New("password required; provide --password-file, --password-stdin, or --credentials-file")
		}
		c.Password, err = ttyInput(ctx, "Password: ", true)
	}
	if err != nil {
		clear(c.Password)
	}
	return c, err
}

func verificationInput(ctx context.Context, challenge auth.Challenge, output io.Writer) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(challenge.Image) > 0 {
		file, err := os.CreateTemp("", "pkudisk-captcha-*.png")
		if err != nil {
			return "", err
		}
		defer os.Remove(file.Name())
		_, err = file.Write(challenge.Image)
		closeErr := file.Close()
		if err != nil {
			return "", err
		}
		if closeErr != nil {
			return "", closeErr
		}
		fmt.Fprintln(output, "CAPTCHA image:", file.Name())
	}
	b, err := ttyInput(ctx, challenge.Kind+": ", true)
	defer clear(b)
	return string(b), err
}

func authenticate(ctx context.Context, opts authFlags) (*auth.Session, string, string, error) {
	return authenticateWith(ctx, opts, auth.New)
}

func authenticateWith(ctx context.Context, opts authFlags, newSession func(auth.Options) (*auth.Session, error)) (*auth.Session, string, string, error) {
	if err := opts.validate(); err != nil {
		return nil, "", "", err
	}
	if opts.input == nil {
		opts.input = os.Stdin
	}
	if opts.output == nil {
		opts.output = os.Stderr
	}
	var store *accounts.Store
	if !opts.noSave {
		var err error
		store, err = accounts.Default()
		if err != nil {
			return nil, "", "", err
		}
	}
	input := session.Input{
		Account: opts.account, Name: opts.saveName, Server: opts.server,
		Explicit:   opts.username != "" || opts.passwordFile != "" || opts.credentialsFile != "" || opts.stdin,
		ForceLogin: opts.forceLogin, Replace: opts.replace, Default: opts.makeDefault, Trace: opts.trace,
		Credentials: func(ctx context.Context) (auth.Credentials, error) { return readCredentials(ctx, opts) },
		Challenge: func(ctx context.Context, challenge auth.Challenge) (string, error) {
			if opts.challenge != nil {
				return opts.challenge(ctx, challenge)
			}
			if opts.nonInteractive {
				return "", fmt.Errorf("%s required; rerun interactively or supply available OTP/SMS codes with --credentials-file", challenge.Kind)
			}
			return verificationInput(ctx, challenge, opts.output)
		},
	}
	if opts.announce {
		input.Selected = func(account session.Identity) {
			if account.Name == account.Username {
				fmt.Fprintf(opts.output, "Using account %q (%s)\n", account.Name, account.Server)
			} else {
				fmt.Fprintf(opts.output, "Using account %q (username %q, %s)\n", account.Name, account.Username, account.Server)
			}
		}
	}
	connected, err := (session.Connector{Store: store, NewSession: newSession}).Connect(ctx, input)
	if err != nil {
		return nil, "", "", err
	}
	if connected.Saved {
		suffix := ""
		if connected.Default {
			suffix = " (default)"
		}
		fmt.Fprintf(opts.output, "Saved account %q%s.\n", connected.Name, suffix)
	}
	return connected.Session, connected.Server, connected.Name, nil
}
