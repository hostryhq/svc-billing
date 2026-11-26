package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/apperr"
	"github.com/hostryhq/go-common/emit"
	"github.com/hostryhq/go-common/ids"
	gsync "github.com/hostryhq/go-common/sync"
	"github.com/hostryhq/go-common/tenancy"

	"github.com/hostryhq/svc-billing/internal/gen"
	"github.com/hostryhq/svc-billing/internal/tstamp"
	"github.com/hostryhq/svc-billing/repository"
	"github.com/hostryhq/svc-billing/tax"
)

// ---- the vendor approval grant (ADR-0014 A1.3, A2.6) ----

// GrantKindPublish is the grant kind this service executes.
const GrantKindPublish = "tax_rule_set_publish"

// GrantAudience is the audience (aud) of the grants billing executes.
const GrantAudience = "billing"

// GrantVerifier is go-auth's vendor approval grant verifier (ADR-0014 A2.6): *auth.GrantVerifier implements it, and the
// double signs real grants under its own key and verifies them with the same code. billing leaves the verifier's single-use
// hook (Used) unset and consumes the jti itself in the publication's transaction (GlobalTx.UseGrant), so the grant, the
// state change and the emissions commit or roll back together.
type GrantVerifier interface {
	Verify(ctx context.Context, token string, want auth.GrantWant) (*auth.GrantClaims, error)
}

// PublishSubjectHash is the subject hash of a tax_rule_set_publish grant (A2.6 item 2): go-auth's SubjectHash of the kind,
// the rule-set id, the content hash and the effective date. The service computes it from its own stored version, never from
// a value the caller sends.
func PublishSubjectHash(ruleSetID, contentHash, effectiveDate string) string {
	return auth.SubjectHash(GrantKindPublish, ruleSetID, contentHash, effectiveDate)
}

// ErrPrecondition is a stale If-Match (412 precondition_failed).
var ErrPrecondition = errors.New("service: precondition failed")

func conflict(code, detail string) error { return apperr.New(apperr.Conflict, code, detail) }

// ---- stored master ----

func masterOf(m repository.Master) (gen.TaxRuleSetMaster, error) {
	var out gen.TaxRuleSetMaster
	err := json.Unmarshal(m.Body, &out)
	return out, err
}

func toRepo(m gen.TaxRuleSetMaster) repository.Master {
	b, _ := json.Marshal(m)
	return repository.Master{ID: m.Id, Version: m.Version, Revision: m.Revision, State: string(m.State), Created: m.CreatedAt.Time, Body: b}
}

var taxClassPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)

func (s *Service) withCounts(ctx context.Context, tx repository.GlobalTx, m gen.TaxRuleSetMaster) (gen.TaxRuleSetMaster, error) {
	if m.State != gen.TaxRuleSetMasterStatePublished {
		return m, nil
	}
	t, a, err := tx.EmissionCounts(ctx, m.Id)
	if err != nil {
		return m, err
	}
	m.OrganisationsTotal, m.OrganisationsAcknowledged = &t, &a
	return m, nil
}

func ruleRows(rs []gen.TaxRule) []tax.RuleRow {
	out := make([]tax.RuleRow, 0, len(rs))
	for _, x := range rs {
		out = append(out, tax.RuleRow{TaxClass: x.TaxClass, Regime: string(x.Regime), SpecifiedPremises: x.SpecifiedPremises, OrderChannel: string(x.OrderChannel),
			DocType: string(x.DocType), GSTApplies: x.GstApplies, CGSTbp: int64(x.CgstBp), SGSTbp: int64(x.SgstBp), IGSTbp: int64(x.IgstBp), SAC: deref(x.Sac)})
	}
	return out
}

