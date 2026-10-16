// Package oidc verifies OAuth 2.0 access tokens issued by the external IdP
// (Keycloak) and turns them into an authz.Principal (ARCHITECTURE §11).
package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"

	"github.com/medbottle/betting-wallet-go/internal/app/authz"
)

// Leeway is the clock tolerance for exp and nbf (ARCHITECTURE §11).
const Leeway = 30 * time.Second

// ErrInvalidToken means the token must be rejected with 401. The wrapped
// cause is for logs only; never log the token itself.
var ErrInvalidToken = errors.New("oidc: invalid token")

// Config identifies the expected issuer and audience.
type Config struct {
	// Issuer is compared with iss by exact equality.
	Issuer string
	// Audience must be contained in aud.
	Audience string
}

// Verifier checks signature (RS256, via KeySet), iss, aud, azp, exp and
// nbf, then builds the Principal from azp, provider_id and
// resource_access.<audience>.roles.
type Verifier struct {
	inner    *gooidc.IDTokenVerifier
	audience string
	now      func() time.Time
}

// NewVerifier builds a verifier on keys. now is injectable for tests.
// Discovery is not used: issuer and JWKS URL are configured separately.
func NewVerifier(cfg Config, keys gooidc.KeySet, now func() time.Time) *Verifier {
	if now == nil {
		now = time.Now
	}
	return &Verifier{
		inner: gooidc.NewVerifier(cfg.Issuer, keys, &gooidc.Config{
			ClientID:             cfg.Audience,
			SupportedSigningAlgs: []string{gooidc.RS256},
			// go-oidc checks exp without leeway and nbf with a fixed 5
			// minutes; both are checked below with the 30s of §11.
			SkipExpiryCheck: true,
		}),
		audience: cfg.Audience,
		now:      now,
	}
}

type claims struct {
	Azp            string                 `json:"azp"`
	ProviderID     string                 `json:"provider_id"`
	Exp            *json.Number           `json:"exp"`
	Nbf            *json.Number           `json:"nbf"`
	ResourceAccess map[string]resourceAcl `json:"resource_access"`
}

type resourceAcl struct {
	Roles []string `json:"roles"`
}

// Verify validates a raw bearer token. Every failure wraps ErrInvalidToken.
func (v *Verifier) Verify(ctx context.Context, raw string) (authz.Principal, error) {
	if raw == "" {
		return authz.Principal{}, fmt.Errorf("%w: missing token", ErrInvalidToken)
	}
	tok, err := v.inner.Verify(ctx, raw)
	if err != nil {
		return authz.Principal{}, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	var c claims
	if err := tok.Claims(&c); err != nil {
		return authz.Principal{}, fmt.Errorf("%w: claims: %w", ErrInvalidToken, err)
	}
	if err := v.checkTimes(c); err != nil {
		return authz.Principal{}, err
	}
	if c.Azp == "" {
		return authz.Principal{}, fmt.Errorf("%w: azp is required", ErrInvalidToken)
	}
	p, err := authz.NewPrincipal(c.Azp, c.ProviderID, c.ResourceAccess[v.audience].Roles)
	if err != nil {
		return authz.Principal{}, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	return p, nil
}

func (v *Verifier) checkTimes(c claims) error {
	now := v.now()
	if c.Exp == nil {
		return fmt.Errorf("%w: exp is required", ErrInvalidToken)
	}
	exp, err := unixSeconds(*c.Exp)
	if err != nil {
		return fmt.Errorf("%w: exp: %w", ErrInvalidToken, err)
	}
	if now.After(exp.Add(Leeway)) {
		return fmt.Errorf("%w: expired at %s", ErrInvalidToken, exp.UTC().Format(time.RFC3339))
	}
	if c.Nbf != nil {
		nbf, err := unixSeconds(*c.Nbf)
		if err != nil {
			return fmt.Errorf("%w: nbf: %w", ErrInvalidToken, err)
		}
		if now.Add(Leeway).Before(nbf) {
			return fmt.Errorf("%w: not valid before %s", ErrInvalidToken, nbf.UTC().Format(time.RFC3339))
		}
	}
	return nil
}

// unixSeconds reads a NumericDate. Fractions are truncated to whole
// seconds by integer parsing of the part before the dot; no floating point
// is involved.
func unixSeconds(n json.Number) (time.Time, error) {
	s := n.String()
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			s = s[:i]
			break
		}
	}
	sec, err := json.Number(s).Int64()
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(sec, 0), nil
}
