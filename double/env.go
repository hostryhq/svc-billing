package double

import (
	"net/http"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-auth/authtest"
	"github.com/hostryhq/go-common/clock"
	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-billing/internal/seed"
)

// Tokens are the test issuers a service under test trusts, with helpers that mint the credentials each kind of
// request needs. For tests only: tokens are signed with throw-away keys.
type Tokens struct {
	TenantIssuer *authtest.Issuer
	VendorIssuer *authtest.Issuer
	Registry     *authtest.ServiceRegistry
}

// NewTokens creates issuers and a service registry containing the platform services and the given extras.
func NewTokens(extraServices ...string) *Tokens {
	clk := clock.System
	return &Tokens{TenantIssuer: authtest.NewTenantIssuer(clk), VendorIssuer: authtest.NewVendorIssuer(clk),
		Registry: authtest.NewServiceRegistry(clk, append([]string{"identity", "admin", "tenancy", "billing", "sync", "orders"}, extraServices...)...)}
}

// Env is a double together with the tokens its verifiers trust.
type Env struct {
	*Double
	*Tokens
	Grants *Grants
}

// NewEnv builds a double that trusts fresh test issuers.
func NewEnv(extraServices ...string) (*Env, error) {
	tk := NewTokens(extraServices...)
	g := &Grants{}
	d, err := New(Config{Tenant: tk.TenantIssuer.Verifier(), Vendor: tk.VendorIssuer.Verifier(), Services: tk.Registry.Verifier("billing"), Grants: g})
	if err != nil {
		return nil, err
	}
	return &Env{Double: d, Tokens: tk, Grants: g}, nil
}

// TenantToken mints a strong user token for tenant with the given grants (an owner without grants).
func (e *Tokens) TenantToken(tenant ids.ID, grants ...auth.Grant) string {
	if len(grants) == 0 {
		grants = authtest.OwnerGrants()
	}
	return e.TenantIssuer.Mint(authtest.Spec{Type: auth.TypeUser, Tenant: tenant, Subject: "member-1", Grants: grants})
}

// PinToken mints a shared-device PIN session token, which can never exercise strong-only permissions.
func (e *Tokens) PinToken(tenant ids.ID, grants ...auth.Grant) string {
	if len(grants) == 0 {
		grants = authtest.OwnerGrants()
	}
	return e.TenantIssuer.Mint(authtest.Spec{Type: auth.TypePin, Tenant: tenant, Grants: grants})
}

// VendorHeaders returns the two credentials a vendor operation needs: the vendor user's token with the given admin
// permissions and the admin service token.
func (e *Tokens) VendorHeaders(perms ...string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+e.VendorIssuer.Mint(authtest.Spec{Type: auth.TypeUser, Subject: seed.ExampleApprover, Grants: authtest.Grants(nil, perms...)}))
	tok, err := e.Registry.Signer("admin").Sign("billing", "")
	if err != nil {
		panic(err)
	}
	h.Set(auth.ServiceTokenHeader, tok)
	return h
}

// ServiceHeaders returns the service token a service (tenancy, identity) sends, naming tenant.
func (e *Tokens) ServiceHeaders(service string, tenant ids.ID) http.Header {
	h := http.Header{}
	tok, err := e.Registry.Signer(service).Sign("billing", tenant.String())
	if err != nil {
		panic(err)
	}
	h.Set(auth.ServiceTokenHeader, tok)
	return h
}
