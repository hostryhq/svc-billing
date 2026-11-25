# Synced entity schemas

One YAML file per synced entity, grouped by product (`restaurant/`). The sync engine (`go-common/sync`, `client-core`)
generates types, local DDL and the per-policy tests from these files; every service generates its bus subscriptions and
projection set from `owner` (ADR-0014 R1: each synced entity has exactly one owning service; a service applies only the
mutations of entities it owns and ignores the rest of an op). `make lint-entities` (`cmd/validate-entities`) is the CI gate.

## Keys

| Key | Meaning |
|---|---|
| `entity`, `entity_v` | Name (equals the file name) and schema version. Older clients ignore unknown fields (ADR-0002 S13). |
| `owner` | `catalog`, `orders` or `billing`. Exactly one. |
| `stream` | `outlet`, `brand` or `tenant` (ADR-0002 S3). |
| `policy` | Entity default: `append`, `lww`, `rank`, or `mixed` (each field states its own). |
| `id` | `client_uuidv7`, or `uuidv5` with a `name` template when two devices must converge on one row (deterministic ids). |
| `tombstone` | Deletable master data; `del` and `undel` sync. |
| `guarded` | Money or tax: only `append` facts; invariants are detectors, resolved by a new op (refund, credit note), never merged. |
| `replicated_to` | Read-only replicas of another owner's reference entity (ADR-0014 A2.4): `[{service, purpose}]`. Applied by the same engine into tables only the projection role writes; never written, emitted or served as the entity; never `pii`; a `guarded` entity only with a stated `guarded_exception` (day_close, see the ADR note). Initial list, all to `orders`: `station`, `dining_table`, `outlet_settings`, `business_day`, `day_close`. |
| `cloud_origin_only` | Written only by a cloud service through the gateway `emit` API. |
| `fields.<name>` | `type`, `required`, `policy`, `description`, plus `group`, `values`, `order`, `ref`, `pii`, `doc_number`, bounds. |
| `detectors` | Derived queries that raise exceptions (shared schema `common/exceptions.yaml`). |
| `written_by_intents` | Informational: intents expected to mutate the entity. The authoritative list is the intent registry (`sync/intents`); `make lint-intents` fails a name that is not in it or does not list this entity. |

## Field policy (ADR-0002 S6)

`immutable` (set at insert, never changed), `lww` (highest `(hlc, origin)` wins per field **group**; fields in one group win
together), `lww_review` (as `lww`, and the mutation carries `prev`, so a concurrent edit keeps the winner, retains the loser and
asks a manager), `delta` (signed deltas, value is the sum), `rank` (forward-only; `order` lists the values lowest first).
`append` entities hold only `immutable` fields. A field that touches money or tax is never silently merged: it is `immutable`
on a guarded entity, or `lww_review` on a draft or setting.

## Types and the wire

Op bodies carry strings, booleans and integers within +-(2^53-1) only (no fractions, no nested objects; `svc-sync/contract`).
`id` is a string; `date` is `YYYY-MM-DD` (business date or invoice date, explicit, never derived from a timestamp);
`ts` is RFC 3339 UTC; `paise` is integer paise (name ends `_paise`); `qty_milli` is a quantity in thousandths (`_milli`);
`bp` is a rate in basis points (`_bp`). Array-valued facts (Z totals by tender and series range) travel as canonical-JSON
strings (`*_json`) until the engine supports child rows; this is marked `x-provisional` where used.

## Implicit columns

Every row carries, from the engine and not from this schema: `tenant_id`, the stream scope id (`outlet_id` or `brand_id`),
the creating op's `origin` (device) and `actor` (member) and its `hlc`. Reports that need "member" and "device" read those,
which is why `check_present` and friends have no `member_id` or `device_id` field. Rows have no foreign-key constraints
(orphan tolerance, ADR-0002 S5); references are checked by detectors.

## `pii`

A field with `pii: true` travels in the op's `pii` object, outside the op hash (ADR-0002 S4). No money, tax or numbering
field may be `pii` (the lint refuses it). Restaurant fields: guest and customer names, a takeaway phone.

## Stations are per outlet, the menu is per brand

`category.default_station_code` and `item.station_code` are codes; `station.code` is unique per outlet, so one brand-wide menu
resolves to each outlet's own stations: `item_outlet.station_id`, then `item.station_code`, then
`category.default_station_code`, then the seeded `DEFAULT` station.

## Ownership table

| Owner | Entities |
|---|---|
| catalog (17) | category, item, item_variant, modifier_group, modifier, item_modifier_group, price_entry, reason_code (brand); item_outlet, station, floor, dining_table, printer, print_route, receipt_profile, outlet_power_profile, outlet_settings (outlet) |
| orders (16) | seating, seating_move, seating_merge, order, order_line, order_line_modifier, order_line_cancel, check, check_cancel, check_present, check_present_change, bill_request, kot, kot_line, kot_print, kot_handover |
| billing (21) | bill, bill_line, bill_tax_line, credit_note, credit_note_line, credit_note_tax_line, payment, refund, cash_event, shift, shift_close, business_day, day_open, day_close, business_day_revision, outlet_tax_profile, billing_settings, tender_kind, internal_consumption (outlet); tax_rule_set, tax_rule (tenant) |

`bill.finalize` is one op that mutates `bill`, `bill_line`, `bill_tax_line` and `payment` (billing) and the `check`'s `status` and
`bill_id` (orders): each owner applies only its own mutations. Not in a stream (server-only): menu import jobs, the document
register, tax verification results, detector results, the vendor access log, print jobs (local to the printing device).

## Provisional choices (see each file's `x-provisional`)

- `outlet_settings` (catalog) is a new entity, not named in ADR-0014 R3; it gives non-money settings one owner.
- `internal_consumption` is owned by billing as a non-tax internal document (founder default O2-1); `check.status` gains
  `closed_internal`. ADR needed.
- `receipt_profile` address edit is a normal outlet setting; the spec places it in the strong GST profile.
- Deterministic UUIDv5 ids use the namespace UUID `9a3c7f1e-5b2d-5c84-a1e6-4d0f8b27c3a9` (to be fixed in `go-common/sync`).
