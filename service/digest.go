package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"

	"github.com/hostryhq/svc-billing/internal/gen"
	"github.com/hostryhq/svc-billing/internal/tstamp"
	"github.com/hostryhq/svc-billing/repository"
)

// digestKinds are the billing entities that make up the digest of a business day.
var digestKinds = []struct{ entity, kind string }{
	{"bill", repository.KindBill}, {"credit_note", repository.KindCreditNote}, {"internal_consumption", repository.KindInternal},
	{"payment", repository.KindPayment}, {"refund", repository.KindRefund}, {"shift", repository.KindShift},
}

// ProjectionDigest is the test and reconciliation aid of ADR-0014 T2, T9, T11: a hash over the billing rows of one
// business date, algorithm sha256-canon-v1: per entity the SHA-256 over "id\n<json>\n" of its rows sorted by id, then
// the state hash over "entity\nrow_count\nhash\n" lines. Rows are the stored read models (canonical JSON, sorted keys).
func (s *Service) ProjectionDigest(ctx context.Context, selected, date string) (gen.ProjectionDigest, error) {
	outlet, err := s.reportOutlet(ctx, selected)
	if err != nil {
		return gen.ProjectionDigest{}, err
	}
	if date == "" {
		return gen.ProjectionDigest{}, validation("validation", "business_date is required", field("business_date", "required"))
	}
	out := gen.ProjectionDigest{OutletId: outlet, BusinessDate: mustDate(date), Algorithm: "sha256-canon-v1", Entities: []gen.EntityDigest{}}
	err = s.repo.InTx(ctx, func(ctx context.Context, tx repository.Tx) error {
		state := sha256.New()
		for _, k := range digestKinds {
			docs, err := tx.FindDocs(ctx, repository.Query{Kind: k.kind, Outlets: []string{outlet}, DateFrom: date, DateTo: date})
			if err != nil {
				return err
			}
			sort.Slice(docs, func(i, j int) bool { return docs[i].ID < docs[j].ID })
			h := sha256.New()
			for _, d := range docs {
				h.Write([]byte(d.ID + "\n"))
				h.Write(canonical(d.Body))
				h.Write([]byte("\n"))
			}
			ed := gen.EntityDigest{Entity: k.entity, RowCount: len(docs), Hash: hex.EncodeToString(h.Sum(nil))}
			out.Entities = append(out.Entities, ed)
			state.Write([]byte(ed.Entity + "\n" + itoa(ed.RowCount) + "\n" + ed.Hash + "\n"))
		}
		out.StateHash = hex.EncodeToString(state.Sum(nil))
		return nil
	})
	out.ComputedAt = tstamp.Of(s.clock.Now())
	return out, err
}
