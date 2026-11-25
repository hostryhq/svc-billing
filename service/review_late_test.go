package service_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/hostryhq/svc-billing/service"
)

// ---- lateness (CR-H3, CR-H4) ----

const closeID = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f7033"

// closeMutFor is the day_close fact of deviceA with its version vector and cutoff instant.
func closeMutFor(dayID string, cutoff map[string]int64, at time.Time, vector bool) service.Mut {
	c := fmt.Sprintf(`{"%s":%d}`, deviceA, cutoff[deviceA])
	if b, ok := cutoff[deviceB]; ok {
		c = fmt.Sprintf(`{"%s":%d,"%s":%d}`, deviceA, cutoff[deviceA], deviceB, b)
	}
	if !vector {
		c = at.UTC().Format(time.RFC3339Nano)
	}
	return service.Mut{E: "day_close", ID: closeID, K: "insert", F: map[string]any{"business_day_id": dayID, "z_number": "Z/2026-11-24/P1", "cutoff": c,
		"cutoff_at": at.UTC().Format(time.RFC3339Nano), "gross_paise": 0, "total_paise": 0, "bill_count": 0, "tenders_json": "[]", "series_ranges_json": "[]"}}
}

func (f *fixture) openDay(t *testing.T) string {
	t.Helper()
	f.apply(tenantA, deviceA, outletStream, closeMuts("2026-11-24", nil)...)
	days, _ := f.svc.ListBusinessDays(f.as(tenantA), outletA, "", "", 10)
	return days.Items[0].Id
}

// CR-H3: a bill outside the Z's vector is late whether it reached billing before or after the close.
func TestLatenessDoesNotDependOnWhetherTheBillOrTheCloseArrivedFirst(t *testing.T) {
	for _, billFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("bill first %v", billFirst), func(t *testing.T) {
			f := newFixture(t)
			dayID := f.openDay(t)
			cutoffAt := f.clk.Now()
			closeOp := func() {
				f.applyAt(deviceA, 50, f.clk.Now().UnixMilli(), "day.close", closeMutFor(dayID, map[string]int64{deviceA: 50}, cutoffAt, true))
			}
			f.clk.Advance(-time.Minute) // finalised before the close on its own clock, but never in the Z
			billOp := func() { f.applyAt(deviceB, 7, f.clk.Now().UnixMilli(), "bill.finalize", billMuts(billID(42), "BBB/2627/000001", 1200, true)...) }
			if billFirst {
				billOp()
				f.clk.Advance(time.Minute)
				closeOp()
			} else {
				f.clk.Advance(time.Minute)
				closeOp()
				f.clk.Advance(-time.Minute)
				billOp()
			}
			if !f.bill(t, billID(42)).Late {
				t.Fatal("the bill is outside the Z and not marked late")
			}
			d, _ := f.svc.GetBusinessDay(f.as(tenantA), dayID)
			if d.LateDocumentCount == 0 {
				t.Fatalf("day %+v has no late document", d)
			}
			f.clk.Advance(time.Hour)
			if n, err := f.svc.FlushRevisions(f.sys(tenantA)); err != nil || n != 1 {
				t.Fatalf("flush %d %v", n, err)
			}
			revs, _ := f.svc.ListBusinessDayRevisions(f.as(tenantA), dayID, "", 10)
			if len(revs.Items) != 1 || revs.Items[0].AddedTotals.BillCount != 1 {
				t.Fatalf("revisions %+v", revs.Items)
			}
			var tender int64
			for _, x := range revs.Items[0].AddedTotals.Tenders {
				tender += x.Amount.AmountPaise
			}
			if tender != 50400 {
				t.Fatalf("revision tenders %d, want the bill's payment 50400", tender)
			}
		})
	}
}

// CR-H4: the Z's version vector decides. A bill inside the vector is not late however far its device clock is ahead of the
// closing device's cutoff instant; the clock is the fallback only when the close carries no vector.
func TestVersionVectorDecidesBeforeTheDeviceClock(t *testing.T) {
	f := newFixture(t)
	dayID := f.openDay(t)
	cutoffAt := f.clk.Now()
	f.clk.Advance(7 * time.Minute) // counter B runs seven minutes fast: its bill's at is after cutoff_at
	f.applyAt(deviceB, 7, f.clk.Now().UnixMilli(), "bill.finalize", billMuts(billID(43), "BBB/2627/000001", 1200, false)...)
	f.applyAt(deviceA, 50, f.clk.Now().UnixMilli(), "day.close", closeMutFor(dayID, map[string]int64{deviceA: 50, deviceB: 7}, cutoffAt, true))
	if f.bill(t, billID(43)).Late {
		t.Fatal("a bill inside the Z's vector was marked late by a skewed clock")
	}
	// the same bill arriving after the close
	f.applyAt(deviceB, 8, f.clk.Now().UnixMilli(), "bill.finalize", billMuts(billID(44), "BBB/2627/000002", 1200, false)...)
	if !f.bill(t, billID(44)).Late {
		t.Fatal("a bill beyond the vector is late")
	}
	f.clk.Advance(time.Hour)
	if n, _ := f.svc.FlushRevisions(f.sys(tenantA)); n != 1 {
		t.Fatalf("flush %d", n)
	}
	revs, _ := f.svc.ListBusinessDayRevisions(f.as(tenantA), dayID, "", 10)
	if len(revs.Items) != 1 || revs.Items[0].AddedTotals.BillCount != 1 {
		t.Fatalf("revisions %+v", revs.Items)
	}
}

// CR-H4 fallback: with no vector in the close, the cutoff instant decides.
func TestCutoffInstantIsTheFallbackWhenTheCloseCarriesNoVector(t *testing.T) {
	f := newFixture(t)
	dayID := f.openDay(t)
	cutoffAt := f.clk.Now()
	f.applyAt(deviceA, 50, f.clk.Now().UnixMilli(), "day.close", closeMutFor(dayID, nil, cutoffAt, false))
	f.clk.Advance(time.Minute)
	f.applyAt(deviceB, 7, f.clk.Now().UnixMilli(), "bill.finalize", billMuts(billID(45), "BBB/2627/000001", 1200, false)...)
	if !f.bill(t, billID(45)).Late {
		t.Fatal("a bill after the cutoff instant is late")
	}
}

// M1: the id of a sealed revision follows from its key, so a restored billing reseals the same row.
func TestRevisionRowIDIsDerivedFromItsKey(t *testing.T) {
	seal := func(extra bool) string {
		f := newFixture(t)
		if extra {
			f.apply(tenantA, deviceB, outletStream, billMuts(billID(46), "CCC/2627/000001", 1200, true)...) // moves the id generator
		}
		dayID := f.openDay(t)
		cutoffAt := f.clk.Now()
		f.applyAt(deviceA, 50, f.clk.Now().UnixMilli(), "day.close", closeMutFor(dayID, map[string]int64{deviceA: 50}, cutoffAt, true))
		f.clk.Advance(time.Minute)
		f.applyAt(deviceB, 90, f.clk.Now().UnixMilli(), "bill.finalize", billMuts(billID(47), "BBB/2627/000001", 1200, true)...)
		f.clk.Advance(time.Minute)
		if n, err := f.svc.FlushRevisions(f.sys(tenantA)); err != nil || n != 1 {
			t.Fatalf("flush %d %v", n, err)
		}
		revs, _ := f.svc.ListBusinessDayRevisions(f.as(tenantA), dayID, "", 10)
		return revs.Items[0].Id
	}
	if a, b := seal(false), seal(true); a != b {
		t.Fatalf("revision ids differ across runs: %s %s", a, b)
	}
}
