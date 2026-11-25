// Package pg is the Postgres implementation of repository.Repository. Every tenant transaction is opened through
// go-auth's audited transaction (which uses go-common's tenancy helper), so the tenant context is applied to the
// transaction, and only to it, before any statement; every statement also carries an explicit tenant predicate. The
// service-global tables are reached through the billing_global pool, with the platform vendor audit row in the same
// transaction. SQL is parameterised; nothing is built from request data.
package pg

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/emit"
	"github.com/hostryhq/go-common/events"
	"github.com/hostryhq/go-common/ids"
	"github.com/hostryhq/go-common/outbox"
	gsync "github.com/hostryhq/go-common/sync"
	"github.com/hostryhq/go-common/tenancy"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/hostryhq/svc-billing/repository"
)

// Repo is the Postgres repository. app runs as billing_app, global as billing_global.
type Repo struct {
	app    *sql.DB
	global *sql.DB
}

// New wraps the two pools. Both must have passed VerifyRuntimeRole.
func New(app, global *sql.DB) *Repo { return &Repo{app: app, global: global} }

var _ repository.Repository = (*Repo)(nil)

// Open opens a pgx-backed pool. Every connection is reset (RESET ALL) when it returns to the pool, so a session
// setting such as app.tenant_id can never leak to the next request; the tenant context is only set per transaction.
func Open(dsn string, maxConns int) (*sql.DB, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	db := stdlib.OpenDB(*cfg, stdlib.OptionResetSession(func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "RESET ALL")
		return err
	}))
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	db.SetConnMaxLifetime(30 * time.Minute)
	return db, nil
}

