package gui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/unofficialpku/disk/internal/localfile"
	"github.com/unofficialpku/disk/internal/transfer"
	"github.com/unofficialpku/disk/internal/vfs"
)

type jobInfo struct {
	ID          string    `json:"id"`
	ClientID    string    `json:"clientId,omitempty"`
	Kind        string    `json:"kind"`
	Account     string    `json:"account"`
	AllAccounts bool      `json:"allAccounts"`
	Accounts    []string  `json:"accounts,omitempty"`
	Name        string    `json:"name"`
	Source      string    `json:"source"`
	Destination string    `json:"destination"`
	State       string    `json:"state"`
	Phase       string    `json:"phase"`
	Done        int64     `json:"done"`
	Total       int64     `json:"total"`
	Percent     int       `json:"percent"`
	Created     time.Time `json:"created"`
	Error       *apiError `json:"error,omitempty"`
	Result      any       `json:"result,omitempty"`
}

type job struct {
	info            jobInfo
	ctx             context.Context
	cancel          context.CancelFunc
	finished        bool
	stopMount       chan chan error
	done            chan struct{}
	responsePending bool
}

func osPlatform() string { return runtime.GOOS }

func (a *Application) newJob(info jobInfo, timeout time.Duration, run func(context.Context, *job) (any, error)) (jobInfo, error) {
	id, err := randomToken()
	if err != nil {
		return jobInfo{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closing {
		return jobInfo{}, ErrClosed
	}
	// Bound in-memory history without evicting running work.
	if len(a.jobs) >= 256 {
		oldest := ""
		for key, j := range a.jobs {
			if j.finished && !j.responsePending && (oldest == "" || j.info.Created.Before(a.jobs[oldest].info.Created)) {
				oldest = key
			}
		}
		if oldest == "" {
			return jobInfo{}, ErrBusy
		}
		delete(a.jobs, oldest)
	}
	if info.Kind == "mount" {
		for _, old := range a.jobs {
			if !old.finished && old.info.Kind == "mount" && sameLocalPath(old.info.Destination, info.Destination) {
				return jobInfo{}, ErrBusy
			}
		}
	}
	ctx, cancel := context.WithCancel(a.ctx)
	info.ID, info.State, info.Created = id, "queued", time.Now()
	j := &job{info: info, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	if info.Kind == "mount" {
		j.stopMount = make(chan chan error)
	}
	a.jobs[id] = j
	a.workers.Go(func() {
		defer cancel()
		var result any
		var err error
		if info.Kind != "mount" {
			err = a.slots.Acquire(ctx, 1)
			if err == nil {
				defer a.slots.Release(1)
			}
		}
		if err == nil {
			workCtx := ctx
			if timeout > 0 {
				var stop context.CancelFunc
				workCtx, stop = context.WithTimeout(ctx, timeout)
				defer stop()
			}
			a.mu.Lock()
			j.info.State = "running"
			a.mu.Unlock()
			result, err = run(workCtx, j)
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		defer close(j.done)
		j.finished = true
		j.info.Result = result
		j.info.State = "completed"
		if err != nil {
			j.info.State, j.info.Error = "failed", describeError(err)
			if errors.Is(err, context.Canceled) {
				j.info.State = "cancelled"
			}
		} else {
			j.info.Percent = 100
			j.info.Error = nil
		}
	})
	return info, nil
}

func sameLocalPath(left, right string) bool {
	left, _ = filepath.Abs(localfile.ExpandPath(left))
	right, _ = filepath.Abs(localfile.ExpandPath(right))
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func (a *Application) observe(j *job) transfer.Observer {
	return func(progress transfer.Progress) {
		a.mu.Lock()
		defer a.mu.Unlock()
		j.info.Done, j.info.Total, j.info.Phase = progress.Done, progress.Total, string(progress.Phase)
		j.info.Percent = progress.Percent
		if progress.Total > 0 {
			j.info.Percent = int(min(100, 100*float64(progress.Done)/float64(progress.Total)))
		}
	}
}

func (a *Application) listJobs(w http.ResponseWriter, r *http.Request) {
	snapshot := a.snapshotJobs()
	rows := make([]jobInfo, 0, len(snapshot))
	for _, item := range snapshot {
		rows = append(rows, item.info)
	}
	writeJSON(w, rows)
}

type jobSnapshot struct {
	info   jobInfo
	active bool
}

func (a *Application) snapshotJobs() []jobSnapshot {
	a.mu.Lock()
	rows := make([]jobSnapshot, 0, len(a.jobs))
	for _, j := range a.jobs {
		rows = append(rows, jobSnapshot{info: j.info, active: !j.finished || j.responsePending})
	}
	a.mu.Unlock()
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].info.Created.Equal(rows[j].info.Created) {
			return rows[i].info.ID > rows[j].info.ID
		}
		return rows[i].info.Created.After(rows[j].info.Created)
	})
	return rows
}

// Browser request retries and the WebView attachment handoff share one visible
// task. Keep cancellation/removal consistent with the frontend's coalesced row.
func sameJobAttempt(left, right jobInfo) bool {
	return left.ID == right.ID || left.ClientID != "" && left.Kind != "mount" &&
		left.ClientID == right.ClientID && left.Kind == right.Kind && left.Account == right.Account
}

func (a *Application) clearJob(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	j := a.jobs[r.PathValue("id")]
	if j == nil {
		writeError(w, anyshare.ErrNotFound)
		return
	}
	for _, candidate := range a.jobs {
		if sameJobAttempt(j.info, candidate.info) && (!candidate.finished || candidate.responsePending) {
			writeError(w, ErrBusy)
			return
		}
	}
	for id, candidate := range a.jobs {
		if sameJobAttempt(j.info, candidate.info) {
			delete(a.jobs, id)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Application) cancelJob(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	j := a.jobs[r.PathValue("id")]
	if j == nil {
		writeError(w, anyshare.ErrNotFound)
		return
	}
	if j.info.Kind != "mount" {
		for _, candidate := range a.jobs {
			if sameJobAttempt(j.info, candidate.info) && !candidate.finished && candidate.info.State != "stopping" {
				candidate.info.State = "stopping"
				candidate.cancel()
			}
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if j.finished || j.info.State == "stopping" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if j.info.State != "mounted" {
		j.info.State = "stopping"
		j.cancel()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Unmount errors keep the live mount in the registry and allow retry. A
	// browser disconnect must not abandon the driver's asynchronous response.
	j.info.State = "stopping"
	j.info.Error = nil
	a.workers.Go(func() {
		reply := make(chan error, 1)
		select {
		case j.stopMount <- reply:
		case <-j.ctx.Done():
			return
		}
		select {
		case err := <-reply:
			if err != nil {
				a.mu.Lock()
				if !j.finished {
					j.info.State = "mounted"
					j.info.Error = describeError(err)
				}
				a.mu.Unlock()
			}
		case <-j.ctx.Done():
		}
	})
	w.WriteHeader(http.StatusNoContent)
}

type operationInput struct {
	Kind           string `json:"kind"`
	Mode           string `json:"mode"`
	Account        string `json:"account"`
	Source         string `json:"source"`
	Destination    string `json:"destination"`
	Overwrite      bool   `json:"overwrite"`
	Fast           bool   `json:"fast"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
}

func (a *Application) startOperation(w http.ResponseWriter, r *http.Request) {
	var input operationInput
	if !decodeJSON(w, r, &input) {
		return
	}
	switch input.Kind {
	case "recover", "copy", "extract", "upload", "download":
	default:
		writeError(w, invalid("unknown transfer operation"))
		return
	}
	if input.Source == "" || input.Destination == "" {
		writeError(w, invalid("source and destination are required"))
		return
	}
	if input.Kind != "recover" && input.Kind != "upload" && input.Fast {
		writeError(w, invalid("unsupported transfer options"))
		return
	}
	if input.Kind == "upload" || input.Kind == "download" || input.Kind == "recover" {
		if !allowLocalFS(w) {
			return
		}
		local := input.Source
		if input.Kind == "download" {
			local = input.Destination
		}
		if !filepath.IsAbs(local) {
			writeError(w, invalid("choose an absolute local path"))
			return
		}
	}
	if input.Mode != "" && input.Mode != "direct" && input.Mode != "archive" && input.Mode != "viaArchive" {
		writeError(w, invalid("unknown download mode"))
		return
	}
	if input.Mode != "" && input.Kind != "download" {
		writeError(w, invalid("download mode is only valid for downloads"))
		return
	}
	if input.TimeoutSeconds == 0 {
		input.TimeoutSeconds = 600
	}
	if input.TimeoutSeconds < 1 || input.TimeoutSeconds > 86400 {
		writeError(w, invalid("timeout must be between 1 and 86400 seconds"))
		return
	}
	if input.Account == "" {
		config, err := a.opts.Store.Load()
		if err != nil {
			writeError(w, err)
			return
		}
		input.Account = config.Default
	}
	if input.Account == "" {
		writeError(w, ErrLoginRequired)
		return
	}
	kind := input.Kind
	if kind == "recover" {
		kind = "upload"
	}
	view, err := a.newJob(jobInfo{Kind: kind, Account: input.Account, Name: filepath.Base(input.Source), Source: input.Source, Destination: input.Destination}, time.Duration(input.TimeoutSeconds)*time.Second,
		func(ctx context.Context, j *job) (any, error) {
			c, err := a.account(ctx, input.Account)
			if err != nil {
				return nil, err
			}
			service := transfer.Service{Backend: c.Client, Fast: input.Fast, Archive: input.Mode == "archive", ViaArchive: input.Mode == "viaArchive"}
			var source anyshare.Entry
			if input.Kind != "recover" && input.Kind != "upload" {
				source, err = c.Client.Resolve(ctx, input.Source)
				if err != nil {
					return nil, err
				}
				a.mu.Lock()
				j.info.Name = source.Name
				a.mu.Unlock()
			}
			switch input.Kind {
			case "upload":
				return service.Upload(ctx, input.Source, input.Destination, input.Overwrite, a.observe(j))
			case "download":
				return service.Download(ctx, input.Source, input.Destination, input.Overwrite, a.observe(j))
			case "recover":
				path := localfile.ExpandPath(input.Source)
				records, err := vfs.RecoveryFiles(filepath.Dir(path))
				if err != nil {
					return nil, err
				}
				for _, record := range records {
					if record.Data != path {
						continue
					}
					file, err := os.Open(path)
					if err != nil {
						return nil, err
					}
					defer file.Close()
					stat, err := file.Stat()
					if err != nil {
						return nil, err
					}
					if !stat.Mode().IsRegular() {
						return nil, invalid("recovery data is not a regular file")
					}
					a.mu.Lock()
					j.info.Name = record.Entry.Name
					a.mu.Unlock()
					return service.UploadReader(ctx, record.Entry.Name, input.Destination, file, stat.Size(), stat.ModTime(), input.Overwrite, a.observe(j))
				}
				return nil, invalid("select a retained recovery record")
			case "extract":
				return service.Extract(ctx, input.Source, input.Destination, a.observe(j))
			case "copy":
				dest, err := c.Client.Resolve(ctx, input.Destination)
				if err != nil {
					return nil, err
				}
				return c.Client.Copy(ctx, source, dest, func(p anyshare.CopyProgress) {
					a.observe(j)(transfer.Progress{Phase: transfer.Preparing, Percent: p.Percent})
				})
			}
			return nil, invalid("unknown transfer operation")
		})
	if err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, view)
}

func (a *Application) startMount(w http.ResponseWriter, r *http.Request) {
	var input MountInput
	if !decodeJSON(w, r, &input) {
		return
	}
	if input.Point == "" || input.AllAccounts && (input.Root != "" || input.Account != "") {
		writeError(w, invalid("choose a mountpoint and either one account/root or all accounts"))
		return
	}
	if a.opts.CheckMount == nil || a.opts.Mount == nil {
		writeError(w, invalid("mounting is unavailable"))
		return
	}
	if a.opts.ValidateMount != nil {
		if err := a.opts.ValidateMount(input); err != nil {
			writeError(w, invalid(err.Error()))
			return
		}
	}
	if err := a.opts.CheckMount(); err != nil {
		writeError(w, &apiError{Code: "mount_unavailable", Message: err.Error()})
		return
	}
	config, err := a.opts.Store.Load()
	if err != nil {
		writeError(w, err)
		return
	}
	names := []string{input.Account}
	if input.Account == "" {
		names = []string{config.Default}
	}
	if input.AllAccounts {
		names = nil
		for _, saved := range config.Accounts {
			names = append(names, saved.Name)
		}
	}
	if len(names) == 0 || names[0] == "" {
		writeError(w, ErrLoginRequired)
		return
	}
	if !input.AllAccounts {
		input.Account = names[0]
	}
	view, err := a.newJob(jobInfo{Kind: "mount", Account: input.Account, AllAccounts: input.AllAccounts, Accounts: names, Name: input.Point, Source: input.Root, Destination: input.Point}, 0,
		func(ctx context.Context, j *job) (any, error) {
			connections := make([]*Connection, 0, len(names))
			for _, name := range names {
				c, err := a.account(ctx, name)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", name, err)
				}
				connections = append(connections, c)
			}
			err := a.opts.Mount(ctx, input, connections, func() { a.mu.Lock(); j.info.State = "mounted"; a.mu.Unlock() }, j.stopMount)
			return nil, err
		})
	if err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, view)
}
