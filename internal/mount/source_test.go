package mount

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/unofficialpku/disk/internal/accounts"
	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/unofficialpku/disk/internal/auth"
	"github.com/unofficialpku/disk/internal/session"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPrepareMountAccountsAndSessionLifecycleThroughHTTP(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var alphaRefreshes, bravoRefreshes, roots atomic.Int32
	var rejectBravo atomic.Bool
	var rejectBravoRoots atomic.Bool
	renewed := make(chan string, 16)
	const library = "gns://shared"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/anyshare/oauth2/login/refreshToken" {
			cookie, err := r.Cookie("client.oauth2_refresh_token")
			if err != nil {
				t.Error("refresh omitted account cookie")
				w.WriteHeader(401)
				return
			}
			name := strings.TrimPrefix(cookie.Value, "refresh-")
			var number int32
			switch name {
			case "alpha":
				number = alphaRefreshes.Add(1)
			case "bravo":
				number = bravoRefreshes.Add(1)
				if rejectBravo.Load() {
					w.WriteHeader(401)
					return
				}
			default:
				t.Error("unknown session identity", name)
			}
			http.SetCookie(w, &http.Cookie{Name: "client.oauth2_token", Value: fmt.Sprintf("access-%s-%d", name, number), Path: "/"})
			http.SetCookie(w, &http.Cookie{Name: "client.oauth2_refresh_token", Value: "refresh-" + name, Path: "/"})
			if number >= 2 {
				select {
				case renewed <- name:
				default:
				}
			}
			return
		}
		name := strings.SplitN(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer access-"), "-", 2)[0]
		if name != "alpha" && name != "bravo" {
			t.Error("API request used wrong session", name)
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/api/efast/v1/entry-doc-lib":
			roots.Add(1)
			if name == "bravo" && rejectBravoRoots.Load() {
				w.WriteHeader(403)
				return
			}
			fmt.Fprintf(w, `[{"id":%q,"name":%q,"size":-1,"type":"user_doc_lib"}]`, library, name+"-library")
		case "/api/efast/v1/folders/" + library + "/sub_objects":
			fmt.Fprintf(w, `{"dirs":[],"files":[{"docid":%q,"name":%q,"size":5,"rev":"same"}]}`, library+"/same", name+".txt")
		default:
			t.Error("unexpected mount preparation request", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	store, err := accounts.Default()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha", "bravo"} {
		if err := store.Put(accounts.Account{Name: name, Username: name, Password: "private-fixture", Server: server.URL, Session: auth.State{AccessToken: "old-" + name, RefreshToken: "refresh-" + name}}, name == "alpha", false); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	connect := func(ctx context.Context, name string) (*session.Connection, error) {
		return (session.Connector{Store: store}).Connect(ctx, session.Input{Account: name})
	}
	opts := SourceOptions{AllAccounts: true, PartSize: 8 << 20, Timeout: time.Second}
	source, err := PrepareSource(context.Background(), store, "", opts, connect)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if source.Root != nil || len(source.sessions) != 2 || roots.Load() != 2 || alphaRefreshes.Load() != 1 || bravoRefreshes.Load() != 1 {
		t.Fatal("not all saved accounts were checked before mounting")
	}
	entries, err := source.Backend.Roots(context.Background())
	if err != nil || len(entries) != 2 {
		t.Fatal(entries, err)
	}
	for _, account := range entries {
		libraries, err := source.Backend.List(context.Background(), account.ID)
		if err != nil || len(libraries) != 1 || libraries[0].Name != account.Name+"-library" {
			t.Fatal("account listing used wrong session", libraries, err)
		}
		files, err := source.Backend.List(context.Background(), libraries[0].ID)
		if err != nil || len(files) != 1 || files[0].Name != account.Name+".txt" {
			t.Fatal("directory listing used wrong session", files, err)
		}
	}
	config, err := store.Load()
	if err != nil || config.Default != "alpha" {
		t.Fatal("aggregate selection changed the saved default", err)
	}
	// Renew both idle accounts. One expiration must identify that account and
	// permit the drivers to stop; cancellation must join all remaining workers.
	failed, stop := source.Maintain(context.Background(), 10*time.Millisecond, &output)
	defer stop()
	seen := make(map[string]bool)
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for len(seen) != 2 {
		select {
		case name := <-renewed:
			seen[name] = true
		case <-timer.C:
			t.Fatal("idle renewal ignored an account")
		}
	}
	rejectBravo.Store(true)
	select {
	case err := <-failed:
		if !errors.Is(err, auth.ErrSessionExpired) || !strings.Contains(err.Error(), "bravo") {
			t.Error("expiration omitted account identity", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("expired account did not stop the aggregate mount")
	}
	stop()
	if alphaRefreshes.Load() < 2 || bravoRefreshes.Load() < 2 {
		t.Fatal("idle renewal ignored an account")
	}
	source.Close()
	for _, account := range source.sessions {
		if account.Session.AccessToken() != "" {
			t.Fatal("mount retained an account session after close")
		}
	}
	rejectBravo.Store(false)
	rejectBravoRoots.Store(true)
	if failed, err := PrepareSource(context.Background(), store, "", opts, connect); failed != nil || !errors.Is(err, anyshare.ErrPermission) || !strings.Contains(err.Error(), "bravo") {
		t.Fatal("startup silently dropped an unusable account", failed, err)
	}
}

func TestPrepareSingleAccountMountPreservesRootSelection(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const library = "gns://single"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/anyshare/oauth2/login/refreshToken":
			http.SetCookie(w, &http.Cookie{Name: "client.oauth2_token", Value: "access", Path: "/"})
			http.SetCookie(w, &http.Cookie{Name: "client.oauth2_refresh_token", Value: "refresh", Path: "/"})
		case "/api/efast/v1/entry-doc-lib":
			fmt.Fprintf(w, `[{"id":%q,"name":"Library","size":-1,"type":"user_doc_lib"}]`, library)
		default:
			t.Error("unexpected single-account request", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	store, err := accounts.Default()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(accounts.Account{Name: "single", Username: "single", Password: "private-fixture", Server: server.URL, Session: auth.State{RefreshToken: "refresh"}}, true, false); err != nil {
		t.Fatal(err)
	}
	source, err := PrepareSource(context.Background(), store, "", SourceOptions{Root: "/Library", PartSize: 8 << 20, Timeout: time.Second}, func(ctx context.Context, name string) (*session.Connection, error) {
		return (session.Connector{Store: store}).Connect(ctx, session.Input{Account: name})
	})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	client, ok := source.Backend.(*anyshare.Client)
	if !ok || client.PartSize != 8<<20 || len(source.sessions) != 1 || source.Root == nil || source.Root.ID != library {
		t.Fatal("single-account mount changed root, upload policy or backend")
	}
}
