package service

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/hostryhq/go-common/emit"
	gsync "github.com/hostryhq/go-common/sync"

	"github.com/hostryhq/svc-billing/internal/gen"
	"github.com/hostryhq/svc-billing/internal/tstamp"
	"github.com/hostryhq/svc-billing/repository"
)

func dayDoc(outlet string, d gen.BusinessDay) repository.Doc {
	return repository.Doc{Kind: repository.KindBusinessDay, ID: d.Id, Outlet: outlet, Date: d.BusinessDate.String(), Sort: d.BusinessDate.String(), K1: string(d.Status), Body: marshalDoc(d)}
}

// getOrNewDay returns the day row, creating the open shell when a close or a document arrives before the open fact.
func (ac *applyCtx) getOrNewDay(ctx context.Context, date string) (gen.BusinessDay, error) {
	id := dayID(ac.outlet, date)
	d, err := load[gen.BusinessDay](ctx, ac.tx, repository.KindBusinessDay, id)
	if err == repository.ErrNotFound {
		return gen.BusinessDay{Id: id, OutletId: ac.outlet, BusinessDate: mustDate(date), Status: gen.BusinessDayStatusOpen}, nil
	}
	return d, err
}

// dayOpen applies business_day and day_open: one row per (outlet, date); who opened is an append fact.
func (ac *applyCtx) dayOpen(ctx context.Context) error {
	for _, m := range ac.g["business_day"] {
		date := fstr(m.F, "business_date")
		if date == "" {
			continue
		}
		d, err := ac.getOrNewDay(ctx, date)
		if err != nil {
			return err
		}
		if fstr(m.F, "status") == "closed" {
			d.Status = gen.BusinessDayStatusClosed // rank: closed outranks open
		}
		if err := ac.tx.SaveDoc(ctx, dayDoc(ac.outlet, d)); err != nil {
			return err
		}
	}
	for _, m := range ac.g["day_open"] {
		date := fstr(m.F, "business_date")
		d, err := ac.getOrNewDay(ctx, date)
		if err != nil {
			return err
		}
		fresh := d.OpenedAt == nil
		if fresh {
			d.OpenedAt = tsPtr(ac.at())
		}
		if err := ac.tx.SaveDoc(ctx, dayDoc(ac.outlet, d)); err != nil {
			return err
		}
		if !fresh || d.Status != gen.BusinessDayStatusOpen {
			continue
		}
		open, _, err := loadAll[gen.BusinessDay](ctx, ac.tx, repository.Query{Kind: repository.KindBusinessDay, Outlets: []string{ac.outlet}, K1: "open"})
		if err != nil {
			return err
		}
		if len(open) > 1 {
			if err := ac.exception(ctx, "two_open_days", gen.Warning, "business_day", d.Id, date, nil); err != nil {
				return err
			}
		}
		if err := ac.tradingDay(ctx, date, "open"); err != nil {
			return err
		}
	}
	return nil
}

func (ac *applyCtx) tradingDay(ctx context.Context, date, state string) error {
	return ac.s.publish(ctx, ac.tx, ac.outlet, "billing.trading_day.changed", ac.outlet+":"+date, ac.op.Actor, map[string]any{
		"outlet_id": ac.outlet, "business_date": date, "state": state, "changed_at": ac.at().String()})
}

type tenderJSON struct {
	Tender      string `json:"tender"`
	Name        string `json:"name"`
	Count       int    `json:"count"`
	AmountPaise int64  `json:"amount_paise"`
}

type rangeJSON struct {
	SeriesCode  string `json:"series_code"`
	DocType     string `json:"doc_type"`
	FirstNumber string `json:"first_number"`
	LastNumber  string `json:"last_number"`
	Count       int    `json:"count"`
}

