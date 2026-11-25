package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Mut is one generic mutation of a sync op (svc-sync syncproto.Mut): entity, row id, kind (insert, set, rank, del,
// undel) and the changed fields.
type Mut struct {
	E    string            `json:"e"`
	ID   string            `json:"id"`
	K    string            `json:"k"`
	F    map[string]any    `json:"f,omitempty"`
	Prev map[string]string `json:"prev,omitempty"`
}

// Op is one accepted sync op as billing receives it from the bus: the op's identity and clock, the member and device
// that recorded it, and its mutations. billing applies only the mutations of the entities it owns and ignores the
// rest (entities/README.md).
//
// The wire shape is the data of the gateway's sync.op.<namespace> event (ADR-0011 B10). The proof of concept
// publishes the identity only; this projection needs the mutations too, so it reads them from the event data when
// present (see docs/notes.md, "ADR needed: sync op event payload").
type Op struct {
	Tenant string    `json:"-"`
	Origin string    `json:"origin"`
	Stream string    `json:"stream"`
	Seq    int64     `json:"seq"`
	Hash   string    `json:"h"`
	HLC    string    `json:"hlc"`
	Intent string    `json:"intent"`
	At     time.Time `json:"at"`
	Actor  string    `json:"actor"`
	Doc    *OpDoc    `json:"doc,omitempty"`
	Muts   []Mut     `json:"muts"`
}

// OpDoc is the legal document an op allocated a number for.
type OpDoc struct {
	Type   string `json:"type"`
	Number string `json:"number"`
}

// ParseOp decodes the data of a sync op event.
func ParseOp(tenant string, data []byte) (Op, error) {
	var op Op
	if err := json.Unmarshal(data, &op); err != nil {
		return Op{}, err
	}
	op.Tenant = tenant
	if op.Origin == "" || op.Stream == "" || op.Seq < 1 {
		return Op{}, fmt.Errorf("op without identity")
	}
	return op, nil
}

// OutletOf returns the outlet id of an "outlet:<id>" stream ("" for other streams).
func (o Op) OutletOf() string {
	if rest, ok := strings.CutPrefix(o.Stream, "outlet:"); ok {
		return rest
	}
	return ""
}

// stamp is the last-writer-wins stamp of the op: hybrid clock then origin.
func (o Op) stamp() string { return o.HLC + "|" + o.Origin }

// ---- field readers (op bodies hold strings, booleans and integers only) ----

func fstr(f map[string]any, k string) string {
	if v, ok := f[k].(string); ok {
		return v
	}
	return ""
}

func fint(f map[string]any, k string) int64 {
	switch v := f[k].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	}
	return 0
}

func fbool(f map[string]any, k string) bool {
	b, _ := f[k].(bool)
	return b
}

func fhas(f map[string]any, k string) bool { _, ok := f[k]; return ok }

// byEntity groups the mutations of an op by entity, keeping their order.
func byEntity(muts []Mut) map[string][]Mut {
	out := map[string][]Mut{}
	for _, m := range muts {
		out[m.E] = append(out[m.E], m)
	}
	return out
}

// financialYear returns the Indian financial year of an invoice date as the four digits of the document number:
// 2026-11-24 is "2627".
func financialYear(date string) string {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return ""
	}
	y := t.Year()
	if t.Month() < time.April {
		y--
	}
	return fmt.Sprintf("%02d%02d", y%100, (y+1)%100)
}

// splitNumber splits "AAA/2627/000123" into code, financial year and running number.
func splitNumber(n string) (code, fy string, running int64, ok bool) {
	p := strings.Split(n, "/")
	if len(p) != 3 {
		return "", "", 0, false
	}
	var v int64
	if _, err := fmt.Sscanf(p[2], "%d", &v); err != nil || v < 1 {
		return "", "", 0, false
	}
	return p[0], p[1], v, true
}

func formatNumber(code, fy string, n int64) string { return fmt.Sprintf("%s/%s/%06d", code, fy, n) }