// checkDraft is the schema-level validation of a draft body (rule-set.schema.yaml).
func checkDraft(d gen.TaxRuleSetDraft) error {
	var fs []apperr.FieldError
	if n := strings.TrimSpace(d.Name); n == "" || len(n) > 80 {
		fs = append(fs, field("name", "invalid"))
	}
	if d.CalcV < 1 {
		fs = append(fs, field("calc_v", "invalid"))
	}
	if len(d.Rules) == 0 {
		fs = append(fs, field("rules", "required"))
	}
	for _, r := range d.Rules {
		if !taxClassPattern.MatchString(r.TaxClass) {
			fs = append(fs, field("rules.tax_class", "invalid"))
			break
		}
	}
	if len(fs) > 0 {
		return validation("validation", "invalid rule set", fs...)
	}
	return nil
}

// ContentHash is the SHA-256 of the canonical content of a version: name, effective date, calc_v and rules.
func ContentHash(m gen.TaxRuleSetMaster) string {
	b, _ := json.Marshal(map[string]any{"name": m.Name, "effective_date": m.EffectiveDate.String(), "calc_v": m.CalcV, "rules": m.Rules})
	sum := sha256.Sum256(canonical(b))
	return hex.EncodeToString(sum[:])
}

// ---- routes ----

// ListTaxRuleSetMasters lists versions newest first with emit progress (counts only).
func (s *Service) ListTaxRuleSetMasters(ctx context.Context, state, after string, limit int) (Page[gen.TaxRuleSetMaster], error) {
	return runGlobal(ctx, s, func(ctx context.Context, tx repository.GlobalTx) (Page[gen.TaxRuleSetMaster], error) {
		ms, err := tx.ListMasters(ctx, state, after, limit+1)
		if err != nil {
			return Page[gen.TaxRuleSetMaster]{}, err
		}
		var out Page[gen.TaxRuleSetMaster]
		for i, m := range ms {
			if i == limit {
				out.Next = ms[limit-1].ID
				break
			}
			v, err := masterOf(m)
			if err != nil {
				return out, err
			}
			if v, err = s.withCounts(ctx, tx, v); err != nil {
				return out, err
			}
			out.Items = append(out.Items, v)
		}
		return out, nil
	})
}

func (s *Service) getMaster(ctx context.Context, tx repository.GlobalTx, id string) (gen.TaxRuleSetMaster, error) {
	m, err := tx.GetMaster(ctx, id)
	if err != nil {
		return gen.TaxRuleSetMaster{}, mapNotFound(err)
	}
	return masterOf(m)
}

// GetTaxRuleSetMaster reads one version with emit progress.
func (s *Service) GetTaxRuleSetMaster(ctx context.Context, id string) (gen.TaxRuleSetMaster, error) {
	return runGlobal(ctx, s, func(ctx context.Context, tx repository.GlobalTx) (gen.TaxRuleSetMaster, error) {
		m, err := s.getMaster(ctx, tx, id)
		if err != nil {
			return m, err
		}
		return s.withCounts(ctx, tx, m)
	})
}

// CreateTaxRuleSetDraft creates draft version max+1.
func (s *Service) CreateTaxRuleSetDraft(ctx context.Context, d gen.TaxRuleSetDraft) (gen.TaxRuleSetMaster, error) {
	if err := checkDraft(d); err != nil {
		return gen.TaxRuleSetMaster{}, err
	}
	return runGlobal(ctx, s, func(ctx context.Context, tx repository.GlobalTx) (gen.TaxRuleSetMaster, error) {
		all, err := tx.ListMasters(ctx, "", "", 0)
		if err != nil {
			return gen.TaxRuleSetMaster{}, err
		}
		for _, m := range all {
			v, err := masterOf(m)
			if err != nil {
				return v, err
			}
			if v.State != gen.TaxRuleSetMasterStateDraft && v.EffectiveDate.String() == d.EffectiveDate.String() {
				return gen.TaxRuleSetMaster{}, conflict("conflict", "a signed or published version already has this effective date")
			}
		}
		ver, err := tx.NextVersion(ctx)
		if err != nil {
			return gen.TaxRuleSetMaster{}, err
		}
		m := gen.TaxRuleSetMaster{Id: s.mintID(), Version: ver, Revision: 1, Name: d.Name, EffectiveDate: d.EffectiveDate, CalcV: d.CalcV, State: gen.TaxRuleSetMasterStateDraft,
			ReviewStatus: gen.TaxRuleSetMasterReviewStatusUnreviewed, Rules: d.Rules, CreatedAt: tstamp.Of(s.clock.Now())}
		return m, tx.PutMaster(ctx, toRepo(m))
	})
}

