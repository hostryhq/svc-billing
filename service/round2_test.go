package service_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/hostryhq/svc-billing/internal/gen"
	"github.com/hostryhq/svc-billing/service"
)

// nonGSTBillMuts is a bill of one Rs 480 taxed line (5 percent) and one Rs 60 non-GST line.
func nonGSTBillMuts(billID, number string) []service.Mut {
	ms := billMuts(billID, number, 1200, true)
	ms[0].F["gross_paise"], ms[0].F["non_gst_subtotal_paise"], ms[0].F["total_paise"] = 54000, 6000, 56400
	ms[len(ms)-1].F["amount_paise"] = 56400
	ms[0].F["ruleset_id"] = service.RuleSetRowID(tenantA, 2)
	return append(ms,
		service.Mut{E: "bill_line", ID: billID[:30] + "0004", K: "insert", F: map[string]any{"bill_id": billID, "line_no": 2, "item_name": "Water", "qty_milli": 1000,
			"unit_price_paise": 6000, "net_paise": 6000, "tax_class": "fnb_non_gst"}},
		service.Mut{E: "bill_tax_line", ID: billID[:30] + "0005", K: "insert", F: map[string]any{"bill_id": billID, "tax_class": "fnb_non_gst", "rate_bp": 0, "gst_applies": false,
			"charge_alloc_paise": 0, "taxable_paise": 6000, "cgst_paise": 0, "sgst_paise": 0, "igst_paise": 0}})
}

// A credit note of a non-GST line is verified: the non-GST amount is not taxable value (F19).
func TestCreditNoteOfNonGSTLineIsVerified(t *testing.T) {
	f := newFixture(t)
	rs := append(f.ruleSetMuts(tenantA, 2, "2026-11-01", 250), service.Mut{E: "tax_rule", ID: "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f9099", K: "insert", F: map[string]any{"rule_set_id": service.RuleSetRowID(tenantA, 2),
		"tax_class": "fnb_non_gst", "regime": "regular", "specified_premises": false, "order_channel": "dine_in", "doc_type": "tax_invoice", "gst_applies": false, "cgst_bp": 0, "sgst_bp": 0, "igst_bp": 0}})
	f.apply(tenantA, "cloud-1", "tenant", rs...)
	f.apply(tenantA, deviceA, "outlet:"+outletA, nonGSTBillMuts(billID(72), number(1))...)
	if st := f.bill(t, billID(72)).Verification.Status; st != gen.VerificationStatusVerified {
		t.Fatalf("bill verification %v %+v", st, f.bill(t, billID(72)).Verification.Difference.AmountPaise)
	}
	f.apply(tenantA, deviceA, "outlet:"+outletA,
		service.Mut{E: "credit_note", ID: creditID, K: "insert", F: map[string]any{"number": "AAA/2627/CN0001", "series_code": "AAA", "bill_id": billID(72), "bill_number": number(1), "bill_invoice_date": "2026-11-24",
			"business_date": "2026-11-24", "invoice_date": "2026-11-24", "financial_year": "2627", "calc_v": 1, "ruleset_id": service.RuleSetRowID(tenantA, 2), "total_paise": 6000, "round_off_paise": 0}},
		service.Mut{E: "credit_note_line", ID: creditID[:30] + "0001", K: "insert", F: map[string]any{"credit_note_id": creditID, "line_no": 1, "bill_line_id": billID(72)[:30] + "0004", "item_name": "Water", "qty_milli": 1000, "net_paise": 6000, "tax_class": "fnb_non_gst"}},
		service.Mut{E: "credit_note_tax_line", ID: creditID[:30] + "0002", K: "insert", F: map[string]any{"credit_note_id": creditID, "tax_class": "fnb_non_gst", "rate_bp": 0, "gst_applies": false,
			"charge_alloc_paise": 0, "taxable_paise": 6000, "cgst_paise": 0, "sgst_paise": 0, "igst_paise": 0}})
	if n := f.creditNote(t); n.Verification.Status != gen.VerificationStatusVerified {
		t.Fatalf("credit verification %+v", n.Verification)
	}
	if ex := f.exceptions("tax_mismatch"); len(ex) != 0 {
		t.Fatalf("exceptions %+v", ex)
	}
}

// A bill finalised before the cutoff on a device the Z did not hold (partitioned) lands in a revision (F22).
func TestBillBeforeCutoffNotCoveredByTheZIsLate(t *testing.T) {
	f := newFixture(t)
	f.apply(tenantA, deviceA, "outlet:"+outletA, closeMuts("2026-11-24", nil)...)
	days, _ := f.svc.ListBusinessDays(f.as(tenantA), outletA, "", "", 10)
	dayID := days.Items[0].Id
	before := f.clk.Now()
	f.clk.Advance(time.Minute)
	f.apply(tenantA, deviceA, "outlet:"+outletA, service.Mut{E: "day_close", ID: "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f7033", K: "insert", F: map[string]any{"business_day_id": dayID,
		"z_number": "Z/2026-11-24/P1", "cutoff": fmt.Sprintf(`{"%s":2}`, deviceA), "cutoff_at": f.clk.Now().UTC().Format(time.RFC3339Nano), "total_paise": 0, "bill_count": 0, "tenders_json": "[]", "series_ranges_json": "[]"}})
	f.clk.Advance(time.Minute)
	op := service.Op{Tenant: tenantA, Origin: deviceB, Stream: "outlet:" + outletA, Seq: 1, Hash: "hb", HLC: fmt.Sprintf("%016x", before.UnixMilli()<<16),
		Intent: "test", At: before, Actor: member, Muts: int64s(billMuts(billID(73), "BBB/2627/000001", 1200, true))}
	if err := f.svc.Project(f.sys(tenantA), op); err != nil {
		t.Fatal(err)
	}
	if !f.bill(t, billID(73)).Late {
		t.Fatal("a bill the Z did not hold must be late though finalised before cutoff_at")
	}
	f.clk.Advance(time.Minute)
	if n, err := f.svc.FlushRevisions(f.sys(tenantA)); err != nil || n != 1 {
		t.Fatalf("flush %d %v", n, err)
	}
}

// Pages of a manual book are never a gap: only the pages typed are in the register (O2-18, F18).
func TestManualBookNeverOpensAGap(t *testing.T) {
	f := newFixture(t)
	ms := billMuts(billID(74), "MB1/2627/000007", 1200, true)
	ms[0].F["source_kind"], ms[0].F["series_code"] = "manual_book", "MB1"
	f.apply(tenantA, deviceA, "outlet:"+outletA, ms...)
	gaps, _ := f.svc.ListDocumentGaps(f.as(tenantA), "", "", "", 50)
	if len(gaps.Items) != 0 {
		t.Fatalf("gaps %+v", gaps.Items)
	}
	if ex := f.exceptions("manual_book_missing_number"); len(ex) != 0 {
		t.Fatalf("exceptions %+v", ex)
	}
}
