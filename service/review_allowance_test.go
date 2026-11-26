package service_test

import (
	"fmt"
	"testing"

	"github.com/hostryhq/svc-billing/service"
)

// P2-H2: detectors for the re-bill and comp allowances (Phase 1 security ruling 8). The device checks first; these fire
// when a modified client skips the check.

const rebillGroup = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f9d01"

func rebillID(n int) string { return fmt.Sprintf("0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f9e%02d", n) }

// rebillOp is one bill.rebill op: a full credit note of orig plus one replacement bill whose CGST is cgst each (1200 keeps
// the value, less takes value away).
func (f *fixture) rebill(t *testing.T, n int, orig string, replacement string, cgst int) {
	t.Helper()
	cn := rebillID(n)
	ms := fullCreditMuts(cn, fmt.Sprintf("AAA/2627/CN%04d", n), orig, 2000, 48000, 1200)
	ms[0].F["rebill_group_id"] = fmt.Sprintf("0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f9d%02d", n)
	bm := billMuts(replacement, number(100+n), cgst, false)
	bm[0].F["rebill_group_id"] = ms[0].F["rebill_group_id"]
	f.apply(tenantA, deviceA, outletStream, append(ms, bm...)...)
}

func (f *fixture) setLimits(rebills int, compPaise int) {
	f.applyAt("cloud-1", 90, 5000, "settings.limits.set", service.Mut{E: "billing_settings", ID: settingsRow(), K: "insert",
		F: map[string]any{"number_at": "settle", "rebill_allowance_per_shift": rebills, "comp_allowance_paise": compPaise}})
}

func TestRebillOfAPaidBillIsFlaggedWhichEverArrivesLast(t *testing.T) {
	t.Run("payment before the re-bill", func(t *testing.T) {
		f := newFixture(t)
		f.apply(tenantA, deviceA, outletStream, billMuts(billID(60), number(1), 1200, true)...)
		f.rebill(t, 1, billID(60), billID(61), 1200)
		if ex := f.exceptions("rebill_after_payment"); len(ex) != 1 || ex[0].Severity != "high" {
			t.Fatalf("exceptions %+v", ex)
		}
	})
	t.Run("payment after the re-bill", func(t *testing.T) {
		f := newFixture(t)
		f.apply(tenantA, deviceA, outletStream, billMuts(billID(60), number(1), 1200, false)...)
		f.rebill(t, 1, billID(60), billID(61), 1200)
		if ex := f.exceptions("rebill_after_payment"); len(ex) != 0 {
			t.Fatalf("an unpaid bill was flagged: %+v", ex)
		}
		f.apply(tenantA, deviceB, outletStream, payMut("0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f9f01", billID(60), "cash", 50400))
		if ex := f.exceptions("rebill_after_payment"); len(ex) != 1 {
			t.Fatalf("exceptions %+v", ex)
		}
	})
}

func TestValueReducingRebillIsFlaggedAndTheSecondOneInAShiftRaisesTheOwnerAlert(t *testing.T) {
	f := newFixture(t)
	f.apply(tenantA, deviceA, outletStream, billMuts(billID(62), number(1), 1200, false)...)
	f.apply(tenantA, deviceA, outletStream, billMuts(billID(63), number(2), 1200, false)...)
	// a value-neutral re-bill (same total) raises nothing
	f.rebill(t, 1, billID(62), billID(64), 1200)
	for _, k := range []string{"rebill_value_reduced", "rebill_value_reduction_repeated", "rebill_allowance_exceeded"} {
		if ex := f.exceptions(k); len(ex) != 0 {
			t.Fatalf("%s: a value-neutral re-bill was flagged: %+v", k, ex)
		}
	}
	// first value-reducing re-bill by the member in the shift: recorded; the second raises the repeated alert
	f.rebill(t, 2, billID(63), billID(65), 0)
	if ex := f.exceptions("rebill_value_reduced"); len(ex) != 1 {
		t.Fatalf("value reduced %+v", ex)
	}
	if ex := f.exceptions("rebill_value_reduction_repeated"); len(ex) != 0 {
		t.Fatalf("alert after the first: %+v", ex)
	}
	f.apply(tenantA, deviceA, outletStream, billMuts(billID(66), number(3), 1200, false)...)
	f.rebill(t, 3, billID(66), billID(67), 0)
	if ex := f.exceptions("rebill_value_reduction_repeated"); len(ex) != 1 || ex[0].Severity != "high" {
		t.Fatalf("repeated %+v", ex)
	}
}

func TestRebillsAboveThePerShiftAllowanceAreFlagged(t *testing.T) {
	f := newFixture(t)
	f.setLimits(2, 50000)
	for i := 1; i <= 3; i++ {
		f.apply(tenantA, deviceA, outletStream, billMuts(billID(70+i), number(i), 1200, false)...)
		f.rebill(t, i, billID(70+i), billID(80+i), 1200)
		want := 0
		if i == 3 {
			want = 1
		}
		if ex := f.exceptions("rebill_allowance_exceeded"); len(ex) != want {
			t.Fatalf("after %d re-bills: %+v", i, ex)
		}
	}
}

func TestCompValueAboveTheMemberAllowanceIsFlaggedFromLineSnapshots(t *testing.T) {
	f := newFixture(t)
	f.setLimits(3, 30000)
	comp := func(id string, n int) {
		ms := billMuts(id, number(n), 1200, false)
		ms[1].F["comp"] = true // 2 x Rs 240 = 48000 list value
		f.apply(tenantA, deviceA, outletStream, ms...)
	}
	comp(billID(90), 1)
	if ex := f.exceptions("comp_allowance_exceeded"); len(ex) != 1 || ex[0].SubjectId != billID(90) {
		t.Fatalf("a comp of 48000 against 30000: %+v", ex)
	}
	f2 := newFixture(t)
	f2.setLimits(3, 60000)
	ms := billMuts(billID(91), number(1), 1200, false)
	ms[1].F["comp"] = true
	f2.apply(tenantA, deviceA, outletStream, ms...)
	if ex := f2.exceptions("comp_allowance_exceeded"); len(ex) != 0 {
		t.Fatalf("within the allowance: %+v", ex)
	}
}
