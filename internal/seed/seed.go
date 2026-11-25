// Package seed puts a repository into one of the example worlds of the billing contract (info.x-example-worlds in
// api-specs openapi/billing/v1). The documents come from the spec's own response examples (examples.json, extracted by
// tools/seedgen), so the double and the real service start from exactly the state the examples describe. The stored
// read models are the API representation, which is why an example can be stored as it is.
package seed

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hostryhq/go-common/ids"
	"github.com/hostryhq/go-common/tenancy"

	"github.com/hostryhq/svc-billing/internal/gen"
	"github.com/hostryhq/svc-billing/internal/tstamp"
	"github.com/hostryhq/svc-billing/repository"
	"github.com/hostryhq/svc-billing/service"
)

//go:embed examples.json
var examplesJSON []byte

// The ids of the example world.
const (
	Tenant       = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a60"
	OtherTenant  = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4b60"
	Outlet       = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a61"
	Registration = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a6b"
	SeriesID     = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a6c"
	Bill         = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f7001"
)

// At is the instant of the examples.
var At = time.Date(2026, 11, 24, 13, 5, 11, 120_000_000, time.UTC)

type example struct {
	World    string          `json:"world"`
	Request  json.RawMessage `json:"request"`
	Response json.RawMessage `json:"response"`
}

func examples() map[string]example {
	var m map[string]example
	if err := json.Unmarshal(examplesJSON, &m); err != nil {
		panic(err)
	}
	return m
}

func resp[T any](ex map[string]example, op string) T {
	var v T
	if err := json.Unmarshal(ex[op].Response, &v); err != nil {
		panic(fmt.Sprintf("seed: example of %s: %v", op, err))
	}
	return v
}

func items[T any](ex map[string]example, op string) []T {
	return resp[struct {
		Items []T `json:"items"`
	}](ex, op).Items
}

// Worlds are the world names of the contract.
var Worlds = []string{"base", "payments-exceed", "rule-set-draft", "rule-set-signed"}

// Apply stores the named world for the example organisation.
func Apply(ctx context.Context, repo repository.Repository, world string) error {
	known := false
	for _, w := range Worlds {
		known = known || w == world
	}
	if !known {
		return fmt.Errorf("seed: unknown world %q", world)
	}
	ex := examples()
	tctx := tenancy.With(ctx, tenancy.Context{TenantID: ids.ID(Tenant)})
	if err := repo.InTx(tctx, func(ctx context.Context, tx repository.Tx) error { return base(ctx, tx, ex, world) }); err != nil {
		return err
	}
	return repo.InGlobalTx(ctx, func(ctx context.Context, tx repository.GlobalTx) error { return global(ctx, tx, ex, world) })
}

func put(ctx context.Context, tx repository.Tx, d repository.Doc, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	d.Body = b
	return tx.SaveDoc(ctx, d)
}

