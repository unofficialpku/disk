package main

import (
	"encoding/json"
	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/unofficialpku/disk/internal/anyshare/mockserver"
	"strings"
	"testing"
)

func TestSharingCLIThroughAuthenticatedHTTP(t *testing.T) {
	server := commandFixture(t)
	const library = mockserver.Library
	const file = library + "/file"
	server.ReplaceObjects("fixture", []mockserver.Object{
		{Entry: anyshare.Entry{ID: library, Name: "Library", Size: -1, Dir: true, Type: "user_doc_lib"}},
		{Entry: anyshare.Entry{ID: file, Name: "报告.txt", Size: 4, Rev: "1"}},
	})
	run := func(args ...string) string {
		t.Helper()
		out, stderr, err := testExecute(append(args, "-a", "fixture", "--non-interactive")...)
		if err != nil || !strings.Contains(stderr, `Using account "fixture"`) || strings.Contains(out, "Using account") {
			t.Fatal(args, out, stderr, err)
		}
		return out
	}
	var recipients []anyshare.Recipient
	if out := run("share", "search", "李", "-j"); json.Unmarshal([]byte(out), &recipients) != nil || len(recipients) != 2 || recipients[0].ID != "recipient" {
		t.Fatal(out)
	}
	if out := run("share", "link", "/Library/报告.txt"); !strings.Contains(out, "/anyshare/zh-cn/link/realname-link?") || mutationCount(server, "grant") != 0 {
		t.Fatal("navigation link granted access", out)
	}
	run("share", "add", "/Library/报告.txt", "--user", "recipient", "--permission", "download", "--expires-in", "1h")
	if out := run("share", "ls", file); !strings.Contains(out, "李同学") || !strings.Contains(out, "direct") || mutationCount(server, "grant") != 1 {
		t.Fatal(out)
	}
	run("share", "remove", file, "--user", "recipient")
	if mutationCount(server, "revoke") != 1 {
		t.Fatal("grant not revoked")
	}
	var links []anyshare.ShareLink
	if out := run("link", "add", file, "--code", "Code1234", "--expires-in", "1h", "--limit", "3", "-j"); json.Unmarshal([]byte(out), &links) != nil || len(links) != 1 || links[0].Code != "Code1234" {
		t.Fatal(out)
	}
	if out := run("link", "ls", file); !strings.Contains(out, "Code1234") || !strings.Contains(out, "anonymous-link") {
		t.Fatal(out)
	}
	run("link", "remove", "anonymous-link")
	if out := run("link", "ls", file, "-j"); strings.TrimSpace(out) != "[]" {
		t.Fatal("empty links must remain an array", out)
	}
	if len(server.Mutations()) != 5 {
		t.Fatal("unexpected extra mutations", server.Mutations())
	}
	for _, request := range server.Requests() {
		if request.Path == "/api/doc-share/v1/doc-perm" {
			configs := request.Body["configs"].([]any)
			if request.Body["doc_id"] != file || len(configs) != 1 || configs[0].(map[string]any)["accessor"].(map[string]any)["id"] != "recipient" {
				t.Fatal("wrong selected recipient", request.Body)
			}
		}
	}
}

func TestSharingRequiredInputsAndHelpStayOffline(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, args := range [][]string{
		{"share", "add"}, {"link", "add"},
		{"share", "add", "path", "--non-interactive"}, {"share", "remove", "path"},
		{"link", "remove", "--non-interactive"},
		{"share", "search", "name", "--limit", "0"},
		{"link", "add", "path", "--limit", "0"},
		{"link", "add", "path", "--expires-in", "-1h"},
		{"link", "add", "path", "--code", "abc"},
		{"share", "add", "path", "--user", "u", "--permission", "owner"},
		{"share", "add", "path", "--interactive"},
		{"link", "add", "path", "-i"},
		{"share", "list", "path"}, {"links"}, {"shares"},
	} {
		out, stderr, err := testExecute(args...)
		if exitCode(err) != 2 || out != "" || stderr != "" {
			t.Fatal("missing/invalid input reached authentication or prompted", args, out, stderr, err)
		}
	}
	for _, group := range []string{"share", "link"} {
		command := newShareCommand()
		if group == "link" {
			command = newLinkCommand()
		}
		for _, sub := range command.Commands() {
			out, stderr, err := testExecute(group, sub.Name(), "-h")
			if err != nil || stderr != "" || !strings.Contains(out, "Examples:") || strings.Contains(out, "--interactive") || !strings.Contains(out, "--non-interactive") {
				t.Fatal("offline help or interaction policy inconsistent", out, stderr, err)
			}
		}
	}
}
