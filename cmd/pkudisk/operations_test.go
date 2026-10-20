package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/unofficialpku/disk/internal/anyshare/mockserver"
)

// Command parsing and both output streams use the same authenticated protocol
// fixture as GUI and mobile tests. Wire edge cases live in anyshare tests.
func TestOperationCommandsThroughHTTP(t *testing.T) {
	server := commandFixture(t)
	const lib = mockserver.Library
	stamp := time.UnixMicro(1753084919050792)
	server.ReplaceObjects("fixture", []mockserver.Object{
		{Entry: anyshare.Entry{ID: lib, Name: "Library", Size: -1, Dir: true, Type: "user_doc_lib"}},
		{Entry: anyshare.Entry{ID: lib + "/reports", Name: "Reports", Size: -1, Dir: true, Modified: stamp}},
		{Entry: anyshare.Entry{ID: lib + "/backup", Name: "Backup", Size: -1, Dir: true}},
		{Entry: anyshare.Entry{ID: lib + "/file", Name: "test.txt", Size: 212, Modified: stamp}},
	})
	server.SetQuota("fixture", anyshare.Quota{Allocated: 200, Used: 73})
	run := func(args ...string) string {
		t.Helper()
		out, stderr, err := testExecute(append(args, "--non-interactive")...)
		if err != nil || !strings.Contains(stderr, `Using account "fixture"`) || strings.Contains(out, "Using account") {
			t.Fatal(args, out, stderr, err)
		}
		return out
	}
	if text := run("ls"); !strings.Contains(text, "USED") || !strings.Contains(text, "36.5%") || !strings.Contains(text, "RW") {
		t.Fatal(text)
	}
	var libraries []anyshare.Entry
	if err := json.Unmarshal([]byte(run("ls", "-j")), &libraries); err != nil || len(libraries) != 1 || libraries[0].Size != -1 {
		t.Fatal(libraries, err)
	}
	if out := run("quota"); !strings.Contains(out, "Available: 127 B") {
		t.Fatal(out)
	}
	short, long := run("quota", "-b", "-a", "fixture", "-t", "30s"), run("quota", "--bytes", "--account", "fixture", "--io-timeout", "30s")
	if short != long || !strings.Contains(short, "Available: 127\n") {
		t.Fatal(short, long)
	}
	for _, scope := range []string{"", "/Library/Reports", lib + "/reports"} {
		args := []string{"search", "test", "--limit", "2"}
		if scope != "" {
			args = append(args, "--in", scope)
		}
		if out := run(args...); !strings.Contains(out, "test.txt") || strings.Contains(out, "EiB") {
			t.Fatal(out)
		}
		var result struct{ Files []anyshare.SearchEntry }
		if err := json.Unmarshal([]byte(run(append(args, "--json")...)), &result); err != nil || len(result.Files) != 1 || !result.Files[0].Modified.Equal(stamp) {
			t.Fatal(result, err)
		}
	}
	var folders struct{ Files []anyshare.SearchEntry }
	if err := json.Unmarshal([]byte(run("search", "Reports", "-j")), &folders); err != nil || len(folders.Files) != 1 || !folders.Files[0].Dir || folders.Files[0].Size != -1 {
		t.Fatal(folders, err)
	}
	if short, long := run("search", "test", "-bj", "-a", "fixture"), run("search", "test", "--json"); short != long {
		t.Fatal("JSON varies with size display flags", short, long)
	}
	var empty struct{ Files []anyshare.SearchEntry }
	if err := json.Unmarshal([]byte(run("search", "no-match", "--json")), &empty); err != nil || empty.Files == nil || len(empty.Files) != 0 {
		t.Fatal(empty, err)
	}
	if out := run("copy", "/Library/test.txt", "/Library/Backup"); !strings.Contains(out, "Copied") || mutationCount(server, "copy") != 1 {
		t.Fatal(out)
	}
	refreshes := 0
	for _, path := range server.Calls() {
		if path == "/anyshare/oauth2/login/refreshToken" {
			refreshes++
		}
	}
	if refreshes < 8 {
		t.Fatal("commands did not restore saved sessions")
	}
	for _, request := range server.Requests() {
		if request.Path == "/api/efast/v1/file/copy" && (request.Body["docid"] != lib+"/file" || request.Body["destparent"] != lib+"/backup") {
			t.Fatal("incorrect copy selectors", request.Body)
		}
		if request.Path == "/api/ecosearch/v1/file-search" {
			ranges := request.Body["range"].([]any)
			if len(ranges) > 0 && ranges[0] != lib+"/reports*" {
				t.Fatal("incorrect search scope", ranges)
			}
		}
	}
}
