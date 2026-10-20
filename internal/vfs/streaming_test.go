package vfs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unofficialpku/disk/internal/anyshare"
)

type streamToken struct{}

func (streamToken) AccessToken() string                   { return "test-access" }
func (streamToken) Refresh(context.Context, string) error { return anyshare.ErrPermission }

type slowObject struct {
	data        []byte
	release     chan struct{}
	prefix      chan struct{}
	cancelled   chan struct{}
	cut         bool
	auths, gets atomic.Int64
}

func streamingTest(t *testing.T, cut bool) (*FileSystem, *Node, *slowObject) {
	t.Helper()
	slow := &slowObject{data: bytes.Repeat([]byte{0x7b}, 5<<20), release: make(chan struct{}), prefix: make(chan struct{}), cancelled: make(chan struct{}), cut: cut}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/efast/v1/file/osdownload" {
			slow.auths.Add(1)
			json.NewEncoder(w).Encode(map[string]any{"authrequest": []string{"GET", "http://" + r.Host + "/object"}})
			return
		}
		if r.URL.Path != "/object" {
			http.NotFound(w, r)
			return
		}
		request := slow.gets.Add(1)
		if r.Header.Get("Range") != fmt.Sprintf("bytes=0-%d", len(slow.data)-1) {
			t.Error("streaming read did not share one full-file range")
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(slow.data)))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(slow.data)-1, len(slow.data)))
		w.WriteHeader(http.StatusPartialContent)
		if request > 1 {
			w.Write(slow.data)
			return
		}
		w.Write(slow.data[:64<<10])
		w.(http.Flusher).Flush()
		close(slow.prefix)
		select {
		case <-slow.release:
			if !slow.cut {
				w.Write(slow.data[64<<10:])
			}
		case <-r.Context().Done():
			close(slow.cancelled)
		}
	}))
	t.Cleanup(server.Close)
	client, err := anyshare.New(server.URL, streamToken{})
	if err != nil {
		t.Fatal(err)
	}
	f, _ := readCacheTest(t, 16<<20)
	f.backend = client
	node := f.makeNode(anyshare.Entry{ID: "gns://library/photo", Name: "photo.jpg", Rev: "1", Size: int64(len(slow.data))})
	return f, node, slow
}

func readStreamPrefix(t *testing.T, node *Node, slow *slowObject) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		got := make([]byte, 64<<10)
		n, err := node.readAt(context.Background(), got, 0)
		if err == nil && (n != len(got) || !bytes.Equal(got, slow.data[:len(got)])) {
			err = errors.New("streamed prefix differs")
		}
		done <- err
	}()
	select {
	case <-slow.prefix:
	case <-time.After(2 * time.Second):
		t.Fatal("prefix was not sent")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("64 KiB read waited for the blocked 5 MiB suffix")
	}
}

func streamFlight(f *FileSystem, node *Node) *readFlight {
	key := fileKey(node.snapshot(), 0)
	f.budgetMu.Lock()
	defer f.budgetMu.Unlock()
	return f.readFlights[key]
}