// dayClose applies the close fact: the lowest stamp is canonical; a second close stays a fact and is covered by a
// revision (A1.1 item 4).
func (ac *applyCtx) dayClose(ctx context.Context) error {
	for _, m := range ac.g["day_close"] {
		f := m.F
		var date string
		d0, err := load[gen.BusinessDay](ctx, ac.tx, repository.KindBusinessDay, fstr(f, "business_day_id"))
		switch {
		case err == nil:
			date = d0.BusinessDate.String()
		case err == repository.ErrNotFound:
			// the day row has not arrived; the z number carries the date: Z/<date>/<device>
			if parts := strings.Split(fstr(f, "z_number"), "/"); len(parts) >= 2 {
				date = parts[1]
			}
		default:
			return err
		}
		if date == "" {
			continue
		}
		d, err := ac.getOrNewDay(ctx, date)
		if err != nil {
			return err
		}
		t := emptyTotals()
		t.Gross, t.Discounts, t.Net, t.ServiceCharge = money(fint(f, "gross_paise")), money(fint(f, "discount_paise")), money(fint(f, "net_paise")), money(fint(f, "service_charge_paise"))
		t.Cgst, t.Sgst, t.NonGst, t.RoundOff, t.Total = money(fint(f, "cgst_paise")), money(fint(f, "sgst_paise")), money(fint(f, "non_gst_paise")), money(fint(f, "round_off_paise")), money(fint(f, "total_paise"))
		t.BillCount, t.CreditNoteCount, t.CreditNoteTotal = int(fint(f, "bill_count")), int(fint(f, "credit_note_count")), money(fint(f, "credit_note_total_paise"))
		t.RefundTotal, t.InternalConsumption = money(fint(f, "refund_total_paise")), money(fint(f, "internal_consumption_paise"))
		var tj []tenderJSON
		_ = json.Unmarshal([]byte(fstr(f, "tenders_json")), &tj)
		for _, x := range tj {
			tt := gen.TenderTotal{Tender: gen.TenderTotalTender(x.Tender), Count: x.Count, Amount: money(x.AmountPaise)}
			if x.Name != "" {
				tt.Name = ptr(x.Name)
			}
			t.Tenders = append(t.Tenders, tt)
		}
		var rj []rangeJSON
		_ = json.Unmarshal([]byte(fstr(f, "series_ranges_json")), &rj)
		sr := []gen.SeriesRange{}
		for _, x := range rj {
			sr = append(sr, gen.SeriesRange{SeriesCode: x.SeriesCode, DocType: gen.SeriesRangeDocType(x.DocType), FirstNumber: x.FirstNumber, LastNumber: x.LastNumber, Count: x.Count})
		}
		cl := gen.DayClose{ZNumber: fstr(f, "z_number"), ClosingDeviceId: ac.op.Origin, ClosingMemberId: ac.op.Actor, ClosedAt: ac.at(), Cutoff: fstr(f, "cutoff"), Totals: t, SeriesRanges: sr}
		stamp := ac.op.stamp()
		doc, gerr := ac.tx.GetDoc(ctx, repository.KindBusinessDay, d.Id)
		switch {
		case d.Close == nil || (gerr == nil && stamp < doc.Stamp):
			if d.Close != nil {
				if err := ac.exception(ctx, "late_document", gen.Info, "business_day", d.Id+"|second_close", date, map[string]any{"cause": "second_close"}); err != nil {
					return err
				}
			}
			d.Status, d.Close = gen.BusinessDayStatusClosed, &cl
			dd := dayDoc(ac.outlet, d)
			dd.Stamp = stamp
			if err := ac.tx.SaveDoc(ctx, dd); err != nil {
				return err
			}
			if err := ac.tradingDay(ctx, date, "closed"); err != nil {
				return err
			}
		default:
			// a second close with a later stamp: kept as a fact only, its difference is a revision (second_close)
			if err := ac.exception(ctx, "late_document", gen.Info, "business_day", d.Id+"|second_close|"+ac.op.Origin, date, map[string]any{"cause": "second_close"}); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---- shifts ----

func (ac *applyCtx) shifts(ctx context.Context) error {
	for _, m := range ac.g["shift"] {
		f := m.F
		date := fstr(f, "business_date")
		err := modify(ctx, ac.tx, repository.Doc{Kind: repository.KindShift, ID: m.ID, Outlet: ac.outlet, Date: date, Sort: ac.at().String()}, func(sh *gen.Shift) {
			if sh.Id == "" {
				sh.Id, sh.OutletId, sh.Status = m.ID, ac.outlet, gen.ShiftStatusOpen
			}
			if fhas(f, "opening_float_paise") || sh.OpenedAt.IsZero() {
				sh.DeviceId, sh.MemberId, sh.BusinessDate, sh.OpenedAt, sh.OpeningFloat = ac.op.Origin, ac.op.Actor, mustDate(date), ac.at(), money(fint(f, "opening_float_paise"))
			}
			if fstr(f, "status") == "closed" {
				sh.Status = gen.ShiftStatusClosed
			}
		})
		if err != nil {
			return err
		}
	}
	for _, m := range ac.g["shift_close"] {
		f := m.F
		sid := fstr(f, "shift_id")
		err := modify(ctx, ac.tx, repository.Doc{Kind: repository.KindShift, ID: sid, Outlet: ac.outlet, Sort: ac.at().String()}, func(sh *gen.Shift) {
			if sh.Id == "" {
				sh.Id, sh.OutletId, sh.DeviceId, sh.MemberId, sh.OpenedAt, sh.OpeningFloat = sid, ac.outlet, ac.op.Origin, ac.op.Actor, ac.at(), money(0)
			}
			sh.Status, sh.ClosedAt = gen.ShiftStatusClosed, tsPtr(ac.at())
			sh.Expected = ptr(money(fint(f, "expected_paise")))
			if fhas(f, "counted_paise") {
				sh.Counted = ptr(money(fint(f, "counted_paise")))
				sh.Variance = ptr(money(fint(f, "variance_paise")))
			}
			sh.ForceClosed, sh.HandoverCounted = ptr(fbool(f, "force_closed")), ptr(fbool(f, "handover_counted"))
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// ---- internal consumption (O2-1, provisional) ----

func (ac *applyCtx) internal(ctx context.Context) error {
	for _, m := range ac.g["internal_consumption"] {
		f := m.F
		ic := gen.InternalConsumption{Id: m.ID, OutletId: ac.outlet, CheckId: fstr(f, "check_id"), Ref: fstr(f, "ref"), Kind: gen.InternalConsumptionKind(fstr(f, "kind")),
			ReasonCodeId: fstr(f, "reason_code_id"), ListValue: money(fint(f, "list_value_paise")), MemberId: ac.op.Actor, DeviceId: ac.op.Origin,
			BusinessDate: mustDate(fstr(f, "business_date")), ClosedAt: ac.at()}
		created, err := ac.tx.PutDoc(ctx, repository.Doc{Kind: repository.KindInternal, ID: m.ID, Outlet: ac.outlet, Date: ic.BusinessDate.String(), Sort: ac.at().String(), Body: marshalDoc(ic)})
		if err != nil || !created {
			return err
		}
		// Not sales and not tax: it shows on the Z-report as its own line only.
		c := emptyTotals()
		c.InternalConsumption = ic.ListValue
		if err := ac.aggDay(ctx, ic.BusinessDate.String(), c); err != nil {
			return err
		}
		if err := ac.discountRow(ctx, "internal_closure", ic.Id, "", ic.ReasonCodeId, ic.ListValue.AmountPaise, ic.BusinessDate.String()); err != nil {
			return err
		}
	}
	return nil
}

// ---- revisions ----

// FlushRevisions seals every batch of late documents that has been quiet for the revision window into one immutable
// Z revision per origin device (A1.1 item 6), and queues its emission. billing is the only writer, so a printed
// revision number never changes. It is run on a timer by the process and directly by tests.
func (s *Service) FlushRevisions(ctx context.Context) (int, error) {
	n := 0
	err := s.repo.InTx(ctx, func(ctx context.Context, tx repository.Tx) error {
		pend, docs, err := loadAll[LatePending](ctx, tx, repository.Query{Kind: repository.KindLatePending})
		if err != nil {
			return err
		}
		now := s.clock.Now()
		for i, p := range pend {
			last, err := tstampParse(p.LastAt)
			if err != nil || now.Sub(last) < s.window {
				continue
			}
			day, err := load[gen.BusinessDay](ctx, tx, repository.KindBusinessDay, p.DayID)
			if err != nil {
				return err
			}
			revs, _, err := loadAll[gen.BusinessDayRevision](ctx, tx, repository.Query{Kind: repository.KindRevision, K1: p.DayID})
			if err != nil {
				return err
			}
			num := len(revs) + 1
			z := ""
			if day.Close != nil {
				z = day.Close.ZNumber
			}
			rev := gen.BusinessDayRevision{Id: s.mintID(), BusinessDayId: p.DayID, Revision: num, DisplayNumber: z + "/R" + itoa(num), Cause: gen.BusinessDayRevisionCause(p.Cause),
				OriginDeviceId: p.Origin, OpFromSeq: p.FromSeq, OpToSeq: p.ToSeq, AddedTotals: p.Totals, AddedSeriesRanges: p.Series, CreatedAt: tstamp.Of(now)}
			if rev.AddedSeriesRanges == nil {
				rev.AddedSeriesRanges = []gen.SeriesRange{}
			}
			if _, err := tx.PutDoc(ctx, repository.Doc{Kind: repository.KindRevision, ID: rev.Id, Outlet: day.OutletId, Date: day.BusinessDate.String(), Sort: itoa(1000000 + num), K1: p.DayID, Body: marshalDoc(rev)}); err != nil {
				return err
			}
			// the revision reaches the outlet's devices as a cloud-origin op, queued in this transaction (A1.1 item 6, A2.2)
			if err := tx.EnqueueEmit(ctx, now, revisionOp(day, rev)); err != nil {
				return err
			}
			day.RevisionCount++
			if err := tx.SaveDoc(ctx, dayDoc(day.OutletId, day)); err != nil {
				return err
			}
			if err := tx.DeleteDoc(ctx, repository.KindLatePending, docs[i].ID); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}

// revisionOp is the day.revision.record op of one sealed revision. Its key is the UUIDv5 of the day and the revision
// number, so a retried or restored emit is one op; billing is the only writer, a system emit with no user (cloud.yaml).
func revisionOp(day gen.BusinessDay, r gen.BusinessDayRevision) emit.Op {
	t := r.AddedTotals
	var tj []tenderJSON
	for _, x := range t.Tenders {
		tj = append(tj, tenderJSON{Tender: string(x.Tender), Name: deref(x.Name), Count: x.Count, AmountPaise: x.Amount.AmountPaise})
	}
	var rj []rangeJSON
	for _, x := range r.AddedSeriesRanges {
		rj = append(rj, rangeJSON{SeriesCode: x.SeriesCode, DocType: string(x.DocType), FirstNumber: x.FirstNumber, LastNumber: x.LastNumber, Count: x.Count})
	}
	tb, _ := json.Marshal(tj)
	rb, _ := json.Marshal(rj)
	f := map[string]any{"business_day_id": r.BusinessDayId, "revision": int64(r.Revision), "cause": string(r.Cause), "origin_device_id": r.OriginDeviceId,
		"op_from_seq": r.OpFromSeq, "op_to_seq": r.OpToSeq, "display_number": r.DisplayNumber,
		"gross_paise": t.Gross.AmountPaise, "discount_paise": t.Discounts.AmountPaise, "net_paise": t.Net.AmountPaise, "service_charge_paise": t.ServiceCharge.AmountPaise,
		"cgst_paise": t.Cgst.AmountPaise, "sgst_paise": t.Sgst.AmountPaise, "non_gst_paise": t.NonGst.AmountPaise, "round_off_paise": t.RoundOff.AmountPaise,
		"total_paise": t.Total.AmountPaise, "bill_count": int64(t.BillCount), "credit_note_count": int64(t.CreditNoteCount), "credit_note_total_paise": t.CreditNoteTotal.AmountPaise,
		"refund_total_paise": t.RefundTotal.AmountPaise, "internal_consumption_paise": t.InternalConsumption.AmountPaise, "tenders_json": string(tb), "series_ranges_json": string(rb)}
	return emit.Op{Key: emit.DeterministicKey("day.revision.record", r.BusinessDayId, itoa(r.Revision)), Stream: "outlet:" + day.OutletId, Intent: "day.revision.record", IntentV: 1,
		Muts: []Mut{{E: "business_day_revision", ID: r.Id, K: gsync.KindInsert, F: f}}, Actor: "service:" + Name}
}

func tstampParse(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }
