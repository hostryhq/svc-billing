package pg

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"regexp"
	"strings"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/ids"
	"github.com/hostryhq/go-common/tenancy"
)

// AuditSink writes the tenant-scoped vendor_access_log row (a support session reading an organisation's billing data)
// in the transaction of the action (ADR-0013 V5). If the row cannot be written the request fails and nothing changes.
type AuditSink struct {
	DB  *sql.DB // the application pool
	Log *slog.Logger
}

var _ auth.VendorAuditSink = (*AuditSink)(nil)

func tokenType(t auth.TokenType) string {
	if t == auth.TypeSupport {
		return "support"
	}
	return "vendor"
}

var camel = regexp.MustCompile(`([a-z0-9])([A-Z])`)

// actionKey turns an operation id into the stable action key pattern ^[a-z]+(\.[a-z_]+)+$.
func actionKey(op, tt string) string {
	s := strings.ToLower(camel.ReplaceAllString(op, "${1}_${2}"))
	s = strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return -1
		}
		return r
	}, s)
	return tt + ".billing." + s
}

func nullStr(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func insertEntry(ctx context.Context, tx tenancy.Execer, tenant ids.ID, e auth.VendorAccessEntry) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO vendor_access_log (tenant_id, id, occurred_at, vendor_user, token_type, support_session_id, action_key, target_id, ticket_reference, outcome, result_count, request_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, tenant, e.ID, e.Time, e.VendorUser, tokenType(e.TokenType), nullStr(e.SupportSession),
		actionKey(e.ActionKey, tokenType(e.TokenType)), nullStr(e.TargetID), nullStr(e.Ticket), e.Outcome, e.ResultCount, e.RequestID)
	return err
}

// RecordInTx implements auth.VendorAuditSink.
func (s *AuditSink) RecordInTx(ctx context.Context, x tenancy.Execer, e auth.VendorAccessEntry) error {
	tid, err := ids.Parse(e.TenantID)
	if err != nil {
		return err
	}
	return insertEntry(ctx, x, tid, e)
}

// RecordRefused implements auth.VendorAuditSink.
func (s *AuditSink) RecordRefused(ctx context.Context, e auth.VendorAccessEntry) error {
	tid, err := ids.Parse(e.TenantID)
	if err != nil {
		s.Log.WarnContext(ctx, "vendor request refused before an organisation was named", "action", e.ActionKey)
		return nil
	}
	e.Outcome = auth.OutcomeRefused
	c := tenancy.With(context.WithoutCancel(ctx), tenancy.Context{TenantID: tid, ActorID: e.VendorUser})
	return tenancy.WithTx(c, s.DB, func(c context.Context, tx *sql.Tx) error { return insertEntry(c, tx, tid, e) })
}

// PlatformAuditSink writes platform_vendor_access_log rows: vendor actions that name no organisation (the rule-set
// routes). The table has no tenant column and no row-level security; the row commits in the transaction of the
// action, and a refused request is recorded in a short transaction of its own.
type PlatformAuditSink struct {
	DB *sql.DB // the global pool
}

var _ auth.PlatformAuditSink = (*PlatformAuditSink)(nil)

func insertPlatformEntry(ctx context.Context, x tenancy.Execer, e auth.VendorAccessEntry) error {
	_, err := x.ExecContext(ctx, `INSERT INTO platform_vendor_access_log (id, occurred_at, vendor_user, token_type, support_session_id, action_key, target_id, ticket_reference, outcome, result_count, request_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, e.ID, e.Time, e.VendorUser, tokenType(e.TokenType), nullStr(e.SupportSession),
		actionKey(e.ActionKey, tokenType(e.TokenType)), nullStr(e.TargetID), nullStr(e.Ticket), e.Outcome, e.ResultCount, e.RequestID)
	return err
}

// RecordPlatformInTx implements auth.PlatformAuditSink.
func (s *PlatformAuditSink) RecordPlatformInTx(ctx context.Context, x tenancy.Execer, e auth.VendorAccessEntry) error {
	return insertPlatformEntry(ctx, x, e)
}

// RecordPlatformRefused implements auth.PlatformAuditSink.
func (s *PlatformAuditSink) RecordPlatformRefused(ctx context.Context, e auth.VendorAccessEntry) error {
	e.Outcome = auth.OutcomeRefused
	tx, err := s.DB.BeginTx(context.WithoutCancel(ctx), nil)
	if err != nil {
		return err
	}
	if err := insertPlatformEntry(ctx, tx, e); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

var _ = errors.New
