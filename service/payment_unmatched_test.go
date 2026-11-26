package service_test

import (
	"testing"

	"github.com/hostryhq/svc-billing/internal/gen"
)

// O2-22: a payment whose bill billing never accepted stays in the tender totals (the cash is real) and is flagged
// payment_unmatched; the finding closes by itself when the bill is accepted, whichever arrived first.
func TestPaymentWithoutAnAcceptedBillIsFlaggedAndClearedWhenTheBillArrives(t *testing.T) {
	const pid = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f9b21"
	t.Run("bill never comes", func(t *testing.T) {
		f := newFixture(t)
		f.apply(tenantA, deviceB, outletStream, payMut(pid, billID(43), "cash", 50400))
		ex := f.exceptions("payment_unmatched")
		if len(ex) != 1 || ex[0].SubjectId != pid || ex[0].Status != gen.ExceptionStatusOpen {
			t.Fatalf("exceptions %+v", ex)
		}
		if day, tender := f.cashTotals(t); day != 50400 || tender != 50400 {
			t.Fatalf("the cash is real: day %d tender %d", day, tender)
		}
	})
	t.Run("payment then bill", func(t *testing.T) {
		f := newFixture(t)
		f.apply(tenantA, deviceB, outletStream, payMut(pid, billID(43), "cash", 50400))
		f.apply(tenantA, deviceA, outletStream, billMuts(billID(43), number(1), 1200, false)...)
		ex := f.exceptions("payment_unmatched")
		if len(ex) != 1 || ex[0].Status != gen.ExceptionStatusResolved {
			t.Fatalf("exceptions %+v", ex)
		}
	})
	t.Run("bill then payment", func(t *testing.T) {
		f := newFixture(t)
		f.apply(tenantA, deviceA, outletStream, billMuts(billID(43), number(1), 1200, false)...)
		f.apply(tenantA, deviceB, outletStream, payMut(pid, billID(43), "cash", 50400))
		if ex := f.exceptions("payment_unmatched"); len(ex) != 0 {
			t.Fatalf("a payment of a known bill was flagged: %+v", ex)
		}
	})
}
