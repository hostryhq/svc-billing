package service

import (
	"context"

	"github.com/hostryhq/go-common/apperr"

	"github.com/hostryhq/svc-billing/internal/gen"
	"github.com/hostryhq/svc-billing/repository"
)

// Page is one page of a list: the items and the id to continue after ("" on the last page).
type Page[T any] struct {
	Items []T
	Next  string
}

const scanChunk = 200

// pageOf lists documents of q (Outlets, dates and keys already set), decoding each into R, keeping those keep accepts
// and mapping them to T, until limit+1 are found. The cursor is the id of the last document of the page.
func pageOf[R, T any](ctx context.Context, tx repository.Tx, q repository.Query, after string, limit int, keep func(repository.Doc, R) bool, conv func(R) T) (Page[T], error) {
	var out Page[T]
	q.After, q.Limit = after, scanChunk
	var lastID string
	for {
		docs, err := tx.FindDocs(ctx, q)
		if err != nil {
			return out, err
		}
		for _, d := range docs {
			var r R
			if err := jsonUnmarshal(d.Body, &r); err != nil {
				return out, err
			}
			q.After = d.ID
			if keep != nil && !keep(d, r) {
				continue
			}
			if len(out.Items) == limit {
				out.Next = lastID
				return out, nil
			}
			out.Items = append(out.Items, conv(r))
			lastID = d.ID
		}
		if len(docs) < scanChunk {
			return out, nil
		}
	}
}

func identity[T any](v T) T { return v }

// ---- bills ----

// BillFilter narrows the bill register.
type BillFilter struct {
	BusinessDate, InvoiceFrom, InvoiceTo, Number, SourceKind, Verification string
}

func (s *Service) ListBills(ctx context.Context, outlet string, f BillFilter, after string, limit int) (Page[gen.Bill], error) {
	scope, err := s.Scope(ctx, "report.view.outlet", outlet)
	if err != nil {
		return Page[gen.Bill]{}, err
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (Page[gen.Bill], error) {
		q := repository.Query{Kind: repository.KindBill, Outlets: scope, K1: f.Number}
		if f.BusinessDate != "" {
			q.DateFrom, q.DateTo = f.BusinessDate, f.BusinessDate
		}
		return pageOf(ctx, tx, q, after, limit, func(_ repository.Doc, r BillRec) bool {
			d := r.Detail
			inv := d.InvoiceDate.String()
			return (f.InvoiceFrom == "" || inv >= f.InvoiceFrom) && (f.InvoiceTo == "" || inv <= f.InvoiceTo) &&
				(f.SourceKind == "" || string(d.SourceKind) == f.SourceKind) && (f.Verification == "" || string(d.Verification.Status) == f.Verification)
		}, func(r BillRec) gen.Bill { return billOf(r.Detail) })
	})
}

func (s *Service) GetBill(ctx context.Context, id string) (gen.BillDetail, error) {
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (gen.BillDetail, error) {
		d, err := tx.GetDoc(ctx, repository.KindBill, id)
		if err != nil {
			return gen.BillDetail{}, mapNotFound(err)
		}
		if err := s.requireOutlet(ctx, "report.view.outlet", d.Outlet); err != nil {
			return gen.BillDetail{}, err
		}
		r, err := unmarshalDoc[BillRec](d)
		return r.Detail, err
	})
}

// ---- credit notes ----

func (s *Service) ListCreditNotes(ctx context.Context, outlet, date, billID, after string, limit int) (Page[gen.CreditNote], error) {
	scope, err := s.Scope(ctx, "report.view.outlet", outlet)
	if err != nil {
		return Page[gen.CreditNote]{}, err
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (Page[gen.CreditNote], error) {
		q := repository.Query{Kind: repository.KindCreditNote, Outlets: scope, K1: billID}
		if date != "" {
			q.DateFrom, q.DateTo = date, date
		}
		return pageOf(ctx, tx, q, after, limit, nil, func(r CreditRec) gen.CreditNote { return r.Note })
	})
}

func (s *Service) GetCreditNote(ctx context.Context, id string) (gen.CreditNote, error) {
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (gen.CreditNote, error) {
		d, err := tx.GetDoc(ctx, repository.KindCreditNote, id)
		if err != nil {
			return gen.CreditNote{}, mapNotFound(err)
		}
		if err := s.requireOutlet(ctx, "report.view.outlet", d.Outlet); err != nil {
			return gen.CreditNote{}, err
		}
		r, err := unmarshalDoc[CreditRec](d)
		return r.Note, err
	})
}

// ---- plain lists ----

func listPlain[T any](s *Service, ctx context.Context, kind, outlet, date, billID, tender, after string, limit int) (Page[T], error) {
	scope, err := s.Scope(ctx, "report.view.outlet", outlet)
	if err != nil {
		return Page[T]{}, err
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (Page[T], error) {
		q := repository.Query{Kind: kind, Outlets: scope, K1: billID, K2: tender}
		if date != "" {
			q.DateFrom, q.DateTo = date, date
		}
		return pageOf[T, T](ctx, tx, q, after, limit, nil, identity[T])
	})
}

