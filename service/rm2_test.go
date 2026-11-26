package service_test

import (
	"testing"
	"time"
)

// RM2: a bill counted late under the higher-stamp close is un-counted when the lower-stamp close, whose vector covers it,
// becomes the canonical one: Z plus revisions must not count it twice.
func TestSwappingInAnEarlierCloseUncountsDocumentsItCovers(t *testing.T) {
	f := newFixture(t)
	dayID := f.openDay(t)
	now := f.clk.Now()
	f.applyAt(deviceB, 7, now.UnixMilli(), "bill.finalize", billMuts(billID(42), "BBB/2627/000001", 1200, true)...)
	missing := closeMutFor(dayID, map[string]int64{deviceA: 50}, now, true)
	f.applyAt(deviceA, 50, now.Add(time.Hour).UnixMilli(), "day.close", missing)
	if !f.bill(t, billID(42)).Late {
		t.Fatal("setup: the bill is outside the first close and must be late")
	}
	covering := closeMutFor(dayID, map[string]int64{deviceA: 51, deviceB: 7}, now, true)
	covering.ID = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f7034"
	f.applyAt(deviceA, 51, now.UnixMilli(), "day.close", covering)
	if f.bill(t, billID(42)).Late {
		t.Fatal("the canonical close covers the bill; it is still marked late")
	}
	d, _ := f.svc.GetBusinessDay(f.as(tenantA), dayID)
	if d.LateDocumentCount != 0 {
		t.Fatalf("late document count %d, want 0", d.LateDocumentCount)
	}
	f.clk.Advance(2 * time.Hour)
	if n, err := f.svc.FlushRevisions(f.sys(tenantA)); err != nil || n != 0 {
		t.Fatalf("flush %d %v: a revision was sealed for a bill the Z covers", n, err)
	}
}
