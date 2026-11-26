package service

import (
	"context"
	"strconv"

	"github.com/hostryhq/svc-billing/internal/gen"
	"github.com/hostryhq/svc-billing/internal/tstamp"
	"github.com/hostryhq/svc-billing/repository"
)

// The aggregates are the "daily sales, tax and tender aggregates" of ADR-0014 R3: stored rows the projection keeps up to
// date as documents arrive (once each: a document is applied only when first inserted), and the reports read. They
// include late documents, because the projection of a business day is the original Z plus its revisions (A1.1 item 7).

func (ac *applyCtx) aggDay(ctx context.Context, date string, c gen.DayTotals) error {
	return modify(ctx, ac.tx, repository.Doc{Kind: repository.KindAggDay, ID: uuid5("agg_day", ac.outlet+"|"+date), Outlet: ac.outlet, Date: date, Sort: date},
		func(t *gen.DayTotals) {
			if t.Gross.Currency == "" {
				*t = emptyTotals()
			}
			*t = addTotals(*t, c)
		})
}

// AggTax is a tax summary row plus the non-GST subtotal, which the row does not show but the report totals do.
type AggTax struct {
	Row    gen.TaxSummaryRow `json:"row"`
	NonGST int64             `json:"non_gst"`
}

func (ac *applyCtx) aggTax(ctx context.Context, invDate string, t gen.TaxLine, bill bool, nonGST int64, all []gen.TaxLine) error {
	id := uuid5("agg_tax", ac.outlet+"|"+invDate+"|"+t.TaxClass+"|"+strconv.Itoa(t.RateBp))
	return modify(ctx, ac.tx, repository.Doc{Kind: repository.KindAggTax, ID: id, Outlet: ac.outlet, Date: invDate, Sort: t.TaxClass}, func(a *AggTax) {
		if a.Row.TaxClass == "" {
			z := money(0)
			a.Row = gen.TaxSummaryRow{TaxClass: t.TaxClass, RateBp: t.RateBp, GstApplies: t.GstApplies, BillTaxable: z, BillCgst: z, BillSgst: z, CreditTaxable: z, CreditCgst: z, CreditSgst: z}
		}
		if bill {
			addTo(&a.Row.BillTaxable, t.Taxable.AmountPaise)
			addTo(&a.Row.BillCgst, t.Cgst.AmountPaise)
			addTo(&a.Row.BillSgst, t.Sgst.AmountPaise)
			if !t.GstApplies {
				a.NonGST += t.ChargeAlloc.AmountPaise
			}
		} else {
			addTo(&a.Row.CreditTaxable, t.Taxable.AmountPaise)
			addTo(&a.Row.CreditCgst, t.Cgst.AmountPaise)
			addTo(&a.Row.CreditSgst, t.Sgst.AmountPaise)
		}
	})
}

func (ac *applyCtx) aggTender(ctx context.Context, date string, tender gen.PaymentTender, name string, count int, paid, refunded int64) error {
	id := uuid5("agg_tender", ac.outlet+"|"+date+"|"+string(tender)+"|"+name)
	return modify(ctx, ac.tx, repository.Doc{Kind: repository.KindAggTender, ID: id, Outlet: ac.outlet, Date: date, Sort: string(tender)}, func(r *gen.TenderSummaryRow) {
		if r.Tender == "" {
			r.Tender = gen.TenderSummaryRowTender(tender)
			if name != "" {
				r.Name = &name
			}
		}
		r.PaymentCount += count
		addTo(&r.Paid, paid)
		addTo(&r.Refunded, refunded)
		r.Net = money(r.Paid.AmountPaise - r.Refunded.AmountPaise)
	})
}

