package api

import (
	"net/http"

	"github.com/hostryhq/svc-billing/internal/gen"
	"github.com/hostryhq/svc-billing/repository"
	"github.com/hostryhq/svc-billing/service"
)

func outletOf(p *string) string { return deref(p) }

func str[T ~string](p *T) string { return string(deref(p)) }

// ---- bills, credit notes, payments, refunds ----

func (s *Server) ListBills(w http.ResponseWriter, r *http.Request, p gen.ListBillsParams) {
	after, n, err := pageParams(p.Cursor, p.Limit, 50, 200)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	f := service.BillFilter{Number: deref(p.Number), SourceKind: str(p.SourceKind), Verification: str(p.Verification)}
	if p.BusinessDate != nil {
		f.BusinessDate = p.BusinessDate.String()
	}
	if p.InvoiceFrom != nil {
		f.InvoiceFrom = p.InvoiceFrom.String()
	}
	if p.InvoiceTo != nil {
		f.InvoiceTo = p.InvoiceTo.String()
	}
	pg, err := s.svc.ListBills(r.Context(), outletOf(p.XOutletId), f, after, n)
	out := gen.BillPage{Items: append([]gen.Bill{}, pg.Items...), NextCursor: nextCursor(pg.Next)}
	s.reply(w, r, http.StatusOK, out, err)
}

func (s *Server) GetBill(w http.ResponseWriter, r *http.Request, id gen.Id) {
	v, err := s.svc.GetBill(r.Context(), id)
	s.reply(w, r, http.StatusOK, v, err)
}

func (s *Server) ListCreditNotes(w http.ResponseWriter, r *http.Request, p gen.ListCreditNotesParams) {
	after, n, err := pageParams(p.Cursor, p.Limit, 50, 200)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	date := ""
	if p.BusinessDate != nil {
		date = p.BusinessDate.String()
	}
	pg, err := s.svc.ListCreditNotes(r.Context(), outletOf(p.XOutletId), date, deref(p.BillId), after, n)
	s.reply(w, r, http.StatusOK, gen.CreditNotePage{Items: append([]gen.CreditNote{}, pg.Items...), NextCursor: nextCursor(pg.Next)}, err)
}

func (s *Server) GetCreditNote(w http.ResponseWriter, r *http.Request, id gen.Id) {
	v, err := s.svc.GetCreditNote(r.Context(), id)
	s.reply(w, r, http.StatusOK, v, err)
}

func (s *Server) ListPayments(w http.ResponseWriter, r *http.Request, p gen.ListPaymentsParams) {
	after, n, err := pageParams(p.Cursor, p.Limit, 50, 200)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	date := ""
	if p.BusinessDate != nil {
		date = p.BusinessDate.String()
	}
	pg, err := s.svc.ListPayments(r.Context(), outletOf(p.XOutletId), date, deref(p.BillId), str(p.Tender), after, n)
	s.reply(w, r, http.StatusOK, gen.PaymentPage{Items: append([]gen.Payment{}, pg.Items...), NextCursor: nextCursor(pg.Next)}, err)
}

func (s *Server) ListRefunds(w http.ResponseWriter, r *http.Request, p gen.ListRefundsParams) {
	after, n, err := pageParams(p.Cursor, p.Limit, 50, 200)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	date := ""
	if p.BusinessDate != nil {
		date = p.BusinessDate.String()
	}
	pg, err := s.svc.ListRefunds(r.Context(), outletOf(p.XOutletId), date, after, n)
	s.reply(w, r, http.StatusOK, gen.RefundPage{Items: append([]gen.Refund{}, pg.Items...), NextCursor: nextCursor(pg.Next)}, err)
}

