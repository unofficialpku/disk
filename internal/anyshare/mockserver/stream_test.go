package mockserver

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/unofficialpku/disk/internal/anyshare"
)

func TestDelayedDownloadPreservesRange(t *testing.T) {
	fixture := New("campus")
	defer fixture.Close()
	data := bytes.Repeat([]byte("0123456789abcdef"), streamChunk/4)
	id := Library + "/slow"
	fixture.PutObject("campus", Object{Entry: anyshare.Entry{ID: id, Name: "slow.bin", Size: int64(len(data))}, Data: data, StreamDelay: 2 * time.Millisecond})
	request, err := http.NewRequest(http.MethodGet, fixture.URL+"/objects?account=campus&id="+url.QueryEscape(id), nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Range", "bytes=31-131102")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusPartialContent || !bytes.Equal(body, data[31:131103]) {
		t.Fatalf("range response: status=%d size=%d err=%v", response.StatusCode, len(body), err)
	}
	request.Method = http.MethodHead
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if body, err = io.ReadAll(response.Body); err != nil || len(body) != 0 || response.ContentLength != 131072 {
		t.Fatalf("head response: length=%d body=%d err=%v", response.ContentLength, len(body), err)
	}
}

func TestDelayedDownloadAllowsConcurrentRequestsAndCancellation(t *testing.T) {
	fixture := New("campus")
	defer fixture.Close()
	id := Library + "/slow"
	fixture.PutObject("campus", Object{Entry: anyshare.Entry{ID: id, Name: "slow.bin", Size: streamChunk * 2}, Data: make([]byte, streamChunk*2), StreamDelay: time.Minute})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fixture.URL+"/objects?account=campus&id="+url.QueryEscape(id), nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.CopyN(io.Discard, response.Body, streamChunk); err != nil {
		t.Fatal(err)
	}
	fastCtx, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	fastRequest, err := http.NewRequestWithContext(fastCtx, http.MethodGet, fixture.URL+"/objects?account=campus&id="+url.QueryEscape(Library+"/report"), nil)
	if err != nil {
		t.Fatal(err)
	}
	fastResponse, err := http.DefaultClient.Do(fastRequest)
	if err != nil {
		t.Fatal("slow stream blocked another request:", err)
	}
	_, _ = io.Copy(io.Discard, fastResponse.Body)
	_ = fastResponse.Body.Close()
	cancel()
	_ = response.Body.Close()
	closed := make(chan struct{})
	go func() { fixture.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt stream delay")
	}
}
