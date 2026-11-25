// Package service holds the business rules of the billing service: the projection of the synced billing entities,
// the tax recompute and the document register, the business day with its Z revisions, the settings, the tax rule
// master and the reports. It has no HTTP and no SQL: it depends on repository interfaces, takes the tenant from the
// tenancy context and the caller's outlet scope from go-auth, and publishes events through the transactional outbox.
//
// billing never allocates, changes or reuses a document number, and never alters or rejects a bill (ADR-0014 R5,
// R6.6): a document that does not verify is recorded as it arrived and flagged.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/apperr"
	"github.com/hostryhq/go-common/clock"
	"github.com/hostryhq/go-common/emit"
	"github.com/hostryhq/go-common/events"
	"github.com/hostryhq/go-common/idempotency"
	"github.com/hostryhq/go-common/ids"
	gsync "github.com/hostryhq/go-common/sync"
	"github.com/hostryhq/go-common/tenancy"

	"github.com/hostryhq/svc-billing/repository"
)

// OpenChecks answers how many checks an outlet has open. It is orders' internal route; billing asks it only when
// the owner changes the numbering mode, never on the trading path.
type OpenChecks interface {
	OpenCheckCount(ctx context.Context, outletID string) (int, error)
}

// Service is the billing business layer.
type Service struct {
	repo    repository.Repository
	clock   clock.Clock
	gen     *ids.Generator
	log     *slog.Logger
	emitter emit.Emitter
	orders  OpenChecks
	grants  GrantVerifier
	newID   func() string
	regOnce sync.Once
	reg     *gsync.Registry
	regErr  error
	lead    time.Duration
	window  time.Duration
}

// Options configure a Service. Zero values pick production defaults.
type Options struct {
	Clock clock.Clock
	IDs   *ids.Generator
	Log   *slog.Logger
	// Emitter is the gateway emit client (go-common emit.Client) for the interactive settings routes; queued emissions
	// (rule sets, Z revisions) go through emit_queue and its worker, not through this field.
	Emitter emit.Emitter
	Orders  OpenChecks
	Grants  GrantVerifier
	// NewID overrides id minting for server-generated ids (the test double serves the ids of the spec examples).
	NewID func() string
	// PublishLead is the lead time between publication and a rule set's effective date (deployment configuration,
	// never a panel setting; ADR-0014 A1.4 item 3). Default 24 hours.
	PublishLead time.Duration
	// RevisionWindow is how long a batch of late documents of one origin must be quiet before it is sealed into
	// an immutable Z revision. Default 30 seconds.
	RevisionWindow time.Duration
}

