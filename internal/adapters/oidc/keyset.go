package oidc

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// maxJWKSBytes bounds the JWKS response we are willing to read.
const maxJWKSBytes = 1 << 20

// ErrUnknownKey means the token's kid is not in the cached key set and a
// refresh is not allowed yet (rate limit) or did not provide it.
var ErrUnknownKey = errors.New("oidc: unknown signing key")

// ErrKeysUnavailable means the token's kid is not cached and the JWKS could
// not be fetched (the IdP is down): the token may be valid, so the request
// is retryable (503), not rejected (ARCHITECTURE §24, Q5).
var ErrKeysUnavailable = errors.New("oidc: signing keys unavailable")

// KeySet caches the IdP's RS256 signing keys and refetches the JWKS when a
// token carries an unknown kid, at most once per minInterval
// (ARCHITECTURE §11/§19). The first unknown kid after a quiet period
// fetches immediately; concurrent requests share one fetch; a failed fetch
// keeps the cached keys.
type KeySet struct {
	url         string
	client      *http.Client
	minInterval time.Duration
	now         func() time.Time

	mu        sync.RWMutex
	keys      []jose.JSONWebKey
	lastFetch time.Time // last attempt, successful or not
	lastErr   error     // outcome of the last attempt

	fetchMu sync.Mutex // one fetch at a time
}

// NewKeySet returns an empty key set; call Refresh before serving.
func NewKeySet(jwksURL string, client *http.Client, minInterval time.Duration, now func() time.Time) *KeySet {
	if client == nil {
		client = http.DefaultClient
	}
	if now == nil {
		now = time.Now
	}
	return &KeySet{url: jwksURL, client: client, minInterval: minInterval, now: now}
}

// Refresh fetches the JWKS unconditionally. It is the startup fetch that
// must succeed (fail fast, ARCHITECTURE §11).
func (k *KeySet) Refresh(ctx context.Context) error {
	k.fetchMu.Lock()
	defer k.fetchMu.Unlock()
	return k.fetch(ctx)
}

// VerifySignature implements go-oidc's KeySet. It accepts RS256 only.
func (k *KeySet) VerifySignature(ctx context.Context, jwt string) ([]byte, error) {
	jws, err := jose.ParseSigned(jwt, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		return nil, fmt.Errorf("oidc: parse token: %w", err)
	}
	if len(jws.Signatures) != 1 {
		return nil, errors.New("oidc: exactly one signature is required")
	}
	kid := jws.Signatures[0].Header.KeyID

	if payload, found, err := verifyWith(jws, kid, k.cached()); found {
		return payload, err
	}
	// Unknown kid: refresh if the rate limit allows, then retry once.
	if err := k.refreshForUnknownKid(ctx); err != nil {
		return nil, err
	}
	if payload, found, err := verifyWith(jws, kid, k.cached()); found {
		return payload, err
	}
	return nil, ErrUnknownKey
}

// verifyWith checks the signature against the key with kid (any key when
// the token has no kid). found reports whether a candidate key existed.
func verifyWith(jws *jose.JSONWebSignature, kid string, keys []jose.JSONWebKey) (payload []byte, found bool, err error) {
	for _, key := range keys {
		if kid != "" && key.KeyID != kid {
			continue
		}
		found = true
		if payload, err := jws.Verify(&key); err == nil {
			return payload, true, nil
		}
	}
	if found {
		return nil, true, errors.New("oidc: invalid token signature")
	}
	return nil, false, nil
}

func (k *KeySet) refreshForUnknownKid(ctx context.Context) error {
	started := k.now()
	k.fetchMu.Lock()
	defer k.fetchMu.Unlock()

	k.mu.RLock()
	last, lastErr := k.lastFetch, k.lastErr
	k.mu.RUnlock()
	if !last.Before(started) {
		// Another request fetched while we waited; use its result.
		return unavailable(lastErr)
	}
	if !last.IsZero() && k.now().Sub(last) < k.minInterval {
		// Rate limited: if the last attempt failed the IdP is still
		// considered down; otherwise the kid is simply unknown.
		if lastErr != nil {
			return unavailable(lastErr)
		}
		return ErrUnknownKey
	}
	return unavailable(k.fetch(ctx))
}

func unavailable(fetchErr error) error {
	if fetchErr == nil {
		return nil
	}
	return fmt.Errorf("%w: refresh failed: %w", ErrKeysUnavailable, fetchErr)
}

// fetch downloads the JWKS and records the outcome. Callers hold fetchMu.
func (k *KeySet) fetch(ctx context.Context) error {
	k.mu.Lock()
	k.lastFetch = k.now()
	k.mu.Unlock()
	keys, err := k.download(ctx)
	k.mu.Lock()
	defer k.mu.Unlock()
	k.lastErr = err
	if err == nil {
		k.keys = keys
	}
	return err
}

func (k *KeySet) download(ctx context.Context) ([]jose.JSONWebKey, error) {

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.url, nil)
	if err != nil {
		return nil, fmt.Errorf("oidc: jwks request: %w", err)
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidc: fetch jwks: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oidc: fetch jwks: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBytes))
	if err != nil {
		return nil, fmt.Errorf("oidc: read jwks: %w", err)
	}
	return parseSigningKeys(body)
}

// parseSigningKeys keeps the RSA signature keys (use "sig" or unset).
// Keycloak also publishes an RSA-OAEP encryption key, which is skipped.
func parseSigningKeys(body []byte) ([]jose.JSONWebKey, error) {
	var raw struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("oidc: parse jwks: %w", err)
	}
	var keys []jose.JSONWebKey
	for _, r := range raw.Keys {
		var meta struct {
			Use string `json:"use"`
			Alg string `json:"alg"`
		}
		if err := json.Unmarshal(r, &meta); err != nil {
			return nil, fmt.Errorf("oidc: parse jwk: %w", err)
		}
		if (meta.Use != "" && meta.Use != "sig") || (meta.Alg != "" && meta.Alg != string(jose.RS256)) {
			continue
		}
		var key jose.JSONWebKey
		if err := key.UnmarshalJSON(r); err != nil {
			continue
		}
		if _, ok := key.Key.(*rsa.PublicKey); ok {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return nil, errors.New("oidc: jwks has no RS256 signing key")
	}
	return keys, nil
}

// CloseIdleConnections releases the HTTP client's idle connections, so a
// stopped application leaves no goroutines behind.
func (k *KeySet) CloseIdleConnections() { k.client.CloseIdleConnections() }

func (k *KeySet) cached() []jose.JSONWebKey {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.keys
}
