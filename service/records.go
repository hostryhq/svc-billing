package service

import (
	"context"
	"encoding/json"

	"github.com/hostryhq/svc-billing/internal/gen"
	"github.com/hostryhq/svc-billing/repository"
)

// The projection stores the API representation of each read model, plus the few inputs the API does not show but the
// recompute needs. A stored document is rebuilt by replaying ops; it never holds a rule of its own.

// NoReason is the reason-code id of a discount row whose source fact carries none (bill discounts have no reason
// field in the synced entity).
const NoReason = "00000000-0000-7000-8000-000000000000"

// NoRegistration is the tax registration id used until tenancy has told billing which registration an outlet bills under.
const NoRegistration = "00000000-0000-7000-8000-000000000001"

// BillRec is the stored bill.
type BillRec struct {
	Detail gen.BillDetail `json:"detail"`
	Calc   BillCalc       `json:"calc"`
}

// BillCalc holds what the bill fact states about its own calculation: the recompute compares against it.
type BillCalc struct {
	SeriesCode      string `json:"series_code"`
	FinancialYear   string `json:"financial_year"`
	DiscountKind    string `json:"discount_kind,omitempty"`
	DiscountValue   int64  `json:"discount_value,omitempty"`
	ServiceChargeBP int64  `json:"service_charge_bp"`
	GrossPaise      int64  `json:"gross_paise"`
	DiscountPaise   int64  `json:"discount_paise"`
	TaxablePaise    int64  `json:"taxable_paise"`
	CGSTPaise       int64  `json:"cgst_paise"`
	SGSTPaise       int64  `json:"sgst_paise"`
	NonGSTPaise     int64  `json:"non_gst_paise"`
	ServiceCharge   int64  `json:"service_charge_paise"`
	PackingPaise    int64  `json:"packing_paise"`
	RoundOffPaise   int64  `json:"round_off_paise"`
	TotalPaise      int64  `json:"total_paise"`
	BookTotalPaise  int64  `json:"book_total_paise,omitempty"`
	LineIDs         []string
	OpOrigin        string `json:"op_origin"`
	OpSeq           int64  `json:"op_seq"`
	// RebillGroupID names the re-bill this bill replaces another bill in ("" for any other bill).
	RebillGroupID string `json:"rebill_group_id,omitempty"`
}

// CreditRec is the stored credit note.
type CreditRec struct {
	Note gen.CreditNote `json:"note"`
	Calc CreditCalc     `json:"calc"`
}

// CreditCalc holds the credit note's own statement of its calculation.
type CreditCalc struct {
	SeriesCode    string       `json:"series_code"`
	FinancialYear string       `json:"financial_year"`
	Credited      []CreditLine `json:"credited"`
	TaxablePaise  int64        `json:"taxable_paise"`
	CGSTPaise     int64        `json:"cgst_paise"`
	SGSTPaise     int64        `json:"sgst_paise"`
	TotalPaise    int64        `json:"total_paise"`
	OpOrigin      string       `json:"op_origin"`
	OpSeq         int64        `json:"op_seq"`
}

// CreditLine is a credited quantity of an original bill line.
type CreditLine struct {
	BillLineID string `json:"bill_line_id"`
	LineNo     int    `json:"line_no"`
	QtyMilli   int64  `json:"qty_milli"`
	NetPaise   int64  `json:"net_paise"`
}

// SeriesRec is a series of the document register: the API row plus the set of numbers seen, as merged intervals.
type SeriesRec struct {
	Series gen.RegisterSeries `json:"series"`
	Ranges [][2]int64         `json:"ranges"`
	// SeriesID is the tenancy registry id of the series, when tenancy told billing (GET last-seen).
	SeriesID string `json:"series_id,omitempty"`
}

// OutletRec is what billing knows of an outlet from tenancy.
type OutletRec struct {
	OutletID       string `json:"outlet_id"`
	RegistrationID string `json:"tax_registration_id"`
	Regime         string `json:"regime"` // regular, composition, unregistered
	// EffectiveFrom is the IST date from which a changed registration or regime applies; "" means the day billing is told.
	EffectiveFrom string `json:"effective_from,omitempty"`
	// Eras is the history: the registration and regime in force from each date, oldest first (M3).
	Eras []OutletEra `json:"eras,omitempty"`
}

