# Notes: choices, gaps and spec problems (Phase 2)

Conservative choices made without an ADR, and what is open. Spec problems are for `api-specs`; nothing here edits it.
Adopted: api-specs `feat/phase2-specs` 1462fb8, go-common `feat/phase2-libs` c6c0fc7, ADR-0014 A2 (sync op event, emit, grant).

## Open points and ADR candidates

- **Internal consumption (O2-1)** stays provisional: stored and listed as a non-sales document, shown on the day totals as its own
  line; no number from the sale series.
- **Grant verification.** go-auth is to own issue and verification (`SubjectHash` included); until then `internal/grant` checks only the
  ES256 signature and key id behind `service.GrantVerifier`. Everything else of A2.6 item 4 (typ, realm, aud = billing, exp, kind,
  no org, approver = the bearer's subject, subject hash from billing's own stored version, single use) is the service's, in
  `PublishTaxRuleSet`, so it does not change when the verifier does. Subject hash and claim names are the shared vectors
  (`internal/grant` test). Open: how the `grant.invalid` family reaches identity's catalogue (new problem types).
- **`series.close` statement.** The control intent's `series_close` record is not part of `sync.Op` (go-common parses mutations only),
  so billing reconciles from `identity.device.drained` (series id and last number) plus `tenancy.series.closed`. A device that closes a
  series without being revoked states its last number only in the op record: ADR needed on whether `ParseOp` exposes control records
  (then `RecordDrain` takes them too). The statement carries no financial year: billing compares it with the newest year's numbers.
- **`setDiscountLimit` before any settings row.** `settings.discount_limit.set` lists only `set` for `billing_settings` (no `insert`), and a
  `set` on a row nobody inserted stays parked in the engine. Devices or the numbering/profile edit create the row. If that is not
  guaranteed at outlet creation, the intent needs `insert` too.

## Spec problems found (api-specs feat/phase2-specs 1462fb8)

Fixed upstream since the first pass and removed here: the inconsistent example worlds, the `next_cursor` lines, the report and register
aggregates, the 48 versus 64 vectors, the OutletScope id, the stale `If-Match`, the old series codes. What is left:

1. `listBills` sends `verification=mismatch` and shows a verified bill. The contract transport drops that one query parameter (the only
   correction left in `internal/contractenv`).
2. `getBillingProjectionDigest`: the example hashes are SHA-256 of the words `foo` and `test`; the digest is computed, so it is checked
   for status, media type and schema only. `signTaxRuleSet`: the example `content_hash` is a made-up value; signing stores the real hash.
   Both are in `contractenv.ExampleUnreachable`.
3. The work package says the billing publish route has `approval_id` and a 409 `approval_not_executable`. The spec says they belong to
   the admin facade only; the billing route takes the approval from the grant and answers `grant.*`. Billing follows the spec.
4. `publishTaxRuleSet` example `published_at` is 13:09:11.120 and the world's other examples are 13:05:11.120. The double and the Postgres
   stack run the `rule-set-signed` world four minutes later (`seed.WorldAt`) to match; and the example states no
   `organisations_acknowledged`, so the publication answer omits it (the queued emits are not acknowledged yet; GET states both).
5. `setOutletTaxProfile` accepts `price_mode`, but `settings.tax_profile.set` lists only `outlet_tax_profile`, and `price_mode` is a
   `billing_settings` field: there is no intent that carries it with this edit. It is validated and not emitted.
6. `getSeriesLastSeen` example uses series 4a6c with register code A7K (consistent now); it needs `tenancy.series.issued` to have arrived.

## Tax engine

- `tax/` is the Go engine seeded from the api-specs reference; all 65 vectors pass (`tax.RunVectors`, also the validation of a rule set).
  TV-065: under `regular`, a tax class with no rule is an error (`unknown_tax_class`), never silently non-GST. The api-specs reference
  now says the same (1462fb8). The recompute then flags the bill as a mismatch with the reason in the exception params and the flagged
  event; the bill is not altered.
- Recompute: round-off uses the outlet's setting (if unknown: total divisible by 100 means on). The packing charge's class comes
  from `billing_settings.packing_tax_class` (default `fnb_standard`). Bill discounts have no reason code in the synced entity; the
  discount report uses a nil reason id.
- Results: `verified`, `mismatch` (high), `profile_mismatch`, `stale_rules`, `manual_entry`; `pending` until the rule set or profile
  arrives, then re-verified. A bill is never altered or rejected.
- Credit notes are recomputed against the stored original bill; if the original has not arrived they stay `pending`.

## Projection and storage

