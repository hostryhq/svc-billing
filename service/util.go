package service

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/hostryhq/svc-billing/internal/gen"
	"github.com/hostryhq/svc-billing/internal/tstamp"
)

// idNamespace is the UUIDv5 namespace of deterministic Hostry ids (api-specs entities/README.md, provisional until
// fixed in go-common/sync).
var idNamespace = uuid.MustParse("9a3c7f1e-5b2d-5c84-a1e6-4d0f8b27c3a9")

func uuid5(prefix, name string) string {
	return uuid.NewSHA1(idNamespace, []byte(prefix+"|"+name)).String()
}

// dayID is the business day id: UUIDv5 of the outlet and the date (A1.1 item 1; entities/restaurant/business_day.yaml).
func dayID(outlet, date string) string {
	return uuid.NewSHA1(idNamespace, []byte("business_day|"+outlet+"|"+date)).String()
}

// RuleSetRowID is the id of the tax_rule_set row emitted to one organisation: UUIDv5 of tenant and version.
func RuleSetRowID(tenant string, version int) string {
	return uuid.NewSHA1(idNamespace, []byte("tax_rule_set|"+tenant+"|"+itoa(version))).String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for ; n > 0; n /= 10 {
		i--
		b[i] = byte('0' + n%10)
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func parseDate(s string) *openapi_types.Date {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return nil
	}
	return &openapi_types.Date{Time: t}
}

func mustDate(s string) openapi_types.Date {
	d := parseDate(s)
	if d == nil {
		return openapi_types.Date{}
	}
	return *d
}

func addTotals(a, b gen.DayTotals) gen.DayTotals {
	a.Gross.AmountPaise += b.Gross.AmountPaise
	a.Discounts.AmountPaise += b.Discounts.AmountPaise
	a.Net.AmountPaise += b.Net.AmountPaise
	a.ServiceCharge.AmountPaise += b.ServiceCharge.AmountPaise
	a.Cgst.AmountPaise += b.Cgst.AmountPaise
	a.Sgst.AmountPaise += b.Sgst.AmountPaise
	a.NonGst.AmountPaise += b.NonGst.AmountPaise
	a.RoundOff.AmountPaise += b.RoundOff.AmountPaise
	a.Total.AmountPaise += b.Total.AmountPaise
	a.BillCount += b.BillCount
	a.CreditNoteCount += b.CreditNoteCount
	a.CreditNoteTotal.AmountPaise += b.CreditNoteTotal.AmountPaise
	a.RefundTotal.AmountPaise += b.RefundTotal.AmountPaise
	a.InternalConsumption.AmountPaise += b.InternalConsumption.AmountPaise
	for _, t := range b.Tenders {
		found := false
		for i := range a.Tenders {
			if a.Tenders[i].Tender == t.Tender && deref(a.Tenders[i].Name) == deref(t.Name) {
				a.Tenders[i].Count += t.Count
				a.Tenders[i].Amount.AmountPaise += t.Amount.AmountPaise
				found = true
			}
		}
		if !found {
			a.Tenders = append(a.Tenders, t)
		}
	}
	sort.SliceStable(a.Tenders, func(i, j int) bool { return a.Tenders[i].Tender < a.Tenders[j].Tender })
	return a
}

func deref[T any](p *T) T {
	var z T
	if p == nil {
		return z
	}
	return *p
}

func ptr[T any](v T) *T { return &v }

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// canonical re-encodes JSON with sorted keys and no whitespace (encoding/json sorts map keys).
func canonical(b []byte) []byte {
	var v any
	if json.Unmarshal(b, &v) != nil {
		return b
	}
	out, err := json.Marshal(v)
	if err != nil {
		return b
	}
	return out
}

func tsOr(p *tstamp.Time, fallback time.Time) tstamp.Time {
	if p != nil {
		return *p
	}
	return tstamp.Of(fallback)
}

func fromTime(t time.Time) tstamp.Time { return tstamp.Of(t) }

func tsPtr(t tstamp.Time) *tstamp.Time { return &t }

// mergeRanges folds series ranges of the same series and document type together by first and last number.
func mergeRanges(a, b []gen.SeriesRange) []gen.SeriesRange {
	out := append([]gen.SeriesRange(nil), a...)
	for _, r := range b {
		found := false
		for i := range out {
			if out[i].SeriesCode == r.SeriesCode && out[i].DocType == r.DocType {
				if r.FirstNumber < out[i].FirstNumber {
					out[i].FirstNumber = r.FirstNumber
				}
				if r.LastNumber > out[i].LastNumber {
					out[i].LastNumber = r.LastNumber
				}
				out[i].Count += r.Count
				found = true
			}
		}
		if !found {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].SeriesCode+string(out[i].DocType) < out[j].SeriesCode+string(out[j].DocType)
	})
	return out
}