// OutletEra is the registration and regime an outlet had from a date.
type OutletEra struct {
	From           string `json:"from"` // IST date, "" for the first era
	RegistrationID string `json:"tax_registration_id"`
	Regime         string `json:"regime"`
}

// At is the registration and regime in force on the date (an IST date; "" means the current one).
func (o OutletRec) At(date string) OutletEra {
	cur := OutletEra{RegistrationID: o.RegistrationID, Regime: o.Regime}
	if date == "" || len(o.Eras) == 0 {
		return cur
	}
	found := false
	for _, e := range o.Eras {
		if e.From <= date {
			cur, found = e, true
		}
	}
	if !found {
		cur = o.Eras[0]
	}
	return cur
}

// RuleSetRec is a tax rule set as received by this organisation (the tax_rule_set row with its rules).
type RuleSetRec struct {
	ID            string        `json:"id"`
	Version       int           `json:"version"`
	Name          string        `json:"name"`
	EffectiveDate string        `json:"effective_date"`
	CalcV         int           `json:"calc_v"`
	ReviewStatus  string        `json:"review_status"`
	ContentHash   string        `json:"content_hash"`
	PublishedAt   string        `json:"published_at"`
	Rules         []gen.TaxRule `json:"rules"`
}

// LatePending accumulates the late documents of one origin device for one business day until the batch is quiet and
// is sealed into an immutable revision.
type LatePending struct {
	DayID   string            `json:"day_id"`
	Origin  string            `json:"origin"`
	Cause   string            `json:"cause"`
	FromSeq int64             `json:"from_seq"`
	ToSeq   int64             `json:"to_seq"`
	LastAt  string            `json:"last_at"`
	Totals  gen.DayTotals     `json:"totals"`
	Series  []gen.SeriesRange `json:"series"`
}

// ---- document helpers ----

func marshalDoc(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // plain structs of the generated types cannot fail to marshal
	}
	return b
}

func unmarshalDoc[T any](d repository.Doc) (T, error) {
	var v T
	err := json.Unmarshal(d.Body, &v)
	return v, err
}

// load reads one document into T.
func load[T any](ctx context.Context, tx repository.Tx, kind, id string) (T, error) {
	var zero T
	d, err := tx.GetDoc(ctx, kind, id)
	if err != nil {
		return zero, err
	}
	return unmarshalDoc[T](d)
}

// loadAll reads every document of a query into []T.
func loadAll[T any](ctx context.Context, tx repository.Tx, q repository.Query) ([]T, []repository.Doc, error) {
	docs, err := tx.FindDocs(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	out := make([]T, 0, len(docs))
	for _, d := range docs {
		v, err := unmarshalDoc[T](d)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, v)
	}
	return out, docs, nil
}

// modify reads the document (kind, id) into T (zero value when absent), applies fn and saves it.
func modify[T any](ctx context.Context, tx repository.Tx, base repository.Doc, fn func(*T)) error {
	var v T
	d, err := tx.GetDoc(ctx, base.Kind, base.ID)
	switch {
	case err == nil:
		if v, err = unmarshalDoc[T](d); err != nil {
			return err
		}
		base = d
	case err == repository.ErrNotFound:
	default:
		return err
	}
	fn(&v)
	base.Body = marshalDoc(v)
	return tx.SaveDoc(ctx, base)
}

// ---- money helpers ----

func money(n int64) gen.Money { return gen.Money{AmountPaise: n, Currency: gen.INR} }

func addTo(m *gen.Money, n int64) { m.AmountPaise += n; m.Currency = gen.INR }

func emptyTotals() gen.DayTotals {
	z := money(0)
	return gen.DayTotals{Gross: z, Discounts: z, Net: z, ServiceCharge: z, Cgst: z, Sgst: z, NonGst: z, RoundOff: z, Total: z,
		CreditNoteTotal: z, RefundTotal: z, InternalConsumption: z, Tenders: []gen.TenderTotal{}}
}
