package gui

import (
	"bytes"
	"image"
	"image/png"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/unofficialpku/disk/internal/anyshare/mockserver"
)

func TestThumbnailIsAuthenticatedBoundedAndAccountIsolated(t *testing.T) {
	f, _, s, token := applicationServer(t)
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	entry := anyshare.Entry{ID: fixtureLibrary + "/photo", Name: "照片.png", Size: 1024, Rev: "current"}
	f.server.PutObject("campus", mockserver.Object{Entry: entry, Thumbnail: data.Bytes()})
	endpoint := "/api/thumbnail?account=campus&path=" + url.QueryEscape(entry.ID)
	if w := appRequest(t, s, "", http.MethodGet, endpoint, nil); w.Code != http.StatusUnauthorized {
		t.Fatal("thumbnail accepted an unauthenticated request", w.Code)
	}
	w := appRequest(t, s, token, http.MethodGet, endpoint, nil)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/png" || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" || !bytes.Equal(w.Body.Bytes(), data.Bytes()) {
		t.Fatal("thumbnail bytes, MIME or private cache policy changed", w.Code, w.Header())
	}
	if w := appRequest(t, s, token, http.MethodGet, strings.Replace(endpoint, "account=campus", "account=work", 1), nil); w.Code != http.StatusNotFound {
		t.Fatal("thumbnail crossed account boundary", w.Code)
	}
	for _, query := range []string{"&size=10000", "&url=http://localhost/private"} {
		if w := appRequest(t, s, token, http.MethodGet, endpoint+query, nil); w.Code != http.StatusBadRequest {
			t.Fatal("arbitrary thumbnail selector accepted", w.Code)
		}
	}
	for _, path := range []string{fixtureLibrary + "/report", fixtureLibrary + "/folder"} {
		if w := appRequest(t, s, token, http.MethodGet, "/api/thumbnail?account=campus&path="+url.QueryEscape(path), nil); w.Code != http.StatusNotFound {
			t.Fatal("unsupported entry requested a thumbnail", w.Code)
		}
	}
	listing := decodeResponse[struct {
		Entries []struct {
			ID        string `json:"id"`
			Thumbnail bool   `json:"thumbnail"`
		}
	}](t, appRequest(t, s, token, http.MethodGet, "/api/files?account=campus&path="+url.QueryEscape(fixtureLibrary), nil), http.StatusOK)
	matched := false
	for _, item := range listing.Entries {
		if item.Thumbnail != (item.ID == entry.ID) {
			t.Fatal("listing thumbnail eligibility differs", item)
		}
		matched = matched || item.ID == entry.ID
	}
	if !matched {
		t.Fatal("thumbnail entry not listed")
	}
}
