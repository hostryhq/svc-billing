// Package itest assembles the real service (Postgres-backed) for integration tests: a fresh database on the local dev
// stack (internal/pgtest), the wired handler, a fake clock for entity timestamps and the test token issuers. Other
// services' integration tests use the double instead.
package itest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hostryhq/go-common/clock"
	"github.com/hostryhq/go-common/emit"
	"github.com/hostryhq/go-common/ids"
	"github.com/hostryhq/go-common/testkit"

	"github.com/hostryhq/svc-billing/double"
	"github.com/hostryhq/svc-billing/internal/app"
	"github.com/hostryhq/svc-billing/internal/pgtest"
	"github.com/hostryhq/svc-billing/internal/seed"
)

// Stack is the real service over a fresh database.
type Stack struct {
	DB    *pgtest.DB
	App   *app.App
	Srv   *httptest.Server
	Clock *testkit.FakeClock
	// Gateway is the in-process fake of the sync gateway (POST /internal/v1/emit), served over HTTP to the real emit.Client
	// that the service and the emit_queue worker use.
	Gateway    *double.Gateway
	GatewaySrv *httptest.Server
	Grants     *double.Grants
	*double.Tokens
}

// New starts a stack.
func New(t testing.TB) *Stack {
	t.Helper()
	db := pgtest.New(t)
	clk := testkit.NewClock()
	clk.Set(seed.At)
	tk := double.NewTokens()
	gw := double.NewGateway(clk)
	gsrv := httptest.NewServer(gw)
	t.Cleanup(gsrv.Close)
	em := &emit.Client{BaseURL: gsrv.URL, Token: func(context.Context) (string, error) { return "test-service-token", nil }}
	g := &double.Grants{}
	a, err := app.Build(app.Options{
		AppDB: db.App, GlobalDB: db.Global,
		Tenant: tk.TenantIssuer.Verifier(), Vendor: tk.VendorIssuer.Verifier(), Services: tk.Registry.Verifier("billing"),
		Clock: clk, AuthClock: clock.System, IDs: ids.NewGenerator(clk.Now, nil), Log: slog.New(slog.DiscardHandler),
		Emitter: em, Orders: double.NoOpenChecks{}, Grants: g,
	})
	if err != nil {
		t.Fatal(err)
	}
	gw.SetService(a.Service)
	s := &Stack{DB: db, App: a, Srv: httptest.NewServer(a.Handler), Clock: clk, Gateway: gw, GatewaySrv: gsrv, Grants: g, Tokens: tk}
	t.Cleanup(s.Srv.Close)
	return s
}

// SeedWorld empties the database and stores the named example world (contracttest Options.SeedWorld).
func (s *Stack) SeedWorld(ctx context.Context, world string) error {
	if err := s.DB.Truncate(ctx); err != nil {
		return err
	}
	s.Gateway.Reset()
	s.Clock.Set(seed.WorldAt(world))
	return seed.Apply(ctx, s.App.Repo, world)
}

// Response is a decoded HTTP answer.
type Response struct {
	Status int
	Body   []byte
	Header http.Header
}

// JSON decodes the body into a map.
func (r Response) JSON(t testing.TB) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatalf("body is not a JSON object: %v: %s", err, r.Body)
	}
	return m
}

// Code returns the problem code of an error response ("" otherwise).
func (r Response) Code() string {
	var p struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(r.Body, &p)
	return p.Code
}

// Do sends a request; body may be nil, a string, []byte or any JSON-marshalable value. The Idempotency-Key header is
// added for mutating methods unless the headers already carry one.
func Do(t testing.TB, base, method, path string, headers http.Header, body any) Response {
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = bytes.NewReader([]byte(b))
	case []byte:
		rd = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, base+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/json")
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet && method != http.MethodHead && headers.Get("Idempotency-Key") == "" {
		req.Header.Set("Idempotency-Key", ids.New().String())
	}
	for k, vs := range headers {
		req.Header.Del(k)
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return Response{Status: resp.StatusCode, Body: b, Header: resp.Header}
}

// Bearer returns headers with a bearer token.
func Bearer(tok string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+tok)
	return h
}

// DrainEmits sends the queued emissions (rule sets, Z revisions) through the real emit_queue worker and emit client to the
// fake gateway, and returns how many rows were sent.
func (s *Stack) DrainEmits(ctx context.Context) (int, error) { return s.App.Queue.Drain(ctx) }
