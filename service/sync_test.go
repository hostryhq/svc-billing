package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hostryhq/go-common/emit"
	gsync "github.com/hostryhq/go-common/sync"

	"github.com/hostryhq/svc-billing/internal/gen"
	"github.com/hostryhq/svc-billing/repository"
	"github.com/hostryhq/svc-billing/service"
)

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

func settingsRow() string { return gsync.UUIDv5("billing_settings|" + outletA) }

// applyAt projects one op with an explicit clock (milliseconds) and sequence, so a test controls the stamp order.
func (f *fixture) applyAt(origin string, seq, ms int64, intent string, muts ...service.Mut) service.Op {
	f.t.Helper()
	op := service.Op{Tenant: tenantA, Origin: origin, Stream: "outlet:" + outletA, Seq: seq, Hash: fmt.Sprintf("h-%s-%d", origin, seq), HLC: fmt.Sprintf("%016x", ms<<16),
		Intent: intent, At: f.clk.Now(), Actor: member, Muts: int64s(muts)}
	if err := f.svc.Project(f.sys(tenantA), op); err != nil {
		f.t.Fatal(err)
	}
	return op
}

func (f *fixture) config(t *testing.T) gen.BillingSettings {
	t.Helper()
	c, err := f.svc.GetOutletBillingConfig(f.as(tenantA), outletA)
	if err != nil {
		t.Fatal(err)
	}
	return c.Settings
}

// Concurrent settings edits converge on the same row whatever the arrival order (the projection engine resolves per field
// group by stamp), and an edit that arrives before the row's insert is parked until the insert comes (ADR-0014 A2.1).
func TestSettingsConvergeWhateverTheArrivalOrder(t *testing.T) {
	ins := service.Mut{E: "billing_settings", ID: settingsRow(), K: "insert", F: map[string]any{"number_at": "settle", "discount_limit_bp": 1000}}
	numbering := service.Mut{E: "billing_settings", ID: settingsRow(), K: "set", F: map[string]any{"number_at": "print"}}
	limit := service.Mut{E: "billing_settings", ID: settingsRow(), K: "set", F: map[string]any{"discount_limit_bp": 1500}}
	apply := func(f *fixture, order ...int) {
		ops := []func(){
			func() { f.applyAt(deviceA, 1, 1000, "settings.numbering.set", ins) },
			func() { f.applyAt(deviceB, 1, 3000, "settings.numbering.set", numbering) },
			func() { f.applyAt("cloud-1", 1, 2000, "settings.discount_limit.set", limit) },
		}
		for _, i := range order {
			ops[i]()
		}
	}
	a, b := newFixture(t), newFixture(t)
	apply(a, 0, 1, 2)
	apply(b, 2, 1, 0) // the edits first, the insert last
	for _, f := range []*fixture{a, b} {
		if s := f.config(t); s.NumberAt != "print" || s.DiscountLimitBp != 1500 {
			t.Fatalf("settings %+v, want number_at print and limit 1500", s)
		}
	}
	// an older edit that arrives last does not win
	a.applyAt(deviceB, 2, 500, "settings.numbering.set", service.Mut{E: "billing_settings", ID: settingsRow(), K: "set", F: map[string]any{"number_at": "settle"}})
	if s := a.config(t); s.NumberAt != "print" {
		t.Fatalf("an older edit won: %+v", s)
	}
}

// A void of a refused configuration op removes its candidates: the row falls back to the value before it.
func TestVoidRestoresTheValueBeforeTheRefusedOp(t *testing.T) {
	f := newFixture(t)
	f.applyAt(deviceA, 1, 1000, "settings.numbering.set", service.Mut{E: "billing_settings", ID: settingsRow(), K: "insert", F: map[string]any{"number_at": "settle", "discount_limit_bp": 1000}})
	f.applyAt("cloud-1", 1, 2000, "settings.discount_limit.set", service.Mut{E: "billing_settings", ID: settingsRow(), K: "set", F: map[string]any{"discount_limit_bp": 1500}})
	if s := f.config(t); s.DiscountLimitBp != 1500 {
		t.Fatalf("limit %d", s.DiscountLimitBp)
	}
	op := service.Op{Tenant: tenantA, Origin: "cloud-1", Stream: "outlet:" + outletA, Seq: 2, Hash: "void", HLC: fmt.Sprintf("%016x", int64(3000)<<16), Intent: "sync.void", At: f.clk.Now(),
		Actor: "service:sync", Voids: []gsync.OpRef{{Origin: "cloud-1", Stream: "outlet:" + outletA, Seq: 1}}}
	if err := f.svc.Project(f.sys(tenantA), op); err != nil {
		t.Fatal(err)
	}
	if s := f.config(t); s.DiscountLimitBp != 1000 {
		t.Fatalf("limit after the void %d, want 1000", s.DiscountLimitBp)
	}
}

