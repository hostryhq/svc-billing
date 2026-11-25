// Package consumer is billing's side of the bus (ADR-0011 B5, B10): the projection of the sync ops that touch the
// entities billing owns, and the tenancy facts billing needs (organisations, outlets and their registration). The
// handlers are idempotent: an op is applied once by its identity (origin, stream, seq), so redelivery, a restore and a
// bus reset change nothing (ADR-0011 E3, E5).
package consumer

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/hostryhq/go-common/events"
	"github.com/hostryhq/go-common/inbox"

	"github.com/hostryhq/svc-billing/repository"
	"github.com/hostryhq/svc-billing/repository/pg"
	"github.com/hostryhq/svc-billing/service"
)

// Name is the durable consumer name.
const Name = "billing"

// Handler returns the inbox handler: sync.op.<namespace> events are projected inside the inbox transaction (the effect
// and the dedupe row commit together); tenancy events update billing's own directory tables.
func Handler(repo *pg.Repo, svc *service.Service) inbox.Handler {
	return func(ctx context.Context, tx *sql.Tx, e events.Envelope) error {
		switch {
		case strings.HasPrefix(e.Type, "sync.op."):
			op, err := service.ParseOp(e.TenantID.String(), e.Data)
			if err != nil {
				return err
			}
			if !service.Owns(op) {
				return nil
			}
			return repo.InSQLTx(ctx, tx, e.TenantID, func(ctx context.Context, rtx repository.Tx) error { return svc.ApplyOp(ctx, rtx, op) })
		case e.Type == "tenancy.organisation.created":
			return svc.RecordOrganisation(ctx, e.TenantID.String())
		case e.Type == "tenancy.outlet.created" || e.Type == "tenancy.outlet.updated":
			var d struct {
				OutletID string `json:"outlet_id"`
				RegID    string `json:"tax_registration_id"`
				Regime   string `json:"regime"`
			}
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return err
			}
			if d.Regime == "" {
				d.Regime = "regular" // tenancy's outlet events carry the registration id only; see docs/notes.md
			}
			return svc.RecordOutlet(ctx, service.OutletRec{OutletID: d.OutletID, RegistrationID: d.RegID, Regime: d.Regime})
		}
		return nil // other events are not billing's
	}
}
