package service

import (
	"context"
	"sort"

	"github.com/hostryhq/svc-billing/internal/gen"
	"github.com/hostryhq/svc-billing/repository"
)

// KindSeriesIssued is the document kind of a series tenancy issued to a device (tenancy.series.issued).
const KindSeriesIssued = "series_issued"

// SeriesIssued is what billing knows of a document series from tenancy and identity: who holds it, whether it was closed
// and the last number the device stated (ADR-0014 A2.5 item 2). Ids and codes only. billing never allocates a number; the
// register only compares what arrived with this statement.
type SeriesIssued struct {
	SeriesID       string `json:"series_id"`
	Code           string `json:"code,omitempty"`
	DocType        string `json:"doc_type,omitempty"`
	RegistrationID string `json:"tax_registration_id,omitempty"`
	OutletID       string `json:"outlet_id,omitempty"`
	DeviceID       string `json:"device_id,omitempty"`
	Closed         bool   `json:"closed,omitempty"`
	CloseCause     string `json:"close_cause,omitempty"`
	// LastNumber is the device's statement of its last running number (the drain record); nil until it arrives.
	LastNumber *int64 `json:"last_number,omitempty"`
	// Reconciled is set once billing.series.reconciled was published; it is published once per series.
	Reconciled bool `json:"reconciled,omitempty"`
}

func issuedDoc(si SeriesIssued) repository.Doc {
	return repository.Doc{Kind: KindSeriesIssued, ID: si.SeriesID, Outlet: si.OutletID, K1: seriesRef(si.RegistrationID, si.Code)}
}

func seriesRef(reg, code string) string { return reg + "|" + code }

// RecordSeriesIssued registers a series tenancy issued (tenancy.series.issued). It is order-free with the closed event
// and the drain statement: each fills its own fields of one row.
func (s *Service) RecordSeriesIssued(ctx context.Context, in SeriesIssued) error {
	return s.repo.InTx(ctx, func(ctx context.Context, tx repository.Tx) error {
		return s.mergeSeries(ctx, tx, in.SeriesID, func(r *SeriesIssued) {
			r.Code, r.DocType, r.RegistrationID, r.OutletID, r.DeviceID = in.Code, in.DocType, in.RegistrationID, in.OutletID, in.DeviceID
		})
	})
}

// RecordSeriesClosed notes that a series was closed (tenancy.series.closed); it reads closed and awaiting its device until
// the statement of the last number arrives.
func (s *Service) RecordSeriesClosed(ctx context.Context, seriesID, cause string) error {
	return s.repo.InTx(ctx, func(ctx context.Context, tx repository.Tx) error {
		return s.mergeSeries(ctx, tx, seriesID, func(r *SeriesIssued) { r.Closed, r.CloseCause = true, cause })
	})
}

// SeriesStatement is a device's statement of the last number it held in one series.
type SeriesStatement struct {
	SeriesID   string
	LastNumber int64
}

