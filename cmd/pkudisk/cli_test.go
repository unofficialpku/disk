package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unofficialpku/disk/internal/accounts"
	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/unofficialpku/disk/internal/transfer"
	"github.com/unofficialpku/disk/internal/vfs"
)

func testExecute(args ...string) (string, string, error) {
	var out, errOut bytes.Buffer
	err := execute(context.Background(), args, strings.NewReader(""), &out, &errOut)
	return out.String(), errOut.String(), err
}
func TestHelpIsOfflineAndConsistent(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", directory)
	// Even a malformed account store must not prevent help or completion.
	path := filepath.Join(directory, "pkudisk")
	os.Mkdir(path, 0700)
	os.WriteFile(filepath.Join(path, "accounts.json"), []byte("not JSON"), 0600)
	paths := [][]string{{"mount"}, {"ls"}, {"upload"}, {"download"}, {"extract"}, {"recover"}, {"tui"}, {"quota"}, {"search"}, {"copy"}, {"delete"}, {"recycle"}, {"recycle", "ls"}, {"recycle", "restore"}, {"recycle", "remove"}, {"recycle", "empty"}, {"revision"}, {"revision", "ls"}, {"revision", "restore"}, {"share"}, {"share", "add"}, {"share", "ls"}, {"share", "link"}, {"share", "search"}, {"share", "remove"}, {"link"}, {"link", "add"}, {"link", "ls"}, {"link", "remove"}, {"account"}, {"account", "add"}, {"account", "ls"}, {"account", "default"}, {"account", "remove"}, {"version"}, {"completion"}}
	for _, path := range paths {
		t.Run(strings.Join(path, "/"), func(t *testing.T) {
			direct, stderr, err := testExecute(append(append([]string{}, path...), "--help")...)
			if err != nil || stderr != "" || !strings.Contains(direct, "Usage:") {
				t.Fatalf("direct help: %v stderr=%q output=%q", err, stderr, direct)
			}
			indirect, stderr, err := testExecute(append([]string{"help"}, path...)...)
			if err != nil || stderr != "" || indirect != direct {
				t.Fatalf("nested help differs: %v stderr=%q", err, stderr)
			}
		})
	}
	out, stderr, err := testExecute()
	if err != nil || stderr != "" || !strings.Contains(out, "recover") {
		t.Fatal("empty invocation must show help")
	}
	out, stderr, err = testExecute("mount", "--verbose", "--help")
	if err != nil || stderr != "" || !strings.Contains(out, "--verbose") {
		t.Fatal("verbose mount help must stay offline and quiet")
	}
	out, stderr, err = testExecute("account")
	if err != nil || stderr != "" || !strings.Contains(out, "Available Commands:") {
		t.Fatal("account group must show help without reading the account store")
	}
	out, stderr, err = testExecute("completion", "zsh")
	if err != nil || stderr != "" || !strings.Contains(out, "#compdef") {
		t.Fatalf("completion: %v", err)
	}
	out, _, err = testExecute("__complete", "account", "")
	if err != nil || !strings.Contains(out, "ls\t") || strings.Contains(out, "list\t") {
		t.Fatal("shell completion must advertise only account ls")
	}
}

func TestAccountLsSummaries(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	out, stderr, err := testExecute("account", "ls", "--json")
	if err != nil || stderr != "" || strings.TrimSpace(out) != "[]" {
		t.Fatalf("empty account listing: %v, %q", err, out)
	}
	store, err := accounts.Default()
	if err != nil {
		t.Fatal(err)
	}
	account := accounts.Account{Name: "work", Username: "test-user", Server: "https://disk.pku.edu.cn", Password: "private-test-password"}
	if err := store.Put(account, true, false); err != nil {
		t.Fatal(err)
	}
	out, stderr, err = testExecute("account", "ls", "--json")
	var rows []map[string]any
	if err != nil || stderr != "" || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 1 {
		t.Fatalf("saved account listing: %v, %q", err, out)
	}
	row := rows[0]
	if len(row) != 4 || row["name"] != "work" || row["username"] != "test-user" || row["server"] != account.Server || row["default"] != true {
		t.Fatal("account ls must expose only the public summary and default marker")
	}
	out, stderr, err = testExecute("account", "ls")
	if err != nil || stderr != "" || !strings.Contains(out, "work") || strings.Contains(out, account.Password) {
		t.Fatal("text account listing must omit credentials")
	}
}

func TestUsageFailuresBeforeAuthentication(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, args := range [][]string{
		{"util"}, {"util", "--help"}, {"help", "util"}, {"util", "quota"}, {"util", "search", "test"}, {"util", "copy", "source", "destination"},
		{"util", "share", "ls", "path"}, {"util", "link", "ls", "path"},
		{"accounts"}, {"roots"}, {"root"}, {"root", "--help"}, {"help", "root"}, {"account", "use", "unused"}, {"account", "delete", "unused"},
		{"help", "missing"}, {"help", "--json"}, {"recover", "ignored"}, {"root", "ignored"},
		{"list"}, {"account", "list"}, {"account", "list", "--help"}, {"help", "account", "list"}, {"account", "--json"},
		{"account", "ls", "unexpected"}, {"account", "add", "--name", "work"},
		{"account", "default"}, {"account", "add", "one", "two"}, {"upload", "only-source"},
		{"upload", "source", "destination", "--remote", "conflict"}, {"download", "remote", "local", "--output", "conflict"},
		{"upload", "source", "--remote", "destination"}, {"download", "remote", "--output", "local"}, {"download", "remote", "-o", "local"},
		{"ls", "--password-stdin", "--password-file", "file"}, {"ls", "--account", "work", "--username", "user"},
		{"download", "remote", "--io-timeout", "invalid"}, {"download", "remote", "--io-timeout", "0s"},
		{"download", "folder", "--archive", "--via-archive"},
		{"upload", "folder", "remote", "--via-archive", "--overwrite"},
		{"extract", "file"}, {"extract", "file", "remote", "--overwrite"},
		{"mount", "unused", "--read-block-size", "0"}, {"mount", "unused", "--read-block-size", "513MiB"},
		{"mount", "unused", "--read-whole-file", "-1"}, {"mount", "unused", "--read-whole-file", "513MiB"},
		{"mount", "unused", "--read-ahead", "wrong"}, {"mount", "unused", "--read-ahead", "513MiB"},
		{"mount", "unused", "--read-concurrency", "0"}, {"mount", "unused", "--read-concurrency", "65"},
		{"mount", "unused", "--cache-limit", "18446744073709551615"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			out, _, err := testExecute(args...)
			if exitCode(err) != 2 || !strings.Contains(err.Error(), "--help") || out != "" {
				t.Fatalf("exit=%d out=%q err=%v", exitCode(err), out, err)
			}
		})
	}
}

