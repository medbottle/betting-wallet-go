package authz

import (
	"context"
	"errors"
	"testing"
)

func mustPrincipal(t *testing.T, client, provider string, roles ...string) Principal {
	t.Helper()
	p, err := NewPrincipal(client, provider, roles)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNewPrincipal(t *testing.T) {
	p := mustPrincipal(t, "provider-a", "provider-a", "provider", "offline_access", "provider")
	if !p.Has(RoleProvider) || p.Has(RoleWalletInternal) || len(p.Roles()) != 1 {
		t.Errorf("roles = %v", p.Roles())
	}
	if id, ok := p.ProviderID(); !ok || id != "provider-a" || p.ClientID() != "provider-a" {
		t.Errorf("principal = %+v", p)
	}
	if _, err := NewPrincipal("", "provider-a", []string{"provider"}); !errors.Is(err, ErrInvalidPrincipal) {
		t.Errorf("missing azp: %v", err)
	}
	if _, err := NewPrincipal("provider-a", "", []string{"provider"}); !errors.Is(err, ErrInvalidPrincipal) {
		t.Errorf("provider without provider_id: %v", err)
	}
	internal := mustPrincipal(t, "wallet-internal", "", "wallet-internal")
	if _, ok := internal.ProviderID(); ok {
		t.Error("internal principal has a provider id")
	}
}

func TestPermissions(t *testing.T) {
	provA := mustPrincipal(t, "provider-a", "provider-a", "provider")
	internal := mustPrincipal(t, "wallet-internal", "", "wallet-internal")
	noRoles := mustPrincipal(t, "some-client", "")
	var zero Principal

	tests := []struct {
		name string
		got  error
		want error
	}{
		// POST /wagering/transactions
		{"provider submits as itself", provA.CanSubmitTransaction("provider-a"), nil},
		{"provider submits as another", provA.CanSubmitTransaction("provider-b"), ErrProviderMismatch},
		{"provider submits without providerId", provA.CanSubmitTransaction(""), ErrProviderMismatch},
		{"internal cannot submit", internal.CanSubmitTransaction("provider-a"), ErrForbidden},
		{"no role cannot submit", noRoles.CanSubmitTransaction(""), ErrForbidden},
		{"zero principal cannot submit", zero.CanSubmitTransaction(""), ErrForbidden},

		// GET /providers/{id}/wagering/transactions/{ext}
		{"provider reads own path", provA.CanReadProviderTransaction("provider-a"), nil},
		{"provider reads other path", provA.CanReadProviderTransaction("provider-b"), ErrNotFound},
		{"internal reads any provider path", internal.CanReadProviderTransaction("provider-b"), nil},
		{"no role reads provider path", noRoles.CanReadProviderTransaction("provider-a"), ErrForbidden},

		// GET /wagering/transactions/{id}
		{"provider reads own transaction", provA.CanReadTransaction("provider-a"), nil},
		{"provider reads other's transaction", provA.CanReadTransaction("provider-b"), ErrNotFound},
		{"provider reads internal opening", provA.CanReadTransaction(""), ErrNotFound},
		{"internal reads any transaction", internal.CanReadTransaction("provider-b"), nil},
		{"internal reads opening", internal.CanReadTransaction(""), nil},
		{"zero principal reads", zero.CanReadTransaction("provider-a"), ErrForbidden},

		// Wallet operations
		{"internal uses wallets", internal.CanUseWalletOperations(), nil},
		{"provider cannot read wallets", provA.CanUseWalletOperations(), ErrForbidden},
		{"no role cannot use wallets", noRoles.CanUseWalletOperations(), ErrForbidden},
	}
	for _, tt := range tests {
		if !errors.Is(tt.got, tt.want) || (tt.want == nil && tt.got != nil) {
			t.Errorf("%s: got %v, want %v", tt.name, tt.got, tt.want)
		}
	}
}

func TestContext(t *testing.T) {
	if _, ok := FromContext(context.Background()); ok {
		t.Error("empty context has a principal")
	}
	p := mustPrincipal(t, "provider-a", "provider-a", "provider")
	got, ok := FromContext(WithPrincipal(context.Background(), p))
	if !ok || got.ClientID() != "provider-a" {
		t.Errorf("round trip = %+v, %v", got, ok)
	}
	if _, ok := FromContext(WithPrincipal(context.Background(), Principal{})); ok {
		t.Error("zero principal must not count as authenticated")
	}
}
