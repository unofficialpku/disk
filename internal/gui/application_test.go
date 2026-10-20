package gui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unofficialpku/disk/internal/accounts"
)

func applicationServer(t *testing.T) (*businessFixture, *Application, *server, string) {
	t.Helper()
	f := newBusinessFixture(t)
	a := NewApplication(context.Background(), f.options())
	t.Cleanup(a.Close)
	s, _, _ := testServer(t)
	s.application = a
	return f, a, s, login(t, s)
}

func appRequest(t *testing.T, s *server, token, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	text := ""
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		text = string(data)
	}
	return request(s, method, path, token, s.origin, text)
}

func decodeResponse[T any](t *testing.T, w *httptest.ResponseRecorder, want int) T {
	t.Helper()
	if w.Code != want {
		t.Fatalf("HTTP %d, expected %d: %s", w.Code, want, w.Body.String())
	}
	var value T
	if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func waitJob(t *testing.T, a *Application, id string) jobInfo {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		a.mu.Lock()
		j := a.jobs[id]
		finished := j != nil && j.finished
		var info jobInfo
		if j != nil {
			info = j.info
		}
		a.mu.Unlock()
		if finished {
			return info
		}
		select {
		case <-deadline:
			t.Fatal("job did not finish")
			return jobInfo{}
		case <-time.After(time.Millisecond):
		}
	}
}

