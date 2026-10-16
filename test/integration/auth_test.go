//go:build integration

package integration

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

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
	mux.Handle("/health/", httpapi.NewRouter(httpapi.Deps{Readiness: readiness, HandlerTimeout: time.Second}))
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

// TestIdPOutageUnknownKid is ARCHITECTURE §24, Q5 against the real
// Keycloak: while the JWKS endpoint is unreachable, a token with an
// unknown kid (a rotated key) cannot be checked and the middleware answers
// 503 retryable; tokens signed with the cached key keep working. Once the
// endpoint is back, the same unknown kid is a plain 401.
func TestIdPOutageUnknownKid(t *testing.T) {
	t.Parallel()
	kc := startKeycloak(t)
	var down atomic.Bool
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "idp down", http.StatusBadGateway)
			return
		}
		resp, err := http.Get(kc.JWKSURL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer jwks.Close()
	keys := oidc.NewKeySet(jwks.URL, &http.Client{Timeout: 5 * time.Second}, time.Second, nil)
	if err := keys.Refresh(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	v := oidc.NewVerifier(oidc.Config{Issuer: kc.Issuer, Audience: "wallet-api"}, keys, nil)
	srv := httptest.NewServer(httpapi.RequireAuth(v, slog.New(slog.NewJSONHandler(io.Discard, nil)))(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })))
	defer srv.Close()
	status := func(token string) (int, string) {
		req, _ := http.NewRequestWithContext(ctxT(t), http.MethodGet, srv.URL, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body struct {
			Error struct {
				Code      string `json:"code"`
				Retryable bool   `json:"retryable"`
			} `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body.Error.Code
	}

	real := mustToken(t, kc, "provider-a")
	rotated := resignWithUnknownKid(t, real)

	down.Store(true)
	// Past the refresh interval since the startup fetch, so the unknown kid
	// triggers a fetch (which fails) instead of being rate limited.
	time.Sleep(1100 * time.Millisecond)
	if code, ec := status(rotated); code != http.StatusServiceUnavailable || ec != "TEMPORARILY_UNAVAILABLE" {
		t.Fatalf("unknown kid during outage: %d %s, want 503 TEMPORARILY_UNAVAILABLE", code, ec)
	}
	if code, _ := status(real); code != http.StatusNoContent {
		t.Fatalf("cached key during outage: %d, want 204", code)
	}
	down.Store(false)
	time.Sleep(1100 * time.Millisecond) // past the 1s refresh interval of this key set
	if code, ec := status(rotated); code != http.StatusUnauthorized || ec != "UNAUTHENTICATED" {
		t.Fatalf("unknown kid after recovery: %d %s, want 401", code, ec)
	}
}

// resignWithUnknownKid keeps a real token's claims and signs them with a
// fresh key under a kid Keycloak never published.
func resignWithUnknownKid(t *testing.T, token string) string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatal("not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "rotated-key"))
	if err != nil {
		t.Fatal(err)
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jws.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
