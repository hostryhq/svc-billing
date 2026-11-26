// Package tax is the Go implementation of tax calculation calc_v 1 (api-specs tax/README.md, ADR-0014 R6).
// billing recomputes every finalised bill and credit note with it and records the result; it never alters or rejects a
// document. It is seeded from the api-specs reference implementation and must reproduce every shared conformance vector
// to the paisa (calc_test.go). Integers only: paise, thousandths of a unit, basis points. Rates never appear in code:
// they come from the rule set row selected for the document (ruleset.go).
package tax

import (
	"errors"
	"sort"
)

// CalcV is the calculation version this package implements.
const CalcV = 1

// Errors returned for inputs the calculation refuses; the codes are the vectors' expected_error values.
var (
	ErrDiscountExceedsValue = errors.New("discount_exceeds_value")
	ErrCreditExceeds        = errors.New("credit_exceeds_original")
	ErrUnknownLine          = errors.New("unknown_line")
	// ErrUnknownTaxClass: under the regular regime a class with no rule is refused, never computed as non-GST
	// (silent non-GST could under-tax a bill). The api-specs reference differs; see docs/notes.md.
	ErrUnknownTaxClass = errors.New("unknown_tax_class")
)

// Rule is the rate row selected from the rule set for a tax class.
type Rule struct {
	GST    bool  `json:"gst"`
	CGSTbp int64 `json:"cgst_bp"`
	SGSTbp int64 `json:"sgst_bp"`
}

// Discount is a line or bill discount: percent in basis points, or amount in paise.
type Discount struct {
	Kind  string `json:"kind"`
	Value int64  `json:"value"`
}

// Line is one input line.
type Line struct {
	ID                 string    `json:"id"`
	QtyMilli           int64     `json:"qty_milli"`
	UnitPricePaise     int64     `json:"unit_price_paise"`
	ModifierDeltaPaise int64     `json:"modifier_delta_paise"`
	TaxClass           string    `json:"tax_class"`
	Discount           *Discount `json:"discount"`
	Comp               bool      `json:"comp"`
}

// Packing is a fixed packing charge taxed in its own class.
type Packing struct {
	AmountPaise int64  `json:"amount_paise"`
	TaxClass    string `json:"tax_class"`
}

// Bill is the calculation input.
type Bill struct {
	Regime          string          `json:"regime"`
	PriceMode       string          `json:"price_mode"`
	RoundOff        bool            `json:"round_off"`
	Rules           map[string]Rule `json:"rules"`
	Lines           []Line          `json:"lines"`
	BillDiscount    *Discount       `json:"bill_discount"`
	ServiceChargeBp int64           `json:"service_charge_bp"`
	Packing         *Packing        `json:"packing"`
}

// LineOut is the per-line result.
type LineOut struct {
	ID                     string `json:"id"`
	ValuePaise             int64  `json:"value_paise"`
	LineDiscountPaise      int64  `json:"line_discount_paise"`
	BillDiscountAllocPaise int64  `json:"bill_discount_alloc_paise"`
	NetPaise               int64  `json:"net_paise"`
}

// Group is one tax group (tax class and effective rate).
type Group struct {
	TaxClass         string `json:"tax_class"`
	GSTApplies       bool   `json:"gst_applies"`
	RateBp           int64  `json:"rate_bp"`
	ValuePaise       int64  `json:"value_paise"`
	ChargeAllocPaise int64  `json:"charge_alloc_paise"`
	TaxablePaise     int64  `json:"taxable_paise"`
	CGSTPaise        int64  `json:"cgst_paise"`
	SGSTPaise        int64  `json:"sgst_paise"`
}

// Result is the calculation output for a bill.
type Result struct {
	Lines              []LineOut `json:"lines"`
	GrossPaise         int64     `json:"gross_paise"`
	LineDiscountPaise  int64     `json:"line_discount_paise"`
	BillDiscountPaise  int64     `json:"bill_discount_paise"`
	NetPaise           int64     `json:"net_paise"`
	ServiceChargePaise int64     `json:"service_charge_paise"`
	PackingPaise       int64     `json:"packing_paise"`
	Groups             []Group   `json:"groups"`
	NonGSTSubtotal     int64     `json:"non_gst_subtotal_paise"`
	TaxablePaise       int64     `json:"taxable_paise"`
	CGSTPaise          int64     `json:"cgst_paise"`
	SGSTPaise          int64     `json:"sgst_paise"`
	PreRoundTotalPaise int64     `json:"pre_round_total_paise"`
	RoundOffPaise      int64     `json:"round_off_paise"`
	TotalPaise         int64     `json:"total_paise"`
}

// CreditLine names a credited quantity of an original line.
type CreditLine struct {
	LineID   string `json:"line_id"`
	QtyMilli int64  `json:"qty_milli"`
}

// CreditLineOut is the per-line credit result.
type CreditLineOut struct {
	ID               string `json:"id"`
	CreditedQtyMilli int64  `json:"credited_qty_milli"`
	CreditedNetPaise int64  `json:"credited_net_paise"`
}

