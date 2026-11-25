// Package double is the in-memory test double of the billing service. It runs the real API layer and the real
// service over an in-memory repository, so it implements the whole public contract (projection of ops, the
// recompute, the register, business days and revisions, settings, the rule-set master and its publication,
// idempotency, outlet scoping), with deterministic ids and time. Other services' tests import it instead of the real
// service, feed it ops with ApplyOp, and read what it published with Events. It is verified by the shared contract
// suite (api-specs/contracttest) so it cannot drift.
//
// Authentication uses the same go-auth verifiers as the real service: give it the test issuers of go-auth/authtest.
// Failure injection (error status, delay, hang) lets callers test their resilience.
package double

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/clock"
	"github.com/hostryhq/go-common/events"
	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-billing/api"
	"github.com/hostryhq/svc-billing/internal/gen"
	"github.com/hostryhq/svc-billing/internal/seed"
	"github.com/hostryhq/svc-billing/repository/mem"
	"github.com/hostryhq/svc-billing/service"
)

// Config configures the double. Tenant, Vendor and Services are required; the rest has defaults.
type Config struct {
	Tenant   *auth.Verifier        // tenant-realm tokens (authtest.NewTenantIssuer(...).Verifier())
	Vendor   *auth.Verifier        // vendor-realm tokens
	Services *auth.ServiceVerifier // service tokens (self must be "billing")
	// Clock defaults to a fixed instant, the instant of the spec examples.
	Clock clock.Clock
	// IDs defaults to a generator driven by Clock with a counter, so ids are deterministic.
	IDs *ids.Generator
	// OpenChecks answers the open-check count of an outlet (default: none open).
	OpenChecks service.OpenChecks
	// Grants verifies vendor approval grants (default: Grants, which knows the example grant).
	Grants service.GrantVerifier
}

// Fault is an injected failure for matching requests.
type Fault struct {
	Match  func(*http.Request) bool
	Status int           // respond with this status and a problem body (0: pass through)
	Delay  time.Duration // sleep before answering
	Hang   bool          // never answer until the request is cancelled (a timeout for the caller)
}

// Double is the running double.
type Double struct {
	Store   *mem.Store
	Svc     *service.Service
	Emitted *Emitter
	idq     *idQueue
	h       http.Handler
	mu      sync.Mutex
	fault   []Fault
}

// New builds a double. The store is empty; call SeedWorld to match the spec examples.
func New(cfg Config) (*Double, error) {
	cat, err := gen.Catalogue()
	if err != nil {
		return nil, err
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.Func(func() time.Time { return seed.At })
	}
	if cfg.IDs == nil {
		cfg.IDs = ids.NewGenerator(cfg.Clock.Now, &counter{})
	}
	if cfg.OpenChecks == nil {
		cfg.OpenChecks = NoOpenChecks{}
	}
	if cfg.Grants == nil {
		cfg.Grants = &Grants{}
	}
	store := mem.New()
	d := &Double{Store: store, idq: newIDQueue()}
	d.Emitted = &Emitter{clock: cfg.Clock}
	d.Svc = service.New(store, service.Options{Clock: cfg.Clock, IDs: cfg.IDs, Log: slog.New(slog.DiscardHandler), Orders: cfg.OpenChecks, Grants: cfg.Grants,
		Emitter: d.Emitted, NewID: d.idq.next})
	d.Emitted.svc = d.Svc
	svc := d.Svc
	guard := &auth.Guard{Catalogue: cat, Tenant: cfg.Tenant, Vendor: cfg.Vendor, Services: cfg.Services, Audit: noAudit{}, PlatformAudit: noAudit{}, Log: slog.New(slog.DiscardHandler)}
	d.h = api.New(api.Config{Service: svc, Guard: guard, Clock: cfg.Clock, Log: slog.New(slog.DiscardHandler)})
	return d, nil
}

type counter struct{ n byte }

func (c *counter) Read(p []byte) (int, error) {
	for i := range p {
		c.n++
		p[i] = c.n
	}
	return len(p), nil
}

// idQueue serves the server-generated ids of the spec examples in order (the draft 7042, then the revision 7071),
// then falls back to counters.
type idQueue struct {
	mu    sync.Mutex
	queue []string
	n     int
}

func newIDQueue() *idQueue { q := &idQueue{}; q.reset(); return q }

func (q *idQueue) reset() {
	q.queue = []string{"0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f7042", "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f7071"}
	q.n = 0
}

func (q *idQueue) next() string {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.queue) > 0 {
		id := q.queue[0]
		q.queue = q.queue[1:]
		return id
	}
	q.n++
	return fmt.Sprintf("0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f9%03d", q.n)
}

// SeedWorld empties the double and stores the named example world of the OpenAPI document
// (info.x-example-worlds). It is the SeedWorld hook of the shared contract suite.
func (d *Double) SeedWorld(ctx context.Context, world string) error {
	d.Store.Reset()
	d.Emitted.Reset()
	d.idq.mu.Lock()
	d.idq.reset()
	d.idq.mu.Unlock()
	return seed.Apply(ctx, d.Store, world)
}

// Events returns the events the double published to its outbox.
func (d *Double) Events() []events.Envelope { return d.Store.Events() }

// Inject adds a failure for matching requests; it returns a function that removes it.
func (d *Double) Inject(f Fault) (remove func()) {
	d.mu.Lock()
	d.fault = append(d.fault, f)
	i := len(d.fault) - 1
	d.mu.Unlock()
	return func() {
		d.mu.Lock()
		d.fault[i].Match = nil
		d.mu.Unlock()
	}
}

// ServeHTTP implements http.Handler.
func (d *Double) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	var hit *Fault
	for i := range d.fault {
		if d.fault[i].Match != nil && d.fault[i].Match(r) {
			f := d.fault[i]
			hit = &f
			break
		}
	}
	d.mu.Unlock()
	if hit != nil {
		if hit.Hang {
			<-r.Context().Done()
			return
		}
		if hit.Delay > 0 {
			select {
			case <-time.After(hit.Delay):
			case <-r.Context().Done():
				return
			}
		}
		if hit.Status != 0 {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(hit.Status)
			_, _ = fmt.Fprintf(w, `{"type":"urn:hostry:problem:injected","title":"Injected failure","status":%d,"code":"injected"}`, hit.Status)
			return
		}
	}
	d.h.ServeHTTP(w, r)
}

// Server starts the double on an httptest server.
func (d *Double) Server() *httptest.Server { return httptest.NewServer(d) }
