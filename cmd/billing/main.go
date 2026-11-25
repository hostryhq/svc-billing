// Command billing is the billing service: the projection of bills, credit notes and payments, the tax recompute, the
// document register, business days with their Z revisions, the tax rule master and the reports. Wiring only.
//
//	billing serve     (default) run the HTTP API, the outbox relay, the bus consumer and the background workers
//	billing migrate   apply the SQL migrations as the migrator role (separate step; never at serve time)
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/bus"
	"github.com/hostryhq/go-common/emit"
	"github.com/hostryhq/go-common/ids"
	"github.com/hostryhq/go-common/inbox"
	"github.com/hostryhq/go-common/logx"
	"github.com/hostryhq/go-common/outbox"

	"github.com/hostryhq/svc-billing/internal/app"
	"github.com/hostryhq/svc-billing/internal/config"
	"github.com/hostryhq/svc-billing/internal/consumer"
	"github.com/hostryhq/svc-billing/internal/ordersclient"
	"github.com/hostryhq/svc-billing/repository/migrations"
	"github.com/hostryhq/svc-billing/repository/pg"
	"github.com/hostryhq/svc-billing/service"
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	cfg, err := config.Load(os.LookupEnv)
	if cmd == "migrate" {
		err = nil
		if cfg.MigrateDSN == "" {
			err = errors.New("BILLING_DB_MIGRATE_DSN is required for migrate")
		}
	}
	log := logx.New(os.Stderr, slog.LevelInfo, "billing")
	if err != nil {
		log.Error("invalid configuration", "error", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	switch cmd {
	case "migrate":
		err = migrate(ctx, cfg)
	case "serve":
		err = serve(ctx, cfg, log)
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		log.Error("exit", "error", err)
		os.Exit(1)
	}
}

func migrate(ctx context.Context, cfg config.Config) error {
	db, err := pg.Open(cfg.MigrateDSN, 1)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	return migrations.Apply(ctx, db)
}

// keyLoader loads a JWKS document from a URL or a file path.
type keyLoader struct{ src string }

func (k keyLoader) Load(ctx context.Context) (auth.KeyDoc, error) {
	var b []byte
	if len(k.src) > 8 && (k.src[:7] == "http://" || k.src[:8] == "https://") {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.src, nil)
		if err != nil {
			return auth.KeyDoc{}, err
		}
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			return auth.KeyDoc{}, err
		}
		defer func() { _ = resp.Body.Close() }()
		if b, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20)); err != nil {
			return auth.KeyDoc{}, err
		}
	} else {
		var err error
		if b, err = os.ReadFile(k.src); err != nil {
			return auth.KeyDoc{}, err
		}
	}
	keys, err := auth.ParseJWKS(b)
	return auth.KeyDoc{Keys: keys}, err
}

func loadSigner(cfg config.Config) (*auth.ServiceSigner, error) {
	b, err := os.ReadFile(cfg.ServiceKeyFile)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("service key file holds no PEM block")
	}
	k, err := x509.ParseECPrivateKey(blk.Bytes)
	if err != nil {
		pk, perr := x509.ParsePKCS8PrivateKey(blk.Bytes)
		var ok bool
		if k, ok = pk.(*ecdsa.PrivateKey); perr != nil || !ok {
			return nil, errors.New("service key is not an EC private key")
		}
	}
	return &auth.ServiceSigner{Name: cfg.SelfName, Key: k}, nil
}

