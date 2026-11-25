package service

import (
	"context"
	"sort"
	"strconv"
	"time"

	"github.com/hostryhq/svc-billing/internal/gen"
	"github.com/hostryhq/svc-billing/internal/tstamp"
	"github.com/hostryhq/svc-billing/repository"
)

func (ac *applyCtx) at() tstamp.Time { return tstamp.Of(ac.op.At) }

func lineFrom(f map[string]any) gen.BillLine {
	l := gen.BillLine{LineNo: int(fint(f, "line_no")), ItemId: fstr(f, "item_id"), ItemName: fstr(f, "item_name"), CategoryName: fstr(f, "category_name"),
		QtyMilli: fint(f, "qty_milli"), UnitPrice: money(fint(f, "unit_price_paise")), ModifierDelta: money(fint(f, "modifier_delta_paise")),
		LineDiscount: money(fint(f, "line_discount_paise")), BillDiscountAlloc: money(fint(f, "bill_discount_alloc_paise")), Net: money(fint(f, "net_paise")),
		Comp: fbool(f, "comp"), TaxClass: fstr(f, "tax_class")}
	if v := fstr(f, "order_line_id"); v != "" {
		l.OrderLineId = &v
	}
	if v := fstr(f, "variant_name"); v != "" {
		l.VariantName = &v
	}
	if v := fstr(f, "category_id"); v != "" {
		l.CategoryId = &v
	}
	if v := fstr(f, "hsn_sac"); v != "" {
		l.HsnSac = &v
	}
	return l
}

func taxLineFrom(f map[string]any) gen.TaxLine {
	return gen.TaxLine{TaxClass: fstr(f, "tax_class"), RateBp: int(fint(f, "rate_bp")), GstApplies: fbool(f, "gst_applies"), ChargeAlloc: money(fint(f, "charge_alloc_paise")),
		Taxable: money(fint(f, "taxable_paise")), Cgst: money(fint(f, "cgst_paise")), Sgst: money(fint(f, "sgst_paise")), Igst: money(fint(f, "igst_paise"))}
}

