// Package mem is the in-memory repository the test double runs on. It honours the same contract as the
// Postgres one: tenant isolation (a transaction sees only its tenant's rows), atomic transactions (all or
// nothing), idempotency keys, a transactional outbox and the global tables.
package mem

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/events"
	"github.com/hostryhq/go-common/tenancy"

	"github.com/hostryhq/svc-billing/internal/nulldb"
	"github.com/hostryhq/svc-billing/repository"
)

type idemRow struct {
	fp   string
	resp []byte
}

type tenant struct {
	docs map[string]repository.Doc // kind + "\x00" + id
	ops  map[string]string
	idem map[string]idemRow
	keys map[string]int64
}

func newTenant() *tenant {
	return &tenant{docs: map[string]repository.Doc{}, ops: map[string]string{}, idem: map[string]idemRow{}, keys: map[string]int64{}}
}

func (t *tenant) clone() *tenant {
	c := newTenant()
	for k, v := range t.docs {
		c.docs[k] = v
	}
	for k, v := range t.ops {
		c.ops[k] = v
	}
	for k, v := range t.idem {
		c.idem[k] = v
	}
	for k, v := range t.keys {
		c.keys[k] = v
	}
	return c
}

type global struct {
	masters   map[string]repository.Master
	grants    map[string]time.Time
	tenants   map[string]time.Time
	emissions map[string]*emissionRow // ruleSetID + "|" + tenant
	idem      map[string]idemRow
	keys      map[string]int64
}

type emissionRow struct {
	ruleSet, tenant string
	acked           bool
	ackedAt         time.Time
}

func newGlobal() *global {
	return &global{masters: map[string]repository.Master{}, grants: map[string]time.Time{}, tenants: map[string]time.Time{},
		emissions: map[string]*emissionRow{}, idem: map[string]idemRow{}, keys: map[string]int64{}}
}

func (g *global) clone() *global {
	c := newGlobal()
	for k, v := range g.masters {
		c.masters[k] = v
	}
	for k, v := range g.grants {
		c.grants[k] = v
	}
	for k, v := range g.tenants {
		c.tenants[k] = v
	}
	for k, v := range g.emissions {
		cp := *v
		c.emissions[k] = &cp
	}
	for k, v := range g.idem {
		c.idem[k] = v
	}
	for k, v := range g.keys {
		c.keys[k] = v
	}
	return c
}

// Store is the in-memory repository. The zero value is not usable; call New.
type Store struct {
	mu      sync.Mutex
	tenants map[string]*tenant
	glob    *global
	outbox  []events.Envelope
	db      *sql.DB // a null database: go-auth's audited transaction needs a *sql.Tx, so the double runs its real code path
}

var _ repository.Repository = (*Store)(nil)

// New returns an empty store.
func New() *Store { return &Store{tenants: map[string]*tenant{}, glob: newGlobal(), db: nulldb.Open()} }

// Reset empties the store (the test double's SeedWorld starts from nothing).
func (s *Store) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tenants = map[string]*tenant{}
	s.glob = newGlobal()
	s.outbox = nil
}

// Events returns the events published so far, in order.
func (s *Store) Events() []events.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]events.Envelope(nil), s.outbox...)
}

// Ping implements repository.Repository.
func (s *Store) Ping(context.Context) error { return nil }

