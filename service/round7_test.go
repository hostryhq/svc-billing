package service_test

import (
	"fmt"
	"testing"

	"github.com/hostryhq/svc-billing/internal/gen"
	"github.com/hostryhq/svc-billing/service"
)

// Round 7: bill.rebill.approved is a re-bill covered by a manager's approval. It counts toward the value-reduction detectors
// but not toward the per-shift allowance count, and replacement_total_paise is an ordinary field of the digest.

// rebillAs is rebill taken under the given intent; replacementTotal, when non-zero, is written on the credit note.
func (f *fixture) rebillAs(t *testing.T, intent string, n int, orig, replacement string, cgst int, replacementTotal int64) {
	t.Helper()
	cn := rebillID(n)
	ms := fullCreditMuts(cn, fmt.Sprintf("AAA/2627/CN%04d", n), orig, 2000, 48000, 1200)
	ms[0].F["rebill_group_id"] = fmt.Sprintf("0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f9d%02d", n)
	if replacementTotal != 0 {
		ms[0].F["replacement_total_paise"] = replacementTotal
	}
	bm := billMuts(replacement, number(100+n), cgst, false)
	bm[0].F["rebill_group_id"] = ms[0].F["rebill_group_id"]
	k := tenantA + deviceA + outletStream
	f.seq[k]++
	op := service.Op{Tenant: tenantA, Origin: deviceA, Stream: outletStream, Seq: f.seq[k], Hash: fmt.Sprintf("h%d", f.seq[k]), HLC: fmt.Sprintf("%016x", f.clk.Now().UnixMilli()<<16),
		Intent: intent, At: f.clk.Now(), Actor: member, Muts: int64s(append(ms, bm...))}
	if err := f.svc.Project(f.sys(tenantA), op); err != nil {
		t.Fatal(err)
	}
}

func TestApprovedRebillsDoNotUseUpThePerShiftAllowance(t *testing.T) {
	f := newFixture(t)
	f.setLimits(1, 50000)
	for i := 1; i <= 4; i++ {
		f.apply(tenantA, deviceA, outletStream, billMuts(billID(70+i), number(i), 1200, false)...)
	}
	f.rebillAs(t, "bill.rebill.approved", 1, billID(71), billID(81), 1200, 0)
	f.rebillAs(t, "bill.rebill.approved", 2, billID(72), billID(82), 1200, 0)
	f.rebillAs(t, "bill.rebill", 3, billID(73), billID(83), 1200, 0)
	if ex := f.exceptions("rebill_allowance_exceeded"); len(ex) != 0 {
		t.Fatalf("one plain re-bill against an allowance of 1 after two approved ones: %+v", ex)
	}
	f.rebillAs(t, "bill.rebill", 4, billID(74), billID(84), 1200, 0)
	ex := f.exceptions("rebill_allowance_exceeded")
	if len(ex) != 1 || ex[0].Reason == nil || *ex[0].Reason != gen.Missing {
		t.Fatalf("the second plain re-bill must be flagged with reason missing: %+v", ex)
	}
}

func TestApprovedRebillValueReductionIsStillFlagged(t *testing.T) {
	f := newFixture(t)
	f.apply(tenantA, deviceA, outletStream, billMuts(billID(62), number(1), 1200, false)...)
	f.apply(tenantA, deviceA, outletStream, billMuts(billID(63), number(2), 1200, false)...)
	f.rebillAs(t, "bill.rebill.approved", 1, billID(62), billID(65), 0, 0)
	f.rebillAs(t, "bill.rebill", 2, billID(63), billID(66), 0, 0)
	if ex := f.exceptions("rebill_value_reduced"); len(ex) != 2 {
		t.Fatalf("both reductions are findings, approved or not: %+v", ex)
	}
	if ex := f.exceptions("rebill_value_reduction_repeated"); len(ex) != 1 {
		t.Fatalf("the approved reduction counts toward the repeat: %+v", ex)
	}
}

func TestRebillAfterPaymentIsAFindingOnlyWithoutTheApproval(t *testing.T) {
	f := newFixture(t)
	f.apply(tenantA, deviceA, outletStream, billMuts(billID(60), number(1), 1200, true)...)
	f.apply(tenantA, deviceA, outletStream, billMuts(billID(64), number(2), 1200, false)...)
	f.apply(tenantA, deviceB, outletStream, payMut("0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f9f02", billID(64), "cash", 50400))
	f.rebillAs(t, "bill.rebill.approved", 1, billID(60), billID(61), 1200, 0)
	if ex := f.exceptions("rebill_after_payment"); len(ex) != 0 {
		t.Fatalf("an approved re-bill after payment was flagged: %+v", ex)
	}
	f.rebillAs(t, "bill.rebill", 2, billID(64), billID(65), 1200, 0)
	ex := f.exceptions("rebill_after_payment")
	if len(ex) != 1 || ex[0].Reason == nil || *ex[0].Reason != gen.Missing {
		t.Fatalf("exceptions %+v", ex)
	}
}

func TestReplacementTotalIsPartOfTheCreditNoteDigest(t *testing.T) {
	hash := func(total int64) string {
		f := newFixture(t)
		f.apply(tenantA, deviceA, outletStream, billMuts(billID(62), number(1), 1200, false)...)
		f.rebillAs(t, "bill.rebill.approved", 1, billID(62), billID(65), 1200, total)
		d, err := f.svc.ProjectionDigest(f.as(tenantA), outletA, "2026-11-24")
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range d.Entities {
			if e.Entity == "credit_note" {
				return e.Hash
			}
		}
		t.Fatal("credit_note not listed")
		return ""
	}
	if a, b, c := hash(0), hash(50400), hash(50400); a == b || b != c {
		t.Fatalf("the digest must cover replacement_total_paise: absent %s, set %s, set again %s", a, b, c)
	}
}