// VerifyRuntimeRole refuses a database role that could defeat row-level security: superuser, BYPASSRLS, or the owner
// of any table. It is run for every runtime pool at start-up.
func VerifyRuntimeRole(ctx context.Context, db *sql.DB) error {
	var name string
	var super, bypass bool
	if err := db.QueryRowContext(ctx, `SELECT current_user, r.rolsuper, r.rolbypassrls FROM pg_roles r WHERE r.rolname = current_user`).Scan(&name, &super, &bypass); err != nil {
		return err
	}
	if super || bypass {
		return errors.New("pg: runtime role " + name + " is superuser or BYPASSRLS; refusing to start")
	}
	var owned int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tableowner = current_user`).Scan(&owned); err != nil {
		return err
	}
	if owned > 0 {
		return errors.New("pg: runtime role " + name + " owns tables; refusing to start")
	}
	return nil
}

// InTx implements repository.Repository.
func (r *Repo) InTx(ctx context.Context, fn func(ctx context.Context, tx repository.Tx) error) error {
	tc, err := tenancy.Require(ctx)
	if err != nil {
		return err
	}
	return auth.WithAuditedTx(ctx, r.app, func(ctx context.Context, tx *sql.Tx) error {
		return fn(ctx, &pgTx{tx: tx, t: tc.TenantID})
	})
}

// InSQLTx runs fn on an existing transaction (the inbox's) with the tenant context applied: the bus consumer applies
// an op and records it as processed in one commit.
func (r *Repo) InSQLTx(ctx context.Context, sqlTx *sql.Tx, tenant ids.ID, fn func(ctx context.Context, tx repository.Tx) error) error {
	if err := tenancy.SetDBContext(ctx, sqlTx, tenant); err != nil {
		return err
	}
	return fn(ctx, &pgTx{tx: sqlTx, t: tenant})
}

// InGlobalTx implements repository.Repository. A vendor principal's request writes its platform audit row in the same
// transaction (go-auth WithPlatformAuditedTx); other callers (the emission worker, tenancy events) run plainly.
func (r *Repo) InGlobalTx(ctx context.Context, fn func(ctx context.Context, tx repository.GlobalTx) error) error {
	p, ok := auth.PrincipalFrom(ctx)
	run := func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "SELECT set_config('app.tenant_id', 'platform', true)"); err != nil {
			return err
		}
		return fn(ctx, &gTx{tx: tx})
	}
	if ok && p.IsVendorActor() && !repository.NoAudit(ctx) {
		return auth.WithPlatformAuditedTx(ctx, r.global, run)
	}
	tx, err := r.global.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := run(ctx, tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Ping implements repository.Repository.
func (r *Repo) Ping(ctx context.Context) error {
	if err := r.app.PingContext(ctx); err != nil {
		return err
	}
	return r.global.PingContext(ctx)
}

// SyncStore implements repository.Tx: the engine's Postgres store on this transaction.
func (x *pgTx) SyncStore() gsync.Store { return gsync.PGStore{DB: x.tx} }

func (x *pgTx) PutPII(ctx context.Context, w gsync.PIIWrite) error {
	return gsync.PGStore{DB: x.tx}.PutPII(ctx, w)
}

func (x *pgTx) ErasePII(ctx context.Context, op gsync.OpRef) (int64, error) {
	res, err := x.tx.ExecContext(ctx, `DELETE FROM sync_pii WHERE tenant_id = $1 AND origin = $2 AND stream = $3 AND seq = $4`, x.t.String(), op.Origin, op.Stream, op.Seq)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (x *pgTx) EnqueueEmit(ctx context.Context, now time.Time, ops ...emit.Op) error {
	return emit.Enqueue(ctx, x.tx, now, x.t, ops...)
}

type pgTx struct {
	tx *sql.Tx
	t  ids.ID
}

func (x *pgTx) PutDoc(ctx context.Context, d repository.Doc) (bool, error) {
	res, err := x.tx.ExecContext(ctx, `INSERT INTO docs (tenant_id, kind, id, outlet, date, sort, k1, k2, stamp, body) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (tenant_id, kind, id) DO NOTHING`, x.t, d.Kind, d.ID, d.Outlet, d.Date, d.Sort, d.K1, d.K2, d.Stamp, []byte(d.Body))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (x *pgTx) SaveDoc(ctx context.Context, d repository.Doc) error {
	_, err := x.tx.ExecContext(ctx, `INSERT INTO docs (tenant_id, kind, id, outlet, date, sort, k1, k2, stamp, body) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (tenant_id, kind, id) DO UPDATE SET outlet = $4, date = $5, sort = $6, k1 = $7, k2 = $8, stamp = $9, body = $10`,
		x.t, d.Kind, d.ID, d.Outlet, d.Date, d.Sort, d.K1, d.K2, d.Stamp, []byte(d.Body))
	return err
}

func (x *pgTx) GetDoc(ctx context.Context, kind, id string) (repository.Doc, error) {
	d := repository.Doc{Kind: kind, ID: id}
	var body []byte
	err := x.tx.QueryRowContext(ctx, `SELECT outlet, date, sort, k1, k2, stamp, body FROM docs WHERE tenant_id = $1 AND kind = $2 AND id = $3`, x.t, kind, id).
		Scan(&d.Outlet, &d.Date, &d.Sort, &d.K1, &d.K2, &d.Stamp, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return d, repository.ErrNotFound
	}
	d.Body = body
	return d, err
}

func (x *pgTx) DeleteDoc(ctx context.Context, kind, id string) error {
	_, err := x.tx.ExecContext(ctx, `DELETE FROM docs WHERE tenant_id = $1 AND kind = $2 AND id = $3`, x.t, kind, id)
	return err
}

func (x *pgTx) FindDocs(ctx context.Context, q repository.Query) ([]repository.Doc, error) {
	var sb strings.Builder
	args := []any{x.t, q.Kind}
	sb.WriteString(`SELECT id, outlet, date, sort, k1, k2, stamp, body FROM docs WHERE tenant_id = $1 AND kind = $2`)
	add := func(cond string, v any) {
		args = append(args, v)
		sb.WriteString(" AND " + strings.ReplaceAll(cond, "?", "$"+strconv.Itoa(len(args))))
	}
	if q.Outlets != nil {
		args = append(args, q.Outlets)
		sb.WriteString(" AND outlet = ANY($" + strconv.Itoa(len(args)) + ")")
	}
	if q.DateFrom != "" {
		add("date >= ?", q.DateFrom)
	}
	if q.DateTo != "" {
		add("date <= ?", q.DateTo)
	}
	if q.K1 != "" {
		add("k1 = ?", q.K1)
	}
	if q.K2 != "" {
		add("k2 = ?", q.K2)
	}
	if q.Sort != "" {
		add("sort = ?", q.Sort)
	}
	order, cmp := "DESC", "<"
	if q.Asc {
		order, cmp = "ASC", ">"
	}
	if q.After != "" {
		var as string
		if err := x.tx.QueryRowContext(ctx, `SELECT sort FROM docs WHERE tenant_id = $1 AND kind = $2 AND id = $3`, x.t, q.Kind, q.After).Scan(&as); err == nil {
			args = append(args, as, q.After)
			n := strconv.Itoa(len(args))
			sb.WriteString(" AND (sort, id) " + cmp + " ($" + strconv.Itoa(len(args)-1) + ", $" + n + ")")
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	sb.WriteString(" ORDER BY sort " + order + ", id " + order)
	if q.Limit > 0 {
		sb.WriteString(" LIMIT " + strconv.Itoa(q.Limit))
	}
	rows, err := x.tx.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []repository.Doc
	for rows.Next() {
		d := repository.Doc{Kind: q.Kind}
		var body []byte
		if err := rows.Scan(&d.ID, &d.Outlet, &d.Date, &d.Sort, &d.K1, &d.K2, &d.Stamp, &body); err != nil {
			return nil, err
		}
		d.Body = body
		out = append(out, d)
	}
	return out, rows.Err()
}

func (x *pgTx) MarkOp(ctx context.Context, origin, stream string, seq int64, hash string) (bool, error) {
	res, err := x.tx.ExecContext(ctx, `INSERT INTO applied_ops (tenant_id, origin, stream, seq, hash) VALUES ($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, x.t, origin, stream, seq, hash)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (x *pgTx) IdemGet(ctx context.Context, op, key string) (string, []byte, bool, error) {
	var fp string
	var resp []byte
	err := x.tx.QueryRowContext(ctx, `SELECT fingerprint, response FROM idempotency_keys WHERE tenant_id = $1 AND op = $2 AND key = $3`, x.t, op, key).Scan(&fp, &resp)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, false, nil
	}
	return fp, resp, err == nil, err
}

