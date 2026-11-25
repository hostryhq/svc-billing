package service

import (
	"context"
	"regexp"
	"time"

	"github.com/hostryhq/go-common/apperr"

	"github.com/hostryhq/svc-billing/internal/gen"
	"github.com/hostryhq/svc-billing/internal/tstamp"
	"github.com/hostryhq/svc-billing/repository"
)

// settingsRec is the stored billing_settings row of an outlet: the API view plus the two fields the recompute needs.
type settingsRec struct {
	Settings        gen.BillingSettings `json:"settings"`
	PackingTaxClass string              `json:"packing_tax_class,omitempty"`
}

// DefaultSettings are the settings of an outlet that has never changed any (service charge off, settle-time numbering).
func DefaultSettings(outlet string) gen.BillingSettings {
	return gen.BillingSettings{OutletId: outlet, NumberAt: "settle", PriceMode: "exclusive", RoundOff: true, ServiceChargeBp: 0, DiscountLimitBp: 1000,
		RebillAllowancePerShift: 3, CompAllowance: money(50000), OpenItemLimit: money(50000), ManualEntryMaxDaysBack: 2, ShiftRequired: true, HandoverCount: true}
}

// config applies billing_settings, outlet_tax_profile and tender_kind mutations: last writer wins by (hlc, origin).
func (ac *applyCtx) config(ctx context.Context) error {
	for _, m := range ac.g["billing_settings"] {
		f := m.F
		d, err := ac.tx.GetDoc(ctx, repository.KindSettings, ac.outlet)
		rec := settingsRec{Settings: DefaultSettings(ac.outlet)}
		if err == nil {
			if rec, err = unmarshalDoc[settingsRec](d); err != nil {
				return err
			}
		} else if err != repository.ErrNotFound {
			return err
		}
		if d.Stamp != "" && ac.op.stamp() < d.Stamp {
			continue
		}
		s := &rec.Settings
		if fhas(f, "number_at") {
			s.NumberAt = gen.BillingSettingsNumberAt(fstr(f, "number_at"))
		}
		if fhas(f, "price_mode") {
			s.PriceMode = gen.BillingSettingsPriceMode(fstr(f, "price_mode"))
		}
		if fhas(f, "round_off") {
			s.RoundOff = fbool(f, "round_off")
		}
		if fhas(f, "service_charge_bp") {
			s.ServiceChargeBp = int(fint(f, "service_charge_bp"))
		}
		if fhas(f, "discount_limit_bp") {
			s.DiscountLimitBp = int(fint(f, "discount_limit_bp"))
		}
		if fhas(f, "rebill_allowance_per_shift") {
			s.RebillAllowancePerShift = int(fint(f, "rebill_allowance_per_shift"))
		}
		if fhas(f, "comp_allowance_paise") {
			s.CompAllowance = money(fint(f, "comp_allowance_paise"))
		}
		if fhas(f, "open_item_limit_paise") {
			s.OpenItemLimit = money(fint(f, "open_item_limit_paise"))
		}
		if fhas(f, "manual_entry_max_days_back") {
			s.ManualEntryMaxDaysBack = int(fint(f, "manual_entry_max_days_back"))
		}
		if fhas(f, "shift_required") {
			s.ShiftRequired = fbool(f, "shift_required")
		}
		if fhas(f, "handover_count") {
			s.HandoverCount = fbool(f, "handover_count")
		}
		if fhas(f, "packing_tax_class") {
			rec.PackingTaxClass = fstr(f, "packing_tax_class")
		}
		s.UpdatedAt = tsPtr(ac.at())
		if err := ac.tx.SaveDoc(ctx, repository.Doc{Kind: repository.KindSettings, ID: ac.outlet, Outlet: ac.outlet, Stamp: ac.op.stamp(), Body: marshalDoc(rec)}); err != nil {
			return err
		}
		if err := ac.reverifyPending(ctx); err != nil {
			return err
		}
	}
	for _, m := range ac.g["outlet_tax_profile"] {
		f := m.F
		fy := fstr(f, "financial_year")
		d, err := ac.tx.GetDoc(ctx, repository.KindTaxProfile, m.ID)
		p := gen.OutletTaxProfile{OutletId: ac.outlet, FinancialYear: fy}
		if err == nil {
			if p, err = unmarshalDoc[gen.OutletTaxProfile](d); err != nil {
				return err
			}
			fy = p.FinancialYear
		} else if err != repository.ErrNotFound {
			return err
		}
		if d.Stamp != "" && ac.op.stamp() < d.Stamp {
			continue
		}
		if fhas(f, "specified_premises") {
			p.SpecifiedPremises = fbool(f, "specified_premises")
		}
		if fhas(f, "fssai_number") {
			p.FssaiNumber = ptr(fstr(f, "fssai_number"))
		}
		p.UpdatedAt = tsPtr(ac.at())
		if err := ac.tx.SaveDoc(ctx, repository.Doc{Kind: repository.KindTaxProfile, ID: m.ID, Outlet: ac.outlet, Date: "", Sort: fy, K1: fy, Stamp: ac.op.stamp(), Body: marshalDoc(p)}); err != nil {
			return err
		}
		if err := ac.reverifyPending(ctx); err != nil {
			return err
		}
	}
	for _, m := range ac.g["tender_kind"] {
		f := m.F
		d, err := ac.tx.GetDoc(ctx, repository.KindTender, m.ID)
		t := gen.TenderKind{Id: m.ID, OutletId: ac.outlet, Kind: gen.TenderKindKind(fstr(f, "kind")), Enabled: true}
		if err == nil {
			if t, err = unmarshalDoc[gen.TenderKind](d); err != nil {
				return err
			}
		} else if err != repository.ErrNotFound {
			return err
		}
		if d.Stamp != "" && ac.op.stamp() < d.Stamp {
			continue
		}
		if fhas(f, "name") {
			t.Name = fstr(f, "name")
		}
		if fhas(f, "enabled") {
			t.Enabled = fbool(f, "enabled")
		}
		if err := ac.tx.SaveDoc(ctx, repository.Doc{Kind: repository.KindTender, ID: m.ID, Outlet: ac.outlet, Sort: t.Name, Stamp: ac.op.stamp(), Body: marshalDoc(t)}); err != nil {
			return err
		}
	}
	return nil
}