func base(ctx context.Context, tx repository.Tx, ex map[string]example, world string) error {
	if err := put(ctx, tx, repository.Doc{Kind: repository.KindOutlet, ID: Outlet, Outlet: Outlet}, service.OutletRec{OutletID: Outlet, RegistrationID: Registration, Regime: "regular"}); err != nil {
		return err
	}
	// the bill, its payments and the credit note
	d := resp[gen.BillDetail](ex, "getBill")
	pays := d.Payments
	var lineIDs []string
	for i := range d.Lines {
		lineIDs = append(lineIDs, fmt.Sprintf("0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f60%02d", 21+i))
	}
	rec := service.BillRec{Detail: d, Calc: service.BillCalc{SeriesCode: "AAA", FinancialYear: "2627", GrossPaise: 48000 * 0, TotalPaise: d.Total.AmountPaise,
		RoundOffPaise: d.RoundOff.AmountPaise, LineIDs: lineIDs}}
	for _, t := range d.TaxLines {
		rec.Calc.TaxablePaise += t.Taxable.AmountPaise
		rec.Calc.CGSTPaise += t.Cgst.AmountPaise
		rec.Calc.SGSTPaise += t.Sgst.AmountPaise
	}
	for _, l := range d.Lines {
		rec.Calc.GrossPaise += l.Net.AmountPaise
	}
	if err := put(ctx, tx, repository.Doc{Kind: repository.KindBill, ID: d.Id, Outlet: Outlet, Date: d.BusinessDate.String(), Sort: d.FinalizedAt.String(), K1: d.Number, K2: *d.CheckId}, rec); err != nil {
		return err
	}
	for _, p := range pays {
		if err := put(ctx, tx, repository.Doc{Kind: repository.KindPayment, ID: p.Id, Outlet: Outlet, Date: p.BusinessDate.String(), Sort: p.PaidAt.String(), K1: p.BillId, K2: string(p.Tender)}, p); err != nil {
			return err
		}
	}
	cn := resp[gen.CreditNote](ex, "getCreditNote")
	if err := put(ctx, tx, repository.Doc{Kind: repository.KindCreditNote, ID: cn.Id, Outlet: Outlet, Date: cn.BusinessDate.String(), Sort: cn.CreatedAt.String(), K1: cn.BillId},
		service.CreditRec{Note: cn, Calc: service.CreditCalc{SeriesCode: "AAB", FinancialYear: "2627", TotalPaise: cn.Total.AmountPaise}}); err != nil {
		return err
	}
	for _, r := range items[gen.Refund](ex, "listRefunds") {
		if err := put(ctx, tx, repository.Doc{Kind: repository.KindRefund, ID: r.Id, Outlet: Outlet, Date: r.BusinessDate.String(), Sort: r.RefundedAt.String(), K1: r.BillId}, r); err != nil {
			return err
		}
	}
	for _, s := range items[gen.Shift](ex, "listShifts") {
		if err := put(ctx, tx, repository.Doc{Kind: repository.KindShift, ID: s.Id, Outlet: Outlet, Date: s.BusinessDate.String(), Sort: s.OpenedAt.String()}, s); err != nil {
			return err
		}
	}
	for _, c := range items[gen.InternalConsumption](ex, "listInternalConsumptions") {
		if err := put(ctx, tx, repository.Doc{Kind: repository.KindInternal, ID: c.Id, Outlet: Outlet, Date: c.BusinessDate.String(), Sort: c.ClosedAt.String()}, c); err != nil {
			return err
		}
	}
	day := resp[gen.BusinessDay](ex, "getBusinessDay")
	if err := put(ctx, tx, repository.Doc{Kind: repository.KindBusinessDay, ID: day.Id, Outlet: Outlet, Date: day.BusinessDate.String(), Sort: day.BusinessDate.String(), K1: string(day.Status)}, day); err != nil {
		return err
	}
	for _, r := range items[gen.BusinessDayRevision](ex, "listBusinessDayRevisions") {
		if err := put(ctx, tx, repository.Doc{Kind: repository.KindRevision, ID: r.Id, Outlet: Outlet, Date: day.BusinessDate.String(), Sort: fmt.Sprint(1000000 + r.Revision), K1: r.BusinessDayId, K2: "emitted"}, r); err != nil {
			return err
		}
	}
	// register and gaps
	for _, s := range items[gen.RegisterSeries](ex, "listDocumentRegister") {
		last := int64(s.Count)
		sr := service.SeriesRec{Series: s, Ranges: [][2]int64{{1, last}}}
		if err := put(ctx, tx, repository.Doc{Kind: repository.KindSeries, ID: fmt.Sprintf("%s|%s|%s", s.TaxRegistrationId, s.SeriesCode, s.FinancialYear), Sort: s.SeriesCode + "|" + s.FinancialYear, K1: string(s.Kind), K2: SeriesID}, sr); err != nil {
			return err
		}
	}
	for i, g := range items[gen.RegisterGap](ex, "listDocumentGaps") {
		key := fmt.Sprintf("%s|%s|%s", g.TaxRegistrationId, g.SeriesCode, g.FinancialYear)
		if err := put(ctx, tx, repository.Doc{Kind: repository.KindGap, ID: fmt.Sprintf("0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f70c%d", i+1), Sort: g.DetectedAt.String(), K1: key, K2: string(g.Status)}, g); err != nil {
			return err
		}
	}
	// configuration and the rule set in force
	cfg := resp[gen.OutletBillingConfig](ex, "getOutletBillingConfig")
	if err := put(ctx, tx, repository.Doc{Kind: repository.KindSettings, ID: Outlet, Outlet: Outlet}, struct {
		Settings gen.BillingSettings `json:"settings"`
	}{cfg.Settings}); err != nil {
		return err
	}
	for i, p := range cfg.TaxProfiles {
		if err := put(ctx, tx, repository.Doc{Kind: repository.KindTaxProfile, ID: fmt.Sprintf("0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f70b%d", i+1), Outlet: Outlet, Sort: p.FinancialYear, K1: p.FinancialYear}, p); err != nil {
			return err
		}
	}
	for _, t := range cfg.Tenders {
		if err := put(ctx, tx, repository.Doc{Kind: repository.KindTender, ID: t.Id, Outlet: Outlet, Sort: t.Name}, t); err != nil {
			return err
		}
	}
	for _, r := range items[gen.TaxRuleSet](ex, "listTaxRuleSets") {
		rs := service.RuleSetRec{ID: r.Id, Version: r.Version, Name: r.Name, EffectiveDate: r.EffectiveDate.String(), CalcV: r.CalcV, ReviewStatus: string(r.ReviewStatus),
			ContentHash: r.ContentHash, PublishedAt: r.PublishedAt.String(), Rules: r.Rules}
		if err := put(ctx, tx, repository.Doc{Kind: repository.KindRuleSet, ID: r.Id, Date: rs.EffectiveDate, Sort: rs.EffectiveDate}, rs); err != nil {
			return err
		}
	}
	if err := aggregates(ctx, tx, ex); err != nil {
		return err
	}
	if world == "payments-exceed" {
		for _, e := range items[gen.Exception](ex, "listBillingExceptions") {
			if err := put(ctx, tx, repository.Doc{Kind: repository.KindException, ID: e.Id, Outlet: Outlet, Date: e.BusinessDate.String(), Sort: e.DetectedAt.String(), K1: e.Kind, K2: string(e.Status)}, e); err != nil {
				return err
			}
		}
	}
	return nil
}