// bills projects every bill of the op with its lines and tax lines (one bill.finalize op carries them together).
func (ac *applyCtx) bills(ctx context.Context) error {
	for _, m := range ac.g["bill"] {
		if m.K != "insert" {
			continue
		}
		f := m.F
		d := gen.BillDetail{Id: m.ID, OutletId: ac.outlet, Number: fstr(f, "number"), DocType: gen.BillDocType(fstr(f, "doc_type")),
			SourceKind: gen.SourceKind(fstr(f, "source_kind")), OrderType: gen.BillDetailOrderType(fstr(f, "order_type")), Eco: fbool(f, "eco"),
			BusinessDate: mustDate(fstr(f, "business_date")), InvoiceDate: mustDate(fstr(f, "invoice_date")), FinalizedAt: ac.at(), DeviceId: ac.op.Origin,
			CalcV: int(fint(f, "calc_v")), PriceMode: gen.BillDetailPriceMode(fstr(f, "price_mode")), Total: money(fint(f, "total_paise")),
			RoundOff: money(fint(f, "round_off_paise")), Paid: money(0), Credited: money(0), PaymentStatus: gen.PaymentStatus("unpaid"),
			Late: false}
		if v := fstr(f, "check_id"); v != "" {
			d.CheckId = &v
		}
		if v := fstr(f, "ruleset_id"); v != "" {
			d.RulesetId = &v
		}
		if ac.op.Actor != "" {
			a := ac.op.Actor
			d.CashierId = &a
		}
		if v := fstr(f, "waiter_id"); v != "" {
			d.WaiterId = &v
		}
		d.DiscountTotal, d.ServiceCharge, d.PackingCharge = money(fint(f, "discount_paise")), money(fint(f, "service_charge_paise")), money(fint(f, "packing_charge_paise"))
		d.NonGstSubtotal = money(fint(f, "non_gst_subtotal_paise"))
		d.Lines, d.TaxLines, d.Payments = []gen.BillLine{}, []gen.TaxLine{}, []gen.Payment{}
		var lineIDs []string
		type lm struct {
			l  gen.BillLine
			id string
		}
		var ls []lm
		for _, c := range ac.g["bill_line"] {
			if fstr(c.F, "bill_id") == m.ID {
				ls = append(ls, lm{lineFrom(c.F), c.ID})
			}
		}
		sort.SliceStable(ls, func(i, j int) bool { return ls[i].l.LineNo < ls[j].l.LineNo })
		for _, x := range ls {
			d.Lines = append(d.Lines, x.l)
			lineIDs = append(lineIDs, x.id)
		}
		for _, c := range ac.g["bill_tax_line"] {
			if fstr(c.F, "bill_id") == m.ID {
				d.TaxLines = append(d.TaxLines, taxLineFrom(c.F))
			}
		}
		code, fy, running, _ := splitNumber(d.Number)
		rec := BillRec{Detail: d, Calc: BillCalc{SeriesCode: fstr(f, "series_code"), FinancialYear: fstr(f, "financial_year"), DiscountKind: fstr(f, "bill_discount_kind"),
			DiscountValue: fint(f, "bill_discount_value"), ServiceChargeBP: fint(f, "service_charge_bp"), GrossPaise: fint(f, "gross_paise"), DiscountPaise: fint(f, "discount_paise"),
			TaxablePaise: fint(f, "taxable_paise"), CGSTPaise: fint(f, "cgst_paise"), SGSTPaise: fint(f, "sgst_paise"), NonGSTPaise: fint(f, "non_gst_subtotal_paise"),
			ServiceCharge: fint(f, "service_charge_paise"), PackingPaise: fint(f, "packing_charge_paise"), RoundOffPaise: fint(f, "round_off_paise"),
			TotalPaise: fint(f, "total_paise"), BookTotalPaise: fint(f, "book_total_paise"), LineIDs: lineIDs, OpOrigin: ac.op.Origin, OpSeq: ac.op.Seq,
			RebillGroupID: fstr(f, "rebill_group_id")}}
		if rec.Calc.SeriesCode == "" {
			rec.Calc.SeriesCode = code
		}
		if rec.Calc.FinancialYear == "" {
			rec.Calc.FinancialYear = fy
		}
		day, late, err := ac.lateDay(ctx, d.BusinessDate.String(), d.Number)
		if err != nil {
			return err
		}
		rec.Detail.Late = late
		// payments that arrived before the bill
		pays, _, err := loadAll[gen.Payment](ctx, ac.tx, repository.Query{Kind: repository.KindPayment, K1: m.ID})
		if err != nil {
			return err
		}
		applyPayments(&rec.Detail, pays)
		// credit notes that arrived before the bill
		earlier, _, err := loadAll[CreditRec](ctx, ac.tx, repository.Query{Kind: repository.KindCreditNote, K1: m.ID})
		if err != nil {
			return err
		}
		for _, n := range earlier {
			addTo(&rec.Detail.Credited, n.Note.Total.AmountPaise)
		}
		if rec.Detail.Verification, err = ac.verifyBill(ctx, rec); err != nil {
			return err
		}
		created, err := ac.tx.PutDoc(ctx, repository.Doc{Kind: repository.KindBill, ID: m.ID, Outlet: ac.outlet, Date: d.BusinessDate.String(), Sort: ac.at().String(),
			K1: d.Number, K2: deref(d.CheckId), Body: marshalDoc(rec)})
		if err != nil {
			return err
		}
		if !created {
			continue
		}
		_ = running
		if err := ac.afterBill(ctx, rec, day, late); err != nil {
			return err
		}
		if err := ac.reverifyCreditsOf(ctx, rec); err != nil {
			return err
		}
		if err := ac.checkPaymentsExceed(ctx, rec.Detail); err != nil { // payments that came before the bill
			return err
		}
		if err := ac.checkCreditCap(ctx, m.ID); err != nil { // notes that came before the bill
			return err
		}
	}
	return nil
}

// applyPayments sets paid and payment_status of a bill from its payments and attaches them.
func applyPayments(d *gen.BillDetail, pays []gen.Payment) {
	var paid int64
	d.Payments = []gen.Payment{}
	sort.SliceStable(pays, func(i, j int) bool { return pays[i].PaidAt.Before(pays[j].PaidAt.Time) })
	for _, p := range pays {
		paid += p.Amount.AmountPaise
		d.Payments = append(d.Payments, p)
	}
	d.Paid = money(paid)
	switch {
	case paid <= 0:
		d.PaymentStatus = "unpaid"
	case paid < d.Total.AmountPaise:
		d.PaymentStatus = "partly_paid"
	default:
		d.PaymentStatus = "paid"
	}
}

