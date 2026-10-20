package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/unofficialpku/disk/internal/diagnostic"
)

// State contains the raw AnyShare cookie values. It is sensitive and must only
// be saved in a private credential store bound to the session's server/account.
// One-time OAuth challenges, IAAA cookies, and verification codes are excluded.
type State struct {
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
}

func (s *Session) state() State {
	state := State{AccessToken: s.token}
	for _, path := range []string{"/", "/oauth2/", "/anyshare/"} {
		u, _ := s.base.Parse(path)
		for _, cookie := range s.http.Jar.Cookies(u) {
			switch cookie.Name {
			case "client.oauth2_token":
				if state.AccessToken == "" {
					state.AccessToken = cookie.Value
				}
			case "client.oauth2_refresh_token":
				state.RefreshToken = cookie.Value
			}
		}
	}
	return state
}

// State returns a snapshot without exposing the private cookie jar.
func (s *Session) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state()
}

func (s *Session) applyState(state State) error {
	cookies := []*http.Cookie{
		{Name: "client.oauth2_token", Value: state.AccessToken, Path: "/", Secure: s.base.Scheme == "https", HttpOnly: true},
		{Name: "client.oauth2_refresh_token", Value: state.RefreshToken, Path: "/", Secure: s.base.Scheme == "https", HttpOnly: true},
	}
	for _, cookie := range cookies {
		if err := cookie.Valid(); err != nil {
			return fmt.Errorf("%w: invalid saved cookie", ErrSessionExpired)
		}
	}
	s.http.Jar.SetCookies(s.base, cookies)
	s.token = ""
	if state.AccessToken != "" {
		if err := s.updateToken(); err != nil {
			return fmt.Errorf("%w: invalid saved access token", ErrSessionExpired)
		}
	}
	return nil
}

// persistState runs under the session lock so rotating refresh tokens are saved
// in order. The callback must not call methods on this Session.
func (s *Session) persistState() error {
	if s.onState == nil {
		return nil
	}
	if err := s.onState(s.state()); err != nil {
		return fmt.Errorf("persist authentication session: %w", err)
	}
	return nil
}

// Restore validates a saved session by renewing it with the server before use.
// An expired refresh token returns ErrSessionExpired, allowing password login.
func (s *Session) Restore(ctx context.Context, state State) (result error) {
	ctx, finish := s.trace.Begin(ctx, "restore saved authentication session")
	defer func() { finish("%s", diagnostic.Status(result)) }()
	var unlock io.Closer
	var err error
	if s.refreshLock != nil {
		s.trace.Printf(ctx, "waiting for saved-session lock")
		unlock, err = s.refreshLock(ctx)
		if err != nil {
			return fmt.Errorf("lock authentication session: %w", err)
		}
		defer unlock.Close()
		s.trace.Printf(ctx, "saved-session lock acquired")
	}
	initial := state
	if s.loadState != nil {
		state, err = s.loadState()
		if err != nil {
			return err
		}
	}
	if state.RefreshToken == "" {
		return ErrSessionExpired
	}
	s.mu.Lock()
	if err = s.applyState(state); err != nil {
		s.mu.Unlock()
		return err
	}
	rejected := s.token
	s.mu.Unlock()
	// Another process may have refreshed this account while we were waiting
	// for the cross-process lock. The persisted state was already validated by
	// that refresh, so use it directly and avoid consuming the rotated token a
	// second time.
	if state != initial && state.AccessToken != "" {
		return nil
	}
	return s.refresh(ctx, rejected, false)
}

func (s *Session) refresh(ctx context.Context, rejected string, reload bool) error {
	s.trace.Printf(ctx, "waiting for authentication lock")
	s.mu.Lock()
	s.trace.Printf(ctx, "authentication lock acquired")
	defer s.mu.Unlock()
	if reload && s.loadState != nil {
		state, err := s.loadState()
		if err != nil {
			return err
		}
		if state.RefreshToken == "" {
			return ErrSessionExpired
		}
		if err = s.applyState(state); err != nil {
			return err
		}
	}
	if s.token != "" && s.token != rejected {
		s.trace.Printf(ctx, "session already renewed; reusing current access token")
		return nil
	}
	_, _, err := s.get(ctx, s.base.String()+"/anyshare/oauth2/login/refreshToken?isforced=true")
	if err != nil {
		var response *HTTPError
		if errors.As(err, &response) && (response.Status == 400 || response.Status == 401 || response.Status == 403) {
			return fmt.Errorf("%w: %w", ErrSessionExpired, err)
		}
		return err
	}
	if err = s.updateToken(); err != nil {
		return fmt.Errorf("%w: %w", ErrSessionExpired, err)
	}
	if s.token == rejected {
		return ErrSessionExpired
	}
	return s.persistState()
}
