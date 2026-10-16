package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/medbottle/betting-wallet-go/internal/app/authz"
)

type stubVerifier struct {
	token string
	p     authz.Principal
}

func (s stubVerifier) Verify(_ context.Context, raw string) (authz.Principal, error) {
	if raw != s.token {
		return authz.Principal{}, errors.New("bad token")
	}
	return s.p, nil
}

func TestRequireAuth(t *testing.T) {
	p, err := authz.NewPrincipal("provider-a", "provider-a", []string{"provider"})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	const secretToken = "good.token.value"
	reached := false
	h := RequireAuth(stubVerifier{token: secretToken, p: p}, logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := authz.FromContext(r.Context())
		if !ok || got.ClientID() != "provider-a" {
			t.Errorf("principal missing in context: %+v", got)
		}
		reached = true
		w.WriteHeader(http.StatusNoContent)
	}))

	rejects := map[string]string{
		"missing header":   "",
		"wrong scheme":     "Basic dXNlcjpwYXNz",
		"empty token":      "Bearer ",
		"no space":         "Bearer" + secretToken,
		"invalid token":    "Bearer forged.token.value",
		"token, no scheme": secretToken,
	}
	for name, header := range rejects {
		reached = false
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/wagering/transactions", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized || reached {
			t.Errorf("%s: code %d, handler reached %v", name, rec.Code, reached)
		}
		if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer error="invalid_token"` {
			t.Errorf("%s: WWW-Authenticate = %q", name, got)
		}
		var body errorBody
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Code != "UNAUTHENTICATED" || body.Error.Retryable {
			t.Errorf("%s: body = %s", name, rec.Body)
		}
	}

	for _, scheme := range []string{"Bearer", "bearer", "BEARER"} {
		reached = false
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("Authorization", scheme+" "+secretToken)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent || !reached {
			t.Errorf("scheme %q: code %d", scheme, rec.Code)
		}
	}

	if out, _ := io.ReadAll(&logs); strings.Contains(string(out), secretToken) || strings.Contains(string(out), "forged.token.value") {
		t.Fatal("a token was written to the logs")
	}
}