func (ac *applyCtx) afterBill(ctx context.Context, rec BillRec, day gen.BusinessDay, late bool) error {
	d := rec.Detail
	date, inv := d.BusinessDate.String(), d.InvoiceDate.String()
	if err := ac.register(ctx, rec.Calc.SeriesCode, rec.Calc.FinancialYear, d.Number, string(d.DocType), string(d.SourceKind), d.DeviceId, "bill", d.Id, inv); err != nil {
		return err
	}
	// aggregates
	contrib := billContribution(rec)
	if err := ac.aggDay(ctx, date, contrib); err != nil {
		return err
	}
	for _, t := range d.TaxLines {
		t := t
		if err := ac.aggTax(ctx, inv, t, true, rec.Calc.NonGSTPaise, d.TaxLines); err != nil {
			return err
		}
	}
	if len(d.TaxLines) == 0 && rec.Calc.NonGSTPaise != 0 {
		if err := ac.aggTax(ctx, inv, gen.TaxLine{TaxClass: "non_gst"}, true, rec.Calc.NonGSTPaise, nil); err != nil {
			return err
		}
	}
	for _, l := range d.Lines {
		if err := ac.aggItem(ctx, date, l); err != nil {
			return err
		}
	}
	if err := ac.aggStaffHourly(ctx, d); err != nil {
		return err
	}
	if rec.Calc.DiscountPaise > 0 {
		if err := ac.discountRow(ctx, "discount", d.Id, d.Number, NoReason, rec.Calc.DiscountPaise, date); err != nil {
			return err
		}
	}
	for _, l := range d.Lines {
		if l.Comp {
			if err := ac.discountRow(ctx, "comp", d.Id+"|"+strconv.Itoa(l.LineNo), d.Number, NoReason, compValue(l), date); err != nil {
				return err
			}
		}
	}
	// detectors
	if d.CheckId != nil {
		others, _, err := loadAll[BillRec](ctx, ac.tx, repository.Query{Kind: repository.KindBill, Outlets: []string{ac.outlet}, K2: *d.CheckId})
		if err != nil {
			return err
		}
		if len(others) > 1 {
			if err := ac.exception(ctx, "two_bills_one_check", gen.Warning, "check", *d.CheckId, date, nil); err != nil {
				return err
			}
		}
	}
	for _, l := range d.Lines {
		if l.OrderLineId == nil {
			continue
		}
		created, err := ac.tx.PutDoc(ctx, repository.Doc{Kind: "line_claim", ID: *l.OrderLineId, Outlet: ac.outlet, K1: d.Id, Body: marshalDoc(map[string]string{"bill_id": d.Id})})
		if err != nil {
			return err
		}
		if !created {
			c, err := ac.tx.GetDoc(ctx, "line_claim", *l.OrderLineId)
			if err != nil {
				return err
			}
			if c.K1 != d.Id {
				if err := ac.exception(ctx, "line_on_two_bills", gen.High, "bill", d.Id, date, map[string]any{"first_bill_id": c.K1}); err != nil {
					return err
				}
			}
		}
	}
	if err := ac.flag(ctx, "bill", d.Id, d.OutletId, d.Verification, date); err != nil {
		return err
	}
	if d.SourceKind == gen.SourceKind("manual_book") {
		if err := ac.exception(ctx, "manual_entry", gen.Warning, "bill", d.Id, date, map[string]any{"number": d.Number}); err != nil {
			return err
		}
	}
	if err := ac.settle(ctx, day, late, "bill", d.Id, d.Number, date, contrib, []gen.SeriesRange{{SeriesCode: rec.Calc.SeriesCode, DocType: gen.SeriesRangeDocType(d.DocType), FirstNumber: d.Number, LastNumber: d.Number, Count: 1}}); err != nil {
		return err
	}
	if err := ac.s.publish(ctx, ac.tx, ac.outlet, "billing.bill.recorded", d.Id, ac.op.Actor, map[string]any{
		"bill_id": d.Id, "outlet_id": ac.outlet, "doc_type": d.DocType, "source_kind": d.SourceKind, "business_date": date, "invoice_date": inv,
		"total_paise": d.Total.AmountPaise, "late": late, "verification": recordedVerification(d.Verification.Status)}); err != nil {
		return err
	}
	return ac.checkPaymentsExceed(ctx, d)
}

