// Package repository holds the persistence interfaces of the billing service. The service depends on these
// interfaces only; repository/pg is the Postgres implementation and repository/mem the in-memory one the test
// double runs on.
//
// Two classes of table exist (ADR-0003 section 2, ADR-0014 A1.4 item 2):
//   - tenant-owned tables, reached through Repository.InTx. Every Tx runs for exactly one tenant taken from the
//     tenancy context in ctx; the implementation applies it to the database (row-level security) and adds an
//     explicit tenant predicate to every statement.
//   - service-global tables (the tax rule master, the platform vendor log, used approval grants, the known
//     organisations and the rule-set emission progress), reached through Repository.InGlobalTx under a separate
//     database role, so that the audit row and the change share a transaction.
//
// Projected read models are stored as documents: one row per (kind, id) holding the API representation, with the
// few columns the queries filter and sort on. The facts they derive from are immutable ops, so a document is
// rebuilt by replaying ops; documents hold no money rule of their own.
package repository

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/hostryhq/go-common/emit"
	"github.com/hostryhq/go-common/events"
	gsync "github.com/hostryhq/go-common/sync"
)

// ErrNotFound is returned for a missing row (and for another tenant's).
var ErrNotFound = errors.New("repository: not found")

// Document kinds. The string is stored.
const (
	KindBill        = "bill"
	KindCreditNote  = "credit_note"
	KindPayment     = "payment"
	KindRefund      = "refund"
	KindShift       = "shift"
	KindBusinessDay = "business_day"
	KindRevision    = "revision"
	KindLatePending = "late_pending"
	KindInternal    = "internal_consumption"
	KindSeries      = "register_series"
	KindGap         = "register_gap"
	KindException   = "exception"
	KindDiscount    = "discount_row"
	KindAggDay      = "agg_day"
	KindAggTax      = "agg_tax"
	KindAggTender   = "agg_tender"
	KindAggItem     = "agg_item"
	KindAggCategory = "agg_category"
	KindAggStaff    = "agg_staff"
	KindAggHourly   = "agg_hourly"
	KindTaxProfile  = "outlet_tax_profile"
	KindSettings    = "billing_settings"
	KindTender      = "tender_kind"
	KindRuleSet     = "tax_rule_set"
	KindOutlet      = "outlet"
	// KindFact is the account of one projected document's contribution to its day (service.Fact); KindNumberClaim is
	// the first document to carry a legal number; KindRebill the account of one re-bill (service.RebillRec).
	KindFact        = "late_fact"
	KindNumberClaim = "number_claim"
	KindRebill      = "rebill_group"
)

// Doc is one stored read-model row.
type Doc struct {
	Kind string
	ID   string
	// Outlet is the owning outlet ("" for tenant-level rows such as rule sets).
	Outlet string
	// Date is the business date (or the date the kind is range-queried by), "" when none.
	Date string
	// Sort orders lists (RFC 3339 instant or other sortable text); ties break on ID.
	Sort string
	// K1 and K2 are lookup keys (for example the bill id of a payment, the series key of a gap).
	K1, K2 string
	// Stamp is the (hlc, origin) of the last applied last-writer-wins mutation, "" for immutable facts.
	Stamp string
	Body  json.RawMessage
}

// Query selects documents of one kind. Zero fields do not filter.
type Query struct {
	Kind string
	// Outlets restricts to these outlets; nil means every outlet of the tenant (the caller's scope check
	// has been done by the service). A non-nil empty slice matches nothing.
	Outlets  []string
	DateFrom string // inclusive
	DateTo   string // inclusive
	K1, K2   string
	Sort     string // exact match on Sort (rarely used)
	// After is the id of the last row of the previous page; the page continues after it in list order. An id that
	// does not exist starts from the top.
	After string
	// Limit 0 returns every match (aggregation); otherwise at most Limit rows.
	Limit int
	// Asc lists oldest first (by Sort, then ID); the default is newest first.
	Asc bool
}

// Repository opens transactions.
type Repository interface {
	// InTx runs fn in one transaction for the tenant in ctx. fn's error rolls everything back.
	InTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error
	// InGlobalTx runs fn in one transaction on the service-global tables. When ctx carries a vendor principal
	// the platform audit row is written in the same transaction (go-auth WithPlatformAuditedTx).
	InGlobalTx(ctx context.Context, fn func(ctx context.Context, tx GlobalTx) error) error
	// Ping checks the database for readiness.
	Ping(ctx context.Context) error
}

