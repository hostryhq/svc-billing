package double

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/apperr"
	"github.com/hostryhq/go-common/clock"
	"github.com/hostryhq/go-common/emit"
	"github.com/hostryhq/go-common/ids"
	"github.com/hostryhq/go-common/tenancy"

	"github.com/hostryhq/svc-billing/internal/seed"
	"github.com/hostryhq/svc-billing/service"
)

// noAudit accepts every vendor audit record; the double keeps no vendor log (the real service's tests cover it).
type noAudit struct{}

func (noAudit) RecordInTx(context.Context, tenancy.Execer, auth.VendorAccessEntry) error { return nil }
func (noAudit) RecordRefused(context.Context, auth.VendorAccessEntry) error              { return nil }
func (noAudit) RecordPlatformInTx(context.Context, tenancy.Execer, auth.VendorAccessEntry) error {
	return nil
}
func (noAudit) RecordPlatformRefused(context.Context, auth.VendorAccessEntry) error { return nil }

// NoOpenChecks reports no open check at any outlet.
type NoOpenChecks struct{}

// OpenCheckCount implements service.OpenChecks.
func (NoOpenChecks) OpenCheckCount(context.Context, string) (int, error) { return 0, nil }

// OpenChecksAt reports a fixed number of open checks.
type OpenChecksAt int

// OpenCheckCount implements service.OpenChecks.
func (n OpenChecksAt) OpenCheckCount(context.Context, string) (int, error) { return int(n), nil }

// Gateway is the double's sync gateway: an in-process fake of POST /internal/v1/emit (api-specs openapi/sync/v1, emitOps)
// with the rules billing depends on. It stores every op once per (tenant, key) (a replay answers the original place with
// replayed true, the same key with other content is refused 422 idempotency.mismatch), refuses an intent billing does not
// own, an entity or kind the intent does not list and a tenant stream it has not seen, stamps origin cloud-1 and a
// sequence, and loops each stored op straight back into the projection, as the bus would. It implements emit.Emitter (for
// the service and the emit_queue drain) and http.Handler (for emit.Client in the real-service tests).
//
// Failure injection: Fail makes every call answer as an unreachable gateway (503 emit.unavailable, nothing stored).
type Gateway struct {
	svc   *service.Service
	clock clock.Clock
	mu    sync.Mutex
	seq   int64
	ops   []GatewayOp
	byKey map[string]int
	// Fail, when set, is returned by Emit (use emit.NewUnavailable for an outage).
	Fail error
}

// GatewayOp is one op the gateway stored.
type GatewayOp struct {
	Tenant string
	Op     emit.Op
	Result emit.Result
}

// NewGateway returns a gateway that loops stored ops back into a service set later with SetService (the real-service
// integration tests wire it to the Postgres-backed service).
func NewGateway(clk clock.Clock) *Gateway { return &Gateway{clock: clk, byKey: map[string]int{}} }

// SetService sets the service the stored ops are projected into.
func (g *Gateway) SetService(svc *service.Service) { g.svc = svc }

// Reset forgets the stored ops and any injected failure.
func (g *Gateway) Reset() {
	g.mu.Lock()
	g.seq, g.ops, g.byKey, g.Fail = 0, nil, map[string]int{}, nil
	g.mu.Unlock()
}

// Ops returns the ops stored so far, in order.
func (g *Gateway) Ops() []GatewayOp {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]GatewayOp(nil), g.ops...)
}

// allowed is what billing's cloud-origin intents list (api-specs sync/intents/cloud.yaml): entity to kinds, and the stream
// kind each uses.
var allowed = map[string]struct {
	entities map[string][]string
	stream   string
}{
	"tax.rule_set.publish":        {map[string][]string{"tax_rule_set": {"insert"}, "tax_rule": {"insert"}}, "tenant"},
	"settings.tax_profile.set":    {map[string][]string{"outlet_tax_profile": {"insert", "set"}}, "outlet"},
	"settings.numbering.set":      {map[string][]string{"billing_settings": {"insert", "set"}}, "outlet"},
	"settings.discount_limit.set": {map[string][]string{"billing_settings": {"set"}}, "outlet"},
	"day.revision.record":         {map[string][]string{"business_day_revision": {"insert"}}, "outlet"},
}

