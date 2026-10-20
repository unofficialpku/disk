package gui

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/unofficialpku/disk/internal/accounts"
	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/unofficialpku/disk/internal/auth"
	"golang.org/x/sync/semaphore"
	"golang.org/x/sync/singleflight"
)

var (
	ErrLoginRequired = errors.New("account needs interactive verification")
	ErrBusy          = errors.New("finish or cancel active operations and unmount filesystems first")
	ErrClosed        = errors.New("desktop service is stopping")
)

type LoginInput struct {
	Account  string `json:"account"`
	Name     string `json:"name"`
	Username string `json:"username"`
	Password string `json:"password"`
	Server   string `json:"server"`
	Replace  bool   `json:"replace"`
	Default  bool   `json:"default"`
}

// Connection is created by the shared authentication adapter. Private
// credentials and renewable server sessions never become browser DTOs.
type Connection struct {
	Name, Server, Username string
	Client                 *anyshare.Client
	Session                *auth.Session
}

func (c *Connection) close() {
	if c.Session != nil {
		c.Session.Close()
	}
}

type MountInput struct {
	Account        string `json:"account"`
	AllAccounts    bool   `json:"allAccounts"`
	Point          string `json:"point"`
	Root           string `json:"root"`
	ReadOnly       bool   `json:"readOnly"`
	Cache          string `json:"cache"`
	CacheLimit     string `json:"cacheLimit"`
	PartSize       string `json:"partSize"`
	WholeFile      string `json:"wholeFile"`
	BlockSize      string `json:"blockSize"`
	ReadAhead      string `json:"readAhead"`
	Concurrency    int    `json:"concurrency"`
	TTLSeconds     *int   `json:"ttlSeconds"`
	RefreshSeconds *int   `json:"refreshSeconds"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
	Verbose        bool   `json:"verbose"`
	Debug          bool   `json:"debug"`
}

type ApplicationOptions struct {
	Store          *accounts.Store
	CacheDirectory string
	Connect        func(context.Context, LoginInput, auth.ChallengeFunc) (*Connection, error)
	CheckMount     func() error
	ValidateMount  func(MountInput) error
	Mount          func(context.Context, MountInput, []*Connection, func(), <-chan chan error) error
}

type Application struct {
	opts          ApplicationOptions
	ctx           context.Context
	cancel        context.CancelFunc
	mu            sync.Mutex
	closing       bool
	activeWrites  int
	connections   map[string]*Connection
	logins        map[string]*loginAttempt
	jobs          map[string]*job
	workers       sync.WaitGroup
	connecting    singleflight.Group
	slots         *semaphore.Weighted
	handler       *http.ServeMux
	uploadSlots   *semaphore.Weighted
	uploadFolders sync.Mutex
	downloads     map[string]browserDownload
}

func NewApplication(ctx context.Context, opts ApplicationOptions) *Application {
	ctx, cancel := context.WithCancel(ctx)
	a := &Application{opts: opts, ctx: ctx, cancel: cancel, connections: make(map[string]*Connection),
		logins: make(map[string]*loginAttempt), jobs: make(map[string]*job), slots: semaphore.NewWeighted(2), uploadSlots: semaphore.NewWeighted(2), downloads: make(map[string]browserDownload)}
	a.handler = http.NewServeMux()
	a.routes()
	return a
}

// BeginShutdown prevents an operation from racing the final empty/busy check.
// GUI exit is refused while work is active; OS signals still cancel cleanly.
func (a *Application) BeginShutdown() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.busyLocked("") || a.activeWrites > 0 {
		return ErrBusy
	}
	a.closing = true
	return nil
}

func (a *Application) busyLocked(account string) bool {
	for _, j := range a.jobs {
		if (!j.finished || j.responsePending) && (account == "" || j.info.Account == account || slices.Contains(j.info.Accounts, account)) {
			return true
		}
	}
	for _, login := range a.logins {
		if !login.finished && (account == "" || login.info.Account == account) {
			return true
		}
	}
	return false
}

func (a *Application) Close() {
	a.mu.Lock()
	a.closing = true
	a.cancel()
	a.mu.Unlock()
	a.workers.Wait()
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, c := range a.connections {
		c.close()
	}
	clear(a.connections)
	clear(a.logins)
}

func (a *Application) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		a.mu.Lock()
		if a.closing {
			a.mu.Unlock()
			writeError(w, ErrClosed)
			return
		}
		a.activeWrites++
		a.mu.Unlock()
		defer func() { a.mu.Lock(); a.activeWrites--; a.mu.Unlock() }()
	}
	// Authenticated operations can involve a slow upstream
	// API. Keep an explicit operation deadline rather than the bootstrap timeout.
	timeout := 2 * time.Minute
	if r.URL.Path == "/api/uploads" {
		timeout = 24 * time.Hour
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	stop := context.AfterFunc(a.ctx, cancel)
	defer stop()
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(timeout + 5*time.Second))
	if r.URL.Path == "/api/uploads" {
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(timeout))
	}
	a.handler.ServeHTTP(w, r.WithContext(ctx))
}

func (a *Application) account(ctx context.Context, name string) (*Connection, error) {
	config, err := a.opts.Store.Load()
	if err != nil {
		return nil, err
	}
	if name == "" {
		name = config.Default
	}
	saved, ok := config.Find(name)
	if !ok {
		return nil, accounts.ErrNotFound
	}
	a.mu.Lock()
	if a.closing {
		a.mu.Unlock()
		return nil, ErrClosed
	}
	connected := a.connections[name]
	if connected != nil && connected.Server == saved.Server && connected.Username == saved.Username {
		a.mu.Unlock()
		return connected, nil
	}
	a.mu.Unlock()
	result := a.connecting.DoChan(name, func() (any, error) {
		a.mu.Lock()
		if a.closing {
			a.mu.Unlock()
			return nil, ErrClosed
		}
		if c := a.connections[name]; c != nil && c.Server == saved.Server && c.Username == saved.Username {
			a.mu.Unlock()
			return c, nil
		}
		a.workers.Add(1)
		a.mu.Unlock()
		defer a.workers.Done()
		loginCtx, cancel := context.WithTimeout(a.ctx, 90*time.Second)
		defer cancel()
		c, err := a.opts.Connect(loginCtx, LoginInput{Account: name}, func(context.Context, auth.Challenge) (string, error) { return "", ErrLoginRequired })
		if err != nil {
			return nil, err
		}
		if err := a.install(c); err != nil {
			return nil, err
		}
		return c, nil
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-result:
		if result.Err != nil {
			return nil, result.Err
		}
		return result.Val.(*Connection), nil
	}
}

func (a *Application) install(c *Connection) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closing {
		c.close()
		return ErrClosed
	}
	if old := a.connections[c.Name]; old != nil {
		old.close()
	}
	a.connections[c.Name] = c
	return nil
}