- Parsing is `sync.ParseOp` (hash, body, size, identity, kinds and values verified; errors never carry the body); `service.OpOf`
  converts it. The registry (`internal/gen/entities`, a copy of api-specs `entities/`, refreshed by `make gen`) says which entities
  billing owns; nothing is replicated to billing, so replica rows are not applied and a mutation of another service's entity is ignored.
- Two layers per op, in one transaction with the inbox row. The projection engine records every owned mutation as a candidate
  (`sync_candidates`, `sync_voids`, `sync_pii`, go-common schema) and resolves rows per field policy, order-free; the configuration
  entities (`billing_settings`, `outlet_tax_profile`, `tender_kind`) are written from those resolved rows, so concurrent edits (per
  field group, highest stamp wins), an edit before its insert, and a void converge whatever the arrival order. The trading facts (bills,
  payments, days, credit notes, refunds, shifts) are immutable inserts folded once per op by `(origin, stream, seq)` (`applied_ops`)
  into the read models and aggregates. A row exposed by a void is written under the voided op's outlet stream.
- Personal data beside an op (`bill.customer_name`) goes to `sync_pii` and never into a read model; `sync.pii.erased` deletes it.
- Read models are documents (`docs`: one row per tenant, kind, id; JSONB body) holding the API representation, with the columns lists
  filter and sort on. Reports read daily aggregates kept by the projection, not raw bills. Late documents are included in the
  aggregates (original Z plus revisions equals the projection). Credit notes do not reduce the item, category, staff and hourly reports.
- Multi-day report lists page in memory over the aggregate rows of the range (fine at pilot scale; measure before wide use).
- The business day is keyed by UUIDv5 of outlet and date. A close later than the lowest stamp stays a fact and a `late_document` finding
  (cause `second_close`); it makes no revision yet.
- Late = the op's seq is above the day's cutoff for its origin. Late documents of one origin accumulate and are sealed into one
  immutable revision once quiet for 30 s (`FlushRevisions`; the process sweeps every 10 s). Numbering `Z/<date>/<device>/R<n>`.
- Series: `tenancy.series.issued` registers a series (id, code, device), `tenancy.series.closed` closes it, `identity.device.drained`
  states the last number; `billing.series.reconciled` is published once when the series is closed, stated, and the register holds every
  number up to the statement (a missing tail is an open gap). Outlet events carry `tax_regime` (default `regular` when absent).
- Not built: quarantine notices (`series_quarantined`), allowance detectors (`allowance_exceeded`, `handover_skipped`), cash events
  (applied nowhere, no API), `business_day_revision` echo handling, orders-side detectors, `bill_reprint` rows (owned, not projected).
  The register needs the permission at every outlet (it is per registration, not per outlet).

## Emission (go-common `emit`)

- Interactive edits (`setDiscountLimit`, `setNumberingMode`, `setOutletTaxProfile`) call the `emit.Client` synchronously outside any
  transaction, under their intent (`settings.discount_limit.set`, `settings.numbering.set`, `settings.tax_profile.set`), with key
  `emit.DeriveKey(op|outlet|Idempotency-Key)`: a retry is one op. A gateway that is down or not configured answers 503
  `emit.unavailable` and nothing changes; a refusal is 500 `emit.rejected` (a defect in billing, logged with status and code).
- Rule sets (`tax.rule_set.publish`) and Z revisions (`day.revision.record`) are queued in `emit_queue` in the transaction that publishes
  or seals them (keys: UUIDv5 of tenant and version; of day and revision number) and sent by go-common's `emit.Queue` worker on the
  global pool (`BILLING_GATEWAY_URL`). `rule_set_emissions` remains the audience list with the queue key; "acknowledged" is the queue row
  being `sent`. An organisation created later is queued the version in force or to come (a system emit, no approval).
- Role split: the tenant role inserts queue rows for its own tenant and reads only `tenant_id` and `key` of its own rows (needed by
  `ON CONFLICT`); the global role drains. The gateway is not built yet: tests use an in-process fake (`double.Gateway`) that implements
  `POST /internal/v1/emit` (also as an `http.Handler` for the real client), checks the intent table, replays by key and refuses
  `idempotency.mismatch`.

## Operations

- Tables of the vendor routes are service-global under `billing_global`; tenant tables use `billing_app` and row-level security.
  The platform vendor log and the `tax_rule_published` / `vendor_access.recorded` events share the publication's transaction.
  Directory-class reads (`x-vendor-audit: none`) run plain.
- `PublishLead` is configuration (`BILLING_PUBLISH_LEAD`, default 24 h), never a request field.
- Platform facts (organisations, outlets, series, drain) are small idempotent writes in their own transaction for the event's tenant;
  only the op projection and the erasure share the inbox transaction.
- CI uses the template's caller job plus an integration job on Postgres 17. golangci-lint was not installed in the build
  environment, so only `go vet` ran locally.