func contains(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

func (g *Gateway) check(o emit.Op) error {
	if err := o.Validate(); err != nil {
		return &emit.RejectedError{Status: http.StatusUnprocessableEntity, Code: "validation"}
	}
	a, ok := allowed[o.Intent]
	if !ok {
		return &emit.RejectedError{Status: http.StatusForbidden, Code: "emit.intent_not_allowed"}
	}
	if !strings.HasPrefix(o.Stream, a.stream+":") {
		return &emit.RejectedError{Status: http.StatusUnprocessableEntity, Code: "emit.stream_not_allowed"}
	}
	for _, m := range o.Muts {
		if !contains(a.entities[m.E], m.K) {
			return &emit.RejectedError{Status: http.StatusUnprocessableEntity, Code: "emit.entity_not_allowed"}
		}
	}
	return nil
}

// Emit implements emit.Emitter.
func (g *Gateway) Emit(ctx context.Context, tenant ids.ID, ops []emit.Op) ([]emit.Result, error) {
	g.mu.Lock()
	if g.Fail != nil {
		err := g.Fail
		g.mu.Unlock()
		return nil, err
	}
	if tenant == "" || len(ops) == 0 || len(ops) > 200 {
		g.mu.Unlock()
		return nil, &emit.RejectedError{Status: http.StatusUnprocessableEntity, Code: "validation"}
	}
	// the batch is atomic: check everything before storing anything
	for _, o := range ops {
		if err := g.check(o); err != nil {
			g.mu.Unlock()
			return nil, err
		}
		if i, ok := g.byKey[tenant.String()+"|"+o.Key]; ok && !sameOp(g.ops[i].Op, o) {
			g.mu.Unlock()
			return nil, &emit.RejectedError{Status: http.StatusUnprocessableEntity, Code: "idempotency.mismatch"}
		}
	}
	res := make([]emit.Result, len(ops))
	var fresh []emit.Result
	var freshOps []emit.Op
	for i, o := range ops {
		k := tenant.String() + "|" + o.Key
		if j, ok := g.byKey[k]; ok {
			r := g.ops[j].Result
			r.Replayed = true
			res[i] = r
			continue
		}
		g.seq++
		r := emit.Result{Key: o.Key, Origin: "cloud-1", Stream: o.Stream, Seq: 730 + g.seq}
		g.ops = append(g.ops, GatewayOp{Tenant: tenant.String(), Op: o, Result: r})
		g.byKey[k] = len(g.ops) - 1
		res[i] = r
		fresh, freshOps = append(fresh, r), append(freshOps, o)
	}
	now := g.clock.Now()
	g.mu.Unlock()
	if g.svc == nil {
		return res, nil
	}
	tctx := tenancy.With(ctx, tenancy.Context{TenantID: tenant})
	for i, r := range fresh {
		o := freshOps[i]
		so := service.Op{Tenant: tenant.String(), Origin: r.Origin, Stream: r.Stream, Seq: r.Seq, Hash: o.Key, HLC: fmt.Sprintf("%016x", now.UnixMilli()<<16),
			Intent: o.Intent, At: now, Actor: o.Actor, Muts: o.Muts}
		if rest, ok := strings.CutPrefix(r.Stream, "outlet:"); ok {
			so.Outlet = rest
		}
		if err := g.svc.Project(tctx, so); err != nil {
			return nil, err
		}
	}
	return res, nil
}

func sameOp(a, b emit.Op) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// ServeHTTP implements POST /internal/v1/emit as the gateway answers it (a service token is required; a refusal is a
// problem with the gateway's code; the unreachable case is the Fail switch).
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != emit.Path {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get(emit.ServiceTokenHeader) == "" {
		problem(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req struct {
		TenantID ids.ID    `json:"tenant_id"`
		Ops      []emit.Op `json:"ops"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		problem(w, http.StatusUnprocessableEntity, "validation")
		return
	}
	res, err := g.Emit(r.Context(), req.TenantID, req.Ops)
	var rej *emit.RejectedError
	switch {
	case errors.As(err, &rej):
		problem(w, rej.Status, rej.Code)
	case err != nil:
		w.Header().Set("Retry-After", "1")
		problem(w, http.StatusServiceUnavailable, "emit.unavailable")
	default:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ops": res})
	}
}

func problem(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "urn:hostry:problem:" + code, "status": status, "code": code})
}

// Grants is the double's vendor approval grant verifier: the signature check behind service.GrantVerifier, which it
// replaces with a table. It knows the example grant of the contract (api-specs common/vendor.yaml, a token for another
// kind) as the publish grant of the rule-set-signed world, and any grant created with Issue. Every claim check after the
// signature is the real service's own.
type Grants struct {
	mu     sync.Mutex
	issued map[string]service.Grant
}

// ExampleGrant is the raw grant the contract examples send.
func ExampleGrant() string { return seed.ExampleGrantToken() }

// Issue registers a grant under raw and returns raw.
func (g *Grants) Issue(raw string, gr service.Grant) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.issued == nil {
		g.issued = map[string]service.Grant{}
	}
	g.issued[raw] = gr
	return raw
}

// Verify implements service.GrantVerifier.
func (g *Grants) Verify(_ context.Context, raw string) (service.Grant, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if gr, ok := g.issued[raw]; ok {
		return gr, nil
	}
	if raw == ExampleGrant() {
		return seed.ExamplePublishGrant(), nil
	}
	return service.Grant{}, service.ErrGrantInvalid
}

var _ = apperr.Internal
