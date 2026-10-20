package session

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/unofficialpku/disk/internal/accounts"
	"github.com/unofficialpku/disk/internal/auth"
)

// Exercise the shared connector through the real IAAA/session protocol.
func TestConnectLoginRestoreAndFailures(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	public := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	var refreshStatus atomic.Int32
	var passwordLogins atomic.Int32
	var refreshes atomic.Int32
	refreshStatus.Store(200)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/anyshare/oauth2/login":
			fmt.Fprintf(w, `<script id="__NEXT_DATA__">{"props":{"pageProps":{"challenge":"test","authconfig":{"thirdauth":{"config":{"authServer":"%s/sso"}}}}}}</script>`, server.URL)
		case "/sso":
			fmt.Fprintf(w, `<form action="%s/iaaa/oauth.jsp"><input name="appID" value="anyshare"><input name="redirectUrl" value="%s/callback"></form>`, server.URL, server.URL)
		case "/iaaa/oauth.jsp":
			fmt.Fprint(w, "login")
		case "/iaaa/getPublicKey.do":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "key": public})
		case "/iaaa/isShowCode.do":
			fmt.Fprint(w, `{"success":false}`)
		case "/iaaa/isMobileAuthen.do":
			fmt.Fprint(w, `{"success":true,"isMobileAuthen":true,"authenMode":"OTP"}`)
		case "/iaaa/oauthlogin.do":
			_ = r.ParseForm()
			encrypted, _ := base64.StdEncoding.DecodeString(r.Form.Get("password"))
			plain, err := rsa.DecryptPKCS1v15(rand.Reader, key, encrypted)
			if err != nil || string(plain) != "test-password" || r.Form.Get("otpCode") != "123456" {
				t.Error("wrong encrypted password or OTP")
				http.Error(w, "bad login", 401)
				return
			}
			passwordLogins.Add(1)
			fmt.Fprint(w, `{"success":true,"token":"test-iaaa"}`)
		case "/callback", "/anyshare/oauth2/login/refreshToken":
			if r.URL.Path != "/callback" && refreshStatus.Load() != 200 {
				w.WriteHeader(int(refreshStatus.Load()))
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "client.oauth2_token", Value: fmt.Sprint("test-access-", refreshes.Add(1)), Path: "/"})
			http.SetCookie(w, &http.Cookie{Name: "client.oauth2_refresh_token", Value: "test-refresh", Path: "/"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	store := &accounts.Store{Path: filepath.Join(t.TempDir(), "private", "accounts.json")}
	connector := Connector{Store: store, NewSession: func(opts auth.Options) (*auth.Session, error) { opts.IAAA = server.URL; return auth.New(opts) }}
	connect := connector.Connect
	challenge := func(context.Context, auth.Challenge) (string, error) { return "123456", nil }
	input := Input{Name: "campus", Server: server.URL, Explicit: true, ForceLogin: true, Default: true, Challenge: challenge, Credentials: func(context.Context) (auth.Credentials, error) {
		return auth.Credentials{Username: "student", Password: []byte("test-password")}, nil
	}}
	connection, err := connect(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	connection.Session.Close()
	config, err := store.Load()
	if err != nil || config.Default != "campus" || len(config.Accounts) != 1 || config.Accounts[0].Session.RefreshToken != "test-refresh" {
		t.Fatalf("login not persisted: %v", err)
	}
	if _, err = connect(context.Background(), input); !errors.Is(err, accounts.ErrExists) {
		t.Fatal("duplicate account accepted", err)
	}
	saved := Input{Account: "campus", Challenge: challenge}
	connection, err = connect(context.Background(), saved)
	if err != nil {
		t.Fatal(err)
	}
	connection.Session.Close()
	if passwordLogins.Load() != 1 {
		t.Fatal("restored session logged in again")
	}
	refreshStatus.Store(503)
	if _, err = connect(context.Background(), saved); err == nil {
		t.Fatal("upstream outage ignored")
	}
	if passwordLogins.Load() != 1 {
		t.Fatal("upstream outage caused password login")
	}
	refreshStatus.Store(401)
	connection, err = connect(context.Background(), saved)
	if err != nil {
		t.Fatal(err)
	}
	connection.Session.Close()
	if passwordLogins.Load() != 2 {
		t.Fatal("expired session did not reauthenticate")
	}
	if _, err = connect(context.Background(), Input{Account: "missing", Challenge: challenge}); !errors.Is(err, accounts.ErrNotFound) {
		t.Fatal(err)
	}
}