// recordedVerification is the stored status as the event states it: a document whose rule set has not arrived is
// "pending", never "verified" (M4). The flagged event follows when the status becomes final and is not verified.
func recordedVerification(s gen.VerificationStatus) string { return string(s) }

// flag raises the exception and the event of a document that did not verify.
func (ac *applyCtx) flag(ctx context.Context, kind, id, outlet string, v gen.Verification, date string) error {
	var ek string
	var sev gen.ExceptionSeverity
	switch v.Status {
	case gen.VerificationStatusMismatch:
		ek, sev = "tax_mismatch", gen.High
	case gen.VerificationStatusStaleRules:
		ek, sev = "stale_rules", gen.Warning
	case gen.VerificationStatusProfileMismatch:
		ek, sev = "profile_mismatch", gen.Warning
	case gen.VerificationStatusManualEntry:
		if v.Difference == nil || v.Difference.AmountPaise > -100 && v.Difference.AmountPaise < 100 {
			return nil // within round-off: listed as manual_entry only
		}
		ek, sev = "manual_entry", gen.High
	default:
		return nil
	}
	var diff int64
	if v.Difference != nil {
		diff = v.Difference.AmountPaise
	}
	params := map[string]any{"difference_paise": diff}
	if ac.reason != "" && v.Status == gen.VerificationStatusMismatch {
		params["reason"] = ac.reason
	}
	if err := ac.exception(ctx, ek, sev, kind, id, date, params); err != nil {
		return err
	}
	if v.Status == gen.VerificationStatusProfileMismatch {
		if err := ac.exception(ctx, "doc_type_regime_mismatch", gen.High, kind, id, date, nil); err != nil {
			return err
		}
	}
	data := map[string]any{"document_kind": kind, "document_id": id, "outlet_id": outlet, "status": v.Status, "calc_v": v.CalcV, "difference_paise": diff}
	if r, ok := params["reason"]; ok {
		data["reason"] = r
	}
	if v.RulesetId != nil {
		data["ruleset_id"] = *v.RulesetId
	}
	return ac.s.publish(ctx, ac.tx, ac.outlet, "billing.tax_verification.flagged", id, ac.op.Actor, data)
}

func billContribution(rec BillRec) gen.DayTotals {
	t := emptyTotals()
	c := rec.Calc
	t.Gross, t.Discounts, t.Net = money(c.GrossPaise), money(c.DiscountPaise), money(c.GrossPaise-c.DiscountPaise)
	t.ServiceCharge, t.Cgst, t.Sgst, t.NonGst = money(c.ServiceCharge), money(c.CGSTPaise), money(c.SGSTPaise), money(c.NonGSTPaise)
	t.RoundOff, t.Total, t.BillCount = money(c.RoundOffPaise), money(c.TotalPaise), 1
	// The tenders are not the bill's: each payment adds its own tender when it is projected, whichever of the two arrived
	// first (CR-H1).
	return t
}

func tenderTotals(p gen.Payment) gen.DayTotals {
	t := emptyTotals()
	t.Tenders = []gen.TenderTotal{{Tender: gen.TenderTotalTender(p.Tender), Name: p.TenderName, Count: 1, Amount: p.Amount}}
	return t
}

// ---- payments ----