func TestCommandDistributionAndCompletionHaveNoAliases(t *testing.T) {
	root := newRootCommand(strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	root.InitDefaultHelpCmd()
	want := map[string]string{
		"ls": "browse", "search": "browse", "tui": "browse", "gui": "browse",
		"upload": "files", "download": "files", "copy": "files", "extract": "files", "delete": "files", "recycle": "files", "revision": "files",
		"share": "sharing", "link": "sharing", "mount": "local", "recover": "local",
		"account": "account", "quota": "account", "completion": "tools", "help": "tools", "version": "tools",
	}
	if len(root.Commands()) != len(want) {
		t.Fatal("unexpected top-level command count", len(root.Commands()))
	}
	for _, command := range root.Commands() {
		if command.GroupID != want[command.Name()] || len(command.Aliases) > 0 {
			t.Fatal("command is misplaced or aliased", command.Name(), command.GroupID)
		}
		for _, subcommand := range command.Commands() {
			if len(subcommand.Aliases) > 0 || subcommand.Name() == "list" {
				t.Fatal("nested command has an alias or list spelling", subcommand.Name())
			}
		}
	}
	out, stderr, err := testExecute("__complete", "")
	if err != nil || strings.Contains(stderr, "Using account") || strings.Contains(out, "util\t") {
		t.Fatal("removed group remains in completion", out, stderr, err)
	}
	for _, name := range []string{"quota", "search", "copy", "share", "link"} {
		if !strings.Contains(out, name+"\t") {
			t.Fatal("top-level completion omits command", name)
		}
	}
}
func TestNonInteractiveCredentialsNeverPrompt(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, args := range [][]string{{"ls", "--non-interactive"}, {"account", "add", "--non-interactive", "--username", "test-user"}} {
		_, _, err := testExecute(args...)
		if exitCode(err) != 1 || !strings.Contains(err.Error(), "provide") {
			t.Fatalf("error=%v", err)
		}
	}
}
func TestRecoverEmptyAndRecords(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "missing")
	for _, cache := range []string{root, missing} {
		out, _, err := testExecute("recover", "--json", "--cache-dir", cache)
		if err != nil || strings.TrimSpace(out) != "[]" {
			t.Fatalf("empty JSON=%q err=%v", out, err)
		}
		out, _, err = testExecute("recover", "--cache-dir", cache)
		if err != nil || !strings.Contains(out, "No uncommitted") {
			t.Fatal(out, err)
		}
	}
	cache := filepath.Join(root, "mount-test")
	os.Mkdir(cache, 0700)
	os.WriteFile(filepath.Join(cache, "data-file"), []byte("test"), 0600)
	record := vfs.RecoveryRecord{Account: "work", Server: "https://disk.pku.edu.cn", Entry: anyshare.Entry{ID: "gns://library/file", Name: "report.txt", Size: 4, Rev: "rev"}, Parent: "gns://library", Data: "data-file"}
	data, _ := json.Marshal(record)
	os.WriteFile(filepath.Join(cache, "data-file.json"), data, 0600)
	out, _, err := testExecute("recover", "--cache-dir", root)
	if err != nil || !strings.Contains(out, filepath.Join(cache, "data-file")) || !strings.Contains(out, "report.txt") || !strings.Contains(out, "copy") {
		t.Fatal(out, err)
	}
	if !strings.Contains(out, "Account:    work (https://disk.pku.edu.cn)") {
		t.Fatal("recovery output omits the account identity", out)
	}
	out, _, err = testExecute("recover", "--cache-dir", root, "-b")
	if err != nil || !strings.Contains(out, "Size:       4 bytes") {
		t.Fatal("recover exact size format", out, err)
	}
	out, _, err = testExecute("recover", "--cache-dir", root, "--json")
	var records []vfs.RecoveryRecord
	if err != nil || json.Unmarshal([]byte(out), &records) != nil || len(records) != 1 {
		t.Fatal(out, err)
	}
	if records[0].Data != filepath.Join(cache, "data-file") || records[0].Account != "work" || records[0].Server != record.Server {
		t.Fatal("recover omitted absolute data path or account identity")
	}
}
func TestProgressRedirectedProducesNoEscapes(t *testing.T) {
	var out bytes.Buffer
	progress := newCLIProgress("upload", &out, sizeFormat{})
	for _, phase := range []transfer.Phase{transfer.Transferring, transfer.Finalizing, transfer.Complete} {
		progress.Update(transfer.Progress{Done: 4, Total: 4, Phase: phase})
	}
	progress.Finish(nil)
	if out.Len() != 0 {
		t.Fatal("redirected progress contaminated output")
	}
}
