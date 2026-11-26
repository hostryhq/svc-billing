package service

import (
	"context"
	"sort"

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

// reconcileCreditTax allocates the bill's tax across its credit notes in a fixed order (invoice date, series, number, id),
// whatever order they arrived in, and corrects the tax summary by the difference from what each note has counted so far.
// A note is first counted in full (CountedTax nil); the bill being known, this takes back the excess from the later notes, and
// it runs again whenever a note or the bill arrives, so the reduction always lands on the same invoice date (RM1).
func (ac *applyCtx) reconcileCreditTax(ctx context.Context, billID string) error {
	bill, err := load[BillRec](ctx, ac.tx, repository.KindBill, billID)
	if err == repository.ErrNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	notes, docs, err := loadAll[CreditRec](ctx, ac.tx, repository.Query{Kind: repository.KindCreditNote, K1: billID})
	if err != nil {
		return err
	}
	idx := make([]int, len(notes))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		x, y := notes[idx[a]], notes[idx[b]]
		if xd, yd := x.Note.InvoiceDate.String(), y.Note.InvoiceDate.String(); xd != yd {
			return xd < yd
		}
		if x.Calc.SeriesCode != y.Calc.SeriesCode {
			return x.Calc.SeriesCode < y.Calc.SeriesCode
		}
		if x.Note.Number != y.Note.Number {
			return x.Note.Number < y.Note.Number
		}
		return x.Note.Id < y.Note.Id
	})
	own := ac.outlet
	defer func() { ac.outlet = own }()
	billTax, prior := sumTaxLines(bill.Detail.TaxLines), map[taxKey]taxAmounts{}
	for _, i := range idx {
		r := notes[i]
		want := cappedTaxOf(billTax, prior, r.Note.TaxLines)
		have := r.Calc.CountedTax
		if have == nil {
			have = r.Note.TaxLines
		}
		ac.outlet = docs[i].Outlet
		changed := false
		for j, t := range r.Note.TaxLines {
			d := t
			d.Taxable = money(want[j].Taxable.AmountPaise - have[j].Taxable.AmountPaise)
			d.Cgst = money(want[j].Cgst.AmountPaise - have[j].Cgst.AmountPaise)
			d.Sgst = money(want[j].Sgst.AmountPaise - have[j].Sgst.AmountPaise)
			if d.Taxable.AmountPaise == 0 && d.Cgst.AmountPaise == 0 && d.Sgst.AmountPaise == 0 {
				continue
			}
			changed = true
			if err := ac.aggTax(ctx, r.Note.InvoiceDate.String(), d, false, 0, nil); err != nil {
				return err
			}
		}
		if changed || r.Calc.CountedTax == nil {
			r.Calc.CountedTax = want
			if r.Calc.CountedTax == nil {
				r.Calc.CountedTax = []gen.TaxLine{}
			}
			docs[i].Body = marshalDoc(r)
			if err := ac.tx.SaveDoc(ctx, docs[i]); err != nil {
				return err
			}
		}
	}
	return nil
}
