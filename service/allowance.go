package service

import (
	"context"
	"strings"
	"time"

	"github.com/hostryhq/svc-billing/internal/gen"
	"github.com/hostryhq/svc-billing/repository"
)

// Detectors that watch what the device is meant to refuse: the credit cap of one bill, and the re-bill and comp
// allowances of Phase 1 security ruling 8. The device checks first; these fire when a modified client skips the check.
// Offline devices that each stay inside a limit may together exceed it; that is accepted provided the owner is alerted the
// same day (ruling 8, "for all allowances"), which a high exception is.

// compValue is the list value of a comped line, from the line's own snapshot: price and modifier, rounded to the paisa.
func compValue(l gen.BillLine) int64 {
	return ((l.UnitPrice.AmountPaise+l.ModifierDelta.AmountPaise)*l.QtyMilli + 500) / 1000
}

// checkCreditCap raises credit_exceeds_bill when the credit notes of one bill together credit more than the bill: more of
// a line than it holds, more value than its total (round-off apart), or more taxable value. Each note verifies alone, so
// only the sum can show it. It runs when a note arrives and when the bill arrives, so the arrival order does not matter. The
// notes stay as issued; the finding is what keeps the output tax from being reduced twice unseen (CR-H2).
func (ac *applyCtx) checkCreditCap(ctx context.Context, billID string) error {
	orig, err := load[BillRec](ctx, ac.tx, repository.KindBill, billID)
	if err == repository.ErrNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	notes, _, err := loadAll[CreditRec](ctx, ac.tx, repository.Query{Kind: repository.KindCreditNote, K1: billID})
	if err != nil {
		return err
	}
	var total, taxable, credited int64
	qty := map[int]int64{}
	for _, n := range notes {
		total += n.Calc.TotalPaise - n.Note.RoundOff.AmountPaise
		taxable += n.Calc.TaxablePaise
		credited += n.Calc.TotalPaise
		for _, c := range n.Calc.Credited {
			qty[origLineIndex(orig, c)] += c.QtyMilli
		}
	}
	slack := int64(len(notes)) // each note's proportional split may round by a paisa
	exceeded := total > orig.Calc.TotalPaise-orig.Calc.RoundOffPaise+slack || taxable > orig.Calc.TaxablePaise+slack
	for k, q := range qty {
		if k >= 0 && k < len(orig.Detail.Lines) && q > orig.Detail.Lines[k].QtyMilli {
			exceeded = true
		}
	}
	if !exceeded {
		return nil
	}
	return ac.exception(ctx, "credit_exceeds_bill", gen.High, "bill", billID, orig.Detail.BusinessDate.String(),
		map[string]any{"credited_paise": credited, "bill_total_paise": orig.Calc.TotalPaise, "notes": len(notes)})
}

// RebillRec is the account of one re-bill (one bill.rebill op: a full credit note and its replacement bills).
type RebillRec struct {
	Group      string    `json:"group"`
	NoteID     string    `json:"note_id"`
	OrigBillID string    `json:"orig_bill_id"`
	Member     string    `json:"member"`
	ShiftKey   string    `json:"shift_key"`
	At         time.Time `json:"at"`
	Credited   int64     `json:"credited_paise"`
	Reduction  int64     `json:"reduction_paise"`
	// Approved is true for a bill.rebill.approved op: a manager's approval, bound to the bill and the value change, covers it.
	// An approved re-bill counts toward the value-reduction detectors but not toward the per-shift allowance count.
	Approved bool `json:"approved,omitempty"`
}

// approvedRebillIntent is the intent of a re-bill taken with a manager's approval (after a payment, or past the allowance).
const approvedRebillIntent = "bill.rebill.approved"

// approvedBy is the mark the sync gateway puts on an op whose cited approval it cleared (svc-sync service.MarkApprovedBy).
const approvedBy = "approved_by:"

// approvedRebill reports a re-bill a cleared approval covers (P2R-M1): the approved intent alone is not enough, because a
// holder of bill.credit_note.create can send it without citing anyone; the gateway's approved_by mark is the proof.
func (o Op) approvedRebill() bool {
	if o.Intent != approvedRebillIntent {
		return false
	}
	for _, m := range o.Marks {
		if strings.HasPrefix(m, approvedBy) && len(m) > len(approvedBy) {
			return true
		}
	}
	return false
}