// UpdateTaxRuleSetDraft edits a draft; ifMatch is the revision from the last read (an ETag such as "7").
func (s *Service) UpdateTaxRuleSetDraft(ctx context.Context, id, ifMatch string, d gen.TaxRuleSetDraft) (gen.TaxRuleSetMaster, error) {
	if err := checkDraft(d); err != nil {
		return gen.TaxRuleSetMaster{}, err
	}
	return runGlobal(ctx, s, func(ctx context.Context, tx repository.GlobalTx) (gen.TaxRuleSetMaster, error) {
		m, err := s.getMaster(ctx, tx, id)
		if err != nil {
			return m, err
		}
		if m.State != gen.TaxRuleSetMasterStateDraft {
			return m, conflict("conflict", "only a draft can be edited")
		}
		if strings.Trim(ifMatch, `"`) != itoa(m.Revision) {
			return m, ErrPrecondition
		}
		m.Name, m.EffectiveDate, m.CalcV, m.Rules = d.Name, d.EffectiveDate, d.CalcV, d.Rules
		m.Revision++
		return m, tx.PutMaster(ctx, toRepo(m))
	})
}

// DeleteTaxRuleSetDraft deletes a draft. A signed or published version is never deleted.
func (s *Service) DeleteTaxRuleSetDraft(ctx context.Context, id string) error {
	_, err := runGlobal(ctx, s, func(ctx context.Context, tx repository.GlobalTx) (struct{}, error) {
		m, err := s.getMaster(ctx, tx, id)
		if err != nil {
			return struct{}{}, err
		}
		if m.State != gen.TaxRuleSetMasterStateDraft {
			return struct{}{}, conflict("conflict", "only a draft can be deleted")
		}
		return struct{}{}, tx.DeleteMaster(ctx, id)
	})
	return err
}

// validate runs BR-A1-82: the structural rules, and every conformance vector through the engine.
func validate(m gen.TaxRuleSetMaster) gen.TaxRuleSetValidation {
	rs := tax.RuleSet{ID: m.Id, Version: m.Version, Name: m.Name, EffectiveDate: m.EffectiveDate.String(), CalcV: m.CalcV, Rules: ruleRows(m.Rules)}
	out := gen.TaxRuleSetValidation{Errors: []struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}{}}
	for _, p := range rs.Validate() {
		out.Errors = append(out.Errors, struct {
			Code   string `json:"code"`
			Detail string `json:"detail"`
		}{p.Code, p.Detail})
	}
	res, err := tax.RunVectors()
	if err != nil {
		out.Errors = append(out.Errors, struct {
			Code   string `json:"code"`
			Detail string `json:"detail"`
		}{"vectors_unavailable", err.Error()})
	}
	out.VectorsTotal = len(res)
	for _, r := range res {
		if r.Error == "" {
			out.VectorsPassed++
		} else {
			out.Errors = append(out.Errors, struct {
				Code   string `json:"code"`
				Detail string `json:"detail"`
			}{"vector_failed", r.ID + ": " + r.Error})
		}
	}
	out.Valid = len(out.Errors) == 0
	return out
}

// ValidateTaxRuleSet validates a draft; valid or not, the call succeeds.
func (s *Service) ValidateTaxRuleSet(ctx context.Context, id string) (gen.TaxRuleSetValidation, error) {
	return runGlobal(ctx, s, func(ctx context.Context, tx repository.GlobalTx) (gen.TaxRuleSetValidation, error) {
		m, err := s.getMaster(ctx, tx, id)
		if err != nil {
			return gen.TaxRuleSetValidation{}, err
		}
		if m.State != gen.TaxRuleSetMasterStateDraft {
			return gen.TaxRuleSetValidation{}, conflict("conflict", "only a draft is validated")
		}
		return validate(m), nil
	})
}

