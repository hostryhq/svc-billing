package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-billing/internal/gen"
	"github.com/hostryhq/svc-billing/internal/tstamp"
	"github.com/hostryhq/svc-billing/repository"
)

// Project applies one op in its own transaction (the double and the tests; the bus consumer calls ApplyOp inside the
// inbox transaction).
func (s *Service) Project(ctx context.Context, op Op) error {
	return s.repo.InTx(ctx, func(ctx context.Context, tx repository.Tx) error { return s.ApplyOp(ctx, tx, op) })
}

// ownedEntities are the synced entities billing owns (entities/README.md ownership table). Mutations of any other
// entity in the same op are ignored.
var ownedEntities = map[string]bool{
	"bill": true, "bill_line": true, "bill_tax_line": true, "credit_note": true, "credit_note_line": true, "credit_note_tax_line": true,
	"payment": true, "refund": true, "cash_event": true, "shift": true, "shift_close": true, "business_day": true, "day_open": true,
	"day_close": true, "business_day_revision": true, "outlet_tax_profile": true, "billing_settings": true, "tender_kind": true,
	"internal_consumption": true, "tax_rule_set": true, "tax_rule": true,
}

// Owns reports whether any mutation of the op touches an entity billing owns.
func Owns(op Op) bool {
	for _, m := range op.Muts {
		if ownedEntities[m.E] {
			return true
		}
	}
	return false
}

type applyCtx struct {
	s      *Service
	tx     repository.Tx
	op     Op
	outlet string
	tenant ids.ID
	g      map[string][]Mut
	reason string // why the last recompute could not run (engine error code)
}

// ApplyOp projects the billing-owned mutations of one accepted op, idempotently: an op is identified by
// (origin, stream, seq), so a replay (redelivery, restore, bus reset) changes nothing (ADR-0011 E3).
func (s *Service) ApplyOp(ctx context.Context, tx repository.Tx, op Op) error {
	if !Owns(op) {
		return nil
	}
	first, err := tx.MarkOp(ctx, op.Origin, op.Stream, op.Seq, op.Hash)
	if err != nil || !first {
		return err
	}
	t, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	ac := &applyCtx{s: s, tx: tx, op: op, outlet: op.OutletOf(), tenant: t, g: byEntity(op.Muts)}
	for _, step := range []func(context.Context) error{
		ac.config, ac.ruleSets, ac.dayOpen, ac.shifts, ac.bills, ac.creditNotes, ac.payments, ac.refunds, ac.internal, ac.dayClose,
	} {
		if err := step(ctx); err != nil {
			return fmt.Errorf("apply %s/%s/%d: %w", op.Origin, op.Stream, op.Seq, err)
		}
	}
	return nil
}

// ---- lateness ----

// lateDay reports whether a document of this op with the business date falls on a day that was closed with a cutoff
// that did not cover it (A1.1 item 5), and returns that day.
func (ac *applyCtx) lateDay(ctx context.Context, date string) (gen.BusinessDay, bool, error) {
	var d gen.BusinessDay
	doc, err := ac.tx.GetDoc(ctx, repository.KindBusinessDay, dayID(ac.outlet, date))
	if err == repository.ErrNotFound {
		return d, false, nil
	}
	if err != nil {
		return d, false, err
	}
	if d, err = unmarshalDoc[gen.BusinessDay](doc); err != nil {
		return d, false, err
	}
	if d.Status != gen.BusinessDayStatusClosed || d.Close == nil {
		return d, false, nil
	}
	var cut map[string]int64
	if json.Unmarshal([]byte(d.Close.Cutoff), &cut) != nil {
		return d, false, nil
	}
	return d, ac.op.Seq > cut[ac.op.Origin], nil
}

// addLate adds a late document's contribution to the origin's pending revision batch and counts it on the day.
func (ac *applyCtx) addLate(ctx context.Context, day gen.BusinessDay, contrib gen.DayTotals, ranges []gen.SeriesRange, kind, subject string) error {
	id := day.Id + "|" + ac.op.Origin
	now := nowUTC(ac.s)
	if err := modify(ctx, ac.tx, repository.Doc{Kind: repository.KindLatePending, ID: id, Outlet: ac.outlet, Date: day.BusinessDate.String(), K1: day.Id, K2: ac.op.Origin},
		func(p *LatePending) {
			if p.DayID == "" {
				p.DayID, p.Origin, p.Cause, p.FromSeq, p.Totals = day.Id, ac.op.Origin, "late_device", ac.op.Seq, emptyTotals()
			}
			if ac.op.Seq > p.ToSeq {
				p.ToSeq = ac.op.Seq
			}
			if ac.op.Seq < p.FromSeq {
				p.FromSeq = ac.op.Seq
			}
			p.LastAt = now.Format(tstamp.Layout)
			p.Totals = addTotals(p.Totals, contrib)
			p.Series = mergeRanges(p.Series, ranges)
		}); err != nil {
		return err
	}
	if err := modify(ctx, ac.tx, repository.Doc{Kind: repository.KindBusinessDay, ID: day.Id, Outlet: ac.outlet, Date: day.BusinessDate.String(), Sort: day.BusinessDate.String()},
		func(d *gen.BusinessDay) { *d = day; d.LateDocumentCount++ }); err != nil {
		return err
	}
	return ac.exception(ctx, "late_document", gen.Info, kind, subject, day.BusinessDate.String(), map[string]any{"revision_origin": ac.op.Origin})
}

// ---- exceptions ----

// exception records a detector finding once per (kind, subject).
func (ac *applyCtx) exception(ctx context.Context, kind string, sev gen.ExceptionSeverity, subjectKind, subjectID, date string, params map[string]any) error {
	id := exceptionID(kind, subjectID, params)
	ex := gen.Exception{Id: id, Kind: kind, Severity: sev, Status: gen.ExceptionStatusOpen, SubjectKind: subjectKind, SubjectId: subjectID,
		OutletId: ac.outlet, DetectedAt: tstamp.Of(ac.s.clock.Now()), DetailKey: "exception." + kind}
	if date != "" {
		var d gen.BusinessDay
		_ = d
		ex.BusinessDate = parseDate(date)
	}
	if len(params) > 0 {
		p := params
		ex.Params = &p
	}
	_, err := ac.tx.PutDoc(ctx, repository.Doc{Kind: repository.KindException, ID: id, Outlet: ac.outlet, Date: date, Sort: ex.DetectedAt.String(), K1: kind, K2: "open", Body: marshalDoc(ex)})
	return err
}

// exceptionID is deterministic so a replay never duplicates a finding.
func exceptionID(kind, subject string, params map[string]any) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	name := kind + "|" + subject
	return uuid5("exception", name)
}

// resolveException closes an open finding when the resolving fact arrived.
func (ac *applyCtx) resolveException(ctx context.Context, kind, subjectID, ref string) error {
	id := exceptionID(kind, subjectID, nil)
	d, err := ac.tx.GetDoc(ctx, repository.KindException, id)
	if err == repository.ErrNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	ex, err := unmarshalDoc[gen.Exception](d)
	if err != nil || ex.Status != gen.ExceptionStatusOpen {
		return err
	}
	ex.Status = gen.ExceptionStatusResolved
	ex.ResolvedAt = tstamp.Ptr(ac.s.clock.Now())
	ex.ResolvedByRef = &ref
	d.K2, d.Body = "resolved", marshalDoc(ex)
	return ac.tx.SaveDoc(ctx, d)
}
