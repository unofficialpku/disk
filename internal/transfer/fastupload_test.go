package transfer

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/unofficialpku/disk/internal/anyshare"
)

type fastFixture struct {
	*fixtureBackend
	match     bool
	fastCalls int
	fastErr   error
}

func (b *fastFixture) FastUpload(_ context.Context, parent, name string, data io.ReaderAt, size int64, _ time.Time, observe func(int64)) (bool, anyshare.Entry, error) {
	b.fastCalls++
	if b.fastErr != nil {
		return false, anyshare.Entry{}, b.fastErr
	}
	if observe != nil {
		observe(0)
		observe(size)
	}
	return b.match, anyshare.Entry{ID: parent + "/instant", Name: name, Size: size}, nil
}

func TestFastTransferMatchFallbackAndOverwrite(t *testing.T) {
	for _, mode := range []string{"match", "miss", "overwrite", "error"} {
		t.Run(mode, func(t *testing.T) {
			local := filepath.Join(t.TempDir(), "source.txt")
			os.WriteFile(local, []byte("file content"), 0600)
			uploads := 0
			backend := &fastFixture{fixtureBackend: &fixtureBackend{entries: map[string]anyshare.Entry{"/Library": {ID: "gns://lib", Dir: true, Name: "Library"}}}, match: mode == "match"}
			backend.uploaded = func(_ context.Context, r io.ReaderAt, size int64) (anyshare.Entry, error) {
				uploads++
				data, err := io.ReadAll(io.NewSectionReader(r, 0, size))
				if string(data) != "file content" {
					t.Error("ordinary upload lost bytes")
				}
				return anyshare.Entry{ID: "gns://lib/file", Name: "source.txt", Size: size}, err
			}
			if mode == "overwrite" {
				backend.entries["old"] = anyshare.Entry{ID: "gns://lib/file", Name: "source.txt", Size: 4, Rev: "old"}
			}
			if mode == "error" {
				backend.fastErr = anyshare.ErrPermission
			}
			var states []Progress
			result, err := (Service{Backend: backend, Fast: true}).Upload(context.Background(), local, "/Library", true, func(p Progress) { states = append(states, p) })
			if mode == "error" {
				if !errors.Is(err, anyshare.ErrPermission) || uploads != 0 {
					t.Fatal("API error silently fell back", err, uploads)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "match" {
				if result.FastFiles != 1 || uploads != 0 {
					t.Fatal(result, uploads)
				}
			} else if result.FastFiles != 0 || uploads != 1 {
				t.Fatal(result, uploads)
			}
			if mode == "overwrite" && backend.fastCalls != 0 {
				t.Fatal("fast overwrite bypasses revision check")
			}
			last := states[len(states)-1]
			if last.Phase != Complete || last.Done != int64(len("file content")) || last.Total != last.Done {
				t.Fatal("hash reads corrupted transfer totals", states)
			}
		})
	}
}

func TestFastDirectoryTransfer(t *testing.T) {
	local := filepath.Join(t.TempDir(), "tree")
	os.Mkdir(local, 0700)
	for _, name := range []string{"one.txt", "two.txt"} {
		os.WriteFile(filepath.Join(local, name), []byte("data"), 0600)
	}
	backend := &fastFixture{fixtureBackend: &fixtureBackend{entries: map[string]anyshare.Entry{"/Library": {ID: "gns://lib", Name: "Library", Dir: true}}}, match: true}
	result, err := (Service{Backend: backend, Fast: true}).Upload(context.Background(), local, "/Library", false, nil)
	if err != nil || result.FastFiles != 2 || result.Files != 2 || result.Bytes != 8 || backend.fastCalls != 2 {
		t.Fatal(result, err)
	}
}
