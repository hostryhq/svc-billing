//go:build integration

package api_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hostryhq/go-common/events"
	"github.com/hostryhq/go-common/ids"
	"github.com/hostryhq/go-common/inbox"
	gsync "github.com/hostryhq/go-common/sync"
	"github.com/hostryhq/go-common/tenancy"

	"github.com/hostryhq/svc-billing/internal/consumer"
	"github.com/hostryhq/svc-billing/internal/contractenv"
	"github.com/hostryhq/svc-billing/internal/itest"
	"github.com/hostryhq/svc-billing/internal/seed"
	"github.com/hostryhq/svc-billing/service"
)

// The same contract suite that runs against the double runs here against the real service on Postgres, in every
// example world, with the roles and row-level security of the real schema. The real service is held to status, media
// type and schema; the verbatim examples are the double's duty.
func TestRealServiceContract(t *testing.T) {
	s := itest.New(t)
	contractenv.Run(t, s.Srv.URL, s.Tokens, s.SeedWorld, false)
}

// syncEvent builds the gateway's sync.op.<namespace> event for one op as go-common's parser expects it: the canonical body
// as a string, its hash and the envelope identity.
func syncEvent(t *testing.T, tenant string, seq int64, intent string, muts ...service.Mut) []byte {
	t.Helper()
	const origin = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5e"
	stream := "outlet:" + seed.Outlet
	raw, err := json.Marshal(map[string]any{"actor": "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a64", "at": "2026-11-24T13:05:11.120Z", "clk": "sync", "hlc": "01a1ee565e000000",
		"intent": intent, "intent_v": 1, "muts": muts, "origin": origin, "ph": gsync.Genesis(origin, stream), "seq": seq, "stream": stream, "tenant_id": tenant, "v": 1})
	if err != nil {
		t.Fatal(err)
	}
	canon, err := gsync.Canonicalize(raw)
	if err != nil {
		t.Fatal(err)
	}
	body := string(canon)
	ns, _, _ := strings.Cut(intent, ".")
	data, _ := json.Marshal(map[string]any{"origin": origin, "stream": stream, "seq": seq, "h": gsync.Hash(body), "intent": intent, "intent_v": 1, "status": "accepted",
		"marks": []string{}, "body": body, "pii": map[string]any{}})
	e := events.Envelope{ID: ids.New(), Type: "sync.op." + ns, V: 1, Source: "sync", TenantID: ids.ID(tenant), OutletID: ids.ID(seed.Outlet), Key: stream + "|" + origin, KeySeq: seq,
		OccurredAt: seed.At, Data: data}
	b, err := e.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type delivery struct {
	data []byte
	acks int
	naks int
}

func (d *delivery) Subject() string         { return "evt.sync.op.bill.v1" }
func (d *delivery) Data() []byte            { return d.data }
func (d *delivery) NumDelivered() uint64    { return 1 }
func (d *delivery) Ack() error              { d.acks++; return nil }
func (d *delivery) Nak(time.Duration) error { d.naks++; return nil }

func envelope(t *testing.T, tenant string, typ string, data []byte) []byte {
	t.Helper()
	source, _, _ := strings.Cut(typ, ".")
	e := events.Envelope{ID: ids.New(), Type: typ, V: 1, Source: source, TenantID: ids.ID(tenant), OccurredAt: seed.At, Data: data}
	b, err := e.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// An op arrives on the bus, is projected inside the inbox transaction under row-level security, and a redelivery of
// the same event and of the same op under a new event id changes nothing.
func TestBusProjectionOnPostgres(t *testing.T) {
	s := itest.New(t)
	ctx := context.Background()
	if err := s.SeedWorld(ctx, "base"); err != nil {
		t.Fatal(err)
	}
	sub := &inbox.Subscriber{DB: s.DB.App, Consumer: "billing.sync", Handle: consumer.Handler(s.App.Repo, s.App.Service), Now: s.Clock.Now}
	bill := "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f7101"
	ev := syncEvent(t, seed.Tenant, 900, "bill.finalize",
		service.Mut{E: "bill", ID: bill, K: "insert", F: map[string]any{"number": "A7K/2627/000125", "series_code": "A7K", "doc_type": "tax_invoice", "source_kind": "counter",
			"order_type": "dine_in", "business_date": "2026-11-24", "invoice_date": "2026-11-24", "financial_year": "2627", "calc_v": 1, "ruleset_id": "9107573a-9de6-569a-a692-fb53d5d4c799",
			"price_mode": "exclusive", "gross_paise": 10000, "taxable_paise": 10000, "cgst_paise": 250, "sgst_paise": 250, "round_off_paise": 0, "total_paise": 10500, "service_charge_bp": 0}},
		service.Mut{E: "bill_line", ID: "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f7102", K: "insert", F: map[string]any{"bill_id": bill, "line_no": 1, "item_id": "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f5101",
			"item_name": "Tea", "category_name": "Drinks", "qty_milli": 1000, "unit_price_paise": 10000, "net_paise": 10000, "tax_class": "fnb_standard"}},
		service.Mut{E: "check", ID: "x", K: "set", F: map[string]any{"status": "billed"}}) // another service's entity in the same op: ignored
	d := &delivery{data: ev}
	if err := sub.Process(ctx, d); err != nil || d.acks != 1 {
		t.Fatalf("process: %v acks %d naks %d", err, d.acks, d.naks)
	}
	// the same op under a new event id (a bus reset) changes nothing
	var again events.Envelope
	if err := json.Unmarshal(ev, &again); err != nil {
		t.Fatal(err)
	}
	again.ID = ids.New()
	raw2, _ := again.Marshal()
	d2 := &delivery{data: raw2}
	if err := sub.Process(ctx, d2); err != nil || d2.acks != 1 {
		t.Fatal(err)
	}
	var n int
	if err := s.DB.Admin.QueryRowContext(ctx, `SELECT count(*) FROM docs WHERE kind = 'bill' AND id = $1`, bill).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d rows (%v)", n, err)
	}
	if err := s.DB.Admin.QueryRowContext(ctx, `SELECT count(*) FROM outbox WHERE type = 'billing.bill.recorded' AND key = $1`, bill).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d bill.recorded events (%v)", n, err)
	}
	// the bill is served, verified against the seeded rule set (5 percent) (5 percent)
	got := itest.Do(t, s.Srv.URL, "GET", "/v1/bills/"+bill, itest.Bearer(s.TenantToken(ids.ID(seed.Tenant))), nil)
	if got.Status != 200 || got.JSON(t)["verification"].(map[string]any)["status"] != "verified" {
		t.Fatalf("%d %s", got.Status, got.Body)
	}
}

