// Package consumer is billing's side of the bus (ADR-0011 B5, B10; ADR-0014 A2): the projection of the sync ops that touch
// the entities billing owns (parsed and verified by go-common's sync.ParseOp, applied through the projection engine), and
// the platform facts billing needs: organisations, outlets with their registration and regime, the series tenancy issues
// and closes, and a revoked device's drain statement. The handlers are idempotent: an op is applied once by its identity
// (origin, stream, seq), so redelivery, a restore and a bus reset change nothing (ADR-0011 E3, E5).
package consumer

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/hostryhq/go-common/events"
	"github.com/hostryhq/go-common/inbox"
	gsync "github.com/hostryhq/go-common/sync"
	"github.com/hostryhq/go-common/tenancy"

	"github.com/hostryhq/svc-billing/repository"
	"github.com/hostryhq/svc-billing/repository/pg"
	"github.com/hostryhq/svc-billing/service"
)

// Name is the durable consumer name.
const Name = "billing"

// Handler returns the inbox handler: sync.op.<namespace> events are projected inside the inbox transaction (the effect
// and the dedupe row commit together); the platform facts are small idempotent writes in their own transaction for the
// tenant of the event.
func Handler(repo *pg.Repo, svc *service.Service) inbox.Handler {
	parser := &gsync.Parser{}
	return func(ctx context.Context, tx *sql.Tx, e events.Envelope) error {
		tctx := tenancy.With(ctx, tenancy.Context{TenantID: e.TenantID, ActorID: "billing"})
		switch {
		case strings.HasPrefix(e.Type, "sync.op."):
			op, err := parser.ParseEnvelope(e)
			if err != nil {
				return err // a malformed op is parked by the inbox after its retries; the error never carries the body
			}
			so := service.OpOf(op)
			if !svc.Owns(so) {
				return nil
			}
			return repo.InSQLTx(ctx, tx, e.TenantID, func(ctx context.Context, rtx repository.Tx) error { return svc.ApplyOp(ctx, rtx, so) })
		case e.Type == "sync.pii.erased":
			var d struct {
				Origin string `json:"origin"`
				Stream string `json:"stream"`
				Seq    int64  `json:"seq"`
			}
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return err
			}
			return repo.InSQLTx(ctx, tx, e.TenantID, func(ctx context.Context, rtx repository.Tx) error {
				_, err := rtx.SyncStore().AddErasure(ctx, e.TenantID.String(), gsync.OpRef{Origin: d.Origin, Stream: d.Stream, Seq: d.Seq})
				return err
			})
		case e.Type == "tenancy.organisation.created":
			return svc.RecordOrganisation(ctx, e.TenantID.String())
		case e.Type == "tenancy.outlet.created" || e.Type == "tenancy.outlet.updated":
			var d struct {
				OutletID string `json:"outlet_id"`
				RegID    string `json:"tax_registration_id"`
				Regime   string `json:"tax_regime"`
			}
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return err
			}
			if d.Regime == "" {
				d.Regime = "regular" // absent when the registration is not yet known
			}
			return svc.RecordOutlet(tctx, service.OutletRec{OutletID: d.OutletID, RegistrationID: d.RegID, Regime: d.Regime})
		case e.Type == "tenancy.series.issued":
			var d struct {
				SeriesID string `json:"series_id"`
				Code     string `json:"code"`
				DocType  string `json:"doc_type"`
				RegID    string `json:"tax_registration_id"`
				OutletID string `json:"outlet_id"`
				DeviceID string `json:"device_id"`
			}
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return err
			}
			return svc.RecordSeriesIssued(tctx, service.SeriesIssued{SeriesID: d.SeriesID, Code: d.Code, DocType: d.DocType, RegistrationID: d.RegID, OutletID: d.OutletID, DeviceID: d.DeviceID})
		case e.Type == "tenancy.series.closed":
			var d struct {
				SeriesID string `json:"series_id"`
				Cause    string `json:"cause"`
			}
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return err
			}
			return svc.RecordSeriesClosed(tctx, d.SeriesID, d.Cause)
		case e.Type == "identity.device.drained":
			var d struct {
				Series []struct {
					SeriesID   string `json:"series_id"`
					LastNumber int64  `json:"last_number"`
				} `json:"series"`
			}
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return err
			}
			stmts := make([]service.SeriesStatement, 0, len(d.Series))
			for _, s := range d.Series {
				stmts = append(stmts, service.SeriesStatement{SeriesID: s.SeriesID, LastNumber: s.LastNumber})
			}
			return svc.RecordDrain(tctx, stmts)
		}
		return nil // other events are not billing's
	}
}
