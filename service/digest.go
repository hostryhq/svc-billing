package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	gsync "github.com/hostryhq/go-common/sync"

	"github.com/hostryhq/svc-billing/internal/gen"
	"github.com/hostryhq/svc-billing/internal/tstamp"
	"github.com/hostryhq/svc-billing/repository"
)

// KindDigestRow is the document kind of the canonical row texts that make up the projection digest of a business day: one
// document per row of an entity billing owns that belongs to a business day (id: entity|row id, K2: entity, Sort: row id,
// Date: the business date, Body: the JSON string holding the row text, which a JSONB column keeps as written).
const KindDigestRow = "digest_row"

// Algorithm is the projection digest algorithm (sync/protocol-v1.md, Projection digest).
const Algorithm = "sha256-canon-v1"

// parentEntities are the owned parents through which an entity without a business_date belongs to a day (the protocol lists
// order_id, check_id, kot_id, bill_id and credit_note_id; the others are orders' entities, which billing does not hold).
var parentEntities = map[string]string{"bill_id": "bill", "credit_note_id": "credit_note"}

// parentOrder is the order of the protocol's parent fields: the first one the entity has is its parent.
var parentOrder = []string{"order_id", "check_id", "kot_id", "bill_id", "credit_note_id"}

func digestDocID(entity, id string) string { return entity + "|" + id }

// DigestRowText is the canonical row text of sha256-canon-v1: the RFC 8785 JSON (integers only) of an object holding id
// and every field the row has set, without null fields and without pii fields.
func DigestRowText(id string, fields map[string]any, pii map[string]bool) (string, error) {
	o := map[string]any{"id": id}
	for k, v := range fields {
		if v == nil || pii[k] {
			continue
		}
		o[k] = v
	}
	b, err := gsync.CanonicalJSON(o)
	return string(b), err
}

// DigestRowDoc is the digest document of one row text.
func DigestRowDoc(entity, id, outlet, date, parent, text string) repository.Doc {
	b, _ := json.Marshal(text)
	return repository.Doc{Kind: KindDigestRow, ID: digestDocID(entity, id), Outlet: outlet, Date: date, Sort: id, K1: parent, K2: entity, Body: b}
}

// dayOf says how an entity belongs to a business day: by its own business_date (parent ""), or through an owned parent
// field. ok is false for an entity that belongs to no day (configuration, rule sets, day-close echoes) or whose parent is
// not billing's.
func dayOf(ent *gsync.Entity) (parentField string, ok bool) {
	if _, own := ent.Fields["business_date"]; own {
		return "", true
	}
	for _, f := range parentOrder {
		if _, has := ent.Fields[f]; has {
			_, owned := parentEntities[f]
			return f, owned
		}
	}
	return "", false
}

// digestRows keeps the digest documents of the rows this op resolved: written (or replaced) while the row exists, deleted
// when its insert was voided or it is tombstoned. A child row whose parent arrives later takes the day of the parent then.
func (ac *applyCtx) digestRows(ctx context.Context, reg *gsync.Registry, rows []gsync.Row) error {
	for _, r := range rows {
		ent := reg.Entities[r.Key.Entity]
		if ent == nil || reg.RoleOf(Name, ent.Name) != gsync.RoleOwner {
			continue
		}
		parentField, ok := dayOf(ent)
		if !ok {
			continue
		}
		docID := digestDocID(ent.Name, r.Key.ID)
		if !r.Exists || r.Deleted {
			if err := ac.tx.DeleteDoc(ctx, KindDigestRow, docID); err != nil {
				return err
			}
			continue
		}
		pii := map[string]bool{}
		for n, f := range ent.Fields {
			pii[n] = f.PII
		}
		text, err := DigestRowText(r.Key.ID, r.Fields, pii)
		if err != nil {
			return err
		}
		outlet, date, parent := ac.outlet, fstr(r.Fields, "business_date"), ""
		if old, err := ac.tx.GetDoc(ctx, KindDigestRow, docID); err == nil {
			outlet = old.Outlet
		}
		if parentField != "" {
			parent = digestDocID(parentEntities[parentField], fstr(r.Fields, parentField))
			if p, err := ac.tx.GetDoc(ctx, KindDigestRow, parent); err == nil {
				date = p.Date
			}
		}
		if err := ac.tx.SaveDoc(ctx, DigestRowDoc(ent.Name, r.Key.ID, outlet, date, parent, text)); err != nil {
			return err
		}
		if ent.Name != "bill" && ent.Name != "credit_note" {
			continue
		}
		kids, err := ac.tx.FindDocs(ctx, repository.Query{Kind: KindDigestRow, K1: docID})
		if err != nil {
			return err
		}
		for _, k := range kids {
			if k.Date != date {
				k.Date = date
				if err := ac.tx.SaveDoc(ctx, k); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// ProjectionDigest is the test and reconciliation aid of ADR-0014 T2, T9, T11, algorithm sha256-canon-v1 (sync/protocol-v1.md):
// the rows of the entities billing owns that belong to the business date (an own business_date, or a parent that has one),
// tombstones left out. An entity digest is the SHA-256 over its row texts in ascending id order, each followed by a line
// feed; the state hash is the SHA-256 over "<entity>\n<row_count>\n<hash>\n" for each listed entity in ascending name order.
// Only entities with at least one row are listed.
func (s *Service) ProjectionDigest(ctx context.Context, selected, date string) (gen.ProjectionDigest, error) {
	outlet, err := s.reportOutlet(ctx, selected)
	if err != nil {
		return gen.ProjectionDigest{}, err
	}
	if date == "" {
		return gen.ProjectionDigest{}, validation("validation", "business_date is required", field("business_date", "required"))
	}
	out := gen.ProjectionDigest{OutletId: outlet, BusinessDate: mustDate(date), Algorithm: Algorithm, Entities: []gen.EntityDigest{}}
	err = s.repo.InTx(ctx, func(ctx context.Context, tx repository.Tx) error {
		docs, err := tx.FindDocs(ctx, repository.Query{Kind: KindDigestRow, Outlets: []string{outlet}, DateFrom: date, DateTo: date})
		if err != nil {
			return err
		}
		by := map[string][]repository.Doc{}
		for _, d := range docs {
			by[d.K2] = append(by[d.K2], d)
		}
		names := make([]string, 0, len(by))
		for n := range by {
			names = append(names, n)
		}
		sort.Strings(names)
		state := sha256.New()
		for _, n := range names {
			rs := by[n]
			sort.Slice(rs, func(i, j int) bool { return rs[i].Sort < rs[j].Sort })
			h := sha256.New()
			for _, d := range rs {
				var text string
				if err := json.Unmarshal(d.Body, &text); err != nil {
					return err
				}
				h.Write([]byte(text + "\n"))
			}
			ed := gen.EntityDigest{Entity: n, RowCount: len(rs), Hash: hex.EncodeToString(h.Sum(nil))}
			out.Entities = append(out.Entities, ed)
			state.Write([]byte(ed.Entity + "\n" + itoa(ed.RowCount) + "\n" + ed.Hash + "\n"))
		}
		out.StateHash = hex.EncodeToString(state.Sum(nil))
		return nil
	})
	out.ComputedAt = tstamp.Of(s.clock.Now())
	return out, err
}
