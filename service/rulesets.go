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
	"github.com/hostryhq/go-common/ids"
	"github.com/hostryhq/go-common/tenancy"

	"github.com/hostryhq/svc-billing/internal/gen"
	"github.com/hostryhq/svc-billing/internal/tstamp"
	"github.com/hostryhq/svc-billing/repository"
	"github.com/hostryhq/svc-billing/tax"
)

// ---- the vendor approval grant (ADR-0014 A1.3) ----

// Grant is the verified content of a vendor approval grant: signed and single-use, issued by identity at the second
// person's approval. The claim names are provisional (api-specs common/vendor.yaml fixes the meaning, not the keys).
type Grant struct {
	ID          string
	ApprovalID  string
	Kind        string
	Requester   string
	Approver    string
	SubjectHash string
	ExpiresAt   time.Time
}

// Errors of a grant check; the service maps them to 403 grant.expired and grant.required.
var (
	ErrGrantInvalid = errors.New("service: grant invalid")
	ErrGrantExpired = errors.New("service: grant expired")
)

// GrantVerifier checks a grant's signature and expiry.
type GrantVerifier interface {
	Verify(ctx context.Context, raw string) (Grant, error)
}

// PublishSubjectHash is the subject hash of a tax_rule_set_publish grant: rule-set id, content hash and effective date.
func PublishSubjectHash(ruleSetID, contentHash, effectiveDate string) string {
	sum := sha256.Sum256([]byte("tax_rule_set_publish\n" + ruleSetID + "\n" + contentHash + "\n" + effectiveDate))
	return hex.EncodeToString(sum[:])
}

// ErrPrecondition is a stale If-Match (412 precondition_failed).
var ErrPrecondition = errors.New("service: precondition failed")

func conflict(code, detail string) error { return apperr.New(apperr.Conflict, code, detail) }

func grantErr(code string) error {
	return apperr.New(apperr.Forbidden, code, "the approval grant was refused")
}

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

// PublishTaxRuleSet publishes a signed version under a vendor approval grant: the grant is verified, its subject hash
// recomputed, its id recorded as used, the lead time checked on this service's clock, the version marked published and
// one emission queued per known organisation, all in one transaction (A1.3, A1.4).
func (s *Service) PublishTaxRuleSet(ctx context.Context, id, rawGrant string) (gen.TaxRuleSetMaster, error) {
	if rawGrant == "" || s.grants == nil {
		return gen.TaxRuleSetMaster{}, grantErr("grant.required")
	}
	g, err := s.grants.Verify(ctx, rawGrant)
	switch {
	case errors.Is(err, ErrGrantExpired):
		return gen.TaxRuleSetMaster{}, grantErr("grant.expired")
	case err != nil:
		return gen.TaxRuleSetMaster{}, grantErr("grant.required")
	}
	p, _ := auth.PrincipalFrom(ctx)
	return runGlobal(ctx, s, func(ctx context.Context, tx repository.GlobalTx) (gen.TaxRuleSetMaster, error) {
		m, err := s.getMaster(ctx, tx, id)
		if err != nil {
			return m, err
		}
		if m.State != gen.TaxRuleSetMasterStateSigned {
			return m, apperr.New(apperr.Conflict, "not_signed", "only a signed version can be published")
		}
		if g.Kind != "tax_rule_set_publish" || g.Requester == g.Approver || (p.Subject != "" && p.Subject != g.Approver) ||
			g.SubjectHash != PublishSubjectHash(m.Id, deref(m.ContentHash), m.EffectiveDate.String()) {
			return m, grantErr("grant.subject_mismatch")
		}
		now := s.clock.Now()
		if !g.ExpiresAt.IsZero() && now.After(g.ExpiresAt) {
			return m, grantErr("grant.expired")
		}
		if m.EffectiveDate.Time.Before(now.Add(s.lead).Truncate(24 * time.Hour)) {
			return m, validation("effective_date_too_soon", "the effective date is closer than the lead time", field("effective_date", "too_soon"))
		}
		first, err := tx.UseGrant(ctx, g.ID, now)
		if err != nil {
			return m, err
		}
		if !first {
			return m, grantErr("grant.used")
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
			if err := tx.QueueEmission(ctx, m.Id, t); err != nil {
				return m, err
			}
		}
		if err := s.publishPlatform(ctx, tx, "billing.tax_rule_set.published", m.Id, p.Subject, map[string]any{"rule_set_id": m.Id, "version": m.Version,
			"effective_date": m.EffectiveDate.String(), "content_hash": deref(m.ContentHash), "review_status": m.ReviewStatus}); err != nil {
			return m, err
		}
		data := map[string]any{"approval_id": g.ApprovalID, "action": "tax_rule_set_publish", "vendor_user_id": p.Subject, "result": "success"}
		if err := s.publishPlatform(ctx, tx, "billing.vendor_access.recorded", g.ApprovalID, p.Subject, data); err != nil {
			return m, err
		}
		return s.withCounts(ctx, tx, m)
	})
}

