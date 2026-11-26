package service

import (
	"context"

	"github.com/hostryhq/svc-billing/internal/gen"
	"github.com/hostryhq/svc-billing/internal/tstamp"
	"github.com/hostryhq/svc-billing/repository"
	"github.com/hostryhq/svc-billing/tax"
)

// verifyEnv is what a recompute needs besides the document: the outlet's regime, its tax profile for the financial
// year, its settings and every rule set the organisation received.
type verifyEnv struct {
	regime    string
	specified bool
	profile   bool // a profile for the year exists
	roundOff  *bool
	packClass string
	sets      []tax.RuleSet
	byID      map[string]tax.RuleSet
}

// verifyEnv resolves the environment of a document of the invoice date: the regime in force on that date, not the outlet's
// current one (M3).
func (ac *applyCtx) verifyEnv(ctx context.Context, fy, invoiceDate string) (verifyEnv, error) {
	env := verifyEnv{regime: "regular", byID: map[string]tax.RuleSet{}, packClass: "fnb_standard"}
	if o, err := load[OutletRec](ctx, ac.tx, repository.KindOutlet, ac.outlet); err == nil {
		if r := o.At(invoiceDate).Regime; r != "" {
			env.regime = r
		}
	}
	profiles, _, err := loadAll[gen.OutletTaxProfile](ctx, ac.tx, repository.Query{Kind: repository.KindTaxProfile, Outlets: []string{ac.outlet}, K1: fy})
	if err != nil {
		return env, err
	}
	if len(profiles) > 0 {
		env.profile, env.specified = true, profiles[0].SpecifiedPremises
	}
	if st, err := load[settingsRec](ctx, ac.tx, repository.KindSettings, ac.outlet); err == nil {
		r := st.Settings.RoundOff
		env.roundOff = &r
		if st.PackingTaxClass != "" {
			env.packClass = st.PackingTaxClass
		}
	}
	recs, _, err := loadAll[RuleSetRec](ctx, ac.tx, repository.Query{Kind: repository.KindRuleSet})
	if err != nil {
		return env, err
	}
	for _, r := range recs {
		rs := r.toTax()
		env.sets = append(env.sets, rs)
		env.byID[rs.ID] = rs
	}
	return env, nil
}

func (r RuleSetRec) toTax() tax.RuleSet {
	rs := tax.RuleSet{ID: r.ID, Version: r.Version, Name: r.Name, EffectiveDate: r.EffectiveDate, CalcV: r.CalcV, ReviewStatus: r.ReviewStatus}
	for _, x := range r.Rules {
		rs.Rules = append(rs.Rules, tax.RuleRow{TaxClass: x.TaxClass, Regime: string(x.Regime), SpecifiedPremises: x.SpecifiedPremises,
			OrderChannel: string(x.OrderChannel), DocType: string(x.DocType), GSTApplies: x.GstApplies, CGSTbp: int64(x.CgstBp), SGSTbp: int64(x.SgstBp), IGSTbp: int64(x.IgstBp)})
	}
	return rs
}

func channelOf(orderType string, eco bool) string {
	if eco {
		return "eco"
	}
	return orderType
}

// taxBill builds the calculation input from the stored bill.
func taxBill(rec BillRec, rules map[string]tax.Rule, regime string, roundOff bool, packClass string) tax.Bill {
	d := rec.Detail
	b := tax.Bill{Regime: regime, PriceMode: string(d.PriceMode), RoundOff: roundOff, Rules: rules, ServiceChargeBp: rec.Calc.ServiceChargeBP}
	for i, l := range d.Lines {
		line := tax.Line{ID: lineKey(i, rec.Calc.LineIDs), QtyMilli: l.QtyMilli, UnitPricePaise: l.UnitPrice.AmountPaise,
			ModifierDeltaPaise: l.ModifierDelta.AmountPaise, TaxClass: l.TaxClass, Comp: l.Comp}
		if !l.Comp && l.LineDiscount.AmountPaise > 0 {
			line.Discount = &tax.Discount{Kind: "amount", Value: l.LineDiscount.AmountPaise}
		}
		b.Lines = append(b.Lines, line)
	}
	if rec.Calc.DiscountKind != "" {
		b.BillDiscount = &tax.Discount{Kind: rec.Calc.DiscountKind, Value: rec.Calc.DiscountValue}
	}
	if rec.Calc.PackingPaise > 0 {
		b.Packing = &tax.Packing{AmountPaise: rec.Calc.PackingPaise, TaxClass: packClass}
	}
	return b
}

func lineKey(i int, ids []string) string {
	if i < len(ids) && ids[i] != "" {
		return ids[i]
	}
	return "line-" + itoa(i+1)
}

func verification(status gen.VerificationStatus, rsID string, diff int64, at tstamp.Time) gen.Verification {
	v := gen.Verification{Status: status, CalcV: tax.CalcV, Difference: ptr(money(diff)), CheckedAt: &at}
	if rsID != "" {
		v.RulesetId = &rsID
	}
	return v
}