// ruleSets applies the tax_rule_set and tax_rule rows emitted to this organisation (cloud-origin, immutable).
func (ac *applyCtx) ruleSets(ctx context.Context) error {
	for _, m := range ac.g["tax_rule_set"] {
		f := m.F
		rec := RuleSetRec{ID: m.ID, Version: int(fint(f, "version")), Name: fstr(f, "name"), EffectiveDate: fstr(f, "effective_date"), CalcV: int(fint(f, "calc_v")),
			ReviewStatus: fstr(f, "review_status"), ContentHash: fstr(f, "content_hash"), PublishedAt: ac.at().String(), Rules: []gen.TaxRule{}}
		for _, r := range ac.g["tax_rule"] {
			if fstr(r.F, "rule_set_id") != m.ID {
				continue
			}
			x := gen.TaxRule{TaxClass: fstr(r.F, "tax_class"), Regime: gen.TaxRuleRegime(fstr(r.F, "regime")), SpecifiedPremises: fbool(r.F, "specified_premises"),
				OrderChannel: gen.TaxRuleOrderChannel(fstr(r.F, "order_channel")), DocType: gen.BillDocType(fstr(r.F, "doc_type")), GstApplies: fbool(r.F, "gst_applies"),
				CgstBp: int(fint(r.F, "cgst_bp")), SgstBp: int(fint(r.F, "sgst_bp")), IgstBp: int(fint(r.F, "igst_bp"))}
			if v := fstr(r.F, "sac"); v != "" {
				x.Sac = &v
			}
			rec.Rules = append(rec.Rules, x)
		}
		created, err := ac.tx.PutDoc(ctx, repository.Doc{Kind: repository.KindRuleSet, ID: m.ID, Date: rec.EffectiveDate, Sort: rec.EffectiveDate, Body: marshalDoc(rec)})
		if err != nil {
			return err
		}
		if created {
			if err := ac.reverifyPending(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

// reverifyPending recomputes the documents of this outlet that were stored as "pending" because a rule set or profile
// had not arrived yet. A document is still never altered: only its verification result is stated.
func (ac *applyCtx) reverifyPending(ctx context.Context) error {
	own := ac.outlet
	defer func() { ac.outlet = own }()
	var scope []string
	if own != "" {
		scope = []string{own}
	}
	recs, docs, err := loadAll[BillRec](ctx, ac.tx, repository.Query{Kind: repository.KindBill, Outlets: scope})
	if err != nil {
		return err
	}
	for i, r := range recs {
		if r.Detail.Verification.Status != gen.VerificationStatusPending {
			continue
		}
		ac.outlet = docs[i].Outlet
		v, err := ac.verifyBill(ctx, r)
		if err != nil || v.Status == gen.VerificationStatusPending {
			if err != nil {
				return err
			}
			continue
		}
		r.Detail.Verification = v
		docs[i].Body = marshalDoc(r)
		if err := ac.tx.SaveDoc(ctx, docs[i]); err != nil {
			return err
		}
		if err := ac.flag(ctx, "bill", r.Detail.Id, docs[i].Outlet, v, r.Detail.BusinessDate.String()); err != nil {
			return err
		}
	}
	notes, ndocs, err := loadAll[CreditRec](ctx, ac.tx, repository.Query{Kind: repository.KindCreditNote, Outlets: scope})
	if err != nil {
		return err
	}
	for i, r := range notes {
		if r.Note.Verification.Status != gen.VerificationStatusPending {
			continue
		}
		ac.outlet = ndocs[i].Outlet
		v, err := ac.verifyCredit(ctx, r)
		if err != nil {
			return err
		}
		if v.Status == gen.VerificationStatusPending {
			continue
		}
		r.Note.Verification = v
		ndocs[i].Body = marshalDoc(r)
		if err := ac.tx.SaveDoc(ctx, ndocs[i]); err != nil {
			return err
		}
		if err := ac.flag(ctx, "credit_note", r.Note.Id, ndocs[i].Outlet, v, r.Note.BusinessDate.String()); err != nil {
			return err
		}
	}
	return nil
}

// ---- reads and strong-session writes ----

// GetOutletBillingConfig returns an outlet's tax profiles (newest year first), settings and tenders.
func (s *Service) GetOutletBillingConfig(ctx context.Context, outlet string) (gen.OutletBillingConfig, error) {
	if err := s.requireOutlet(ctx, "settings.read", outlet); err != nil {
		return gen.OutletBillingConfig{}, err
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (gen.OutletBillingConfig, error) {
		if err := s.knownOutlet(ctx, tx, outlet); err != nil {
			return gen.OutletBillingConfig{}, err
		}
		out := gen.OutletBillingConfig{OutletId: outlet, Settings: DefaultSettings(outlet), TaxProfiles: []gen.OutletTaxProfile{}, Tenders: []gen.TenderKind{}}
		if rec, err := load[settingsRec](ctx, tx, repository.KindSettings, outlet); err == nil {
			out.Settings = rec.Settings
		}
		ps, _, err := loadAll[gen.OutletTaxProfile](ctx, tx, repository.Query{Kind: repository.KindTaxProfile, Outlets: []string{outlet}})
		if err != nil {
			return out, err
		}
		out.TaxProfiles = append(out.TaxProfiles, ps...)
		ts, _, err := loadAll[gen.TenderKind](ctx, tx, repository.Query{Kind: repository.KindTender, Outlets: []string{outlet}, Asc: true})
		if err != nil {
			return out, err
		}
		out.Tenders = append(out.Tenders, ts...)
		return out, nil
	})
}

func isNF(err error) bool { return err == repository.ErrNotFound }

var fyPattern = regexp.MustCompile(`^[0-9]{4}$`)

func (s *Service) emitConfig(ctx context.Context, outlet, idemOp string, id string, entity string, fields map[string]any) (gen.ConfigChange, error) {
	t, err := tenantOf(ctx)
	if err != nil {
		return gen.ConfigChange{}, err
	}
	key := ""
	if im, ok := idemFrom(ctx); ok {
		key = im.Key
	}
	op := EmitOp{TenantID: t.String(), Stream: "outlet:" + outlet, OpID: uuid5("emit|"+idemOp, outlet+"|"+key), Intent: "cloud.emit",
		Muts: []Mut{{E: entity, ID: id, K: "set", F: fields}}}
	opID, err := s.emitter.Emit(ctx, op)
	if err != nil {
		return gen.ConfigChange{}, err
	}
	return gen.ConfigChange{OutletId: outlet, UpdatedAt: tstamp.Of(s.clock.Now()), OpId: opID}, nil
}

// SetDiscountLimit sets the cashier discount limit (strong session). It emits a billing_settings config op.
func (s *Service) SetDiscountLimit(ctx context.Context, outlet string, bp int) (gen.ConfigChange, error) {
	if err := s.requireOutlet(ctx, "bill.discount.limit.edit", outlet); err != nil {
		return gen.ConfigChange{}, err
	}
	if bp < 0 || bp > 10000 {
		return gen.ConfigChange{}, validation("validation", "invalid discount limit", field("discount_limit_bp", "out_of_range"))
	}
	if err := s.check(ctx, outlet); err != nil {
		return gen.ConfigChange{}, err
	}
	return s.emitConfig(ctx, outlet, "setDiscountLimit", uuid5("billing_settings", outlet), "billing_settings", map[string]any{"discount_limit_bp": bp})
}

// SetNumberingMode chooses when a bill takes its number. It is refused while the outlet has an open check, which
// billing asks orders for; it is not on the trading path.
func (s *Service) SetNumberingMode(ctx context.Context, outlet, mode string) (gen.ConfigChange, error) {
	if err := s.requireOutlet(ctx, "settings.numbering.edit", outlet); err != nil {
		return gen.ConfigChange{}, err
	}
	if mode != "settle" && mode != "print" {
		return gen.ConfigChange{}, validation("validation", "invalid numbering mode", field("number_at", "invalid"))
	}
	if err := s.check(ctx, outlet); err != nil {
		return gen.ConfigChange{}, err
	}
	if s.orders == nil {
		return gen.ConfigChange{}, apperr.New(apperr.Unavailable, "orders.unavailable", "the open-check count is not available")
	}
	n, err := s.orders.OpenCheckCount(ctx, outlet)
	if err != nil {
		return gen.ConfigChange{}, apperr.Wrap(apperr.Unavailable, "orders.unavailable", "the open-check count is not available", err)
	}
	if n > 0 {
		return gen.ConfigChange{}, apperr.New(apperr.Conflict, "open_checks_exist", "the outlet has open checks")
	}
	return s.emitConfig(ctx, outlet, "setNumberingMode", uuid5("billing_settings", outlet), "billing_settings", map[string]any{"number_at": mode})
}

// check reads the outlet in a short transaction of its own: the emit must not run inside a transaction, because the
// gateway answers through the bus and the projection of the answer needs its own.
func (s *Service) check(ctx context.Context, outlet string) error {
	return s.repo.InTx(ctx, func(ctx context.Context, tx repository.Tx) error { return s.knownOutlet(ctx, tx, outlet) })
}

// retroactiveDays is how long after a financial year starts the specified-premises flag may still change.
const retroactiveDays = 30

// SetOutletTaxProfile sets the GST profile of an outlet for a financial year (strong session, online only).
func (s *Service) SetOutletTaxProfile(ctx context.Context, outlet, fy string, specified bool, fssai *string, priceMode *string) (gen.ConfigChange, error) {
	if err := s.requireOutlet(ctx, "settings.gst_profile.edit", outlet); err != nil {
		return gen.ConfigChange{}, err
	}
	if !fyPattern.MatchString(fy) {
		return gen.ConfigChange{}, validation("validation", "invalid financial year", field("financial_year", "invalid"))
	}
	if priceMode != nil && *priceMode != "exclusive" && *priceMode != "inclusive" {
		return gen.ConfigChange{}, validation("validation", "invalid price mode", field("price_mode", "invalid"))
	}
	var cur bool
	if err := s.repo.InTx(ctx, func(ctx context.Context, tx repository.Tx) error {
		if err := s.knownOutlet(ctx, tx, outlet); err != nil {
			return err
		}
		ps, _, err := loadAll[gen.OutletTaxProfile](ctx, tx, repository.Query{Kind: repository.KindTaxProfile, Outlets: []string{outlet}, K1: fy})
		cur = len(ps) > 0 && ps[0].SpecifiedPremises
		return err
	}); err != nil {
		return gen.ConfigChange{}, err
	}
	start, ok := fyStart(fy)
	if !ok {
		return gen.ConfigChange{}, validation("validation", "invalid financial year", field("financial_year", "invalid"))
	}
	if specified != cur && s.clock.Now().After(start.AddDate(0, 0, retroactiveDays)) {
		return gen.ConfigChange{}, validation("specified_premises_retroactive", "the flag cannot change this long after the financial year started", field("specified_premises", "retroactive"))
	}
	f := map[string]any{"financial_year": fy, "specified_premises": specified}
	if fssai != nil {
		f["fssai_number"] = *fssai
	}
	ch, err := s.emitConfig(ctx, outlet, "setOutletTaxProfile", uuid5("outlet_tax_profile", outlet+"|"+fy), "outlet_tax_profile", f)
	if err != nil {
		return ch, err
	}
	if priceMode != nil {
		if _, err := s.emitConfig(ctx, outlet, "setOutletTaxProfile.price_mode", uuid5("billing_settings", outlet), "billing_settings", map[string]any{"price_mode": *priceMode}); err != nil {
			return ch, err
		}
	}
	return ch, nil
}

func fyStart(fy string) (time.Time, bool) {
	if !fyPattern.MatchString(fy) {
		return time.Time{}, false
	}
	y := int(fy[0]-'0')*10 + int(fy[1]-'0')
	return time.Date(2000+y, time.April, 1, 0, 0, 0, 0, time.UTC), true
}

// knownOutlet checks the outlet exists in this tenant (tenancy told billing of it); another tenant's outlet is a 404.
func (s *Service) knownOutlet(ctx context.Context, tx repository.Tx, outlet string) error {
	_, err := tx.GetDoc(ctx, repository.KindOutlet, outlet)
	return mapNotFound(err)
}
