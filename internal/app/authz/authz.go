// Package authz holds the authenticated Principal and the permission rules
// of ARCHITECTURE §11. It is pure: transports authenticate, build the
// Principal, then ask these rules before calling a use case.
package authz

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

// Role is a client role of the wallet-api resource
// (resource_access.wallet-api.roles).
type Role string

const (
	// RoleProvider: submit operations, read its own transactions.
	RoleProvider Role = "provider"
	// RoleWalletInternal: wallet operations, ledger, reconciliation, any
	// transaction.
	RoleWalletInternal Role = "wallet-internal"
)

// Errors returned by the rules. The transport maps them to
// FORBIDDEN (403), PROVIDER_MISMATCH (403) and a plain 404 that does not
// reveal whether another provider's resource exists (ARCHITECTURE §11).
var (
	ErrForbidden        = errors.New("authz: forbidden")
	ErrProviderMismatch = errors.New("authz: providerId does not match the authenticated provider")
	ErrNotFound         = errors.New("authz: not found")
	// ErrInvalidPrincipal means a verified token does not describe a usable
	// identity (for example a provider without provider_id).
	ErrInvalidPrincipal = errors.New("authz: invalid principal")
)

// Principal is the authenticated caller. providerID comes only from the
// token's provider_id claim, never from a request body (CHALLENGE §2).
type Principal struct {
	clientID   string
	providerID string
	roles      []Role
}

// NewPrincipal builds a Principal from verified token claims. A provider
// role requires a provider id.
func NewPrincipal(clientID, providerID string, roles []string) (Principal, error) {
	if clientID == "" {
		return Principal{}, fmt.Errorf("%w: azp is required", ErrInvalidPrincipal)
	}
	p := Principal{clientID: clientID, providerID: providerID}
	for _, r := range roles {
		switch Role(r) {
		case RoleProvider, RoleWalletInternal:
			if !slices.Contains(p.roles, Role(r)) {
				p.roles = append(p.roles, Role(r))
			}
		}
	}
	if p.Has(RoleProvider) && providerID == "" {
		return Principal{}, fmt.Errorf("%w: provider role without provider_id claim", ErrInvalidPrincipal)
	}
	return p, nil
}

// ClientID returns the azp claim.
func (p Principal) ClientID() string { return p.clientID }

// ProviderID returns the provider_id claim, if any.
func (p Principal) ProviderID() (string, bool) { return p.providerID, p.providerID != "" }

// Has reports whether p holds role r.
func (p Principal) Has(r Role) bool { return slices.Contains(p.roles, r) }

// Roles returns a copy of the recognised roles.
func (p Principal) Roles() []Role { return slices.Clone(p.roles) }

func (p Principal) valid() bool { return p.clientID != "" }

// CanSubmitTransaction: POST /wagering/transactions. Only providers, and the
// body's providerId must equal the token's.
func (p Principal) CanSubmitTransaction(bodyProviderID string) error {
	if !p.valid() || !p.Has(RoleProvider) {
		return ErrForbidden
	}
	if bodyProviderID != p.providerID {
		return ErrProviderMismatch
	}
	return nil
}

// CanReadProviderTransaction: GET /providers/{providerId}/wagering/
// transactions/{externalId}. A provider may read only under its own id;
// any other id answers 404. The internal service reads any transaction.
func (p Principal) CanReadProviderTransaction(pathProviderID string) error {
	switch {
	case !p.valid():
		return ErrForbidden
	case p.Has(RoleWalletInternal):
		return nil
	case p.Has(RoleProvider) && pathProviderID == p.providerID:
		return nil
	case p.Has(RoleProvider):
		return ErrNotFound
	default:
		return ErrForbidden
	}
}

// CanLookUpTransactions: GET /wagering/transactions/{id}, before the
// lookup. Only a provider or the internal service may look a transaction
// up; anyone else is refused before anything is read, so the answer never
// depends on whether the transaction exists (CHALLENGE §13). The
// transaction found is then checked by CanReadTransaction.
func (p Principal) CanLookUpTransactions() error {
	if !p.valid() || !(p.Has(RoleProvider) || p.Has(RoleWalletInternal)) {
		return ErrForbidden
	}
	return nil
}

// CanReadTransaction: GET /wagering/transactions/{id}, once the transaction
// is loaded. A provider reads only its own (another provider's answers
// 404); the internal service reads any. txProviderID is "" for internal
// operations such as OPENING.
func (p Principal) CanReadTransaction(txProviderID string) error {
	switch {
	case !p.valid():
		return ErrForbidden
	case p.Has(RoleWalletInternal):
		return nil
	case p.Has(RoleProvider) && txProviderID != "" && txProviderID == p.providerID:
		return nil
	case p.Has(RoleProvider):
		return ErrNotFound
	default:
		return ErrForbidden
	}
}

// CanUseWalletOperations: POST /wallets, GET /wallets/*, the ledger and
// reconciliation. Internal service only; providers do not read wallets.
func (p Principal) CanUseWalletOperations() error {
	if !p.valid() || !p.Has(RoleWalletInternal) {
		return ErrForbidden
	}
	return nil
}

type principalKey struct{}

// WithPrincipal returns ctx carrying p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// FromContext returns the Principal set by the authentication middleware.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok && p.valid()
}