func (s *Service) ListPayments(ctx context.Context, outlet, date, billID, tender, after string, limit int) (Page[gen.Payment], error) {
	return listPlain[gen.Payment](s, ctx, repository.KindPayment, outlet, date, billID, tender, after, limit)
}

func (s *Service) ListRefunds(ctx context.Context, outlet, date, after string, limit int) (Page[gen.Refund], error) {
	return listPlain[gen.Refund](s, ctx, repository.KindRefund, outlet, date, "", "", after, limit)
}

func (s *Service) ListShifts(ctx context.Context, outlet, date, after string, limit int) (Page[gen.Shift], error) {
	return listPlain[gen.Shift](s, ctx, repository.KindShift, outlet, date, "", "", after, limit)
}

func (s *Service) ListInternalConsumptions(ctx context.Context, outlet, date, after string, limit int) (Page[gen.InternalConsumption], error) {
	return listPlain[gen.InternalConsumption](s, ctx, repository.KindInternal, outlet, date, "", "", after, limit)
}

// ---- business days ----

func (s *Service) ListBusinessDays(ctx context.Context, outlet, status, after string, limit int) (Page[gen.BusinessDay], error) {
	scope, err := s.Scope(ctx, "report.view.outlet", outlet)
	if err != nil {
		return Page[gen.BusinessDay]{}, err
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (Page[gen.BusinessDay], error) {
		return pageOf[gen.BusinessDay, gen.BusinessDay](ctx, tx, repository.Query{Kind: repository.KindBusinessDay, Outlets: scope, K1: status}, after, limit, nil, identity[gen.BusinessDay])
	})
}

func (s *Service) GetBusinessDay(ctx context.Context, id string) (gen.BusinessDay, error) {
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (gen.BusinessDay, error) {
		d, err := tx.GetDoc(ctx, repository.KindBusinessDay, id)
		if err != nil {
			return gen.BusinessDay{}, mapNotFound(err)
		}
		if err := s.requireOutlet(ctx, "report.view.outlet", d.Outlet); err != nil {
			return gen.BusinessDay{}, err
		}
		return unmarshalDoc[gen.BusinessDay](d)
	})
}

// ListBusinessDayRevisions lists the Z revisions of a day, revision ascending.
func (s *Service) ListBusinessDayRevisions(ctx context.Context, dayID, after string, limit int) (Page[gen.BusinessDayRevision], error) {
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (Page[gen.BusinessDayRevision], error) {
		d, err := tx.GetDoc(ctx, repository.KindBusinessDay, dayID)
		if err != nil {
			return Page[gen.BusinessDayRevision]{}, mapNotFound(err)
		}
		if err := s.requireOutlet(ctx, "report.view.outlet", d.Outlet); err != nil {
			return Page[gen.BusinessDayRevision]{}, err
		}
		return pageOf[gen.BusinessDayRevision, gen.BusinessDayRevision](ctx, tx, repository.Query{Kind: repository.KindRevision, K1: dayID, Asc: true}, after, limit, nil, identity[gen.BusinessDayRevision])
	})
}

// ---- register ----

// registerScope: the register is per registration, not per outlet, so it needs the permission at every outlet.
func (s *Service) registerScope(ctx context.Context) error {
	sc, err := s.Scope(ctx, "report.view.outlet", "")
	if err != nil {
		return err
	}
	if sc != nil {
		return apperr.New(apperr.Forbidden, "forbidden", "the document register needs the permission at every outlet")
	}
	return nil
}

func (s *Service) ListDocumentRegister(ctx context.Context, fy, kind, after string, limit int) (Page[gen.RegisterSeries], error) {
	if err := s.registerScope(ctx); err != nil {
		return Page[gen.RegisterSeries]{}, err
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (Page[gen.RegisterSeries], error) {
		return pageOf(ctx, tx, repository.Query{Kind: repository.KindSeries, K1: kind, Asc: true}, after, limit,
			func(_ repository.Doc, r SeriesRec) bool { return fy == "" || r.Series.FinancialYear == fy }, func(r SeriesRec) gen.RegisterSeries { return r.Series })
	})
}

func (s *Service) ListDocumentGaps(ctx context.Context, status, kind, after string, limit int) (Page[gen.RegisterGap], error) {
	if err := s.registerScope(ctx); err != nil {
		return Page[gen.RegisterGap]{}, err
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (Page[gen.RegisterGap], error) {
		return pageOf(ctx, tx, repository.Query{Kind: repository.KindGap, K2: status}, after, limit,
			func(_ repository.Doc, g gen.RegisterGap) bool { return kind == "" || string(g.Kind) == kind }, identity[gen.RegisterGap])
	})
}

