// Package transfer provides file and directory operations shared by the CLI and TUI. It never
// prints or prompts; callers receive progress and decide how to present it.
package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/unofficialpku/disk/internal/localfile"
)

type Backend interface {
	Resolve(context.Context, string) (anyshare.Entry, error)
	List(context.Context, string) ([]anyshare.Entry, error)
	Upload(context.Context, string, string, *anyshare.Entry, io.ReaderAt, int64, time.Time) (anyshare.Entry, error)
	Download(context.Context, anyshare.Entry, io.Writer) error
	Mkdir(context.Context, string, string) (anyshare.Entry, error)
	DownloadArchive(context.Context, anyshare.Entry, io.Writer, func(anyshare.ArchiveProgress)) (int64, error)
	Extract(context.Context, anyshare.Entry, string, string, func(int)) (anyshare.Entry, error)
}
type Phase string

const (
	Preparing    Phase = "preparing"
	Hashing      Phase = "hashing"
	Transferring Phase = "transferring"
	Finalizing   Phase = "finalizing"
	Extracting   Phase = "extracting"
	Complete     Phase = "complete"
)

type Progress struct {
	Done, Total int64
	Phase       Phase
	Percent     int // Server job progress during Preparing/Extracting.
}
type Observer func(Progress)
type Result struct {
	Entry         anyshare.Entry
	Local, Remote string
	Bytes         int64
	Files, Dirs   int
	FastFiles     int
}
type Service struct {
	Backend             Backend
	Archive, ViaArchive bool
	Fast                bool
}

