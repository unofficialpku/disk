package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
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

// Opt-in acceptance uses the saved account and only mutates a private temporary
// fixture. Matched searches require existing index entries; an empty result never
// counts as successful parser/metadata validation.
func TestLiveTransfers(t *testing.T) {
	if os.Getenv("PKUDISK_TEST_LIVE") != "1" {
		t.Skip("set PKUDISK_TEST_LIVE=1 to exercise the saved account and temporary remote data")
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
	roots, err := client.Roots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var library anyshare.Entry
	for _, root := range roots {
		if root.Type == "user_doc_lib" {
			library = root
			break
		}
	}
	if library.ID == "" {
		t.Fatal("a writable personal library is needed for the temporary acceptance fixture")
	}
	fixture, err := client.Mkdir(ctx, library.ID, fmt.Sprintf("pkudisk-e2e-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		clean, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := client.Delete(clean, fixture); err != nil {
			t.Error("remote fixture cleanup:", err)
		} else {
			t.Log("temporary remote fixture removed")
		}
	}()
	local := t.TempDir()
	source := filepath.Join(local, "source")
	for _, folder := range []string{"test-nested", "test-empty"} {
		if err := os.MkdirAll(filepath.Join(source, folder), 0755); err != nil {
			t.Fatal(err)
		}
	}
	content := map[string][]byte{"test.txt": []byte("test bytes\n"), "test 中文.md": []byte("中文 filename\n"), "README": {}, "test-nested/test.bin": bytes.Repeat([]byte("multipart-acceptance\n"), (21<<20)/21)}
	for name, data := range content {
		if err := os.WriteFile(filepath.Join(source, filepath.FromSlash(name)), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) (string, string, error) {
		args = append(args, "--account", account, "--non-interactive", "--io-timeout", "2m")
		var out, stderr bytes.Buffer
		if binary := os.Getenv("PKUDISK_TEST_BINARY"); binary != "" {
			cmd := exec.CommandContext(ctx, binary, args...)
			cmd.Stdout = &out
			cmd.Stderr = &stderr
			err = cmd.Run()
		} else {
			err = execute(ctx, args, strings.NewReader(""), &out, &stderr)
		}
		return out.String(), stderr.String(), err
	}
	mustRun := func(args ...string) string {
		t.Helper()
		out, stderr, err := run(args...)
		if err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(args[:min(len(args), 2)], " "), err, stderr)
		}
		if strings.Contains(out, "Using account") || !strings.Contains(stderr, "Using account") {
			t.Fatal("account announcement/output streams are mixed")
		}
		return out
	}
	remote := "/" + library.Name + "/" + fixture.Name + "/source"
	mustRun("upload", source, remote)
	selected, err := client.Resolve(ctx, remote)
	if err != nil {
		t.Fatal(err)
	}
	verifyTree := func(folder string) {
		t.Helper()
		for name, want := range content {
			got, err := os.ReadFile(filepath.Join(folder, filepath.FromSlash(name)))
			if err != nil || sha256.Sum256(got) != sha256.Sum256(want) {
				t.Fatalf("round-trip hash failed for %s: %v", name, err)
			}
		}
		if info, err := os.Stat(filepath.Join(folder, "test-empty")); err != nil || !info.IsDir() {
			t.Fatal("empty folder was lost", err)
		}
	}
	t.Run("matched-search", func(t *testing.T) {
		type page struct {
			Hits  int `json:"hits"`
			Files []struct {
				ID, Name string
				Size     int64
				Dir      bool      `json:"directory"`
				Modified time.Time `json:"modified_at"`
				Path     string    `json:"path"`
			} `json:"files"`
		}
		decode := func(output string) page {
			t.Helper()
			var result page
			if err := json.Unmarshal([]byte(output), &result); err != nil {
				t.Fatal(err)
			}
			return result
		}
		// Reproduce the reported command and require real metadata, not zero hits.
		global := decode(mustRun("search", "test", "--json", "--limit", "25"))
		if len(global.Files) == 0 {
			t.Fatal("matched live search unexpectedly returned no entries")
		}
		for _, entry := range global.Files {
			if entry.ID == "" || entry.Modified.IsZero() || entry.Modified.Year() > 2100 {
				t.Fatal("matched search lost ID or timestamp precision")
			}
		}
		// Search indexes can lag newly uploaded data by minutes. Use a known
		// indexed match for scope/format acceptance; transfer fixtures remain
		// independent and cannot turn an empty search into a passing test.
		matchedLibrary := anyshare.LibraryID(global.Files[0].ID)
		var searchRoot anyshare.Entry
		for _, root := range roots {
			if root.ID == matchedLibrary {
				searchRoot = root
				break
			}
		}
		if searchRoot.ID == "" {
			t.Fatal("search returned an inaccessible library")
		}
		result := decode(mustRun("search", "test", "--in", searchRoot.ID, "--json", "--limit", "100"))
		if len(result.Files) == 0 {
			t.Fatal("scoped matched search unexpectedly returned no entries")
		}
		path := "/" + searchRoot.Name
		text := mustRun("search", "test", "--in", path, "--limit", "100")
		if !strings.Contains(text, result.Files[0].Name) || strings.Contains(text, "EiB") {
			t.Fatal("matched text search output is invalid")
		}
		limited := decode(mustRun("search", "test", "--in", path, "--json", "--limit", "1"))
		if len(limited.Files) != 1 {
			t.Fatal("search limit was ignored")
		}
		folders := decode(mustRun("search", "e", "--json", "--limit", "100"))
		sawDir := false
		for _, entry := range folders.Files {
			if entry.Dir {
				if entry.Size != -1 || entry.Modified.IsZero() {
					t.Fatal("folder sentinel or timestamp was lost")
				}
				sawDir = true
			}
		}
		if !sawDir {
			t.Fatal("live matched search must exercise at least one indexed folder")
		}
		absent := mustRun("search", fixture.Name+"-not-present", "--in", selected.ID, "--json")
		if !strings.Contains(absent, `"files":[]`) {
			t.Fatal("empty JSON result is not an array")
		}
		t.Log("global/scoped file and folder matches, text/JSON, timestamps, empty page and limit verified")
	})
	t.Run("transfers", func(t *testing.T) {
		direct := filepath.Join(local, "direct")
		mustRun("download", selected.ID, direct)
		verifyTree(direct)
		copied := filepath.Join(local, "copy.pdf")
		mustRun("download", remote+"/test.txt", copied)
		_, _, err := run("download", remote+"/test.txt", copied)
		if err == nil {
			t.Fatal("existing local destination overwritten without a flag")
		}
		mustRun("download", remote+"/test.txt", copied, "--overwrite")
		formatted := mustRun("download", remote+"/test.txt", copied, "-f")
		exact := mustRun("download", remote+"/test.txt", copied, "-f", "-b", "-t", "30s")
		if !strings.Contains(formatted, "11 B") || !strings.Contains(exact, "11 bytes") {
			t.Fatal("transfer size modes differ from actual bytes")
		}

		packed := filepath.Join(local, "source.zip")
		mustRun("download", selected.ID, packed, "--archive")
		reader, err := zip.OpenReader(packed)
		if err != nil {
			t.Fatal(err)
		}
		for _, member := range reader.File {
			if member.FileInfo().IsDir() {
				continue
			}
			name := strings.TrimPrefix(member.Name, "source/")
			want, ok := content[name]
			if !ok {
				t.Fatal("unexpected ZIP member")
			}
			r, err := member.Open()
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(r)
			r.Close()
			if err != nil || sha256.Sum256(got) != sha256.Sum256(want) {
				t.Fatal("ZIP hash mismatch", name, err)
			}
		}
		reader.Close()
		via := filepath.Join(local, "via")
		mustRun("download", selected.ID, via, "--via-archive")
		verifyTree(via)
		t.Log("recursive/multipart upload, direct/ZIP/local extraction downloads and overwrite verified")
	})
	t.Run("server-copy", func(t *testing.T) {
		dest, err := client.Mkdir(ctx, fixture.ID, "copied")
		if err != nil {
			t.Fatal(err)
		}
		mustRun("copy", remote+"/test.txt", dest.ID)
		destPath := "/" + library.Name + "/" + fixture.Name + "/copied"
		out := filepath.Join(local, "server-file")
		mustRun("download", destPath+"/test.txt", out)
		got, err := os.ReadFile(out)
		if err != nil || !bytes.Equal(got, content["test.txt"]) {
			t.Fatal("server file copy changed content", err)
		}
		if _, _, err := run("copy", remote+"/test.txt", dest.ID); err == nil {
			t.Fatal("duplicate server copy unexpectedly replaced a file")
		}
		mustRun("copy", selected.ID, dest.ID)
		tree := filepath.Join(local, "server-tree")
		mustRun("download", destPath+"/source", tree)
		verifyTree(tree)
		// Same-folder duplication must print the renamed output instead of implying
		// that it replaced the original.
		output := mustRun("copy", remote+"/test.txt", selected.ID)
		if strings.HasSuffix(strings.TrimSpace(output), "/test.txt") {
			t.Fatal("same-folder copy output uses the source's name")
		}
		t.Log("file/directory/same-folder server copies and duplicate conflict verified")
	})
	t.Run("quota", func(t *testing.T) {
		quota, err := client.Quota(ctx)
		if err != nil {
			t.Fatal(err)
		}
		readable := mustRun("quota")
		if !strings.Contains(readable, "GiB") && !strings.Contains(readable, "TiB") {
			t.Fatal("quota is not readable by default")
		}
		text := mustRun("quota", "-b")
		if !strings.Contains(text, fmt.Sprintf("Allocated: %d", quota.Allocated)) {
			t.Fatal("quota CLI differs from library aggregate")
		}
		t.Log("library aggregate quota and CLI output verified")
	})
	if t.Failed() {
		return
	}
	if _, err := client.Resolve(ctx, fixture.ID); errors.Is(err, anyshare.ErrNotFound) {
		t.Fatal("acceptance fixture disappeared before cleanup")
	}
}
