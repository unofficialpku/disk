package gui

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/unofficialpku/disk/internal/localfile"
)

type instanceInfo struct {
	Version int    `json:"version"`
	Origin  string `json:"origin"`
	Control string `json:"control"`
}

type instance struct {
	lock *os.File
	path string
}

// acquire returns nil when an existing instance has opened the browser. The
// OS lock, not a PID or stale metadata file, is the authority on ownership.
func acquire(ctx context.Context, dir string) (*instance, error) {
	if err := localfile.EnsurePrivateDir(dir); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	path := filepath.Join(dir, "instance.json")
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("existing GUI is not responding: %w", err)
		}
		lock, err := localfile.TryLock(filepath.Join(dir, "instance.lock"), true)
		if err != nil {
			return nil, err
		}
		if lock != nil {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				lock.Close()
				return nil, err
			}
			return &instance{lock: lock, path: path}, nil
		}
		info, err := readInstance(path)
		if err == nil {
			activated, err := activateInstance(ctx, info)
			if err != nil {
				return nil, err
			}
			if activated {
				return nil, nil
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}

func readInstance(path string) (instanceInfo, error) {
	var info instanceInfo
	file, err := localfile.OpenPrivate(path, os.O_RDONLY)
	if err != nil {
		return info, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(data) > 4096 || json.Unmarshal(data, &info) != nil {
		return info, errors.New("invalid GUI instance metadata")
	}
	u, err := url.Parse(info.Origin)
	if err != nil || info.Version != 1 || u.Scheme != "http" || u.Hostname() != "127.0.0.1" ||
		u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return info, errors.New("unsupported GUI instance metadata")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 || u.Host != "127.0.0.1:"+strconv.Itoa(port) {
		return info, errors.New("invalid GUI instance address")
	}
	secret, err := base64.RawURLEncoding.DecodeString(info.Control)
	if err != nil || len(secret) != 32 || len(info.Control) != 43 {
		return info, errors.New("invalid GUI instance credentials")
	}
	return info, nil
}

func activateInstance(ctx context.Context, info instanceInfo) (bool, error) {
	return controlInstance(ctx, info, "activate")
}

func controlInstance(ctx context.Context, info instanceInfo, action string) (bool, error) {
	// Bypass proxy environment variables, and never follow a redirect with the
	// control credential. Only a validated IPv4 loopback address reaches here.
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 6 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, info.Origin+"/internal/"+action, nil)
	if err != nil {
		return false, errors.New("invalid GUI control request")
	}
	req.Header.Set("Authorization", "Bearer "+info.Control)
	response, err := client.Do(req)
	if err != nil {
		return false, nil // The owner may still be starting, or releasing its lock.
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNoContent {
		return true, nil
	}
	if action == "stop" {
		if response.StatusCode == http.StatusConflict {
			return false, ErrBusy
		}
		if response.StatusCode == http.StatusNotFound {
			return false, errors.New("running GUI does not support gui stop; exit it from the tray/page once and restart with this version")
		}
	}
	return false, fmt.Errorf("GUI %s failed (HTTP %d)", action, response.StatusCode)
}

// Stop uses the same private control channel as activation. It never trusts a
// stale PID, starts a service, or force-kills work. Success waits for ownership
// to be released; a replacement instance must not receive another stop request.
func Stop(ctx context.Context, dir string) (bool, error) {
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var accepted *instanceInfo
	for {
		if err := ctx.Err(); err != nil {
			return false, fmt.Errorf("waiting for GUI to stop: %w", err)
		}
		lock, err := localfile.TryLock(filepath.Join(dir, "instance.lock"), true)
		if err != nil {
			return false, err
		}
		if lock != nil {
			_ = lock.Close()
			return accepted != nil, nil
		}
		info, err := readInstance(filepath.Join(dir, "instance.json"))
		if err == nil {
			if accepted != nil {
				if info.Control != accepted.Control {
					return true, nil
				}
			} else {
				ok, err := controlInstance(ctx, info, "stop")
				if err != nil {
					return false, err
				}
				if ok {
					accepted = &info
				}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (i *instance) publish(info instanceInfo) error {
	data, err := json.Marshal(info)
	if err != nil {
		return err
	}
	file, err := localfile.CreateTemp(filepath.Dir(i.path), ".instance-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err := io.Copy(file, bytes.NewReader(data)); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := localfile.Install(file.Name(), i.path, true); err != nil {
		return err
	}
	return localfile.SyncDir(filepath.Dir(i.path))
}

func (i *instance) close() {
	// Remove discovery metadata while still holding the persistent lock inode.
	_ = os.Remove(i.path)
	_ = i.lock.Close()
}