func TestStreamingPrefixReturnsBeforeSuffixAndSharesOneGET(t *testing.T) {
	f, node, slow := streamingTest(t, false)
	readStreamPrefix(t, node, slow)
	flight := streamFlight(f, node)
	if flight == nil {
		t.Fatal("range stopped filling after its first reader returned")
	}
	for i := 0; i < 4; i++ {
		got := make([]byte, 1024)
		if _, err := node.readAt(context.Background(), got, int64(i*1024)); err != nil || !bytes.Equal(got, slow.data[:len(got)]) {
			t.Fatal("available prefix could not be reused", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := node.readAt(ctx, make([]byte, 1), node.entry.Size-1); !errors.Is(err, context.Canceled) {
		t.Fatal("tail waiter ignored cancellation", err)
	}
	select {
	case <-slow.cancelled:
		t.Fatal("one cancelled waiter interrupted a useful shared transfer")
	default:
	}
	full := make([]byte, len(slow.data))
	done := make(chan error, 1)
	go func() { _, err := node.readAt(context.Background(), full, 0); done <- err }()
	select {
	case err := <-done:
		t.Fatal("full read completed before its tail arrived", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(slow.release)
	if err := <-done; err != nil || !bytes.Equal(full, slow.data) {
		t.Fatal("complete streamed image differs", err)
	}
	<-flight.done
	if slow.auths.Load() != 1 || slow.gets.Load() != 1 {
		t.Fatal("progressive reads duplicated an authorization or GET")
	}
	f.budgetMu.Lock()
	charged := f.cacheBytes
	f.budgetMu.Unlock()
	if charged != node.entry.Size {
		t.Fatal("complete range was not retained after validation")
	}
}

func TestStreamingFailureNeverRetainsAnIncompleteRange(t *testing.T) {
	f, node, slow := streamingTest(t, true)
	readStreamPrefix(t, node, slow)
	flight := streamFlight(f, node)
	close(slow.release)
	<-flight.done
	if flight.err == nil {
		t.Fatal("truncated range was accepted")
	}
	f.budgetMu.Lock()
	retained := len(f.readFiles) != 0 || f.cacheBytes != 0
	f.budgetMu.Unlock()
	if retained {
		t.Fatal("truncated stream entered the clean cache")
	}
	full := make([]byte, len(slow.data))
	if _, err := node.readAt(context.Background(), full, 0); err != nil || !bytes.Equal(full, slow.data) {
		t.Fatal("failed range could not be fetched anew", err)
	}
	if slow.gets.Load() != 2 {
		t.Fatal("incomplete range was reused")
	}
}

func TestStreamingUnmountCancelsAndJoinsPendingFill(t *testing.T) {
	f, node, slow := streamingTest(t, false)
	readStreamPrefix(t, node, slow)
	done := make(chan error, 1)
	go func() { done <- f.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("unmount did not cancel/join a blocked range fill")
	}
	select {
	case <-slow.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("object request remained active after unmount")
	}
	if _, err := os.Stat(f.CachePath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial read cache survived unmount", err)
	}
	if _, err := node.readAt(context.Background(), make([]byte, 1), 0); !errors.Is(err, context.Canceled) {
		t.Fatal("closed mount started another range", err)
	}
}

func TestStreamingAbandonedColdReadCancelsItsTransfer(t *testing.T) {
	f, b := readCacheTest(t, 1<<20)
	entry := seedFile(t, b.memoryBackend, "photo.jpg", make([]byte, 4096))
	started, cancelled := make(chan struct{}), make(chan struct{})
	b.stream = func(ctx context.Context, _ anyshare.Entry, _, _ int64, _ io.Writer) error {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	}
	node := f.makeNode(entry)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := node.readAt(ctx, make([]byte, 1), 0); done <- err }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("unused cold transfer continued after its only waiter cancelled")
	}
}

func TestStreamingNewReaderDoesNotJoinAbandonedTransfer(t *testing.T) {
	f, b := readCacheTest(t, 1<<20)
	entry := seedFile(t, b.memoryBackend, "photo.jpg", []byte("new data"))
	started, oldCleanup := make(chan struct{}), make(chan struct{})
	newCleanup := make(chan struct{})
	var oldOnce, newOnce sync.Once
	finishOld := func() { oldOnce.Do(func() { close(oldCleanup) }) }
	finishNew := func() { newOnce.Do(func() { close(newCleanup) }) }
	defer finishOld()
	defer finishNew()
	b.stream = func(ctx context.Context, _ anyshare.Entry, _, _ int64, dest io.Writer) error {
		if b.reads.Load() == 1 {
			close(started)
			<-ctx.Done()
			<-oldCleanup
			// Simulate a transport completing buffered bytes during cancellation.
			_, err := dest.Write([]byte("new data"))
			return err
		}
		if _, err := dest.Write([]byte("new ")); err != nil {
			return err
		}
		<-newCleanup
		_, err := dest.Write([]byte("data"))
		return err
	}
	node := f.makeNode(entry)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := node.readAt(ctx, make([]byte, 1), 0); done <- err }()
	<-started
	old := streamFlight(f, node)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	got := make([]byte, 1)
	if _, err := node.readAt(context.Background(), got, 0); err != nil || got[0] != 'n' {
		t.Fatal("new reader joined a cancelled transfer", err)
	}
	current := streamFlight(f, node)
	finishOld()
	<-old.done
	if !errors.Is(old.err, context.Canceled) {
		t.Fatal("abandoned completed bytes were retained")
	}
	if streamFlight(f, node) != current {
		t.Fatal("old transfer removed the replacement's range index")
	}
	finishNew()
	got = make([]byte, 8)
	if _, err := node.readAt(context.Background(), got, 0); err != nil || string(got) != "new data" {
		t.Fatal(err)
	}
	if b.reads.Load() != 2 {
		t.Fatal("unexpected fetch count after abandonment")
	}
}
