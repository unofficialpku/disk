package gui

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/unofficialpku/disk/internal/auth"
)

func TestAPIErrorCodesAreCanonicalAndKeepSharedErrorsImmutable(t *testing.T) {
	for _, test := range []struct {
		err    error
		code   string
		status int
	}{
		{anyshare.ErrExists, "ERR_EXISTS", http.StatusConflict},
		{os.ErrExist, "ERR_EXISTS", http.StatusConflict},
		{anyshare.ErrPending, "ERR_PENDING", http.StatusConflict},
		{anyshare.ErrPickupCodeRequired, "ERR_PICKUP_CODE_REQUIRED", http.StatusConflict},
		{auth.ErrSessionExpired, "ERR_SESSION_EXPIRED", http.StatusConflict},
		{context.DeadlineExceeded, "ERR_TIMEOUT", http.StatusGatewayTimeout},
		{os.ErrPermission, "ERR_PERMISSION", http.StatusForbidden},
	} {
		w := httptest.NewRecorder()
		writeError(w, fmt.Errorf("operation: %w", test.err))
		if w.Code != test.status || !strings.Contains(w.Body.String(), `"code":"`+test.code+`"`) {
			t.Fatalf("%v: HTTP %d %s", test.err, w.Code, w.Body.String())
		}
	}
	legacy := &apiError{Code: "approval_pending", Message: "waiting for review"}
	if got := describeError(legacy); got.Code != "ERR_PENDING" || got == legacy || legacy.Code != "approval_pending" {
		t.Fatal("normalization changed a potentially shared error", got, legacy)
	}
	if got := describeError(&apiError{Code: "ERR_PICKUP_CODE_REQUIRED", Message: "code required"}); got.Code != "ERR_PICKUP_CODE_REQUIRED" {
		t.Fatal("canonical code was prefixed twice", got)
	}
}

func TestJSONHandlerRejectsMalformedBodiesBeforeActions(t *testing.T) {
	type input struct {
		Name string `json:"name"`
	}
	calls := 0
	handler := jsonHandler(http.StatusAccepted, func(_ *http.Request, body input) (any, error) {
		calls++
		return map[string]string{"name": body.Name}, nil
	})
	for _, test := range []struct{ media, body string }{
		{"text/plain", `{"name":"ok"}`}, {"application/json", `null`},
		{"application/json", `{"name":"ok","unknown":true}`},
		{"application/json", `{"name":"ok"}{"name":"again"}`},
		{"application/json", `[]`}, {"application/json", ""},
		{"application/json", `{"name":"` + strings.Repeat("a", 2<<20) + `"}`},
	} {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(test.body))
		r.Header.Set("Content-Type", test.media)
		w := httptest.NewRecorder()
		handler(w, r)
		if w.Code != http.StatusBadRequest || calls != 0 || !strings.Contains(w.Body.String(), "ERR_INVALID_INPUT") {
			t.Fatalf("malformed body reached action: HTTP %d, calls=%d", w.Code, calls)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"accepted"}`))
	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	w := httptest.NewRecorder()
	handler(w, r)
	if w.Code != http.StatusAccepted || calls != 1 || w.Header().Get("Content-Type") != "application/json" || !strings.Contains(w.Body.String(), "accepted") {
		t.Fatal("valid response lost body, headers or status", w.Code, w.Header(), w.Body.String())
	}
}
