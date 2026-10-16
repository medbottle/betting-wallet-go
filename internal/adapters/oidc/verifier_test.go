package oidc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/medbottle/betting-wallet-go/internal/app"
	"github.com/medbottle/betting-wallet-go/internal/app/authz"
)

const (
	testIssuer   = "http://idp.test/realms/wagering"
	testAudience = "wallet-api"
)

// fakeClock is a settable clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// idp serves a JWKS for the keys it currently publishes and counts fetches.
type idp struct {
	mu      sync.Mutex
	keys    map[string]*rsa.PrivateKey
	extra   []json.RawMessage // extra raw JWKs (e.g. an encryption key)
	fail    atomic.Bool
	fetches atomic.Int32
	srv     *httptest.Server
}

func newIDP(t *testing.T) *idp {
	t.Helper()
	p := &idp{keys: map[string]*rsa.PrivateKey{}}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		p.fetches.Add(1)
		if p.fail.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		set := struct {
			Keys []any `json:"keys"`
		}{}
		for kid, k := range p.keys {
			set.Keys = append(set.Keys, jose.JSONWebKey{Key: &k.PublicKey, KeyID: kid, Algorithm: "RS256", Use: "sig"})
		}
		for _, e := range p.extra {
			set.Keys = append(set.Keys, e)
		}
		_ = json.NewEncoder(w).Encode(set)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *idp) addKey(t *testing.T, kid string) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.keys[kid] = k
	p.mu.Unlock()
}

func (p *idp) sign(t *testing.T, kid string, c map[string]any) string {
	t.Helper()
	p.mu.Lock()
	k := p.keys[kid]
	p.mu.Unlock()
	return signWith(t, jose.SigningKey{Algorithm: jose.RS256, Key: k}, kid, c)
}

func signWith(t *testing.T, key jose.SigningKey, kid string, c map[string]any) string {
	t.Helper()
	opts := (&jose.SignerOptions{}).WithType("JWT")
	if kid != "" {
		opts = opts.WithHeader("kid", kid)
	}
	s, err := jose.NewSigner(key, opts)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	jws, err := s.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jws.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// providerClaims mirrors what the Keycloak realm issues for provider-a.
func providerClaims(now time.Time) map[string]any {
	return map[string]any{
		"iss":             testIssuer,
		"aud":             testAudience,
		"azp":             "provider-a",
		"provider_id":     "provider-a",
		"exp":             now.Add(5 * time.Minute).Unix(),
		"iat":             now.Unix(),
		"resource_access": map[string]any{"wallet-api": map[string]any{"roles": []string{"provider"}}},
	}
}

type fixture struct {
	idp   *idp
	clock *fakeClock
	keys  *KeySet
	v     *Verifier
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	p := newIDP(t)
	p.addKey(t, "k1")
	clock := &fakeClock{t: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)}
	keys := NewKeySet(p.srv.URL, p.srv.Client(), 30*time.Second, clock.Now)
	if err := keys.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return fixture{idp: p, clock: clock, keys: keys, v: NewVerifier(Config{Issuer: testIssuer, Audience: testAudience}, keys, clock.Now)}
}

func TestVerifyValid(t *testing.T) {
	f := newFixture(t)
	p, err := f.v.Verify(context.Background(), f.idp.sign(t, "k1", providerClaims(f.clock.Now())))
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := p.ProviderID(); id != "provider-a" || p.ClientID() != "provider-a" || !p.Has(authz.RoleProvider) {
		t.Errorf("principal = %+v", p)
	}

	internal := providerClaims(f.clock.Now())
	delete(internal, "provider_id")
	internal["azp"] = "wallet-internal"
	internal["aud"] = []string{"account", testAudience} // aud as an array
	internal["resource_access"] = map[string]any{"wallet-api": map[string]any{"roles": []string{"wallet-internal"}}, "account": map[string]any{"roles": []string{"provider"}}}
	p, err = f.v.Verify(context.Background(), f.idp.sign(t, "k1", internal))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Has(authz.RoleWalletInternal) || p.Has(authz.RoleProvider) {
		t.Errorf("roles must come from resource_access.wallet-api only: %v", p.Roles())
	}
}

