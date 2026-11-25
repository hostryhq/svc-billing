// Package app wires the billing service: repository, service, go-auth guard and HTTP handler. The process
// (cmd/billing) and the integration tests build it the same way.
package app

import (
	"database/sql"
	"log/slog"
	"net/http"
	"time"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/clock"
	"github.com/hostryhq/go-common/health"
	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-billing/api"
	"github.com/hostryhq/svc-billing/internal/gen"
	"github.com/hostryhq/svc-billing/repository/pg"
	"github.com/hostryhq/svc-billing/service"
)

// Options are the dependencies of a running service.
type Options struct {
	AppDB    *sql.DB // runtime pool as billing_app
	GlobalDB *sql.DB // pool as billing_global (rule master, platform vendor log)

	Tenant   *auth.Verifier
	Vendor   *auth.Verifier
	Services *auth.ServiceVerifier

	Clock     clock.Clock // entity timestamps and ids
	AuthClock clock.Clock // token checks; defaults to Clock
	IDs       *ids.Generator
	Log       *slog.Logger
	Emitter   service.Emitter
	Orders    service.OpenChecks
	Grants    service.GrantVerifier
	// PublishLead is the rule-set publication lead time (deployment configuration; default 24 h).
	PublishLead time.Duration
}

// App is the wired service.
type App struct {
	Handler http.Handler
	Service *service.Service
	Repo    *pg.Repo
	Health  *health.Handler
}

// Build wires the Postgres-backed service.
func Build(o Options) (*App, error) {
	cat, err := gen.Catalogue()
	if err != nil {
		return nil, err
	}
	if o.Clock == nil {
		o.Clock = clock.System
	}
	if o.AuthClock == nil {
		o.AuthClock = o.Clock
	}
	if o.IDs == nil {
		o.IDs = ids.NewGenerator(o.Clock.Now, nil)
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	repo := pg.New(o.AppDB, o.GlobalDB)
	svc := service.New(repo, service.Options{Clock: o.Clock, IDs: o.IDs, Log: o.Log, Emitter: o.Emitter, Orders: o.Orders, Grants: o.Grants, PublishLead: o.PublishLead})
	guard := &auth.Guard{
		Catalogue: cat, Tenant: o.Tenant, Vendor: o.Vendor, Services: o.Services, Clock: o.AuthClock, Log: o.Log,
		Audit: &pg.AuditSink{DB: o.AppDB, Log: o.Log}, PlatformAudit: &pg.PlatformAuditSink{DB: o.GlobalDB},
	}
	h := health.New(0)
	h.Add("postgres", repo.Ping)
	return &App{Handler: api.New(api.Config{Service: svc, Guard: guard, Clock: o.Clock, Log: o.Log, Health: h}), Service: svc, Repo: repo, Health: h}, nil
}
