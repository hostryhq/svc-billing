package tax_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/hostryhq/svc-billing/tax"
)

func specsDir(t testing.TB) string {
	t.Helper()
	if d := os.Getenv("API_SPECS_DIR"); d != "" {
		return d
	}
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/hostryhq/api-specs").Output()
	if err != nil {
		t.Skipf("api-specs not resolvable: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// Every shared conformance vector must be reproduced to the paisa (ADR-0014 R6.1): a difference blocks merge.
func TestConformanceVectors(t *testing.T) {
	res, err := tax.RunVectors()
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 64 {
		t.Fatalf("%d vectors embedded, the shared file has 64", len(res))
	}
	for _, r := range res {
		t.Run(r.ID, func(t *testing.T) {
			if r.Error != "" {
				t.Fatal(r.Error)
			}
		})
	}
}

// The embedded copy must equal the file of the pinned api-specs, so the two cannot drift.
func TestEmbeddedVectorsMatchSpec(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(specsDir(t), "tax/conformance/vectors.v1.json"))
	if err != nil {
		t.Skipf("spec vectors not readable: %v", err)
	}
	if !bytes.Equal(b, tax.VectorBytes()) {
		t.Fatal("tax/vectors/vectors.v1.json differs from api-specs tax/conformance/vectors.v1.json; run make gen")
	}
}

func TestDraftRuleSetValidates(t *testing.T) {
	var rs tax.RuleSet
	var doc map[string]any
	if err := yaml.Unmarshal(tax.DraftYAML(), &doc); err != nil {
		t.Fatal(err)
	}
	j, _ := json.Marshal(doc)
	if err := json.Unmarshal(j, &rs); err != nil {
		t.Fatal(err)
	}
	if p := rs.Validate(); len(p) != 0 {
		t.Fatalf("the api-specs example draft must validate: %v", p)
	}
}

func TestRuleSetValidationFindsEachDefect(t *testing.T) {
	ok := tax.RuleRow{TaxClass: "fnb_standard", Regime: "regular", OrderChannel: "dine_in", DocType: "tax_invoice", GSTApplies: true, CGSTbp: 250, SGSTbp: 250}
	cases := map[string]struct {
		mut  func(rs *tax.RuleSet)
		code string
	}{
		"unequal halves":   {func(rs *tax.RuleSet) { rs.Rules[0].SGSTbp = 300 }, "components_unequal"},
		"rate without gst": {func(rs *tax.RuleSet) { rs.Rules[0].GSTApplies = false }, "rate_without_gst"},
		"igst":             {func(rs *tax.RuleSet) { rs.Rules[0].IGSTbp = 100 }, "igst_not_supported"},
		"duplicate":        {func(rs *tax.RuleSet) { rs.Rules = append(rs.Rules, rs.Rules[0]) }, "duplicate_mapping"},
		"missing variant": {func(rs *tax.RuleSet) {
			r := rs.Rules[0]
			r.SpecifiedPremises, r.TaxClass = true, "exempt"
			r.CGSTbp, r.SGSTbp = 0, 0
			rs.Rules = append(rs.Rules, r)
		}, "missing_mapping"},
		"unknown calc_v": {func(rs *tax.RuleSet) { rs.CalcV = 9 }, "calc_v_unsupported"},
		"no rules":       {func(rs *tax.RuleSet) { rs.Rules = nil }, "no_rules"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			rs := tax.RuleSet{CalcV: 1, Rules: []tax.RuleRow{ok}}
			c.mut(&rs)
			for _, p := range rs.Validate() {
				if p.Code == c.code {
					return
				}
			}
			t.Fatalf("want a %s problem, got %v", c.code, rs.Validate())
		})
	}
}

func TestInForceTakesLatestEffectiveNotAfterDate(t *testing.T) {
	sets := []tax.RuleSet{{ID: "a", Version: 1, EffectiveDate: "2026-04-01"}, {ID: "b", Version: 2, EffectiveDate: "2026-12-15"}}
	for date, want := range map[string]string{"2026-11-24": "a", "2026-12-15": "b", "2027-01-01": "b"} {
		got, ok := tax.InForce(sets, date)
		if !ok || got.ID != want {
			t.Errorf("%s: got %q, want %q", date, got.ID, want)
		}
	}
	if _, ok := tax.InForce(sets, "2026-03-31"); ok {
		t.Error("nothing is in force before the first effective date")
	}
}

// An unknown tax class under the regular regime is an error, never silently non-GST (it could under-tax a bill).
func TestUnknownTaxClassIsRefusedUnderRegular(t *testing.T) {
	b := tax.Bill{Regime: "regular", PriceMode: "exclusive", Rules: map[string]tax.Rule{"fnb_standard": {GST: true, CGSTbp: 250, SGSTbp: 250}},
		Lines: []tax.Line{{ID: "a", QtyMilli: 1000, UnitPricePaise: 10000, TaxClass: "typo_class"}}}
	if _, err := tax.Calculate(b); !errors.Is(err, tax.ErrUnknownTaxClass) {
		t.Fatalf("got %v", err)
	}
	if _, err := tax.CalculateCredit(b, []tax.CreditLine{{LineID: "a", QtyMilli: 1000}}); !errors.Is(err, tax.ErrUnknownTaxClass) {
		t.Fatalf("credit: got %v", err)
	}
	b.Regime = "unregistered" // no GST is computed there, so the class does not matter
	if _, err := tax.Calculate(b); err != nil {
		t.Fatalf("unregistered must not need rules: %v", err)
	}
}
