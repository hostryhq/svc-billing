package tax

import (
	"fmt"
	"sort"
)

// RuleRow is one mapping of a rule set: (tax class, regime, specified premises, order channel) to a document type and
// rates (api-specs tax/rule-set.schema.yaml).
type RuleRow struct {
	TaxClass          string `json:"tax_class"`
	Regime            string `json:"regime"`
	SpecifiedPremises bool   `json:"specified_premises"`
	OrderChannel      string `json:"order_channel"`
	DocType           string `json:"doc_type"`
	GSTApplies        bool   `json:"gst_applies"`
	CGSTbp            int64  `json:"cgst_bp"`
	SGSTbp            int64  `json:"sgst_bp"`
	IGSTbp            int64  `json:"igst_bp"`
	SAC               string `json:"sac,omitempty"`
}

// RuleSet is a published (or draft) version.
type RuleSet struct {
	ID            string    `json:"id"`
	Version       int       `json:"version"`
	Name          string    `json:"name"`
	EffectiveDate string    `json:"effective_date"` // YYYY-MM-DD, IST invoice date
	CalcV         int       `json:"calc_v"`
	ReviewStatus  string    `json:"review_status"`
	Rules         []RuleRow `json:"rules"`
}

// InForce returns the version in force on an invoice date: the latest effective date not after it (R6.5).
func InForce(sets []RuleSet, date string) (RuleSet, bool) {
	var best RuleSet
	found := false
	for _, s := range sets {
		if s.EffectiveDate > date {
			continue
		}
		if !found || s.EffectiveDate > best.EffectiveDate || (s.EffectiveDate == best.EffectiveDate && s.Version > best.Version) {
			best, found = s, true
		}
	}
	return best, found
}

// Resolve selects the rule rows for a regime, the outlet's specified-premises flag and an order channel. The document
// type is the one the rows state; rows of one selection must agree (Validate guarantees it).
func (s RuleSet) Resolve(regime string, specified bool, channel string) (map[string]Rule, string) {
	rules := map[string]Rule{}
	docType := ""
	for _, r := range s.Rules {
		if r.Regime != regime || r.SpecifiedPremises != specified || r.OrderChannel != channel {
			continue
		}
		rules[r.TaxClass] = Rule{GST: r.GSTApplies, CGSTbp: r.CGSTbp, SGSTbp: r.SGSTbp}
		if docType == "" {
			docType = r.DocType
		}
	}
	return rules, docType
}

// Problem is one reason a rule set fails validation (BR-A1-82).
type Problem struct {
	Code   string
	Detail string
}

// Validate checks the structural rules of BR-A1-82 that need no vectors: one mapping per combination, whole basis
// points in range, two equal halves for an intra-state supply, no rate on a rule without GST, IGST zero, and the
// document type agreeing within a (regime, premises, channel) selection. A combination is "in use" when the same class
// has a mapping for the regime under another premises flag or channel, so a forgotten variant is found rather than
// silently inherited.
func (s RuleSet) Validate() []Problem {
	var out []Problem
	add := func(code, f string, a ...any) { out = append(out, Problem{Code: code, Detail: fmt.Sprintf(f, a...)}) }
	if s.CalcV < 1 || !Supported(s.CalcV) {
		add("calc_v_unsupported", "calc_v %d is not implemented by billing", s.CalcV)
	}
	if len(s.Rules) == 0 {
		add("no_rules", "a rule set needs at least one rule")
	}
	type sel struct {
		regime string
		prem   bool
		ch     string
	}
	seen := map[string]bool{}
	docOf := map[sel]string{}
	for _, r := range s.Rules {
		key := fmt.Sprintf("%s|%s|%t|%s", r.TaxClass, r.Regime, r.SpecifiedPremises, r.OrderChannel)
		if seen[key] {
			add("duplicate_mapping", "two rules for %s", key)
		}
		seen[key] = true
		if r.CGSTbp != r.SGSTbp {
			add("components_unequal", "%s: CGST and SGST must be two equal halves", key)
		}
		if r.CGSTbp < 0 || r.SGSTbp < 0 || r.CGSTbp > 2800 || r.SGSTbp > 2800 {
			add("rate_out_of_range", "%s: rate outside 0 to 28 percent per component", key)
		}
		if r.IGSTbp != 0 {
			add("igst_not_supported", "%s: IGST is 0 in Phase 2", key)
		}
		if !r.GSTApplies && (r.CGSTbp != 0 || r.SGSTbp != 0) {
			add("rate_without_gst", "%s: a rule without GST carries a rate", key)
		}
		switch r.DocType {
		case "tax_invoice", "bill_of_supply", "plain_bill":
		default:
			add("doc_type_invalid", "%s: unknown document type %q", key, r.DocType)
		}
		sl := sel{r.Regime, r.SpecifiedPremises, r.OrderChannel}
		if d, ok := docOf[sl]; ok && d != r.DocType {
			add("doc_type_conflict", "%s|%t|%s: rules of one selection state different document types", r.Regime, r.SpecifiedPremises, r.OrderChannel)
		}
		docOf[sl] = r.DocType
	}
	// A class mapped for a regime under dine-in must be mapped under every premises flag that has any dine-in rule.
	type rc struct {
		regime, class string
	}
	prem := map[rc]map[bool]bool{}
	flags := map[string]map[bool]bool{}
	for _, r := range s.Rules {
		if r.OrderChannel != "dine_in" {
			continue
		}
		k := rc{r.Regime, r.TaxClass}
		if prem[k] == nil {
			prem[k] = map[bool]bool{}
		}
		prem[k][r.SpecifiedPremises] = true
		if flags[r.Regime] == nil {
			flags[r.Regime] = map[bool]bool{}
		}
		flags[r.Regime][r.SpecifiedPremises] = true
	}
	for k, have := range prem {
		for f := range flags[k.regime] {
			if !have[f] {
				add("missing_mapping", "no dine_in rule for %s|%s|%t", k.class, k.regime, f)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Detail < out[j].Detail })
	return out
}
