package gui

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"time"

	"github.com/unofficialpku/disk/internal/accounts"
	"github.com/unofficialpku/disk/internal/auth"
)

type loginInfo struct {
	ID        string          `json:"id"`
	Account   string          `json:"account"`
	State     string          `json:"state"`
	Challenge *loginChallenge `json:"challenge,omitempty"`
	Error     *apiError       `json:"error,omitempty"`
}

type loginChallenge struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Image string `json:"image,omitempty"`
}

type loginAttempt struct {
	info     loginInfo
	owner    [32]byte
	answer   chan string
	cancel   context.CancelFunc
	finished bool
	created  time.Time
}

func (a *Application) startLogin(w http.ResponseWriter, r *http.Request) {
	var input LoginInput
	if !decodeJSON(w, r, &input) {
		return
	}
	if input.Account == "" {
		if input.Name == "" {
			input.Name = input.Username
		}
		if !accounts.ValidName(input.Name) || input.Username == "" || len(input.Password) == 0 || len(input.Password) > 8192 {
			writeError(w, invalid("name, username and password are required"))
			return
		}
	} else if input.Username != "" || input.Password != "" || input.Server != "" {
		writeError(w, invalid("choose a saved account or new credentials"))
		return
	}
	name := input.Account
	if name == "" {
		name = input.Name
	}
	id, err := randomToken()
	if err != nil {
		writeError(w, err)
		return
	}
	a.mu.Lock()
	for key, previous := range a.logins {
		if previous.finished && time.Since(previous.created) > 10*time.Minute {
			delete(a.logins, key)
		}
	}
	if a.closing || a.busyLocked(name) || len(a.logins) >= 32 {
		a.mu.Unlock()
		writeError(w, ErrBusy)
		return
	}
	ctx, cancel := context.WithTimeout(a.ctx, 5*time.Minute)
	attempt := &loginAttempt{info: loginInfo{ID: id, Account: name, State: "connecting"},
		owner: sha256.Sum256([]byte(bearer(r))), cancel: cancel, created: time.Now()}
	a.logins[id] = attempt
	a.workers.Add(1)
	info := attempt.info
	a.mu.Unlock()
	go func() {
		defer a.workers.Done()
		defer cancel()
		c, err := a.opts.Connect(ctx, input, func(ctx context.Context, challenge auth.Challenge) (string, error) {
			challengeID, err := randomToken()
			if err != nil {
				return "", err
			}
			view := &loginChallenge{ID: challengeID, Kind: challenge.Kind}
			if len(challenge.Image) > 0 {
				view.Image = "data:image/png;base64," + base64.StdEncoding.EncodeToString(challenge.Image)
			}
			answer := make(chan string, 1)
			a.mu.Lock()
			attempt.answer, attempt.info.Challenge, attempt.info.State = answer, view, "challenge"
			a.mu.Unlock()
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case value := <-answer:
				return value, nil
			}
		})
		input.Password = ""
		if err == nil && ctx.Err() != nil {
			c.close()
			err = ctx.Err()
		}
		if err == nil {
			err = a.install(c)
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		attempt.finished, attempt.answer, attempt.info.Challenge = true, nil, nil
		if err == nil {
			attempt.info.State = "completed"
		} else {
			attempt.info.State, attempt.info.Error = "failed", describeError(err)
			if errors.Is(err, context.Canceled) {
				attempt.info.State = "cancelled"
			}
		}
	}()
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, info)
}

func (a *Application) loginRequest(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Challenge string `json:"challenge"`
		Answer    string `json:"answer"`
	}
	if r.Method == http.MethodPost && !decodeJSON(w, r, &input) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	attempt := a.logins[r.PathValue("id")]
	if attempt == nil || attempt.owner != sha256.Sum256([]byte(bearer(r))) {
		writeError(w, accounts.ErrNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, attempt.info)
	case http.MethodDelete:
		attempt.cancel()
		w.WriteHeader(http.StatusNoContent)
	case http.MethodPost:
		if attempt.finished || attempt.answer == nil || attempt.info.Challenge == nil ||
			input.Challenge != attempt.info.Challenge.ID || input.Answer == "" || len(input.Answer) > 1024 {
			writeError(w, invalid("verification challenge expired or answer is empty"))
			return
		}
		attempt.answer <- input.Answer
		attempt.answer, attempt.info.Challenge, attempt.info.State = nil, nil, "connecting"
		w.WriteHeader(http.StatusNoContent)
	}
}
