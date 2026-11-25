package service_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/hostryhq/svc-billing/internal/gen"
	"github.com/hostryhq/svc-billing/service"
)

type fakeGrants map[string]service.Grant

func (g fakeGrants) Verify(_ context.Context, raw string) (service.Grant, error) {
	if gr, ok := g[raw]; ok {
		return gr, nil
	}
	return service.Grant{}, service.ErrGrantInvalid
}

func draft(effective string) gen.TaxRuleSetDraft {
	d, _ := time.Parse("2006-01-02", effective)
	return gen.TaxRuleSetDraft{Name: "GST 2026 B", EffectiveDate: openapi_types.Date{Time: d}, CalcV: 1, Rules: []gen.TaxRule{{TaxClass: "fnb_standard", Regime: "regular", OrderChannel: "dine_in",
		DocType: "tax_invoice", GstApplies: true, CgstBp: 250, SgstBp: 250}}}
}

func signed(t *testing.T, f *fixture, svc *service.Service, effective string) gen.TaxRuleSetMaster {
	t.Helper()
	m, err := svc.CreateTaxRuleSetDraft(f.sys(tenantA), draft(effective))
	if err != nil {
		t.Fatal(err)
	}
	if v, err := svc.ValidateTaxRuleSet(f.sys(tenantA), m.Id); err != nil || !v.Valid || v.VectorsTotal != 64 || v.VectorsPassed != 64 {
		t.Fatalf("validation %+v %v", v, err)
	}
	m, err = svc.SignTaxRuleSet(f.sys(tenantA), m.Id, gen.SignOff{ReviewerName: "A. Reviewer", SignedOn: m.EffectiveDate, ReferenceNote: "CA advice note 2026-11 ref 12"})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPublicationNeedsAGrantForTheSubjectAndTheLeadTime(t *testing.T) {
	f := newFixture(t)
	grants := fakeGrants{}
	em := &fakeEmitter{}
	svc := service.New(f.store, service.Options{Clock: f.clk, Log: slog.New(slog.DiscardHandler), Grants: grants, Emitter: em})
	ctx := f.sys(tenantA)
	for _, tn := range []string{tenantA, tenantB} {
		if err := svc.RecordOrganisation(ctx, tn); err != nil {
			t.Fatal(err)
		}
	}
	m := signed(t, f, svc, "2026-12-15")
	subject := service.PublishSubjectHash(m.Id, *m.ContentHash, "2026-12-15")
	good := service.Grant{ID: "g1", ApprovalID: "a1", Kind: "tax_rule_set_publish", Requester: "u2", Approver: "u1", SubjectHash: subject}
	grants["good"] = good
	grants["other-subject"] = service.Grant{ID: "g2", Kind: "tax_rule_set_publish", Requester: "u2", Approver: "u1", SubjectHash: "deadbeef"}
	grants["same-person"] = service.Grant{ID: "g3", Kind: "tax_rule_set_publish", Requester: "u1", Approver: "u1", SubjectHash: subject}
	grants["expired"] = service.Grant{ID: "g4", Kind: "tax_rule_set_publish", Requester: "u2", Approver: "u1", SubjectHash: subject, ExpiresAt: f.clk.Now().Add(-time.Minute)}

	_, err := svc.PublishTaxRuleSet(ctx, m.Id, "")
	wantCode(t, err, "grant.required")
	_, err = svc.PublishTaxRuleSet(ctx, m.Id, "unknown")
	wantCode(t, err, "grant.required")
	_, err = svc.PublishTaxRuleSet(ctx, m.Id, "other-subject")
	wantCode(t, err, "grant.subject_mismatch")
	_, err = svc.PublishTaxRuleSet(ctx, m.Id, "same-person")
	wantCode(t, err, "grant.subject_mismatch")
	_, err = svc.PublishTaxRuleSet(ctx, m.Id, "expired")
	wantCode(t, err, "grant.expired")

	pub, err := svc.PublishTaxRuleSet(ctx, m.Id, "good")
	if err != nil || pub.State != gen.TaxRuleSetMasterStatePublished || *pub.OrganisationsTotal != 2 {
		t.Fatalf("%+v %v", pub, err)
	}
	// a grant is single use
	_, err = svc.PublishTaxRuleSet(ctx, m.Id, "good")
	wantCode(t, err, "not_signed")
	// a published version is never edited or deleted
	if _, err := svc.UpdateTaxRuleSetDraft(ctx, m.Id, `"1"`, draft("2026-12-15")); err == nil {
		t.Fatal("a published version was edited")
	}
	if err := svc.DeleteTaxRuleSetDraft(ctx, m.Id); err == nil {
		t.Fatal("a published version was deleted")
	}
	var types []string
	for _, e := range f.store.Events() {
		if e.TenantID.String() == service.PlatformTenant {
			types = append(types, e.Type)
		}
	}
	if count(types, "billing.tax_rule_set.published") != 1 || count(types, "billing.vendor_access.recorded") != 1 {
		t.Fatalf("platform events %v", types)
	}
	// one emit per organisation, op id UUIDv5(tenant, version); a second delivery emits nothing more
	if n, err := svc.DeliverEmissions(ctx, 10); err != nil || n != 2 {
		t.Fatalf("delivered %d %v", n, err)
	}
	if n, _ := svc.DeliverEmissions(ctx, 10); n != 0 || len(em.ops) != 2 {
		t.Fatalf("second delivery %d, ops %d", n, len(em.ops))
	}
	seen := map[string]bool{}
	for _, op := range em.ops {
		if op.OpID != service.RuleSetRowID(op.TenantID, pub.Version) || seen[op.OpID] || op.Muts[0].E != "tax_rule_set" {
			t.Fatalf("op %+v", op)
		}
		seen[op.OpID] = true
	}
	if got, _ := svc.GetTaxRuleSetMaster(ctx, m.Id); *got.OrganisationsAcknowledged != 2 {
		t.Fatalf("acknowledged %v", *got.OrganisationsAcknowledged)
	}
	// an organisation created later receives the version in force or to come
	if err := svc.RecordOrganisation(ctx, "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4c60"); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.GetTaxRuleSetMaster(ctx, m.Id); *got.OrganisationsTotal != 3 {
		t.Fatalf("total after a new organisation %v", *got.OrganisationsTotal)
	}
}

func TestPublicationUnderTheLeadTimeIsRefused(t *testing.T) {
	f := newFixture(t)
	grants := fakeGrants{}
	svc := service.New(f.store, service.Options{Clock: f.clk, Log: slog.New(slog.DiscardHandler), Grants: grants})
	m := signed(t, f, svc, "2026-11-24") // effective today: closer than 24 hours
	grants["g"] = service.Grant{ID: "g", Kind: "tax_rule_set_publish", Requester: "u2", Approver: "u1", SubjectHash: service.PublishSubjectHash(m.Id, *m.ContentHash, "2026-11-24")}
	_, err := svc.PublishTaxRuleSet(f.sys(tenantA), m.Id, "g")
	wantCode(t, err, "effective_date_too_soon")
	// the refusal used no grant: the same grant is still usable once the date is far enough (a new version is signed)
	if got, _ := svc.GetTaxRuleSetMaster(f.sys(tenantA), m.Id); got.State != gen.TaxRuleSetMasterStateSigned {
		t.Fatalf("state %v", got.State)
	}
}

func TestSigningRefusesAnInvalidDraft(t *testing.T) {
	f := newFixture(t)
	svc := service.New(f.store, service.Options{Clock: f.clk, Log: slog.New(slog.DiscardHandler)})
	d := draft("2026-12-20")
	d.Rules[0].SgstBp = 300 // not two equal halves
	m, err := svc.CreateTaxRuleSetDraft(f.sys(tenantA), d)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := svc.ValidateTaxRuleSet(f.sys(tenantA), m.Id)
	if v.Valid || !strings.Contains(v.Errors[0].Code, "components_unequal") {
		t.Fatalf("validation %+v", v)
	}
	_, err = svc.SignTaxRuleSet(f.sys(tenantA), m.Id, gen.SignOff{ReviewerName: "A. Reviewer", SignedOn: m.EffectiveDate, ReferenceNote: "CA advice note 2026-11 ref 12"})
	wantCode(t, err, "validation")
	if _, err := svc.UpdateTaxRuleSetDraft(f.sys(tenantA), m.Id, `"9"`, draft("2026-12-20")); err == nil {
		t.Fatal("a stale If-Match must be refused")
	}
}
