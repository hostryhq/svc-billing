// Package api is the controller layer: it authenticates through go-auth, validates the transport (strict JSON,
// Idempotency-Key, path ids), calls the service and maps results to the generated DTOs. It holds no business rule.
// Routes and their permissions are generated from the OpenAPI document (internal/gen.Policies), so a route cannot
// be registered without a declared permission.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/apperr"
	"github.com/hostryhq/go-common/clock"
	"github.com/hostryhq/go-common/health"
	"github.com/hostryhq/go-common/httpx"
	"github.com/hostryhq/go-common/idempotency"

	"github.com/hostryhq/svc-billing/internal/gen"
	"github.com/hostryhq/svc-billing/internal/page"
	"github.com/hostryhq/svc-billing/service"
)

// Config wires the handler.
type Config struct {
	Service *service.Service
	Guard   *auth.Guard
	Clock   clock.Clock
	Log     *slog.Logger
	Health  *health.Handler
}

// Server implements gen.ServerInterface.
type Server struct {
	svc *service.Service
	log *slog.Logger
}

var _ gen.ServerInterface = (*Server)(nil)

// New builds the HTTP handler: request id, panic recovery, generated routes behind the go-auth guard.
func New(cfg Config) http.Handler {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	s := &Server{svc: cfg.Service, log: cfg.Log}
	mux := http.NewServeMux()
	rt := &router{mux: mux, cfg: cfg}
	gen.HandlerWithOptions(s, gen.StdHTTPServerOptions{BaseRouter: rt, ErrorHandlerFunc: badParam})
	if cfg.Health != nil {
		cfg.Health.Register(mux)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		apperr.WriteProblem(w, r, apperr.New(apperr.NotFound, "not_found", ""))
	})
	return httpx.Chain(mux, httpx.RequestID, httpx.Recover)
}

type router struct {
	mux *http.ServeMux
	cfg Config
}

func (rt *router) ServeHTTP(w http.ResponseWriter, r *http.Request) { rt.mux.ServeHTTP(w, r) }

func (rt *router) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	p, ok := gen.Policies[pattern]
	if !ok {
		panic("api: no policy generated for " + pattern)
	}
	pol := auth.Policy{OperationID: p.OperationID, Permission: p.Permission, StepUp: p.StepUp, Internal: p.Internal, ServiceOnly: p.ServiceOnly}
	if p.Permission == "authenticated" {
		pol.Permission, pol.Authenticated = "", true
	}
	pol.Callers = append(pol.Callers, p.Callers...)
	if p.VendorRealm {
		// The rule-set routes name no organisation: they are recorded in the platform vendor log (A1.4 item 2).
		pol.Realm, pol.VendorAuditNone, pol.DirectoryClass, pol.PlatformAudit = auth.RealmVendor, p.VendorAuditNone, p.DirectoryClass, !p.VendorAuditNone
	}
	if p.ServiceOnly {
		// Tenancy and identity read the register on a user's behalf and name the organisation in the service token.
		pol.Permission, pol.ServiceOnly = "", true
	}
	rt.cfg.Guard.Handle(rt.mux, pattern, pol, http.HandlerFunc(h))
}

func badParam(w http.ResponseWriter, r *http.Request, err error) {
	var req *gen.RequiredHeaderError
	if errors.As(err, &req) {
		switch {
		case strings.EqualFold(req.ParamName, idempotency.Header):
			apperr.WriteProblem(w, r, idempotency.ErrKeyInvalid)
		case strings.EqualFold(req.ParamName, "Hostry-Vendor-Approval"):
			apperr.WriteProblem(w, r, apperr.New(apperr.Forbidden, "grant.required", "the approval grant is required"))
		case strings.EqualFold(req.ParamName, "If-Match"):
			apperr.WriteProblem(w, r, apperr.New(apperr.Validation, "validation", "If-Match is required").WithFields(apperr.FieldError{Field: "If-Match", Code: "required"}))
		default:
			apperr.WriteProblem(w, r, apperr.New(apperr.Validation, "validation", "missing header").WithFields(apperr.FieldError{Field: req.ParamName, Code: "required"}))
		}
		return
	}
	var f apperr.FieldError
	var inv *gen.InvalidParamFormatError
	if errors.As(err, &inv) {
		f = apperr.FieldError{Field: inv.ParamName, Code: "invalid"}
	}
	var missing *gen.RequiredParamError
	if errors.As(err, &missing) {
		f = apperr.FieldError{Field: missing.ParamName, Code: "required"}
	}
	apperr.WriteProblem(w, r, apperr.New(apperr.Validation, "validation", "invalid request parameter").WithFields(f))
}

const maxBody = 1 << 20

// begin prepares a mutating request: it decodes the body strictly into body (an unknown field, such as a tenant id,
// is a 422), validates the Idempotency-Key and puts the idempotency identity on the context.
func (s *Server) begin(w http.ResponseWriter, r *http.Request, op string, body any) (context.Context, bool) {
	ctx := r.Context()
	var canonical []byte
	if body != nil {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			s.fail(w, r, apperr.New(apperr.Validation, "validation", "request body unreadable"))
			return nil, false
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(body); err != nil || dec.More() {
			s.fail(w, r, decodeError(err))
			return nil, false
		}
		canonical, _ = json.Marshal(body)
	}
	key, ok, err := idempotency.KeyFromRequest(r)
	if err != nil || !ok {
		s.fail(w, r, idempotency.ErrKeyInvalid)
		return nil, false
	}
	return service.WithIdem(ctx, service.Idem{Op: op, Key: key, Fingerprint: idempotency.Fingerprint(r.Method, r.URL.Path, canonical)}), true
}

func decodeError(err error) error {
	field, code := "body", "malformed"
	if err != nil {
		if msg := err.Error(); strings.HasPrefix(msg, "json: unknown field ") {
			field, code = strings.Trim(strings.TrimPrefix(msg, "json: unknown field "), `"`), "unknown_field"
		}
	}
	return apperr.New(apperr.Validation, "validation", "invalid request body").WithFields(apperr.FieldError{Field: field, Code: code})
}

// fail writes err as a problem; a stale If-Match is a 412.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, service.ErrPrecondition) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusPreconditionFailed)
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "urn:hostry:problem:precondition_failed", "title": "Precondition failed", "status": 412,
			"code": "precondition_failed", "request_id": apperr.RequestIDFunc(r)})
		return
	}
	apperr.WriteProblem(w, r, err)
}

func (s *Server) reply(w http.ResponseWriter, r *http.Request, status int, v any, err error) {
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// pageParams validates cursor and limit.
func pageParams(cursor *string, limit *int, def, max int) (after string, n int, err error) {
	if after, err = page.Decode(cursor); err != nil {
		return "", 0, err
	}
	n, err = page.Limit(limit, def, max)
	return after, n, err
}

func nextCursor(next string) *string {
	if next == "" {
		return nil
	}
	c := page.Encode(next)
	return &c
}

func deref[T any](p *T) T {
	var z T
	if p == nil {
		return z
	}
	return *p
}
