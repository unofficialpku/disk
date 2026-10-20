package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/unofficialpku/disk/internal/accounts"
	mountcore "github.com/unofficialpku/disk/internal/mount"
)

func TestAllAccountsFlagScopeAndConflicts(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, command := range []string{"mount", "tui"} {
		for _, flag := range []string{"account", "root", "server", "username", "password-file", "credentials-file", "password-stdin", "no-save"} {
			if command == "tui" && flag == "root" {
				continue
			}
			args := []string{command, "--all-accounts", "--" + flag}
			if command == "mount" {
				args = append(args, "unused")
			}
			if flag != "password-stdin" && flag != "no-save" {
				args = append(args, "value")
			}
			out, stderr, err := testExecute(args...)
			if exitCode(err) != 2 || !strings.Contains(err.Error(), "--all-accounts cannot be combined") || out != "" || stderr != "" {
				t.Fatal("invalid selection reached authentication or driver startup", args, out, stderr, err)
			}
		}
	}
	for _, args := range [][]string{
		{"ls", "--all-accounts"}, {"quota", "--all-accounts"},
		{"upload", "local", "remote", "--all-accounts"}, {"download", "remote", "--all-accounts"},
		{"account", "ls", "--all-accounts"}, {"share", "ls", "path", "--all-accounts"},
	} {
		if _, _, err := testExecute(args...); exitCode(err) != 2 {
			t.Fatal("multi-account flag leaked into another command", args, err)
		}
	}
	out, stderr, err := testExecute("mount", "--all-accounts", "-h")
	if err != nil || stderr != "" || !strings.Contains(out, "--all-accounts") || !strings.Contains(out, "Cross-account moves") {
		t.Fatal("multi-account help is incomplete or went online", out, stderr, err)
	}
	out, stderr, err = testExecute("tui", "--all-accounts", "-h")
	if err != nil || stderr != "" || !strings.Contains(out, "--all-accounts") || !strings.Contains(out, "top-level directories") {
		t.Fatal("aggregate TUI help is incomplete or went online", out, stderr, err)
	}
	if _, err := prepareAccounts(context.Background(), authFlags{nonInteractive: true}, mountcore.SourceOptions{AllAccounts: true}); err == nil || !strings.Contains(err.Error(), "account add") {
		t.Fatal("empty account store did not suggest account add", err)
	}
	// A star stays a literal saved-account name; it never changes selection.
	if _, _, _, err := authenticate(context.Background(), authFlags{account: "*", nonInteractive: true}); !errors.Is(err, accounts.ErrNotFound) {
		t.Fatal("account wildcard was given special meaning", err)
	}
}