// CreditResult is the calculation output for a credit note.
type CreditResult struct {
	Lines              []CreditLineOut `json:"lines"`
	CreditedNetPaise   int64           `json:"credited_net_paise"`
	Groups             []Group         `json:"groups"`
	NonGSTSubtotal     int64           `json:"non_gst_subtotal_paise"`
	TaxablePaise       int64           `json:"taxable_paise"`
	CGSTPaise          int64           `json:"cgst_paise"`
	SGSTPaise          int64           `json:"sgst_paise"`
	PreRoundTotalPaise int64           `json:"pre_round_total_paise"`
	RoundOffPaise      int64           `json:"round_off_paise"`
	TotalPaise         int64           `json:"total_paise"`
}

// roundHalfUp is a/b rounded half up for a >= 0, b > 0.
func roundHalfUp(a, b int64) int64 { return (2*a + b) / (2 * b) }

// allocate splits total over weights by the largest-remainder method; remainder ties go to the earlier index.
func allocate(total int64, weights []int64) []int64 {
	out := make([]int64, len(weights))
	var sum int64
	for _, w := range weights {
		sum += w
	}
	if total == 0 || sum == 0 {
		return out
	}
	rem := make([]int64, len(weights))
	var given int64
	for i, w := range weights {
		out[i] = total * w / sum
		rem[i] = total*w - out[i]*sum
		given += out[i]
	}
	idx := make([]int, len(weights))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return rem[idx[a]] > rem[idx[b]] })
	for k := int64(0); k < total-given; k++ {
		out[idx[k]]++
	}
	return out
}

func ruleOf(b *Bill, class string) (gst bool, cg, sg int64) {
	if b.Regime != "regular" {
		return false, 0, 0
	}
	r := b.Rules[class]
	return r.GST, r.CGSTbp, r.SGSTbp
}

type calcLine struct {
	id                  string
	class               string
	qty, value, ld, net int64
	alloc, net2         int64
}

func (b *Bill) lines() ([]calcLine, int64, error) {
	ls := make([]calcLine, len(b.Lines))
	var s int64
	for i, in := range b.Lines {
		unit := in.UnitPricePaise + in.ModifierDeltaPaise
		if unit < 0 {
			unit = 0
		}
		v := roundHalfUp(unit*in.QtyMilli, 1000)
		var d int64
		switch {
		case in.Comp:
			d = v
		case in.Discount != nil:
			if in.Discount.Kind == "percent" {
				if in.Discount.Value > 10000 {
					return nil, 0, ErrDiscountExceedsValue
				}
				d = roundHalfUp(v*in.Discount.Value, 10000)
			} else {
				d = in.Discount.Value
			}
			if d > v {
				return nil, 0, ErrDiscountExceedsValue
			}
		}
		ls[i] = calcLine{id: in.ID, class: in.TaxClass, qty: in.QtyMilli, value: v, ld: d, net: v - d}
		s += v - d
	}
	var dd int64
	if bd := b.BillDiscount; bd != nil {
		if bd.Kind == "percent" {
			if bd.Value > 10000 {
				return nil, 0, ErrDiscountExceedsValue
			}
			dd = roundHalfUp(s*bd.Value, 10000)
		} else {
			dd = bd.Value
		}
		if dd > s {
			return nil, 0, ErrDiscountExceedsValue
		}
	}
	w := make([]int64, len(ls))
	for i := range ls {
		w[i] = ls[i].net
	}
	for i, a := range allocate(dd, w) {
		ls[i].alloc = a
		ls[i].net2 = ls[i].net - a
	}
	return ls, dd, nil
}

// taxGroup applies the group arithmetic to a group value.
func (b *Bill) taxGroup(class string, value, charge int64) Group {
	gst, cg, sg := ruleOf(b, class)
	g := Group{TaxClass: class, GSTApplies: gst, RateBp: cg + sg, ValuePaise: value, ChargeAllocPaise: charge, TaxablePaise: value}
	if gst {
		taxable := value
		if b.PriceMode == "inclusive" {
			taxable = roundHalfUp(value*10000, 10000+cg+sg)
		}
		g.TaxablePaise = taxable
		g.CGSTPaise = roundHalfUp(taxable*cg, 10000)
		g.SGSTPaise = roundHalfUp(taxable*sg, 10000)
	}
	return g
}

type totals struct {
	nonGST, taxable, cgst, sgst, pre, roundOff, total int64
}

func (b *Bill) totalsOf(groups []Group) totals {
	var t totals
	var gross int64
	for _, g := range groups {
		gross += g.ValuePaise
		if g.GSTApplies {
			t.taxable += g.TaxablePaise
		} else {
			t.nonGST += g.ValuePaise
		}
		t.cgst += g.CGSTPaise
		t.sgst += g.SGSTPaise
	}
	if b.PriceMode == "inclusive" {
		t.pre = gross
	} else {
		t.pre = t.nonGST + t.taxable + t.cgst + t.sgst
	}
	t.total = t.pre
	if b.RoundOff {
		t.total = roundHalfUp(t.pre, 100) * 100
	}
	t.roundOff = t.total - (t.nonGST + t.taxable + t.cgst + t.sgst)
	return t
}

