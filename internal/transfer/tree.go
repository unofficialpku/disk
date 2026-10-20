package transfer

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/unofficialpku/disk/internal/localfile"
)

type treeNode struct {
	rel   string
	entry anyshare.Entry
	info  fs.FileInfo
}

func scanLocal(ctx context.Context, local string) ([]treeNode, int64, error) {
	var nodes []treeNode
	var total int64
	err := filepath.WalkDir(local, func(name string, dir fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := dir.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("%s: only regular files and directories are uploaded; symlinks are not followed", name)
		}
		if !anyshare.ValidName(info.Name()) {
			return fmt.Errorf("%s: invalid remote name", name)
		}
		rel, err := filepath.Rel(local, name)
		if err != nil {
			return err
		}
		if rel == "." {
			rel = ""
		}
		entry := anyshare.Entry{Name: info.Name(), Dir: info.IsDir(), Size: info.Size()}
		if entry.Dir {
			entry.Size = -1
		} else if err := addSize(&total, entry.Size); err != nil {
			return err
		}
		nodes = append(nodes, treeNode{rel: rel, entry: entry, info: info})
		return nil
	})
	return nodes, total, err
}

func (s Service) scanRemote(ctx context.Context, entry anyshare.Entry) ([]treeNode, int64, error) {
	var nodes []treeNode
	var total int64
	seen := map[string]bool{}
	var walk func(anyshare.Entry, string) error
	walk = func(e anyshare.Entry, rel string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if seen[e.ID] {
			return errors.New("remote directory tree repeats an object ID")
		}
		seen[e.ID] = true
		if rel != "" && !localfile.ValidName(e.Name) {
			return fmt.Errorf("%q cannot be used as a local name on this platform", e.Name)
		}
		if !e.Dir {
			if err := addSize(&total, e.Size); err != nil {
				return err
			}
		}
		nodes = append(nodes, treeNode{rel: rel, entry: e})
		if !e.Dir {
			return nil
		}
		entries, err := s.Backend.List(ctx, e.ID)
		if err != nil {
			return err
		}
		names := map[string]bool{}
		for _, child := range entries {
			k := localNameKey(child.Name)
			if names[k] {
				return fmt.Errorf("duplicate local name %q in remote folder", child.Name)
			}
			names[k] = true
			if err := walk(child, filepath.Join(rel, child.Name)); err != nil {
				return err
			}
		}
		return nil
	}
	err := walk(entry, "")
	return nodes, total, err
}
func addSize(total *int64, size int64) error {
	if size < 0 || size > math.MaxInt64-*total {
		return errors.New("invalid or overflowing tree size")
	}
	*total += size
	return nil
}
func localNameKey(name string) string {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.ToLower(name)
	}
	return name
}
func preparing(observe Observer) {
	if observe != nil {
		observe(Progress{Phase: Preparing})
	}
}

func (s Service) uploadTree(ctx context.Context, local, remote string, overwrite bool, observe Observer) (Result, error) {
	result := Result{Local: local, Remote: remote}
	preparing(observe)
	nodes, total, err := scanLocal(ctx, local)
	if err != nil {
		return result, err
	}
	parent, name, existing, err := s.uploadDestination(ctx, filepath.Base(filepath.Clean(local)), remote)
	if err != nil {
		return result, err
	}
	if existing != nil {
		if !existing.Dir {
			return result, errors.New("a remote file already has the directory's destination name")
		}
		if !overwrite {
			return result, fmt.Errorf("%w: %q; directory merging requires --overwrite", anyshare.ErrExists, name)
		}
	}
	var root anyshare.Entry
	if existing != nil {
		root = *existing
	} else {
		root, err = s.Backend.Mkdir(ctx, parent, name)
		if err != nil {
			return result, err
		}
	}
	result.Entry = root
	progress := newReporter(total, observe)
	// One listing per destination directory also supplies the selected revisions.
	folders := map[string]anyshare.Entry{"": root}
	children := map[string]map[string]anyshare.Entry{}
	for _, node := range nodes {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if node.rel == "" {
			result.Dirs++
			continue
		}
		parentRel := filepath.Dir(node.rel)
		if parentRel == "." {
			parentRel = ""
		}
		folder := folders[parentRel]
		items, ok := children[parentRel]
		if !ok {
			entries, err := s.Backend.List(ctx, folder.ID)
			if err != nil {
				return result, err
			}
			items = map[string]anyshare.Entry{}
			for _, e := range entries {
				items[e.Name] = e
			}
			children[parentRel] = items
		}
		old, exists := items[node.entry.Name]
		if node.entry.Dir {
			if exists && !old.Dir {
				return result, fmt.Errorf("%s: remote file conflicts with directory", node.rel)
			}
			if !exists {
				old, err = s.Backend.Mkdir(ctx, folder.ID, node.entry.Name)
				if err != nil {
					return result, fmt.Errorf("%s: %w", node.rel, err)
				}
			}
			folders[node.rel] = old
			result.Dirs++
			continue
		}
		if exists && old.Dir {
			return result, fmt.Errorf("%s: remote directory conflicts with file", node.rel)
		}
		if exists && !overwrite {
			return result, fmt.Errorf("%w: %s", anyshare.ErrExists, node.rel)
		}
		var previous *anyshare.Entry
		if exists {
			previous = &old
		}
		file, err := os.Open(filepath.Join(local, node.rel))
		if err != nil {
			return result, err
		}
		fresh, statErr := file.Stat()
		if statErr != nil || !fresh.Mode().IsRegular() || fresh.Size() != node.entry.Size || !fresh.ModTime().Equal(node.info.ModTime()) {
			file.Close()
			return result, fmt.Errorf("%s: local file changed after scanning", node.rel)
		}
		_, fast, uploadErr := s.uploadFile(ctx, folder.ID, node.entry.Name, previous, file, node.entry.Size, node.info.ModTime(), progress)
		file.Close()
		if uploadErr != nil {
			return result, fmt.Errorf("%s: %w", node.rel, uploadErr)
		}
		if fast {
			result.FastFiles++
		}
		result.Bytes += node.entry.Size
		result.Files++
	}
	progress.complete()
	return result, nil
}

