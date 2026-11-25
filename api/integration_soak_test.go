//go:build integration

package api_test

import (
	"context"
	"testing"

	"github.com/hostryhq/go-common/inbox"

	"github.com/hostryhq/svc-billing/internal/consumer"
	"github.com/hostryhq/svc-billing/internal/itest"
	"github.com/hostryhq/svc-billing/internal/seed"
	"github.com/hostryhq/svc-billing/service"
)

// A real bill from an order line is projected on Postgres (docs.body is NOT NULL), and the seeded double payment, the
// rest of a split bill recorded twice, is a detector finding.
func TestBillFromOrderLineAndDoublePaymentOnPostgres(t *testing.T) {
	s := itest.New(t)
	ctx := context.Background()
	if err := s.SeedWorld(ctx, "base"); err != nil {
		t.Fatal(err)
	}
	sub := &inbox.Subscriber{DB: s.DB.App, Consumer: "billing.sync", Handle: consumer.Handler(s.App.Repo, s.App.Service), Now: s.Clock.Now}
	bill := "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f7201"
	pay := func(id, tender string, amount int) service.Mut {
		return service.Mut{E: "payment", ID: id, K: "insert", F: map[string]any{"bill_id": bill, "target_kind": "bill", "tender": tender, "amount_paise": amount, "business_date": "2026-11-24"}}
	}
	send := func(seq int64, intent string, muts ...service.Mut) {
		t.Helper()
		d := &delivery{data: syncEvent(t, seed.Tenant, seq, intent, muts...)}
		if err := sub.Process(ctx, d); err != nil || d.acks != 1 {
			t.Fatalf("seq %d: %v acks %d naks %d", seq, err, d.acks, d.naks)
		}
	}
	send(950, "bill.finalize",
		service.Mut{E: "bill", ID: bill, K: "insert", F: map[string]any{"number": "A7K/2627/000201", "series_code": "A7K", "doc_type": "tax_invoice", "source_kind": "counter",
			"order_type": "dine_in", "business_date": "2026-11-24", "invoice_date": "2026-11-24", "financial_year": "2627", "calc_v": 1, "ruleset_id": "9107573a-9de6-569a-a692-fb53d5d4c799",
			"price_mode": "exclusive", "gross_paise": 10000, "taxable_paise": 10000, "cgst_paise": 250, "sgst_paise": 250, "round_off_paise": 0, "total_paise": 10500, "service_charge_bp": 0}},
		service.Mut{E: "bill_line", ID: "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f7202", K: "insert", F: map[string]any{"bill_id": bill, "line_no": 1, "item_id": "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f5101",
			"item_name": "Tea", "category_name": "Drinks", "qty_milli": 1000, "unit_price_paise": 10000, "net_paise": 10000, "tax_class": "fnb_standard", "order_line_id": "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f7203"}},
		pay("0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f7204", "cash", 5250))
	send(951, "payment.record", pay("0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f7205", "upi", 5250))
	send(952, "payment.record", pay("0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f7206", "cash", 5250))
	var n int
	if err := s.DB.Admin.QueryRowContext(ctx, `SELECT count(*) FROM docs WHERE kind = 'exception' AND k1 = 'payments_exceed_total' AND k2 = 'open'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d payments_exceed_total findings (%v)", n, err)
	}
}
