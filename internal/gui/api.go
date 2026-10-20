package gui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/unofficialpku/disk/internal/accounts"
	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/unofficialpku/disk/internal/auth"
)

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *apiError) Error() string  { return e.Message }
func invalid(message string) error { return &apiError{Code: "invalid_input", Message: message} }

func describeError(err error) *apiError {
	var typed *apiError
	if errors.As(err, &typed) {
		return &apiError{Code: canonicalErrorCode(typed.Code), Message: typed.Message}
	}
	code := "operation_failed"
	for _, item := range []struct {
		err  error
		code string
	}{
		{ErrBusy, "busy"}, {ErrClosed, "stopping"}, {ErrLoginRequired, "login_required"}, {auth.ErrSessionExpired, "session_expired"},
		{accounts.ErrNotFound, "account_not_found"}, {accounts.ErrExists, "account_exists"},
		{anyshare.ErrNotFound, "not_found"}, {anyshare.ErrExists, "exists"}, {anyshare.ErrPermission, "permission"},
		{anyshare.ErrConflict, "conflict"}, {anyshare.ErrQuota, "quota"}, {anyshare.ErrSizeLimit, "size_limit"},
		{anyshare.ErrPending, "approval_pending"}, {anyshare.ErrPickupCodeRequired, "pickup_code"},
		{os.ErrNotExist, "not_found"}, {os.ErrExist, "exists"}, {os.ErrPermission, "permission"},
		{context.Canceled, "cancelled"}, {context.DeadlineExceeded, "timeout"},
	} {
		if errors.Is(err, item.err) {
			code = item.code
			break
		}
	}
	return &apiError{Code: canonicalErrorCode(code), Message: err.Error()}
}

// Canonicalize at the response boundary so older internal typed errors remain
// accepted without mutating error values that may be shared by running tasks.
func canonicalErrorCode(code string) string {
	code = strings.TrimPrefix(strings.ToUpper(code), "ERR_")
	switch code {
	case "":
		code = "OPERATION_FAILED"
	case "APPROVAL_PENDING":
		code = "PENDING"
	case "PICKUP_CODE":
		code = "PICKUP_CODE_REQUIRED"
	}
	return "ERR_" + code
}