// SignTaxRuleSet validates again, stores the CA sign-off and the content hash, and makes the version immutable.
func (s *Service) SignTaxRuleSet(ctx context.Context, id string, so gen.SignOff) (gen.TaxRuleSetMaster, error) {
	if len(strings.TrimSpace(so.ReviewerName)) < 2 || len(strings.TrimSpace(so.ReferenceNote)) < 10 {
		return gen.TaxRuleSetMaster{}, validation("validation", "invalid sign-off", field("signoff", "invalid"))
	}
	return runGlobal(ctx, s, func(ctx context.Context, tx repository.GlobalTx) (gen.TaxRuleSetMaster, error) {
		m, err := s.getMaster(ctx, tx, id)
		if err != nil {
			return m, err
		}
		if m.State != gen.TaxRuleSetMasterStateDraft {
			return m, conflict("conflict", "only a draft can be signed")
		}
		if v := validate(m); !v.Valid {
			fs := []apperr.FieldError{}
			for _, e := range v.Errors {
				fs = append(fs, field("rules", e.Code))
			}
			return m, validation("validation", "the draft does not validate", fs...)
		}
		m.State, m.ReviewStatus, m.Signoff = gen.TaxRuleSetMasterStateSigned, gen.TaxRuleSetMasterReviewStatusReviewed, &so
		h := ContentHash(m)
		m.ContentHash = &h
		return m, tx.PutMaster(ctx, toRepo(m))
	})
}

// effectiveStartIST is the instant a rule set's effective date begins: the date is an IST business date, so it starts at
// 00:00 +05:30 (A1.4 item 3). No truncation: the lead time is measured to this instant.
func effectiveStartIST(d time.Time) time.Time {
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, istZone)
}

// PublishTaxRuleSet publishes a signed version under a vendor approval grant: the grant is verified, its subject hash
// recomputed, its id recorded as used, the lead time checked on this service's clock, the version marked published and
// one emission queued per known organisation, all in one transaction (A1.3, A1.4).
func (s *Service) PublishTaxRuleSet(ctx context.Context, id, rawGrant string) (gen.TaxRuleSetMaster, error) {
	if rawGrant == "" || s.grants == nil {
		return gen.TaxRuleSetMaster{}, apperr.New(apperr.Forbidden, auth.CodeGrantRequired, "a vendor approval grant is required")
	}
	p, _ := auth.PrincipalFrom(ctx)
	now := s.clock.Now()
	return runGlobal(ctx, s, func(ctx context.Context, tx repository.GlobalTx) (gen.TaxRuleSetMaster, error) {
		m, err := s.getMaster(ctx, tx, id)
		if err != nil {
			return m, err
		}
		if m.State != gen.TaxRuleSetMasterStateSigned {
			return m, apperr.New(apperr.Conflict, "not_signed", "only a signed version can be published")
		}
		// Every claim check of A2.6 item 4 is go-auth's, in its order (signature and key id, typ, realm, iss, aud, kind, org,
		// approver = the bearer's subject, lifetime, exp), then the subject hash that this service recomputes from the stored
		// version. Single use follows in this transaction.
		g, err := s.grants.Verify(ctx, rawGrant, auth.GrantWant{Kind: GrantKindPublish, Org: "", Approver: p.Subject,
			SubjectHash: func() (string, error) {
				return PublishSubjectHash(m.Id, deref(m.ContentHash), m.EffectiveDate.String()), nil
			}})
		if err != nil {
			return m, err
		}
		if effectiveStartIST(m.EffectiveDate.Time).Before(now.Add(s.lead)) {
			return m, validation("effective_date_too_soon", "the effective date is closer than the lead time", field("effective_date", "too_soon"))
		}
		first, err := tx.UseGrant(ctx, g.JTI, now)
		if err != nil {
			return m, err
		}
		if !first {
			return m, apperr.New(apperr.Forbidden, auth.CodeGrantUsed, "vendor approval grant was already used")
		}
		m.State, m.PublishedAt = gen.TaxRuleSetMasterStatePublished, tstamp.Ptr(now)
		if err := tx.PutMaster(ctx, toRepo(m)); err != nil {
			return m, err
		}
		tenants, err := tx.Tenants(ctx)
		if err != nil {
			return m, err
		}
		for _, t := range tenants {
			if err := s.queueRuleSet(ctx, tx, m, t, "support:"+p.Subject, g.ApprovalID, now); err != nil {
				return m, err
			}
		}
		if err := s.publishPlatform(ctx, tx, "billing.tax_rule_set.published", m.Id, p.Subject, map[string]any{"rule_set_id": m.Id, "version": m.Version,
			"effective_date": m.EffectiveDate.String(), "content_hash": deref(m.ContentHash), "review_status": m.ReviewStatus}); err != nil {
			return m, err
		}
		data := map[string]any{"approval_id": g.ApprovalID, "action": "tax_rule_set_publish", "grant_jti": g.JTI, "vendor_user_id": p.Subject, "result": "success"}
		if err := s.publishPlatform(ctx, tx, "billing.vendor_access.recorded", g.ApprovalID, p.Subject, data); err != nil {
			return m, err
		}
		out, err := s.withCounts(ctx, tx, m)
		// the emissions were queued just now: none is acknowledged yet, so the publication does not state a count of them
		out.OrganisationsAcknowledged = nil
		return out, err
	})
}

