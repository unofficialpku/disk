package gui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/unofficialpku/disk/internal/transfer"
)

func TestTaskSnapshotCoalescesAttemptsAndKeepsResponseActive(t *testing.T) {
	app := NewApplication(context.Background(), ApplicationOptions{})
	t.Cleanup(app.Close)
	created := time.Now()
	app.jobs = map[string]*job{
		"old":           {info: jobInfo{ID: "old", Account: "campus", Kind: "download", ClientID: "handoff", Created: created}, finished: true, responsePending: true},
		"new":           {info: jobInfo{ID: "new", Account: "campus", Kind: "download", ClientID: "handoff", Created: created.Add(time.Second), State: "completed", Percent: 100}, finished: true},
		"other-account": {info: jobInfo{ID: "other-account", Account: "work", Kind: "download", ClientID: "handoff", Created: created.Add(2 * time.Second), State: "running"}},
		"other-kind":    {info: jobInfo{ID: "other-kind", Account: "campus", Kind: "upload", ClientID: "handoff", Created: created.Add(3 * time.Second), State: "queued"}},
	}
	rows := app.Tasks()
	if len(rows) != 3 || rows[0].ID != "other-kind" || rows[1].ID != "other-account" || rows[2].ID != "new" {
		t.Fatalf("snapshot lost isolation/order or retained duplicate handoff: %+v", rows)
	}
	if !rows[2].Active || rows[2].Percent != 100 || rows[2].State != "completed" {
		t.Fatalf("latest progress or older response lifetime lost: %+v", rows[2])
	}
	app.jobs["old"].responsePending = false
	rows[2].Name = "modified by host"
	if fresh := app.Tasks()[2]; fresh.Active || fresh.Name != "" {
		t.Fatalf("snapshot shared mutable data or retained old lifetime: %+v", fresh)
	}
	if len(app.snapshotJobs()) != 4 {
		t.Fatal("raw API snapshot must retain all attempts")
	}
	delete(app.jobs, "other-account")
	delete(app.jobs, "other-kind")
	app.jobs["old"].responsePending = true
	if !app.Busy() || !errors.Is(app.BeginShutdown(), ErrBusy) {
		t.Fatal("host shutdown ignored an older attempt's pending response")
	}
	app.jobs["old"].responsePending = false
	if app.Busy() {
		t.Fatal("completed attempts kept application busy")
	}
}

func TestBusyWhileReceivingUploadBeforeTaskExists(t *testing.T) {
	_, app, server, token := applicationServer(t)
	reader, writer := io.Pipe()
	multipartWriter := multipart.NewWriter(writer)
	request := httptest.NewRequest("POST", server.origin+"/api/uploads", reader)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Origin", server.origin)
	request.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.ServeHTTP(response, request)
	}()
	defer func() {
		_ = reader.Close()
		_ = writer.Close()
		<-done
	}()
	for key, value := range map[string]string{
		"account": "campus", "destination": fixtureLibrary, "relativePath": "slow-upload.txt",
		"lastModified": "1720000000000", "timeoutSeconds": "600", "fast": "true",
	} {
		if err := multipartWriter.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	part, err := multipartWriter.CreateFormFile("file", "slow-upload.txt")
	if err != nil {
		t.Fatal(err)
	}
	// A successful pipe write proves the handler is receiving the body, while
	// the unfinished multipart boundary keeps it before task creation.
	if _, err := part.Write([]byte("upload body still arriving")); err != nil {
		t.Fatal(err)
	}
	if len(app.Tasks()) != 0 || !app.Busy() {
		t.Fatal("incoming upload must keep host alive before a task exists")
	}
	if err := app.BeginShutdown(); !errors.Is(err, ErrBusy) {
		t.Fatalf("shutdown accepted unfinished upload body: %v", err)
	}
	if err := multipartWriter.Close(); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("upload did not finish")
	}
	if response.Code != 200 {
		t.Fatalf("upload failed: %d %s", response.Code, response.Body.String())
	}
	if app.Busy() {
		t.Fatal("finished upload kept host alive")
	}
	if err := app.BeginShutdown(); err != nil {
		t.Fatalf("shutdown rejected after upload released its resources: %v", err)
	}
}

func TestTaskSnapshotConcurrentProgress(t *testing.T) {
	app := NewApplication(context.Background(), ApplicationOptions{})
	t.Cleanup(app.Close)
	j := &job{info: jobInfo{ID: "download", Kind: "download", State: "running"}}
	app.jobs[j.info.ID] = j
	observer := app.observe(j)
	var workers sync.WaitGroup
	workers.Go(func() {
		for done := int64(0); done <= 1000; done++ {
			observer(transfer.Progress{Done: done, Total: 1000})
		}
	})
	for range 1000 {
		row := app.Tasks()[0]
		if row.Total != 0 && row.Percent != int(100*float64(row.Done)/float64(row.Total)) {
			t.Fatalf("snapshot mixed progress updates: %+v", row)
		}
		row.Name = "host-local change"
	}
	workers.Wait()
	if row := app.Tasks()[0]; row.Done != 1000 || row.Percent != 100 || row.Name != "" {
		t.Fatalf("unexpected final snapshot: %+v", row)
	}
	delete(app.jobs, j.info.ID)
	data, err := json.Marshal(app.Tasks())
	if err != nil || string(data) != "[]" {
		t.Fatalf("empty snapshot = %s, %v", data, err)
	}
}
