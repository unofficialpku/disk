package transfer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/unofficialpku/disk/internal/anyshare"
)

func TestRemoteParent(t *testing.T) {
	tests := []struct {
		selector string
		parent   string
		name     string
	}{
		{"/Library/report.txt", "/Library", "report.txt"},
		{"Library/folder/report.txt", "Library/folder", "report.txt"},
		{"gns://00000000000000000000000000000001/folder/report.txt", "gns://00000000000000000000000000000001/folder", "report.txt"},
	}
	for _, test := range tests {
		parent, name, err := remoteParent(test.selector)
		if err != nil {
			t.Errorf("remoteParent(%q): %v", test.selector, err)
			continue
		}
		if parent != test.parent || name != test.name {
			t.Errorf("remoteParent(%q) = (%q, %q), want (%q, %q)", test.selector, parent, name, test.parent, test.name)
		}
	}
}

func TestRemoteParentRejectsUnsafeDestination(t *testing.T) {
	for _, selector := range []string{"", "/", "Library/..", "Library/", "report.txt"} {
		if _, _, err := remoteParent(selector); err == nil {
			t.Errorf("remoteParent(%q) accepted unsafe destination", selector)
		}
	}
}

type transferTestTokens struct{}

func (transferTestTokens) AccessToken() string                   { return "test-token" }
func (transferTestTokens) Refresh(context.Context, string) error { return nil }

