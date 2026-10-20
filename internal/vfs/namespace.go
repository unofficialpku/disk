package vfs

import (
	"context"
	"github.com/unofficialpku/disk/internal/anyshare"
	"syscall"
)

// AnyShare exposes move and rename separately. Use a unique intermediate name
// and compensate on failure. Same-directory replacement uses one server call.
func (f *FileSystem) moveAndRename(ctx context.Context, e anyshare.Entry, oldParent, newParent, newName string, replace bool) (anyshare.Entry, error) {
	if libraryID(oldParent) != libraryID(newParent) {
		return anyshare.Entry{}, syscall.EXDEV
	}
	current := e
	var err error
	if e.Name != newName || replace {
		var renamed anyshare.Entry
		renamed, err = f.backend.Rename(ctx, current, RandomName(), false)
		if err != nil {
			return anyshare.Entry{}, err
		}
		current = renamed
	}
	var moved anyshare.Entry
	moved, err = f.backend.Move(ctx, current, newParent)
	if err == nil {
		current = moved
	}
	if err == nil && current.Name != newName {
		var renamed anyshare.Entry
		renamed, err = f.backend.Rename(ctx, current, newName, replace)
		if err == nil {
			current = renamed
		}
	}
	if err == nil {
		return current, nil
	}
	// Rollback deliberately has its own context, even if the caller was canceled.
	rollbackCtx, cancel := context.WithTimeout(context.Background(), f.opts.IOTimeout)
	defer cancel()
	rollbackErr := error(nil)
	if parentID(current.ID) != oldParent && current.ID != "" {
		var restored anyshare.Entry
		restored, rollbackErr = f.backend.Move(rollbackCtx, current, oldParent)
		if rollbackErr == nil {
			current = restored
		}
	}
	if rollbackErr == nil && current.ID != "" && current.Name != e.Name {
		_, rollbackErr = f.backend.Rename(rollbackCtx, current, e.Name, false)
	}
	if rollbackErr != nil {
		f.opts.Logger.Printf("cross-directory rename rollback failed: %v", rollbackErr)
	}
	return anyshare.Entry{}, err
}
