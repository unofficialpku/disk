package transfer

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/unofficialpku/disk/internal/localfile"
)

func (s Service) downloadArchive(ctx context.Context, entry anyshare.Entry, local string, overwrite bool, observe Observer) (Result, error) {
	result := Result{Entry: entry, Remote: entry.ID}
	if s.Archive && s.ViaArchive {
		return result, errors.New("choose --archive or --via-archive")
	}
	name := entry.Name
	if s.Archive {
		name += ".zip"
	}
	var err error
	result.Local, err = localDestination(name, local)
	if err != nil {
		return result, err
	}
	if _, err := os.Lstat(result.Local); err == nil && !overwrite {
		return result, fmt.Errorf("%w: %s; use --overwrite to replace a ZIP or merge a directory", os.ErrExist, result.Local)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	file, err := os.CreateTemp(filepath.Dir(result.Local), ".pkudisk-download-*.zip")
	if err != nil {
		return result, err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	preparing(observe)
	size, err := s.Backend.DownloadArchive(ctx, entry, file, func(p anyshare.ArchiveProgress) {
		if observe == nil {
			return
		}
		if p.Preparing {
			observe(Progress{Phase: Preparing, Percent: p.Percent})
		} else {
			observe(Progress{Phase: Transferring, Done: p.Done, Total: p.Total})
		}
	})
	if err != nil {
		return result, fmt.Errorf("download server ZIP (use download without archive flags for direct transfers): %w", err)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	reader, err := zip.NewReader(file, size)
	if err != nil {
		return result, fmt.Errorf("validate server ZIP: %w", err)
	}
	if s.Archive {
		if observe != nil {
			observe(Progress{Phase: Finalizing, Done: size, Total: size})
		}
		if err := file.Sync(); err != nil {
			return result, err
		}
		if err := file.Close(); err != nil {
			return result, err
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if err := localfile.Install(file.Name(), result.Local, overwrite); err != nil {
			return result, err
		}
		result.Bytes = size
		for _, member := range reader.File {
			if member.FileInfo().IsDir() {
				result.Dirs++
			} else {
				result.Files++
			}
		}
		if observe != nil {
			observe(Progress{Phase: Complete, Done: size, Total: size})
		}
		return result, nil
	}
	nodes, members, total, err := archiveTree(ctx, reader, entry.Name)
	if err != nil {
		return result, err
	}
	root, err := openTree(result.Local, nodes, overwrite)
	if err != nil {
		return result, err
	}
	defer root.Close()
	progress := newReporter(total, func(p Progress) {
		if p.Phase != Complete {
			p.Phase = Extracting
			if total > 0 {
				p.Percent = int(float64(p.Done) / float64(total) * 100)
			}
		}
		if observe != nil {
			observe(p)
		}
	})
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
		member := members[node.rel]
		err := installTreeFile(ctx, root, node.rel, node.entry.Size, overwrite, func(w io.Writer) error {
			source, err := member.Open()
			if err != nil {
				return err
			}
			defer source.Close()
			_, err = io.Copy(progressWriter{w, progress}, contextReader{ctx, source})
			return err
		})
		if err != nil {
			return result, fmt.Errorf("extract %s: %w", node.rel, err)
		}
		result.Files++
		result.Bytes += node.entry.Size
	}
	progress.complete()
	return result, nil
}

// Validate the whole ZIP before creating local paths. Only the selected root
// and regular files/directories are accepted; implicit parent dirs are added.
func archiveTree(ctx context.Context, reader *zip.Reader, name string) ([]treeNode, map[string]*zip.File, int64, error) {
	nodes := map[string]treeNode{"": {entry: anyshare.Entry{Name: name, Dir: true, Size: -1}}}
	members := map[string]*zip.File{}
	explicit := map[string]bool{}
	var total int64
	for _, member := range reader.File {
		if err := ctx.Err(); err != nil {
			return nil, nil, 0, err
		}
		if !strings.HasPrefix(member.Name, name+"/") {
			return nil, nil, 0, errors.New("server ZIP contains a path outside the selected directory")
		}
		rel := strings.TrimPrefix(member.Name, name+"/")
		isDir := member.FileInfo().IsDir()
		if isDir {
			rel = strings.TrimSuffix(rel, "/")
		}
		if strings.ContainsAny(rel, "\\\x00") || (rel == "" && !isDir) || (rel != "" && path.Clean(rel) != rel) {
			return nil, nil, 0, errors.New("server ZIP contains an unsafe path")
		}
		if !isDir && !member.Mode().IsRegular() {
			return nil, nil, 0, errors.New("server ZIP contains a symlink or special file")
		}
		for _, part := range strings.Split(rel, "/") {
			if rel != "" && !localfile.ValidName(part) {
				return nil, nil, 0, fmt.Errorf("ZIP member %q cannot be used locally on this platform", member.Name)
			}
		}
		key := localNameKey(rel)
		if explicit[key] {
			return nil, nil, 0, errors.New("server ZIP contains duplicate local paths")
		}
		explicit[key] = true
		old, exists := nodes[key]
		if exists && (old.rel != filepath.FromSlash(rel) || old.entry.Dir != isDir) {
			return nil, nil, 0, errors.New("server ZIP contains conflicting local paths")
		}
		size := int64(member.UncompressedSize64)
		if !isDir {
			if err := addSize(&total, size); err != nil {
				return nil, nil, 0, err
			}
		}
		nodes[key] = treeNode{rel: filepath.FromSlash(rel), entry: anyshare.Entry{Name: path.Base(rel), Dir: isDir, Size: size}}
		if !isDir {
			members[filepath.FromSlash(rel)] = member
		}
		for parent := path.Dir(rel); parent != "." && parent != "/"; parent = path.Dir(parent) {
			k := localNameKey(parent)
			if old, exists := nodes[k]; exists {
				if !old.entry.Dir || old.rel != filepath.FromSlash(parent) {
					return nil, nil, 0, errors.New("server ZIP contains conflicting parent paths")
				}
			} else {
				nodes[k] = treeNode{rel: filepath.FromSlash(parent), entry: anyshare.Entry{Name: path.Base(parent), Dir: true, Size: -1}}
			}
		}
	}
	if len(reader.File) == 0 {
		return nil, nil, 0, errors.New("server returned an empty ZIP without the selected directory")
	}
	result := make([]treeNode, 0, len(nodes))
	for _, node := range nodes {
		result = append(result, node)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].rel < result[j].rel })
	return result, members, total, nil
}

func (s Service) Extract(ctx context.Context, remote, destination string, observe Observer) (Result, error) {
	result := Result{Remote: destination}
	archive, err := s.Backend.Resolve(ctx, remote)
	if err != nil {
		return result, err
	}
	if archive.Dir {
		return result, errors.New("extract source must be a remote archive file")
	}
	name := archive.Name
	for _, suffix := range []string{".tar.gz", ".tar.bz2", ".tar.xz", ".zip", ".7z", ".rar", ".tar", ".tgz"} {
		if strings.HasSuffix(strings.ToLower(name), suffix) {
			name = name[:len(name)-len(suffix)]
			break
		}
	}
	parent, name, existing, err := s.uploadDestination(ctx, name, destination)
	if err != nil {
		return result, err
	}
	if existing != nil {
		return result, fmt.Errorf("%w: %q; extraction creates a new directory", anyshare.ErrExists, name)
	}
	entry, err := s.Backend.Extract(ctx, archive, parent, name, func(percent int) {
		if observe != nil {
			observe(Progress{Phase: Extracting, Percent: percent})
		}
	})
	result.Entry = entry
	if err != nil {
		return result, fmt.Errorf("%w; stopping polling does not cancel the server job", err)
	}
	nodes, total, err := s.scanRemote(ctx, entry)
	if err != nil {
		return result, fmt.Errorf("server reported extraction complete but the output tree is invalid: %w", err)
	}
	result.Bytes = total
	for _, node := range nodes {
		if node.entry.Dir {
			result.Dirs++
		} else {
			result.Files++
		}
	}
	if observe != nil {
		observe(Progress{Phase: Complete, Percent: 100})
	}
	return result, nil
}