// Row-level security: with another tenant's context set, the application role sees none of tenant A's rows, whatever
// the statement asks for, and cannot write one in tenant A's name.
func TestRowLevelSecurityIsolatesTenants(t *testing.T) {
	s := itest.New(t)
	ctx := context.Background()
	if err := s.SeedWorld(ctx, "base"); err != nil {
		t.Fatal(err)
	}
	other := ids.ID(seed.OtherTenant)
	err := tenancy.WithTx(tenancy.With(ctx, tenancy.Context{TenantID: other}), s.DB.App, func(ctx context.Context, tx *sql.Tx) error {
		for _, tbl := range []string{"docs", "applied_ops", "idempotency_keys", "event_keys"} {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM `+tbl).Scan(&n); err != nil {
				return err
			}
			if n != 0 {
				t.Errorf("%s: tenant B sees %d rows", tbl, n)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// a write in tenant A's name from tenant B's context is refused by the policy
	werr := tenancy.WithTx(tenancy.With(ctx, tenancy.Context{TenantID: other}), s.DB.App, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO docs (tenant_id, kind, id, body) VALUES ($1, 'bill', 'x', '{}')`, seed.Tenant)
		return err
	})
	if werr == nil {
		t.Error("tenant B wrote a row in tenant A's name")
	}
	// without any tenant context nothing is visible either
	var n int
	if err := s.DB.App.QueryRowContext(ctx, `SELECT count(*) FROM docs`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("no-context read saw %d rows (%v)", n, err)
	}
	// and the service says 404 to another organisation's owner
	r := itest.Do(t, s.Srv.URL, "GET", "/v1/bills/"+seed.Bill, itest.Bearer(s.TenantToken(other)), nil)
	if r.Status != 404 {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
}

// The runtime roles are what production uses: the application role cannot reach the global tables and the global role
// cannot reach tenant documents.
func TestRolesAreSeparated(t *testing.T) {
	s := itest.New(t)
	ctx := context.Background()
	if _, err := s.DB.App.ExecContext(ctx, `SELECT count(*) FROM tax_rule_masters`); err == nil {
		t.Error("billing_app can read the rule master")
	}
	if _, err := s.DB.Global.ExecContext(ctx, `SELECT count(*) FROM docs`); err == nil {
		t.Error("billing_global can read tenant documents")
	}
}
