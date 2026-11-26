package service_test

import (
	"testing"

	"github.com/hostryhq/svc-billing/service"
)

// Phase 2 review fixes (docs/reviews/phase-2-code-review.md, phase-2-security-review.md). Each test is the reviewer's
// probe, written to fail before the fix.

const outletStream = "outlet:" + outletA

func (f *fixture) cashTotals(t *testing.T) (day, tender int64) {
	t.Helper()
	rep, err := f.svc.DaySummary(f.as(tenantA), outletA, service.Range{From: "2026-11-24", To: "2026-11-24"}, "", 10)
	if err != nil || len(rep.Items) != 1 {
		t.Fatalf("day summary %v %v", rep, err)
	}
	for _, x := range rep.Items[0].Totals.Tenders {
		day += x.Amount.AmountPaise
	}
	tr, err := f.svc.TenderSummary(f.as(tenantA), outletA, service.Range{From: "2026-11-24", To: "2026-11-24"}, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range tr.Items {
		tender += x.Paid.AmountPaise
	}
	return
}

// CR-H1: the day's tender total must not depend on whether the payment reached billing before, with or after its bill.
func TestTenderTotalsDoNotDependOnArrivalOrder(t *testing.T) {
	pid := "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f9b01"
	cases := map[string]func(f *fixture){
		"bill then payment": func(f *fixture) {
			f.apply(tenantA, deviceA, outletStream, billMuts(billID(40), number(1), 1200, false)...)
			f.apply(tenantA, deviceB, outletStream, payMut(pid, billID(40), "cash", 50400))
		},
		"payment then bill": func(f *fixture) {
			f.apply(tenantA, deviceB, outletStream, payMut(pid, billID(40), "cash", 50400))
			f.apply(tenantA, deviceA, outletStream, billMuts(billID(40), number(1), 1200, false)...)
		},
		"bill with its payment": func(f *fixture) {
			f.apply(tenantA, deviceA, outletStream, billMuts(billID(40), number(1), 1200, true)...)
		},
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			run(f)
			if day, tender := f.cashTotals(t); day != 50400 || tender != 50400 {
				t.Fatalf("day tenders %d, tender report %d, want 50400 each", day, tender)
			}
			if b := f.bill(t, billID(40)); b.Paid.AmountPaise != 50400 || b.PaymentStatus != "paid" {
				t.Fatalf("bill paid %v %v", b.Paid, b.PaymentStatus)
			}
		})
	}
}