func (x *pgTx) IdemPut(ctx context.Context, op, key, fp string, resp []byte, now time.Time) error {
	_, err := x.tx.ExecContext(ctx, `INSERT INTO idempotency_keys (tenant_id, op, key, fingerprint, response, created_at) VALUES ($1,$2,$3,$4,$5,$6)`, x.t, op, key, fp, resp, now.UTC())
	return err
}

func (x *pgTx) Publish(ctx context.Context, e events.Envelope) error {
	var seq int64
	if err := x.tx.QueryRowContext(ctx, `INSERT INTO event_keys (tenant_id, key, last_seq) VALUES ($1,$2,1)
		ON CONFLICT (tenant_id, key) DO UPDATE SET last_seq = event_keys.last_seq + 1 RETURNING last_seq`, x.t, e.Key).Scan(&seq); err != nil {
		return err
	}
	e.KeySeq = seq
	return outbox.Publish(ctx, x.tx, e)
}

// ---- global ----

type gTx struct{ tx *sql.Tx }

func (x *gTx) PutMaster(ctx context.Context, m repository.Master) error {
	_, err := x.tx.ExecContext(ctx, `INSERT INTO tax_rule_masters (id, version, revision, state, created_at, body) VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (id) DO UPDATE SET revision = $3, state = $4, body = $6`, m.ID, m.Version, m.Revision, m.State, m.Created.UTC(), []byte(m.Body))
	return err
}

func scanMaster(sc interface{ Scan(...any) error }) (repository.Master, error) {
	var m repository.Master
	var body []byte
	err := sc.Scan(&m.ID, &m.Version, &m.Revision, &m.State, &m.Created, &body)
	m.Body = body
	return m, err
}