func (ac *applyCtx) payments(ctx context.Context) error {
	for _, m := range ac.g["payment"] {
		if m.K != "insert" {
			continue
		}
		f := m.F
		p := gen.Payment{Id: m.ID, BillId: fstr(f, "bill_id"), TargetKind: "bill", Tender: gen.PaymentTender(fstr(f, "tender")), Amount: money(fint(f, "amount_paise")),
			MemberId: ac.op.Actor, DeviceId: ac.op.Origin, BusinessDate: mustDate(fstr(f, "business_date")), PaidAt: ac.at()}
		if v := fstr(f, "tender_name"); v != "" {
			p.TenderName = &v
		}
		if fhas(f, "tendered_paise") {
			p.Tendered = ptr(money(fint(f, "tendered_paise")))
		}
		if fhas(f, "change_paise") {
			p.Change = ptr(money(fint(f, "change_paise")))
		}
		if v := fstr(f, "reference"); v != "" {
			p.Reference = &v
		}
		if v := fstr(f, "shift_id"); v != "" {
			p.ShiftId = &v
		}
		created, err := ac.tx.PutDoc(ctx, repository.Doc{Kind: repository.KindPayment, ID: m.ID, Outlet: ac.outlet, Date: p.BusinessDate.String(), Sort: ac.at().String(),
			K1: p.BillId, K2: string(p.Tender), Body: marshalDoc(p)})
		if err != nil {
			return err
		}
		if !created {
			continue
		}
		date := p.BusinessDate.String()
		// the payment is late with its bill: a numbered bill of the same op, or the one billing already holds, names the series
		billNumber := ""
		for _, b := range ac.g["bill"] {
			if b.ID == p.BillId {
				billNumber = fstr(b.F, "number")
			}
		}
		if bd, err := ac.tx.GetDoc(ctx, repository.KindBill, p.BillId); billNumber == "" && err == nil {
			billNumber = bd.K1
		}
		day, late, err := ac.lateDay(ctx, date, billNumber)
		if err != nil {
			return err
		}
		if err := ac.aggTender(ctx, date, p.Tender, deref(p.TenderName), 1, p.Amount.AmountPaise, 0); err != nil {
			return err
		}
		contrib := tenderTotals(p)
		if err := ac.aggDay(ctx, date, contrib); err != nil {
			return err
		}
		if err := ac.settle(ctx, day, late, "payment", p.Id, billNumber, date, contrib, nil); err != nil {
			return err
		}
		b, err := ac.tx.GetDoc(ctx, repository.KindBill, p.BillId)
		if err == repository.ErrNotFound {
			continue // orphan tolerance: the bill's arrival picks the payment up
		}
		if err != nil {
			return err
		}
		rec, err := unmarshalDoc[BillRec](b)
		if err != nil {
			return err
		}
		pays, _, err := loadAll[gen.Payment](ctx, ac.tx, repository.Query{Kind: repository.KindPayment, K1: p.BillId})
		if err != nil {
			return err
		}
		applyPayments(&rec.Detail, pays)
		b.Body = marshalDoc(rec)
		if err := ac.tx.SaveDoc(ctx, b); err != nil {
			return err
		}
		if err := ac.checkPaymentsExceed(ctx, rec.Detail); err != nil {
			return err
		}
	}
	return nil
}

// checkPaymentsExceed raises the detector when the money recorded for a bill is more than its total.
func (ac *applyCtx) checkPaymentsExceed(ctx context.Context, d gen.BillDetail) error {
	if d.Paid.AmountPaise <= d.Total.AmountPaise {
		return nil
	}
	// The money taken was more than the bill, whatever order the refund that corrects it arrived in: devices that could
	// not hear each other each took the rest, and the refund may be projected before the second payment. The finding is
	// always raised; a refund that already covers the excess closes it at once.
	if err := ac.exception(ctx, "payments_exceed_total", gen.High, "bill", d.Id, d.BusinessDate.String(), map[string]any{"total_paise": d.Total.AmountPaise, "paid_paise": d.Paid.AmountPaise}); err != nil {
		return err
	}
	refunded, last, err := ac.refundedFor(ctx, d.Id)
	if err != nil {
		return err
	}
	if d.Paid.AmountPaise-refunded <= d.Total.AmountPaise {
		return ac.resolveException(ctx, "payments_exceed_total", d.Id, last)
	}
	return nil
}

// refundedFor is the sum of the refunds of a bill and the id of the latest of them.
func (ac *applyCtx) refundedFor(ctx context.Context, billID string) (int64, string, error) {
	rs, _, err := loadAll[gen.Refund](ctx, ac.tx, repository.Query{Kind: repository.KindRefund, K1: billID})
	var n int64
	var last string
	var lastAt time.Time
	for _, r := range rs {
		n += r.Amount.AmountPaise
		if last == "" || r.RefundedAt.Time.After(lastAt) {
			last, lastAt = r.Id, r.RefundedAt.Time
		}
	}
	return n, last, err
}

// ---- refunds ----

