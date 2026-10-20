package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"testing"
	"time"

	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/unofficialpku/disk/internal/localfile"
)

// Opt-in acceptance uses a new login session and reads only remote metadata and
// file ranges. Account details and credentials never appear in test output.
func TestLiveSessionRefresh(t *testing.T) {
	path := os.Getenv("PKUDISK_TEST_CREDENTIALS")
	if path == "" {
		t.Skip("set PKUDISK_TEST_CREDENTIALS to an owner-only credential file for live read-only acceptance")
	}
	f, err := localfile.OpenPrivate(path, os.O_RDONLY)
	if err != nil {
		t.Fatal("cannot open private live credential file")
	}
	b, err := io.ReadAll(io.LimitReader(f, 8193))
	f.Close()
	if err != nil || len(b) > 8192 {
		t.Fatal("cannot read live credential file")
	}
	defer clear(b)
	var v struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err = json.Unmarshal(b, &v); err != nil {
		t.Fatal("invalid live credential file")
	}
	c := Credentials{Username: v.Username, Password: []byte(v.Password)}
	defer clear(c.Password)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	s, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Login(ctx, c); err != nil {
		t.Fatal(err)
	}
	client, err := anyshare.New("https://disk.pku.edu.cn", s)
	if err != nil {
		t.Fatal(err)
	}
	roots, err := client.Roots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var probe anyshare.Entry
	for _, root := range roots {
		if root.Type != "user_doc_lib" {
			continue
		}
		entries, err := client.List(ctx, root.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if !e.Dir && e.Size > 0 {
				probe = e
				break
			}
		}
	}
	if probe.ID == "" {
		t.Fatal("no readable file available for live range acceptance")
	}
	before := make([]byte, min(int64(64), probe.Size))
	if _, err = client.ReadAt(ctx, probe, before, 0); err != nil {
		t.Fatal(err)
	}
	for cycle := 0; cycle < 2; cycle++ {
		old := s.AccessToken()
		if err = s.Refresh(ctx, old); err != nil {
			t.Fatal(err)
		}
		if s.AccessToken() == old {
			t.Fatal("live session did not renew its access token")
		}
		if _, err = client.Roots(ctx); err != nil {
			t.Fatal(err)
		}
		after := make([]byte, len(before))
		if _, err = client.ReadAt(ctx, probe, after, 0); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatal("live range content changed after renewal")
		}
	}
	// A synthetic invalid access token exercises the real API's HTTP-401 retry;
	// the refresh cookie remains private and valid in the same login session.
	s.mu.Lock()
	s.token = "invalid-live-test-token"
	s.mu.Unlock()
	if _, err = client.Roots(ctx); err != nil {
		t.Fatal(err)
	}
	t.Log("live login, repeated renewal, HTTP-401 recovery, and range reads passed")
}
