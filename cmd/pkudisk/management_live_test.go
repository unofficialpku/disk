package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unofficialpku/disk/internal/anyshare"
)

// All mutations and permanent cleanup are restricted to freshly created data.
// In particular, no live recycle-empty command targets a user's existing bin.
func TestLiveFileManagement(t *testing.T) {
	if os.Getenv("PKUDISK_TEST_LIVE_MANAGEMENT") != "1" {
		t.Skip("set PKUDISK_TEST_LIVE_MANAGEMENT=1 to test disposable remote objects")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	session, server, account, err := authenticate(ctx, authFlags{account: os.Getenv("PKUDISK_TEST_ACCOUNT"), nonInteractive: true, output: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	client, err := anyshare.New(server, session)
	if err != nil {
		t.Fatal(err)
	}
	libs, err := client.RecycleLibraries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var library anyshare.Entry
	for _, lib := range libs {
		if lib.Type == "user_doc_lib" {
			library = lib
			break
		}
	}
	if library.ID == "" {
		t.Fatal("personal library required")
	}
	folder, err := client.Mkdir(ctx, library.ID, fmt.Sprintf("pkudisk-management-e2e-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		clean, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := client.Delete(clean, folder); err != nil {
			t.Error("fixture cleanup", err)
			return
		}
		entries, err := client.RecycleList(clean, library.ID)
		if err != nil {
			t.Error("cleanup listing", err)
			return
		}
		for _, entry := range entries {
			if entry.Name == folder.Name {
				if err := client.RecycleDelete(clean, entry.ID); err != nil {
					t.Error("permanent fixture cleanup", err)
				}
				return
			}
		}
		t.Error("cleanup fixture missing from recycle bin")
	}()
	run := func(args ...string) (string, error) {
		args = append(args, "-a", account, "--non-interactive", "-t", "45s")
		var out, stderr bytes.Buffer
		if binary := os.Getenv("PKUDISK_TEST_BINARY"); binary != "" {
			cmd := exec.CommandContext(ctx, binary, args...)
			cmd.Stdout, cmd.Stderr = &out, &stderr
			err = cmd.Run()
		} else {
			err = execute(ctx, args, strings.NewReader(""), &out, &stderr)
		}
		if err != nil {
			return out.String(), fmt.Errorf("%s: %w (%s)", strings.Join(args[:min(len(args), 3)], " "), err, stderr.String())
		}
		if !strings.Contains(stderr.String(), "Using account") || strings.Contains(out.String(), "Using account") {
			t.Fatal("incorrect output streams")
		}
		return out.String(), nil
	}
	mustRun := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	local := t.TempDir()
	payload := make([]byte, 1<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(local, "source.bin")
	if err := os.WriteFile(source, payload, 0600); err != nil {
		t.Fatal(err)
	}
	remoteFolder := "/" + library.Name + "/" + folder.Name
	remote := remoteFolder + "/source.bin"
	output := mustRun("upload", source, remote, "--fast")
	if strings.Contains(output, "instant upload") {
		t.Fatal("unique content should exercise ordinary-upload fallback")
	}
	entry, err := client.Resolve(ctx, remote)
	if err != nil {
		t.Fatal(err)
	}
	// The server's deduplication index does not immediately include each newly
	// uploaded object. Use content already verified by its predupload endpoint
	// for the hit branch; the random source independently exercises a miss.
	known := []byte("test")
	knownSource := filepath.Join(local, "known.bin")
	if err := os.WriteFile(knownSource, known, 0600); err != nil {
		t.Fatal(err)
	}
	output = mustRun("upload", knownSource, remoteFolder+"/instant.bin", "--fast")
	if !strings.Contains(output, "1 instant upload") {
		t.Fatal("known content did not hit instant upload", output)
	}
	target := filepath.Join(local, "roundtrip.bin")
	mustRun("download", remoteFolder+"/instant.bin", target)
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, known) {
		t.Fatal("instant-upload content differs", err)
	}
	t.Log("unique-content fallback and hash-match instant upload verified by round-trip bytes")

	updated := []byte("second historical version\n")
	if err := os.WriteFile(source, updated, 0600); err != nil {
		t.Fatal(err)
	}
	mustRun("upload", source, entry.ID, "--fast", "--overwrite")
	fresh, err := client.Resolve(ctx, entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("version IDs: original=%s updated=%s", entry.Rev, fresh.Rev)
	var versions []anyshare.Revision
	if err := json.Unmarshal([]byte(mustRun("revision", "ls", entry.ID, "-j")), &versions); err != nil {
		t.Fatal(err)
	}
	expectedCurrent := updated
	if len(versions) >= 2 {
		var original string
		for _, version := range versions {
			if version.Size == int64(len(payload)) {
				original = version.Rev
			}
		}
		if original == "" {
			t.Fatal("revision endpoint returned history without the original fixture")
		}
		mustRun("revision", "restore", entry.ID, original, "-y")
		mustRun("download", entry.ID, target, "--overwrite")
		got, err = os.ReadFile(target)
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatal("restored historical content differs", err)
		}
		expectedCurrent = payload
		t.Log("populated revision metadata, revision-checked overwrite and historical restore verified")
	} else {
		if len(versions) != 1 {
			t.Fatal("current version missing from live revision metadata")
		}
		mustRun("revision", "restore", entry.ID, versions[0].Rev, "-y")
		mustRun("download", entry.ID, target, "--overwrite")
		got, err = os.ReadFile(target)
		if err != nil || !bytes.Equal(got, updated) {
			t.Fatal("available-version restore changed contents", err)
		}
		t.Log("server retains only the current revision; listing and restore endpoint verified with that available version")
	}

	mustRun("delete", entry.ID, "-y")
	recycledID := func(name string) string {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			items, err := client.RecycleList(ctx, library.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range items {
				if item.Name == name && strings.Contains(item.Path, folder.Name) {
					return item.ID
				}
			}
			time.Sleep(2 * time.Second)
		}
		t.Fatal("fixture missing from recycle bin after eventual-consistency wait", name)
		return ""
	}
	firstDeletedID := recycledID(entry.Name)
	var recycled []anyshare.RecycleEntry
	if err := json.Unmarshal([]byte(mustRun("recycle", "ls", library.ID, "-j")), &recycled); err != nil {
		t.Fatal(err)
	}
	for _, item := range recycled {
		if item.ID == firstDeletedID && (item.Dir || item.Size != int64(len(expectedCurrent)) || item.Modified.IsZero()) {
			t.Fatal("recycle metadata incorrect", item)
		}
	}
	mustRun("recycle", "restore", firstDeletedID, "-y")
	mustRun("download", entry.ID, target, "--overwrite")
	got, err = os.ReadFile(target)
	if err != nil || !bytes.Equal(got, expectedCurrent) {
		t.Fatal("recycle restore changed content", err)
	}

	// Exercise a real same-name collision, then preserve both via --rename.
	mustRun("delete", entry.ID, "-y")
	deletedID := recycledID(entry.Name)
	mustRun("upload", source, remote)
	if _, err := run("recycle", "restore", deletedID, "-y"); err == nil {
		t.Fatal("restore collision overwrote the replacement")
	}
	mustRun("recycle", "restore", deletedID, "--rename", "-y")
	children, err := client.List(ctx, folder.ID)
	if err != nil {
		t.Fatal(err)
	}
	var restored anyshare.Entry
	for _, child := range children {
		if child.Name != "source.bin" && child.Name != "instant.bin" && child.Size == int64(len(expectedCurrent)) {
			restored = child
		}
	}
	if restored.ID == "" || restored.Name == "source.bin" {
		t.Fatal("collision restore failed to keep both files")
	}
	mustRun("delete", restored.ID, "-y")
	deletedID = recycledID(restored.Name)
	mustRun("recycle", "remove", deletedID, "-y")
	if _, err := run("recycle", "restore", deletedID, "-y"); err == nil {
		t.Fatal("permanently removed fixture could still be restored")
	}
	t.Log("delete/list/restore, collision rename and permanent removal verified")

	child, err := client.Mkdir(ctx, folder.ID, "subtree")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Upload(ctx, child.ID, "inside.txt", nil, bytes.NewReader(updated), int64(len(updated)), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := run("delete", child.ID, "-y"); err == nil {
		t.Fatal("directory deletion did not require --recursive")
	}
	mustRun("delete", child.ID, "-ry")
	mustRun("recycle", "restore", recycledID(child.Name), "-y")
	if entries, err := client.List(ctx, child.ID); err != nil || len(entries) != 1 {
		t.Fatal("directory subtree restore failed", err)
	}
	t.Log("recursive directory deletion and subtree restore verified")
}
