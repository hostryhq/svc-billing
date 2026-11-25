package api

import (
	"net/http"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/hostryhq/svc-billing/internal/gen"
	"github.com/hostryhq/svc-billing/service"
)

func rng(from, to *openapi_types.Date) service.Range {
	var r service.Range
	if from != nil {
		r.From = from.String()
	}
	if to != nil {
		r.To = to.String()
	}
	return r
}

// bounds returns the dates echoed in a report: the requested ones, or the range of the rows.
func bounds(from, to *openapi_types.Date) (openapi_types.Date, openapi_types.Date) {
	var f, t openapi_types.Date
	if from != nil {
		f = *from
	}
	if to != nil {
		t = *to
	}
	return f, t
}

func (s *Server) GetDaySummaryReport(w http.ResponseWriter, r *http.Request, p gen.GetDaySummaryReportParams) {
	after, n, err := pageParams(p.Cursor, p.Limit, 50, 200)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rep, err := s.svc.DaySummary(r.Context(), outletOf(p.XOutletId), rng(p.From, p.To), after, n)
	f, t := bounds(p.From, p.To)
	s.reply(w, r, http.StatusOK, gen.DaySummaryReport{OutletId: rep.OutletID, From: f, To: t, Basis: gen.DaySummaryReportBasisBusinessDate,
		Items: append([]gen.DaySummaryRow{}, rep.Items...), NextCursor: nextCursor(rep.Next)}, err)
}

func (s *Server) GetTaxSummaryReport(w http.ResponseWriter, r *http.Request, p gen.GetTaxSummaryReportParams) {
	after, n, err := pageParams(p.Cursor, p.Limit, 50, 200)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rep, err := s.svc.TaxSummary(r.Context(), outletOf(p.XOutletId), rng(p.From, p.To), after, n)
	f, t := bounds(p.From, p.To)
	out := gen.TaxSummaryReport{OutletId: rep.OutletID, From: f, To: t, Basis: "invoice_date", Items: append([]gen.TaxSummaryRow{}, rep.Items...), NextCursor: nextCursor(rep.Next)}
	out.Totals.NetTaxable, out.Totals.NetCgst, out.Totals.NetSgst, out.Totals.NonGst = money(rep.Totals["net_taxable"]), money(rep.Totals["net_cgst"]), money(rep.Totals["net_sgst"]), money(rep.Totals["non_gst"])
	s.reply(w, r, http.StatusOK, out, err)
}

func money(n int64) gen.Money { return gen.Money{AmountPaise: n, Currency: gen.INR} }

func (s *Server) GetTenderSummaryReport(w http.ResponseWriter, r *http.Request, p gen.GetTenderSummaryReportParams) {
	after, n, err := pageParams(p.Cursor, p.Limit, 50, 200)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rep, err := s.svc.TenderSummary(r.Context(), outletOf(p.XOutletId), rng(p.From, p.To), after, n)
	f, t := bounds(p.From, p.To)
	s.reply(w, r, http.StatusOK, gen.TenderSummaryReport{OutletId: rep.OutletID, From: f, To: t, Basis: "business_date", Items: append([]gen.TenderSummaryRow{}, rep.Items...), NextCursor: nextCursor(rep.Next)}, err)
}

func (s *Server) GetItemSalesReport(w http.ResponseWriter, r *http.Request, p gen.GetItemSalesReportParams) {
	after, n, err := pageParams(p.Cursor, p.Limit, 50, 200)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rep, err := s.svc.ItemSales(r.Context(), outletOf(p.XOutletId), rng(p.From, p.To), after, n)
	f, t := bounds(p.From, p.To)
	s.reply(w, r, http.StatusOK, gen.ItemSalesReport{OutletId: rep.OutletID, From: f, To: t, Basis: "business_date", Items: append([]gen.ItemSalesRow{}, rep.Items...), NextCursor: nextCursor(rep.Next)}, err)
}

func (s *Server) GetCategorySalesReport(w http.ResponseWriter, r *http.Request, p gen.GetCategorySalesReportParams) {
	after, n, err := pageParams(p.Cursor, p.Limit, 50, 200)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rep, err := s.svc.CategorySales(r.Context(), outletOf(p.XOutletId), rng(p.From, p.To), after, n)
	f, t := bounds(p.From, p.To)
	s.reply(w, r, http.StatusOK, gen.CategorySalesReport{OutletId: rep.OutletID, From: f, To: t, Basis: "business_date", Items: append([]gen.CategorySalesRow{}, rep.Items...), NextCursor: nextCursor(rep.Next)}, err)
}

func (s *Server) GetStaffSalesReport(w http.ResponseWriter, r *http.Request, p gen.GetStaffSalesReportParams) {
	after, n, err := pageParams(p.Cursor, p.Limit, 50, 200)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rep, err := s.svc.StaffSales(r.Context(), outletOf(p.XOutletId), rng(p.From, p.To), after, n)
	f, t := bounds(p.From, p.To)
	s.reply(w, r, http.StatusOK, gen.StaffSalesReport{OutletId: rep.OutletID, From: f, To: t, Basis: "business_date", Items: append([]gen.StaffSalesRow{}, rep.Items...), NextCursor: nextCursor(rep.Next)}, err)
}

func (s *Server) GetHourlySalesReport(w http.ResponseWriter, r *http.Request, p gen.GetHourlySalesReportParams) {
	after, n, err := pageParams(p.Cursor, p.Limit, 50, 200)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rep, err := s.svc.HourlySales(r.Context(), outletOf(p.XOutletId), rng(p.From, p.To), after, n)
	f, t := bounds(p.From, p.To)
	s.reply(w, r, http.StatusOK, gen.HourlySalesReport{OutletId: rep.OutletID, From: f, To: t, Basis: "business_date", Items: append([]gen.HourlySalesRow{}, rep.Items...), NextCursor: nextCursor(rep.Next)}, err)
}

func (s *Server) GetDiscountReport(w http.ResponseWriter, r *http.Request, p gen.GetDiscountReportParams) {
	after, n, err := pageParams(p.Cursor, p.Limit, 50, 200)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rep, err := s.svc.Discounts(r.Context(), outletOf(p.XOutletId), rng(p.From, p.To), str(p.Kind), after, n)
	f, t := bounds(p.From, p.To)
	out := gen.DiscountReport{OutletId: rep.OutletID, From: f, To: t, Basis: "business_date", Items: append([]gen.DiscountRow{}, rep.Items...), NextCursor: nextCursor(rep.Next)}
	out.Totals.Count, out.Totals.Value = int(rep.Totals["count"]), money(rep.Totals["value"])
	s.reply(w, r, http.StatusOK, out, err)
}
