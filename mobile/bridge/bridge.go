// Package mobilegui is the shared gomobile boundary for Android and iOS hosts.
// Native hosts provide private directories and own their WebView/WKWebView.
package mobilegui

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/unofficialpku/disk/internal/accounts"
	"github.com/unofficialpku/disk/internal/gui"
)

// Bridge owns one in-process local service. Its public API uses only gomobile
// supported types, so the same package produces Java and Objective-C bindings.
type Bridge struct {
	mu                        sync.Mutex
	directory, cache, version string
	current                   *instance
	running                   atomic.Bool
	quitRequested             atomic.Bool
	suspended                 atomic.Bool
}
type instance struct {
	ctx         context.Context
	cancel      context.CancelFunc
	done, ready chan struct{}
	err         error // Written before done closes.
	urlMu       sync.Mutex
	url         string
	options     gui.Options
	application *gui.Application
}

func NewBridge(dataDirectory, cacheDirectory, version string) *Bridge {
	return &Bridge{directory: dataDirectory, cache: cacheDirectory, version: version}
}

// Open starts the local service if needed and returns a fresh one-use URL.
// Never log this URL or send it to another application.
func (b *Bridge) Open() (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.directory == "" || b.cache == "" {
		return "", errors.New("private data and cache directories are required")
	}
	if b.current != nil {
		select {
		case <-b.current.done:
			b.current.cancel()
			b.current = nil
		default:
			if err := gui.Run(b.current.ctx, b.current.options); err != nil {
				return "", err
			}
			return b.current.address(), nil
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	b.quitRequested.Store(false)
	b.suspended.Store(false)
	next := &instance{ctx: ctx, cancel: cancel, done: make(chan struct{}), ready: make(chan struct{})}
	store := &accounts.Store{Path: filepath.Join(b.directory, "accounts.json")}
	next.options = gui.Options{
		Directory: filepath.Join(b.directory, "gui"), Version: b.version,
		OnQuit: func() { b.quitRequested.Store(true) },
		NewApplication: func(ctx context.Context) (*gui.Application, error) {
			next.application = gui.NewApplication(ctx, gui.ApplicationOptions{
				Store: store, CacheDirectory: b.cache, Connect: gui.NewConnector(store),
				CheckMount: func() error { return errors.New("filesystem mounting is unavailable on mobile") },
			})
			return next.application, nil
		},
		NewTray:     func(func(), func()) (gui.Tray, error) { return nil, nil },
		OpenBrowser: func(address string) error { next.urlMu.Lock(); next.url = address; next.urlMu.Unlock(); return nil },
		Ready: func(owner bool) {
			if owner {
				b.running.Store(true)
				close(next.ready)
			}
		},
	}
	b.current = next
	go func() { next.err = gui.Run(ctx, next.options); b.running.Store(false); close(next.done) }()
	select {
	case <-next.ready:
		return next.address(), nil
	case <-next.done:
		cancel()
		b.current = nil
		if next.err != nil {
			return "", next.err
		}
		return "", errors.New("another mobile bridge already owns this service")
	}
}
func (i *instance) address() string { i.urlMu.Lock(); defer i.urlMu.Unlock(); return i.url }

// Running is nonblocking, including while another call waits for shutdown.
func (b *Bridge) Running() bool { return b.running.Load() }

// QuitRequested distinguishes explicit graceful exit from service failure or
// host cancellation. Hosts may close their window once Running becomes false.
// Opening a new service resets this flag.
func (b *Bridge) QuitRequested() bool { return b.quitRequested.Load() }

// Suspended reports an idle background release, which a host may automatically
// resume. Unexpected service failures and explicit Quit do not set this flag.
func (b *Bridge) Suspended() bool { return b.suspended.Load() }

// Status exposes the same task state to native hosts on Android and iOS. Hosts
// own notifications and background policy; the core contains neither.
func (b *Bridge) Status() (string, error) {
	b.mu.Lock()
	current := b.current
	b.mu.Unlock()
	state := struct {
		Running       bool           `json:"running"`
		QuitRequested bool           `json:"quitRequested"`
		Busy          bool           `json:"busy"`
		Tasks         []gui.TaskInfo `json:"tasks"`
	}{Running: b.Running(), QuitRequested: b.QuitRequested(), Tasks: []gui.TaskInfo{}}
	if current != nil && current.application != nil {
		state.Tasks = current.application.Tasks()
		state.Busy = current.application.Busy()
	}
	data, err := json.Marshal(state)
	return string(data), err
}

// Suspend releases an idle service without cancelling work or requesting app
// exit. BeginShutdown makes the empty check atomic with new operations.
func (b *Bridge) Suspend() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.current == nil {
		return true
	}
	if !b.Running() || b.QuitRequested() || b.current.application.BeginShutdown() != nil {
		return false
	}
	b.current.cancel()
	<-b.current.done
	b.current = nil
	b.suspended.Store(true)
	return true
}

// Stop requests graceful shutdown and refuses to interrupt active operations.
func (b *Bridge) Stop() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.current == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := gui.Stop(ctx, b.current.options.Directory); err != nil {
		return err
	}
	<-b.current.done
	b.current.cancel()
	b.current = nil
	return nil
}

// Close cancels outstanding work for host destruction or a platform timeout.
// Call it off the UI thread. The same Bridge can be opened again afterwards.
func (b *Bridge) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.current == nil {
		return
	}
	b.current.cancel()
	<-b.current.done
	b.current = nil
}
