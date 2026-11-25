package double

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/apperr"
	"github.com/hostryhq/go-common/clock"
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

// Emitter is the double's sync gateway emit: it records every emitted op and loops it straight back into the
// projection, as the bus would, so a settings change is visible to the next read.
type Emitter struct {
	svc   *service.Service
	clock clock.Clock
	mu    sync.Mutex
	seq   int64
	ops   []service.EmitOp
	Fail  error // when set, Emit returns it (a gateway outage)
}

// NewEmitter returns an emitter that loops ops back into a service set later with SetService (the real-service
// integration tests wire it to the Postgres-backed service).
func NewEmitter(clk clock.Clock) *Emitter { return &Emitter{clock: clk} }

// SetService sets the service the emitted ops are projected into.
func (e *Emitter) SetService(svc *service.Service) { e.svc = svc }

// Reset forgets the emitted ops.
func (e *Emitter) Reset() { e.mu.Lock(); e.seq, e.ops, e.Fail = 0, nil, nil; e.mu.Unlock() }

// Ops returns the ops emitted so far.
func (e *Emitter) Ops() []service.EmitOp {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]service.EmitOp(nil), e.ops...)
}

// Emit implements service.Emitter. The example op id is "cloud-1/outlet:<first 8 of the outlet id>/731".
func (e *Emitter) Emit(ctx context.Context, op service.EmitOp) (string, error) {
	e.mu.Lock()
	if e.Fail != nil {
		err := e.Fail
		e.mu.Unlock()
		return "", err
	}
	for _, o := range e.ops {
		if o.OpID == op.OpID {
			e.mu.Unlock()
			return opIdent(o, e.seqOf(o)), nil // a retry is never duplicated
		}
	}
	e.seq++
	n := 730 + e.seq
	e.ops = append(e.ops, op)
	e.mu.Unlock()
	pid := opIdent(op, n)
	tctx := tenancy.With(ctx, tenancy.Context{TenantID: ids.ID(op.TenantID)})
	err := e.svc.Project(tctx, service.Op{Tenant: op.TenantID, Origin: "cloud-1", Stream: op.Stream, Seq: n, Hash: op.OpID, HLC: fmt.Sprintf("%020d", e.clock.Now().UnixMilli()),
		Intent: op.Intent, At: e.clock.Now(), Muts: op.Muts})
	return pid, err
}

func (e *Emitter) seqOf(o service.EmitOp) int64 {
	for i, x := range e.ops {
		if x.OpID == o.OpID {
			return 731 + int64(i)
		}
	}
	return 0
}

func opIdent(o service.EmitOp, n int64) string {
	stream := o.Stream
	if len(stream) > len("outlet:")+8 {
		stream = stream[:len("outlet:")+8]
	}
	return fmt.Sprintf("cloud-1/%s/%d", stream, n)
}

// Grants is the double's vendor approval grant verifier. It knows the example grant of the contract
// ("grant_example_0190a1b2", for rule set 7042) and any grant created with Issue.
type Grants struct {
	mu     sync.Mutex
	issued map[string]service.Grant
}

// ExampleGrant is the raw grant the contract examples send.
const ExampleGrant = "grant_example_0190a1b2"

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
		if !gr.ExpiresAt.IsZero() && time.Now().After(gr.ExpiresAt) && gr.ExpiresAt.Before(seed.At) {
			return gr, service.ErrGrantExpired
		}
		return gr, nil
	}
	if raw == ExampleGrant {
		return service.Grant{ID: "grant-example-1", ApprovalID: "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f90f1", Kind: "tax_rule_set_publish", Requester: "vendor-user-2",
			Approver: "vendor-user-1", SubjectHash: seed.PublishGrantSubject()}, nil
	}
	return service.Grant{}, service.ErrGrantInvalid
}

var _ = apperr.Internal