func (ac *applyCtx) aggItem(ctx context.Context, date string, l gen.BillLine) error {
	gross := l.UnitPrice.AmountPaise + l.ModifierDelta.AmountPaise
	gross = (gross*l.QtyMilli + 500) / 1000
	net := l.Net.AmountPaise
	if err := modify(ctx, ac.tx, repository.Doc{Kind: repository.KindAggItem, ID: uuid5("agg_item", ac.outlet+"|"+date+"|"+l.ItemId+"|"+deref(l.VariantName)), Outlet: ac.outlet, Date: date, Sort: l.ItemId},
		func(r *gen.ItemSalesRow) {
			if r.ItemId == "" {
				r.ItemId, r.ItemName, r.VariantName = l.ItemId, l.ItemName, l.VariantName
			}
			r.QtyMilli += l.QtyMilli
			addTo(&r.Gross, gross)
			addTo(&r.Net, net)
		}); err != nil {
		return err
	}
	cat := deref(l.CategoryId)
	return modify(ctx, ac.tx, repository.Doc{Kind: repository.KindAggCategory, ID: uuid5("agg_category", ac.outlet+"|"+date+"|"+cat+"|"+l.CategoryName), Outlet: ac.outlet, Date: date, Sort: cat},
		func(r *gen.CategorySalesRow) {
			if r.CategoryName == "" {
				r.CategoryId, r.CategoryName = cat, l.CategoryName
			}
			r.QtyMilli += l.QtyMilli
			addTo(&r.Gross, gross)
			addTo(&r.Net, net)
		})
}

func (ac *applyCtx) aggStaffHourly(ctx context.Context, d gen.BillDetail) error {
	date := d.BusinessDate.String()
	net := d.Total.AmountPaise - d.RoundOff.AmountPaise
	for _, t := range d.TaxLines {
		net -= t.Cgst.AmountPaise + t.Sgst.AmountPaise + t.Igst.AmountPaise
	}
	for _, who := range []struct {
		id, role string
	}{{deref(d.CashierId), "cashier"}, {deref(d.WaiterId), "waiter"}} {
		if who.id == "" {
			continue
		}
		who := who
		if err := modify(ctx, ac.tx, repository.Doc{Kind: repository.KindAggStaff, ID: uuid5("agg_staff", ac.outlet+"|"+date+"|"+who.id+"|"+who.role), Outlet: ac.outlet, Date: date, Sort: who.id},
			func(r *gen.StaffSalesRow) {
				if r.MemberId == "" {
					r.MemberId, r.Role = who.id, gen.StaffSalesRowRole(who.role)
				}
				r.BillCount++
				addTo(&r.Net, net)
			}); err != nil {
			return err
		}
	}
	h := istHour(d.FinalizedAt.Time)
	return modify(ctx, ac.tx, repository.Doc{Kind: repository.KindAggHourly, ID: uuid5("agg_hourly", ac.outlet+"|"+date+"|"+strconv.Itoa(h)+"|"+string(d.OrderType)), Outlet: ac.outlet, Date: date, Sort: strconv.Itoa(h)},
		func(r *gen.HourlySalesRow) {
			if r.Net.Currency == "" {
				r.BusinessDate, r.Hour, r.OrderType = d.BusinessDate, h, gen.HourlySalesRowOrderType(d.OrderType)
			}
			r.BillCount++
			addTo(&r.Net, net)
		})
}

// discountRow records one row of the discount report (discounts, comps, refunds, credit notes, internal closures).
func (ac *applyCtx) discountRow(ctx context.Context, kind, subject, billNumber, reason string, value int64, date string) error {
	if value <= 0 {
		return nil
	}
	r := gen.DiscountRow{Kind: gen.DiscountRowKind(kind), SubjectId: subject, ReasonCodeId: reason, MemberId: ac.op.Actor, WithinAllowance: false, Value: money(value), At: ac.at(), BusinessDate: mustDate(date)}
	if billNumber != "" {
		r.BillNumber = &billNumber
	}
	_, err := ac.tx.PutDoc(ctx, repository.Doc{Kind: repository.KindDiscount, ID: uuid5("discount", kind+"|"+subject), Outlet: ac.outlet, Date: date, Sort: ac.at().String(), K1: kind, Body: marshalDoc(r)})
	return err
}

var _ = tstamp.Layout