// ---- emission ----

func emitMuts(tenant string, m gen.TaxRuleSetMaster) (string, []Mut) {
	rowID := RuleSetRowID(tenant, m.Version)
	muts := []Mut{{E: "tax_rule_set", ID: rowID, K: "insert", F: map[string]any{"version": m.Version, "name": m.Name, "effective_date": m.EffectiveDate.String(),
		"calc_v": m.CalcV, "review_status": string(m.ReviewStatus), "content_hash": deref(m.ContentHash)}}}
	for _, r := range m.Rules {
		f := map[string]any{"rule_set_id": rowID, "tax_class": r.TaxClass, "regime": string(r.Regime), "specified_premises": r.SpecifiedPremises,
			"order_channel": string(r.OrderChannel), "doc_type": string(r.DocType), "gst_applies": r.GstApplies, "cgst_bp": r.CgstBp, "sgst_bp": r.SgstBp, "igst_bp": r.IgstBp}
		if r.Sac != nil {
			f["sac"] = *r.Sac
		}
		id := uuid5("tax_rule", rowID+"|"+r.TaxClass+"|"+string(r.Regime)+"|"+boolStr(r.SpecifiedPremises)+"|"+string(r.OrderChannel))
		muts = append(muts, Mut{E: "tax_rule", ID: id, K: "insert", F: f})
	}
	return rowID, muts
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// DeliverEmissions emits every queued rule-set row (one op per organisation, op id UUIDv5(tenant, version), so a
// retry cannot duplicate) and marks it acknowledged when the gateway accepted it. It is run on a timer by the process
// and directly by tests; it returns how many were acknowledged.
func (s *Service) DeliverEmissions(ctx context.Context, batch int) (int, error) {
	var pending []repository.Emission
	if err := s.repo.InGlobalTx(ctx, func(ctx context.Context, tx repository.GlobalTx) error {
		var err error
		pending, err = tx.PendingEmissions(ctx, batch)
		return err
	}); err != nil {
		return 0, err
	}
	n := 0
	for _, e := range pending {
		var m gen.TaxRuleSetMaster
		if err := s.repo.InGlobalTx(ctx, func(ctx context.Context, tx repository.GlobalTx) error {
			var err error
			m, err = s.getMaster(ctx, tx, e.RuleSetID)
			return err
		}); err != nil {
			return n, err
		}
		rowID, muts := emitMuts(e.TenantID, m)
		if _, err := s.emitter.Emit(ctx, EmitOp{TenantID: e.TenantID, Stream: "tenant", OpID: rowID, Intent: "cloud.emit", Muts: muts}); err != nil {
			s.log.Warn("rule-set emission failed; it stays queued", "rule_set", e.RuleSetID, "error", err)
			continue
		}
		if err := s.repo.InGlobalTx(ctx, func(ctx context.Context, tx repository.GlobalTx) error {
			_, err := tx.AckEmission(ctx, e.RuleSetID, e.TenantID, s.clock.Now())
			return err
		}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
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
				if err := tx.QueueEmission(ctx, v.Id, tenant); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// RecordOutlet stores what tenancy says of an outlet: its registration and regime.
func (s *Service) RecordOutlet(ctx context.Context, o OutletRec) error {
	return s.repo.InTx(ctx, func(ctx context.Context, tx repository.Tx) error {
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