// ---- emission ----

// ruleSetMuts are the mutations that carry one published version to an organisation's tenant stream: the tax_rule_set
// row and one tax_rule row per rule, with ids derived from their natural keys so a repeated emit converges on the same
// rows (entities/restaurant/tax_rule_set.yaml, tax_rule.yaml).
func ruleSetMuts(tenant string, m gen.TaxRuleSetMaster) []Mut {
	rowID := RuleSetRowID(tenant, m.Version)
	muts := []Mut{{E: "tax_rule_set", ID: rowID, K: gsync.KindInsert, F: map[string]any{"version": int64(m.Version), "name": m.Name, "effective_date": m.EffectiveDate.String(),
		"calc_v": int64(m.CalcV), "review_status": string(m.ReviewStatus), "content_hash": deref(m.ContentHash)}}}
	for _, r := range m.Rules {
		f := map[string]any{"rule_set_id": rowID, "tax_class": r.TaxClass, "regime": string(r.Regime), "specified_premises": r.SpecifiedPremises,
			"order_channel": string(r.OrderChannel), "doc_type": string(r.DocType), "gst_applies": r.GstApplies, "cgst_bp": int64(r.CgstBp), "sgst_bp": int64(r.SgstBp), "igst_bp": int64(r.IgstBp)}
		if r.Sac != nil {
			f["sac"] = *r.Sac
		}
		id := uuid5("tax_rule", rowID+"|"+r.TaxClass+"|"+string(r.Regime)+"|"+boolStr(r.SpecifiedPremises)+"|"+string(r.OrderChannel))
		muts = append(muts, Mut{E: "tax_rule", ID: id, K: gsync.KindInsert, F: f})
	}
	return muts
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// queueRuleSet records that a published version is owed to an organisation and queues its op in emit_queue, in the
// caller's transaction (ADR-0014 A2.2: the key is the UUIDv5 of tenant and version, so the publication, a later
// organisation and a restore all converge on one op per organisation). A publication carries the vendor approver and the
// verified grant's approval id; the emission to an organisation created later is a system emit.
func (s *Service) queueRuleSet(ctx context.Context, tx repository.GlobalTx, m gen.TaxRuleSetMaster, tenant, actor, approval string, now time.Time) error {
	key := emit.DeterministicKey("tax.rule_set.publish", tenant, itoa(m.Version))
	op := emit.Op{Key: key, Stream: "tenant:" + tenant, Intent: "tax.rule_set.publish", IntentV: 1, Muts: ruleSetMuts(tenant, m), Actor: "service:" + Name}
	if approval != "" {
		by := "admin.taxrules.publish"
		op.Actor, op.AuthorisedBy, op.Approval = actor, &by, approval
	}
	if err := tx.QueueEmission(ctx, m.Id, tenant, key); err != nil {
		return err
	}
	return tx.EnqueueEmit(ctx, now, tenant, op)
}

// RecordOrganisation is called on tenancy's creation event: the organisation becomes an audience of rule-set
// emissions, and every published version in force or still to come is queued for it (BR-A1-120).
func (s *Service) RecordOrganisation(ctx context.Context, tenant string) error {
	return s.repo.InGlobalTx(ctx, func(ctx context.Context, tx repository.GlobalTx) error {
		if err := tx.AddTenant(ctx, tenant, s.clock.Now()); err != nil {
			return err
		}
		ms, err := tx.ListMasters(ctx, string(gen.TaxRuleSetMasterStatePublished), "", 0)
		if err != nil {
			return err
		}
		today := istDate(s.clock.Now())
		var current string
		var vs []gen.TaxRuleSetMaster
		for _, m := range ms {
			v, err := masterOf(m)
			if err != nil {
				return err
			}
			vs = append(vs, v)
			if e := v.EffectiveDate.String(); e <= today && e > current {
				current = e
			}
		}
		for _, v := range vs {
			if v.EffectiveDate.String() >= current {
				if err := s.queueRuleSet(ctx, tx, v, tenant, "", "", s.clock.Now()); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// RecordOutlet stores what tenancy says of an outlet: its registration and regime.
//
// The registration and regime are kept as a history (M3): a change opens a new era from its effective IST date (the day
// billing is told when the fact names none), and a fact that repeats the current registration and regime opens nothing, so a
// replayed tenancy event is harmless. Documents are registered and verified under the era of their invoice date.
func (s *Service) RecordOutlet(ctx context.Context, o OutletRec) error {
	return s.repo.InTx(ctx, func(ctx context.Context, tx repository.Tx) error {
		eras := []OutletEra{{RegistrationID: o.RegistrationID, Regime: o.Regime}}
		if prev, err := load[OutletRec](ctx, tx, repository.KindOutlet, o.OutletID); err == nil {
			eras = prev.Eras
			if len(eras) == 0 {
				eras = []OutletEra{{RegistrationID: prev.RegistrationID, Regime: prev.Regime}}
			}
			last := eras[len(eras)-1]
			if last.RegistrationID != o.RegistrationID || last.Regime != o.Regime {
				from := o.EffectiveFrom
				if from == "" {
					from = istDate(s.clock.Now())
				}
				eras = append(eras, OutletEra{From: from, RegistrationID: o.RegistrationID, Regime: o.Regime})
			}
		} else if err != repository.ErrNotFound {
			return err
		}
		o.Eras, o.EffectiveFrom = eras, ""
		return tx.SaveDoc(ctx, repository.Doc{Kind: repository.KindOutlet, ID: o.OutletID, Outlet: o.OutletID, Body: marshalDoc(o)})
	})
}

// SealRevisions runs FlushRevisions for every organisation billing knows; it returns how many revisions it sealed.
func (s *Service) SealRevisions(ctx context.Context) (int, error) {
	var tenants []string
	if err := s.repo.InGlobalTx(ctx, func(ctx context.Context, tx repository.GlobalTx) error {
		var err error
		tenants, err = tx.Tenants(ctx)
		return err
	}); err != nil {
		return 0, err
	}
	total := 0
	for _, t := range tenants {
		n, err := s.FlushRevisions(tenancy.With(ctx, tenancy.Context{TenantID: ids.ID(t), ActorID: "billing"}))
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}
