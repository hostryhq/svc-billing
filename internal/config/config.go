// Package config reads the service configuration from the environment (12-factor) and fails fast with every problem at
// once. Secrets are database credentials and key material by reference only: a DSN or a file path, never a literal.
package config

import (
	"time"

	"github.com/hostryhq/go-common/config"
)

// Config is the validated configuration.
type Config struct {
	Addr string
	// AppDSN connects as billing_app, GlobalDSN as billing_global, RelayDSN as billing_relay (outbox only).
	AppDSN, GlobalDSN, RelayDSN string
	// MigrateDSN connects as the migrator; used only by `billing migrate`.
	MigrateDSN string
	// Key sets: JWKS documents (URL or file path) of the tenant and vendor token issuers, the service key registry
	// (a JWKS file whose kid is the service name) and the keys that sign vendor approval grants.
	TenantJWKS, VendorJWKS, ServiceKeys, GrantKeys string
	// SelfName is this service's name in service tokens (aud).
	SelfName string
	// Issuer and audience names of the two token realms (ADR-0005).
	TenantIssuer, TenantAudience, VendorIssuer, VendorAudience string
	// OrdersURL is orders' base URL for the open-check count (empty: the numbering-mode route answers 503).
	OrdersURL string
	// GatewayURL is the sync gateway base URL for POST /internal/v1/emit (empty: the settings routes answer 503
	// emit.unavailable and rule-set and revision emissions stay queued).
	GatewayURL string
	// ServiceKeyFile is the PEM file of this service's private signing key (service tokens to orders).
	ServiceKeyFile string
	// NatsURL and NatsSeedFile connect the outbox relay and the consumer to the bus; empty disables both.
	NatsURL, NatsSeedFile string
	// PublishLead is the lead time between publishing a rule set and its effective date (deployment configuration).
	PublishLead time.Duration
	KeyRefresh  time.Duration
	LogLevel    string
}

// Load reads the environment through lookup.
func Load(lookup config.Lookup) (Config, error) {
	l := config.New(lookup)
	c := Config{
		Addr:           l.String("BILLING_ADDR", ":8080"),
		AppDSN:         l.Required("BILLING_DB_APP_DSN"),
		GlobalDSN:      l.Required("BILLING_DB_GLOBAL_DSN"),
		RelayDSN:       l.String("BILLING_DB_RELAY_DSN", ""),
		MigrateDSN:     l.String("BILLING_DB_MIGRATE_DSN", ""),
		TenantJWKS:     l.Required("BILLING_TENANT_JWKS"),
		VendorJWKS:     l.Required("BILLING_VENDOR_JWKS"),
		ServiceKeys:    l.Required("BILLING_SERVICE_KEYS"),
		GrantKeys:      l.Required("BILLING_GRANT_KEYS"),
		SelfName:       l.String("BILLING_SELF_NAME", "billing"),
		TenantIssuer:   l.String("BILLING_TENANT_ISSUER", "hostry-identity"),
		TenantAudience: l.String("BILLING_TENANT_AUDIENCE", "hostry-tenant"),
		VendorIssuer:   l.String("BILLING_VENDOR_ISSUER", "hostry-identity-vendor"),
		VendorAudience: l.String("BILLING_VENDOR_AUDIENCE", "hostry-vendor"),
		OrdersURL:      l.String("BILLING_ORDERS_URL", ""),
		GatewayURL:     l.String("BILLING_GATEWAY_URL", ""),
		ServiceKeyFile: l.String("BILLING_SERVICE_KEY_FILE", ""),
		NatsURL:        l.String("NATS_URL", ""),
		NatsSeedFile:   l.String("NATS_SEED_FILE", ""),
		PublishLead:    l.Duration("BILLING_PUBLISH_LEAD", 24*time.Hour),
		KeyRefresh:     l.Duration("BILLING_KEY_REFRESH", time.Minute),
		LogLevel:       l.String("LOG_LEVEL", "info"),
	}
	return c, l.Err()
}