func writeError(w http.ResponseWriter, err error) {
	detail := describeError(err)
	status := http.StatusBadGateway
	switch detail.Code {
	case "ERR_INVALID_INPUT":
		status = http.StatusBadRequest
	case "ERR_BUSY", "ERR_EXISTS", "ERR_CONFLICT", "ERR_ACCOUNT_EXISTS", "ERR_LOGIN_REQUIRED", "ERR_SESSION_EXPIRED", "ERR_STOPPING", "ERR_PENDING", "ERR_PICKUP_CODE_REQUIRED":
		status = http.StatusConflict
	case "ERR_NOT_FOUND", "ERR_ACCOUNT_NOT_FOUND":
		status = http.StatusNotFound
	case "ERR_PERMISSION":
		status = http.StatusForbidden
	case "ERR_CANCELLED":
		status = http.StatusRequestTimeout
	case "ERR_TIMEOUT":
		status = http.StatusGatewayTimeout
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": detail})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, destination any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, invalid("JSON required"))
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(destination) != nil || !errors.Is(decoder.Decode(new(any)), io.EOF) {
		writeError(w, invalid("invalid request body"))
		return false
	}
	return true
}

// jsonHandler keeps request size/type/shape validation and error responses
// consistent. The action receives a decoded value and never writes HTTP headers.
func jsonHandler[Input any](status int, action func(*http.Request, Input) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input *Input
		if !decodeJSON(w, r, &input) {
			return
		}
		if input == nil {
			writeError(w, invalid("invalid request body"))
			return
		}
		result, err := action(r, *input)
		if err != nil {
			writeError(w, err)
			return
		}
		if status == http.StatusNoContent {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		writeJSON(w, result)
	}
}

func (a *Application) routes() {
	m := a.handler
	m.HandleFunc("GET /api/flags", a.flags)
	m.HandleFunc("GET /api/local-fs", a.browseLocalFS)
	m.HandleFunc("POST /api/local-fs/mkdir", a.mkdirLocalFS)
	m.HandleFunc("GET /api/accounts", a.listAccounts)
	m.HandleFunc("POST /api/accounts/default", jsonHandler(http.StatusNoContent, a.defaultAccount))
	m.HandleFunc("POST /api/accounts/remove", jsonHandler(http.StatusNoContent, a.removeAccount))
	m.HandleFunc("POST /api/logins", a.startLogin)
	m.HandleFunc("GET /api/logins/{id}", a.loginRequest)
	m.HandleFunc("POST /api/logins/{id}/answer", a.loginRequest)
	m.HandleFunc("DELETE /api/logins/{id}", a.loginRequest)
	m.HandleFunc("GET /api/files", a.listFiles)
	m.HandleFunc("GET /api/thumbnail", a.thumbnail)
	m.HandleFunc("GET /api/search", a.searchFiles)
	m.HandleFunc("GET /api/quota", a.quota)
	m.HandleFunc("POST /api/files/action", jsonHandler(http.StatusOK, a.fileAction))
	m.HandleFunc("GET /api/tasks", a.listJobs)
	m.HandleFunc("POST /api/tasks", a.startOperation)
	m.HandleFunc("POST /api/tasks/{id}/cancel", a.cancelJob)
	m.HandleFunc("DELETE /api/tasks/{id}", a.clearJob)
	m.HandleFunc("GET /api/mounts/support", a.mountSupport)
	m.HandleFunc("POST /api/mounts", a.startMount)
	m.HandleFunc("POST /api/uploads", a.browserUpload)
	m.HandleFunc("POST /api/downloads", a.prepareDownload)
	m.HandleFunc("GET /api/recycle", a.recycle)
	m.HandleFunc("POST /api/recycle/action", jsonHandler(http.StatusAccepted, a.recycleAction))
	m.HandleFunc("GET /api/revisions", a.revisions)
	m.HandleFunc("POST /api/revisions/restore", jsonHandler(http.StatusNoContent, a.restoreRevision))
	m.HandleFunc("GET /api/sharing", a.sharing)
	m.HandleFunc("GET /api/sharing/recipients", a.recipients)
	m.HandleFunc("POST /api/sharing/action", a.shareAction)
	m.HandleFunc("GET /api/recovery", a.recovery)
}

func (a *Application) listAccounts(w http.ResponseWriter, r *http.Request) {
	config, err := a.opts.Store.Load()
	if err != nil {
		writeError(w, err)
		return
	}
	type summary struct {
		Name      string `json:"name"`
		Username  string `json:"username"`
		Server    string `json:"server"`
		Default   bool   `json:"default"`
		Connected bool   `json:"connected"`
	}
	rows := make([]summary, 0, len(config.Accounts))
	a.mu.Lock()
	for _, saved := range config.Accounts {
		rows = append(rows, summary{saved.Name, saved.Username, saved.Server, saved.Name == config.Default, a.connections[saved.Name] != nil})
	}
	a.mu.Unlock()
	writeJSON(w, map[string]any{"accounts": rows, "default": config.Default, "cache": a.opts.CacheDirectory, "flags": hostFlags()})
}

type accountInput struct {
	Account string `json:"account"`
}

type removeAccountInput struct {
	Account string `json:"account"`
	Confirm bool   `json:"confirm"`
}

func (a *Application) defaultAccount(_ *http.Request, input accountInput) (any, error) {
	return nil, a.opts.Store.SetDefault(input.Account)
}

func (a *Application) removeAccount(_ *http.Request, input removeAccountInput) (any, error) {
	if !input.Confirm {
		return nil, invalid("confirmation required")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.busyLocked(input.Account) {
		return nil, ErrBusy
	}
	if err := a.opts.Store.Remove(input.Account); err != nil {
		return nil, err
	}
	if c := a.connections[input.Account]; c != nil {
		c.close()
		delete(a.connections, input.Account)
	}
	return nil, nil
}

func (a *Application) client(w http.ResponseWriter, r *http.Request, name string) *Connection {
	c, err := a.account(r.Context(), name)
	if err != nil {
		writeError(w, err)
		return nil
	}
	return c
}

func (a *Application) listFiles(w http.ResponseWriter, r *http.Request) {
	c := a.client(w, r, r.URL.Query().Get("account"))
	if c == nil {
		return
	}
	path := r.URL.Query().Get("path")
	var entries []anyshare.Entry
	var folder *anyshare.Entry
	var err error
	writable := false
	if path == "" || path == "/" {
		entries, err = c.Client.Roots(r.Context())
	} else {
		entry, e := c.Client.Resolve(r.Context(), path)
		if e != nil {
			writeError(w, e)
			return
		}
		if !entry.Dir {
			writeError(w, invalid("choose a remote directory"))
			return
		}
		folder = &entry
		entries, err = c.Client.List(r.Context(), entry.ID)
		if err == nil {
			writable, _ = c.Client.Writable(r.Context(), entry.ID)
		}
	}
	if err != nil {
		writeError(w, err)
		return
	}
	if entries == nil {
		entries = []anyshare.Entry{}
	}
	stats := make(map[string]anyshare.LibraryStat)
	if folder == nil {
		stats = anyshare.LibraryStats(r.Context(), c.Client, entries)
	}
	writeJSON(w, map[string]any{"entries": thumbnailEntries(entries), "folder": folder, "writable": writable, "account": c.Name, "stats": stats})
}

func queryLimit(r *http.Request) (int, error) {
	value := r.URL.Query().Get("limit")
	if value == "" {
		return 100, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 || n > 1000 {
		return 0, invalid("limit must be between 1 and 1000")
	}
	return n, nil
}

func (a *Application) searchFiles(w http.ResponseWriter, r *http.Request) {
	limit, err := queryLimit(r)
	if err != nil {
		writeError(w, err)
		return
	}
	c := a.client(w, r, r.URL.Query().Get("account"))
	if c == nil {
		return
	}
	result, err := c.Client.Search(r.Context(), r.URL.Query().Get("q"), r.URL.Query().Get("scope"), limit)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]any{"entries": thumbnailSearchEntries(result.Files), "hits": result.Hits, "next": result.Next})
}

func (a *Application) quota(w http.ResponseWriter, r *http.Request) {
	c := a.client(w, r, r.URL.Query().Get("account"))
	if c == nil {
		return
	}
	quota, err := c.Client.Quota(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, quota)
}

func (a *Application) mountSupport(w http.ResponseWriter, r *http.Request) {
	var err error
	if a.opts.CheckMount != nil {
		err = a.opts.CheckMount()
	} else {
		err = fmt.Errorf("mounting is unavailable")
	}
	message := ""
	if err != nil {
		message = err.Error()
	}
	writeJSON(w, map[string]any{"available": err == nil, "message": message, "platform": osPlatform()})
}
