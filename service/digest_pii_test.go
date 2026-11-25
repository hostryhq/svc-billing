package service_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	gsync "github.com/hostryhq/go-common/sync"

	"github.com/hostryhq/svc-billing/internal/gen"
	"github.com/hostryhq/svc-billing/internal/seed"
	"github.com/hostryhq/svc-billing/service"
)

// The canonical row text covers the non-pii fields and each pii field's commitment, never a value (api-specs
// sync/conformance digest_row vectors, ADR-0014 A3.2): the same text whether the node holds the values or not.
func TestDigestRowTextMatchesTheSharedVectors(t *testing.T) {
	var vs []struct {
		Case      string
		ID        string
		PIIFields []string `json:"pii_fields"`
		Fields    map[string]any
		PIIValues map[string]any    `json:"pii_values"`
		Commits   map[string]string `json:"commitments"`
		RowText   string            `json:"row_text"`
	}
	if err := json.Unmarshal(seed.DigestRowVectors(), &vs); err != nil || len(vs) < 3 {
		t.Fatalf("vectors: %v (%d)", err, len(vs))
	}
	for _, v := range vs {
		pii := map[string]bool{}
		for _, f := range v.PIIFields {
			pii[f] = true
		}
		withValues := map[string]any{}
		for k, x := range v.Fields {
			withValues[k] = x
		}
		for k, x := range v.PIIValues {
			withValues[k] = x
		}
		for name, fields := range map[string]map[string]any{"values held": withValues, "none held": v.Fields} {
			for k, x := range fields { // as ParseOp delivers integers
				if n, ok := x.(float64); ok {
					fields[k] = int64(n)
				}
			}
			got, err := service.DigestRowText(v.ID, fields, pii, v.Commits)
			if err != nil || got != v.RowText {
				t.Errorf("%s (%s): %s (%v), vector %s", v.Case, name, got, err, v.RowText)
			}
		}
	}
}

