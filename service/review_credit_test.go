package service_test

import (
	"testing"

	"github.com/hostryhq/svc-billing/internal/gen"
	"github.com/hostryhq/svc-billing/service"
)

// ---- credit cap (CR-H2) ----

func fullCreditMuts(id, num, bill string, qty, net, cgst int) []service.Mut {
	return []service.Mut{
		{E: "credit_note", ID: id, K: "insert", F: map[string]any{"number": num, "series_code": "AAA", "bill_id": bill, "bill_number": number(1), "bill_invoice_date": "2026-11-24",
			"business_date": "2026-11-24", "invoice_date": "2026-11-24", "financial_year": "2627", "calc_v": 1, "ruleset_id": service.RuleSetRowID(tenantA, 1), "total_paise": net + 2*cgst, "round_off_paise": 0}},
		{E: "credit_note_line", ID: id[:30] + "0001", K: "insert", F: map[string]any{"credit_note_id": id, "line_no": 1, "bill_line_id": bill[:30] + "0001", "item_name": "Paneer Tikka", "qty_milli": qty, "net_paise": net, "tax_class": "fnb_standard"}},
		{E: "credit_note_tax_line", ID: id[:30] + "0002", K: "insert", F: map[string]any{"credit_note_id": id, "tax_class": "fnb_standard", "rate_bp": 500, "gst_applies": true,
			"charge_alloc_paise": 0, "taxable_paise": net, "cgst_paise": cgst, "sgst_paise": cgst, "igst_paise": 0}},
	}
}

const (
	cn1 = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f9c01"
	cn2 = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f9c11"
)

func TestSecondFullCreditNoteOfOneBillIsFlaggedInAnyOrder(t *testing.T) {
	orders := map[string]func(f *fixture){
		"bill first": func(f *fixture) {
			f.apply(tenantA, deviceA, outletStream, billMuts(billID(41), number(1), 1200, true)...)
			f.apply(tenantA, deviceA, outletStream, fullCreditMuts(cn1, "AAA/2627/CN0001", billID(41), 2000, 48000, 1200)...)
			f.apply(tenantA, deviceB, outletStream, fullCreditMuts(cn2, "BBB/2627/CN0001", billID(41), 2000, 48000, 1200)...)
		},
		"notes first": func(f *fixture) {
			f.apply(tenantA, deviceA, outletStream, fullCreditMuts(cn1, "AAA/2627/CN0001", billID(41), 2000, 48000, 1200)...)
			f.apply(tenantA, deviceB, outletStream, fullCreditMuts(cn2, "BBB/2627/CN0001", billID(41), 2000, 48000, 1200)...)
			f.apply(tenantA, deviceA, outletStream, billMuts(billID(41), number(1), 1200, true)...)
		},
	}
	for name, run := range orders {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			run(f)
			if ex := f.exceptions("credit_exceeds_bill"); len(ex) != 1 || ex[0].SubjectId != billID(41) || ex[0].Severity != gen.High {
				t.Fatalf("exceptions %+v", ex)
			}
			if got := f.bill(t, billID(41)).Credited.AmountPaise; got != 100800 {
				t.Fatalf("credited %d, want the 100800 that was issued, stated rather than hidden", got)
			}
		})
	}
}

// Two partial notes that together stay inside the bill are not an over-credit.
func TestPartialCreditNotesWithinTheBillAreNotFlagged(t *testing.T) {
	f := newFixture(t)
	f.apply(tenantA, deviceA, outletStream, billMuts(billID(41), number(1), 1200, true)...)
	f.apply(tenantA, deviceA, outletStream, fullCreditMuts(cn1, "AAA/2627/CN0001", billID(41), 1000, 24000, 600)...)
	f.apply(tenantA, deviceB, outletStream, fullCreditMuts(cn2, "BBB/2627/CN0001", billID(41), 1000, 24000, 600)...)
	if ex := f.exceptions("credit_exceeds_bill"); len(ex) != 0 {
		t.Fatalf("two halves flagged: %+v", ex)
	}
}
