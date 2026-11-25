# Operations status

Every operationId of `api-specs/openapi/billing/v1/openapi.yaml` (commit recorded in `internal/gen/SPEC_VERSION`: 1462fb8).
Status is **implemented** in the service (`api` -> `service` -> `repository/pg`), in the test double (`double/`, same API and service over
`repository/mem`) and covered by the shared contract suite (`contracttest`) in the example world the spec declares for it (`SeedWorld`,
`internal/seed`), on the real service and the double, with no operation skipped. The double also serves the spec examples verbatim,
except for the two operations in `internal/contractenv.ExampleUnreachable` (`getBillingProjectionDigest`, `signTaxRuleSet`: checked for status, media type and schema only); the one
correction left in the contract transport is `listBills`' contradictory `verification=mismatch` example (docs/notes.md, spec problems).
**Not yet implemented: none** (details of what an operation does not cover are in Notes and `docs/notes.md`).

| operationId | Method and path | Permission or caller | Status | Example world | Notes |
|---|---|---|---|---|---|
| getSeriesLastSeen | GET /internal/v1/document-series/{series_id}/last-seen | register.read | implemented | base | internal; the series is named by its tenancy id, learned from tenancy.series.issued; a series no number reached answers count 0 |
| listTaxRuleSetMasters | GET /internal/v1/vendor/tax-rule-sets | admin.taxrules.manage | implemented | rule-set-draft | vendor; emit counts only; x-vendor-audit none |
| createTaxRuleSetDraft | POST /internal/v1/vendor/tax-rule-sets | admin.taxrules.manage | implemented | base | vendor; version = max + 1; 409 when a signed or published version has the effective date |
| getTaxRuleSetMaster | GET /internal/v1/vendor/tax-rule-sets/{rule_set_id} | admin.taxrules.manage | implemented | rule-set-draft | vendor; ETag = revision |
| updateTaxRuleSetDraft | PATCH /internal/v1/vendor/tax-rule-sets/{rule_set_id} | admin.taxrules.manage | implemented | rule-set-draft | vendor; If-Match; 412 precondition_failed |
| deleteTaxRuleSetDraft | DELETE /internal/v1/vendor/tax-rule-sets/{rule_set_id} | admin.taxrules.manage | implemented | rule-set-draft | vendor; draft only |
| publishTaxRuleSet | POST /internal/v1/vendor/tax-rule-sets/{rule_set_id}/publication | admin.taxrules.publish | implemented | rule-set-signed | vendor; grant (A2.6 order: signature, typ/realm/aud, exp, kind, org, approver = bearer, subject hash from the stored version, single use; grant.invalid/expired/subject_mismatch/used), lead time on this clock, one emit_queue op per organisation, two platform events, one audit row; no approval_id here (admin facade only) |
| signTaxRuleSet | POST /internal/v1/vendor/tax-rule-sets/{rule_set_id}/signature | admin.taxrules.manage | implemented | rule-set-draft | vendor; validates again; stores sign-off and content hash |
| validateTaxRuleSet | POST /internal/v1/vendor/tax-rule-sets/{rule_set_id}/validation | admin.taxrules.manage | implemented | rule-set-draft | vendor; BR-A1-82 structure and all 65 conformance vectors |
| listBills | GET /v1/bills | report.view.outlet | implemented | base | bill register; filters by business date, invoice dates, number, source, verification; newest first |
| getBill | GET /v1/bills/{bill_id} | report.view.outlet | implemented | base | with lines, tax lines and payments; another outlet or tenant is 404 |
| listBusinessDays | GET /v1/business-days | report.view.outlet | implemented | base | one row per outlet and date (UUIDv5 id) |
| getBusinessDay | GET /v1/business-days/{business_day_id} | report.view.outlet | implemented | base | close fact, revision and late counts |
| listBusinessDayRevisions | GET /v1/business-days/{business_day_id}/revisions | report.view.outlet | implemented | late-revision | immutable; one per origin device and quiet batch (FlushRevisions) |
| listCreditNotes | GET /v1/credit-notes | report.view.outlet | implemented | base |  |
| getCreditNote | GET /v1/credit-notes/{credit_note_id} | report.view.outlet | implemented | base |  |
| listDocumentRegister | GET /v1/document-register | report.view.outlet | implemented | base | needs the permission at every outlet (the register is per registration); count, first, last, gaps |
| listDocumentGaps | GET /v1/document-register/gaps | report.view.outlet | implemented | gap-open | open, arrived; manual-book gaps listed apart (kind) |
| listBillingExceptions | GET /v1/exceptions | exceptions.view | implemented | payments-exceed | detectors: payments_exceed_total, line_on_two_bills, two_bills_one_check, tax_mismatch, stale_rules, profile_mismatch, doc_type_regime_mismatch, manual_entry, manual_book_missing_number, document_gap, two_open_days, late_document. Not built: series_quarantined, allowance_exceeded, handover_skipped |
| getBillingException | GET /v1/exceptions/{exception_id} | exceptions.view | implemented | payments-exceed |  |
| listInternalConsumptions | GET /v1/internal-consumptions | report.view.outlet | implemented | base | O2-1 provisional; not sales, not tax |
| getOutletBillingConfig | GET /v1/outlets/{outlet_id}/billing-config | settings.read | implemented | base | defaults when the outlet never changed a setting |
| setDiscountLimit | PUT /v1/outlets/{outlet_id}/discount-limit | bill.discount.limit.edit (step-up) | implemented | base | strong session; emit.Client under settings.discount_limit.set (set only); 503 emit.unavailable when the gateway is down; key from the Idempotency-Key |
| setNumberingMode | PUT /v1/outlets/{outlet_id}/numbering-mode | settings.numbering.edit (step-up) | implemented | base | strong session; 409 open_checks_exist from orders getOpenCheckCount; emit.Client under settings.numbering.set; 503 emit.unavailable |
| setOutletTaxProfile | PUT /v1/outlets/{outlet_id}/tax-profile | settings.gst_profile.edit (step-up) | implemented | base | strong session; 422 specified_premises_retroactive after 30 days of the financial year; emit.Client under settings.tax_profile.set (price_mode validated, not emitted: no intent carries it); 503 emit.unavailable |
| listPayments | GET /v1/payments | report.view.outlet | implemented | base |  |
| getBillingProjectionDigest | GET /v1/projection-digests | report.view.outlet | implemented | base | sha256-canon-v1 over the stored rows of the date; example is not derivable (spec problem) |
| listRefunds | GET /v1/refunds | report.view.outlet | implemented | base |  |
| getCategorySalesReport | GET /v1/reports/category-sales | report.view.outlet | implemented | base | reads the daily aggregates |
| getDaySummaryReport | GET /v1/reports/day-summary | report.view.outlet | implemented | base | reads the daily aggregates; status and revision count from the day |
| getDiscountReport | GET /v1/reports/discounts | report.view.outlet | implemented | base | discounts, comps, refunds, credit notes, internal closures |
| getHourlySalesReport | GET /v1/reports/hourly-sales | report.view.outlet | implemented | base | IST hour |
| getItemSalesReport | GET /v1/reports/item-sales | report.view.outlet | implemented | base | variant level, top first |
| getStaffSalesReport | GET /v1/reports/staff-sales | report.view.outlet | implemented | base | cashier and waiter |
| getTaxSummaryReport | GET /v1/reports/tax-summary | report.view.outlet | implemented | base | by invoice date; bills and credit notes apart |
| getTenderSummaryReport | GET /v1/reports/tender-summary | report.view.outlet | implemented | base | cash and UPI never combined |
| listShifts | GET /v1/shifts | report.view.outlet | implemented | base |  |
| listTaxRuleSets | GET /v1/tax-rule-sets | settings.read | implemented | base | rule sets as received by the organisation (tax_rule_set rows) |
| getTaxRuleSet | GET /v1/tax-rule-sets/{rule_set_id} | settings.read | implemented | base |  |

39 operations.