// Personal data beside an op goes to the erasable side table, never into a read model, and an erasure removes it.
func TestPersonalDataStaysOutOfTheReadModelAndIsErasable(t *testing.T) {
	f := newFixture(t)
	op := service.Op{Tenant: tenantA, Origin: deviceA, Stream: "outlet:" + outletA, Seq: 1, Hash: "h1", HLC: fmt.Sprintf("%016x", int64(1000)<<16), Intent: "bill.finalize",
		At: f.clk.Now(), Actor: member, Muts: int64s(billMuts(billID(50), number(1), 1200, true)),
		PII: map[string]map[string]map[string]any{"bill": {billID(50): {"customer_name": "Asha Verma"}}}}
	if err := f.svc.Project(f.sys(tenantA), op); err != nil {
		t.Fatal(err)
	}
	if kept := f.store.PII(tenantA); len(kept) != 1 || kept[0].Fields["customer_name"] != "Asha Verma" {
		t.Fatalf("pii kept %+v", kept)
	}
	if b, _ := json.Marshal(f.bill(t, billID(50))); strings.Contains(string(b), "Asha") {
		t.Fatalf("personal data in the read model: %s", b)
	}
	if err := f.store.InTx(f.sys(tenantA), func(ctx context.Context, tx repository.Tx) error {
		n, err := tx.ErasePII(ctx, gsync.OpRef{Origin: deviceA, Stream: "outlet:" + outletA, Seq: 1})
		if n != 1 {
			t.Fatalf("erased %d", n)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(f.store.PII(tenantA)) != 0 {
		t.Fatal("personal data survived the erasure")
	}
}

// The strong-session routes emit under their own intent, with a key derived from the Idempotency-Key (a retry is one op),
// and answer emit.unavailable, changing nothing, while the gateway is down.
func TestSettingsRoutesEmitUnderTheirIntent(t *testing.T) {
	f := newFixture(t)
	em := &fakeEmitter{}
	svc := service.New(f.store, service.Options{Clock: f.clk, Log: discardLog(), Orders: fakeOrders{}, Emitter: em})
	ctx := service.WithIdem(f.as(tenantA), service.Idem{Op: "setDiscountLimit", Key: "k-1", Fingerprint: "x"})
	ch, err := svc.SetDiscountLimit(ctx, outletA, 1500)
	if err != nil || !strings.HasPrefix(ch.OpId, "cloud-1/outlet:") {
		t.Fatalf("%+v %v", ch, err)
	}
	op := em.ops[0]
	if op.Intent != "settings.discount_limit.set" || op.Muts[0].K != "set" || op.Muts[0].F["discount_limit_bp"] != int64(1500) || op.AuthorisedBy == nil || *op.AuthorisedBy != "bill.discount.limit.edit" || op.Actor == "" {
		t.Fatalf("op %+v", op)
	}
	// the same Idempotency-Key is the same op at the gateway
	if _, err := svc.SetDiscountLimit(ctx, outletA, 1500); err != nil || len(em.ops) != 1 || em.ops[0].Key != op.Key {
		t.Fatalf("a retry made a second op: %v %d", err, len(em.ops))
	}
	ctx2 := service.WithIdem(f.as(tenantA), service.Idem{Op: "setOutletTaxProfile", Key: "k-2", Fingerprint: "y"})
	if _, err := svc.SetOutletTaxProfile(ctx2, outletA, "2627", false, nil, nil); err != nil {
		t.Fatal(err)
	}
	if p := em.ops[1]; p.Intent != "settings.tax_profile.set" || p.Muts[0].E != "outlet_tax_profile" || p.Muts[0].K != "insert" {
		t.Fatalf("op %+v", p)
	}
	em.fail = emit.NewUnavailable(errors.New("down"), 0)
	_, err = svc.SetDiscountLimit(service.WithIdem(f.as(tenantA), service.Idem{Op: "setDiscountLimit", Key: "k-3", Fingerprint: "z"}), outletA, 1200)
	wantCode(t, err, "emit.unavailable")
	if len(em.ops) != 2 {
		t.Fatal("an op was stored although the gateway was down")
	}
}

// A sealed Z revision is queued for the outlet's devices in the transaction that seals it, under its deterministic key.
func TestRevisionIsQueuedForTheDevices(t *testing.T) {
	f := newFixture(t)
	f.apply(tenantA, deviceA, "outlet:"+outletA, closeMuts("2026-11-24", nil)...)
	days, _ := f.svc.ListBusinessDays(f.as(tenantA), outletA, "", "", 10)
	dayID := days.Items[0].Id
	f.apply(tenantA, deviceA, "outlet:"+outletA, service.Mut{E: "day_close", ID: "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f7033", K: "insert", F: map[string]any{"business_day_id": dayID,
		"z_number": "Z/2026-11-24/P1", "cutoff": fmt.Sprintf(`{"%s":2}`, deviceA), "gross_paise": 0, "total_paise": 0, "bill_count": 0, "tenders_json": "[]", "series_ranges_json": "[]"}})
	f.apply(tenantA, deviceB, "outlet:"+outletA, billMuts(billID(60), "BBB/2627/000001", 1200, true)...)
	f.clk.Advance(time.Minute)
	if n, err := f.svc.FlushRevisions(f.sys(tenantA)); err != nil || n != 1 {
		t.Fatalf("flush %d %v", n, err)
	}
	q := f.store.Queued()
	if len(q) != 1 || q[0].Tenant != tenantA {
		t.Fatalf("queued %+v", q)
	}
	o := q[0].Op
	if o.Intent != "day.revision.record" || o.Stream != "outlet:"+outletA || o.Key != emit.DeterministicKey("day.revision.record", dayID, "1") || o.Muts[0].E != "business_day_revision" ||
		o.Muts[0].F["display_number"] != "Z/2026-11-24/P1/R1" || o.Muts[0].F["bill_count"] != int64(1) || o.AuthorisedBy != nil {
		t.Fatalf("op %+v", o)
	}
}

// billing publishes billing.series.reconciled once, when the series is closed, its device stated the last number and the
// register holds every number up to it; until then the missing numbers stay open gaps, a tail gap included.
func TestSeriesIsReconciledOnceWhenTheRegisterIsComplete(t *testing.T) {
	f := newFixture(t)
	ctx := f.sys(tenantA)
	const sid = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a6c"
	reg := "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a6b"
	if err := f.svc.RecordSeriesIssued(ctx, service.SeriesIssued{SeriesID: sid, Code: "AAA", DocType: "tax_invoice", RegistrationID: reg, OutletID: outletA, DeviceID: deviceA}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{1, 2, 4} {
		f.apply(tenantA, deviceA, "outlet:"+outletA, billMuts(billID(70+n), number(n), 1200, false)...)
	}
	if err := f.svc.RecordDrain(ctx, []service.SeriesStatement{{SeriesID: sid, LastNumber: 5}}); err != nil { // statement before the close: order-free
		t.Fatal(err)
	}
	if err := f.svc.RecordSeriesClosed(ctx, sid, "outlet_moved"); err != nil {
		t.Fatal(err)
	}
	if count(f.eventTypes(tenantA), "billing.series.reconciled") != 0 {
		t.Fatal("reconciled while numbers 3 and 5 are missing")
	}
	if gaps, _ := f.svc.ListDocumentGaps(f.as(tenantA), "open", "", "", 50); len(gaps.Items) != 2 { // the hole 3 and the tail 5
		t.Fatalf("open gaps %+v", gaps.Items)
	}
	f.apply(tenantA, deviceA, "outlet:"+outletA, billMuts(billID(73), number(3), 1200, false)...)
	if count(f.eventTypes(tenantA), "billing.series.reconciled") != 0 {
		t.Fatal("reconciled while number 5 is missing")
	}
	f.apply(tenantA, deviceA, "outlet:"+outletA, billMuts(billID(75), number(5), 1200, false)...)
	var ev []string
	for _, e := range f.store.Events() {
		if e.Type == "billing.series.reconciled" {
			ev = append(ev, string(e.Data))
		}
	}
	if len(ev) != 1 || !strings.Contains(ev[0], `"last_number":5`) || !strings.Contains(ev[0], `"gap_count":0`) || !strings.Contains(ev[0], sid) {
		t.Fatalf("events %v", ev)
	}
	// published once: a later number or a repeated statement adds none
	f.apply(tenantA, deviceA, "outlet:"+outletA, billMuts(billID(76), number(6), 1200, false)...)
	if err := f.svc.RecordDrain(ctx, []service.SeriesStatement{{SeriesID: sid, LastNumber: 5}}); err != nil {
		t.Fatal(err)
	}
	if count(f.eventTypes(tenantA), "billing.series.reconciled") != 1 {
		t.Fatal("reconciled more than once")
	}
	if ls, err := f.svc.GetSeriesLastSeen(ctx, sid); err != nil || ls.Count != 6 {
		t.Fatalf("last seen %+v %v", ls, err)
	}
}
