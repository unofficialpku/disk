package vfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/unofficialpku/disk/internal/anyshare"
	"io"
	"strings"
	"sync"
	"time"
)

type memoryBackend struct {
	mu         sync.Mutex
	entries    map[string]anyshare.Entry
	data       map[string][]byte
	serial     int
	uploads    int
	failUpload bool
	failMove   bool
	roots      []anyshare.Entry
	quota      anyshare.Quota
}

func newMemory() *memoryBackend {
	root := anyshare.Entry{ID: "gns://00000000000000000000000000000001", Name: "测试文档库", Dir: true, Size: -1, Rev: "1", Type: "user_doc_lib"}
	return &memoryBackend{entries: map[string]anyshare.Entry{root.ID: root}, data: map[string][]byte{}, roots: []anyshare.Entry{root}}
}
func (b *memoryBackend) Roots(ctx context.Context) ([]anyshare.Entry, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]anyshare.Entry(nil), b.roots...), nil
}
func (b *memoryBackend) StorageQuota(_ context.Context, _ string, readOnly bool) (anyshare.Quota, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	quota := b.quota
	if readOnly {
		quota.Allocated = quota.Used
	}
	return quota, nil
}
func (b *memoryBackend) List(ctx context.Context, id string) ([]anyshare.Entry, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.entries[id]; !ok {
		return nil, anyshare.ErrNotFound
	}
	var entries []anyshare.Entry
	for _, e := range b.entries {
		if anyshare.ParentID(e.ID) == id {
			entries = append(entries, e)
		}
	}
	return entries, nil
}
func (b *memoryBackend) Stat(ctx context.Context, e anyshare.Entry) (anyshare.Entry, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	v, ok := b.entries[e.ID]
	if !ok {
		return v, anyshare.ErrNotFound
	}
	return v, nil
}
func (b *memoryBackend) Can(context.Context, string, string) (bool, error) { return true, nil }
func (b *memoryBackend) ReadAt(ctx context.Context, e anyshare.Entry, d []byte, off int64) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	actual, ok := b.entries[e.ID]
	if !ok {
		return 0, anyshare.ErrNotFound
	}
	if actual.Rev != e.Rev {
		return 0, anyshare.ErrConflict
	}
	return bytes.NewReader(b.data[e.ID]).ReadAt(d, off)
}

func (b *memoryBackend) ReadRange(ctx context.Context, e anyshare.Entry, off, length int64, dest io.Writer) error {
	data := make([]byte, length)
	n, err := b.ReadAt(ctx, e, data, off)
	if err == io.EOF && n == len(data) {
		err = nil
	}
	if err != nil {
		return err
	}
	_, err = dest.Write(data[:n])
	return err
}
func (b *memoryBackend) Download(ctx context.Context, e anyshare.Entry, w io.Writer) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	actual, ok := b.entries[e.ID]
	if !ok {
		return anyshare.ErrNotFound
	}
	if actual.Rev != e.Rev {
		return anyshare.ErrConflict
	}
	_, err := w.Write(b.data[e.ID])
	return err
}
func (b *memoryBackend) Upload(ctx context.Context, parent, name string, existing *anyshare.Entry, r io.ReaderAt, size int64, mtime time.Time) (anyshare.Entry, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failUpload {
		return anyshare.Entry{}, errors.New("simulated interrupted upload")
	}
	id := ""
	if existing != nil {
		actual, ok := b.entries[existing.ID]
		if !ok {
			return anyshare.Entry{}, anyshare.ErrNotFound
		}
		if actual.Rev != existing.Rev {
			return anyshare.Entry{}, anyshare.ErrConflict
		}
		id = existing.ID
		name = actual.Name
	} else {
		for _, e := range b.entries {
			if anyshare.ParentID(e.ID) == parent && e.Name == name {
				return anyshare.Entry{}, anyshare.ErrExists
			}
		}
		b.serial++
		id = fmt.Sprintf("%s/%032x", parent, b.serial)
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(io.NewSectionReader(r, 0, size), data); err != nil {
		return anyshare.Entry{}, err
	}
	b.serial++
	e := anyshare.Entry{ID: id, Name: name, Size: size, Rev: fmt.Sprint(b.serial), Modified: mtime}
	b.entries[id] = e
	b.data[id] = data
	b.uploads++
	return e, nil
}
func (b *memoryBackend) Mkdir(ctx context.Context, parent, name string) (anyshare.Entry, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, e := range b.entries {
		if anyshare.ParentID(e.ID) == parent && e.Name == name {
			return anyshare.Entry{}, anyshare.ErrExists
		}
	}
	b.serial++
	e := anyshare.Entry{ID: fmt.Sprintf("%s/%032x", parent, b.serial), Name: name, Dir: true, Size: -1, Rev: fmt.Sprint(b.serial)}
	b.entries[e.ID] = e
	return e, nil
}
func (b *memoryBackend) Delete(ctx context.Context, e anyshare.Entry) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.entries[e.ID]; !ok {
		return anyshare.ErrNotFound
	}
	delete(b.entries, e.ID)
	delete(b.data, e.ID)
	return nil
}
func (b *memoryBackend) Rename(ctx context.Context, e anyshare.Entry, name string, replace bool) (anyshare.Entry, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	actual, ok := b.entries[e.ID]
	if !ok {
		return e, anyshare.ErrNotFound
	}
	for id, v := range b.entries {
		if id != e.ID && anyshare.ParentID(id) == anyshare.ParentID(e.ID) && v.Name == name {
			if !replace {
				return e, anyshare.ErrExists
			}
			delete(b.entries, id)
			delete(b.data, id)
		}
	}
	actual.Name = name
	b.entries[e.ID] = actual
	return actual, nil
}
func (b *memoryBackend) Move(ctx context.Context, e anyshare.Entry, parent string) (anyshare.Entry, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failMove {
		return anyshare.Entry{}, errors.New("simulated move failure")
	}
	actual, ok := b.entries[e.ID]
	if !ok {
		return e, anyshare.ErrNotFound
	}
	for _, v := range b.entries {
		if anyshare.ParentID(v.ID) == parent && v.Name == e.Name {
			return e, anyshare.ErrExists
		}
	}
	newID := parent + "/" + e.ID[strings.LastIndexByte(e.ID, '/')+1:]
	moved := map[string]anyshare.Entry{}
	data := map[string][]byte{}
	for id, v := range b.entries {
		if id == e.ID || strings.HasPrefix(id, e.ID+"/") {
			v.ID = newID + strings.TrimPrefix(id, e.ID)
			moved[v.ID] = v
			data[v.ID] = b.data[id]
			delete(b.entries, id)
			delete(b.data, id)
		}
	}
	for id, v := range moved {
		b.entries[id] = v
		b.data[id] = data[id]
	}
	actual.ID = newID
	return actual, nil
}
