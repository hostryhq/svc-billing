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
