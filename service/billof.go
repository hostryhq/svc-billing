package service

import "github.com/hostryhq/svc-billing/internal/gen"

// billOf is the list view of a bill detail.
func billOf(d gen.BillDetail) gen.Bill {
	return gen.Bill{BusinessDate: d.BusinessDate, CalcV: d.CalcV, CashierId: d.CashierId, CheckId: d.CheckId, Credited: d.Credited, DeviceId: d.DeviceId,
		DocType: d.DocType, Eco: d.Eco, FinalizedAt: d.FinalizedAt, Id: d.Id, InvoiceDate: d.InvoiceDate, Late: d.Late, Number: d.Number,
		OrderType: gen.BillOrderType(d.OrderType), OutletId: d.OutletId, Paid: d.Paid, PaymentStatus: d.PaymentStatus, PriceMode: gen.BillPriceMode(d.PriceMode), RoundOff: d.RoundOff,
		RulesetId: d.RulesetId, SourceKind: d.SourceKind, Total: d.Total, Verification: d.Verification, WaiterId: d.WaiterId}
}