func TestVerifyRejects(t *testing.T) {
	f := newFixture(t)
	now := f.clock.Now()
	mut := func(change func(c map[string]any)) string {
		c := providerClaims(now)
		change(c)
		return f.idp.sign(t, "k1", c)
	}
	hmac := signWith(t, jose.SigningKey{Algorithm: jose.HS256, Key: []byte("0123456789abcdef0123456789abcdef")}, "k1", providerClaims(now))
	f.idp.mu.Lock()
	k1 := f.idp.keys["k1"]
	f.idp.mu.Unlock()
	rs512 := signWith(t, jose.SigningKey{Algorithm: jose.RS512, Key: k1}, "k1", providerClaims(now))
	tampered := f.idp.sign(t, "k1", providerClaims(now))
	tampered = tampered[:len(tampered)-4] + "AAAA"

	cases := map[string]string{
		"empty":                      "",
		"garbage":                    "not-a-jwt",
		"wrong issuer":               mut(func(c map[string]any) { c["iss"] = "http://evil.test/realms/wagering" }),
		"issuer with trailing slash": mut(func(c map[string]any) { c["iss"] = testIssuer + "/" }),
		"wrong audience":             mut(func(c map[string]any) { c["aud"] = "account" }),
		"no audience":                mut(func(c map[string]any) { delete(c, "aud") }),
		"no azp":                     mut(func(c map[string]any) { delete(c, "azp") }),
		"provider without claim":     mut(func(c map[string]any) { delete(c, "provider_id") }),
		"no exp":                     mut(func(c map[string]any) { delete(c, "exp") }),
		"expired beyond leeway":      mut(func(c map[string]any) { c["exp"] = now.Add(-Leeway - time.Second).Unix() }),
		"nbf beyond leeway":          mut(func(c map[string]any) { c["nbf"] = now.Add(Leeway + 2*time.Second).Unix() }),
		"HS256":                      hmac,
		"RS512":                      rs512,
		"tampered signature":         tampered,
	}
	for name, raw := range cases {
		if _, err := f.v.Verify(context.Background(), raw); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s: err = %v, want ErrInvalidToken", name, err)
		}
	}
}

func TestLeeway(t *testing.T) {
	f := newFixture(t)
	now := f.clock.Now()
	within := providerClaims(now)
	within["exp"] = now.Add(-Leeway + time.Second).Unix()
	within["nbf"] = now.Add(Leeway - time.Second).Unix()
	if _, err := f.v.Verify(context.Background(), f.idp.sign(t, "k1", within)); err != nil {
		t.Fatalf("token inside the 30s leeway rejected: %v", err)
	}
	// The same real token becomes invalid once the clock passes exp+leeway.
	raw := f.idp.sign(t, "k1", providerClaims(now))
	f.clock.Add(5*time.Minute + Leeway + time.Second)
	if _, err := f.v.Verify(context.Background(), raw); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired token accepted: %v", err)
	}
}

func TestKeyRotationAndRateLimit(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if got := f.idp.fetches.Load(); got != 1 {
		t.Fatalf("startup fetches = %d", got)
	}

	// Known kid: served from cache.
	if _, err := f.v.Verify(ctx, f.idp.sign(t, "k1", providerClaims(f.clock.Now()))); err != nil {
		t.Fatal(err)
	}
	if got := f.idp.fetches.Load(); got != 1 {
		t.Fatalf("known kid refetched: %d", got)
	}

	// Rotation after the quiet period: the first unknown kid fetches at once.
	f.clock.Add(31 * time.Second)
	f.idp.addKey(t, "k2")
	if _, err := f.v.Verify(ctx, f.idp.sign(t, "k2", providerClaims(f.clock.Now()))); err != nil {
		t.Fatalf("rotated key rejected: %v", err)
	}
	if got := f.idp.fetches.Load(); got != 2 {
		t.Fatalf("fetches after rotation = %d, want 2", got)
	}

	// Forged kids within the window do not reach the IdP.
	for i := 0; i < 20; i++ {
		forged := signWith(t, jose.SigningKey{Algorithm: jose.RS256, Key: mustKey(t)}, "forged-kid", providerClaims(f.clock.Now()))
		if _, err := f.v.Verify(ctx, forged); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("forged token accepted: %v", err)
		}
	}
	if got := f.idp.fetches.Load(); got != 2 {
		t.Fatalf("forged kids triggered %d fetches inside the window, want none", got-2)
	}

	// After the interval one more fetch is allowed.
	f.clock.Add(30 * time.Second)
	forged := signWith(t, jose.SigningKey{Algorithm: jose.RS256, Key: mustKey(t)}, "forged-kid", providerClaims(f.clock.Now()))
	_, _ = f.v.Verify(ctx, forged)
	if got := f.idp.fetches.Load(); got != 3 {
		t.Fatalf("fetches after the window = %d, want 3", got)
	}
}