// InTx implements repository.Repository.
func (s *Store) InTx(ctx context.Context, fn func(ctx context.Context, tx repository.Tx) error) error {
	tc, err := tenancy.Require(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := tc.TenantID.String()
	cur := s.tenants[id]
	if cur == nil {
		cur = newTenant()
	}
	x := &tx{t: cur.clone()}
	// the same audited path as production: a vendor or support principal's audit row is written with the work
	if err := auth.WithAuditedTx(ctx, s.db, func(ctx context.Context, _ *sql.Tx) error { return fn(ctx, x) }); err != nil {
		return err
	}
	s.tenants[id] = x.t
	s.outbox = append(s.outbox, x.pub...)
	return nil
}

// InGlobalTx implements repository.Repository.
func (s *Store) InGlobalTx(ctx context.Context, fn func(ctx context.Context, tx repository.GlobalTx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	x := &gtx{g: s.glob.clone()}
	run := func(ctx context.Context, _ *sql.Tx) error { return fn(ctx, x) }
	var err error
	if p, ok := auth.PrincipalFrom(ctx); ok && p.IsVendorActor() && !repository.NoAudit(ctx) {
		err = auth.WithPlatformAuditedTx(ctx, s.db, run)
	} else {
		err = run(ctx, nil)
	}
	if err != nil {
		return err
	}
	s.glob = x.g
	s.outbox = append(s.outbox, x.pub...)
	return nil
}

type tx struct {
	t   *tenant
	pub []events.Envelope
}

func dk(kind, id string) string { return kind + "\x00" + id }

func (x *tx) PutDoc(_ context.Context, d repository.Doc) (bool, error) {
	k := dk(d.Kind, d.ID)
	if _, ok := x.t.docs[k]; ok {
		return false, nil
	}
	x.t.docs[k] = d
	return true, nil
}

func (x *tx) SaveDoc(_ context.Context, d repository.Doc) error {
	x.t.docs[dk(d.Kind, d.ID)] = d
	return nil
}

func (x *tx) GetDoc(_ context.Context, kind, id string) (repository.Doc, error) {
	d, ok := x.t.docs[dk(kind, id)]
	if !ok {
		return repository.Doc{}, repository.ErrNotFound
	}
	return d, nil
}

func (x *tx) DeleteDoc(_ context.Context, kind, id string) error {
	delete(x.t.docs, dk(kind, id))
	return nil
}

func inOutlets(outlets []string, o string) bool {
	if outlets == nil {
		return true
	}
	for _, v := range outlets {
		if v == o {
			return true
		}
	}
	return false
}

// less reports list order: newest first by default (Sort then ID descending), oldest first when asc.
func less(a, b repository.Doc, asc bool) bool {
	if a.Sort != b.Sort {
		if asc {
			return a.Sort < b.Sort
		}
		return a.Sort > b.Sort
	}
	if asc {
		return a.ID < b.ID
	}
	return a.ID > b.ID
}

func (x *tx) FindDocs(_ context.Context, q repository.Query) ([]repository.Doc, error) {
	var out []repository.Doc
	for _, d := range x.t.docs {
		if d.Kind != q.Kind || !inOutlets(q.Outlets, d.Outlet) {
			continue
		}
		if q.DateFrom != "" && d.Date < q.DateFrom || q.DateTo != "" && d.Date > q.DateTo {
			continue
		}
		if q.K1 != "" && d.K1 != q.K1 || q.K2 != "" && d.K2 != q.K2 || q.Sort != "" && d.Sort != q.Sort {
			continue
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return less(out[i], out[j], q.Asc) })
	if q.After != "" {
		if cur, ok := x.t.docs[dk(q.Kind, q.After)]; ok {
			i := sort.Search(len(out), func(i int) bool { return less(cur, out[i], q.Asc) })
			out = out[i:]
		}
	}
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func (x *tx) MarkOp(_ context.Context, origin, stream string, seq int64, hash string) (bool, error) {
	k := fmt.Sprintf("%s|%s|%d", origin, stream, seq)
	if _, ok := x.t.ops[k]; ok {
		return false, nil
	}
	x.t.ops[k] = hash
	return true, nil
}

func (x *tx) IdemGet(_ context.Context, op, key string) (string, []byte, bool, error) {
	r, ok := x.t.idem[op+"\x00"+key]
	return r.fp, r.resp, ok, nil
}

func (x *tx) IdemPut(_ context.Context, op, key, fp string, resp []byte, _ time.Time) error {
	x.t.idem[op+"\x00"+key] = idemRow{fp, resp}
	return nil
}

func (x *tx) Publish(_ context.Context, e events.Envelope) error {
	x.t.keys[e.Key]++
	e.KeySeq = x.t.keys[e.Key]
	x.pub = append(x.pub, e)
	return nil
}

type gtx struct {
	g   *global
	pub []events.Envelope
}

func (x *gtx) PutMaster(_ context.Context, m repository.Master) error {
	x.g.masters[m.ID] = m
	return nil
}

func (x *gtx) GetMaster(_ context.Context, id string) (repository.Master, error) {
	m, ok := x.g.masters[id]
	if !ok {
		return repository.Master{}, repository.ErrNotFound
	}
	return m, nil
}

func (x *gtx) ListMasters(_ context.Context, state, after string, limit int) ([]repository.Master, error) {
	var out []repository.Master
	for _, m := range x.g.masters {
		if state == "" || m.State == state {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Version != out[j].Version {
			return out[i].Version > out[j].Version
		}
		return out[i].ID > out[j].ID
	})
	if after != "" {
		if cur, ok := x.g.masters[after]; ok {
			i := sort.Search(len(out), func(i int) bool {
				return out[i].Version < cur.Version || out[i].Version == cur.Version && out[i].ID < cur.ID
			})
			out = out[i:]
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (x *gtx) DeleteMaster(_ context.Context, id string) error {
	delete(x.g.masters, id)
	return nil
}

func (x *gtx) NextVersion(context.Context) (int, error) {
	n := 0
	for _, m := range x.g.masters {
		if m.Version > n {
			n = m.Version
		}
	}
	return n + 1, nil
}

func (x *gtx) UseGrant(_ context.Context, id string, at time.Time) (bool, error) {
	if _, ok := x.g.grants[id]; ok {
		return false, nil
	}
	x.g.grants[id] = at
	return true, nil
}

func (x *gtx) AddTenant(_ context.Context, id string, at time.Time) error {
	if _, ok := x.g.tenants[id]; !ok {
		x.g.tenants[id] = at
	}
	return nil
}

func (x *gtx) Tenants(context.Context) ([]string, error) {
	var out []string
	for t := range x.g.tenants {
		out = append(out, t)
	}
	sort.Strings(out)
	return out, nil
}

func (x *gtx) QueueEmission(_ context.Context, rs, t string) error {
	k := rs + "|" + t
	if _, ok := x.g.emissions[k]; !ok {
		x.g.emissions[k] = &emissionRow{ruleSet: rs, tenant: t}
	}
	return nil
}

func (x *gtx) AckEmission(_ context.Context, rs, t string, at time.Time) (bool, error) {
	e, ok := x.g.emissions[rs+"|"+t]
	if !ok {
		return false, nil
	}
	if !e.acked {
		e.acked, e.ackedAt = true, at
	}
	return true, nil
}

func (x *gtx) EmissionCounts(_ context.Context, rs string) (int, int, error) {
	var total, acked int
	for _, e := range x.g.emissions {
		if e.ruleSet == rs {
			total++
			if e.acked {
				acked++
			}
		}
	}
	return total, acked, nil
}

func (x *gtx) PendingEmissions(_ context.Context, limit int) ([]repository.Emission, error) {
	var out []repository.Emission
	for _, e := range x.g.emissions {
		if !e.acked {
			out = append(out, repository.Emission{RuleSetID: e.ruleSet, TenantID: e.tenant})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RuleSetID+out[i].TenantID < out[j].RuleSetID+out[j].TenantID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (x *gtx) IdemGet(_ context.Context, op, key string) (string, []byte, bool, error) {
	r, ok := x.g.idem[op+"\x00"+key]
	return r.fp, r.resp, ok, nil
}

func (x *gtx) IdemPut(_ context.Context, op, key, fp string, resp []byte, _ time.Time) error {
	x.g.idem[op+"\x00"+key] = idemRow{fp, resp}
	return nil
}

func (x *gtx) Publish(_ context.Context, e events.Envelope) error {
	x.g.keys[e.Key]++
	e.KeySeq = x.g.keys[e.Key]
	x.pub = append(x.pub, e)
	return nil
}

// TenantIDs returns the ids of every tenant that holds rows (for isolation tests).
func (s *Store) TenantIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for t := range s.tenants {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// DocCount returns how many documents of kind the tenant holds (test helper).
func (s *Store) DocCount(tenantID, kind string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	if t := s.tenants[tenantID]; t != nil {
		for k := range t.docs {
			if strings.HasPrefix(k, kind+"\x00") {
				n++
			}
		}
	}
	return n
}