// verifyBill recomputes a bill with its own calc_v and rule set and states the result. It never changes the bill.
func (ac *applyCtx) verifyBill(ctx context.Context, rec BillRec) (gen.Verification, error) {
	now := tstamp.Of(ac.s.clock.Now())
	d := rec.Detail
	rsID := deref(d.RulesetId)
	env, err := ac.verifyEnv(ctx, rec.Calc.FinancialYear, d.InvoiceDate.String())
	if err != nil {
		return gen.Verification{}, err
	}
	manual := d.SourceKind == gen.SourceKind("manual_book")
	rs, known := env.byID[rsID]
	if !known || !tax.Supported(d.CalcV) {
		if manual {
			return verification(gen.VerificationStatusManualEntry, rsID, 0, now), nil
		}
		return verification(gen.VerificationStatusPending, rsID, 0, now), nil
	}
	rules, docType := rs.Resolve(env.regime, env.specified, channelOf(string(d.OrderType), d.Eco))
	roundOff := rec.Calc.TotalPaise%100 == 0
	if env.roundOff != nil {
		roundOff = *env.roundOff
	}
	res, err := tax.Calculate(taxBill(rec, rules, env.regime, roundOff, env.packClass))
	if err != nil {
		// A bill the engine refuses (a discount above the value, an unknown tax class) cannot be reproduced: it is
		// flagged as a mismatch, never altered. The reason travels in the exception params.
		ac.reason = err.Error()
		return verification(gen.VerificationStatusMismatch, rsID, rec.Calc.TotalPaise, now), nil
	}
	stated := rec.Calc.TotalPaise
	if manual && rec.Calc.BookTotalPaise > 0 {
		stated = rec.Calc.BookTotalPaise
	}
	diff := stated - res.TotalPaise
	if manual {
		// beyond round-off (a rupee) is flagged; the entry is always listed.
		return verification(gen.VerificationStatusManualEntry, rsID, diff, now), nil
	}
	same := res.GrossPaise == rec.Calc.GrossPaise && res.ServiceChargePaise == rec.Calc.ServiceCharge && res.TaxablePaise == rec.Calc.TaxablePaise &&
		res.CGSTPaise == rec.Calc.CGSTPaise && res.SGSTPaise == rec.Calc.SGSTPaise && res.NonGSTSubtotal == rec.Calc.NonGSTPaise &&
		res.RoundOffPaise == rec.Calc.RoundOffPaise && res.TotalPaise == rec.Calc.TotalPaise
	switch {
	case !same:
		return verification(gen.VerificationStatusMismatch, rsID, diff, now), nil
	case docType != "" && docType != string(d.DocType):
		return verification(gen.VerificationStatusProfileMismatch, rsID, diff, now), nil
	}
	if cur, ok := tax.InForce(env.sets, d.InvoiceDate.String()); ok && cur.ID != rsID {
		return verification(gen.VerificationStatusStaleRules, rsID, diff, now), nil
	}
	return verification(gen.VerificationStatusVerified, rsID, diff, now), nil
}

// verifyCredit recomputes a credit note against the stored original bill.
func (ac *applyCtx) verifyCredit(ctx context.Context, rec CreditRec) (gen.Verification, error) {
	now := tstamp.Of(ac.s.clock.Now())
	n := rec.Note
	rsID := deref(n.RulesetId)
	orig, err := load[BillRec](ctx, ac.tx, repository.KindBill, n.BillId)
	if err == repository.ErrNotFound {
		return verification(gen.VerificationStatusPending, rsID, 0, now), nil
	}
	if err != nil {
		return gen.Verification{}, err
	}
	env, err := ac.verifyEnv(ctx, financialYear(n.InvoiceDate.String()), orig.Detail.InvoiceDate.String())
	if err != nil {
		return gen.Verification{}, err
	}
	rs, known := env.byID[rsID]
	if !known || !tax.Supported(n.CalcV) {
		return verification(gen.VerificationStatusPending, rsID, 0, now), nil
	}
	rules, _ := rs.Resolve(env.regime, env.specified, channelOf(string(orig.Detail.OrderType), orig.Detail.Eco))
	roundOff := orig.Calc.RoundOffPaise != 0 || orig.Calc.TotalPaise%100 == 0
	if env.roundOff != nil {
		roundOff = *env.roundOff
	}
	var cl []tax.CreditLine
	for _, c := range rec.Calc.Credited {
		cl = append(cl, tax.CreditLine{LineID: lineKey(origLineIndex(orig, c), orig.Calc.LineIDs), QtyMilli: c.QtyMilli})
	}
	res, err := tax.CalculateCredit(taxBill(orig, rules, env.regime, roundOff, env.packClass), cl)
	if err != nil {
		return verification(gen.VerificationStatusMismatch, rsID, rec.Calc.TotalPaise, now), nil
	}
	diff := rec.Calc.TotalPaise - res.TotalPaise
	if res.TotalPaise != rec.Calc.TotalPaise || res.CGSTPaise != rec.Calc.CGSTPaise || res.SGSTPaise != rec.Calc.SGSTPaise || res.TaxablePaise != rec.Calc.TaxablePaise {
		return verification(gen.VerificationStatusMismatch, rsID, diff, now), nil
	}
	if cur, ok := tax.InForce(env.sets, n.InvoiceDate.String()); ok && cur.ID != rsID {
		return verification(gen.VerificationStatusStaleRules, rsID, diff, now), nil
	}
	return verification(gen.VerificationStatusVerified, rsID, diff, now), nil
}
