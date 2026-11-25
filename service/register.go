package service

import (
	"context"
	"sort"

	"github.com/hostryhq/svc-billing/internal/gen"
	"github.com/hostryhq/svc-billing/internal/tstamp"
	"github.com/hostryhq/svc-billing/repository"
)

// The document register (ADR-0014 R5) holds every number seen per registration, financial year and series with its
// state. billing never allocates, changes or reuses a number: it only records what arrived and states what is missing.

func seriesKey(reg, code, fy string) string { return reg + "|" + code + "|" + fy }

// insertNumber adds n to the sorted, merged intervals and reports whether it was new.
func insertNumber(r [][2]int64, n int64) ([][2]int64, bool) {
	for _, iv := range r {
		if n >= iv[0] && n <= iv[1] {
			return r, false
		}
	}
	r = append(r, [2]int64{n, n})
	sort.Slice(r, func(i, j int) bool { return r[i][0] < r[j][0] })
	out := r[:0:0]
	for _, iv := range r {
		if len(out) > 0 && iv[0] <= out[len(out)-1][1]+1 {
			if iv[1] > out[len(out)-1][1] {
				out[len(out)-1][1] = iv[1]
			}
			continue
		}
		out = append(out, iv)
	}
	return out, true
}

// holes returns the missing runs between 1 and the highest number seen.
func holes(r [][2]int64) [][2]int64 {
	var out [][2]int64
	next := int64(1)
	for _, iv := range r {
		if iv[0] > next {
			out = append(out, [2]int64{next, iv[0] - 1})
		}
		next = iv[1] + 1
	}
	return out
}

func count(r [][2]int64) int {
	var n int64
	for _, iv := range r {
		n += iv[1] - iv[0] + 1
	}
	return int(n)
}

// register records a document number in the register of its series and detects gaps.
func (ac *applyCtx) register(ctx context.Context, code, fy, number, docType, sourceKind, device string) error {
	c, f, running, ok := splitNumber(number)
	if !ok {
		return nil // a number that is not <code>/<FY>/<running> is the gateway's to quarantine, not the register's
	}
	if code == "" {
		code = c
	}
	if fy == "" {
		fy = f
	}
	reg := NoRegistration
	if o, err := load[OutletRec](ctx, ac.tx, repository.KindOutlet, ac.outlet); err == nil && o.RegistrationID != "" {
		reg = o.RegistrationID
	}
	kind := gen.RegisterSeriesKind("device")
	gapKind := "device"
	if sourceKind == "manual_book" {
		kind, gapKind = "manual_book", "manual_book"
	}
	id := seriesKey(reg, code, fy)
	now := tstamp.Of(ac.s.clock.Now())
	var rec SeriesRec
	var h [][2]int64
	err := modify(ctx, ac.tx, repository.Doc{Kind: repository.KindSeries, ID: id, Sort: code + "|" + fy, K1: string(kind)}, func(r *SeriesRec) {
		if r.Series.SeriesCode == "" {
			r.Series = gen.RegisterSeries{TaxRegistrationId: reg, SeriesCode: code, Kind: kind, DocType: gen.RegisterSeriesDocType(docType), FinancialYear: fy, State: "active"}
			if kind == "device" {
				r.Series.DeviceId = &device
			}
		}
		var added bool
		r.Ranges, added = insertNumber(r.Ranges, running)
		if added {
			r.Series.LastSeenAt = &now
		}
		r.Series.Count = count(r.Ranges)
		if len(r.Ranges) > 0 {
			r.Series.FirstNumber = ptr(formatNumber(code, fy, r.Ranges[0][0]))
			r.Series.LastNumber = ptr(formatNumber(code, fy, r.Ranges[len(r.Ranges)-1][1]))
		}
		hs := holes(r.Ranges)
		r.Series.GapCount = len(hs)
		rec, h = *r, hs
	})
	if err != nil {
		return err
	}
	if err := ac.syncGaps(ctx, rec, h, gapKind, ac.op.Actor); err != nil {
		return err
	}
	return ac.linkAndReconcile(ctx, reg, code)
}

// syncGaps makes the gap documents of a series equal the current holes: a hole that is new raises the alert, a hole
// that was filled becomes "arrived". A gap is never a silent state (R5).
func (ac *applyCtx) syncGaps(ctx context.Context, rec SeriesRec, hs [][2]int64, kind, actor string) error {
	s := rec.Series
	key := seriesKey(s.TaxRegistrationId, s.SeriesCode, s.FinancialYear)
	gaps, docs, err := loadAll[gen.RegisterGap](ctx, ac.tx, repository.Query{Kind: repository.KindGap, K1: key})
	if err != nil {
		return err
	}
	covered := func(h [2]int64) bool {
		for _, g := range gaps {
			if int64(g.MissingFrom) <= h[0] && h[1] <= int64(g.MissingTo) {
				return true
			}
		}
		return false
	}
	still := func(g gen.RegisterGap) bool {
		for _, h := range hs {
			if int64(g.MissingFrom) <= h[1] && h[0] <= int64(g.MissingTo) {
				return true
			}
		}
		return false
	}
	for i, g := range gaps {
		if g.Status == "open" && !still(g) {
			g.Status = "arrived"
			docs[i].Body = marshalDoc(g)
			docs[i].K2 = "arrived"
			if err := ac.tx.SaveDoc(ctx, docs[i]); err != nil {
				return err
			}
		}
	}
	now := tstamp.Of(ac.s.clock.Now())
	for _, h := range hs {
		if covered(h) {
			continue
		}
		g := gen.RegisterGap{TaxRegistrationId: s.TaxRegistrationId, SeriesCode: s.SeriesCode, FinancialYear: s.FinancialYear, Kind: gen.RegisterGapKind(kind),
			MissingFrom: int(h[0]), MissingTo: int(h[1]), DetectedAt: now, Status: "open"}
		gid := uuid5("gap", key+"|"+itoa(int(h[0]))+"|"+itoa(int(h[1])))
		if _, err := ac.tx.PutDoc(ctx, repository.Doc{Kind: repository.KindGap, ID: gid, Date: "", Sort: now.String(), K1: key, K2: "open", Body: marshalDoc(g)}); err != nil {
			return err
		}
		ek := "document_gap"
		if kind == "manual_book" {
			ek = "manual_book_missing_number"
		}
		if err := ac.exception(ctx, ek, gen.High, "series", s.SeriesCode+"|"+itoa(int(h[0])), "", map[string]any{"missing_from": h[0], "missing_to": h[1]}); err != nil {
			return err
		}
		if err := ac.s.publish(ctx, ac.tx, ac.outlet, "billing.document_gap.detected", s.TaxRegistrationId+":"+s.SeriesCode+":"+s.FinancialYear, actor, map[string]any{
			"tax_registration_id": s.TaxRegistrationId, "series_code": s.SeriesCode, "financial_year": s.FinancialYear, "kind": kind, "missing_from": h[0], "missing_to": h[1]}); err != nil {
			return err
		}
	}
	return nil
}