// RecordDrain takes the device's statements (identity.device.drained). A second statement with another number is
// ignored: the first one stands (the gateway refuses and alerts on the control path).
func (s *Service) RecordDrain(ctx context.Context, stmts []SeriesStatement) error {
	return s.repo.InTx(ctx, func(ctx context.Context, tx repository.Tx) error {
		for _, st := range stmts {
			st := st
			if err := s.mergeSeries(ctx, tx, st.SeriesID, func(r *SeriesIssued) {
				if r.LastNumber == nil {
					r.LastNumber = &st.LastNumber
				}
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

// mergeSeries applies fn to the series row (created when absent), links the register series of its code and reconciles.
func (s *Service) mergeSeries(ctx context.Context, tx repository.Tx, seriesID string, fn func(*SeriesIssued)) error {
	var si SeriesIssued
	if err := modify(ctx, tx, repository.Doc{Kind: KindSeriesIssued, ID: seriesID}, func(r *SeriesIssued) {
		r.SeriesID = seriesID
		fn(r)
		si = *r
	}); err != nil {
		return err
	}
	d, err := tx.GetDoc(ctx, KindSeriesIssued, seriesID)
	if err != nil {
		return err
	}
	d.Outlet, d.K1 = si.OutletID, seriesRef(si.RegistrationID, si.Code)
	if err := tx.SaveDoc(ctx, d); err != nil {
		return err
	}
	if si.Code != "" {
		if err := s.linkRegister(ctx, tx, si); err != nil {
			return err
		}
	}
	return s.reconcile(ctx, tx, si)
}

// registerOf returns the register series (one per financial year) of a registration and code, oldest year first.
func registerOf(ctx context.Context, tx repository.Tx, reg, code string) ([]SeriesRec, []repository.Doc, error) {
	recs, docs, err := loadAll[SeriesRec](ctx, tx, repository.Query{Kind: repository.KindSeries, Asc: true})
	if err != nil {
		return nil, nil, err
	}
	var outR []SeriesRec
	var outD []repository.Doc
	for i, r := range recs {
		if r.Series.TaxRegistrationId == reg && r.Series.SeriesCode == code {
			outR, outD = append(outR, r), append(outD, docs[i])
		}
	}
	return outR, outD, nil
}

// linkRegister names the tenancy series id on the register rows of its code, so last-seen answers by series id.
func (s *Service) linkRegister(ctx context.Context, tx repository.Tx, si SeriesIssued) error {
	_, docs, err := registerOf(ctx, tx, si.RegistrationID, si.Code)
	if err != nil {
		return err
	}
	for _, d := range docs {
		if d.K2 != si.SeriesID {
			d.K2 = si.SeriesID
			if err := tx.SaveDoc(ctx, d); err != nil {
				return err
			}
		}
	}
	return nil
}

// missingUpTo counts the numbers in 1..last that the intervals do not hold.
func missingUpTo(r [][2]int64, last int64) int64 {
	var held int64
	for _, iv := range r {
		lo, hi := iv[0], iv[1]
		if hi > last {
			hi = last
		}
		if hi >= lo {
			held += hi - lo + 1
		}
	}
	return last - held
}

// reconcile publishes billing.series.reconciled, once, when the series is closed, its device stated the last number and the
// register holds every number up to it. Numbers still missing stay open gaps (an alert, never silent): the holes the
// register already raised and, past the highest number seen, a tail gap up to the statement.
func (s *Service) reconcile(ctx context.Context, tx repository.Tx, si SeriesIssued) error {
	if !si.Closed || si.LastNumber == nil || si.Reconciled || si.Code == "" {
		return nil
	}
	last := *si.LastNumber
	recs, docs, err := registerOf(ctx, tx, si.RegistrationID, si.Code)
	if err != nil {
		return err
	}
	var latest *SeriesRec
	if n := len(recs); n > 0 {
		sort.SliceStable(recs, func(i, j int) bool { return recs[i].Series.FinancialYear < recs[j].Series.FinancialYear })
		latest = &recs[len(recs)-1]
		_ = docs
	}
	var ranges [][2]int64
	if latest != nil {
		ranges = latest.Ranges
	}
	missing := missingUpTo(ranges, last)
	if missing > 0 {
		if latest == nil {
			return nil // nothing arrived yet; the first number opens the register series and the gaps with it
		}
		hs := holes(ranges)
		if top := latest.Ranges[len(latest.Ranges)-1][1]; last > top {
			hs = append(hs, [2]int64{top + 1, last})
		}
		ac := &applyCtx{s: s, tx: tx, outlet: si.OutletID, op: Op{Actor: "billing"}}
		return ac.syncGaps(ctx, *latest, hs, "device", "billing")
	}
	if err := s.publish(ctx, tx, si.OutletID, "billing.series.reconciled", si.SeriesID, "", map[string]any{"series_id": si.SeriesID, "last_number": last, "gap_count": 0}); err != nil {
		return err
	}
	return modify(ctx, tx, repository.Doc{Kind: KindSeriesIssued, ID: si.SeriesID}, func(r *SeriesIssued) { r.Reconciled = true })
}

// seriesLastSeen is the answer of GetSeriesLastSeen for a series no number has reached yet.
func (s *Service) emptyLastSeen(seriesID string) gen.SeriesLastSeen {
	return gen.SeriesLastSeen{SeriesId: seriesID, Count: 0, AsOf: tsOr(nil, s.clock.Now())}
}

// linkAndReconcile runs after a number reached the register: the register series of the code takes its tenancy series id,
// and a closed series whose device stated its last number is reconciled when this number completed it.
func (ac *applyCtx) linkAndReconcile(ctx context.Context, reg, code string) error {
	docs, err := ac.tx.FindDocs(ctx, repository.Query{Kind: KindSeriesIssued, K1: seriesRef(reg, code)})
	if err != nil {
		return err
	}
	for _, d := range docs {
		si, err := unmarshalDoc[SeriesIssued](d)
		if err != nil {
			return err
		}
		if err := ac.s.linkRegister(ctx, ac.tx, si); err != nil {
			return err
		}
		if err := ac.s.reconcile(ctx, ac.tx, si); err != nil {
			return err
		}
	}
	return nil
}
