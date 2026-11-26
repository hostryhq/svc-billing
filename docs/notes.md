# Notes: choices, gaps and spec problems (Phase 2)

Conservative choices made without an ADR, and what is open. Spec problems are for `api-specs`; nothing here edits it.
Adopted: api-specs `feat/phase2-specs` b153c62, go-common `feat/phase2-libs` 92884d6, go-auth `feat/phase2-auth` 94f1970, ADR-0014 A2 (sync op event, emit, grant).

## Open points and ADR candidates

- **Internal consumption (O2-1)** stays provisional: stored and listed as a non-sales document, shown on the day totals as its own
  line; no number from the sale series.
- **Grant verification** is go-auth's `GrantVerifier` (ES256 under the vendor key set by kid; iss, realm, aud = billing, typ, kind, no org,
  approver = the bearer's subject, requester differs, lifetime at most 300 s, exp, then the subject hash that billing recomputes from its
  own stored version with `auth.SubjectHash`). Single use stays billing's: the verifier's `Used` hook is nil and `GlobalTx.UseGrant` records
  the jti in the publication's transaction. `BILLING_GRANT_ISSUER` (default `https://identity.hostry.invalid/vendor`, the contract
  example's iss) is the expected issuer; `BILLING_GRANT_KEYS` the key set. The double signs real grants under its own key
  (`double.Grants`) and maps the contract's example token to a fresh publish grant. Open: how the `grant.*` family reaches identity's
  catalogue (new problem types).
- **`series.close` statement.** `ParseOp` now exposes the control record (`Op.SeriesClose`: series id, code, financial year, last number).
  `ApplyOp` takes it as the device's statement (first one stands; a statement from a device other than the one the series was issued to is
  ignored) and `billing.series.reconciled` is published once when the series is closed (`tenancy.series.closed`), stated and the register
  holds every number up to it. The statement's financial year picks the register series; a drain statement (`identity.device.drained`)
  has none and is compared with the newest year. A statement alone does not close a series: closing stays tenancy's.
- **`setDiscountLimit` before any settings row** is an insert of the row with defaults (b153c62), the limit as given; a set otherwise.
  The fields the API does not show take assumed defaults: `service_charge_dine_in` true, `_takeaway` and `_counter` false,
  `packing_charge_paise` 0, `packing_tax_class` `fnb_standard`, `day_cutoff_minutes` 300 (the entity states only the last). Confirm.
  Because the op shape now depends on stored state, the two routes whose body depends on it record their answer under the
  Idempotency-Key and replay it without emitting (a retry never reaches the gateway with another body for the same derived key). Open: a
  crash between the emit and that record leaves a retry to rebuild the op; if the row has appeared meanwhile the gateway answers
  `idempotency.mismatch`.

## Spec problems found (api-specs feat/phase2-specs b153c62)

Fixed upstream since the first pass and removed here: the inconsistent example worlds, the `next_cursor` lines, the report and register
aggregates, the 48 versus 64 vectors, the OutletScope id, the stale `If-Match`, the old series codes. What is left:

1. Fixed upstream in b153c62 and removed here: the `listBills` example (the contract transport no longer drops a query parameter), the
   digest example (`digest-day` world, `examples/digests`; billing computes it) and the `price_mode` carrier (`settings.tax_profile.set`).
2. `signTaxRuleSet`: the example `content_hash` is a made-up value; signing stores the real hash. It is in `contractenv.ExampleUnreachable`.
3. The work package says the billing publish route has `approval_id` and a 409 `approval_not_executable`. The spec says they belong to
   the admin facade only; the billing route takes the approval from the grant and answers `grant.*`. Billing follows the spec.
4. `publishTaxRuleSet` example `published_at` is 13:09:11.120 and the world's other examples are 13:05:11.120. The double and the Postgres
   stack run the `rule-set-signed` world four minutes later (`seed.WorldAt`) to match; and the example states no
   `organisations_acknowledged`, so the publication answer omits it (the queued emits are not acknowledged yet; GET states both).
5. `getSeriesLastSeen` example uses series 4a6c with register code A7K (consistent now); it needs `tenancy.series.issued` to have arrived.

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
- Personal data beside an op (`bill.customer_name`) is verified by `ParseOp` against the salted commitment in the sealed body (`pii_c`),
  then kept by the engine in `sync_pii` (value, salt, commitment) and never in a read model. The cloud-origin `sync.pii.erase` op (parsed to
  `Op.PIIErase`) and the `sync.pii.erased` event both record the erasure (`sync_pii_erased`) and empty the values and salts of that op,
  keeping the commitments; an op whose erasure arrived first never stores its data.
- Projection digest (`sha256-canon-v1`): each owned row that belongs to a business day (own `business_date`, or `bill_id` /
  `credit_note_id` parent) is kept as a `digest_row` document holding its canonical text (RFC 8785 of id and the set non-pii fields, from
  the engine's resolved row); a voided or tombstoned row's document is deleted. `getBillingProjectionDigest` hashes them per entity and
  overall as `sync/protocol-v1.md` says, and equals `examples/digests` (unit test). Children whose parent arrives later take the parent's
  day then. Rows through `order_id`, `check_id` or `kot_id` parents belong to orders and are not billing's.
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
