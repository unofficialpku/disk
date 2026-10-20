package gui

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/unofficialpku/disk/internal/anyshare"
)

type fileEntry struct {
	anyshare.Entry
	Thumbnail bool   `json:"thumbnail"`
	Path      string `json:"path,omitempty"`
	Extension string `json:"extension,omitempty"`
}

func thumbnailSearchEntries(entries []anyshare.SearchEntry) []fileEntry {
	rows := make([]fileEntry, 0, len(entries))
	for _, entry := range entries {
		rows = append(rows, fileEntry{Entry: entry.Entry, Path: entry.Path, Extension: entry.Extension,
			Thumbnail: !entry.Dir && entry.Size > 0 && anyshare.ThumbnailSupported(entry.Name)})
	}
	return rows
}

func thumbnailEntries(entries []anyshare.Entry) []fileEntry {
	rows := make([]fileEntry, 0, len(entries))
	for _, entry := range entries {
		rows = append(rows, fileEntry{Entry: entry, Thumbnail: !entry.Dir && entry.Size > 0 && anyshare.ThumbnailSupported(entry.Name)})
	}
	return rows
}

func (a *Application) thumbnail(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if query.Get("size") != "" && query.Get("size") != "small" || query.Get("path") == "" || query.Has("url") {
		writeError(w, invalid("select a file and a small thumbnail"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	c, err := a.account(ctx, query.Get("account"))
	if err != nil {
		writeError(w, err)
		return
	}
	entry, err := c.Client.Resolve(ctx, query.Get("path"))
	if err != nil {
		writeError(w, err)
		return
	}
	preview, err := c.Client.Thumbnail(ctx, entry)
	if err != nil {
		if errors.Is(err, anyshare.ErrThumbnailUnavailable) {
			writeError(w, anyshare.ErrNotFound)
		} else if errors.Is(err, anyshare.ErrThumbnailPending) {
			w.Header().Set("Retry-After", "5")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			writeJSON(w, map[string]any{"error": apiError{Code: "ERR_THUMBNAIL_PENDING", Message: err.Error()}})
		} else {
			writeError(w, err)
		}
		return
	}
	// Never persist private thumbnails in an HTTP cache or expose their signed
	// source URL. The browser owns its short-lived per-account blob URL instead.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", preview.ContentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(preview.Data)))
	_, _ = w.Write(preview.Data)
}
