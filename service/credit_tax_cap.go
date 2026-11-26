package service

import (
	"context"

	"github.com/hostryhq/svc-billing/internal/gen"
	"github.com/hostryhq/svc-billing/repository"
)

// The tax summary credits a bill's tax once at most. Credit notes that together credit more than the bill are flagged
// (credit_exceeds_bill) and stay as issued, but the output tax they reduce is capped at the bill's own tax, class by class:
// otherwise two offline full credit notes would reduce the day's output tax twice (F28).

type taxKey struct {
	class string
	rate  int
}

type taxAmounts struct{ taxable, cgst, sgst int64 }

func sumTaxLines(lines []gen.TaxLine) map[taxKey]taxAmounts {
	out := map[taxKey]taxAmounts{}
	for _, t := range lines {
		k := taxKey{t.TaxClass, t.RateBp}
		a := out[k]
		a.taxable += t.Taxable.AmountPaise
		a.cgst += t.Cgst.AmountPaise
		a.sgst += t.Sgst.AmountPaise
		out[k] = a
	}
	return out
}

func minI(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// cappedTaxOf returns what a note may reduce the bill's tax by, given the notes before it (uncapped totals, as stored):
// the part of the running total that still lies inside the bill. prior and note are keyed like the bill.
func cappedTaxOf(bill, prior map[taxKey]taxAmounts, note []gen.TaxLine) []gen.TaxLine {
	out := make([]gen.TaxLine, 0, len(note))
	for _, t := range note {
		k := taxKey{t.TaxClass, t.RateBp}
		b, p := bill[k], prior[k]
		c := t
		c.Taxable = money(minI(p.taxable+t.Taxable.AmountPaise, b.taxable) - minI(p.taxable, b.taxable))
		c.Cgst = money(minI(p.cgst+t.Cgst.AmountPaise, b.cgst) - minI(p.cgst, b.cgst))
		c.Sgst = money(minI(p.sgst+t.Sgst.AmountPaise, b.sgst) - minI(p.sgst, b.sgst))
		out = append(out, c)
		a := prior[k]
		a.taxable += t.Taxable.AmountPaise
		a.cgst += t.Cgst.AmountPaise
		a.sgst += t.Sgst.AmountPaise
		prior[k] = a
	}
	return out
}

// creditTaxLines are the tax lines of a note as the tax summary may count them. With the bill unknown they are the note's
// own; the bill's arrival takes back any excess (trimCreditTaxOf).
func (ac *applyCtx) creditTaxLines(ctx context.Context, n gen.CreditNote) ([]gen.TaxLine, error) {
	orig, err := load[BillRec](ctx, ac.tx, repository.KindBill, n.BillId)
	if err == repository.ErrNotFound {
		return n.TaxLines, nil
	}
	if err != nil {
		return nil, err
	}
	others, _, err := loadAll[CreditRec](ctx, ac.tx, repository.Query{Kind: repository.KindCreditNote, K1: n.BillId})
	if err != nil {
		return nil, err
	}
	var prior []gen.TaxLine
	for _, o := range others {
		if o.Note.Id != n.Id {
			prior = append(prior, o.Note.TaxLines...)
		}
	}
	return cappedTaxOf(sumTaxLines(orig.Detail.TaxLines), sumTaxLines(prior), n.TaxLines), nil
}

// trimCreditTaxOf runs when a bill arrives after notes that were counted uncapped: it takes back from the tax summary
// what the notes together reduced beyond the bill.
func (ac *applyCtx) trimCreditTaxOf(ctx context.Context, bill BillRec) error {
	notes, docs, err := loadAll[CreditRec](ctx, ac.tx, repository.Query{Kind: repository.KindCreditNote, K1: bill.Detail.Id})
	if err != nil {
		return err
	}
	own := ac.outlet
	defer func() { ac.outlet = own }()
	billTax, prior := sumTaxLines(bill.Detail.TaxLines), map[taxKey]taxAmounts{}
	for i, r := range notes {
		capped := cappedTaxOf(billTax, prior, r.Note.TaxLines)
		ac.outlet = docs[i].Outlet
		for j, t := range r.Note.TaxLines {
			d := t
			d.Taxable = money(capped[j].Taxable.AmountPaise - t.Taxable.AmountPaise)
			d.Cgst = money(capped[j].Cgst.AmountPaise - t.Cgst.AmountPaise)
			d.Sgst = money(capped[j].Sgst.AmountPaise - t.Sgst.AmountPaise)
			if d.Taxable.AmountPaise == 0 && d.Cgst.AmountPaise == 0 && d.Sgst.AmountPaise == 0 {
				continue
			}
			if err := ac.aggTax(ctx, r.Note.InvoiceDate.String(), d, false, 0, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

