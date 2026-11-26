package service_test

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-auth/authtest"
	"github.com/hostryhq/go-common/apperr"
	"github.com/hostryhq/go-common/clock"
	"github.com/hostryhq/go-common/emit"
	"github.com/hostryhq/go-common/ids"
	"github.com/hostryhq/go-common/tenancy"
	"github.com/hostryhq/go-common/testkit"

	"github.com/hostryhq/svc-billing/internal/gen"
	"github.com/hostryhq/svc-billing/repository/mem"
	"github.com/hostryhq/svc-billing/service"
)

const (
	tenantA = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a60"
	tenantB = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4b60"
	outletA = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a61"
	deviceA = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5e"
	deviceB = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5f"
	member  = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a66"
	rsID    = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f7041"
)

type fixture struct {
	t     *testing.T
	svc   *service.Service
	store *mem.Store
	clk   *testkit.FakeClock
	cat   *auth.Catalogue
	iss   *authtest.Issuer
	seq   map[string]int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	cat, err := gen.Catalogue()
	if err != nil {
		t.Fatal(err)
	}
	clk := testkit.NewClock()
	clk.Set(time.Date(2026, 11, 24, 13, 5, 11, 0, time.UTC))
	store := mem.New()
	f := &fixture{t: t, store: store, clk: clk, cat: cat, iss: authtest.NewTenantIssuer(clock.System), seq: map[string]int64{}}
	f.svc = service.New(store, service.Options{Clock: clk, IDs: clk.IDs(), Log: slog.New(slog.DiscardHandler), RevisionWindow: 30 * time.Second})
	for _, tn := range []string{tenantA, tenantB} {
		if err := f.svc.RecordOutlet(f.sys(tn), service.OutletRec{OutletID: outletA, RegistrationID: "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a6b", Regime: "regular"}); err != nil {
			t.Fatal(err)
		}
	}
	f.apply(tenantA, "cloud-1", "tenant", f.ruleSetMuts(tenantA, 1, "2026-04-01", 250)...)
	return f
}

func (f *fixture) sys(tenant string) context.Context {
	return tenancy.With(context.Background(), tenancy.Context{TenantID: ids.ID(tenant), ActorID: "test"})
}

func (f *fixture) as(tenant string) context.Context {
	return f.iss.ContextFor(context.Background(), f.cat, authtest.Spec{Type: auth.TypeUser, Tenant: ids.ID(tenant), Grants: authtest.OwnerGrants()})
}

// apply projects one op with a fresh sequence number for (origin, stream).
func (f *fixture) apply(tenant, origin, stream string, muts ...service.Mut) service.Op {
	f.t.Helper()
	k := tenant + origin + stream
	f.seq[k]++
	op := service.Op{Tenant: tenant, Origin: origin, Stream: stream, Seq: f.seq[k], Hash: fmt.Sprintf("h%d", f.seq[k]), HLC: fmt.Sprintf("%016x", f.clk.Now().UnixMilli()<<16),
		Intent: "test", At: f.clk.Now(), Actor: member, Muts: int64s(muts)}
	if err := f.svc.Project(f.sys(tenant), op); err != nil {
		f.t.Fatal(err)
	}
	return op
}

// int64s turns the integers of the test data into int64, as sync.ParseOp delivers them (strings, booleans, int64 and
// nil only).
func int64s(muts []service.Mut) []service.Mut {
	out := make([]service.Mut, len(muts))
	for i, m := range muts {
		out[i] = m
		if m.F == nil {
			continue
		}
		out[i].F = map[string]any{}
		for k, v := range m.F {
			if n, ok := v.(int); ok {
				v = int64(n)
			}
			out[i].F[k] = v
		}
	}
	return out
}

func (f *fixture) ruleSetMuts(tenant string, version int, effective string, half int) []service.Mut {
	id := service.RuleSetRowID(tenant, version)
	return []service.Mut{
		{E: "tax_rule_set", ID: id, K: "insert", F: map[string]any{"version": version, "name": "t", "effective_date": effective, "calc_v": 1, "review_status": "unreviewed", "content_hash": "x"}},
		{E: "tax_rule", ID: fmt.Sprintf("0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f90%02d", version), K: "insert", F: map[string]any{"rule_set_id": id, "tax_class": "fnb_standard", "regime": "regular",
			"specified_premises": false, "order_channel": "dine_in", "doc_type": "tax_invoice", "gst_applies": true, "cgst_bp": half, "sgst_bp": half, "igst_bp": 0}},
	}
}

// billMuts is one bill.finalize op: a bill of 2 x Rs 240 at 5 percent (total Rs 504), its line, its tax line and a payment.
func billMuts(billID, number string, cgst int, pay bool) []service.Mut {
	ms := []service.Mut{
		{E: "bill", ID: billID, K: "insert", F: map[string]any{"number": number, "series_code": "AAA", "doc_type": "tax_invoice", "source_kind": "check", "order_type": "dine_in",
			"eco": false, "business_date": "2026-11-24", "invoice_date": "2026-11-24", "financial_year": "2627", "calc_v": 1, "ruleset_id": service.RuleSetRowID(tenantA, 1),
			"price_mode": "exclusive", "gross_paise": 48000, "discount_paise": 0, "service_charge_bp": 0, "service_charge_paise": 0, "packing_charge_paise": 0,
			"non_gst_subtotal_paise": 0, "taxable_paise": 48000, "cgst_paise": cgst, "sgst_paise": cgst, "igst_paise": 0, "round_off_paise": 0, "total_paise": 48000 + 2*cgst}},
		{E: "bill_line", ID: billID[:30] + "0001", K: "insert", F: map[string]any{"bill_id": billID, "line_no": 1, "item_id": "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f5101", "item_name": "Paneer Tikka",
			"category_name": "Starters", "qty_milli": 2000, "unit_price_paise": 24000, "modifier_delta_paise": 0, "line_discount_paise": 0, "bill_discount_alloc_paise": 0,
			"net_paise": 48000, "comp": false, "comp_value_paise": 0, "tax_class": "fnb_standard"}},
		{E: "bill_tax_line", ID: billID[:30] + "0002", K: "insert", F: map[string]any{"bill_id": billID, "tax_class": "fnb_standard", "rate_bp": 500, "gst_applies": true,
			"charge_alloc_paise": 0, "taxable_paise": 48000, "cgst_paise": cgst, "sgst_paise": cgst, "igst_paise": 0}},
	}
	if pay {
		ms = append(ms, service.Mut{E: "payment", ID: billID[:30] + "0003", K: "insert", F: map[string]any{"bill_id": billID, "target_kind": "bill", "tender": "cash", "amount_paise": 48000 + 2*cgst, "business_date": "2026-11-24"}})
	}
	return ms
}

func billID(n int) string { return fmt.Sprintf("0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f%04d", 7000+n) }

func number(n int) string { return fmt.Sprintf("AAA/2627/%06d", n) }

func (f *fixture) bill(t *testing.T, id string) gen.BillDetail {
	t.Helper()
	b, err := f.svc.GetBill(f.as(tenantA), id)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var e *apperr.Error
	if err == nil || !errorsAs(err, &e) || e.Code != code {
		t.Fatalf("want error %q, got %v", code, err)
	}
}

func errorsAs(err error, e **apperr.Error) bool {
	for err != nil {
		if x, ok := err.(*apperr.Error); ok {
			*e = x
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func (f *fixture) eventTypes(tenant string) []string {
	var out []string
	for _, e := range f.store.Events() {
		if e.TenantID.String() == tenant {
			out = append(out, e.Type)
		}
	}
	return out
}

func count(xs []string, x string) int {
	n := 0
	for _, v := range xs {
		if v == x {
			n++
		}
	}
	return n
}

func (f *fixture) exceptions(kind string) []gen.Exception {
	pg, err := f.svc.ListExceptions(f.as(tenantA), outletA, "", kind, "", 50)
	if err != nil {
		f.t.Fatal(err)
	}
	return pg.Items
}

// ---- verification ----

func TestBillVerifiedAndReplayIsIdempotent(t *testing.T) {
	f := newFixture(t)
	op := f.apply(tenantA, deviceA, "outlet:"+outletA, billMuts(billID(1), number(1), 1200, true)...)
	b := f.bill(t, billID(1))
	if b.Verification.Status != gen.VerificationStatusVerified || b.Paid.AmountPaise != 50400 || b.PaymentStatus != "paid" {
		t.Fatalf("verification %+v paid %v status %v", b.Verification, b.Paid, b.PaymentStatus)
	}
	// the same op again (redelivery, restore, bus reset) changes nothing and publishes nothing more
	if err := f.svc.Project(f.sys(tenantA), op); err != nil {
		t.Fatal(err)
	}
	if n := count(f.eventTypes(tenantA), "billing.bill.recorded"); n != 1 {
		t.Fatalf("%d bill.recorded events, want 1", n)
	}
	pg, _ := f.svc.ListPayments(f.as(tenantA), outletA, "", "", "", "", 50)
	if len(pg.Items) != 1 {
		t.Fatalf("%d payments, want 1", len(pg.Items))
	}
}

// A bill whose tax does not match the recompute is flagged and kept exactly as it arrived (R6.6).
func TestMismatchIsFlaggedNeverAltered(t *testing.T) {
	f := newFixture(t)
	f.apply(tenantA, deviceA, "outlet:"+outletA, billMuts(billID(2), number(1), 1100, false)...) // CGST 1100 instead of 1200
	b := f.bill(t, billID(2))
	if b.Verification.Status != gen.VerificationStatusMismatch || b.Verification.Difference.AmountPaise != -200 {
		t.Fatalf("verification %+v", b.Verification)
	}
	if b.Total.AmountPaise != 50200 || b.Lines[0].Net.AmountPaise != 48000 {
		t.Fatalf("the bill was altered: total %d", b.Total.AmountPaise)
	}
	ex := f.exceptions("tax_mismatch")
	if len(ex) != 1 || ex[0].Severity != gen.High {
		t.Fatalf("exceptions %+v", ex)
	}
	if count(f.eventTypes(tenantA), "billing.tax_verification.flagged") != 1 {
		t.Fatal("no flagged event")
	}
}

// An unknown tax class under the regular regime is flagged with a reason, never computed as non-GST.
func TestUnknownTaxClassIsFlagged(t *testing.T) {
	f := newFixture(t)
	ms := billMuts(billID(3), number(1), 1200, false)
	ms[1].F["tax_class"] = "typo_class"
	f.apply(tenantA, deviceA, "outlet:"+outletA, ms...)
	b := f.bill(t, billID(3))
	if b.Verification.Status != gen.VerificationStatusMismatch {
		t.Fatalf("verification %+v", b.Verification)
	}
	ex := f.exceptions("tax_mismatch")
	if len(ex) != 1 || ex[0].Params == nil || (*ex[0].Params)["reason"] != "unknown_tax_class" {
		t.Fatalf("exceptions %+v", ex)
	}
}

// A bill made under a rule set that is no longer the one in force on its invoice date is flagged stale_rules.
func TestStaleRulesAreFlagged(t *testing.T) {
	f := newFixture(t)
	f.apply(tenantA, "cloud-1", "tenant", f.ruleSetMuts(tenantA, 2, "2026-11-01", 300)...)
	f.apply(tenantA, deviceA, "outlet:"+outletA, billMuts(billID(4), number(1), 1200, false)...)
	if st := f.bill(t, billID(4)).Verification.Status; st != gen.VerificationStatusStaleRules {
		t.Fatalf("status %v", st)
	}
	if len(f.exceptions("stale_rules")) != 1 {
		t.Fatal("no stale_rules exception")
	}
}

// A bill that arrives before its rule set is pending, and is verified when the rule set arrives.
func TestPendingBillIsVerifiedWhenRuleSetArrives(t *testing.T) {
	f := newFixture(t)
	ms := billMuts(billID(5), number(1), 1200, false)
	ms[0].F["ruleset_id"] = service.RuleSetRowID(tenantA, 9)
	f.apply(tenantA, deviceA, "outlet:"+outletA, ms...)
	if st := f.bill(t, billID(5)).Verification.Status; st != gen.VerificationStatusPending {
		t.Fatalf("status %v", st)
	}
	f.apply(tenantA, "cloud-1", "tenant", f.ruleSetMuts(tenantA, 9, "2026-04-01", 250)...)
	if st := f.bill(t, billID(5)).Verification.Status; st == gen.VerificationStatusPending {
		t.Fatal("still pending after the rule set arrived")
	}
}

// ---- register ----

func TestGapIsDetectedAndArrives(t *testing.T) {
	f := newFixture(t)
	for _, n := range []int{1, 2, 4} {
		f.apply(tenantA, deviceA, "outlet:"+outletA, billMuts(billID(10+n), number(n), 1200, false)...)
	}
	gaps, err := f.svc.ListDocumentGaps(f.as(tenantA), "", "", "", 50)
	if err != nil || len(gaps.Items) != 1 || gaps.Items[0].MissingFrom != 3 || gaps.Items[0].MissingTo != 3 || gaps.Items[0].Status != "open" {
		t.Fatalf("gaps %+v err %v", gaps.Items, err)
	}
	if count(f.eventTypes(tenantA), "billing.document_gap.detected") != 1 {
		t.Fatal("no gap event")
	}
	f.apply(tenantA, deviceA, "outlet:"+outletA, billMuts(billID(13), number(3), 1200, false)...)
	gaps, _ = f.svc.ListDocumentGaps(f.as(tenantA), "arrived", "", "", 50)
	if len(gaps.Items) != 1 {
		t.Fatalf("the gap did not arrive: %+v", gaps.Items)
	}
	reg, _ := f.svc.ListDocumentRegister(f.as(tenantA), "", "", "", 50)
	if len(reg.Items) != 1 || reg.Items[0].Count != 4 || reg.Items[0].GapCount != 0 || *reg.Items[0].FirstNumber != number(1) || *reg.Items[0].LastNumber != number(4) {
		t.Fatalf("register %+v", reg.Items)
	}
}

func TestPaymentsExceedTotalIsAnException(t *testing.T) {
	f := newFixture(t)
	f.apply(tenantA, deviceA, "outlet:"+outletA, billMuts(billID(20), number(1), 1200, true)...)
	f.apply(tenantA, deviceB, "outlet:"+outletA, service.Mut{E: "payment", ID: "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f9a01", K: "insert",
		F: map[string]any{"bill_id": billID(20), "target_kind": "bill", "tender": "upi", "amount_paise": 50400, "business_date": "2026-11-24"}})
	if ex := f.exceptions("payments_exceed_total"); len(ex) != 1 || ex[0].Status != gen.ExceptionStatusOpen {
		t.Fatalf("exceptions %+v", ex)
	}
	// a refund is the resolving fact
	f.apply(tenantA, deviceB, "outlet:"+outletA, service.Mut{E: "refund", ID: "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f9a02", K: "insert",
		F: map[string]any{"bill_id": billID(20), "amount_paise": 50400, "tender": "upi", "reason_code_id": "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f5162", "business_date": "2026-11-24"}})
	if ex := f.exceptions("payments_exceed_total"); ex[0].Status != gen.ExceptionStatusResolved {
		t.Fatalf("not resolved: %+v", ex[0])
	}
}

// ---- business day ----

func closeMuts(date string, cutoff map[string]int64) []service.Mut {
	return []service.Mut{
		{E: "business_day", ID: "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f7031", K: "insert", F: map[string]any{"business_date": date, "status": "open"}},
		{E: "day_open", ID: "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f7032", K: "insert", F: map[string]any{"business_day_id": "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f7031", "business_date": date}},
	}
}

func TestLateBillStaysOnItsDayAndBecomesOneImmutableRevision(t *testing.T) {
	f := newFixture(t)
	f.apply(tenantA, deviceA, "outlet:"+outletA, closeMuts("2026-11-24", nil)...)
	f.apply(tenantA, deviceA, "outlet:"+outletA, billMuts(billID(30), number(1), 1200, true)...)
	dayID := ""
	days, _ := f.svc.ListBusinessDays(f.as(tenantA), outletA, "", "", 10)
	dayID = days.Items[0].Id
	f.apply(tenantA, deviceA, "outlet:"+outletA, service.Mut{E: "day_close", ID: "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f7033", K: "insert", F: map[string]any{"business_day_id": dayID,
		"z_number": "Z/2026-11-24/P1", "cutoff": fmt.Sprintf(`{"%s":2}`, deviceA), "cutoff_at": f.clk.Now().UTC().Format(time.RFC3339Nano), "gross_paise": 48000, "total_paise": 50400, "bill_count": 1, "tenders_json": "[]", "series_ranges_json": "[]"}})
	// a second device that was offline sends its bill after the close
	f.clk.Advance(time.Minute)
	f.apply(tenantA, deviceB, "outlet:"+outletA, billMuts(billID(31), "BBB/2627/000001", 1200, true)...)
	if !f.bill(t, billID(31)).Late || f.bill(t, billID(30)).Late {
		t.Fatal("only the document outside the cutoff is late")
	}
	d, _ := f.svc.GetBusinessDay(f.as(tenantA), dayID)
	if d.LateDocumentCount != 1 || d.Close == nil || d.Close.ZNumber != "Z/2026-11-24/P1" || d.RevisionCount != 0 {
		t.Fatalf("day %+v", d)
	}
	f.clk.Advance(time.Minute)
	if n, err := f.svc.FlushRevisions(f.sys(tenantA)); err != nil || n != 1 {
		t.Fatalf("flush %d %v", n, err)
	}
	revs, _ := f.svc.ListBusinessDayRevisions(f.as(tenantA), dayID, "", 10)
	if len(revs.Items) != 1 || revs.Items[0].DisplayNumber != "Z/2026-11-24/P1/R1" || revs.Items[0].AddedTotals.BillCount != 1 || revs.Items[0].Cause != "late_device" {
		t.Fatalf("revisions %+v", revs.Items)
	}
	// the original Z is untouched, and a further late document makes revision 2, never changing revision 1
	d, _ = f.svc.GetBusinessDay(f.as(tenantA), dayID)
	if d.Close.Totals.BillCount != 1 || d.RevisionCount != 1 {
		t.Fatalf("day after flush %+v", d)
	}
	f.apply(tenantA, deviceB, "outlet:"+outletA, billMuts(billID(32), "BBB/2627/000002", 1200, false)...)
	f.clk.Advance(time.Minute)
	_, _ = f.svc.FlushRevisions(f.sys(tenantA))
	revs, _ = f.svc.ListBusinessDayRevisions(f.as(tenantA), dayID, "", 10)
	if len(revs.Items) != 2 || revs.Items[0].DisplayNumber != "Z/2026-11-24/P1/R1" || revs.Items[1].DisplayNumber != "Z/2026-11-24/P1/R2" {
		t.Fatalf("revisions %+v", revs.Items)
	}
	if count(f.eventTypes(tenantA), "billing.trading_day.changed") != 2 { // open and closed
		t.Fatalf("trading day events: %v", f.eventTypes(tenantA))
	}
}

// ---- tenant isolation and authorisation ----

func TestTenantIsolation(t *testing.T) {
	f := newFixture(t)
	f.apply(tenantA, deviceA, "outlet:"+outletA, billMuts(billID(40), number(1), 1200, true)...)
	b := f.as(tenantB)
	if _, err := f.svc.GetBill(b, billID(40)); err == nil {
		t.Fatal("tenant B read tenant A's bill")
	} else {
		wantCode(t, err, "not_found")
	}
	for name, fn := range map[string]func() (int, error){
		"bills": func() (int, error) {
			p, err := f.svc.ListBills(b, "", service.BillFilter{}, "", 50)
			return len(p.Items), err
		},
		"payments": func() (int, error) { p, err := f.svc.ListPayments(b, "", "", "", "", "", 50); return len(p.Items), err },
		"register": func() (int, error) { p, err := f.svc.ListDocumentRegister(b, "", "", "", 50); return len(p.Items), err },
	} {
		if n, err := fn(); err != nil || n != 0 {
			t.Errorf("%s: tenant B sees %d rows (%v)", name, n, err)
		}
	}
	if _, err := f.svc.SetDiscountLimit(f.as(tenantB), "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a99", 500); err == nil {
		t.Fatal("tenant B changed an outlet it does not have")
	}
	if f.store.DocCount(tenantB, "bill") != 0 {
		t.Fatal("tenant B holds tenant A's rows")
	}
}

func TestOutletScopeLimitsLists(t *testing.T) {
	f := newFixture(t)
	f.apply(tenantA, deviceA, "outlet:"+outletA, billMuts(billID(41), number(1), 1200, false)...)
	other := "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a77"
	ctx := f.iss.ContextFor(context.Background(), f.cat, authtest.Spec{Type: auth.TypeUser, Tenant: ids.ID(tenantA), Grants: authtest.Grants([]string{other}, "report.view.outlet")})
	p, err := f.svc.ListBills(ctx, "", service.BillFilter{}, "", 50)
	if err != nil || len(p.Items) != 0 {
		t.Fatalf("a member of another outlet saw %d bills (%v)", len(p.Items), err)
	}
	if _, err := f.svc.ListBills(ctx, outletA, service.BillFilter{}, "", 50); err == nil {
		t.Fatal("selecting an outlet outside the grants must fail")
	}
	if _, err := f.svc.GetBill(ctx, billID(41)); err == nil {
		t.Fatal("reading a bill of another outlet must fail")
	}
}

// ---- strong-session settings ----

type fakeOrders struct{ n int }

func (o fakeOrders) OpenCheckCount(context.Context, string) (int, error) { return o.n, nil }

// fakeEmitter is the gateway as the service and the queue see it: it stores every op once per (tenant, key).
type fakeEmitter struct {
	ops     []emit.Op
	tenants []string
	fail    error
}

func (e *fakeEmitter) Emit(_ context.Context, tenant ids.ID, ops []emit.Op) ([]emit.Result, error) {
	if e.fail != nil {
		return nil, e.fail
	}
	var res []emit.Result
	for _, o := range ops {
		seq := int64(0)
		for i, x := range e.ops {
			if e.tenants[i] == tenant.String() && x.Key == o.Key {
				seq = int64(i + 1)
			}
		}
		replayed := seq != 0
		if !replayed {
			e.ops, e.tenants = append(e.ops, o), append(e.tenants, tenant.String())
			seq = int64(len(e.ops))
		}
		res = append(res, emit.Result{Key: o.Key, Origin: "cloud-1", Stream: o.Stream, Seq: seq, Replayed: replayed})
	}
	return res, nil
}

func TestNumberingModeIsRefusedWithOpenChecks(t *testing.T) {
	f := newFixture(t)
	em := &fakeEmitter{}
	svc := service.New(f.store, service.Options{Clock: f.clk, Log: slog.New(slog.DiscardHandler), Orders: fakeOrders{n: 2}, Emitter: em})
	_, err := svc.SetNumberingMode(f.as(tenantA), outletA, "print")
	wantCode(t, err, "open_checks_exist")
	if len(em.ops) != 0 {
		t.Fatal("an op was emitted although the change was refused")
	}
	svc = service.New(f.store, service.Options{Clock: f.clk, Log: slog.New(slog.DiscardHandler), Orders: fakeOrders{}, Emitter: em})
	ch, err := svc.SetNumberingMode(f.as(tenantA), outletA, "print")
	if err != nil || ch.OpId == "" || len(em.ops) != 1 || em.ops[0].Muts[0].F["number_at"] != "print" || em.ops[0].Intent != "settings.numbering.set" {
		t.Fatalf("%+v %v ops %d", ch, err, len(em.ops))
	}
}

func TestSpecifiedPremisesCannotChangeRetroactively(t *testing.T) {
	f := newFixture(t)
	em := &fakeEmitter{}
	svc := service.New(f.store, service.Options{Clock: f.clk, Log: slog.New(slog.DiscardHandler), Emitter: em})
	_, err := svc.SetOutletTaxProfile(f.as(tenantA), outletA, "2627", true, nil, nil)
	wantCode(t, err, "specified_premises_retroactive")
	if _, err := svc.SetOutletTaxProfile(f.as(tenantA), outletA, "2627", false, nil, nil); err != nil {
		t.Fatalf("an unchanged flag is accepted: %v", err)
	}
}