func (s Service) Upload(ctx context.Context, local, remote string, overwrite bool, observe Observer) (Result, error) {
	if local == "" {
		return Result{}, errors.New("upload source path is empty")
	}
	local, err := filepath.Abs(localfile.ExpandPath(local))
	result := Result{Local: local, Remote: remote}
	if err != nil {
		return result, err
	}
	if s.Archive || s.ViaArchive {
		return result, errors.New("archive modes are available for directory downloads; upload directory trees directly")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	info, err := os.Lstat(local)
	if err != nil {
		return result, err
	}
	if info.IsDir() {
		return s.uploadTree(ctx, local, remote, overwrite, observe)
	}
	if !info.Mode().IsRegular() {
		return result, errors.New("upload source must be a regular file or directory; symlinks are not followed")
	}
	file, err := os.Open(local)
	if err != nil {
		return result, err
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil {
		return result, err
	}
	if !info.Mode().IsRegular() {
		return result, errors.New("upload source must be a regular file")
	}
	result, err = s.UploadReader(ctx, filepath.Base(local), remote, file, info.Size(), info.ModTime(), overwrite, observe)
	result.Local = local
	return result, err
}

// UploadReader shares destination, conflict and fast-upload handling with file
// uploads. HTTP multipart.File implements ReaderAt without loading large files
// into memory or needing a browser's private filesystem path.
func (s Service) UploadReader(ctx context.Context, name, remote string, data io.ReaderAt, size int64, modified time.Time, overwrite bool, observe Observer) (Result, error) {
	result := Result{Remote: remote}
	if !anyshare.ValidName(name) || size < 0 || data == nil {
		return result, errors.New("invalid upload file")
	}
	parent, name, existing, err := s.uploadDestination(ctx, name, remote)
	if err != nil {
		return result, err
	}
	if existing != nil {
		if existing.Dir {
			return result, errors.New("a remote folder already has that name")
		}
		if !overwrite {
			return result, fmt.Errorf("%w: %q; use --overwrite to replace it", anyshare.ErrExists, name)
		}
	}
	progress := newReporter(size, observe)
	entry, fast, err := s.uploadFile(ctx, parent, name, existing, data, size, modified, progress)
	if err != nil {
		return result, err
	}
	result.Entry, result.Bytes, result.Files = entry, size, 1
	if fast {
		result.FastFiles = 1
	}
	progress.complete()
	return result, nil
}

type fastUploader interface {
	FastUpload(context.Context, string, string, io.ReaderAt, int64, time.Time, func(int64)) (bool, anyshare.Entry, error)
}

func (s Service) uploadFile(ctx context.Context, parent, name string, existing *anyshare.Entry, data io.ReaderAt, size int64, mtime time.Time, progress *reporter) (anyshare.Entry, bool, error) {
	if s.Fast && existing == nil {
		backend, ok := s.Backend.(fastUploader)
		if !ok {
			return anyshare.Entry{}, false, errors.New("fast upload is unavailable for this backend")
		}
		matched, entry, err := backend.FastUpload(ctx, parent, name, data, size, mtime, func(done int64) {
			if progress.observe != nil {
				progress.observe(Progress{Phase: Hashing, Done: done, Total: size})
			}
		})
		if err != nil {
			return anyshare.Entry{}, false, err
		}
		if matched {
			progress.addBytes(size)
			return entry, true, nil
		}
	}
	progress.resume()
	entry, err := s.Backend.Upload(ctx, parent, name, existing, progressReaderAt{data, progress}, size, mtime)
	return entry, false, err
}

func (s Service) uploadDestination(ctx context.Context, basename, remote string) (parent, name string, existing *anyshare.Entry, err error) {
	if remote == "" || remote == "/" {
		return "", "", nil, errors.New("upload destination must be inside a remote library")
	}
	candidate, resolveErr := s.Backend.Resolve(ctx, remote)
	if resolveErr == nil {
		if candidate.Dir {
			parent = candidate.ID
			name = basename
			entries, err := s.Backend.List(ctx, parent)
			if err != nil {
				return "", "", nil, err
			}
			for _, entry := range entries {
				if entry.Name == name {
					copy := entry
					existing = &copy
					break
				}
			}
		} else {
			parent = anyshare.ParentID(candidate.ID)
			name = candidate.Name
			existing = &candidate
		}
	} else if errors.Is(resolveErr, anyshare.ErrNotFound) {
		parentSelector, fileName, err := remoteParent(remote)
		if err != nil {
			return "", "", nil, err
		}
		folder, err := s.Backend.Resolve(ctx, parentSelector)
		if err != nil {
			return "", "", nil, fmt.Errorf("resolve destination parent: %w", err)
		}
		if !folder.Dir {
			return "", "", nil, errors.New("destination parent is not a remote folder")
		}
		parent = folder.ID
		name = fileName
	} else {
		return "", "", nil, resolveErr
	}
	if parent == "" || !anyshare.ValidName(name) {
		return "", "", nil, errors.New("invalid remote destination or file name")
	}
	return parent, name, existing, nil
}

func (s Service) Download(ctx context.Context, remote, local string, overwrite bool, observe Observer) (Result, error) {
	entry, err := s.Backend.Resolve(ctx, remote)
	if err != nil {
		return Result{Remote: remote, Local: local}, err
	}
	result, err := s.DownloadEntry(ctx, entry, local, overwrite, observe)
	result.Remote = remote
	return result, err
}
func (s Service) DownloadEntry(ctx context.Context, entry anyshare.Entry, local string, overwrite bool, observe Observer) (Result, error) {
	local = localfile.ExpandPath(local)
	result := Result{Entry: entry, Local: local, Remote: entry.ID}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if entry.Dir {
		if s.Archive || s.ViaArchive {
			return s.downloadArchive(ctx, entry, local, overwrite, observe)
		}
		return s.downloadTree(ctx, entry, local, overwrite, observe)
	}
	if s.Archive || s.ViaArchive {
		return result, errors.New("archive download flags require a remote directory")
	}
	if !anyshare.ValidName(entry.Name) || entry.Size < 0 {
		return result, errors.New("invalid remote file name or size")
	}
	if local == "" {
		if !localfile.ValidName(entry.Name) {
			return result, errors.New("remote name cannot be used as a local filename on this platform; provide an explicit local file path")
		}
		local = entry.Name
	}
	if info, err := os.Stat(local); err == nil && info.IsDir() {
		if !localfile.ValidName(entry.Name) {
			return result, errors.New("remote name cannot be used as a local filename on this platform; provide an explicit local file path")
		}
		local = filepath.Join(local, entry.Name)
	}
	result.Local = local
	if _, err := os.Lstat(local); err == nil && !overwrite {
		return result, fmt.Errorf("%w: %s; use --overwrite to replace it", os.ErrExist, local)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	file, err := os.CreateTemp(filepath.Dir(local), ".pkudisk-download-*")
	if err != nil {
		return result, err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	progress := newReporter(entry.Size, observe)
	if err = s.Backend.Download(ctx, entry, progressWriter{file, progress}); err != nil {
		return result, err
	}
	info, err := file.Stat()
	if err != nil {
		return result, err
	}
	if info.Size() != entry.Size {
		return result, errors.New("download size differs from the selected file")
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if err = file.Sync(); err != nil {
		return result, err
	}
	if err = file.Close(); err != nil {
		return result, err
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	err = localfile.Install(file.Name(), local, overwrite)
	if err != nil {
		return result, err
	}
	progress.complete()
	result.Bytes, result.Files = entry.Size, 1
	return result, nil
}

func remoteParent(selector string) (string, string, error) {
	selector = strings.TrimRight(selector, "/")
	at := strings.LastIndex(selector, "/")
	if at < 0 {
		return "", "", errors.New("upload destination must include an accessible library")
	}
	parent, name := selector[:at], selector[at+1:]
	if !anyshare.ValidName(name) || parent == "" || parent == "gns:" || parent == "gns:/" {
		return "", "", errors.New("invalid remote upload destination")
	}
	return parent, name, nil
}

type reporter struct {
	mu      sync.Mutex
	state   Progress
	observe Observer
}

func newReporter(total int64, observe Observer) *reporter {
	r := &reporter{state: Progress{Total: total, Phase: Transferring}, observe: observe}
	if total == 0 {
		r.state.Phase = Finalizing
	}
	if observe != nil {
		observe(r.state)
	}
	return r
}
func (r *reporter) add(n int) {
	r.addBytes(int64(n))
}
func (r *reporter) addBytes(n int64) {
	if n <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state.Phase == Complete {
		return
	}
	r.state.Done += min(n, r.state.Total-r.state.Done)
	if r.state.Done == r.state.Total {
		r.state.Phase = Finalizing
	}
	if r.observe != nil {
		r.observe(r.state)
	}
}
func (r *reporter) resume() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.observe != nil {
		r.observe(r.state)
	}
}
func (r *reporter) complete() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state.Done = r.state.Total
	r.state.Phase = Complete
	if r.observe != nil {
		r.observe(r.state)
	}
}

type progressReaderAt struct {
	io.ReaderAt
	progress *reporter
}

func (r progressReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := r.ReaderAt.ReadAt(p, off)
	r.progress.add(n)
	return n, err
}

type progressWriter struct {
	io.Writer
	progress *reporter
}

func (w progressWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	w.progress.add(n)
	return n, err
}