func serve(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	appDB, err := pg.Open(cfg.AppDSN, 20)
	if err != nil {
		return err
	}
	defer func() { _ = appDB.Close() }()
	globalDB, err := pg.Open(cfg.GlobalDSN, 4)
	if err != nil {
		return err
	}
	defer func() { _ = globalDB.Close() }()
	for name, db := range map[string]*sql.DB{"app": appDB, "global": globalDB} {
		if err := pg.VerifyRuntimeRole(ctx, db); err != nil {
			return fmt.Errorf("%s pool: %w", name, err)
		}
	}
	tenantKeys := auth.NewCachedKeySet(keyLoader{cfg.TenantJWKS}, log)
	vendorKeys := auth.NewCachedKeySet(keyLoader{cfg.VendorJWKS}, log)
	grantKeys := auth.NewCachedKeySet(keyLoader{cfg.GrantKeys}, log)
	for _, ks := range []*auth.CachedKeySet{tenantKeys, vendorKeys, grantKeys} {
		if err := ks.Refresh(ctx); err != nil {
			return fmt.Errorf("load signing keys: %w", err)
		}
		go ks.Run(ctx, cfg.KeyRefresh)
	}
	svcDoc, err := keyLoader{cfg.ServiceKeys}.Load(ctx)
	if err != nil {
		return fmt.Errorf("load service keys: %w", err)
	}
	var orders service.OpenChecks
	if cfg.OrdersURL != "" {
		signer, err := loadSigner(cfg)
		if err != nil {
			return fmt.Errorf("service signing key: %w", err)
		}
		orders = &ordersclient.Client{BaseURL: cfg.OrdersURL, Signer: signer}
	}
	var emitter emit.Emitter
	if cfg.GatewayURL != "" {
		signer, err := loadSigner(cfg)
		if err != nil {
			return fmt.Errorf("service signing key: %w", err)
		}
		// Emit tokens are short-lived and minted per call for the tenant of the emit: the gateway requires the token's tenant to
		// equal the request tenant (O2-11).
		emitter = &emit.Client{BaseURL: cfg.GatewayURL, Token: func(_ context.Context, tenant ids.ID) (string, error) { return signer.Sign("sync", tenant.String()) }}
	} else {
		log.Warn("gateway disabled: BILLING_GATEWAY_URL not set; settings routes answer 503 emit.unavailable and emissions stay queued")
	}
	a, err := app.Build(app.Options{
		AppDB: appDB, GlobalDB: globalDB, Log: log, Orders: orders, Emitter: emitter, Grants: &auth.GrantVerifier{Keys: grantKeys, Issuer: cfg.GrantIssuer, Audience: service.GrantAudience}, PublishLead: cfg.PublishLead,
		Tenant:   &auth.Verifier{Keys: tenantKeys, Issuer: cfg.TenantIssuer, Audience: cfg.TenantAudience, Realm: auth.RealmTenant},
		Vendor:   &auth.Verifier{Keys: vendorKeys, Issuer: cfg.VendorIssuer, Audience: cfg.VendorAudience, Realm: auth.RealmVendor},
		Services: &auth.ServiceVerifier{Registry: auth.NewStaticKeySet(svcDoc), Self: cfg.SelfName},
	})
	if err != nil {
		return err
	}
	go sweep(ctx, a.Service, log)
	if a.Queue != nil {
		go func() { _ = a.Queue.Run(ctx, 5*time.Second) }()
	}
	if cfg.NatsURL != "" && cfg.RelayDSN != "" {
		go relay(ctx, cfg, log)
		go consume(ctx, cfg, a, appDB, log)
	} else {
		log.Warn("bus disabled: NATS_URL or BILLING_DB_RELAY_DSN not set; events stay in the outbox and nothing is projected")
	}
	srv := &http.Server{Addr: cfg.Addr, Handler: a.Handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("billing listening", "addr", cfg.Addr)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	}
}

// sweep runs the periodic work: sealing quiet batches of late documents into Z revisions (their emission is queued in
// the same transaction and drained by the emit queue worker).
func sweep(ctx context.Context, svc *service.Service, log *slog.Logger) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := svc.SealRevisions(ctx); err != nil {
				log.Error("revision sweep failed", "error", err)
			} else if n > 0 {
				log.Info("Z revisions sealed", "count", n)
			}
		}
	}
}

func relay(ctx context.Context, cfg config.Config, log *slog.Logger) {
	db, err := pg.Open(cfg.RelayDSN, 2)
	if err != nil {
		log.Error("relay: open database", "error", err)
		return
	}
	defer func() { _ = db.Close() }()
	nc, err := bus.Connect(cfg.NatsURL, "billing", cfg.NatsSeedFile)
	if err != nil {
		log.Error("relay: connect to the bus", "error", err)
		return
	}
	defer nc.Close()
	pub, err := bus.NewPublisher(nc)
	if err != nil {
		log.Error("relay: publisher", "error", err)
		return
	}
	r := &outbox.Relay{DB: db, Sink: pub, LockKey: 7_464_012_003, Log: log}
	if err := r.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("relay stopped", "error", err)
	}
}

// consume runs the durable consumers of the publishers billing subscribes to: sync (op projection, pii erasure), tenancy
// (organisations, outlets, series) and identity (a revoked device's drain statement).
func consume(ctx context.Context, cfg config.Config, a *app.App, db *sql.DB, log *slog.Logger) {
	nc, err := bus.Connect(cfg.NatsURL, "billing", cfg.NatsSeedFile)
	if err != nil {
		log.Error("consumer: connect to the bus", "error", err)
		return
	}
	defer nc.Close()
	for _, publisher := range []string{"sync", "tenancy", "identity"} {
		c, err := bus.BindConsumer(ctx, nc, publisher, "billing")
		if err != nil {
			log.Error("consumer: bind", "publisher", publisher, "error", err)
			continue
		}
		sub := &inbox.Subscriber{DB: db, Consumer: consumer.Name + "." + publisher, Handle: consumer.Handler(a.Repo, a.Service), Log: log,
			OnPark: func(c, id, reason string) { log.Error("event parked", "consumer", c, "event_id", id, "reason", reason) }}
		go func() { _ = sub.Run(ctx, c.Source(5*time.Second)) }()
	}
	<-ctx.Done()
}
