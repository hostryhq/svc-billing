//go:build integration

package api_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hostryhq/go-common/emit"
	"github.com/hostryhq/go-common/ids"
	"github.com/hostryhq/go-common/inbox"
	gsync "github.com/hostryhq/go-common/sync"
	"github.com/hostryhq/go-common/tenancy"

	"github.com/hostryhq/svc-billing/internal/consumer"
	"github.com/hostryhq/svc-billing/internal/itest"
	"github.com/hostryhq/svc-billing/internal/seed"
	"github.com/hostryhq/svc-billing/service"
)

func mutOf(e, id, k string, f map[string]any) service.Mut {
	return service.Mut{E: e, ID: id, K: k, F: f}
}

const ruleSet = "72bdd3f5-68b5-59b0-8e62-2075a1f43c47"

func (s *integStack) publish(t *testing.T) itest.Response {
	t.Helper()
	h := s.VendorHeaders("admin.taxrules.manage", "admin.taxrules.publish")
	h.Set("Hostry-Vendor-Approval", seed.ExampleGrantToken())
	return itest.Do(t, s.Srv.URL, "POST", "/internal/v1/vendor/tax-rule-sets/"+ruleSet+"/publication", h, `{}`)
}

type integStack struct{ *itest.Stack }

// Publication queues one emit per organisation in emit_queue in its own transaction; the real worker and the real emit client
// send them to the gateway (an in-process fake of POST /internal/v1/emit) once it is up; nothing is lost while it is down, and a
// second drain, a replayed publication and the gateway's own idempotency make no second op.
func TestRuleSetEmissionThroughTheQueueOnPostgres(t *testing.T) {
	s := &integStack{itest.New(t)}
	ctx := context.Background()
	if err := s.SeedWorld(ctx, "rule-set-signed"); err != nil {
		t.Fatal(err)
	}
	if r := s.publish(t); r.Status != 200 {
		t.Fatalf("publish %d %s", r.Status, r.Body)
	}
	var pending int
	if err := s.DB.Admin.QueryRowContext(ctx, `SELECT count(*) FROM emit_queue WHERE state = 'pending' AND op->>'intent' = 'tax.rule_set.publish'`).Scan(&pending); err != nil || pending != 3 {
		t.Fatalf("%d queued (%v), want one per known organisation (3)", pending, err)
	}
	// the gateway is down: nothing is sent, nothing is lost
	s.Gateway.Fail = emit.NewUnavailable(errors.New("down"), 0)
	if n, err := s.DrainEmits(ctx); err != nil || n != 0 {
		t.Fatalf("drain while down: %d %v", n, err)
	}
	s.Gateway.Fail = nil
	s.Clock.Advance(10 * time.Minute) // past the worker's back-off after the failed attempt
	if n, err := s.DrainEmits(ctx); err != nil || n != 3 {
		t.Fatalf("drain: %d %v", n, err)
	}
	if n, _ := s.DrainEmits(ctx); n != 0 || len(s.Gateway.Ops()) != 3 {
		t.Fatalf("second drain %d, gateway ops %d", n, len(s.Gateway.Ops()))
	}
	for _, o := range s.Gateway.Ops() {
		if o.Op.Intent != "tax.rule_set.publish" || o.Op.Stream != "tenant:"+o.Tenant || o.Op.Approval != "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a6d" {
			t.Fatalf("gateway op %+v", o.Op)
		}
	}
	got := itest.Do(t, s.Srv.URL, "GET", "/internal/v1/vendor/tax-rule-sets/"+ruleSet, s.VendorHeaders("admin.taxrules.manage"), nil)
	if got.Status != 200 || got.JSON(t)["organisations_acknowledged"] != float64(3) {
		t.Fatalf("%d %s", got.Status, got.Body)
	}
	// the single-use grant: the same call again is refused
	if r := s.publish(t); r.Status == 200 {
		t.Fatalf("a second publication with the same grant: %s", r.Body)
	}
}

// The settings routes emit synchronously through the real client: a retry with the same Idempotency-Key is one op at the
// gateway, and while the gateway is down the route answers 503 emit.unavailable.
func TestSettingsEmitAnd503WhileTheGatewayIsDown(t *testing.T) {
	s := &integStack{itest.New(t)}
	ctx := context.Background()
	if err := s.SeedWorld(ctx, "base"); err != nil {
		t.Fatal(err)
	}
	h := itest.Bearer(s.TenantToken(ids.ID(seed.Tenant)))
	h.Set("Idempotency-Key", "dl-1")
	path := "/v1/outlets/" + seed.Outlet + "/discount-limit"
	r1 := itest.Do(t, s.Srv.URL, "PUT", path, h, map[string]any{"discount_limit_bp": 1500})
	r2 := itest.Do(t, s.Srv.URL, "PUT", path, h, map[string]any{"discount_limit_bp": 1500})
	if r1.Status != 200 || r2.Status != 200 || len(s.Gateway.Ops()) != 1 || s.Gateway.Ops()[0].Op.Intent != "settings.discount_limit.set" {
		t.Fatalf("%d %s / %d %s, gateway ops %d", r1.Status, r1.Body, r2.Status, r2.Body, len(s.Gateway.Ops()))
	}
	s.Gateway.Fail = emit.NewUnavailable(errors.New("down"), 0)
	h.Set("Idempotency-Key", "dl-2")
	r3 := itest.Do(t, s.Srv.URL, "PUT", path, h, map[string]any{"discount_limit_bp": 1200})
	if r3.Status != http.StatusServiceUnavailable || r3.Code() != "emit.unavailable" || len(s.Gateway.Ops()) != 1 {
		t.Fatalf("%d %s", r3.Status, r3.Body)
	}
}