// shiftKey names the shift a member's document belongs to: the member's shift that was open at the instant, else the
// business date (a shift fact that has not arrived yet must not let a count restart).
func (ac *applyCtx) shiftKey(ctx context.Context, member, date string, at time.Time) (string, error) {
	shifts, _, err := loadAll[gen.Shift](ctx, ac.tx, repository.Query{Kind: repository.KindShift, Outlets: []string{ac.outlet}, DateFrom: date, DateTo: date})
	if err != nil {
		return "", err
	}
	for _, sh := range shifts {
		if sh.MemberId == member && !sh.OpenedAt.Time.After(at) && (sh.ClosedAt == nil || !at.After(sh.ClosedAt.Time)) {
			return "shift:" + sh.Id, nil
		}
	}
	return "date:" + date, nil
}

func (ac *applyCtx) limits(ctx context.Context) (gen.BillingSettings, error) {
	st, err := load[settingsRec](ctx, ac.tx, repository.KindSettings, ac.outlet)
	if err == repository.ErrNotFound {
		return DefaultSettings(ac.outlet), nil
	}
	return st.Settings, err
}

// allowances runs the allowance detectors for the documents of this op: re-bills, payments taken against a bill that was
// re-billed, and comped lines.
func (ac *applyCtx) allowances(ctx context.Context) error {
	for _, m := range ac.g["credit_note"] {
		if group := fstr(m.F, "rebill_group_id"); m.K == "insert" && group != "" {
			if err := ac.rebill(ctx, m, group); err != nil {
				return err
			}
		}
	}
	for _, m := range ac.g["payment"] {
		if m.K != "insert" {
			continue
		}
		if err := ac.paymentAfterRebill(ctx, fstr(m.F, "bill_id")); err != nil {
			return err
		}
	}
	for _, m := range ac.g["bill"] {
		if m.K == "insert" {
			if err := ac.compAllowance(ctx, m.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// rebill checks one re-bill (ruling 8: bill.rebill). It is refused on a bill with money recorded against it, counted per
// member per shift against rebill_allowance_per_shift, and a replacement worth less than the original counts the difference
// against the discount limit and the comp allowance. A value-neutral re-bill (split, GSTIN) raises nothing; the second
// value-reducing re-bill of a member in a shift is the owner's alert.
func (ac *applyCtx) rebill(ctx context.Context, note Mut, group string) error {
	date := fstr(note.F, "business_date")
	member := ac.op.Actor
	credited := fint(note.F, "total_paise")
	var replaced int64
	for _, b := range ac.g["bill"] {
		if fstr(b.F, "rebill_group_id") == group {
			replaced += fint(b.F, "total_paise")
		}
	}
	shift, err := ac.shiftKey(ctx, member, date, ac.op.At)
	if err != nil {
		return err
	}
	rec := RebillRec{Group: group, NoteID: note.ID, OrigBillID: fstr(note.F, "bill_id"), Member: member, ShiftKey: shift, At: ac.op.At, Credited: credited, Reduction: credited - replaced,
		Approved: ac.op.approvedRebill()}
	created, err := ac.tx.PutDoc(ctx, repository.Doc{Kind: repository.KindRebill, ID: group, Outlet: ac.outlet, Date: date, K1: member, Body: marshalDoc(rec)})
	if err != nil || !created {
		return err
	}
	if !rec.Approved { // a re-bill after payment is the approved intent's case; without the approval it is a finding
		if err := ac.paymentAfterRebill(ctx, rec.OrigBillID); err != nil {
			return err
		}
	}
	set, err := ac.limits(ctx)
	if err != nil {
		return err
	}
	all, _, err := loadAll[RebillRec](ctx, ac.tx, repository.Query{Kind: repository.KindRebill, Outlets: []string{ac.outlet}, DateFrom: date, DateTo: date, K1: member})
	if err != nil {
		return err
	}
	var count, reducing int
	for _, r := range all {
		if r.ShiftKey != shift {
			continue
		}
		if !r.Approved {
			count++
		}
		if r.Reduction > 0 {
			reducing++
		}
	}
	params := map[string]any{"member_id": member, "shift": shift}
	if !rec.Approved && count > set.RebillAllowancePerShift {
		// Past the allowance the re-bill needed bill.rebill.approved; this op is the plain intent, so the approval is missing.
		if err := ac.exceptionWhy(ctx, "rebill_allowance_exceeded", gen.High, gen.Missing, "credit_note", note.ID, date, mergeParams(params, "count", count, "allowance", set.RebillAllowancePerShift)); err != nil {
			return err
		}
	}
	if rec.Reduction <= 0 {
		return nil
	}
	if err := ac.exception(ctx, "rebill_value_reduced", gen.Warning, "credit_note", note.ID, date, mergeParams(params, "reduction_paise", rec.Reduction)); err != nil {
		return err
	}
	if allowed := int64(set.DiscountLimitBp)*credited/10000 + set.CompAllowance.AmountPaise; rec.Reduction > allowed {
		if err := ac.exception(ctx, "rebill_value_reduction_over_allowance", gen.High, "credit_note", note.ID, date, mergeParams(params, "reduction_paise", rec.Reduction, "allowed_paise", allowed)); err != nil {
			return err
		}
	}
	if reducing >= 2 {
		return ac.exception(ctx, "rebill_value_reduction_repeated", gen.High, "credit_note", note.ID, date, mergeParams(params, "count", reducing))
	}
	return nil
}

func mergeParams(p map[string]any, kv ...any) map[string]any {
	out := map[string]any{}
	for k, v := range p {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

// paymentAfterRebill flags a re-billed bill that has money recorded against it, whichever of the re-bill and the payment
// arrived last (ruling 8 (b): refused once any payment is recorded).
func (ac *applyCtx) paymentAfterRebill(ctx context.Context, billID string) error {
	notes, _, err := loadAll[CreditRec](ctx, ac.tx, repository.Query{Kind: repository.KindCreditNote, K1: billID})
	if err != nil {
		return err
	}
	var rebilled []CreditRec
	for _, n := range notes {
		if n.Note.RebillGroupId == nil {
			continue
		}
		if g, err := load[RebillRec](ctx, ac.tx, repository.KindRebill, *n.Note.RebillGroupId); err == nil && g.Approved {
			continue
		}
		rebilled = append(rebilled, n)
	}
	if len(rebilled) == 0 {
		return nil
	}
	pays, _, err := loadAll[gen.Payment](ctx, ac.tx, repository.Query{Kind: repository.KindPayment, K1: billID})
	if err != nil || len(pays) == 0 {
		return err
	}
	for _, n := range rebilled {
		if err := ac.exceptionWhy(ctx, "rebill_after_payment", gen.High, gen.Missing, "credit_note", n.Note.Id, n.Note.BusinessDate.String(), map[string]any{"bill_id": billID, "payments": len(pays)}); err != nil {
			return err
		}
	}
	return nil
}

// compAllowance flags a bill whose comped lines take the cashier's comp value in the shift past comp_allowance_paise.
// The value comes from the line snapshots (ruling 8: never typed). It runs for each bill as it arrives, so the bill that
// crosses the limit, and every one after it, is flagged.
func (ac *applyCtx) compAllowance(ctx context.Context, billID string) error {
	bill, err := load[BillRec](ctx, ac.tx, repository.KindBill, billID)
	if err != nil {
		return err
	}
	var own int64
	for _, l := range bill.Detail.Lines {
		if l.Comp {
			own += compValue(l)
		}
	}
	if own == 0 {
		return nil
	}
	date := bill.Detail.BusinessDate.String()
	member := deref(bill.Detail.CashierId)
	shift, err := ac.shiftKey(ctx, member, date, bill.Detail.FinalizedAt.Time)
	if err != nil {
		return err
	}
	bills, _, err := loadAll[BillRec](ctx, ac.tx, repository.Query{Kind: repository.KindBill, Outlets: []string{ac.outlet}, DateFrom: date, DateTo: date})
	if err != nil {
		return err
	}
	var used int64
	for _, b := range bills {
		if deref(b.Detail.CashierId) != member {
			continue
		}
		if k, err := ac.shiftKey(ctx, member, date, b.Detail.FinalizedAt.Time); err != nil {
			return err
		} else if k != shift {
			continue
		}
		for _, l := range b.Detail.Lines {
			if l.Comp {
				used += compValue(l)
			}
		}
	}
	set, err := ac.limits(ctx)
	if err != nil {
		return err
	}
	if used <= set.CompAllowance.AmountPaise {
		return nil
	}
	return ac.exception(ctx, "comp_allowance_exceeded", gen.High, "bill", billID, date, map[string]any{"member_id": member, "shift": shift, "comp_paise": used, "allowance_paise": set.CompAllowance.AmountPaise})
}
