package gui

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/unofficialpku/disk/internal/transfer"
)

type browserDownload struct {
	Account        string    `json:"account"`
	Path           string    `json:"path"`
	Archive        bool      `json:"archive"`
	TimeoutSeconds int       `json:"timeoutSeconds"`
	Expires        time.Time `json:"-"`
	ClientID       string    `json:"-"`
	Proof          [32]byte  `json:"-"`
	Uses           int       `json:"-"`
}

func transferTimeout(seconds int) (time.Duration, error) {
	if seconds == 0 {
		seconds = 600
	}
	if seconds < 1 || seconds > 86400 {
		return 0, invalid("timeout must be between 1 and 86400 seconds")
	}
	return time.Duration(seconds) * time.Second, nil
}

// The HTTP request owns its file/response resources until the job is joined.
// Disconnects cancel the operation; no body or writer outlives its handler.
func (a *Application) requestJob(r *http.Request, info jobInfo, timeout time.Duration, run func(context.Context, *job) (any, error)) (jobInfo, error) {
	view, err := a.newJob(info, timeout, func(ctx context.Context, j *job) (any, error) {
		a.mu.Lock()
		j.responsePending = true
		a.mu.Unlock()
		return run(ctx, j)
	})
	if err != nil {
		return view, err
	}
	a.mu.Lock()
	j := a.jobs[view.ID]
	a.mu.Unlock()
	if j == nil {
		return view, context.Canceled
	}
	stop := context.AfterFunc(r.Context(), j.cancel)
	defer stop()
	<-j.done
	a.mu.Lock()
	defer a.mu.Unlock()
	j.responsePending = false
	return j.info, nil
}

