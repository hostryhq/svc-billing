//go:build integration

package api_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hostryhq/go-common/events"
	"github.com/hostryhq/go-common/ids"
	"github.com/hostryhq/go-common/inbox"
	gsync "github.com/hostryhq/go-common/sync"

	"github.com/hostryhq/svc-billing/internal/consumer"
	"github.com/hostryhq/svc-billing/internal/itest"
	"github.com/hostryhq/svc-billing/internal/seed"
	"github.com/hostryhq/svc-billing/service"
)

// piiEvent is syncEvent for an op that carries personal data with its salt and the commitment in the sealed body (pii_c), as
// the gateway publishes it (O2-12): ParseOp verifies the commitment before billing stores the value.
func piiEvent(t *testing.T, seq int64, bill, name string, salt []byte, muts ...service.Mut) []byte {
	t.Helper()
	const origin = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5e"
	stream := "outlet:" + seed.Outlet
	commit, err := gsync.PIICommit(salt, name)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"actor": "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a64", "at": "2026-11-24T13:05:11.120Z", "clk": "sync", "hlc": "01a1ee565e000000",
		"intent": "bill.finalize", "intent_v": 1, "muts": muts, "origin": origin, "ph": gsync.Genesis(origin, stream), "seq": seq, "stream": stream, "tenant_id": seed.Tenant, "v": 1,
		"pii_c": map[string]any{"bill": map[string]any{bill: map[string]any{"customer_name": commit}}}})
	if err != nil {
		t.Fatal(err)
	}
	canon, err := gsync.Canonicalize(raw)
	if err != nil {
		t.Fatal(err)
	}
	body := string(canon)
	data, _ := json.Marshal(map[string]any{"origin": origin, "stream": stream, "seq": seq, "h": gsync.Hash(body), "intent": "bill.finalize", "intent_v": 1, "status": "accepted",
		"marks": []string{}, "body": body,
		"pii": map[string]any{"bill": map[string]any{bill: map[string]any{"customer_name": name, "~s:customer_name": base64.RawURLEncoding.EncodeToString(salt)}}}})
	e := events.Envelope{ID: ids.New(), Type: "sync.op.bill", V: 1, Source: "sync", TenantID: ids.ID(seed.Tenant), OutletID: ids.ID(seed.Outlet), Key: stream + "|" + origin, KeySeq: seq,
		OccurredAt: seed.At, Data: data}
	b, err := e.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Personal data arrives with its salt and is stored only if it verifies against the commitment; sync.pii.erased empties the
// values and salts on Postgres (under row-level security and the role's grants) and keeps the commitment, and an op whose
// erasure came first never stores its data.
func TestPersonalDataVerifiedStoredAndErasedOnPostgres(t *testing.T) {
	s := itest.New(t)
	ctx := context.Background()
	if err := s.SeedWorld(ctx, "base"); err != nil {
		t.Fatal(err)
	}
	sub := &inbox.Subscriber{DB: s.DB.App, Consumer: "billing.sync", Handle: consumer.Handler(s.App.Repo, s.App.Service), Now: s.Clock.Now}
	salt := []byte("0123456789abcdef")
	bill := "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f7201"
	ev := piiEvent(t, 910, bill, "Asha Verma", salt,
		service.Mut{E: "bill", ID: bill, K: "insert", F: map[string]any{"number": "A7K/2627/000126", "series_code": "A7K", "doc_type": "tax_invoice", "source_kind": "counter",
			"order_type": "dine_in", "business_date": "2026-11-24", "invoice_date": "2026-11-24", "financial_year": "2627", "calc_v": 1, "price_mode": "exclusive",
			"gross_paise": 10000, "taxable_paise": 10000, "cgst_paise": 250, "sgst_paise": 250, "round_off_paise": 0, "total_paise": 10500, "service_charge_bp": 0}})
	if err := sub.Process(ctx, &delivery{data: ev}); err != nil {
		t.Fatal(err)
	}
	var fields, salts, commits string
	read := func() error {
		return s.DB.Admin.QueryRowContext(ctx, `SELECT fields::text, salts::text, commits::text FROM sync_pii WHERE row_id = $1`, bill).Scan(&fields, &salts, &commits)
	}
	if err := read(); err != nil || !strings.Contains(fields, "Asha Verma") || !strings.Contains(salts, "customer_name") || !strings.Contains(commits, "customer_name") {
		t.Fatalf("stored %s %s %s (%v)", fields, salts, commits, err)
	}
	if got := itest.Do(t, s.Srv.URL, "GET", "/v1/bills/"+bill, itest.Bearer(s.TenantToken(ids.ID(seed.Tenant))), nil); strings.Contains(string(got.Body), "Asha") {
		t.Fatalf("personal data in the read model: %s", got.Body)
	}
	erased, _ := json.Marshal(map[string]any{"origin": "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5e", "stream": "outlet:" + seed.Outlet, "seq": 910})
	if err := sub.Process(ctx, &delivery{data: envelope(t, seed.Tenant, "sync.pii.erased", erased)}); err != nil {
		t.Fatal(err)
	}
	if err := read(); err != nil || strings.Contains(fields, "Asha") || salts != "{}" || !strings.Contains(commits, "customer_name") {
		t.Fatalf("after the erasure: %s %s %s (%v)", fields, salts, commits, err)
	}
	// the erasure of op 911 arrives before the op: its data is never stored
	erased2, _ := json.Marshal(map[string]any{"origin": "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5e", "stream": "outlet:" + seed.Outlet, "seq": 911})
	if err := sub.Process(ctx, &delivery{data: envelope(t, seed.Tenant, "sync.pii.erased", erased2)}); err != nil {
		t.Fatal(err)
	}
	bill2 := "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f7202"
	ev2 := piiEvent(t, 911, bill2, "Ravi Kumar", salt,
		service.Mut{E: "bill", ID: bill2, K: "insert", F: map[string]any{"number": "A7K/2627/000127", "series_code": "A7K", "doc_type": "tax_invoice", "source_kind": "counter",
			"order_type": "dine_in", "business_date": "2026-11-24", "invoice_date": "2026-11-24", "financial_year": "2627", "calc_v": 1, "price_mode": "exclusive",
			"gross_paise": 10000, "taxable_paise": 10000, "cgst_paise": 250, "sgst_paise": 250, "round_off_paise": 0, "total_paise": 10500, "service_charge_bp": 0}})
	if err := sub.Process(ctx, &delivery{data: ev2}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.DB.Admin.QueryRowContext(ctx, `SELECT count(*) FROM sync_pii WHERE row_id = $1`, bill2).Scan(&n); err != nil || n != 0 {
		t.Fatalf("data of an op erased before its arrival was stored (%d, %v)", n, err)
	}
	if err := s.DB.Admin.QueryRowContext(ctx, `SELECT count(*) FROM docs WHERE kind = 'bill' AND id = $1`, bill2).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the bill of the erased op was not applied (%d, %v)", n, err)
	}
}
