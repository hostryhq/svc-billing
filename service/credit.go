package service

import (
	"context"

	"github.com/hostryhq/svc-billing/internal/gen"
	"github.com/hostryhq/svc-billing/repository"
)

// origLineIndex is the position, in the original bill, of the line a credit line credits. The credit line's own number
// counts the lines of the note, not of the bill, so the bill line it names decides.
func origLineIndex(orig BillRec, c CreditLine) int {
	for i, id := range orig.Calc.LineIDs {
		if id != "" && id == c.BillLineID {
			return i
		}
	}
	return c.LineNo - 1
}

// enrichCredit fills the lines of a credit note from the bill it credits (item and category names, ids, prices).
func enrichCredit(rec *CreditRec, orig BillRec) {
	for i, c := range rec.Calc.Credited {
		k := origLineIndex(orig, c)
		if k < 0 || k >= len(orig.Detail.Lines) || i >= len(rec.Note.Lines) {
			continue
		}
		ol := orig.Detail.Lines[k]
		nl := rec.Note.Lines[i]
		nl.ItemId, nl.ItemName, nl.VariantName, nl.CategoryId, nl.CategoryName, nl.OrderLineId, nl.UnitPrice, nl.ModifierDelta = ol.ItemId, ol.ItemName, ol.VariantName, ol.CategoryId, ol.CategoryName, ol.OrderLineId, ol.UnitPrice, ol.ModifierDelta
		rec.Note.Lines[i] = nl
	}
}

// reverifyCreditsOf states the verification of the credit notes of a bill that reached billing before the bill did.
// Such a note is stored "pending" because there was nothing to recompute it against; the bill's arrival settles it. The
// note is never altered beyond naming its lines from the bill.
func (ac *applyCtx) reverifyCreditsOf(ctx context.Context, bill BillRec) error {
	notes, docs, err := loadAll[CreditRec](ctx, ac.tx, repository.Query{Kind: repository.KindCreditNote, K1: bill.Detail.Id})
	if err != nil {
		return err
	}
	own := ac.outlet
	defer func() { ac.outlet = own }()
	for i, r := range notes {
		if r.Note.Verification.Status != gen.VerificationStatusPending {
			continue
		}
		ac.outlet = docs[i].Outlet
		enrichCredit(&r, bill)
		v, err := ac.verifyCredit(ctx, r)
		if err != nil {
			return err
		}
		if v.Status == gen.VerificationStatusPending {
			continue
		}
		r.Note.Verification = v
		docs[i].Body = marshalDoc(r)
		if err := ac.tx.SaveDoc(ctx, docs[i]); err != nil {
			return err
		}
		if err := ac.flag(ctx, "credit_note", r.Note.Id, docs[i].Outlet, v, r.Note.BusinessDate.String()); err != nil {
			return err
		}
	}
	return nil
}