// A credit note's buyer name and GSTIN are personal data (A3.1): stored in the erasable side table, never in the read
// model, and the projection digest is the same whether the op carried the values or only the commitments, and unchanged
// when sync.pii.erase removes them.
func TestCreditNotePersonalDataIsErasedWithoutChangingTheDigest(t *testing.T) {
	salt := []byte("0123456789abcdef")
	name, gstin := "Asha Verma", "33AAAAA0000A1Z5"
	cName, _ := gsync.PIICommit(salt, name)
	cGstin, _ := gsync.PIICommit(salt, gstin)
	enc := base64.RawURLEncoding.EncodeToString(salt)
	setup := func(withValues bool) (*fixture, service.Op) {
		f := newFixture(t)
		rs := append(f.ruleSetMuts(tenantA, 2, "2026-11-01", 250), service.Mut{E: "tax_rule", ID: "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f9099", K: "insert", F: map[string]any{"rule_set_id": service.RuleSetRowID(tenantA, 2),
			"tax_class": "fnb_non_gst", "regime": "regular", "specified_premises": false, "order_channel": "dine_in", "doc_type": "tax_invoice", "gst_applies": false, "cgst_bp": 0, "sgst_bp": 0, "igst_bp": 0}})
		f.apply(tenantA, "cloud-1", "tenant", rs...)
		f.apply(tenantA, deviceA, "outlet:"+outletA, nonGSTBillMuts(billID(72), number(1))...)
		op := service.Op{Tenant: tenantA, Origin: deviceA, Stream: "outlet:" + outletA, Seq: 100, Hash: "hcn", HLC: fmt.Sprintf("%016x", f.clk.Now().UnixMilli()<<16),
			Intent: "bill.credit_note", At: f.clk.Now(), Actor: member, Muts: int64s([]service.Mut{
				{E: "credit_note", ID: creditID, K: "insert", F: map[string]any{"number": "AAA/2627/CN0001", "series_code": "AAA", "bill_id": billID(72), "bill_number": number(1), "bill_invoice_date": "2026-11-24",
					"business_date": "2026-11-24", "invoice_date": "2026-11-24", "financial_year": "2627", "calc_v": 1, "ruleset_id": service.RuleSetRowID(tenantA, 2), "total_paise": 6000, "round_off_paise": 0}},
				{E: "credit_note_line", ID: creditID[:30] + "0001", K: "insert", F: map[string]any{"credit_note_id": creditID, "line_no": 1, "bill_line_id": billID(72)[:30] + "0004", "item_name": "Water", "qty_milli": 1000, "net_paise": 6000, "tax_class": "fnb_non_gst"}},
				{E: "credit_note_tax_line", ID: creditID[:30] + "0002", K: "insert", F: map[string]any{"credit_note_id": creditID, "tax_class": "fnb_non_gst", "rate_bp": 0, "gst_applies": false,
					"charge_alloc_paise": 0, "taxable_paise": 6000, "cgst_paise": 0, "sgst_paise": 0, "igst_paise": 0}}}),
			PIICommits: gsync.PIIC{"credit_note": {creditID: {"customer_name": cName, "customer_gstin": cGstin}}}}
		if withValues {
			op.PII = map[string]map[string]map[string]any{"credit_note": {creditID: {"customer_name": name, "customer_gstin": gstin}}}
			op.PIISalts = map[string]map[string]map[string]string{"credit_note": {creditID: {"customer_name": enc, "customer_gstin": enc}}}
		}
		if err := f.svc.Project(f.sys(tenantA), op); err != nil {
			t.Fatal(err)
		}
		return f, op
	}
	digest := func(f *fixture) gen.ProjectionDigest {
		d, err := f.svc.ProjectionDigest(f.as(tenantA), outletA, "2026-11-24")
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	hash := func(d gen.ProjectionDigest) string {
		for _, e := range d.Entities {
			if e.Entity == "credit_note" {
				return e.Hash
			}
		}
		t.Fatal("credit_note not listed")
		return ""
	}

	f, op := setup(true)
	kept := f.store.PII(tenantA)
	var cn *gsync.PIIWrite
	for i := range kept {
		if kept[i].Key.Entity == "credit_note" {
			cn = &kept[i]
		}
	}
	if cn == nil || cn.Fields["customer_name"] != name || cn.Fields["customer_gstin"] != gstin || cn.Commits["customer_name"] != cName {
		t.Fatalf("credit note pii kept %+v", kept)
	}
	if b, _ := json.Marshal(f.creditNote(t)); strings.Contains(string(b), "Asha") || strings.Contains(string(b), gstin) {
		t.Fatalf("personal data in the read model: %s", b)
	}
	before := digest(f)

	erase := service.Op{Tenant: tenantA, Origin: "cloud-1", Stream: "outlet:" + outletA, Seq: 900, Hash: "e900", HLC: fmt.Sprintf("%016x", (f.clk.Now().UnixMilli()+5000)<<16),
		Intent: gsync.PIIEraseIntent, At: f.clk.Now(), Actor: "service:sync", PIIErase: []gsync.OpRef{{Origin: op.Origin, Stream: op.Stream, Seq: op.Seq}}}
	if err := f.svc.Project(f.sys(tenantA), erase); err != nil {
		t.Fatal(err)
	}
	for _, w := range f.store.PII(tenantA) {
		if w.Key.Entity == "credit_note" && (len(w.Fields) != 0 || w.Commits["customer_gstin"] != cGstin) {
			t.Fatalf("after erasure: values gone, commitments kept: %+v", w)
		}
	}
	after := digest(f)
	if after.StateHash != before.StateHash || hash(after) != hash(before) {
		t.Fatalf("erasure changed the digest: %+v -> %+v", before, after)
	}
	// a node that never held the values (commitments only) has the same digest
	g, _ := setup(false)
	if d := digest(g); d.StateHash != before.StateHash {
		t.Fatalf("digest without values %s, with values %s", d.StateHash, before.StateHash)
	}
}
