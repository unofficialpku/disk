package main

import (
	"encoding/json"
	"fmt"
	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/unofficialpku/disk/internal/anyshare/mockserver"
	"strings"
	"testing"
)

func TestManagementCommandsRequireExplicitScriptConsent(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, args := range [][]string{
		{"delete", "/Library/file", "--non-interactive"},
		{"delete", "--non-interactive", "-y"},
		{"recycle", "remove", "gns://lib/bin/file", "--non-interactive"},
		{"recycle", "empty", "--non-interactive"},
		{"recycle", "restore", "--non-interactive"},
		{"recycle", "restore", "gns://lib/bin/file", "--non-interactive"},
		{"revision", "ls", "--non-interactive"},
		{"revision", "restore", "/Library/file", "old", "--non-interactive"},
		{"revision", "restore", "/Library/file", "--non-interactive", "-y"},
		{"recycle", "list", "--help"}, {"revision", "list", "--help"},
	} {
		out, stderr, err := testExecute(args...)
		if exitCode(err) != 2 || out != "" || strings.Contains(stderr, "Using account") {
			t.Fatal(args, out, stderr, err)
		}
	}
}

// Drive parsing, account restore, real HTTP payloads and both output streams
// together. Recycle IDs deliberately differ from the active document IDs.
func TestManagementCommandsThroughHTTP(t *testing.T) {
	server := commandFixture(t)
	const lib = mockserver.Library
	server.ReplaceObjects("fixture", []mockserver.Object{
		{Entry: anyshare.Entry{ID: lib, Name: "Library", Size: -1, Dir: true, Type: "user_doc_lib"}},
		{Entry: anyshare.Entry{ID: lib + "/file", Name: "file.txt", Size: 1536, Rev: "current"}},
		{Entry: anyshare.Entry{ID: lib + "/folder", Name: "Folder", Size: -1, Dir: true}},
	})
	server.ReplaceRecycled("fixture", nil)
	run := func(args ...string) (string, error) {
		t.Helper()
		out, stderr, err := testExecute(append(args, "--non-interactive")...)
		if !strings.Contains(stderr, `Using account "fixture"`) || strings.Contains(out, "Using account") {
			t.Fatal(out, stderr, err)
		}
		return out, err
	}
	mustRun := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatal(args, err)
		}
		return out
	}
	if _, err := run("delete", "/Library", "-ry"); err == nil || mutationCount(server, "fixture:delete") != 0 {
		t.Fatal("library root deleted")
	}
	if _, err := run("delete", "/Library/Folder", "-y"); exitCode(err) != 2 || mutationCount(server, "fixture:delete") != 0 {
		t.Fatal("directory deleted without recursive consent", err)
	}
	if text := mustRun("revision", "ls", "/Library/file.txt"); !strings.Contains(text, "1.5 KiB") || !strings.Contains(text, "CURRENT") || !strings.Contains(text, "old") {
		t.Fatal(text)
	}
	if text := mustRun("revision", "ls", "/Library/file.txt", "-b"); strings.Contains(text, "KiB") || !strings.Contains(text, "1536") {
		t.Fatal(text)
	}
	mustRun("revision", "restore", "/Library/file.txt", "old", "-y")
	if _, err := run("revision", "restore", "/Library/file.txt", "missing", "-y"); err == nil || mutationCount(server, "revision:old") != 1 {
		t.Fatal("unknown version restored")
	}
	mustRun("delete", "/Library/file.txt", "-y")
	var entries []anyshare.RecycleEntry
	if err := json.Unmarshal([]byte(mustRun("recycle", "ls", "/Library", "-j")), &entries); err != nil || len(entries) != 1 || entries[0].ID != lib+"/bin/file" || entries[0].Size != 1536 || entries[0].Modified.IsZero() {
		t.Fatal(entries, err)
	}
	if text := mustRun("recycle", "ls"); !strings.Contains(text, "1.5 KiB") || !strings.Contains(text, "ORIGINAL PATH") {
		t.Fatal(text)
	}
	if text := mustRun("recycle", "ls", "-b"); strings.Contains(text, "KiB") || !strings.Contains(text, "1536") {
		t.Fatal(text)
	}
	mustRun("recycle", "restore", lib+"/bin/file", "--rename", "-y")
	if mutationCount(server, "restore") != 1 {
		t.Fatal("restore count")
	}
	mustRun("delete", "/Library/Folder", "-ry")
	mustRun("recycle", "remove", lib+"/bin/folder", "-y")
	if mutationCount(server, "purge") != 1 {
		t.Fatal("permanent removal count")
	}
	if out := mustRun("recycle", "ls", "-j"); strings.TrimSpace(out) != "[]" {
		t.Fatal(out)
	}
	// A concurrent arrival after the first delete must remain in the bin.
	snapshot := []mockserver.Object{}
	for i := 0; i < 101; i++ {
		id := fmt.Sprintf("%s/bin/old-%03d", lib, i)
		snapshot = append(snapshot, mockserver.Object{Entry: anyshare.Entry{ID: id, Name: fmt.Sprintf("old-%03d", i), Size: -2}})
	}
	server.ReplaceRecycled("fixture", snapshot)
	server.ArriveAfterDelete("fixture", mockserver.Object{Entry: anyshare.Entry{ID: lib + "/bin/new", Name: "New concurrent deletion", Size: 1}})
	before := len(server.Requests())
	text := mustRun("recycle", "empty", "/Library", "-y")
	remaining := server.Recycled("fixture")
	offsets := []int{}
	for _, request := range server.Requests()[before:] {
		if request.Path == "/api/efast/v1/recycle/list" {
			offsets = append(offsets, int(request.Body["start"].(float64)))
		}
	}
	if !strings.Contains(text, "101") || mutationCount(server, "purge") != 102 || len(remaining) != 1 || remaining[0].Entry.ID != lib+"/bin/new" || len(offsets) != 2 || offsets[1] != 100 {
		t.Fatal("empty failed snapshot/pagination guarantees", text, remaining, offsets)
	}
}