func (s *Server) ListShifts(w http.ResponseWriter, r *http.Request, p gen.ListShiftsParams) {
	after, n, err := pageParams(p.Cursor, p.Limit, 50, 200)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	date := ""
	if p.BusinessDate != nil {
		date = p.BusinessDate.String()
	}
	pg, err := s.svc.ListShifts(r.Context(), outletOf(p.XOutletId), date, after, n)
	s.reply(w, r, http.StatusOK, gen.ShiftPage{Items: append([]gen.Shift{}, pg.Items...), NextCursor: nextCursor(pg.Next)}, err)
}

func (s *Server) ListInternalConsumptions(w http.ResponseWriter, r *http.Request, p gen.ListInternalConsumptionsParams) {
	after, n, err := pageParams(p.Cursor, p.Limit, 50, 200)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	date := ""
	if p.BusinessDate != nil {
		date = p.BusinessDate.String()
	}
	pg, err := s.svc.ListInternalConsumptions(r.Context(), outletOf(p.XOutletId), date, after, n)
	s.reply(w, r, http.StatusOK, gen.InternalConsumptionPage{Items: append([]gen.InternalConsumption{}, pg.Items...), NextCursor: nextCursor(pg.Next)}, err)
}

// ---- business days ----

func (s *Server) ListBusinessDays(w http.ResponseWriter, r *http.Request, p gen.ListBusinessDaysParams) {
	after, n, err := pageParams(p.Cursor, p.Limit, 50, 200)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	pg, err := s.svc.ListBusinessDays(r.Context(), outletOf(p.XOutletId), str(p.Status), after, n)
	s.reply(w, r, http.StatusOK, gen.BusinessDayPage{Items: append([]gen.BusinessDay{}, pg.Items...), NextCursor: nextCursor(pg.Next)}, err)
}

func (s *Server) GetBusinessDay(w http.ResponseWriter, r *http.Request, id gen.Id) {
	v, err := s.svc.GetBusinessDay(r.Context(), id)
	s.reply(w, r, http.StatusOK, v, err)
}

func (s *Server) ListBusinessDayRevisions(w http.ResponseWriter, r *http.Request, id gen.Id, p gen.ListBusinessDayRevisionsParams) {
	after, n, err := pageParams(p.Cursor, p.Limit, 50, 200)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	pg, err := s.svc.ListBusinessDayRevisions(r.Context(), id, after, n)
	s.reply(w, r, http.StatusOK, gen.BusinessDayRevisionPage{Items: append([]gen.BusinessDayRevision{}, pg.Items...), NextCursor: nextCursor(pg.Next)}, err)
}

// ---- register, exceptions, internal ----

func (s *Server) ListDocumentRegister(w http.ResponseWriter, r *http.Request, p gen.ListDocumentRegisterParams) {
	after, n, err := pageParams(p.Cursor, p.Limit, 50, 200)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	pg, err := s.svc.ListDocumentRegister(r.Context(), deref(p.FinancialYear), str(p.Kind), after, n)
	s.reply(w, r, http.StatusOK, gen.RegisterSeriesPage{Items: append([]gen.RegisterSeries{}, pg.Items...), NextCursor: nextCursor(pg.Next)}, err)
}

func (s *Server) ListDocumentGaps(w http.ResponseWriter, r *http.Request, p gen.ListDocumentGapsParams) {
	after, n, err := pageParams(p.Cursor, p.Limit, 50, 200)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	pg, err := s.svc.ListDocumentGaps(r.Context(), str(p.Status), str(p.Kind), after, n)
	s.reply(w, r, http.StatusOK, gen.RegisterGapPage{Items: append([]gen.RegisterGap{}, pg.Items...), NextCursor: nextCursor(pg.Next)}, err)
}

func (s *Server) ListBillingExceptions(w http.ResponseWriter, r *http.Request, p gen.ListBillingExceptionsParams) {
	after, n, err := pageParams(p.Cursor, p.Limit, 50, 200)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	pg, err := s.svc.ListExceptions(r.Context(), outletOf(p.XOutletId), str(p.Status), deref(p.Kind), after, n)
	s.reply(w, r, http.StatusOK, gen.ExceptionPage{Items: append([]gen.Exception{}, pg.Items...), NextCursor: nextCursor(pg.Next)}, err)
}