// The new tables are under row-level security: a tenant never sees another's candidates, voids or personal data, the
// application role cannot read the emit queue and cannot queue an op in another tenant's name, and the global role can
// read what it drains.
func TestProjectionAndQueueTablesAreIsolated(t *testing.T) {
	s := &integStack{itest.New(t)}
	ctx := context.Background()
	if err := s.SeedWorld(ctx, "base"); err != nil {
		t.Fatal(err)
	}
	// an op through the bus fills sync_candidates for tenant A
	sub := &inbox.Subscriber{DB: s.DB.App, Consumer: "billing.sync", Handle: consumer.Handler(s.App.Repo, s.App.Service), Now: s.Clock.Now}
	ev := syncEvent(t, seed.Tenant, 901, "settings.numbering.set", mutOf("billing_settings", "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f7aa1", "insert", map[string]any{"number_at": "print"}))
	if err := sub.Process(ctx, &delivery{data: ev}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.DB.Admin.QueryRowContext(ctx, `SELECT count(*) FROM sync_candidates WHERE tenant_id = $1`, seed.Tenant).Scan(&n); err != nil || n != 1 {
		t.Fatalf("candidates %d (%v)", n, err)
	}
	other := ids.ID(seed.OtherTenant)
	err := tenancy.WithTx(tenancy.With(ctx, tenancy.Context{TenantID: other}), s.DB.App, func(ctx context.Context, tx *sql.Tx) error {
		for _, tbl := range []string{"sync_candidates", "sync_voids", "sync_pii"} {
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM `+tbl).Scan(&n); err != nil || n != 0 {
				t.Errorf("%s: tenant B sees %d rows (%v)", tbl, n, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// the application role never reads an op body from the queue (the error aborts the transaction, so it is its own)
	if err := tenancy.WithTx(tenancy.With(ctx, tenancy.Context{TenantID: other}), s.DB.App, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `SELECT op FROM emit_queue`)
		return err
	}); err == nil {
		t.Error("billing_app can read an op body from the emit queue")
	}
	insert := func(tenantOfRow string, ctxTenant ids.ID) error {
		return tenancy.WithTx(tenancy.With(ctx, tenancy.Context{TenantID: ctxTenant}), s.DB.App, func(ctx context.Context, tx *sql.Tx) error {
			return emit.Enqueue(ctx, tx, s.Clock.Now(), ids.ID(tenantOfRow), emit.Op{Key: "k-" + tenantOfRow, Stream: "outlet:" + seed.Outlet, Intent: "day.revision.record", IntentV: 1,
				Muts: []gsync.Mut{{E: "business_day_revision", ID: "x", K: "insert"}}, Actor: "service:billing"})
		})
	}
	if err := insert(seed.Tenant, ids.ID(seed.Tenant)); err != nil {
		t.Fatalf("a tenant could not queue for itself: %v", err)
	}
	// and another tenant sees none of its keys
	if err := tenancy.WithTx(tenancy.With(ctx, tenancy.Context{TenantID: other}), s.DB.App, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT count(*) FROM emit_queue`).Scan(&n)
	}); err != nil || n != 0 {
		t.Fatalf("tenant B sees %d queue rows (%v)", n, err)
	}
	if err := insert(seed.Tenant, other); err == nil {
		t.Fatal("tenant B queued an op in tenant A's name")
	}
	if err := s.DB.Global.QueryRowContext(ctx, `SELECT count(*) FROM emit_queue`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("global role sees %d queued rows (%v)", n, err)
	}
}

// The platform facts arrive through the inbox: a series issued, closed and drained with nothing used is reconciled and
// published once; the outlet's regime comes from the event.
func TestSeriesFactsThroughTheInbox(t *testing.T) {
	s := &integStack{itest.New(t)}
	ctx := context.Background()
	if err := s.SeedWorld(ctx, "base"); err != nil {
		t.Fatal(err)
	}
	sub := &inbox.Subscriber{DB: s.DB.App, Consumer: "billing.tenancy", Handle: consumer.Handler(s.App.Repo, s.App.Service), Now: s.Clock.Now}
	send := func(typ string, data map[string]any) {
		b, _ := json.Marshal(data)
		if err := sub.Process(ctx, &delivery{data: envelope(t, seed.Tenant, typ, b)}); err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
	}
	const sid = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a95"
	send("tenancy.series.issued", map[string]any{"series_id": sid, "code": "A7M", "doc_type": "credit_note", "tax_registration_id": seed.Registration, "outlet_id": seed.Outlet, "device_id": "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5e"})
	send("tenancy.series.closed", map[string]any{"series_id": sid, "code": "A7M", "doc_type": "credit_note", "tax_registration_id": seed.Registration, "outlet_id": seed.Outlet, "device_id": "x", "cause": "outlet_moved"})
	send("identity.device.drained", map[string]any{"device_id": "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5e", "drained_at": "2026-11-24T13:25:11.120Z", "series": []map[string]any{{"series_id": sid, "last_number": 0}}})
	send("identity.device.drained", map[string]any{"device_id": "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5e", "drained_at": "2026-11-24T13:26:11.120Z", "series": []map[string]any{{"series_id": sid, "last_number": 0}}})
	var n int
	if err := s.DB.Admin.QueryRowContext(ctx, `SELECT count(*) FROM outbox WHERE type = 'billing.series.reconciled' AND key = $1`, sid).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d reconciled events (%v)", n, err)
	}
	ls := itest.Do(t, s.Srv.URL, "GET", "/internal/v1/document-series/"+sid+"/last-seen", s.ServiceHeaders("tenancy", ids.ID(seed.Tenant)), nil)
	if ls.Status != 200 || !strings.Contains(string(ls.Body), `"count":0`) {
		t.Fatalf("%d %s", ls.Status, ls.Body)
	}
}
