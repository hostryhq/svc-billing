# svc-billing

Billing service: the cloud side of numbered bills, credit notes, payments, refunds, shifts and the business day; the tax
recompute, the document register, the tax rule master and the sales, tax and tender reports (ADR-0014, Amendment A1).

Documents are created on devices as sync ops (`bill.finalize` takes the number on the device). billing projects the ops it receives
from the bus, recomputes every bill and credit note with the shared tax engine and **flags** a difference; it never alters, rejects,
allocates or reuses a bill or a number.

## Layout
```
cmd/billing        wiring only (serve, migrate)
api/               controller: generated routes behind go-auth, strict JSON, idempotency; no business rule
service/           business rules: projection, recompute, register, business day, reports, rule-set master
repository/        interfaces; mem/ (double) and pg/ (Postgres, row-level security), migrations/
tax/               the Go tax engine, rule-set resolution and validation, the 64 shared vectors (embedded copy)
double/            the test double of the public contract, with failure injection (import it in other services' tests)
internal/gen       generated from api-specs (make gen); internal/seed example worlds; internal/contractenv shared suite
docs/              operations-status.md (every operationId), notes.md (choices, gaps, spec problems)
```

## Run
Config is environment only (`internal/config`): `BILLING_DB_APP_DSN`, `BILLING_DB_GLOBAL_DSN`, optional `BILLING_DB_RELAY_DSN`,
`BILLING_TENANT_JWKS`, `BILLING_VENDOR_JWKS`, `BILLING_SERVICE_KEYS`, `BILLING_GRANT_KEYS` (URLs or file paths, never values),
`NATS_URL` and `NATS_SEED_FILE` for the bus, `BILLING_PUBLISH_LEAD` (default 24h), `BILLING_ORDERS_URL`. Roles are created by
`scripts/provision-roles.sql` (deployment owns it); `billing migrate` applies the schema as the migrator role.

## Test
```
make test         # unit, the 64 tax vectors, and the contract suite against the double (no database)
make dev-up       # Postgres from ../infra/dev, compose project "billing", port 55446
make integration  # real service on Postgres: shared contract suite, bus projection, row-level security, roles
make gen          # regenerate internal/gen, seed examples and tax vectors from ../api-specs (records the commit)
```
For local work against sibling checkouts use an uncommitted `go.work` (go-common, go-auth, api-specs).
