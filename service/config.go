package service

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/apperr"
	"github.com/hostryhq/go-common/emit"
	"github.com/hostryhq/go-common/idempotency"
	gsync "github.com/hostryhq/go-common/sync"

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

// config projects billing_settings, outlet_tax_profile and tender_kind from the rows the projection engine resolved
// (go-common sync, ADR-0014 A2.1): per field group the highest stamp (hlc, origin, seq, index) wins, whatever the arrival
// order, and a void of a refused op removes its candidates. A row the engine does not (or no longer) see as existing
// removes the stored document, which is how a voided first insert disappears.
func (ac *applyCtx) config(ctx context.Context) error {
	changed := false
	for _, r := range ac.rows["billing_settings"] {
		o := ac.outletOfRow(r.Key)
		changed = true
		if !r.Exists {
			if err := ac.tx.DeleteDoc(ctx, repository.KindSettings, o); err != nil {
				return err
			}
			continue
		}
		f := r.Fields
		rec := settingsRec{Settings: DefaultSettings(o)}
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
		if err := ac.tx.SaveDoc(ctx, repository.Doc{Kind: repository.KindSettings, ID: o, Outlet: o, Body: marshalDoc(rec)}); err != nil {
			return err
		}
	}
	for _, r := range ac.rows["outlet_tax_profile"] {
		o := ac.outletOfRow(r.Key)
		changed = true
		if !r.Exists {
			if err := ac.tx.DeleteDoc(ctx, repository.KindTaxProfile, r.Key.ID); err != nil {
				return err
			}
			continue
		}
		f := r.Fields
		fy := fstr(f, "financial_year")
		p := gen.OutletTaxProfile{OutletId: o, FinancialYear: fy, SpecifiedPremises: fbool(f, "specified_premises")}
		if fhas(f, "fssai_number") {
			p.FssaiNumber = ptr(fstr(f, "fssai_number"))
		}
		p.UpdatedAt = tsPtr(ac.at())
		if err := ac.tx.SaveDoc(ctx, repository.Doc{Kind: repository.KindTaxProfile, ID: r.Key.ID, Outlet: o, Date: "", Sort: fy, K1: fy, Body: marshalDoc(p)}); err != nil {
			return err
		}
	}
	for _, r := range ac.rows["tender_kind"] {
		o := ac.outletOfRow(r.Key)
		if !r.Exists {
			if err := ac.tx.DeleteDoc(ctx, repository.KindTender, r.Key.ID); err != nil {
				return err
			}
			continue
		}
		f := r.Fields
		t := gen.TenderKind{Id: r.Key.ID, OutletId: o, Kind: gen.TenderKindKind(fstr(f, "kind")), Enabled: true, Name: fstr(f, "name")}
		if fhas(f, "enabled") {
			t.Enabled = fbool(f, "enabled")
		}
		if err := ac.tx.SaveDoc(ctx, repository.Doc{Kind: repository.KindTender, ID: r.Key.ID, Outlet: o, Sort: t.Name, Body: marshalDoc(t)}); err != nil {
			return err
		}
	}
	if changed {
		return ac.reverifyPending(ctx)
	}
	return nil
}