func TestDownloadIsAtomicAndRefusesExistingDestination(t *testing.T) {
	content := []byte("downloaded bytes")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/efast/v1/file/osdownload":
			json.NewEncoder(w).Encode(map[string]any{"authrequest": []string{"GET", serverURL(r, "/object")}})
		case "/object":
			w.Header().Set("Content-Length", stringLength(len(content)))
			_, _ = w.Write(content)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := anyshare.New(server.URL, transferTestTokens{})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "out.bin")
	entry := anyshare.Entry{ID: "gns://root/file", Name: "remote.bin", Size: int64(len(content)), Rev: "rev-1"}
	if _, err := (Service{Backend: client}).DownloadEntry(context.Background(), entry, target, false, nil); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(target)
	if err != nil || string(b) != string(content) {
		t.Fatalf("downloaded content = %q, err = %v", b, err)
	}
	if err := os.WriteFile(target, []byte("keep me"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := (Service{Backend: client}).DownloadEntry(context.Background(), entry, target, false, nil); err == nil {
		t.Fatal("existing destination was overwritten without --overwrite")
	}
	b, err = os.ReadFile(target)
	if err != nil || string(b) != "keep me" {
		t.Fatalf("existing destination changed to %q, err = %v", b, err)
	}
	if _, err := (Service{Backend: client}).DownloadEntry(context.Background(), entry, target, true, nil); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(target)
	if err != nil || !bytes.Equal(b, content) {
		t.Fatalf("explicit overwrite content = %q, err = %v", b, err)
	}
}

func serverURL(r *http.Request, p string) string {
	// The test server is reachable through the Host header and the request's
	// URL does not retain its scheme, so use the local HTTP origin explicitly.
	return "http://" + r.Host + p
}

func stringLength(n int) string {
	if n == 0 {
		return "0"
	}
	buf := make([]byte, 0, 20)
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	return string(buf)
}

type fixtureBackend struct {
	entries    map[string]anyshare.Entry
	data       []byte
	downloaded func(context.Context, anyshare.Entry, io.Writer) error
	uploaded   func(context.Context, io.ReaderAt, int64) (anyshare.Entry, error)
	archive    func(context.Context, anyshare.Entry, io.Writer, func(anyshare.ArchiveProgress)) (int64, error)
	extract    func(context.Context, anyshare.Entry, string, string, func(int)) (anyshare.Entry, error)
}

func (b *fixtureBackend) Resolve(_ context.Context, selector string) (anyshare.Entry, error) {
	if entry, ok := b.entries[selector]; ok {
		return entry, nil
	}
	for _, entry := range b.entries {
		if entry.ID == selector {
			return entry, nil
		}
	}
	return anyshare.Entry{}, anyshare.ErrNotFound
}
func (b *fixtureBackend) List(_ context.Context, parent string) ([]anyshare.Entry, error) {
	var entries []anyshare.Entry
	for _, entry := range b.entries {
		if anyshare.ParentID(entry.ID) == parent {
			entries = append(entries, entry)
		}
	}
	return entries, nil
}
func (b *fixtureBackend) Upload(ctx context.Context, parent, name string, existing *anyshare.Entry, data io.ReaderAt, size int64, mtime time.Time) (anyshare.Entry, error) {
	return b.uploaded(ctx, data, size)
}
func (b *fixtureBackend) Download(ctx context.Context, entry anyshare.Entry, dest io.Writer) error {
	if b.downloaded != nil {
		return b.downloaded(ctx, entry, dest)
	}
	_, err := dest.Write(b.data)
	return err
}
func TestUploadPublicationProgress(t *testing.T) {
	root := anyshare.Entry{ID: "gns://library", Name: "Library", Dir: true, Size: -1}
	source := filepath.Join(t.TempDir(), "source.bin")
	content := []byte("multipart file contents")
	if err := os.WriteFile(source, content, 0600); err != nil {
		t.Fatal(err)
	}
	for _, fail := range []bool{false, true} {
		var states []Progress
		backend := &fixtureBackend{entries: map[string]anyshare.Entry{"/Library": root}, uploaded: func(ctx context.Context, data io.ReaderAt, size int64) (anyshare.Entry, error) {
			// Read separate ranges as the multipart transport does.
			for off := int64(0); off < size; off += 8 {
				if _, err := io.Copy(io.Discard, io.NewSectionReader(data, off, min(8, size-off))); err != nil {
					return anyshare.Entry{}, err
				}
			}
			if states[len(states)-1].Phase != Finalizing {
				t.Error("progress declared completion before upload publication")
			}
			if fail {
				return anyshare.Entry{}, errors.New("publication failed")
			}
			return anyshare.Entry{ID: root.ID + "/file", Name: "source.bin", Size: size}, nil
		}}
		_, err := (Service{Backend: backend}).Upload(context.Background(), source, "/Library", false, func(p Progress) { states = append(states, p) })
		if (err != nil) != fail {
			t.Fatalf("fail=%v err=%v", fail, err)
		}
		last := states[len(states)-1]
		if (last.Phase == Complete) == fail || last.Done != int64(len(content)) {
			t.Fatalf("bad publication state: %+v", last)
		}
	}
}
func TestCancelledDownloadPreservesDestination(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "existing.txt")
	if err := os.WriteFile(target, []byte("keep me"), 0600); err != nil {
		t.Fatal(err)
	}
	entry := anyshare.Entry{ID: "gns://library/file", Name: "report.txt", Size: 4}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	backend := &fixtureBackend{downloaded: func(_ context.Context, e anyshare.Entry, dest io.Writer) error {
		_, err := dest.Write([]byte("test"))
		cancel()
		return err
	}}
	var states []Progress
	_, err := (Service{Backend: backend}).DownloadEntry(ctx, entry, target, true, func(p Progress) { states = append(states, p) })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "keep me" {
		t.Fatalf("content=%q err=%v", content, err)
	}
	if states[len(states)-1].Phase == Complete {
		t.Fatal("cancelled file installation was shown as complete")
	}
	matches, _ := filepath.Glob(filepath.Join(dir, ".pkudisk-download-*"))
	if len(matches) != 0 {
		t.Fatal("temporary download leaked")
	}
}
func TestDestinationCreatedDuringDownloadIsPreserved(t *testing.T) {
	target := filepath.Join(t.TempDir(), "report.txt")
	entry := anyshare.Entry{ID: "gns://library/file", Name: "report.txt", Size: 4}
	backend := &fixtureBackend{downloaded: func(_ context.Context, e anyshare.Entry, dest io.Writer) error {
		if err := os.WriteFile(target, []byte("created concurrently"), 0600); err != nil {
			return err
		}
		_, err := dest.Write([]byte("test"))
		return err
	}}
	_, err := (Service{Backend: backend}).DownloadEntry(context.Background(), entry, target, false, nil)
	if !errors.Is(err, os.ErrExist) {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(target)
	if string(content) != "created concurrently" {
		t.Fatal("concurrent destination overwritten")
	}
}
func TestDownloadByIDUsesRemoteName(t *testing.T) {
	entry := anyshare.Entry{ID: "gns://library/file", Name: "report.txt", Size: 4}
	backend := &fixtureBackend{entries: map[string]anyshare.Entry{entry.ID: entry}, data: []byte("test")}
	// An explicit local directory exercises the same file-name selection
	// without changing the process working directory.
	directory := t.TempDir()
	result, err := (Service{Backend: backend}).Download(context.Background(), entry.ID, directory, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(result.Local) != entry.Name {
		t.Fatal("gns object ID was used as the local name")
	}
}