// aggregates stores the report examples as the daily aggregates they read.
func aggregates(ctx context.Context, tx repository.Tx, ex map[string]example) error {
	date := "2026-11-24"
	id := func(k, n string) string { return "0190a1b2-c3d4-7e5f-8a9b-" + k + n }
	for _, r := range items[gen.DaySummaryRow](ex, "getDaySummaryReport") {
		if err := put(ctx, tx, repository.Doc{Kind: repository.KindAggDay, ID: id("0c1d2e3f80", "01"), Outlet: Outlet, Date: r.BusinessDate.String(), Sort: r.BusinessDate.String()}, r.Totals); err != nil {
			return err
		}
	}
	for i, r := range items[gen.TaxSummaryRow](ex, "getTaxSummaryReport") {
		if err := put(ctx, tx, repository.Doc{Kind: repository.KindAggTax, ID: id("0c1d2e3f81", fmt.Sprintf("%02d", i)), Outlet: Outlet, Date: date, Sort: r.TaxClass}, service.AggTax{Row: r}); err != nil {
			return err
		}
	}
	for i, r := range items[gen.TenderSummaryRow](ex, "getTenderSummaryReport") {
		if err := put(ctx, tx, repository.Doc{Kind: repository.KindAggTender, ID: id("0c1d2e3f82", fmt.Sprintf("%02d", i)), Outlet: Outlet, Date: date, Sort: string(r.Tender)}, r); err != nil {
			return err
		}
	}
	for i, r := range items[gen.ItemSalesRow](ex, "getItemSalesReport") {
		if err := put(ctx, tx, repository.Doc{Kind: repository.KindAggItem, ID: id("0c1d2e3f83", fmt.Sprintf("%02d", i)), Outlet: Outlet, Date: date, Sort: r.ItemId}, r); err != nil {
			return err
		}
	}
	for i, r := range items[gen.CategorySalesRow](ex, "getCategorySalesReport") {
		if err := put(ctx, tx, repository.Doc{Kind: repository.KindAggCategory, ID: id("0c1d2e3f84", fmt.Sprintf("%02d", i)), Outlet: Outlet, Date: date, Sort: r.CategoryId}, r); err != nil {
			return err
		}
	}
	for i, r := range items[gen.StaffSalesRow](ex, "getStaffSalesReport") {
		if err := put(ctx, tx, repository.Doc{Kind: repository.KindAggStaff, ID: id("0c1d2e3f85", fmt.Sprintf("%02d", i)), Outlet: Outlet, Date: date, Sort: r.MemberId}, r); err != nil {
			return err
		}
	}
	for i, r := range items[gen.HourlySalesRow](ex, "getHourlySalesReport") {
		if err := put(ctx, tx, repository.Doc{Kind: repository.KindAggHourly, ID: id("0c1d2e3f86", fmt.Sprintf("%02d", i)), Outlet: Outlet, Date: date, Sort: fmt.Sprint(r.Hour)}, r); err != nil {
			return err
		}
	}
	for i, r := range items[gen.DiscountRow](ex, "getDiscountReport") {
		if err := put(ctx, tx, repository.Doc{Kind: repository.KindDiscount, ID: id("0c1d2e3f87", fmt.Sprintf("%02d", i)), Outlet: Outlet, Date: date, Sort: r.At.String(), K1: string(r.Kind)}, r); err != nil {
			return err
		}
	}
	return nil
}