func (s *Server) GetBillingException(w http.ResponseWriter, r *http.Request, id gen.Id) {
	v, err := s.svc.GetException(r.Context(), id)
	s.reply(w, r, http.StatusOK, v, err)
}

func (s *Server) GetSeriesLastSeen(w http.ResponseWriter, r *http.Request, id gen.Id) {
	v, err := s.svc.GetSeriesLastSeen(r.Context(), id)
	s.reply(w, r, http.StatusOK, v, err)
}

func (s *Server) GetBillingProjectionDigest(w http.ResponseWriter, r *http.Request, p gen.GetBillingProjectionDigestParams) {
	date := ""
	if p.BusinessDate != nil {
		date = p.BusinessDate.String()
	}
	v, err := s.svc.ProjectionDigest(r.Context(), outletOf(p.XOutletId), date)
	s.reply(w, r, http.StatusOK, v, err)
}

// ---- configuration ----

func (s *Server) GetOutletBillingConfig(w http.ResponseWriter, r *http.Request, id gen.Id) {
	v, err := s.svc.GetOutletBillingConfig(r.Context(), id)
	s.reply(w, r, http.StatusOK, v, err)
}

func (s *Server) SetDiscountLimit(w http.ResponseWriter, r *http.Request, id gen.Id, _ gen.SetDiscountLimitParams) {
	var in gen.SetDiscountLimit
	ctx, ok := s.begin(w, r, "setDiscountLimit", &in)
	if !ok {
		return
	}
	v, err := s.svc.SetDiscountLimit(ctx, id, in.DiscountLimitBp)
	s.reply(w, r, http.StatusOK, v, err)
}

func (s *Server) SetNumberingMode(w http.ResponseWriter, r *http.Request, id gen.Id, _ gen.SetNumberingModeParams) {
	var in gen.SetNumberingMode
	ctx, ok := s.begin(w, r, "setNumberingMode", &in)
	if !ok {
		return
	}
	v, err := s.svc.SetNumberingMode(ctx, id, string(in.NumberAt))
	s.reply(w, r, http.StatusOK, v, err)
}

func (s *Server) SetOutletTaxProfile(w http.ResponseWriter, r *http.Request, id gen.Id, _ gen.SetOutletTaxProfileParams) {
	var in gen.SetOutletTaxProfile
	ctx, ok := s.begin(w, r, "setOutletTaxProfile", &in)
	if !ok {
		return
	}
	var pm *string
	if in.PriceMode != nil {
		v := string(*in.PriceMode)
		pm = &v
	}
	v, err := s.svc.SetOutletTaxProfile(ctx, id, in.FinancialYear, in.SpecifiedPremises, in.FssaiNumber, pm)
	s.reply(w, r, http.StatusOK, v, err)
}

// ---- rule sets as received ----

func (s *Server) ListTaxRuleSets(w http.ResponseWriter, r *http.Request, p gen.ListTaxRuleSetsParams) {
	after, n, err := pageParams(p.Cursor, p.Limit, 50, 200)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	on := ""
	if p.InForceOn != nil {
		on = p.InForceOn.String()
	}
	pg, err := s.svc.ListTaxRuleSets(r.Context(), on, after, n)
	s.reply(w, r, http.StatusOK, gen.TaxRuleSetPage{Items: append([]gen.TaxRuleSet{}, pg.Items...), NextCursor: nextCursor(pg.Next)}, err)
}

func (s *Server) GetTaxRuleSet(w http.ResponseWriter, r *http.Request, id gen.Id) {
	v, err := s.svc.GetTaxRuleSet(r.Context(), id)
	s.reply(w, r, http.StatusOK, v, err)
}

// ---- vendor rule-set master ----

func (s *Server) ListTaxRuleSetMasters(w http.ResponseWriter, r *http.Request, p gen.ListTaxRuleSetMastersParams) {
	after, n, err := pageParams(p.Cursor, p.Limit, 25, 100)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	pg, err := s.svc.ListTaxRuleSetMasters(repository.WithNoAudit(r.Context()), str(p.State), after, n)
	s.reply(w, r, http.StatusOK, gen.TaxRuleSetMasterPage{Items: append([]gen.TaxRuleSetMaster{}, pg.Items...), NextCursor: nextCursor(pg.Next)}, err)
}

func (s *Server) CreateTaxRuleSetDraft(w http.ResponseWriter, r *http.Request, _ gen.CreateTaxRuleSetDraftParams) {
	var in gen.TaxRuleSetDraft
	ctx, ok := s.begin(w, r, "createTaxRuleSetDraft", &in)
	if !ok {
		return
	}
	v, err := s.svc.CreateTaxRuleSetDraft(ctx, in)
	s.reply(w, r, http.StatusCreated, v, err)
}

func (s *Server) GetTaxRuleSetMaster(w http.ResponseWriter, r *http.Request, id gen.Id, _ gen.GetTaxRuleSetMasterParams) {
	v, err := s.svc.GetTaxRuleSetMaster(repository.WithNoAudit(r.Context()), id)
	if err == nil {
		w.Header().Set("ETag", `"`+itoaInt(v.Revision)+`"`)
	}
	s.reply(w, r, http.StatusOK, v, err)
}

func (s *Server) UpdateTaxRuleSetDraft(w http.ResponseWriter, r *http.Request, id gen.Id, p gen.UpdateTaxRuleSetDraftParams) {
	var in gen.TaxRuleSetDraft
	ctx, ok := s.begin(w, r, "updateTaxRuleSetDraft", &in)
	if !ok {
		return
	}
	v, err := s.svc.UpdateTaxRuleSetDraft(ctx, id, p.IfMatch, in)
	if err == nil {
		w.Header().Set("ETag", `"`+itoaInt(v.Revision)+`"`)
	}
	s.reply(w, r, http.StatusOK, v, err)
}

func (s *Server) DeleteTaxRuleSetDraft(w http.ResponseWriter, r *http.Request, id gen.Id, _ gen.DeleteTaxRuleSetDraftParams) {
	ctx, ok := s.begin(w, r, "deleteTaxRuleSetDraft", nil)
	if !ok {
		return
	}
	if err := s.svc.DeleteTaxRuleSetDraft(ctx, id); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) ValidateTaxRuleSet(w http.ResponseWriter, r *http.Request, id gen.Id, _ gen.ValidateTaxRuleSetParams) {
	ctx, ok := s.begin(w, r, "validateTaxRuleSet", nil)
	if !ok {
		return
	}
	v, err := s.svc.ValidateTaxRuleSet(ctx, id)
	s.reply(w, r, http.StatusOK, v, err)
}

func (s *Server) SignTaxRuleSet(w http.ResponseWriter, r *http.Request, id gen.Id, _ gen.SignTaxRuleSetParams) {
	var in gen.SignTaxRuleSet
	ctx, ok := s.begin(w, r, "signTaxRuleSet", &in)
	if !ok {
		return
	}
	v, err := s.svc.SignTaxRuleSet(ctx, id, in.Signoff)
	s.reply(w, r, http.StatusOK, v, err)
}

func (s *Server) PublishTaxRuleSet(w http.ResponseWriter, r *http.Request, id gen.Id, p gen.PublishTaxRuleSetParams) {
	var in gen.PublishTaxRuleSet
	ctx, ok := s.begin(w, r, "publishTaxRuleSet", &in)
	if !ok {
		return
	}
	v, err := s.svc.PublishTaxRuleSet(ctx, id, p.HostryVendorApproval)
	s.reply(w, r, http.StatusOK, v, err)
}

func itoaInt(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for ; n > 0; n /= 10 {
		i--
		b[i] = byte('0' + n%10)
	}
	return string(b[i:])
}
