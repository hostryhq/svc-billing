package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hostryhq/go-common/ids"
	gsync "github.com/hostryhq/go-common/sync"
)

// Mut is one mutation of a sync op, as go-common's parser reads it: entity, row id, kind (insert, set, delta, rank,
// del, undel), the changed fields (strings, booleans, int64 and nil only) and the predecessor stamps of a review field.
type Mut = gsync.Mut

// Op is one accepted sync op as billing applies it: the op's identity and clock, the member and device that recorded
// it, and its mutations. billing applies only the mutations of the entities it owns (the registry says which) and
// ignores the rest.
//
// The consumer builds it from go-common's sync.ParseOp (OpOf), which verifies the hash and the body; tests and the
// double build it directly.
type Op struct {
	Tenant string
	Outlet string // the envelope's outlet, when the op names one
	Origin string
	Stream string
	Seq    int64
	Hash   string
	HLC    string
	Intent string
	At     time.Time
	Actor  string
	// Marks are the gateway's marks on the op (approved_by:<approver> when a cited approval cleared it).
	Marks []string
	Doc   *OpDoc
	Muts  []Mut
	// Voids are the ops named by sync_void records. PII is the verified personal data beside the op with its salts and
	// commitments, PIIErase the ops whose personal data this op erases (intent sync.pii.erase) and SeriesClose the device's
	// statements of the last number of a closed series (intent series.close); all from ParseOp.
	Voids       []gsync.OpRef
	PII         map[string]map[string]map[string]any
	PIISalts    map[string]map[string]map[string]string
	PIICommits  gsync.PIIC
	PIIErase    []gsync.OpRef
	SeriesClose []gsync.SeriesClose
}

// OpDoc is the legal document an op allocated a number for.
type OpDoc struct {
	Type   string
	Number string
}

// OpOf converts a parsed sync op.
func OpOf(o *gsync.Op) Op {
	op := Op{Tenant: o.TenantID.String(), Outlet: o.OutletID.String(), Origin: o.Ref.Origin, Stream: o.Ref.Stream, Seq: o.Ref.Seq, Hash: o.H, HLC: o.HLC,
		Intent: o.Intent, At: o.At, Actor: o.Actor, Marks: o.Marks, Muts: o.Muts, Voids: o.Voids, PII: o.PII, PIISalts: o.PIISalts, PIICommits: o.PIICommits, PIIErase: o.PIIErase, SeriesClose: o.SeriesClose}
	if o.Doc != nil {
		op.Doc = &OpDoc{Type: o.Doc.Type, Number: o.Doc.Number}
	}
	return op
}

// syncOp is the engine's view of the op.
func (o Op) syncOp() *gsync.Op {
	return &gsync.Op{Ref: gsync.OpRef{Origin: o.Origin, Stream: o.Stream, Seq: o.Seq}, H: o.Hash, Intent: o.Intent, Status: "accepted", TenantID: ids.ID(o.Tenant),
		OutletID: ids.ID(o.Outlet), Actor: o.Actor, HLC: o.HLC, At: o.At, Muts: o.Muts, Voids: o.Voids, PII: o.PII, PIISalts: o.PIISalts, PIICommits: o.PIICommits, PIIErase: o.PIIErase}
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