func global(ctx context.Context, tx repository.GlobalTx, ex map[string]example, world string) error {
	for _, t := range []string{Tenant, OtherTenant, "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4c60"} {
		if err := tx.AddTenant(ctx, t, At); err != nil {
			return err
		}
	}
	// version 1, published, in force
	rs := items[gen.TaxRuleSet](ex, "listTaxRuleSets")[0]
	h := rs.ContentHash
	v1 := gen.TaxRuleSetMaster{Id: "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f7041", Version: 1, Revision: 1, Name: rs.Name, EffectiveDate: rs.EffectiveDate, CalcV: 1,
		State: gen.TaxRuleSetMasterStatePublished, ReviewStatus: gen.TaxRuleSetMasterReviewStatusUnreviewed, ContentHash: &h, Rules: rs.Rules, CreatedAt: tstamp.Of(At), PublishedAt: tstamp.Ptr(At)}
	if err := putMaster(ctx, tx, v1); err != nil {
		return err
	}
	switch world {
	case "rule-set-draft":
		return putMaster(ctx, tx, resp[gen.TaxRuleSetMaster](ex, "getTaxRuleSetMaster"))
	case "rule-set-signed":
		m := resp[gen.TaxRuleSetMaster](ex, "signTaxRuleSet")
		hh := service.ContentHash(m)
		m.ContentHash = &hh
		return putMaster(ctx, tx, m)
	}
	return nil
}

func putMaster(ctx context.Context, tx repository.GlobalTx, m gen.TaxRuleSetMaster) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return tx.PutMaster(ctx, repository.Master{ID: m.Id, Version: m.Version, Revision: m.Revision, State: string(m.State), Created: m.CreatedAt.Time, Body: b})
}

// PublishGrantSubject is the subject hash the example grant must carry for the example rule set 7042 in the
// rule-set-signed world.
func PublishGrantSubject() string {
	ex := examples()
	m := resp[gen.TaxRuleSetMaster](ex, "signTaxRuleSet")
	return service.PublishSubjectHash(m.Id, service.ContentHash(m), m.EffectiveDate.String())
}