func classesOf(ls []calcLine, p *Packing) []string {
	set := map[string]bool{}
	for _, l := range ls {
		set[l.class] = true
	}
	if p != nil && p.AmountPaise > 0 {
		set[p.TaxClass] = true
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

func (b *Bill) groups(ls []calcLine) (groups []Group, service, packing int64) {
	var base int64
	for _, l := range ls {
		base += l.net2
	}
	service = roundHalfUp(base*b.ServiceChargeBp, 10000)
	classes := classesOf(ls, b.Packing)
	vals := make([]int64, len(classes))
	for i, c := range classes {
		for _, l := range ls {
			if l.class == c {
				vals[i] += l.net2
			}
		}
	}
	share := allocate(service, vals)
	for i, c := range classes {
		charge := share[i]
		if b.Packing != nil && b.Packing.AmountPaise > 0 && b.Packing.TaxClass == c {
			charge += b.Packing.AmountPaise
			packing = b.Packing.AmountPaise
		}
		groups = append(groups, b.taxGroup(c, vals[i]+charge, charge))
	}
	return groups, service, packing
}

// Calculate computes a bill.
func Calculate(b Bill) (Result, error) {
	if err := b.checkClasses(); err != nil {
		return Result{}, err
	}
	ls, dd, err := b.lines()
	if err != nil {
		return Result{}, err
	}
	groups, service, packing := b.groups(ls)
	t := b.totalsOf(groups)
	r := Result{BillDiscountPaise: dd, ServiceChargePaise: service, PackingPaise: packing, Groups: groups,
		NonGSTSubtotal: t.nonGST, TaxablePaise: t.taxable, CGSTPaise: t.cgst, SGSTPaise: t.sgst, PreRoundTotalPaise: t.pre, RoundOffPaise: t.roundOff, TotalPaise: t.total}
	for _, l := range ls {
		r.Lines = append(r.Lines, LineOut{ID: l.id, ValuePaise: l.value, LineDiscountPaise: l.ld, BillDiscountAllocPaise: l.alloc, NetPaise: l.net2})
		r.GrossPaise += l.value
		r.LineDiscountPaise += l.ld
		r.NetPaise += l.net2
	}
	return r, nil
}

// CalculateCredit computes a credit note against the original bill's inputs: the credited value of a line is its
// value after the original discounts, prorated by quantity; each group's charges are prorated with the credited value.
func CalculateCredit(orig Bill, credit []CreditLine) (CreditResult, error) {
	if err := orig.checkClasses(); err != nil {
		return CreditResult{}, err
	}
	ls, _, err := orig.lines()
	if err != nil {
		return CreditResult{}, err
	}
	known := map[string]bool{}
	for _, l := range ls {
		known[l.id] = true
	}
	qty := map[string]int64{}
	for _, c := range credit {
		if !known[c.LineID] {
			return CreditResult{}, ErrUnknownLine
		}
		qty[c.LineID] = c.QtyMilli
	}
	origGroups, _, _ := orig.groups(ls)
	var out CreditResult
	credited := map[string]int64{}
	original := map[string]int64{}
	for _, l := range ls {
		q := qty[l.id]
		if q > l.qty {
			return CreditResult{}, ErrCreditExceeds
		}
		var cn int64
		if q > 0 {
			cn = roundHalfUp(l.net2*q, l.qty)
		}
		out.Lines = append(out.Lines, CreditLineOut{ID: l.id, CreditedQtyMilli: q, CreditedNetPaise: cn})
		out.CreditedNetPaise += cn
		credited[l.class] += cn
		original[l.class] += l.net2
	}
	for _, g := range origGroups {
		var cc int64
		if original[g.TaxClass] > 0 {
			cc = roundHalfUp(g.ChargeAllocPaise*credited[g.TaxClass], original[g.TaxClass])
		}
		out.Groups = append(out.Groups, orig.taxGroup(g.TaxClass, credited[g.TaxClass]+cc, cc))
	}
	t := orig.totalsOf(out.Groups)
	out.NonGSTSubtotal, out.TaxablePaise, out.CGSTPaise, out.SGSTPaise = t.nonGST, t.taxable, t.cgst, t.sgst
	out.PreRoundTotalPaise, out.RoundOffPaise, out.TotalPaise = t.pre, t.roundOff, t.total
	return out, nil
}

// checkClasses refuses an unknown tax class under the regular regime.
func (b *Bill) checkClasses() error {
	if b.Regime != "regular" {
		return nil
	}
	for _, l := range b.Lines {
		if _, ok := b.Rules[l.TaxClass]; !ok {
			return ErrUnknownTaxClass
		}
	}
	if b.Packing != nil && b.Packing.AmountPaise > 0 {
		if _, ok := b.Rules[b.Packing.TaxClass]; !ok {
			return ErrUnknownTaxClass
		}
	}
	return nil
}