// GetSeriesLastSeen answers tenancy and identity (internal, service caller): the last number seen of a series. The
// series is named by its tenancy registry id, which billing learns with RecordSeries.
func (s *Service) GetSeriesLastSeen(ctx context.Context, seriesID string) (gen.SeriesLastSeen, error) {
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (gen.SeriesLastSeen, error) {
		docs, err := tx.FindDocs(ctx, repository.Query{Kind: repository.KindSeries, K2: seriesID})
		if err != nil || len(docs) == 0 {
			return gen.SeriesLastSeen{}, notFoundOr(err)
		}
		r, err := unmarshalDoc[SeriesRec](docs[0])
		if err != nil {
			return gen.SeriesLastSeen{}, err
		}
		out := gen.SeriesLastSeen{SeriesId: seriesID, LastNumber: r.Series.LastNumber, Count: r.Series.Count, AsOf: tsOr(r.Series.LastSeenAt, s.clock.Now())}
		return out, nil
	})
}

func notFoundOr(err error) error {
	if err != nil {
		return err
	}
	return notFound()
}

// RecordSeries tells billing which tenancy registry series a register series is (ids and codes only).
func (s *Service) RecordSeries(ctx context.Context, seriesID, registrationID, code, fy string) error {
	return s.repo.InTx(ctx, func(ctx context.Context, tx repository.Tx) error {
		id := seriesKey(registrationID, code, fy)
		d, err := tx.GetDoc(ctx, repository.KindSeries, id)
		if err != nil {
			return mapNotFound(err)
		}
		d.K2 = seriesID
		return tx.SaveDoc(ctx, d)
	})
}

// ---- exceptions ----

func (s *Service) ListExceptions(ctx context.Context, outlet, status, kind, after string, limit int) (Page[gen.Exception], error) {
	scope, err := s.Scope(ctx, "exceptions.view", outlet)
	if err != nil {
		return Page[gen.Exception]{}, err
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (Page[gen.Exception], error) {
		return pageOf(ctx, tx, repository.Query{Kind: repository.KindException, Outlets: scope, K1: kind, K2: status}, after, limit, nil, identity[gen.Exception])
	})
}

func (s *Service) GetException(ctx context.Context, id string) (gen.Exception, error) {
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (gen.Exception, error) {
		d, err := tx.GetDoc(ctx, repository.KindException, id)
		if err != nil {
			return gen.Exception{}, mapNotFound(err)
		}
		if err := s.requireOutlet(ctx, "exceptions.view", d.Outlet); err != nil {
			return gen.Exception{}, err
		}
		return unmarshalDoc[gen.Exception](d)
	})
}

// ---- tax rule sets as received by the organisation ----

func (s *Service) ListTaxRuleSets(ctx context.Context, inForceOn, after string, limit int) (Page[gen.TaxRuleSet], error) {
	if _, err := s.Scope(ctx, "settings.read", ""); err != nil {
		return Page[gen.TaxRuleSet]{}, err
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (Page[gen.TaxRuleSet], error) {
		var best string
		if inForceOn != "" {
			all, _, err := loadAll[RuleSetRec](ctx, tx, repository.Query{Kind: repository.KindRuleSet})
			if err != nil {
				return Page[gen.TaxRuleSet]{}, err
			}
			var sets []RuleSetRec
			sets = append(sets, all...)
			bestEff := ""
			for _, r := range sets {
				if r.EffectiveDate <= inForceOn && r.EffectiveDate >= bestEff {
					best, bestEff = r.ID, r.EffectiveDate
				}
			}
			if best == "" {
				return Page[gen.TaxRuleSet]{}, nil
			}
		}
		return pageOf(ctx, tx, repository.Query{Kind: repository.KindRuleSet}, after, limit,
			func(_ repository.Doc, r RuleSetRec) bool { return best == "" || r.ID == best }, func(r RuleSetRec) gen.TaxRuleSet { return r.api() })
	})
}

func (s *Service) GetTaxRuleSet(ctx context.Context, id string) (gen.TaxRuleSet, error) {
	if _, err := s.Scope(ctx, "settings.read", ""); err != nil {
		return gen.TaxRuleSet{}, err
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (gen.TaxRuleSet, error) {
		r, err := load[RuleSetRec](ctx, tx, repository.KindRuleSet, id)
		if err != nil {
			return gen.TaxRuleSet{}, mapNotFound(err)
		}
		return r.api(), nil
	})
}

func (r RuleSetRec) api() gen.TaxRuleSet {
	out := gen.TaxRuleSet{Id: r.ID, Version: r.Version, Name: r.Name, EffectiveDate: mustDate(r.EffectiveDate), CalcV: r.CalcV, ReviewStatus: gen.TaxRuleSetReviewStatus(r.ReviewStatus),
		ContentHash: r.ContentHash, Rules: r.Rules}
	if t, err := tstampParse(r.PublishedAt); err == nil {
		out.PublishedAt = fromTime(t)
	}
	if out.Rules == nil {
		out.Rules = []gen.TaxRule{}
	}
	return out
}
