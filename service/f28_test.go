package service_test

import (
	"testing"

	"github.com/hostryhq/svc-billing/service"
)

// F28: a second full credit note of one bill is flagged, and must not reduce the output tax a second time.
func TestOverCreditDoesNotReduceTaxBeyondTheBill(t *testing.T) {
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
			rep, err := f.svc.TaxSummary(f.as(tenantA), outletA, service.Range{From: "2026-11-24", To: "2026-11-24"}, "", 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(rep.Items) == 0 {
				t.Fatal("no rows")
			}
			for _, r := range rep.Items {
				if r.CreditTaxable.AmountPaise > r.BillTaxable.AmountPaise || r.CreditCgst.AmountPaise > r.BillCgst.AmountPaise || r.CreditSgst.AmountPaise > r.BillSgst.AmountPaise {
					t.Fatalf("credit exceeds bill in the tax summary: %+v", r)
				}
			}
		})
	}
}

// RM1: the capped reduction goes to the earlier note by invoice date (then number, then id), whatever the arrival order,
// so it lands in the same GST period across a month boundary.
func TestCappedCreditTaxLandsOnTheSameDateInAnyArrivalOrder(t *testing.T) {
	early := func() []service.Mut { return fullCreditMuts(cn1, "AAA/2627/CN0001", billID(41), 2000, 48000, 1200) }
	late := func() []service.Mut {
		ms := fullCreditMuts(cn2, "BBB/2627/CN0001", billID(41), 2000, 48000, 1200)
		ms[0].F["invoice_date"] = "2026-12-01"
		return ms
	}
	orders := map[string]func(f *fixture){
		"early, late, bill": func(f *fixture) {
			f.apply(tenantA, deviceA, outletStream, early()...)
			f.apply(tenantA, deviceB, outletStream, late()...)
			f.apply(tenantA, deviceA, outletStream, billMuts(billID(41), number(1), 1200, true)...)
		},
		"late, early, bill": func(f *fixture) {
			f.apply(tenantA, deviceB, outletStream, late()...)
			f.apply(tenantA, deviceA, outletStream, early()...)
			f.apply(tenantA, deviceA, outletStream, billMuts(billID(41), number(1), 1200, true)...)
		},
		"bill, early, late": func(f *fixture) {
			f.apply(tenantA, deviceA, outletStream, billMuts(billID(41), number(1), 1200, true)...)
			f.apply(tenantA, deviceA, outletStream, early()...)
			f.apply(tenantA, deviceB, outletStream, late()...)
		},
		"bill, late, early": func(f *fixture) {
			f.apply(tenantA, deviceA, outletStream, billMuts(billID(41), number(1), 1200, true)...)
			f.apply(tenantA, deviceB, outletStream, late()...)
			f.apply(tenantA, deviceA, outletStream, early()...)
		},
	}
	for name, run := range orders {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			run(f)
			credit := func(from, to string) (cgst, taxable int64) {
				rep, err := f.svc.TaxSummary(f.as(tenantA), outletA, service.Range{From: from, To: to}, "", 10)
				if err != nil {
					t.Fatal(err)
				}
				for _, r := range rep.Items {
					cgst += r.CreditCgst.AmountPaise
					taxable += r.CreditTaxable.AmountPaise
				}
				return
			}
			if c, x := credit("2026-11-24", "2026-11-24"); c != 1200 || x != 48000 {
				t.Fatalf("24 Nov credit cgst %d taxable %d, want 1200 and 48000", c, x)
			}
			if c, x := credit("2026-12-01", "2026-12-01"); c != 0 || x != 0 {
				t.Fatalf("1 Dec credit cgst %d taxable %d, want none", c, x)
			}
		})
	}
}

// F30: in print mode the bill is numbered at Present with no payment, and a later payment.record op carries the split
// tender. Each payment is counted once in the tender totals, and a replay of the op changes nothing.
func TestPrintModeSplitPaymentIsCountedOnce(t *testing.T) {
	f := newFixture(t)
	p1, p2 := "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f9b11", "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f9b12"
	f.apply(tenantA, deviceA, outletStream, billMuts(billID(42), number(1), 1200, false)...)
	op := f.apply(tenantA, deviceA, outletStream, payMut(p1, billID(42), "cash", 25200), payMut(p2, billID(42), "card", 25200))
	if err := f.svc.Project(f.sys(tenantA), op); err != nil {
		t.Fatal(err)
	}
	if day, tender := f.cashTotals(t); day != 50400 || tender != 50400 {
		t.Fatalf("day tenders %d, tender report %d, want 50400 each", day, tender)
	}
}
