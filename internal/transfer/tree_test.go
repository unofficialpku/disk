package transfer

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/unofficialpku/disk/internal/anyshare"
)

func (b *fixtureBackend) Mkdir(_ context.Context, parent, name string) (anyshare.Entry, error) {
	for _, e := range b.entries {
		if anyshare.ParentID(e.ID) == parent && e.Name == name {
			return anyshare.Entry{}, anyshare.ErrExists
		}
	}
	entry := anyshare.Entry{ID: fmt.Sprintf("%s/dir%d", parent, len(b.entries)), Name: name, Dir: true, Size: -1}
	b.entries[entry.ID] = entry
	return entry, nil
}
func (b *fixtureBackend) DownloadArchive(ctx context.Context, e anyshare.Entry, w io.Writer, observe func(anyshare.ArchiveProgress)) (int64, error) {
	if b.archive == nil {
		return 0, errors.New("archive unavailable")
	}
	return b.archive(ctx, e, w, observe)
}
func (b *fixtureBackend) Extract(ctx context.Context, e anyshare.Entry, parent, name string, observe func(int)) (anyshare.Entry, error) {
	if b.extract == nil {
		return anyshare.Entry{}, errors.New("extract unavailable")
	}
	return b.extract(ctx, e, parent, name, observe)
}
func TestDirectoryRoundTripAndMerge(t *testing.T) {
	local := filepath.Join(t.TempDir(), "tree")
	for _, dir := range []string{"nested", "empty"} {
		if err := os.MkdirAll(filepath.Join(local, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	content := map[string]string{"hello.txt": "hello", "nested/中文.txt": "你好", "zero.txt": ""}
	for name, data := range content {
		if err := os.WriteFile(filepath.Join(local, filepath.FromSlash(name)), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	library := anyshare.Entry{ID: "gns://library", Name: "Library", Dir: true, Size: -1}
	backend := &fixtureBackend{entries: map[string]anyshare.Entry{"/Library": library}}
	data := map[string][]byte{}
	wrapped := &treeBackend{fixtureBackend: backend, data: data}
	service := Service{Backend: wrapped}
	var states []Progress
	uploaded, err := service.Upload(context.Background(), local, "/Library", false, func(p Progress) { states = append(states, p) })
	if err != nil {
		t.Fatal(err)
	}
	if uploaded.Files != 3 || uploaded.Dirs != 3 || uploaded.Bytes != 11 || states[len(states)-1].Phase != Complete {
		t.Fatalf("upload: %+v progress=%+v", uploaded, states)
	}
	destination := t.TempDir()
	downloaded, err := service.DownloadEntry(context.Background(), uploaded.Entry, destination, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range content {
		got, err := os.ReadFile(filepath.Join(downloaded.Local, filepath.FromSlash(name)))
		if err != nil || string(got) != want {
			t.Fatalf("%s=%q err=%v", name, got, err)
		}
	}
	if info, err := os.Stat(filepath.Join(downloaded.Local, "empty")); err != nil || !info.IsDir() {
		t.Fatal("empty folder lost", err)
	}
	if _, err := service.Upload(context.Background(), local, "/Library", false, nil); !errors.Is(err, anyshare.ErrExists) {
		t.Fatal(err)
	}
	unrelated := filepath.Join(downloaded.Local, "unrelated.txt")
	os.WriteFile(unrelated, []byte("keep"), 0600)
	if _, err := service.DownloadEntry(context.Background(), uploaded.Entry, destination, false, nil); !errors.Is(err, os.ErrExist) {
		t.Fatal(err)
	}
	if _, err := service.DownloadEntry(context.Background(), uploaded.Entry, destination, true, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(unrelated); string(got) != "keep" {
		t.Fatal("unrelated file removed")
	}
	if _, err := service.Upload(context.Background(), local, "/Library", true, nil); err != nil {
		t.Fatal(err)
	}
}

type treeBackend struct {
	*fixtureBackend
	data map[string][]byte
}

func (b *treeBackend) Upload(_ context.Context, parent, name string, existing *anyshare.Entry, reader io.ReaderAt, size int64, _ time.Time) (anyshare.Entry, error) {
	data, err := io.ReadAll(io.NewSectionReader(reader, 0, size))
	if err != nil {
		return anyshare.Entry{}, err
	}
	entry := anyshare.Entry{ID: fmt.Sprintf("%s/file%d", parent, len(b.entries)), Name: name, Size: size, Rev: "revision"}
	if existing != nil {
		entry.ID = existing.ID
	}
	b.entries[entry.ID] = entry
	b.data[entry.ID] = data
	return entry, nil
}
func (b *treeBackend) Download(ctx context.Context, e anyshare.Entry, w io.Writer) error {
	if b.downloaded != nil {
		return b.downloaded(ctx, e, w)
	}
	_, err := w.Write(b.data[e.ID])
	return err
}
func testZIP(t *testing.T, names map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	for name, data := range names {
		w, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
func zipBackend(data []byte) *fixtureBackend {
	return &fixtureBackend{archive: func(_ context.Context, _ anyshare.Entry, w io.Writer, observe func(anyshare.ArchiveProgress)) (int64, error) {
		if observe != nil {
			observe(anyshare.ArchiveProgress{Preparing: true, Percent: 50})
			observe(anyshare.ArchiveProgress{Total: int64(len(data))})
		}
		n, err := w.Write(data)
		if observe != nil {
			observe(anyshare.ArchiveProgress{Done: int64(n), Total: int64(len(data))})
		}
		return int64(n), err
	}}
}
func TestArchiveDownloadAsZIPAndFolder(t *testing.T) {
	data := testZIP(t, map[string]string{"tree/": "", "tree/empty/": "", "tree/nested/中文.txt": "hello"})
	entry := anyshare.Entry{ID: "gns://library/dir", Name: "tree", Dir: true}
	for _, archive := range []bool{false, true} {
		service := Service{Backend: zipBackend(data), Archive: archive, ViaArchive: !archive}
		local := t.TempDir()
		var states []Progress
		result, err := service.DownloadEntry(context.Background(), entry, local, false, func(p Progress) { states = append(states, p) })
		if err != nil {
			t.Fatal(err)
		}
		wantDirs := 3
		if archive {
			wantDirs = 2
		} // The saved ZIP has one implicit parent directory.
		if result.Files != 1 || result.Dirs != wantDirs || states[len(states)-1].Phase != Complete {
			t.Fatalf("%+v %+v", result, states)
		}
		if archive {
			got, err := os.ReadFile(result.Local)
			if err != nil || !bytes.Equal(got, data) || filepath.Base(result.Local) != "tree.zip" {
				t.Fatal(err)
			}
		} else {
			got, err := os.ReadFile(filepath.Join(result.Local, "nested", "中文.txt"))
			if err != nil || string(got) != "hello" {
				t.Fatal(err)
			}
			if info, err := os.Stat(filepath.Join(result.Local, "empty")); err != nil || !info.IsDir() {
				t.Fatal(err)
			}
		}
	}
}

func TestRemoteExtractionRejectsAmbiguousTree(t *testing.T) {
	backend := &fixtureBackend{entries: map[string]anyshare.Entry{
		"/Library":            {ID: "gns://lib", Name: "Library", Dir: true},
		"/Library/source.zip": {ID: "gns://lib/file", Name: "source.zip"},
	}}
	backend.extract = func(_ context.Context, _ anyshare.Entry, parent, name string, observe func(int)) (anyshare.Entry, error) {
		entry := anyshare.Entry{ID: parent + "/output", Name: name, Dir: true}
		backend.entries[entry.ID] = entry
		for _, id := range []string{"first", "second"} {
			child := anyshare.Entry{ID: entry.ID + "/" + id, Name: "nested", Dir: true}
			backend.entries[child.ID] = child
		}
		observe(100)
		return entry, nil
	}
	var states []Progress
	_, err := (Service{Backend: backend}).Extract(context.Background(), "/Library/source.zip", "/Library", func(p Progress) { states = append(states, p) })
	if err == nil || states[len(states)-1].Phase == Complete {
		t.Fatal("ambiguous server output was reported as complete", err)
	}
}
func TestUnsafeArchiveDoesNotTouchDestination(t *testing.T) {
	for _, name := range []string{"tree/../escape", "tree/nested/../../escape", "elsewhere/file", "tree/a\\b", "tree//file", "tree/./file"} {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			target := filepath.Join(parent, "target")
			service := Service{Backend: zipBackend(testZIP(t, map[string]string{name: "unsafe"})), ViaArchive: true}
			_, err := service.DownloadEntry(context.Background(), anyshare.Entry{ID: "gns://lib/dir", Name: "tree", Dir: true}, target, false, nil)
			if err == nil {
				t.Fatal("unsafe ZIP accepted")
			}
			if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("unsafe ZIP created destination")
			}
			files, _ := os.ReadDir(parent)
			if len(files) != 0 {
				t.Fatal("temporary file leaked")
			}
		})
	}
	// ZIP attributes must not allow a symlink to redirect subsequent writes.
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	h := &zip.FileHeader{Name: "tree/link"}
	h.SetMode(os.ModeSymlink | 0777)
	w, _ := writer.CreateHeader(h)
	io.WriteString(w, "../outside")
	writer.Close()
	reader, _ := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if _, _, _, err := archiveTree(context.Background(), reader, "tree"); err == nil {
		t.Fatal("symlink accepted")
	}
	data := testZIP(t, map[string]string{"tree/a": "file", "tree/a/child": "conflict"})
	reader, _ = zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if _, _, _, err := archiveTree(context.Background(), reader, "tree"); err == nil {
		t.Fatal("parent/file conflict accepted")
	}
}
func TestSymlinkUploadRejectedBeforeMutation(t *testing.T) {
	source := t.TempDir()
	if err := os.Symlink("outside", filepath.Join(source, "link")); err != nil {
		t.Skip(err)
	}
	library := anyshare.Entry{ID: "gns://lib", Name: "Library", Dir: true}
	backend := &fixtureBackend{entries: map[string]anyshare.Entry{"/Library": library}}
	if _, err := (Service{Backend: backend}).Upload(context.Background(), source, "/Library", false, nil); err == nil {
		t.Fatal("symlink uploaded")
	}
	if len(backend.entries) != 1 {
		t.Fatal("remote mutation before complete scan")
	}
}
func TestDirectoryCancellationPreservesExistingFile(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "tree")
	os.Mkdir(target, 0755)
	file := filepath.Join(target, "hello")
	os.WriteFile(file, []byte("keep"), 0600)
	root := anyshare.Entry{ID: "gns://lib/dir", Name: "tree", Dir: true}
	child := anyshare.Entry{ID: root.ID + "/file", Name: "hello", Size: 5}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	backend := &fixtureBackend{entries: map[string]anyshare.Entry{root.ID: root, child.ID: child}, downloaded: func(_ context.Context, _ anyshare.Entry, w io.Writer) error {
		_, err := io.WriteString(w, "hello")
		cancel()
		return err
	}}
	_, err := (Service{Backend: backend}).DownloadEntry(ctx, root, parent, true, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(file)
	if string(got) != "keep" {
		t.Fatal("cancelled overwrite changed existing file")
	}
	files, _ := os.ReadDir(target)
	if len(files) != 1 {
		t.Fatal("temporary files leaked")
	}
}

func TestCorruptZIPPreservesExistingFile(t *testing.T) {
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	w, _ := writer.CreateHeader(&zip.FileHeader{Name: "tree/file.txt", Method: zip.Store})
	io.WriteString(w, "replacement")
	writer.Close()
	data := buf.Bytes()
	data[bytes.Index(data, []byte("replacement"))] ^= 1 // Valid headers, invalid payload CRC.
	parent := t.TempDir()
	target := filepath.Join(parent, "tree")
	os.Mkdir(target, 0755)
	file := filepath.Join(target, "file.txt")
	os.WriteFile(file, []byte("keep"), 0600)
	_, err := (Service{Backend: zipBackend(data), ViaArchive: true}).DownloadEntry(context.Background(), anyshare.Entry{ID: "gns://lib/dir", Name: "tree", Dir: true}, parent, true, nil)
	if !errors.Is(err, zip.ErrChecksum) {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(file)
	if string(got) != "keep" {
		t.Fatal("corrupt archive replaced the existing file")
	}
	files, _ := os.ReadDir(target)
	if len(files) != 1 {
		t.Fatal("temporary extraction file leaked")
	}
}

func TestDirectoryDownloadRejectsDestinationSymlink(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "tree")
	os.Mkdir(target, 0755)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(target, "nested")); err != nil {
		t.Skip(err)
	}
	root := anyshare.Entry{ID: "gns://lib/dir", Name: "tree", Dir: true}
	child := anyshare.Entry{ID: root.ID + "/child", Name: "nested", Dir: true}
	file := anyshare.Entry{ID: child.ID + "/file", Name: "file.txt", Size: 4}
	backend := &fixtureBackend{entries: map[string]anyshare.Entry{root.ID: root, child.ID: child, file.ID: file}, data: []byte("test")}
	if _, err := (Service{Backend: backend}).DownloadEntry(context.Background(), root, parent, true, nil); err == nil {
		t.Fatal("destination symlink accepted")
	}
	files, _ := os.ReadDir(outside)
	if len(files) != 0 {
		t.Fatal("download escaped into symlink destination")
	}
}