func (x *gTx) GetMaster(ctx context.Context, id string) (repository.Master, error) {
	m, err := scanMaster(x.tx.QueryRowContext(ctx, `SELECT id, version, revision, state, created_at, body FROM tax_rule_masters WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return m, repository.ErrNotFound
	}
	return m, err
}

func (x *gTx) ListMasters(ctx context.Context, state, after string, limit int) ([]repository.Master, error) {
	q := `SELECT id, version, revision, state, created_at, body FROM tax_rule_masters WHERE ($1 = '' OR state = $1)`
	args := []any{state}
	if after != "" {
		var v int
		if err := x.tx.QueryRowContext(ctx, `SELECT version FROM tax_rule_masters WHERE id = $1`, after).Scan(&v); err == nil {
			args = append(args, v)
			q += ` AND version < $2`
		}
	}
	q += ` ORDER BY version DESC`
	if limit > 0 {
		q += ` LIMIT ` + strconv.Itoa(limit)
	}
	rows, err := x.tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []repository.Master
	for rows.Next() {
		m, err := scanMaster(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (x *gTx) DeleteMaster(ctx context.Context, id string) error {
	_, err := x.tx.ExecContext(ctx, `DELETE FROM tax_rule_masters WHERE id = $1`, id)
	return err
}

func (x *gTx) NextVersion(ctx context.Context) (int, error) {
	var n int
	err := x.tx.QueryRowContext(ctx, `SELECT COALESCE(max(version), 0) + 1 FROM tax_rule_masters`).Scan(&n)
	return n, err
}

func (x *gTx) UseGrant(ctx context.Context, id string, at time.Time) (bool, error) {
	res, err := x.tx.ExecContext(ctx, `INSERT INTO used_grants (grant_id, used_at) VALUES ($1,$2) ON CONFLICT DO NOTHING`, id, at.UTC())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (x *gTx) AddTenant(ctx context.Context, id string, at time.Time) error {
	_, err := x.tx.ExecContext(ctx, `INSERT INTO known_tenants (tenant_id, added_at) VALUES ($1,$2) ON CONFLICT DO NOTHING`, id, at.UTC())
	return err
}

func (x *gTx) Tenants(ctx context.Context) ([]string, error) {
	rows, err := x.tx.QueryContext(ctx, `SELECT tenant_id::text FROM known_tenants ORDER BY tenant_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (x *gTx) QueueEmission(ctx context.Context, rs, t, opKey string) error {
	_, err := x.tx.ExecContext(ctx, `INSERT INTO rule_set_emissions (rule_set_id, tenant_id, op_key) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, rs, t, opKey)
	return err
}

// EmissionCounts counts acknowledged rows through the emit_queue: an op is acknowledged when the worker stored it at the
// gateway (state sent).
func (x *gTx) EmissionCounts(ctx context.Context, rs string) (int, int, error) {
	var total, acked int
	err := x.tx.QueryRowContext(ctx, `SELECT count(*), count(q.id) FROM rule_set_emissions e
		LEFT JOIN emit_queue q ON q.tenant_id = e.tenant_id::text AND q.key = e.op_key AND q.state = 'sent'
		WHERE e.rule_set_id = $1`, rs).Scan(&total, &acked)
	return total, acked, err
}

func (x *gTx) EnqueueEmit(ctx context.Context, now time.Time, tenant string, ops ...emit.Op) error {
	return emit.Enqueue(ctx, x.tx, now, ids.ID(tenant), ops...)
}

func (x *gTx) IdemGet(ctx context.Context, op, key string) (string, []byte, bool, error) {
	var fp string
	var resp []byte
	err := x.tx.QueryRowContext(ctx, `SELECT fingerprint, response FROM global_idempotency_keys WHERE op = $1 AND key = $2`, op, key).Scan(&fp, &resp)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, false, nil
	}
	return fp, resp, err == nil, err
}

func (x *gTx) IdemPut(ctx context.Context, op, key, fp string, resp []byte, now time.Time) error {
	_, err := x.tx.ExecContext(ctx, `INSERT INTO global_idempotency_keys (op, key, fingerprint, response, created_at) VALUES ($1,$2,$3,$4,$5)`, op, key, fp, resp, now.UTC())
	return err
}

func (x *gTx) Publish(ctx context.Context, e events.Envelope) error {
	var seq int64
	if err := x.tx.QueryRowContext(ctx, `INSERT INTO global_event_keys (key, last_seq) VALUES ($1,1)
		ON CONFLICT (key) DO UPDATE SET last_seq = global_event_keys.last_seq + 1 RETURNING last_seq`, e.Key).Scan(&seq); err != nil {
		return err
	}
	e.KeySeq = seq
	return outbox.Publish(ctx, x.tx, e)
}
