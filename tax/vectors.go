package tax

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"reflect"
)

// vectorsJSON is a copy of api-specs tax/conformance/vectors.v1.json, refreshed by `make gen` and compared with the
// pinned api-specs file by TestEmbeddedVectorsMatchSpec, so the copy cannot drift.
//
//go:embed vectors/vectors.v1.json
var vectorsJSON []byte

//go:embed vectors/draft-unreviewed.v1.yaml
var draftYAML []byte

// DraftYAML is the example draft rule set of api-specs (tests and the admin panel's first load).
func DraftYAML() []byte { return append([]byte(nil), draftYAML...) }

// Supported reports whether billing implements calc_v. Billing keeps every calc_v a released client could have used
// (ADR-0014 R6.7); today that is 1.
func Supported(calcV int) bool { return calcV == CalcV }

type vector struct {
	ID            string          `json:"id"`
	Kind          string          `json:"kind"`
	Invariant     string          `json:"invariant"`
	Input         json.RawMessage `json:"input"`
	Expected      json.RawMessage `json:"expected"`
	ExpectedError string          `json:"expected_error"`
}

type vectorFile struct {
	CalcV int      `json:"calc_v"`
	Cases []vector `json:"cases"`
}

type creditInput struct {
	Original Bill         `json:"original"`
	Credit   []CreditLine `json:"credit"`
}

// VectorResult is the outcome of one conformance vector.
type VectorResult struct {
	ID    string
	Error string // empty when the engine reproduced the vector to the paisa
}

// RunVectors runs every embedded conformance vector through the engine. The tax rates inside the vectors are part of
// each vector; the rule set under validation is checked structurally by RuleSet.Validate.
func RunVectors() ([]VectorResult, error) {
	var f vectorFile
	if err := json.Unmarshal(vectorsJSON, &f); err != nil {
		return nil, err
	}
	if f.CalcV != CalcV {
		return nil, errors.New("tax: embedded vectors are for another calc_v")
	}
	out := make([]VectorResult, 0, len(f.Cases))
	for _, v := range f.Cases {
		out = append(out, VectorResult{ID: v.ID, Error: runVector(v)})
	}
	return out, nil
}

func runVector(v vector) string {
	switch v.Kind {
	case "bill":
		var in Bill
		if err := json.Unmarshal(v.Input, &in); err != nil {
			return err.Error()
		}
		r, err := Calculate(in)
		if v.ExpectedError != "" {
			return expectErr(err, v.ExpectedError)
		}
		if err != nil {
			return err.Error()
		}
		var want Result
		if err := json.Unmarshal(v.Expected, &want); err != nil {
			return err.Error()
		}
		if !reflect.DeepEqual(r, want) {
			return "result differs from the vector"
		}
	case "credit_note":
		var in creditInput
		if err := json.Unmarshal(v.Input, &in); err != nil {
			return err.Error()
		}
		r, err := CalculateCredit(in.Original, in.Credit)
		if v.ExpectedError != "" {
			return expectErr(err, v.ExpectedError)
		}
		if err != nil {
			return err.Error()
		}
		var want CreditResult
		if err := json.Unmarshal(v.Expected, &want); err != nil {
			return err.Error()
		}
		if !reflect.DeepEqual(r, want) {
			return "result differs from the vector"
		}
		if v.Invariant == "totals_equal_original" {
			b, err := Calculate(in.Original)
			if err != nil {
				return err.Error()
			}
			if r.TotalPaise != b.TotalPaise || r.CGSTPaise != b.CGSTPaise || r.SGSTPaise != b.SGSTPaise || r.RoundOffPaise != b.RoundOffPaise {
				return "a full credit note must equal the bill"
			}
		}
	default:
		return "unknown vector kind " + v.Kind
	}
	return ""
}

func expectErr(err error, want string) string {
	if err == nil || err.Error() != want {
		return "want error " + want
	}
	return ""
}

// VectorBytes returns the embedded vector file (the copy test compares it with api-specs).
func VectorBytes() []byte { return bytes.Clone(vectorsJSON) }
