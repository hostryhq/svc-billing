# Notes: choices, gaps and spec problems (Phase 2)

Conservative choices made without an ADR, and what is open. Spec problems are for `api-specs`; nothing here edits it.

## ADR needed

- **Sync op event payload for domain projections.** The gateway's PoC event `sync.op.stored` carries only
  `{origin, stream, seq, h, intent, status}`; billing must apply mutations, so it reads `muts`, `hlc`, `at`, `actor` and
  `doc` from the event data of `sync.op.<namespace>` (`service.ParseOp`). Either the event carries the op body or a read API
  returns it by identity. Billing applies by entity (`muts[].e`), not by intent name, so it needs no intent table.
- **The gateway `emit` API for domain services.** Settings changes, Z revisions and rule-set rows are cloud-origin ops. There is
  no wire contract for `emit` (only a Go method in svc-sync). billing depends on the `service.Emitter` port; the process wires
  none yet, so the three settings routes answer 503 `emit.unavailable` and rule-set emissions stay queued (counts show 0
  acknowledged). Z revisions are sealed and stored, and marked `pending` in `K2`; their emission is not delivered yet.
- **Vendor approval grant wire format.** `internal/grant` assumes a compact ES256 JWS with claims `jti, typ=vendor_approval,
  approval_id, kind, requester, approver, subject_hash, exp`. Subject hash for `tax_rule_set_publish` is
  `sha256("tax_rule_set_publish\n" + id + "\n" + content_hash + "\n" + effective_date)` in hex. Identity and go-auth should fix both.
- **Internal consumption (O2-1)** stays provisional: stored and listed as a non-sales document, shown on the day totals as its own
  line; no number from the sale series.

## Spec problems found (api-specs feat/phase2-specs)

1. `info.x-example-worlds` base says "no exception, no gap", but `listDocumentGaps` (base) shows an open gap, the day has
   revision_count 0 while `listBusinessDayRevisions` (base) shows R1, and the register shows count 123 with gap_count 0 next to a
   gap. The double stores each example as its own stored fact.
2. Every list example carries `next_cursor` although its world has one row. No service can reproduce it; the contract env serves a
   copy of the spec without those example lines.
3. Report, digest and register examples are not derivable from the world's single bill (12 bills, quantity 24, a digest of the words
   `foo` and `test`); the aggregates are stored rows, seeded from the examples. The digest example is not reproducible at all.
4. `validateTaxRuleSet` example says 48 vectors; the shared file has 64. `signTaxRuleSet` example omits `content_hash` that signing
   stores; `publishTaxRuleSet` shows `published_at` 13:09:40 and `organisations_total` without an acknowledged count, while every
   other example holds the 13:05:11.120 clock. These four are checked for status, media type and schema only.
5. `OutletScope` example id (4a5c) is not the world's outlet (4a61); `listBills` sends `verification=mismatch` while its answer is a
   verified bill; `updateTaxRuleSetDraft` sends If-Match "7" for a draft at revision 1. The contract transport corrects these three.
6. Cursor: lists say "newest first" but the shared cursor example is below every id, so an id-descending "after" would return
   nothing. A cursor naming a row that does not exist restarts from the top.
7. The spec has **39** operations, not 44 as the work package says (8 vendor tax-rule-set routes plus `getSeriesLastSeen`).
8. `admin` now passes `approval_id` to publish (api-specs f717a3e) but the billing publish route declares no such parameter or 409
   `approval_not_executable`; billing takes the approval id from the grant (it goes in `billing.vendor_access.recorded`).
9. Tenancy outlet events carry the registration id only: no regime, and billing is not in `x-subscribers` of
   `tenancy.outlet.*` or `tenancy.organisation.created`. No tenancy event carries a series id and code, so `getSeriesLastSeen`
   resolves a series only after `RecordSeries`. Regime defaults to `regular` until tenancy says otherwise.
10. `getSeriesLastSeen` example uses series 4a6c with register code AAA; tenancy's example for 4a6c is A7K.

## Tax engine

- `tax/` is the Go engine seeded from the api-specs reference; all 64 vectors pass (`tax.RunVectors`, also the validation of a rule
  set). **Divergence from the reference, on purpose:** under `regular`, a tax class with no rule is an error (`unknown_tax_class`),
  never silently non-GST (it could under-tax a bill). The recompute then flags the bill as a mismatch with the reason in the
  exception params and the flagged event; the bill is not altered. The api-specs Go reference needs the same change and a vector
  for it (client-core's TypeScript engine already throws).
- Recompute: round-off uses the outlet's setting (if unknown: total divisible by 100 means on). The packing charge's class comes
  from `billing_settings.packing_tax_class` (default `fnb_standard`). Bill discounts have no reason code in the synced entity; the
  discount report uses a nil reason id.
- Results: `verified`, `mismatch` (high), `profile_mismatch`, `stale_rules`, `manual_entry`; `pending` until the rule set or profile
  arrives, then re-verified. A bill is never altered or rejected.
- Credit notes are recomputed against the stored original bill; if the original has not arrived they stay `pending`.

## Projection and storage

- Read models are stored as documents holding the API representation (`docs` table, one row per tenant, kind, id; JSONB body) with the
  columns lists filter and sort on. The generated API types are the model. Facts are immutable ops applied once by
  `(origin, stream, seq)`; an entity row is inserted if absent (a second copy is ignored).
- Reports read daily aggregates kept by the projection (the "daily sales, tax and tender aggregates" of ADR-0014 R3), not raw bills.
  Late documents are included in the aggregates (original Z plus revisions equals the projection). Credit notes do not reduce the
  item, category, staff and hourly reports (they are in the tax and day reports).
- Multi-day report lists page in memory over the aggregate rows of the range (fine at pilot scale; measure before wide use).
- Last-writer-wins entities (settings, tax profile, tender kinds) compare `(hlc, origin)` as text at row level, not per field group.
- The business day is keyed by UUIDv5 of outlet and date with the namespace in `entities/README.md` (provisional there).
  A close later than the lowest stamp stays a fact and a `late_document` finding (cause `second_close`); it makes no revision yet.
- Late = the op's seq is above the day's cutoff for its origin. Late documents of one origin accumulate and are sealed into one
  immutable revision once quiet for 30 s (`FlushRevisions`, `SealRevisions`; the process sweeps every 10 s). Numbering
  `Z/<date>/<device>/R<n>` comes from the canonical close's Z number.
- Not built: quarantine notices (`series_quarantined`), allowance detectors (`allowance_exceeded`, `handover_skipped`), cash events
  (applied nowhere, no API), `business_day_revision` echo handling, orders-side detectors. The register needs the permission at every
  outlet (it is per registration, not per outlet).

## Operations

- Tables of the vendor routes are service-global under `billing_global`; tenant tables use `billing_app` and row-level security.
  The platform vendor log and the `tax_rule_published` / `vendor_access.recorded` events share the publication's transaction.
  Directory-class reads (`x-vendor-audit: none`) run plain.
- `PublishLead` is configuration (`BILLING_PUBLISH_LEAD`, default 24 h), never a request field.
- Emitting settings is outside any transaction (the answer comes back through the bus). A retry with the same Idempotency-Key
  re-emits the same op id; a different body under the same key is not detected as `idempotency.mismatch` on these three routes.
- CI uses the template's caller job plus an integration job on Postgres 17. golangci-lint was not installed in the build
  environment, so only `go vet` ran locally.