func TestApplicationAccountIsolationAndMutationConsent(t *testing.T) {
	f, _, s, token := applicationServer(t)
	listed := appRequest(t, s, token, "GET", "/api/accounts", nil)
	if listed.Code != 200 || strings.Contains(listed.Body.String(), "private-fixture-password") || strings.Contains(listed.Body.String(), "fixture-campus") {
		t.Fatal("account summary leaked credentials")
	}
	for _, account := range []string{"campus", "work"} {
		rows := appRequest(t, s, token, "GET", "/api/files?account="+account+"&path="+url.QueryEscape(fixtureLibrary), nil)
		if rows.Code != 200 || !strings.Contains(rows.Body.String(), `"account":"`+account+`"`) {
			t.Fatal(rows.Code, rows.Body.String())
		}
		name := "研究报告.txt"
		if account == "work" {
			name = "工作报告.txt"
		}
		if !strings.Contains(rows.Body.String(), name) {
			t.Fatal("account used the wrong remote tree")
		}
	}
	for path, body := range map[string]any{
		"/api/files/action":      map[string]any{"account": "campus", "action": "delete", "path": fixtureLibrary + "/report"},
		"/api/recycle/action":    map[string]any{"account": "campus", "action": "remove", "ids": []string{fixtureLibrary + "/bin/old"}},
		"/api/revisions/restore": map[string]any{"account": "campus", "path": fixtureLibrary + "/report", "revision": "old"},
		"/api/sharing/action":    map[string]any{"account": "campus", "path": fixtureLibrary + "/report", "action": "external_link"},
		"/api/accounts/remove":   map[string]any{"account": "campus"},
	} {
		if w := appRequest(t, s, token, "POST", path, body); w.Code != 400 {
			t.Fatalf("missing consent accepted: %s (%d)", path, w.Code)
		}
	}
	count := len(f.server.Mutations())
	if count != 0 {
		t.Fatal("unconfirmed requests changed the remote")
	}
	if w := appRequest(t, s, token, "POST", "/api/files/action", map[string]any{"account": "campus", "action": "delete", "path": fixtureLibrary + "/report", "confirm": true}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	first, second := false, false
	for _, object := range f.server.Objects("campus") {
		first = first || object.Entry.ID == fixtureLibrary+"/report"
	}
	for _, object := range f.server.Objects("work") {
		second = second || object.Entry.ID == fixtureLibrary+"/report"
	}
	if first || !second {
		t.Fatal("mutation crossed account boundaries")
	}
}

func TestApplicationFileSearchQuotaAndManagement(t *testing.T) {
	_, _, s, token := applicationServer(t)
	for _, path := range []string{
		"/api/files?account=campus", "/api/search?account=campus&q=" + url.QueryEscape("报告"), "/api/quota?account=campus",
		"/api/revisions?account=campus&path=" + url.QueryEscape(fixtureLibrary+"/report"),
		"/api/recycle?account=campus", "/api/recycle?account=campus&library=" + url.QueryEscape(fixtureLibrary),
		"/api/mounts/support", "/api/recovery",
	} {
		if w := appRequest(t, s, token, "GET", path, nil); w.Code != 200 {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
	for _, body := range []map[string]any{
		{"account": "campus", "action": "mkdir", "path": fixtureLibrary, "name": "New folder"},
		{"account": "campus", "action": "rename", "path": fixtureLibrary + "/report", "name": "Renamed.txt"},
		{"account": "campus", "action": "move", "path": fixtureLibrary + "/report", "destination": fixtureLibrary + "/backup"},
	} {
		if w := appRequest(t, s, token, "POST", "/api/files/action", body); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}

func TestApplicationSharingAndRevisions(t *testing.T) {
	f, _, s, token := applicationServer(t)
	for _, external := range []string{"false", "true"} {
		w := appRequest(t, s, token, "GET", "/api/sharing?account=campus&path="+url.QueryEscape(fixtureLibrary+"/report")+"&external="+external, nil)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if w := appRequest(t, s, token, "GET", "/api/sharing/recipients?account=campus&q=student", nil); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, action := range []string{"internal_link", "grant", "revoke", "external_link", "remove_link"} {
		body := map[string]any{"account": "campus", "path": fixtureLibrary + "/report", "action": action, "confirm": true, "recipients": []map[string]string{{"id": "recipient", "type": "user"}}, "linkId": "anonymous-link"}
		w := appRequest(t, s, token, "POST", "/api/sharing/action", body)
		if w.Code != 200 && w.Code != 204 {
			t.Fatal(action, w.Code, w.Body.String())
		}
	}
	if w := appRequest(t, s, token, "POST", "/api/revisions/restore", map[string]any{"account": "campus", "path": fixtureLibrary + "/report", "revision": "old", "confirm": true}); w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	events := strings.Join(f.server.Mutations(), ",")
	for _, event := range []string{"realname", "anonymous", "grant", "revoke", "remove_link", "revision:old"} {
		if !strings.Contains(events, event) {
			t.Fatal("missing business operation", event, events)
		}
	}
}

func TestApplicationTransfersAndRecycleJobs(t *testing.T) {
	_, a, s, token := applicationServer(t)
	for _, input := range []operationInput{
		{Kind: "copy", Account: "campus", Source: fixtureLibrary + "/report", Destination: fixtureLibrary + "/backup"},
		{Kind: "extract", Account: "campus", Source: fixtureLibrary + "/archive", Destination: fixtureLibrary + "/backup"},
	} {
		view := decodeResponse[jobInfo](t, appRequest(t, s, token, "POST", "/api/tasks", input), 202)
		result := waitJob(t, a, view.ID)
		if result.State != "completed" {
			t.Fatalf("%s failed: %+v", input.Kind, result.Error)
		}
	}
	view := decodeResponse[jobInfo](t, appRequest(t, s, token, "POST", "/api/recycle/action", map[string]any{"account": "campus", "action": "restore", "ids": []string{fixtureLibrary + "/bin/old"}, "confirm": true, "rename": true}), 202)
	if result := waitJob(t, a, view.ID); result.State != "completed" {
		t.Fatal(result.Error)
	}
}

func TestLoginChallengeOwnershipAndCancellation(t *testing.T) {
	_, a, s, token := applicationServer(t)
	input := LoginInput{Name: "verified", Username: "verify", Password: "private-new-password"}
	view := decodeResponse[loginInfo](t, appRequest(t, s, token, "POST", "/api/logins", input), 202)
	var status loginInfo
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status = decodeResponse[loginInfo](t, appRequest(t, s, token, "GET", "/api/logins/"+view.ID, nil), 200)
		if status.State == "challenge" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if status.Challenge == nil {
		t.Fatal("verification challenge not delivered")
	}
	if strings.Contains(appRequest(t, s, token, "GET", "/api/logins/"+view.ID, nil).Body.String(), input.Password) {
		t.Fatal("password leaked through polling")
	}
	other := login(t, s)
	if w := appRequest(t, s, other, "GET", "/api/logins/"+view.ID, nil); w.Code != 404 {
		t.Fatal("another browser read the challenge")
	}
	answer := map[string]string{"challenge": status.Challenge.ID, "answer": "123456"}
	if w := appRequest(t, s, token, "POST", "/api/logins/"+view.ID+"/answer", answer); w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status = decodeResponse[loginInfo](t, appRequest(t, s, token, "GET", "/api/logins/"+view.ID, nil), 200)
		if status.State == "completed" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if status.State != "completed" {
		t.Fatal(status)
	}
	if w := appRequest(t, s, token, "POST", "/api/logins/"+view.ID+"/answer", answer); w.Code != 400 {
		t.Fatal("replayed challenge accepted")
	}
	input.Name = "cancelled"
	view = decodeResponse[loginInfo](t, appRequest(t, s, token, "POST", "/api/logins", input), 202)
	if w := appRequest(t, s, token, "DELETE", "/api/logins/"+view.ID, nil); w.Code != 204 {
		t.Fatal(w.Code)
	}
	a.Close()
	config, err := a.opts.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := config.Find("cancelled"); exists {
		t.Fatal("cancelled verification saved an account")
	}
}

func TestMountFailureCanBeRetriedAndBlocksQuit(t *testing.T) {
	f := newBusinessFixture(t)
	opts := f.options()
	var attempts atomic.Int32
	opts.Mount = func(ctx context.Context, _ MountInput, _ []*Connection, ready func(), stop <-chan chan error) error {
		ready()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case reply := <-stop:
				if attempts.Add(1) == 1 {
					reply <- errors.New("open files are busy")
				} else {
					reply <- nil
					return nil
				}
			}
		}
	}
	a := NewApplication(context.Background(), opts)
	defer a.Close()
	s, _, _ := testServer(t)
	s.application = a
	token := login(t, s)
	view := decodeResponse[jobInfo](t, appRequest(t, s, token, "POST", "/api/mounts", MountInput{Account: "campus", Point: filepath.Join(f.root, "mount")}), 202)
	waitState := func(want string, withError bool) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			a.mu.Lock()
			info := a.jobs[view.ID].info
			a.mu.Unlock()
			if info.State == want && (!withError || info.Error != nil) {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("mount did not reach", want)
	}
	waitState("mounted", false)
	if w := appRequest(t, s, token, "POST", "/api/quit", nil); w.Code != 409 {
		t.Fatal("quit ignored live mount", w.Code)
	}
	if w := appRequest(t, s, token, "POST", "/api/tasks/"+view.ID+"/cancel", nil); w.Code != 204 {
		t.Fatal(w.Code)
	}
	waitState("mounted", true)
	if w := appRequest(t, s, token, "POST", "/api/tasks/"+view.ID+"/cancel", nil); w.Code != 204 {
		t.Fatal(w.Code)
	}
	result := waitJob(t, a, view.ID)
	if result.State != "completed" || result.Error != nil {
		t.Fatal("successful retry retained error", result)
	}
	if w := appRequest(t, s, token, "POST", "/api/quit", nil); w.Code != 204 {
		t.Fatal("idle service did not quit", w.Code)
	}
}

func TestAllAccountMountProtectsItsAccountSnapshot(t *testing.T) {
	f := newBusinessFixture(t)
	opts := f.options()
	mounted := make(chan []string, 1)
	opts.Mount = func(ctx context.Context, _ MountInput, connections []*Connection, ready func(), _ <-chan chan error) error {
		var names []string
		for _, c := range connections {
			names = append(names, c.Name)
		}
		ready()
		mounted <- names
		<-ctx.Done()
		return ctx.Err()
	}
	a := NewApplication(context.Background(), opts)
	defer a.Close()
	s, _, _ := testServer(t)
	s.application = a
	token := login(t, s)
	decodeResponse[jobInfo](t, appRequest(t, s, token, "POST", "/api/mounts", MountInput{AllAccounts: true, Point: filepath.Join(f.root, "mount")}), 202)
	select {
	case names := <-mounted:
		if strings.Join(names, ",") != "campus,work" {
			t.Fatal("unexpected mounted accounts", names)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("mount did not become ready")
	}
	for _, name := range []string{"campus", "work"} {
		if w := appRequest(t, s, token, "POST", "/api/accounts/remove", map[string]any{"account": name, "confirm": true}); w.Code != 409 {
			t.Fatalf("mounted account %s could be removed: %d", name, w.Code)
		}
		if w := appRequest(t, s, token, "POST", "/api/logins", LoginInput{Account: name}); w.Code != 409 {
			t.Fatalf("mounted session %s could be replaced: %d", name, w.Code)
		}
	}
	if err := f.store.Put(accounts.Account{Name: "later", Username: "later", Server: f.server.URL, Password: "private-fixture-password"}, false, false); err != nil {
		t.Fatal(err)
	}
	if w := appRequest(t, s, token, "POST", "/api/accounts/remove", map[string]any{"account": "later", "confirm": true}); w.Code != 204 {
		t.Fatal("new account was incorrectly treated as mounted", w.Code, w.Body.String())
	}
}

func TestQueueWaitDoesNotConsumeOperationTimeout(t *testing.T) {
	f := newBusinessFixture(t)
	a := NewApplication(context.Background(), f.options())
	defer a.Close()
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	for range 2 {
		_, err := a.newJob(jobInfo{Kind: "copy"}, 0, func(ctx context.Context, _ *job) (any, error) {
			started <- struct{}{}
			select {
			case <-release:
				return nil, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	<-started
	<-started
	view, err := a.newJob(jobInfo{Kind: "copy"}, 50*time.Millisecond, func(ctx context.Context, _ *job) (any, error) { return nil, ctx.Err() })
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	a.mu.Lock()
	state := a.jobs[view.ID].info.State
	a.mu.Unlock()
	if state != "queued" {
		t.Fatal("queued operation timed out before starting", state)
	}
	close(release)
	if result := waitJob(t, a, view.ID); result.State != "completed" {
		t.Fatal(result.Error)
	}
}
