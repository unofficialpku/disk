package gui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/unofficialpku/disk/internal/localfile"
	"github.com/unofficialpku/disk/internal/vfs"
)

type fileActionInput struct {
	Account     string `json:"account"`
	Path        string `json:"path"`
	Action      string `json:"action"`
	Name        string `json:"name"`
	Destination string `json:"destination"`
	Confirm     bool   `json:"confirm"`
}

func (a *Application) fileAction(r *http.Request, input fileActionInput) (any, error) {
	if input.Action == "delete" && !input.Confirm {
		return nil, invalid("confirmation required")
	}
	c, err := a.account(r.Context(), input.Account)
	if err != nil {
		return nil, err
	}
	entry, err := c.Client.Resolve(r.Context(), input.Path)
	if err != nil {
		return nil, err
	}
	var result any
	switch input.Action {
	case "mkdir":
		if !entry.Dir {
			err = invalid("select a directory")
		} else {
			result, err = c.Client.Mkdir(r.Context(), entry.ID, input.Name)
		}
	case "rename":
		result, err = c.Client.Rename(r.Context(), entry, input.Name, false)
	case "move":
		var destination anyshare.Entry
		destination, err = c.Client.Resolve(r.Context(), input.Destination)
		if err == nil {
			if !destination.Dir {
				err = invalid("select a destination directory")
			} else {
				result, err = c.Client.Move(r.Context(), entry, destination.ID)
			}
		}
	case "delete":
		err = c.Client.Delete(r.Context(), entry)
	default:
		err = invalid("unknown file operation")
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"entry": result}, nil
}

func (a *Application) recycle(w http.ResponseWriter, r *http.Request) {
	c := a.client(w, r, r.URL.Query().Get("account"))
	if c == nil {
		return
	}
	library := r.URL.Query().Get("library")
	if library == "" {
		rows, err := c.Client.RecycleLibraries(r.Context())
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, map[string]any{"libraries": rows, "account": c.Name})
		return
	}
	rows, err := c.Client.RecycleList(r.Context(), library)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]any{"entries": rows, "account": c.Name, "library": library})
}

type recycleActionInput struct {
	Account string   `json:"account"`
	Action  string   `json:"action"`
	IDs     []string `json:"ids"`
	Rename  bool     `json:"rename"`
	Confirm bool     `json:"confirm"`
}

func (a *Application) recycleAction(r *http.Request, input recycleActionInput) (any, error) {
	if !input.Confirm || len(input.IDs) == 0 || len(input.IDs) > 10000 {
		return nil, invalid("confirm a nonempty recycle-bin selection")
	}
	if input.Action != "restore" && input.Action != "remove" {
		return nil, invalid("unknown recycle-bin operation")
	}
	c, err := a.account(r.Context(), input.Account)
	if err != nil {
		return nil, err
	}
	// Emptying uses the IDs the user actually reviewed; a later arrival in the
	// recycle bin must not be silently included in the permanent deletion.
	view, err := a.newJob(jobInfo{Kind: "recycle_" + input.Action, Account: c.Name, Name: fmt.Sprintf("%d", len(input.IDs))}, 10*time.Minute, func(ctx context.Context, j *job) (any, error) {
		if input.Action == "remove" {
			snapshot := make([]anyshare.RecycleEntry, len(input.IDs))
			for i, id := range input.IDs {
				snapshot[i].ID = id
			}
			completed, err := c.Client.RecycleEmpty(ctx, snapshot, func(completed, total int) {
				a.mu.Lock()
				j.info.Done, j.info.Total, j.info.Percent = int64(completed), int64(total), 100*completed/total
				a.mu.Unlock()
			})
			return map[string]any{"completed": completed}, err
		}
		for i, id := range input.IDs {
			_, err := c.Client.RecycleRestore(ctx, id, input.Rename)
			if err != nil {
				return map[string]any{"completed": i}, err
			}
			a.mu.Lock()
			j.info.Done = int64(i + 1)
			j.info.Total = int64(len(input.IDs))
			j.info.Percent = 100 * (i + 1) / len(input.IDs)
			a.mu.Unlock()
		}
		return map[string]any{"completed": len(input.IDs)}, nil
	})
	if err != nil {
		return nil, err
	}
	return view, nil
}

func (a *Application) revisions(w http.ResponseWriter, r *http.Request) {
	c := a.client(w, r, r.URL.Query().Get("account"))
	if c == nil {
		return
	}
	entry, err := c.Client.Resolve(r.Context(), r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, err)
		return
	}
	rows, err := c.Client.Revisions(r.Context(), entry)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, rows)
}

type restoreRevisionInput struct {
	Account  string `json:"account"`
	Path     string `json:"path"`
	Revision string `json:"revision"`
	Confirm  bool   `json:"confirm"`
}

func (a *Application) restoreRevision(r *http.Request, input restoreRevisionInput) (any, error) {
	if !input.Confirm {
		return nil, invalid("confirmation required")
	}
	c, err := a.account(r.Context(), input.Account)
	if err != nil {
		return nil, err
	}
	entry, err := c.Client.Resolve(r.Context(), input.Path)
	if err == nil {
		err = c.Client.RestoreRevision(r.Context(), entry, input.Revision)
	}
	return nil, err
}

func (a *Application) recovery(w http.ResponseWriter, r *http.Request) {
	if !allowLocalFS(w) {
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		path = a.opts.CacheDirectory
	}
	path = localfile.ExpandPath(path)
	rows, err := vfs.RecoveryFiles(path)
	if err != nil {
		if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
			writeError(w, err)
			return
		}
	}
	if rows == nil {
		rows = []vfs.RecoveryRecord{}
	}
	writeJSON(w, map[string]any{"path": path, "entries": rows})
}