func TestConcurrentUnknownKidSharesOneFetch(t *testing.T) {
	f := newFixture(t)
	f.clock.Add(time.Minute)
	f.idp.addKey(t, "k2")
	raw := f.idp.sign(t, "k2", providerClaims(f.clock.Now()))

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.v.Verify(context.Background(), raw); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent verify: %v", err)
	}
	if got := f.idp.fetches.Load(); got != 2 {
		t.Fatalf("fetches = %d, want 1 startup + 1 shared refresh", got)
	}
}

func TestFailedRefreshKeepsCachedKeys(t *testing.T) {
	f := newFixture(t)
	f.clock.Add(time.Minute)
	f.idp.fail.Store(true)
	forged := signWith(t, jose.SigningKey{Algorithm: jose.RS256, Key: mustKey(t)}, "unknown", providerClaims(f.clock.Now()))
	if _, err := f.v.Verify(context.Background(), forged); !errors.Is(err, app.ErrTransient) {
		t.Fatalf("err = %v", err)
	}
	if _, err := f.v.Verify(context.Background(), f.idp.sign(t, "k1", providerClaims(f.clock.Now()))); err != nil {
		t.Fatalf("cached key lost after a failed refresh: %v", err)
	}
}

// TestUnknownKidWhileIdPDown is ARCHITECTURE §24, Q5: an unknown kid that
// cannot be checked because the JWKS fetch fails is transient (503), also
// inside the rate-limit window; once the IdP answers again an unknown kid
// is an invalid token (401). Tokens signed with cached keys keep working.
func TestUnknownKidWhileIdPDown(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.clock.Add(time.Minute)
	f.idp.fail.Store(true)
	forged := func() string {
		return signWith(t, jose.SigningKey{Algorithm: jose.RS256, Key: mustKey(t)}, "unknown", providerClaims(f.clock.Now()))
	}
	for i := 0; i < 3; i++ { // first: a failed fetch; then: rate limited, last fetch failed
		_, err := f.v.Verify(ctx, forged())
		if !errors.Is(err, app.ErrTransient) || !errors.Is(err, ErrKeysUnavailable) || errors.Is(err, ErrInvalidToken) {
			t.Fatalf("attempt %d: err = %v, want transient ErrKeysUnavailable", i, err)
		}
	}
	if got := f.idp.fetches.Load(); got != 2 {
		t.Fatalf("fetches = %d: the rate limit must still apply while the IdP is down", got)
	}
	if _, err := f.v.Verify(ctx, f.idp.sign(t, "k1", providerClaims(f.clock.Now()))); err != nil {
		t.Fatalf("cached key rejected during the outage: %v", err)
	}
	// A bad signature with a cached kid is invalid, outage or not.
	bad := signWith(t, jose.SigningKey{Algorithm: jose.RS256, Key: mustKey(t)}, "k1", providerClaims(f.clock.Now()))
	if _, err := f.v.Verify(ctx, bad); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("forged cached kid: err = %v, want ErrInvalidToken", err)
	}

	f.idp.fail.Store(false)
	f.clock.Add(31 * time.Second)
	if _, err := f.v.Verify(ctx, forged()); !errors.Is(err, ErrInvalidToken) || errors.Is(err, app.ErrTransient) {
		t.Fatalf("after recovery: err = %v, want ErrInvalidToken", err)
	}
}

func TestStartupRefreshFailsFast(t *testing.T) {
	p := newIDP(t)
	p.fail.Store(true)
	if err := NewKeySet(p.srv.URL, p.srv.Client(), 30*time.Second, nil).Refresh(context.Background()); err == nil {
		t.Fatal("startup refresh against a failing IdP must error")
	}
	empty := newIDP(t) // no signing keys published
	if err := NewKeySet(empty.srv.URL, empty.srv.Client(), 30*time.Second, nil).Refresh(context.Background()); err == nil {
		t.Fatal("a JWKS without RS256 signing keys must be refused")
	}
}

func TestEncryptionKeysAreIgnored(t *testing.T) {
	p := newIDP(t)
	p.addKey(t, "sig")
	enc := mustKey(t)
	raw, err := json.Marshal(jose.JSONWebKey{Key: &enc.PublicKey, KeyID: "enc", Algorithm: "RSA-OAEP", Use: "enc"})
	if err != nil {
		t.Fatal(err)
	}
	p.extra = append(p.extra, raw)
	keys := NewKeySet(p.srv.URL, p.srv.Client(), 30*time.Second, nil)
	if err := keys.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := keys.cached(); len(got) != 1 || got[0].KeyID != "sig" {
		t.Fatalf("cached keys = %v, want only the signing key", got)
	}
}

func mustKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}
