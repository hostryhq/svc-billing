package service_test

import (
	"testing"
	"time"

	"github.com/hostryhq/svc-billing/internal/gen"
	"github.com/hostryhq/svc-billing/service"
)

// withOrderLines gives each bill_line of muts an order_line_id, as a bill finalised from a check carries.
func withOrderLines(muts []service.Mut, lineID string) []service.Mut {
	out := make([]service.Mut, len(muts))
	copy(out, muts)
	for i, m := range out {
		if m.E == "bill_line" {
			g := map[string]any{}
			for k, v := range m.F {
				g[k] = v
			}
			g["order_line_id"] = lineID
			out[i].F = g
		}
	}
	return out
}

func payMut(id, billID, tender string, amount int) service.Mut {
	return service.Mut{E: "payment", ID: id, K: "insert", F: map[string]any{"bill_id": billID, "target_kind": "bill", "tender": tender, "amount_paise": amount, "business_date": "2026-11-24"}}
}

// A bill that came from an order line is projected (the line claim is a stored document with a body, as docs.body is
// NOT NULL), and the same line on a second bill is flagged, never rejected.
func TestBillFromOrderLineProjectsAndASecondClaimIsFlagged(t *testing.T) {
	f := newFixture(t)
	const line = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f6a01"
	f.apply(tenantA, deviceA, "outlet:"+outletA, withOrderLines(billMuts(billID(50), number(1), 1200, true), line)...)
	f.apply(tenantA, deviceB, "outlet:"+outletA, withOrderLines(billMuts(billID(51), "BBB/2627/000001", 1200, true), line)...)
	if ex := f.exceptions("line_on_two_bills"); len(ex) != 1 || ex[0].SubjectId != billID(51) {
		t.Fatalf("exceptions %+v", ex)
	}
}

// The seeded double payment: part with the bill, the rest recorded twice by two devices that could not hear each
// other. The detector fires when the second arrives, not before.
func TestSplitPaymentRecordedTwiceRaisesPaymentsExceedTotal(t *testing.T) {
	f := newFixture(t)
	ms := append(billMuts(billID(52), number(1), 1200, false), payMut("0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f9b01", billID(52), "cash", 25200))
	f.apply(tenantA, deviceA, "outlet:"+outletA, ms...)
	f.apply(tenantA, deviceA, "outlet:"+outletA, payMut("0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f9b02", billID(52), "upi", 25200))
	if ex := f.exceptions("payments_exceed_total"); len(ex) != 0 {
		t.Fatalf("an exact payment raised %+v", ex)
	}
	f.apply(tenantA, deviceB, "outlet:"+outletA, payMut("0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f9b03", billID(52), "cash", 25200))
	if ex := f.exceptions("payments_exceed_total"); len(ex) != 1 || ex[0].SubjectId != billID(52) {
		t.Fatalf("exceptions %+v", ex)
	}
}

// Payments that reach billing before their bill are counted when the bill arrives, and the overpayment is found then.
func TestOverpaymentArrivingBeforeItsBillIsFound(t *testing.T) {
	f := newFixture(t)
	f.apply(tenantA, deviceB, "outlet:"+outletA, payMut("0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f9c01", billID(53), "cash", 50400))
	f.apply(tenantA, deviceB, "outlet:"+outletA, payMut("0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f9c02", billID(53), "upi", 50400))
	f.apply(tenantA, deviceA, "outlet:"+outletA, billMuts(billID(53), number(1), 1200, false)...)
	if ex := f.exceptions("payments_exceed_total"); len(ex) != 1 {
		t.Fatalf("exceptions %+v", ex)
	}
}

// The day-close cutoff is the moment the closing device printed the Z, as an RFC 3339 timestamp: a document made after
// it is late and becomes a Z revision; one made before it is not, whatever device sent it.
func TestCutoffAsTimestampMarksLateDocumentsAndSealsARevision(t *testing.T) {
	f := newFixture(t)
	f.apply(tenantA, deviceA, "outlet:"+outletA, closeMuts("2026-11-24", nil)...)
	f.apply(tenantA, deviceA, "outlet:"+outletA, billMuts(billID(60), number(1), 1200, true)...)
	days, _ := f.svc.ListBusinessDays(f.as(tenantA), outletA, "", "", 10)
	dayID := days.Items[0].Id
	cutoff := f.clk.Now().UTC().Format(time.RFC3339Nano)
	f.apply(tenantA, deviceA, "outlet:"+outletA, service.Mut{E: "day_close", ID: "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f7033", K: "insert", F: map[string]any{"business_day_id": dayID,
		"z_number": "Z/2026-11-24/P1", "cutoff": cutoff, "gross_paise": 48000, "total_paise": 50400, "bill_count": 1, "tenders_json": "[]", "series_ranges_json": "[]"}})
	f.clk.Advance(20 * time.Minute)
	f.apply(tenantA, deviceB, "outlet:"+outletA, billMuts(billID(61), "BBB/2627/000001", 1200, true)...)
	if !f.bill(t, billID(61)).Late || f.bill(t, billID(60)).Late {
		t.Fatal("only the document made after the cutoff is late")
	}
	f.clk.Advance(time.Minute)
	if n, err := f.svc.FlushRevisions(f.sys(tenantA)); err != nil || n != 1 {
		t.Fatalf("flush %d %v", n, err)
	}
	d, _ := f.svc.GetBusinessDay(f.as(tenantA), dayID)
	if d.RevisionCount != 1 || d.LateDocumentCount != 1 {
		t.Fatalf("day %+v", d)
	}
}

