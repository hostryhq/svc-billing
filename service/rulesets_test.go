package service_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-auth/authtest"
	"github.com/hostryhq/go-common/apperr"
	"github.com/hostryhq/go-common/emit"

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
	if v, err := svc.ValidateTaxRuleSet(f.sys(tenantA), m.Id); err != nil || !v.Valid || v.VectorsTotal != 65 || v.VectorsPassed != 65 {
		t.Fatalf("validation %+v %v", v, err)
	}
	m, err = svc.SignTaxRuleSet(f.sys(tenantA), m.Id, gen.SignOff{ReviewerName: "A. Reviewer", SignedOn: m.EffectiveDate, ReferenceNote: "CA advice note 2026-11 ref 12"})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

const approver = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a78"

// vendorCtx is the request context of the bearer of the publish call. The service reads only the bearer's subject (the
// approver check of A2.6); the vendor realm and its audit row belong to the guard and to the contract and integration
// suites, so a plain principal with that subject stands in here.
func (f *fixture) vendorCtx(subject string) context.Context {
	return f.iss.ContextFor(context.Background(), f.cat, authtest.Spec{Type: auth.TypeUser, Tenant: tenantA, Subject: subject, Grants: authtest.OwnerGrants()})
}

func grantFor(subject string) service.Grant {
	return service.Grant{ID: "g", ApprovalID: "a1", Kind: service.GrantKindPublish, Type: "vendor_approval", Realm: "vendor", Audience: "billing",
		Requester: "u2", Approver: approver, SubjectHash: subject}
}