// outletOfRow is the outlet a resolved row belongs to: the stream of this op for a row it mutates, the stream of the
// voided op for a row a void exposed.
func (ac *applyCtx) outletOfRow(k gsync.RowKey) string {
	for _, m := range ac.op.Muts {
		if m.E == k.Entity && m.ID == k.ID {
			return ac.outlet
		}
	}
	for _, v := range ac.op.Voids {
		if o, ok := strings.CutPrefix(v.Stream, "outlet:"); ok {
			return o
		}
	}
	return ac.outlet
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

// emitConfig sends one cloud-origin config op through the gateway emit and answers where it landed. It runs outside any
// transaction: the answer comes back through the bus and its projection needs a transaction of its own. The key is
// derived from the caller's Idempotency-Key (ADR-0014 A2.2 item 4), so a retried edit is one op; a retry with another
// body under the same key is the gateway's idempotency.mismatch (a defect in the caller, 500 emit.rejected). When the
// gateway is down nothing changed and the route answers 503 emit.unavailable.
func (s *Service) emitConfig(ctx context.Context, outlet, opName, intent, authorisedBy string, muts []Mut) (gen.ConfigChange, error) {
	t, err := tenantOf(ctx)
	if err != nil {
		return gen.ConfigChange{}, err
	}
	key := s.mintID()
	if im, ok := idemFrom(ctx); ok {
		key = im.Key
	}
	p, _ := auth.PrincipalFrom(ctx)
	by := authorisedBy
	op := emit.Op{Key: emit.DeriveKey(opName+"|"+outlet+"|"+key, 0), Stream: "outlet:" + outlet, Intent: intent, IntentV: 1, Muts: muts, Actor: p.Subject, AuthorisedBy: &by}
	res, err := s.emitter.Emit(ctx, t, []emit.Op{op})
	if err != nil {
		var rej *emit.RejectedError
		if errors.As(err, &rej) {
			s.log.Error("emit refused by the gateway: defect in billing", "intent", intent, "status", rej.Status, "code", rej.Code)
			return gen.ConfigChange{}, rej.AppError()
		}
		return gen.ConfigChange{}, err
	}
	return gen.ConfigChange{OutletId: outlet, UpdatedAt: tstamp.Of(s.clock.Now()), OpId: opID(res[0])}, nil
}

// opID is the identity of an emitted op as the API shows it: origin, stream and sequence, the outlet id shortened to its
// first eight characters as in the contract's example.
func opID(r emit.Result) string {
	stream := r.Stream
	if rest, ok := strings.CutPrefix(stream, "outlet:"); ok && len(rest) > 8 {
		stream = "outlet:" + rest[:8]
	}
	return r.Origin + "/" + stream + "/" + itoa(int(r.Seq))
}

// replayed answers a retry of an edit whose body depends on stored state (an insert of the settings row while there is none, a
// set after): the first answer of the Idempotency-Key is returned and nothing is emitted, so the op the gateway holds under
// the derived key is never asked again with another body. found is false for a first request.
func (s *Service) replayed(ctx context.Context) (out gen.ConfigChange, found bool, err error) {
	im, ok := idemFrom(ctx)
	if !ok {
		return out, false, nil
	}
	err = s.repo.InTx(ctx, func(ctx context.Context, tx repository.Tx) error {
		fp, stored, has, err := tx.IdemGet(ctx, im.Op, im.Key)
		if err != nil || !has {
			return err
		}
		if fp != im.Fingerprint {
			return idempotency.ErrMismatch
		}
		found = true
		return json.Unmarshal(stored, &out)
	})
	return out, found, err
}

// recordReplay stores the answer under the Idempotency-Key once the op was emitted. The op exists whatever happens here, so a
// failure is logged, not returned.
func (s *Service) recordReplay(ctx context.Context, ch gen.ConfigChange) {
	im, ok := idemFrom(ctx)
	if !ok {
		return
	}
	b, err := json.Marshal(ch)
	if err == nil {
		err = s.repo.InTx(ctx, func(ctx context.Context, tx repository.Tx) error {
			return tx.IdemPut(ctx, im.Op, im.Key, im.Fingerprint, b, s.clock.Now())
		})
	}
	if err != nil {
		s.log.Warn("idempotency record of an emitted edit failed", "op", im.Op, "error", err)
	}
}

// SetDiscountLimit sets the cashier discount limit (strong session). It emits a billing_settings config op.
func (s *Service) SetDiscountLimit(ctx context.Context, outlet string, bp int) (gen.ConfigChange, error) {
	if err := s.requireOutlet(ctx, "bill.discount.limit.edit", outlet); err != nil {
		return gen.ConfigChange{}, err
	}
	if ch, found, err := s.replayed(ctx); found || err != nil {
		return ch, err
	}
	if bp < 0 || bp > 10000 {
		return gen.ConfigChange{}, validation("validation", "invalid discount limit", field("discount_limit_bp", "out_of_range"))
	}
	if err := s.check(ctx, outlet); err != nil {
		return gen.ConfigChange{}, err
	}
	// An outlet whose row does not exist yet gets an insert of the row (every other field at its default, the limit as given);
	// otherwise a set of discount_limit_bp only (sync/intents/cloud.yaml settings.discount_limit.set).
	mut, err := s.settingsMut(ctx, outlet, map[string]any{"discount_limit_bp": int64(bp)})
	if err != nil {
		return gen.ConfigChange{}, err
	}
	ch, err := s.emitConfig(ctx, outlet, "setDiscountLimit", "settings.discount_limit.set", "bill.discount.limit.edit", []Mut{mut})
	if err == nil {
		s.recordReplay(ctx, ch)
	}
	return ch, err
}

// settingsDefaults are the fields of an inserted billing_settings row: the defaults of DefaultSettings and, for the fields
// the API does not show, the entity's defaults (service charge applies to dine-in only, no packing charge taxed as
// fnb_standard, day-close banner at 05:00 IST).
func settingsDefaults() map[string]any {
	d := DefaultSettings("")
	return map[string]any{"number_at": string(d.NumberAt), "price_mode": string(d.PriceMode), "round_off": d.RoundOff, "service_charge_bp": int64(d.ServiceChargeBp),
		"service_charge_dine_in": true, "service_charge_takeaway": false, "service_charge_counter": false, "packing_charge_paise": int64(0), "packing_tax_class": "fnb_standard",
		"discount_limit_bp": int64(d.DiscountLimitBp), "rebill_allowance_per_shift": int64(d.RebillAllowancePerShift), "comp_allowance_paise": d.CompAllowance.AmountPaise,
		"open_item_limit_paise": d.OpenItemLimit.AmountPaise, "manual_entry_max_days_back": int64(d.ManualEntryMaxDaysBack), "shift_required": d.ShiftRequired,
		"handover_count": d.HandoverCount, "day_cutoff_minutes": int64(300)}
}

// settingsMut is the mutation that changes fields of the outlet's billing_settings row: a set when the row exists, else an
// insert of the row with its defaults and these fields.
func (s *Service) settingsMut(ctx context.Context, outlet string, fields map[string]any) (Mut, error) {
	exists := false
	if err := s.repo.InTx(ctx, func(ctx context.Context, tx repository.Tx) error {
		_, err := tx.GetDoc(ctx, repository.KindSettings, outlet)
		switch {
		case err == nil:
			exists = true
		case !isNF(err):
			return err
		}
		return nil
	}); err != nil {
		return Mut{}, err
	}
	id := uuid5("billing_settings", outlet)
	if exists {
		return Mut{E: "billing_settings", ID: id, K: gsync.KindSet, F: fields}, nil
	}
	f := settingsDefaults()
	for k, v := range fields {
		f[k] = v
	}
	return Mut{E: "billing_settings", ID: id, K: gsync.KindInsert, F: f}, nil
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
	return s.emitConfig(ctx, outlet, "setNumberingMode", "settings.numbering.set", "settings.numbering.edit",
		[]Mut{{E: "billing_settings", ID: uuid5("billing_settings", outlet), K: gsync.KindInsert, F: map[string]any{"number_at": mode}}})
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
	if ch, found, err := s.replayed(ctx); found || err != nil {
		return ch, err
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
	// One op, one intent: the profile row and, when price_mode is sent, price_mode on the outlet's billing_settings row (a set,
	// or an insert of the row with its defaults when it does not exist; no other settings field changes).
	muts := []Mut{{E: "outlet_tax_profile", ID: uuid5("outlet_tax_profile", outlet+"|"+fy), K: gsync.KindInsert, F: f}}
	if priceMode != nil {
		m, err := s.settingsMut(ctx, outlet, map[string]any{"price_mode": *priceMode})
		if err != nil {
			return gen.ConfigChange{}, err
		}
		muts = append(muts, m)
	}
	ch, err := s.emitConfig(ctx, outlet, "setOutletTaxProfile", "settings.tax_profile.set", "settings.gst_profile.edit", muts)
	if err == nil {
		s.recordReplay(ctx, ch)
	}
	return ch, err
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