// Go's multipart parser provides a bounded-memory ReaderAt and private temporary
// files. Reuse the existing transfer service's hashing and revision checks.
func (a *Application) browserUpload(w http.ResponseWriter, r *http.Request) {
	if err := a.uploadSlots.Acquire(r.Context(), 1); err != nil {
		writeError(w, err)
		return
	}
	defer a.uploadSlots.Release(1)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeError(w, invalid("invalid multipart upload"))
		return
	}
	defer r.MultipartForm.RemoveAll()
	files := r.MultipartForm.File["file"]
	if len(files) != 1 || len(r.MultipartForm.File) != 1 {
		writeError(w, invalid("select one file per upload"))
		return
	}
	seconds, err := strconv.Atoi(r.FormValue("timeoutSeconds"))
	if err != nil {
		writeError(w, invalid("invalid timeout"))
		return
	}
	timeout, err := transferTimeout(seconds)
	if err != nil {
		writeError(w, err)
		return
	}
	relative := r.FormValue("relativePath")
	if relative == "" {
		relative = files[0].Filename
	}
	parts := strings.Split(relative, "/")
	if len(parts) > 128 || len(relative) > 32768 || strings.Contains(relative, `\`) {
		writeError(w, invalid("upload path is too deep"))
		return
	}
	for _, part := range parts {
		if !anyshare.ValidName(part) {
			writeError(w, invalid("invalid relative upload path"))
			return
		}
	}
	modified, err := strconv.ParseInt(r.FormValue("lastModified"), 10, 64)
	if err != nil || modified < 0 {
		writeError(w, invalid("invalid file timestamp"))
		return
	}
	data, err := files[0].Open()
	if err != nil {
		writeError(w, err)
		return
	}
	defer data.Close()
	account, destination := r.FormValue("account"), r.FormValue("destination")
	overwrite := r.FormValue("overwrite") == "true"
	info, err := a.requestJob(r, jobInfo{Kind: "upload", Account: account, ClientID: r.FormValue("clientId"), Name: relative, Source: relative, Destination: destination}, timeout, func(ctx context.Context, j *job) (any, error) {
		c, err := a.account(ctx, account)
		if err != nil {
			return nil, err
		}
		parent, err := a.uploadParent(ctx, c.Client, destination, parts[:len(parts)-1])
		if err != nil {
			return nil, err
		}
		service := transfer.Service{Backend: c.Client, Fast: r.FormValue("fast") == "true"}
		return service.UploadReader(ctx, parts[len(parts)-1], parent, data, files[0].Size, time.UnixMilli(modified), overwrite, a.observe(j))
	})
	if err != nil {
		writeError(w, err)
		return
	}
	if info.Error != nil {
		writeError(w, info.Error)
		return
	}
	writeJSON(w, info)
}

// Relative paths describe remote directories, never local filesystem paths.
// Serialize folder creation so parallel uploads into one tree cannot race.
func (a *Application) uploadParent(ctx context.Context, c *anyshare.Client, destination string, parts []string) (string, error) {
	a.uploadFolders.Lock()
	defer a.uploadFolders.Unlock()
	folder, err := c.Resolve(ctx, destination)
	if err != nil {
		return "", err
	}
	if !folder.Dir {
		return "", invalid("select a destination directory")
	}
	for _, name := range parts {
		entries, err := c.List(ctx, folder.ID)
		if err != nil {
			return "", err
		}
		var next *anyshare.Entry
		for _, entry := range entries {
			if entry.Name == name {
				next = &entry
				break
			}
		}
		if next != nil {
			if !next.Dir {
				return "", anyshare.ErrExists
			}
			folder = *next
		} else {
			folder, err = c.Mkdir(ctx, folder.ID, name)
			if err != nil {
				return "", err
			}
		}
	}
	return folder.ID, nil
}

func (a *Application) prepareDownload(w http.ResponseWriter, r *http.Request) {
	var input browserDownload
	if !decodeJSON(w, r, &input) {
		return
	}
	if _, err := transferTimeout(input.TimeoutSeconds); err != nil {
		writeError(w, err)
		return
	}
	if input.Account == "" || input.Path == "" {
		writeError(w, invalid("select an account and file"))
		return
	}
	id, err := randomToken()
	if err != nil {
		writeError(w, err)
		return
	}
	proof, err := randomToken()
	if err != nil {
		writeError(w, err)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for key, old := range a.downloads {
		if !now.Before(old.Expires) {
			delete(a.downloads, key)
		}
	}
	if len(a.downloads) >= 128 {
		writeError(w, ErrBusy)
		return
	}
	input.ClientID, input.Expires, input.Proof = id, now.Add(time.Minute), sha256.Sum256([]byte(proof))
	a.downloads[id] = input
	path := "/download/" + id
	http.SetCookie(w, &http.Cookie{Name: downloadCookie, Value: proof, Path: path,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 60, Secure: r.TLS != nil})
	writeJSON(w, map[string]string{"id": id, "url": path})
}

const downloadCookie = "pkudisk-download"

// The URL is a resource identifier, not a credential. Its short-lived proof is
// held in an HttpOnly cookie scoped to this exact attachment path. WebView first
// requests the attachment to discover its headers, then DownloadListener asks
// the OS download manager to fetch it; only those two GETs are authorized.
// Cookie authority never extends to /api/, which continues to require a bearer.
func (s *server) browserDownload(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodGet) {
		return
	}
	if s.application == nil {
		http.Error(w, "invalid download", http.StatusBadRequest)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/download/")
	cookie, err := r.Cookie(downloadCookie)
	if err != nil || len(id) != 43 || len(cookie.Value) != 43 {
		http.Error(w, "download expired; request it again", http.StatusUnauthorized)
		return
	}
	digest := sha256.Sum256([]byte(cookie.Value))
	a := s.application
	a.mu.Lock()
	input, ok := a.downloads[id]
	valid := ok && time.Now().Before(input.Expires) && input.Uses < 2 && subtle.ConstantTimeCompare(digest[:], input.Proof[:]) == 1
	if valid {
		input.Uses++
		if input.Uses == 2 {
			delete(a.downloads, id)
		} else {
			a.downloads[id] = input
		}
	} else if ok && !time.Now().Before(input.Expires) {
		delete(a.downloads, id)
	}
	a.mu.Unlock()
	if !valid {
		http.Error(w, "download expired; request it again", http.StatusUnauthorized)
		return
	}
	if input.Uses == 2 {
		http.SetCookie(w, &http.Cookie{Name: downloadCookie, Path: r.URL.Path,
			HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1, Secure: r.TLS != nil})
	}
	a.streamDownload(w, r, input)
}

type downloadWriter struct {
	w           http.ResponseWriter
	started     bool
	total, done int64
	observe     transfer.Observer
}

func (w *downloadWriter) Write(data []byte) (int, error) {
	w.started = true
	n, err := w.w.Write(data)
	w.done += int64(n)
	w.observe(transfer.Progress{Phase: transfer.Transferring, Done: w.done, Total: w.total})
	return n, err
}

func (a *Application) streamDownload(w http.ResponseWriter, r *http.Request, input browserDownload) {
	timeout, err := transferTimeout(input.TimeoutSeconds)
	if err != nil {
		writeError(w, err)
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(timeout + 5*time.Second))
	writer := &downloadWriter{w: w}
	info, err := a.requestJob(r, jobInfo{Kind: "download", Account: input.Account, ClientID: input.ClientID, Name: input.Path, Source: input.Path}, timeout, func(ctx context.Context, j *job) (any, error) {
		c, err := a.account(ctx, input.Account)
		if err != nil {
			return nil, err
		}
		entry, err := c.Client.Resolve(ctx, input.Path)
		if err != nil {
			return nil, err
		}
		if entry.Dir != input.Archive {
			return nil, invalid("download directories as ZIP or select individual files")
		}
		name := entry.Name
		if entry.Dir {
			name += ".zip"
		}
		if !anyshare.ValidName(name) {
			return nil, invalid("invalid download filename")
		}
		a.mu.Lock()
		j.info.Name = name
		a.mu.Unlock()
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
		w.Header().Set("Content-Type", "application/octet-stream")
		writer.total = entry.Size
		writer.observe = a.observe(j)
		if !entry.Dir {
			w.Header().Set("Content-Length", strconv.FormatInt(entry.Size, 10))
			err = c.Client.Download(ctx, entry, writer)
		} else {
			w.Header().Set("Content-Type", "application/zip")
			_, err = c.Client.DownloadArchive(ctx, entry, writer, func(p anyshare.ArchiveProgress) {
				if p.Preparing {
					a.observe(j)(transfer.Progress{Phase: transfer.Preparing, Percent: p.Percent})
				} else {
					writer.total = p.Total
				}
			})
		}
		return nil, err
	})
	if err == nil && info.Error != nil {
		err = info.Error
	}
	if err != nil {
		if writer.started {
			// Finishing a chunked response would make a truncated ZIP look like a
			// successful download. The job has joined, so abort only the HTTP
			// stream; net/http closes HTTP/1 connections or resets HTTP/2 streams.
			panic(http.ErrAbortHandler)
		}
		w.Header().Del("Content-Length")
		w.Header().Del("Content-Disposition")
		if errors.Is(err, context.Canceled) {
			return
		}
		writeError(w, err)
	}
}