// The grant is checked in the order of ADR-0014 A2.6 item 4, each refusal with its own code, and a good one publishes.
func TestPublicationNeedsAGrantForTheSubjectAndTheLeadTime(t *testing.T) {
	f := newFixture(t)
	grants := fakeGrants{}
	svc := service.New(f.store, service.Options{Clock: f.clk, Log: slog.New(slog.DiscardHandler), Grants: grants})
	ctx := f.vendorCtx(approver)
	for _, tn := range []string{tenantA, tenantB} {
		if err := svc.RecordOrganisation(ctx, tn); err != nil {
			t.Fatal(err)
		}
	}
	m := signed(t, f, svc, "2026-12-15")
	subject := service.PublishSubjectHash(m.Id, *m.ContentHash, "2026-12-15")
	mod := func(fn func(*service.Grant)) service.Grant { g := grantFor(subject); fn(&g); return g }
	grants["good"] = grantFor(subject)
	grants["wrong-aud"] = mod(func(g *service.Grant) { g.Audience = "tenancy" })
	grants["wrong-type"] = mod(func(g *service.Grant) { g.Type = "access" })
	grants["wrong-realm"] = mod(func(g *service.Grant) { g.Realm = "tenant" })
	grants["wrong-kind"] = mod(func(g *service.Grant) { g.Kind = "org_suspend" })
	grants["with-org"] = mod(func(g *service.Grant) { g.Org = tenantA })
	grants["other-approver"] = mod(func(g *service.Grant) { g.Approver = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4aff" })
	grants["expired-and-wrong-kind"] = mod(func(g *service.Grant) { g.ExpiresAt, g.Kind = f.clk.Now().Add(-time.Minute), "org_suspend" })
	grants["other-subject"] = mod(func(g *service.Grant) { g.SubjectHash = "deadbeef" })
	grants["same-person"] = mod(func(g *service.Grant) { g.Requester = approver })

	_, err := svc.PublishTaxRuleSet(ctx, m.Id, "")
	wantCode(t, err, "grant.required")
	for _, bad := range []string{"unknown", "wrong-aud", "wrong-type", "wrong-realm", "wrong-kind", "with-org", "other-approver"} {
		_, err = svc.PublishTaxRuleSet(ctx, m.Id, bad)
		if !hasCode(err, "grant.invalid") {
			t.Fatalf("%s: %v, want grant.invalid", bad, err)
		}
	}
	// exp is checked before kind: an expired grant of the wrong kind says expired
	_, err = svc.PublishTaxRuleSet(ctx, m.Id, "expired-and-wrong-kind")
	wantCode(t, err, "grant.expired")
	_, err = svc.PublishTaxRuleSet(ctx, m.Id, "other-subject")
	wantCode(t, err, "grant.subject_mismatch")
	_, err = svc.PublishTaxRuleSet(ctx, m.Id, "same-person")
	wantCode(t, err, "grant.subject_mismatch")
	// the bearer must be the approver
	_, err = svc.PublishTaxRuleSet(f.vendorCtx("0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4aee"), m.Id, "good")
	if !hasCode(err, "grant.invalid") {
		t.Fatalf("a bearer who is not the approver: %v", err)
	}

	pub, err := svc.PublishTaxRuleSet(ctx, m.Id, "good")
	if err != nil || pub.State != gen.TaxRuleSetMasterStatePublished || *pub.OrganisationsTotal != 2 {
		t.Fatalf("%+v %v", pub, err)
	}
	// a grant is single use: the version is now published, so the replay is refused by the guard before the grant is read
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
}

// Publication queues one op per organisation in emit_queue in its own transaction (key UUIDv5 of tenant and version); the
// worker sends them, a second drain sends nothing, the acknowledged count follows the queue, and an organisation created
// later is queued the version in force or to come.
func TestPublicationQueuesOneEmitPerOrganisation(t *testing.T) {
	f := newFixture(t)
	grants := fakeGrants{}
	svc := service.New(f.store, service.Options{Clock: f.clk, Log: slog.New(slog.DiscardHandler), Grants: grants})
	ctx := f.vendorCtx(approver)
	for _, tn := range []string{tenantA, tenantB} {
		if err := svc.RecordOrganisation(ctx, tn); err != nil {
			t.Fatal(err)
		}
	}
	m := signed(t, f, svc, "2026-12-15")
	grants["good"] = grantFor(service.PublishSubjectHash(m.Id, *m.ContentHash, "2026-12-15"))
	pub, err := svc.PublishTaxRuleSet(ctx, m.Id, "good")
	if err != nil {
		t.Fatal(err)
	}
	q := f.store.Queued()
	if len(q) != 2 || pub.OrganisationsAcknowledged != nil {
		t.Fatalf("queued %d, acknowledged %v (the publication states no count of acknowledged emits yet)", len(q), pub.OrganisationsAcknowledged)
	}
	for _, r := range q {
		want := emit.DeterministicKey("tax.rule_set.publish", r.Tenant, fmt.Sprint(pub.Version))
		if r.Op.Key != want || r.Op.Intent != "tax.rule_set.publish" || r.Op.Stream != "tenant:"+r.Tenant || r.Op.Muts[0].E != "tax_rule_set" ||
			r.Op.Muts[0].ID != service.RuleSetRowID(r.Tenant, pub.Version) || r.Op.Approval != "a1" || r.Op.AuthorisedBy == nil || *r.Op.AuthorisedBy != "admin.taxrules.publish" {
			t.Fatalf("queued op %+v", r.Op)
		}
	}
	gw := &fakeEmitter{fail: emit.NewUnavailable(errors.New("down"), 0)}
	if n, err := f.store.DrainEmits(ctx, gw); err != nil || n != 0 {
		t.Fatalf("a drain while the gateway is down sent %d (%v)", n, err)
	}
	gw.fail = nil
	if n, err := f.store.DrainEmits(ctx, gw); err != nil || n != 2 {
		t.Fatalf("drained %d %v", n, err)
	}
	if n, _ := f.store.DrainEmits(ctx, gw); n != 0 || len(gw.ops) != 2 {
		t.Fatalf("second drain %d, ops %d", n, len(gw.ops))
	}
	if got, _ := svc.GetTaxRuleSetMaster(ctx, m.Id); *got.OrganisationsAcknowledged != 2 {
		t.Fatalf("acknowledged %v", *got.OrganisationsAcknowledged)
	}
	if err := svc.RecordOrganisation(ctx, "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4c60"); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.GetTaxRuleSetMaster(ctx, m.Id); *got.OrganisationsTotal != 3 || len(f.store.Queued()) != 3 {
		t.Fatalf("total after a new organisation %v, queued %d", *got.OrganisationsTotal, len(f.store.Queued()))
	}
}

func TestPublicationUnderTheLeadTimeIsRefused(t *testing.T) {
	f := newFixture(t)
	grants := fakeGrants{}
	svc := service.New(f.store, service.Options{Clock: f.clk, Log: slog.New(slog.DiscardHandler), Grants: grants})
	m := signed(t, f, svc, "2026-11-24") // effective today: closer than 24 hours
	grants["g"] = grantFor(service.PublishSubjectHash(m.Id, *m.ContentHash, "2026-11-24"))
	_, err := svc.PublishTaxRuleSet(f.vendorCtx(approver), m.Id, "g")
	wantCode(t, err, "effective_date_too_soon")
	// the refusal used no grant, and the version stays signed
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

func hasCode(err error, code string) bool {
	var ae *apperr.Error
	return errors.As(err, &ae) && ae.Code == code
}
