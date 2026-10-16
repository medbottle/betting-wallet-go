//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/medbottle/betting-wallet-go/internal/adapters/httpapi"
	"github.com/medbottle/betting-wallet-go/internal/adapters/oidc"
	"github.com/medbottle/betting-wallet-go/internal/app/authz"
	"github.com/medbottle/betting-wallet-go/internal/platform/health"
)

func newKeycloakVerifier(t *testing.T, kc keycloakEnv, issuer, audience string, now func() time.Time) *oidc.Verifier {
	t.Helper()
	keys := oidc.NewKeySet(kc.JWKSURL, &http.Client{Timeout: 5 * time.Second}, 30*time.Second, nil)
	if err := keys.Refresh(ctxT(t)); err != nil {
		t.Fatalf("initial JWKS fetch from Keycloak: %v", err)
	}
	return oidc.NewVerifier(oidc.Config{Issuer: issuer, Audience: audience}, keys, now)
}

// TestKeycloakTokens uses real client_credentials tokens from the realm
// file (no IdP mock, CHALLENGE §13).
func TestKeycloakTokens(t *testing.T) {
	t.Parallel()
	kc := startKeycloak(t)
	v := newKeycloakVerifier(t, kc, kc.Issuer, "wallet-api", nil)

	t.Run("provider identities carry provider_id from the token", func(t *testing.T) {
		for _, id := range []string{"provider-a", "provider-b"} {
			p, err := v.Verify(ctxT(t), mustToken(t, kc, id))
			if err != nil {
				t.Fatalf("%s: %v", id, err)
			}
			got, ok := p.ProviderID()
			if !ok || got != id || p.ClientID() != id || !p.Has(authz.RoleProvider) || p.Has(authz.RoleWalletInternal) {
				t.Errorf("%s principal = %+v", id, p)
			}
		}
	})

	t.Run("internal identity", func(t *testing.T) {
		p, err := v.Verify(ctxT(t), mustToken(t, kc, "wallet-internal"))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := p.ProviderID(); ok || !p.Has(authz.RoleWalletInternal) || p.Has(authz.RoleProvider) {
			t.Errorf("principal = %+v", p)
		}
		if err := p.CanUseWalletOperations(); err != nil {
			t.Errorf("internal cannot use wallet operations: %v", err)
		}
	})

	t.Run("wrong client secret gets no token", func(t *testing.T) {
		if _, code := clientToken(t, kc, "provider-a", "wrong-secret"); code != http.StatusUnauthorized {
			t.Fatalf("token endpoint answered %d, want 401", code)
		}
	})

	t.Run("rejections", func(t *testing.T) {
		good := mustToken(t, kc, "provider-a")
		tampered := good[:len(good)-6] + "AAAAAA"
		cases := map[string]struct {
			verifier *oidc.Verifier
			token    string
		}{
			"missing token":      {v, ""},
			"tampered signature": {v, tampered},
			"wrong issuer":       {newKeycloakVerifier(t, kc, "http://localhost:8080/realms/other", "wallet-api", nil), good},
			"wrong audience":     {newKeycloakVerifier(t, kc, kc.Issuer, "another-api", nil), good},
			// The same real token, verified by a clock moved past its 5
			// minute lifetime plus the 30s leeway.
			"expired": {newKeycloakVerifier(t, kc, kc.Issuer, "wallet-api", func() time.Time {
				return time.Now().Add(5*time.Minute + oidc.Leeway + time.Minute)
			}), good},
		}
		for name, c := range cases {
			if _, err := c.verifier.Verify(ctxT(t), c.token); !errors.Is(err, oidc.ErrInvalidToken) {
				t.Errorf("%s: err = %v, want ErrInvalidToken", name, err)
			}
		}
	})
}

// TestAuthMiddlewareWithKeycloak puts the real verifier behind the
// middleware: business routes need a valid token, /health stays public.
func TestAuthMiddlewareWithKeycloak(t *testing.T) {
	t.Parallel()
	kc := startKeycloak(t)
	v := newKeycloakVerifier(t, kc, kc.Issuer, "wallet-api", nil)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	readiness := health.NewReadiness(nil, time.Second, time.Second, logger)
	readiness.CheckNow(context.Background())
	mux := http.NewServeMux()
	mux.Handle("/health/", httpapi.NewRouter(readiness, time.Second))
	mux.Handle("GET /protected", httpapi.RequireAuth(v, logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := authz.FromContext(r.Context())
		id, _ := p.ProviderID()
		_ = json.NewEncoder(w).Encode(map[string]string{"azp": p.ClientID(), "providerId": id})
	})))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	call := func(path, token string) (*http.Response, map[string]any) {
		req, _ := http.NewRequestWithContext(ctxT(t), http.MethodGet, srv.URL+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp, body
	}

	if resp, _ := call("/health/live", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("/health/live without token = %d, want 200", resp.StatusCode)
	}
	resp, body := call("/protected", "")
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") != `Bearer error="invalid_token"` {
		t.Errorf("no token: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	if e, _ := body["error"].(map[string]any); e["code"] != "UNAUTHENTICATED" {
		t.Errorf("no token body = %v", body)
	}
	if resp, _ := call("/protected", "not.a.token"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("garbage token = %d", resp.StatusCode)
	}
	resp, body = call("/protected", mustToken(t, kc, "provider-b"))
	if resp.StatusCode != http.StatusOK || body["providerId"] != "provider-b" || body["azp"] != "provider-b" {
		t.Errorf("valid token: %d %v", resp.StatusCode, body)
	}
}
