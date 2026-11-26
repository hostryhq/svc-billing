package service_test

import (
	"testing"

	"github.com/hostryhq/svc-billing/service"
)

// A value-reducing re-bill beyond the discount limit plus the comp allowance is the manager-approval case of ruling 8 (a).
func TestRebillReductionBeyondTheDiscountAndCompAllowanceIsFlaggedHigh(t *testing.T) {
	f := newFixture(t)
	f.applyAt("cloud-1", 91, 5000, "settings.limits.set", service.Mut{E: "billing_settings", ID: settingsRow(), K: "insert",
		F: map[string]any{"number_at": "settle", "discount_limit_bp": 100, "comp_allowance_paise": 0}}) // 1 percent of 50400 = 504
	f.apply(tenantA, deviceA, outletStream, billMuts(billID(68), number(1), 1200, false)...)
	f.rebill(t, 4, billID(68), billID(69), 0) // value drops by 2400
	if ex := f.exceptions("rebill_value_reduction_over_allowance"); len(ex) != 1 || ex[0].Severity != "high" {
		t.Fatalf("exceptions %+v", ex)
	}
}
