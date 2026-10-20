package gui

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unofficialpku/disk/internal/localfile"
)

type testTray struct {
	done chan struct{}
	once sync.Once
}

func (t *testTray) Run() error { <-t.done; return nil }
func (t *testTray) Close()     { t.once.Do(func() { close(t.done) }) }

func receiveURL(t *testing.T, opened <-chan string) string {
	t.Helper()
	select {
	case address := <-opened:
		return address
	case <-time.After(5 * time.Second):
		t.Fatal("browser was not opened")
		return ""
	}
}

func browserRequest(t *testing.T, method, address, token, body string) (int, []byte) {
	t.Helper()
	r, err := http.NewRequest(method, address, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(address)
	r.Header.Set("Origin", u.Scheme+"://"+u.Host)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	defer client.CloseIdleConnections()
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, data
}

func browserLogin(t *testing.T, address string) (string, string) {
	t.Helper()
	u, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	fragment, err := url.ParseQuery(u.Fragment)
	if err != nil || u.RawQuery != "" || fragment.Get("ticket") == "" {
		t.Fatal("login credential must be a fragment")
	}
	origin := u.Scheme + "://" + u.Host
	code, body := browserRequest(t, "POST", origin+"/api/session", "", fmt.Sprintf(`{"ticket":%q}`, fragment.Get("ticket")))
	var response struct {
		Token string `json:"token"`
	}
	if code != 200 || json.Unmarshal(body, &response) != nil || response.Token == "" {
		t.Fatalf("login failed: HTTP %d", code)
	}
	return origin, response.Token
}

func awaitRun(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("GUI did not shut down")
	}
}

func TestRunSingleInstanceLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	dir := filepath.Join(t.TempDir(), "gui")
	opened := make(chan string, 32)
	var trays atomic.Int32
	opts := Options{Version: "test", Directory: dir,
		OpenBrowser: func(address string) error { opened <- address; return nil },
		NewTray: func(open, quit func()) (Tray, error) {
			trays.Add(1)
			return &testTray{done: make(chan struct{})}, nil
		}}
	done := make(chan error, 1)
	go func() { done <- Run(ctx, opts) }()
	origin, token := browserLogin(t, receiveURL(t, opened))
	// Merely finishing a browser request must not stop the resident service.
	if code, _ := browserRequest(t, "GET", origin+"/api/status", token, ""); code != 200 {
		t.Fatal(code)
	}
	var callers sync.WaitGroup
	errorsOut := make(chan error, 8)
	for range 8 {
		callers.Go(func() { errorsOut <- Run(ctx, opts) })
	}
	callers.Wait()
	for range 8 {
		if err := <-errorsOut; err != nil {
			t.Fatal(err)
		}
	}
	if trays.Load() != 1 {
		t.Fatalf("created %d trays", trays.Load())
	}
	seen := make(map[string]bool)
	for range 8 {
		address := receiveURL(t, opened)
		if seen[address] {
			t.Fatal("activation reused a login ticket")
		}
		seen[address] = true
		if !strings.HasPrefix(address, origin+"/") {
			t.Fatal("activation created another listener")
		}
	}
	if code, _ := browserRequest(t, "POST", origin+"/api/quit", token, ""); code != 204 {
		t.Fatal(code)
	}
	awaitRun(t, done)
	if _, err := os.Stat(filepath.Join(dir, "instance.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("discovery metadata survived shutdown")
	}
	client := &http.Client{Timeout: time.Second}
	response, err := client.Get(origin)
	if err == nil {
		response.Body.Close()
		t.Fatal("listener survived shutdown")
	}
	// Restart must replace the control credential and reject old browser tokens.
	go func() { done <- Run(ctx, opts) }()
	newOrigin, newToken := browserLogin(t, receiveURL(t, opened))
	if code, _ := browserRequest(t, "GET", newOrigin+"/api/status", token, ""); code != 401 {
		t.Fatal("old session survived restart")
	}
	if code, _ := browserRequest(t, "POST", newOrigin+"/api/quit", newToken, ""); code != 204 {
		t.Fatal(code)
	}
	awaitRun(t, done)
}

func TestRunWithoutTrayAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opened := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{Version: "fallback", Directory: filepath.Join(t.TempDir(), "gui"),
			NewTray:     func(func(), func()) (Tray, error) { return nil, errors.New("no tray") },
			OpenBrowser: func(address string) error { opened <- address; return nil },
		})
	}()
	origin, token := browserLogin(t, receiveURL(t, opened))
	code, data := browserRequest(t, "GET", origin+"/api/status", token, "")
	if code != 200 || !strings.Contains(string(data), `"tray":false`) {
		t.Fatal("fallback status incorrect")
	}
	cancel()
	awaitRun(t, done)
}

func TestRunUnavailableCleansUpAndRedactsURL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gui")
	var output strings.Builder
	err := Run(context.Background(), Options{Directory: dir, Output: &output,
		NewTray:     func(func(), func()) (Tray, error) { return nil, errors.New("no tray") },
		OpenBrowser: func(address string) error { return fmt.Errorf("could not open %s", address) },
	})
	if err == nil || strings.Contains(err.Error(), "ticket=") || strings.Contains(output.String(), "ticket=") {
		t.Fatal("missing or unsafe launch error")
	}
	owner, err := acquire(context.Background(), dir)
	if err != nil || owner == nil {
		t.Fatal("failed launch retained lock", err)
	}
	owner.close()
}

func TestRejectUntrustedInstanceMetadata(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gui")
	owner, err := acquire(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.close()
	secret, _ := randomToken()
	for _, address := range []string{"http://example.org:80", "http://localhost:8080", "http://127.0.0.1:8080/path", "http://127.0.0.1:8080?x=y", "http://user@127.0.0.1:8080", "http://127.0.0.1:0", "http://127.0.0.1:99999", "https://127.0.0.1:8080"} {
		if err := owner.publish(instanceInfo{Version: 1, Origin: address, Control: secret}); err != nil {
			t.Fatal(err)
		}
		if _, err := readInstance(owner.path); err == nil {
			t.Fatal("accepted unsafe control address", address)
		}
	}
}

func TestGUIInstanceProcess(t *testing.T) {
	dir := os.Getenv("PKUDISK_GUI_TEST_INSTANCE")
	if dir == "" {
		return
	}
	owner, err := acquire(context.Background(), dir)
	if err != nil || owner == nil {
		t.Fatal("helper cannot own instance", err)
	}
	defer owner.close()
	secret, _ := randomToken()
	if err := owner.publish(instanceInfo{Version: 1, Origin: "http://127.0.0.1:1", Control: secret}); err != nil {
		t.Fatal(err)
	}
	fmt.Println("ready")
	// The parent deliberately kills this process to test stale-file recovery.
	<-time.After(30 * time.Second)
}

func TestProcessLockAndCrashRecovery(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gui")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestGUIInstanceProcess$")
	cmd.Env = append(os.Environ(), "PKUDISK_GUI_TEST_INSTANCE="+dir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "ready\n" {
		t.Fatal("helper failed to start", err)
	}
	lock, err := localfile.TryLock(filepath.Join(dir, "instance.lock"), true)
	if err != nil || lock != nil {
		if lock != nil {
			lock.Close()
		}
		t.Fatal("lock did not exclude second process", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if _, err := os.Stat(filepath.Join(dir, "instance.json")); err != nil {
		t.Fatal("fixture did not leave stale metadata", err)
	}
	owner, err := acquire(ctx, dir)
	if err != nil || owner == nil {
		t.Fatal("crashed process retained lock", err)
	}
	defer owner.close()
	if _, err := os.Stat(owner.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("new owner retained stale metadata")
	}
}
