package service_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
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

	"github.com/hostryhq/svc-billing/double"
	"github.com/hostryhq/svc-billing/internal/gen"
	"github.com/hostryhq/svc-billing/service"
)

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

// publishGrant signs a publish grant of the double's vendor realm for subject (requester u2, approver the bearer, approval a1),
// after edit changed a claim to make a refusal.
func publishGrant(g *double.Grants, subject string, edit func(*auth.GrantClaims)) string {
	c := g.Claims("0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a66", approver, subject)
	c.ApprovalID = "a1"
	if edit != nil {
		edit(&c)
	}
	return g.Mint(c)
}

// The grant is verified by go-auth under the vendor key (each refusal with its own code, claims in its order), the subject hash is
// recomputed from the stored version, and a good one publishes.
func TestPublicationNeedsAGrantForTheSubjectAndTheLeadTime(t *testing.T) {
	f := newFixture(t)
	grants := double.NewGrants(f.clk)
	svc := service.New(f.store, service.Options{Clock: f.clk, Log: slog.New(slog.DiscardHandler), Grants: grants})
	ctx := f.vendorCtx(approver)
	for _, tn := range []string{tenantA, tenantB} {
		if err := svc.RecordOrganisation(ctx, tn); err != nil {
			t.Fatal(err)
		}
	}
	m := signed(t, f, svc, "2026-12-15")
	subject := service.PublishSubjectHash(m.Id, *m.ContentHash, "2026-12-15")
	tok := func(edit func(*auth.GrantClaims)) string { return publishGrant(grants, subject, edit) }
	now := f.clk.Now()

	_, err := svc.PublishTaxRuleSet(ctx, m.Id, "")
	wantCode(t, err, auth.CodeGrantRequired)
	invalid := map[string]string{
		"garbage":        "not-a-grant",
		"unknown kid":    grants.MintKid("other-key", grants.Claims("u2", approver, subject)),
		"wrong-aud":      tok(func(c *auth.GrantClaims) { c.Audience = "tenancy" }),
		"wrong-issuer":   tok(func(c *auth.GrantClaims) { c.Issuer = "https://evil.invalid" }),
		"wrong-type":     tok(func(c *auth.GrantClaims) { c.Type = "access" }),
		"wrong-realm":    tok(func(c *auth.GrantClaims) { c.Realm = auth.RealmTenant }),
		"wrong-kind":     tok(func(c *auth.GrantClaims) { c.Kind = "org_suspend" }),
		"with-org":       tok(func(c *auth.GrantClaims) { c.Org = tenantA }),
		"other-approver": tok(func(c *auth.GrantClaims) { c.Approver = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4aff" }),
		"same-person":    tok(func(c *auth.GrantClaims) { c.Requester = c.Approver }),
		"too-long":       tok(func(c *auth.GrantClaims) { c.ExpiresAt = c.IssuedAt + 301 }),
	}
	for name, raw := range invalid {
		_, err = svc.PublishTaxRuleSet(ctx, m.Id, raw)
		if !hasCode(err, auth.CodeGrantInvalid) {
			t.Fatalf("%s: %v, want %s", name, err, auth.CodeGrantInvalid)
		}
	}
	// exp is checked before the subject: an expired grant says expired
	_, err = svc.PublishTaxRuleSet(ctx, m.Id, tok(func(c *auth.GrantClaims) {
		c.IssuedAt, c.ExpiresAt, c.SubjectHash = now.Add(-6*time.Minute).Unix(), now.Add(-time.Minute).Unix(), "deadbeef"
	}))
	wantCode(t, err, auth.CodeGrantExpired)
	_, err = svc.PublishTaxRuleSet(ctx, m.Id, tok(func(c *auth.GrantClaims) { c.SubjectHash = "deadbeef" }))
	wantCode(t, err, auth.CodeGrantSubjectMismatch)
	// the bearer must be the approver
	good := tok(nil)
	_, err = svc.PublishTaxRuleSet(f.vendorCtx("0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4aee"), m.Id, good)
	if !hasCode(err, auth.CodeGrantInvalid) {
		t.Fatalf("a bearer who is not the approver: %v", err)
	}

	pub, err := svc.PublishTaxRuleSet(ctx, m.Id, good)
	if err != nil || pub.State != gen.TaxRuleSetMasterStatePublished || *pub.OrganisationsTotal != 2 {
		t.Fatalf("%+v %v", pub, err)
	}
	// the version is now published, so a replay is refused for its state; the grant is single use, so the same jti on another
	// signed version is refused as used
	_, err = svc.PublishTaxRuleSet(ctx, m.Id, good)
	wantCode(t, err, "not_signed")
	m2 := signed(t, f, svc, "2026-12-20")
	var c2 auth.GrantClaims
	c2 = grants.Claims("0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a66", approver, service.PublishSubjectHash(m2.Id, *m2.ContentHash, "2026-12-20"))
	c2.JTI = jtiOf(t, good)
	_, err = svc.PublishTaxRuleSet(ctx, m2.Id, grants.Mint(c2))
	wantCode(t, err, auth.CodeGrantUsed)
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
	grants := double.NewGrants(f.clk)
	svc := service.New(f.store, service.Options{Clock: f.clk, Log: slog.New(slog.DiscardHandler), Grants: grants})
	ctx := f.vendorCtx(approver)
	for _, tn := range []string{tenantA, tenantB} {
		if err := svc.RecordOrganisation(ctx, tn); err != nil {
			t.Fatal(err)
		}
	}
	m := signed(t, f, svc, "2026-12-15")
	pub, err := svc.PublishTaxRuleSet(ctx, m.Id, publishGrant(grants, service.PublishSubjectHash(m.Id, *m.ContentHash, "2026-12-15"), nil))
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
	grants := double.NewGrants(f.clk)
	svc := service.New(f.store, service.Options{Clock: f.clk, Log: slog.New(slog.DiscardHandler), Grants: grants})
	m := signed(t, f, svc, "2026-11-24") // effective today: closer than 24 hours
	_, err := svc.PublishTaxRuleSet(f.vendorCtx(approver), m.Id, publishGrant(grants, service.PublishSubjectHash(m.Id, *m.ContentHash, "2026-11-24"), nil))
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

// jtiOf reads the jti claim of a compact grant (the signature was checked by the verifier).
func jtiOf(t *testing.T, raw string) string {
	t.Helper()
	parts := strings.Split(raw, ".")
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var c auth.GrantClaims
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c.JTI
}

func hasCode(err error, code string) bool {
	var ae *apperr.Error
	return errors.As(err, &ae) && ae.Code == code
}