// A credit note that matches the recompute of the original bill is verified, not left pending or flagged.
func TestCreditNoteIsVerifiedWhenCorrect(t *testing.T) {
	f := newFixture(t)
	f.apply(tenantA, deviceA, "outlet:"+outletA, twoLineBillMuts(billID(70), number(1))...)
	f.apply(tenantA, deviceA, "outlet:"+outletA, creditMuts(creditID, billID(70), "AAA/2627/CN0001", billID(70)[:30]+"0004")...)
	if n := f.creditNote(t); n.Verification.Status != gen.VerificationStatusVerified {
		t.Fatalf("verification %+v", n.Verification)
	}
}

// A credit note that reaches billing before its bill is verified when the bill arrives.
func TestCreditNoteBeforeItsBillIsVerifiedWhenTheBillArrives(t *testing.T) {
	f := newFixture(t)
	f.apply(tenantA, deviceB, "outlet:"+outletA, creditMuts(creditID, billID(71), "AAA/2627/CN0001", billID(71)[:30]+"0004")...)
	if n := f.creditNote(t); n.Verification.Status != gen.VerificationStatusPending {
		t.Fatalf("verification %+v", n.Verification)
	}
	f.apply(tenantA, deviceA, "outlet:"+outletA, twoLineBillMuts(billID(71), number(1))...)
	if n := f.creditNote(t); n.Verification.Status != gen.VerificationStatusVerified {
		t.Fatalf("verification after the bill %+v", n.Verification)
	}
}

// twoLineBillMuts is billMuts with its 2 x Rs 240 split into two lines of 1 x Rs 240 (same totals).
func twoLineBillMuts(billID, number string) []service.Mut {
	ms := billMuts(billID, number, 1200, true)
	for i, m := range ms {
		if m.E != "bill_line" {
			continue
		}
		g := map[string]any{}
		for k, v := range m.F {
			g[k] = v
		}
		g["qty_milli"], g["net_paise"] = 1000, 24000
		ms[i].F = g
		second := map[string]any{}
		for k, v := range g {
			second[k] = v
		}
		second["line_no"] = 2
		return append(ms, service.Mut{E: "bill_line", ID: billID[:30] + "0004", K: "insert", F: second})
	}
	return ms
}

// creditMuts credits one line (Rs 240 + 5 percent = Rs 252) of a two-line bill; the note has one line, its number 1,
// though it credits the bill's second line.
func creditMuts(creditID, billID, number, billLine string) []service.Mut {
	return []service.Mut{
		{E: "credit_note", ID: creditID, K: "insert", F: map[string]any{"number": number, "series_code": "AAA", "bill_id": billID, "bill_number": "AAA/2627/000001", "bill_invoice_date": "2026-11-24",
			"business_date": "2026-11-24", "invoice_date": "2026-11-24", "financial_year": "2627", "calc_v": 1, "ruleset_id": service.RuleSetRowID(tenantA, 1), "total_paise": 25200, "round_off_paise": 0}},
		{E: "credit_note_line", ID: creditID[:30] + "0001", K: "insert", F: map[string]any{"credit_note_id": creditID, "line_no": 1, "bill_line_id": billLine, "item_name": "Paneer Tikka", "qty_milli": 1000, "net_paise": 24000, "tax_class": "fnb_standard"}},
		{E: "credit_note_tax_line", ID: creditID[:30] + "0002", K: "insert", F: map[string]any{"credit_note_id": creditID, "tax_class": "fnb_standard", "rate_bp": 500, "gst_applies": true,
			"charge_alloc_paise": 0, "taxable_paise": 24000, "cgst_paise": 600, "sgst_paise": 600, "igst_paise": 0}},
	}
}

const creditID = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f7c01"

func (f *fixture) creditNote(t *testing.T) gen.CreditNote {
	t.Helper()
	n, err := f.svc.GetCreditNote(f.as(tenantA), creditID)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