// New builds a Service over repo.
func New(repo repository.Repository, o Options) *Service {
	s := &Service{repo: repo, clock: o.Clock, gen: o.IDs, log: o.Log, emitter: o.Emitter, orders: o.Orders, grants: o.Grants, newID: o.NewID, lead: o.PublishLead, window: o.RevisionWindow}
	if s.clock == nil {
		s.clock = clock.System
	}
	if s.gen == nil {
		s.gen = ids.NewGenerator(s.clock.Now, nil)
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.emitter == nil {
		s.emitter = unavailableEmitter{}
	}
	if s.lead == 0 {
		s.lead = 24 * time.Hour
	}
	if s.window == 0 {
		s.window = 30 * time.Second
	}
	return s
}

type unavailableEmitter struct{}

func (unavailableEmitter) Emit(context.Context, ids.ID, []emit.Op) ([]emit.Result, error) {
	return nil, emit.NewUnavailable(errors.New("the sync gateway emit is not configured"), 0)
}

// Idem identifies one idempotent request: the operation, the client's key and the request fingerprint.
type Idem struct{ Op, Key, Fingerprint string }

type idemKey struct{}

// WithIdem marks ctx as carrying an Idempotency-Key request; the next transaction replays or records it.
func WithIdem(ctx context.Context, i Idem) context.Context {
	return context.WithValue(ctx, idemKey{}, i)
}

func idemFrom(ctx context.Context) (Idem, bool) { i, ok := ctx.Value(idemKey{}).(Idem); return i, ok }

// run executes fn in one tenant transaction. With an Idempotency-Key in ctx it first claims the key in the same
// transaction: a replay returns the stored result and runs nothing. Errors are not stored.
func run[T any](ctx context.Context, s *Service, fn func(ctx context.Context, tx repository.Tx) (T, error)) (T, error) {
	var out T
	err := s.repo.InTx(ctx, func(ctx context.Context, tx repository.Tx) error {
		im, ok := idemFrom(ctx)
		if !ok {
			v, err := fn(ctx, tx)
			out = v
			return err
		}
		fp, stored, found, err := tx.IdemGet(ctx, im.Op, im.Key)
		if err != nil {
			return err
		}
		if found {
			if fp != im.Fingerprint {
				return idempotency.ErrMismatch
			}
			return json.Unmarshal(stored, &out)
		}
		v, err := fn(ctx, tx)
		if err != nil {
			return err
		}
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		out = v
		return tx.IdemPut(ctx, im.Op, im.Key, im.Fingerprint, b, s.clock.Now())
	})
	var zero T
	if err != nil {
		return zero, err
	}
	return out, nil
}

// runGlobal is run for the vendor routes, on the service-global tables.
func runGlobal[T any](ctx context.Context, s *Service, fn func(ctx context.Context, tx repository.GlobalTx) (T, error)) (T, error) {
	var out T
	err := s.repo.InGlobalTx(ctx, func(ctx context.Context, tx repository.GlobalTx) error {
		im, ok := idemFrom(ctx)
		if !ok {
			v, err := fn(ctx, tx)
			out = v
			return err
		}
		fp, stored, found, err := tx.IdemGet(ctx, im.Op, im.Key)
		if err != nil {
			return err
		}
		if found {
			if fp != im.Fingerprint {
				return idempotency.ErrMismatch
			}
			return json.Unmarshal(stored, &out)
		}
		v, err := fn(ctx, tx)
		if err != nil {
			return err
		}
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		out = v
		return tx.IdemPut(ctx, im.Op, im.Key, im.Fingerprint, b, s.clock.Now())
	})
	var zero T
	if err != nil {
		return zero, err
	}
	return out, nil
}

// ---- errors ----

func notFound() error { return apperr.New(apperr.NotFound, "not_found", "") }

func validation(code, detail string, fields ...apperr.FieldError) error {
	return apperr.New(apperr.Validation, code, detail).WithFields(fields...)
}

func field(name, code string) apperr.FieldError { return apperr.FieldError{Field: name, Code: code} }

func mapNotFound(err error) error {
	if err == repository.ErrNotFound {
		return notFound()
	}
	return err
}

// ---- outlet scope ----

// Scope resolves the outlets a list call covers: the caller's grants for permission, narrowed to selected (the
// X-Outlet-Id header) when given. An outlet outside the caller's grants is a 404, as for another tenant's.
func (s *Service) Scope(ctx context.Context, permission, selected string) ([]string, error) {
	sc, err := auth.ScopeFor(ctx, permission)
	if err != nil {
		return nil, err
	}
	if selected != "" {
		if _, err := ids.Parse(selected); err != nil || !(sc.All() || sc.Contains(selected)) {
			return nil, notFound()
		}
		return []string{selected}, nil
	}
	if sc.All() {
		return nil, nil // every outlet of the tenant
	}
	return sc.IDs(), nil
}

// requireOutlet checks the caller holds permission at the stored resource's outlet (404 otherwise).
func (s *Service) requireOutlet(ctx context.Context, permission, outlet string) error {
	return auth.Require(ctx, permission, outlet)
}

// tenantOf returns the tenant of ctx.
func tenantOf(ctx context.Context) (ids.ID, error) {
	tc, err := tenancy.Require(ctx)
	if err != nil {
		return "", err
	}
	return tc.TenantID, nil
}

// ---- events ----

func (s *Service) newEvent(tenant ids.ID, outlet, typ, key, actor string, data any) (events.Envelope, error) {
	b, err := json.Marshal(data)
	if err != nil {
		return events.Envelope{}, err
	}
	return events.Envelope{ID: s.gen.New(), Type: typ, V: 1, Source: "billing", TenantID: tenant, OutletID: ids.ID(outlet), Key: key,
		OccurredAt: s.clock.Now().UTC().Truncate(time.Millisecond), Actor: actor, Data: b}, nil
}

func (s *Service) publish(ctx context.Context, tx repository.Tx, outlet, typ, key, actor string, data any) error {
	t, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	e, err := s.newEvent(t, outlet, typ, key, actor, data)
	if err != nil {
		return err
	}
	return tx.Publish(ctx, e)
}

// PlatformTenant is the literal tenant of platform-level events (ADR-0011 B6).
const PlatformTenant = "platform"

func (s *Service) publishPlatform(ctx context.Context, tx repository.GlobalTx, typ, key, actor string, data any) error {
	e, err := s.newEvent(PlatformTenant, "", typ, key, actor, data)
	if err != nil {
		return err
	}
	return tx.Publish(ctx, e)
}

// ---- small helpers ----

func nowUTC(s *Service) time.Time { return s.clock.Now().UTC().Truncate(time.Millisecond) }

// istDate returns the IST calendar date of t.
func istDate(t time.Time) string {
	return t.In(time.FixedZone("IST", 5*3600+1800)).Format("2006-01-02")
}

func istHour(t time.Time) int { return t.In(time.FixedZone("IST", 5*3600+1800)).Hour() }

// mintID returns a new time-sortable id for a server-generated row.
func (s *Service) mintID() string {
	if s.newID != nil {
		return s.newID()
	}
	return s.gen.New().String()
}