// Tx is one tenant-scoped transaction.
type Tx interface {
	// PutDoc inserts d if (kind, id) is absent and reports whether it did; an existing row is left untouched.
	PutDoc(ctx context.Context, d Doc) (created bool, err error)
	// SaveDoc inserts or replaces d.
	SaveDoc(ctx context.Context, d Doc) error
	// GetDoc returns ErrNotFound when absent.
	GetDoc(ctx context.Context, kind, id string) (Doc, error)
	DeleteDoc(ctx context.Context, kind, id string) error
	FindDocs(ctx context.Context, q Query) ([]Doc, error)

	// MarkOp records that the op (origin, stream, seq) was applied and reports whether this call was the first.
	MarkOp(ctx context.Context, origin, stream string, seq int64, hash string) (first bool, err error)

	// IdemGet and IdemPut are the Idempotency-Key store, scoped by tenant, operation and key.
	IdemGet(ctx context.Context, op, key string) (fingerprint string, response []byte, found bool, err error)
	IdemPut(ctx context.Context, op, key, fingerprint string, response []byte, now time.Time) error

	// Publish writes e to the outbox in this transaction, assigning key_seq per key.
	Publish(ctx context.Context, e events.Envelope) error

	// SyncStore is the projection engine's store (go-common sync) bound to this transaction, so candidates, the personal
	// data side table (values and salts erasable, commitments kept) and erasures commit with the read models and the inbox row.
	SyncStore() gsync.Store

	// EnqueueEmit adds cloud-origin ops to emit_queue for this tenant in this transaction, so they exist exactly when
	// the state change that caused them commits (go-common emit). An op whose key is already queued is ignored.
	EnqueueEmit(ctx context.Context, now time.Time, ops ...emit.Op) error
}

// GlobalTx is one transaction on the service-global tables.
type GlobalTx interface {
	// Rule-set master (draft, signed, published). Body is the whole master as JSON.
	PutMaster(ctx context.Context, m Master) error
	GetMaster(ctx context.Context, id string) (Master, error)
	ListMasters(ctx context.Context, state, after string, limit int) ([]Master, error)
	DeleteMaster(ctx context.Context, id string) error
	// NextVersion returns max(version)+1 over all masters (1 when none).
	NextVersion(ctx context.Context) (int, error)

	// UseGrant records a vendor approval grant id as used and reports whether this call was the first.
	UseGrant(ctx context.Context, grantID string, at time.Time) (bool, error)

	// Organisations known to billing (from tenancy events), the audience of a rule-set emission.
	AddTenant(ctx context.Context, tenantID string, at time.Time) error
	Tenants(ctx context.Context) ([]string, error)

	// Emission progress of a published rule set: one row per organisation, naming the emit_queue key of its op. The
	// op itself is queued with EnqueueEmit in the same transaction; its acknowledgement is the queue row being sent.
	QueueEmission(ctx context.Context, ruleSetID, tenantID, opKey string) error
	// EmissionCounts counts the organisations a rule set was queued for and those whose op the gateway stored.
	EmissionCounts(ctx context.Context, ruleSetID string) (total, acknowledged int, err error)
	// EnqueueEmit adds cloud-origin ops of one organisation to emit_queue (see Tx.EnqueueEmit).
	EnqueueEmit(ctx context.Context, now time.Time, tenantID string, ops ...emit.Op) error

	// IdemGet and IdemPut serve the vendor routes (no tenant): scoped by operation and key.
	IdemGet(ctx context.Context, op, key string) (fingerprint string, response []byte, found bool, err error)
	IdemPut(ctx context.Context, op, key, fingerprint string, response []byte, now time.Time) error

	// Publish writes a platform event (tenant "platform") to the outbox in this transaction.
	Publish(ctx context.Context, e events.Envelope) error
}

// Master is one rule-set version of the master. Revision counts edits of a draft (the ETag).
type Master struct {
	ID       string
	Version  int
	Revision int
	State    string // draft, signed, published
	Created  time.Time
	Body     json.RawMessage
}

type noAuditKey struct{}

// WithNoAudit marks a vendor request on a directory-class route declared x-vendor-audit: none (counts only, no tenant
// data): its global transaction is plain, with no platform audit row.
func WithNoAudit(ctx context.Context) context.Context {
	return context.WithValue(ctx, noAuditKey{}, true)
}

// NoAudit reports whether ctx was marked by WithNoAudit.
func NoAudit(ctx context.Context) bool { v, _ := ctx.Value(noAuditKey{}).(bool); return v }