func (ac *applyCtx) refunds(ctx context.Context) error {
	for _, m := range ac.g["refund"] {
		if m.K != "insert" {
			continue
		}
		f := m.F
		r := gen.Refund{Id: m.ID, BillId: fstr(f, "bill_id"), Amount: money(fint(f, "amount_paise")), Tender: gen.RefundTender(fstr(f, "tender")),
			ReasonCodeId: fstr(f, "reason_code_id"), MemberId: ac.op.Actor, BusinessDate: mustDate(fstr(f, "business_date")), RefundedAt: ac.at()}
		if v := fstr(f, "payment_id"); v != "" {
			r.PaymentId = &v
		}
		created, err := ac.tx.PutDoc(ctx, repository.Doc{Kind: repository.KindRefund, ID: m.ID, Outlet: ac.outlet, Date: r.BusinessDate.String(), Sort: ac.at().String(), K1: r.BillId, Body: marshalDoc(r)})
		if err != nil {
			return err
		}
		if !created {
			continue
		}
		date := r.BusinessDate.String()
		day, late, err := ac.lateDay(ctx, date, "")
		if err != nil {
			return err
		}
		contrib := emptyTotals()
		contrib.RefundTotal = r.Amount
		if err := ac.aggDay(ctx, date, contrib); err != nil {
			return err
		}
		if err := ac.aggTender(ctx, date, gen.PaymentTender(r.Tender), "", 0, 0, r.Amount.AmountPaise); err != nil {
			return err
		}
		if err := ac.discountRow(ctx, "refund", r.Id, "", r.ReasonCodeId, r.Amount.AmountPaise, date); err != nil {
			return err
		}
		if err := ac.settle(ctx, day, late, "refund", r.Id, "", date, contrib, nil); err != nil {
			return err
		}
		if b, err := load[BillRec](ctx, ac.tx, repository.KindBill, r.BillId); err == nil {
			net := b.Detail.Paid.AmountPaise
			refunded, _, err := ac.refundedFor(ctx, r.BillId)
			if err != nil {
				return err
			}
			if net-refunded <= b.Detail.Total.AmountPaise {
				if err := ac.resolveException(ctx, "payments_exceed_total", r.BillId, r.Id); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// ---- credit notes ----

func (ac *applyCtx) creditNotes(ctx context.Context) error {
	for _, m := range ac.g["credit_note"] {
		if m.K != "insert" {
			continue
		}
		f := m.F
		n := gen.CreditNote{Id: m.ID, OutletId: ac.outlet, Number: fstr(f, "number"), BillId: fstr(f, "bill_id"), BillNumber: fstr(f, "bill_number"),
			BillInvoiceDate: mustDate(fstr(f, "bill_invoice_date")), BusinessDate: mustDate(fstr(f, "business_date")), InvoiceDate: mustDate(fstr(f, "invoice_date")),
			DeviceId: ac.op.Origin, MemberId: ac.op.Actor, CalcV: int(fint(f, "calc_v")), Total: money(fint(f, "total_paise")), RoundOff: money(fint(f, "round_off_paise")),
			Lines: []gen.BillLine{}, TaxLines: []gen.TaxLine{}, CreatedAt: ac.at()}
		if v := fstr(f, "reason_code_id"); v != "" {
			n.ReasonCodeId = &v
		}
		if v := fstr(f, "rebill_group_id"); v != "" {
			n.RebillGroupId = &v
		}
		if v := fstr(f, "ruleset_id"); v != "" {
			n.RulesetId = &v
		}
		if v := fstr(f, "refund_tender"); v != "" {
			n.RefundTender = ptr(gen.CreditNoteRefundTender(v))
		}
		calc := CreditCalc{SeriesCode: fstr(f, "series_code"), FinancialYear: fstr(f, "financial_year"), TotalPaise: fint(f, "total_paise"), OpOrigin: ac.op.Origin, OpSeq: ac.op.Seq}
		code, fy, _, _ := splitNumber(n.Number)
		if calc.SeriesCode == "" {
			calc.SeriesCode = code
		}
		if calc.FinancialYear == "" {
			calc.FinancialYear = fy
		}
		for _, c := range ac.g["credit_note_line"] {
			if fstr(c.F, "credit_note_id") != m.ID {
				continue
			}
			ln := int(fint(c.F, "line_no"))
			n.Lines = append(n.Lines, gen.BillLine{LineNo: ln, ItemId: "", ItemName: fstr(c.F, "item_name"), QtyMilli: fint(c.F, "qty_milli"), Net: money(fint(c.F, "net_paise")),
				TaxClass: fstr(c.F, "tax_class"), UnitPrice: money(0), ModifierDelta: money(0), LineDiscount: money(0), BillDiscountAlloc: money(0), CategoryName: ""})
			calc.Credited = append(calc.Credited, CreditLine{BillLineID: fstr(c.F, "bill_line_id"), LineNo: ln, QtyMilli: fint(c.F, "qty_milli"), NetPaise: fint(c.F, "net_paise")})
		}
		for _, c := range ac.g["credit_note_tax_line"] {
			if fstr(c.F, "credit_note_id") == m.ID {
				t := taxLineFrom(c.F)
				n.TaxLines = append(n.TaxLines, t)
				if t.GstApplies { // the value of a non-GST line is not taxable value
					calc.TaxablePaise += t.Taxable.AmountPaise
				}
				calc.CGSTPaise += t.Cgst.AmountPaise
				calc.SGSTPaise += t.Sgst.AmountPaise
			}
		}
		// Enrich the lines from the original bill when it is known (item and category names, ids).
		rec := CreditRec{Note: n, Calc: calc}
		if orig, err := load[BillRec](ctx, ac.tx, repository.KindBill, n.BillId); err == nil {
			enrichCredit(&rec, orig)
		}
		day, late, err := ac.lateDay(ctx, n.BusinessDate.String(), n.Number)
		if err != nil {
			return err
		}
		var verr error
		if rec.Note.Verification, verr = ac.verifyCredit(ctx, rec); verr != nil {
			return verr
		}
		created, err := ac.tx.PutDoc(ctx, repository.Doc{Kind: repository.KindCreditNote, ID: m.ID, Outlet: ac.outlet, Date: n.BusinessDate.String(), Sort: ac.at().String(), K1: n.BillId, Body: marshalDoc(rec)})
		if err != nil {
			return err
		}
		if !created {
			continue
		}
		if err := ac.afterCredit(ctx, rec, day, late); err != nil {
			return err
		}
	}
	return nil
}

func (ac *applyCtx) afterCredit(ctx context.Context, rec CreditRec, day gen.BusinessDay, late bool) error {
	n := rec.Note
	date, inv := n.BusinessDate.String(), n.InvoiceDate.String()
	if err := ac.register(ctx, rec.Calc.SeriesCode, rec.Calc.FinancialYear, n.Number, "credit_note", "device", n.DeviceId, "credit_note", n.Id, inv); err != nil {
		return err
	}
	contrib := emptyTotals()
	contrib.CreditNoteCount, contrib.CreditNoteTotal = 1, n.Total
	if err := ac.aggDay(ctx, date, contrib); err != nil {
		return err
	}
	for _, t := range n.TaxLines {
		if err := ac.aggTax(ctx, inv, t, false, 0, n.TaxLines); err != nil {
			return err
		}
	}
	if err := ac.discountRow(ctx, "credit_note", n.Id, n.Number, deref(n.ReasonCodeId), n.Total.AmountPaise, date); err != nil {
		return err
	}
	if b, err := ac.tx.GetDoc(ctx, repository.KindBill, n.BillId); err == nil {
		br, err := unmarshalDoc[BillRec](b)
		if err != nil {
			return err
		}
		addTo(&br.Detail.Credited, n.Total.AmountPaise)
		b.Body = marshalDoc(br)
		if err := ac.tx.SaveDoc(ctx, b); err != nil {
			return err
		}
	}
	if err := ac.flag(ctx, "credit_note", n.Id, n.OutletId, n.Verification, date); err != nil {
		return err
	}
	if err := ac.settle(ctx, day, late, "credit_note", n.Id, n.Number, date, contrib, []gen.SeriesRange{{SeriesCode: rec.Calc.SeriesCode, DocType: "credit_note", FirstNumber: n.Number, LastNumber: n.Number, Count: 1}}); err != nil {
		return err
	}
	if err := ac.checkCreditCap(ctx, n.BillId); err != nil {
		return err
	}
	return ac.s.publish(ctx, ac.tx, ac.outlet, "billing.credit_note.recorded", n.Id, ac.op.Actor, map[string]any{
		"credit_note_id": n.Id, "bill_id": n.BillId, "outlet_id": ac.outlet, "business_date": date, "invoice_date": inv,
		"total_paise": n.Total.AmountPaise, "late": late, "verification": recordedVerification(n.Verification.Status)})
}
