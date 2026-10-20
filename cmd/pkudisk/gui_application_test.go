package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/unofficialpku/disk/internal/auth"
)

func TestGUIAuthenticationUsesChallengeCallbackWithoutTerminal(t *testing.T) {
	called := false
	want := errors.New("stop before networking")
	_, _, _, err := authenticateWith(context.Background(), authFlags{username: "fixture", stdin: true, input: strings.NewReader("fixture-password"), output: io.Discard, noSave: true, nonInteractive: true,
		challenge: func(_ context.Context, challenge auth.Challenge) (string, error) {
			called = true
			if challenge.Kind != "OTP" {
				t.Fatal(challenge.Kind)
			}
			return "123456", nil
		}},
		func(options auth.Options) (*auth.Session, error) {
			answer, err := options.Challenge(context.Background(), auth.Challenge{Kind: "OTP"})
			if err != nil || answer != "123456" {
				t.Fatal("browser verification was replaced by a terminal prompt", err)
			}
			return nil, want
		})
	if !called || !errors.Is(err, want) {
		t.Fatal("GUI authentication adapter did not use the shared callback", err)
	}
}
