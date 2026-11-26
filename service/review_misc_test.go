package service_test

import (
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/hostryhq/svc-billing/double"
	"github.com/hostryhq/svc-billing/internal/gen"
	"github.com/hostryhq/svc-billing/service"
)

// CR-H5: the same legal number under two bill ids is flagged; the first claim keeps the number.
func TestSameNumberUnderTwoBillIdsIsFlagged(t *testing.T) {
	f := newFixture(t)
	f.apply(tenantA, deviceA, outletStream, billMuts(billID(48), number(42), 1200, true)...)
	f.apply(tenantA, deviceA, outletStream, billMuts(billID(49), number(42), 1200, true)...)
	ex := f.exceptions("duplicate_number")
	if len(ex) != 1 || ex[0].SubjectId != billID(49) || ex[0].Severity != gen.High || (*ex[0].Params)["first_doc_id"] != billID(48) {
		t.Fatalf("exceptions %+v", ex)
	}
	reg, _ := f.svc.ListDocumentRegister(f.as(tenantA), "", "", "", 10)
	if len(reg.Items) != 1 || reg.Items[0].Count != 1 {
		t.Fatalf("register %+v", reg.Items)
	}
	// a replay of the first bill is not a duplicate
	f2 := newFixture(t)
	op := f2.apply(tenantA, deviceA, outletStream, billMuts(billID(48), number(42), 1200, true)...)
	_ = f2.svc.Project(f2.sys(tenantA), op)
	if ex := f2.exceptions("duplicate_number"); len(ex) != 0 {
		t.Fatalf("replay flagged: %+v", ex)
	}
}

// M2 and P2-M5: the lead time runs from the start of the effective date in IST, with no truncation.
func TestRuleSetLeadTimeIsMeasuredToTheStartOfTheEffectiveDayInIST(t *testing.T) {
	cases := []struct {
		now       time.Time
		effective string
		refused   bool
	}{
		{time.Date(2026, 11, 25, 23, 59, 0, 0, time.UTC), "2026-11-26", true}, // 26 Nov IST began 5.5 h ago
		{time.Date(2026, 11, 25, 18, 29, 0, 0, time.UTC), "2026-11-26", true}, // 26 Nov IST begins in one minute
		{time.Date(2026, 11, 25, 18, 31, 0, 0, time.UTC), "2026-11-27", true}, // 27 Nov IST begins in 23 h 59 min
		{time.Date(2026, 11, 25, 18, 29, 0, 0, time.UTC), "2026-11-27", false}, // 24 h 1 min ahead
		{time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC), "2026-11-26", false},
	}
	for _, c := range cases {
		t.Run(c.now.Format(time.RFC3339)+" "+c.effective, func(t *testing.T) {
			f := newFixture(t)
			f.clk.Set(c.now)
			grants := double.NewGrants(f.clk)
			svc := service.New(f.store, service.Options{Clock: f.clk, Log: slog.New(slog.DiscardHandler), Grants: grants})
			m := signed(t, f, svc, c.effective)
			_, err := svc.PublishTaxRuleSet(f.vendorCtx(approver), m.Id, publishGrant(grants, service.PublishSubjectHash(m.Id, *m.ContentHash, c.effective), nil))
			if c.refused {
				wantCode(t, err, "effective_date_too_soon")
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// M3: an outlet that moved to a new registration and regime at a day boundary: a bill of the earlier invoice date is
// registered and verified under the registration and regime in force on that date.
func TestRegisterAndVerificationUseTheRegistrationInForceOnTheInvoiceDate(t *testing.T) {
	f := newFixture(t)
	const newReg = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a7c"
	if err := f.svc.RecordOutlet(f.sys(tenantA), service.OutletRec{OutletID: outletA, RegistrationID: newReg, Regime: "composition", EffectiveFrom: "2026-11-25"}); err != nil {
		t.Fatal(err)
	}
	f.apply(tenantA, deviceA, outletStream, billMuts(billID(53), number(1), 1200, true)...) // invoice date 2026-11-24
	if st := f.bill(t, billID(53)).Verification.Status; st != gen.VerificationStatusVerified {
		t.Fatalf("verification %v: the bill was recomputed under the new regime", st)
	}
	reg, _ := f.svc.ListDocumentRegister(f.as(tenantA), "", "", "", 10)
	if len(reg.Items) != 1 || reg.Items[0].TaxRegistrationId != "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a6b" {
		t.Fatalf("register %+v: the bill was registered under the new registration", reg.Items)
	}
	// a replay of the same tenancy fact opens no new era
	if err := f.svc.RecordOutlet(f.sys(tenantA), service.OutletRec{OutletID: outletA, RegistrationID: newReg, Regime: "composition", EffectiveFrom: "2026-11-26"}); err != nil {
		t.Fatal(err)
	}
	f.clk.Set(time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC))
	ms := billMuts(billID(54), number(2), 1200, false)
	ms[0].F["invoice_date"], ms[0].F["business_date"] = "2026-11-25", "2026-11-25"
	f.apply(tenantA, deviceA, outletStream, ms...)
	reg, _ = f.svc.ListDocumentRegister(f.as(tenantA), "", "", "", 10)
	if len(reg.Items) != 2 {
		t.Fatalf("want one series per registration, got %+v", reg.Items)
	}
}

// M4: the recorded events state the verification status as it is: pending until the rule set is known.
func TestRecordedEventsReportPendingVerification(t *testing.T) {
	f := newFixture(t)
	ms := billMuts(billID(55), number(1), 1200, false)
	ms[0].F["ruleset_id"] = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f7fff" // not received yet
	f.apply(tenantA, deviceA, outletStream, ms...)
	if st := f.bill(t, billID(55)).Verification.Status; st != gen.VerificationStatusPending {
		t.Fatalf("status %v", st)
	}
	for _, e := range f.store.Events() {
		if e.Type != "billing.bill.recorded" {
			continue
		}
		var d struct {
			Verification string `json:"verification"`
		}
		if err := json.Unmarshal(e.Data, &d); err != nil || d.Verification != "pending" {
			t.Fatalf("event says %q (%v)", d.Verification, err)
		}
		return
	}
	t.Fatal("no billing.bill.recorded event")
}