func localDestination(name, local string) (string, error) {
	local = localfile.ExpandPath(local)
	useName := local == ""
	if info, err := os.Lstat(local); err == nil && info.IsDir() {
		useName = true
	}
	if useName {
		if !localfile.ValidName(name) {
			return "", errors.New("remote name cannot be used locally on this platform; provide an explicit destination path")
		}
		local = filepath.Join(local, name)
	}
	return local, nil
}

func (s Service) downloadTree(ctx context.Context, entry anyshare.Entry, local string, overwrite bool, observe Observer) (Result, error) {
	result := Result{Entry: entry, Remote: entry.ID}
	var err error
	result.Local, err = localDestination(entry.Name, local)
	if err != nil {
		return result, err
	}
	preparing(observe)
	nodes, total, err := s.scanRemote(ctx, entry)
	if err != nil {
		return result, err
	}
	root, err := openTree(result.Local, nodes, overwrite)
	if err != nil {
		return result, err
	}
	defer root.Close()
	progress := newReporter(total, observe)
	for _, node := range nodes {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if node.entry.Dir {
			if node.rel != "" {
				if err := root.MkdirAll(node.rel, 0755); err != nil {
					return result, err
				}
			}
			result.Dirs++
			continue
		}
		err := installTreeFile(ctx, root, node.rel, node.entry.Size, overwrite, func(w io.Writer) error { return s.Backend.Download(ctx, node.entry, progressWriter{w, progress}) })
		if err != nil {
			return result, fmt.Errorf("%s: %w", node.rel, err)
		}
		result.Bytes += node.entry.Size
		result.Files++
	}
	progress.complete()
	return result, nil
}

// Root confines every write/rename even if another process changes a symlink
// after preflight. Reject existing symlinks rather than traversing them.
func openTree(local string, nodes []treeNode, overwrite bool) (*os.Root, error) {
	info, err := os.Lstat(local)
	if err == nil {
		if !info.IsDir() {
			return nil, errors.New("local directory destination is a file or symlink")
		}
		if !overwrite {
			return nil, fmt.Errorf("%w: %s; use --overwrite to merge the directory", os.ErrExist, local)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	// Validate existing paths before creating directories or downloading files.
	parent, err := os.OpenRoot(filepath.Dir(local))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	for _, node := range nodes {
		if node.rel == "" {
			continue
		}
		info, err := parent.Lstat(filepath.Join(filepath.Base(local), node.rel))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || info.IsDir() != node.entry.Dir || (!info.IsDir() && !info.Mode().IsRegular()) {
			return nil, fmt.Errorf("%s: local destination has an incompatible file, directory, or symlink", node.rel)
		}
	}
	if err := parent.Mkdir(filepath.Base(local), 0755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	return parent.OpenRoot(filepath.Base(local))
}

func installTreeFile(ctx context.Context, root *os.Root, name string, size int64, overwrite bool, write func(io.Writer) error) error {
	temp := ".pkudisk-download-" + rand.Text()
	file, err := root.OpenFile(temp, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(temp)
	defer file.Close()
	if err := write(file); err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() != size {
		return errors.New("download size differs from the selected file")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if overwrite {
		return root.Rename(temp, name)
	}
	return root.Link(temp, name)
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
