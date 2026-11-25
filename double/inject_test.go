package double_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-billing/double"
	"github.com/hostryhq/svc-billing/internal/itest"
	"github.com/hostryhq/svc-billing/internal/seed"
	"github.com/hostryhq/svc-billing/service"
)

// Callers test their resilience against the double: injected errors, delays and hangs, removable.
func TestFailureInjection(t *testing.T) {
	env, err := double.NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	if err := env.SeedWorld(context.Background(), "base"); err != nil {
		t.Fatal(err)
	}
	srv := env.Server()
	defer srv.Close()
	h := itest.Bearer(env.TenantToken(ids.ID(seed.Tenant)))
	bills := func(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, "/v1/bills") }

	remove := env.Inject(double.Fault{Match: bills, Status: 503})
	r := itest.Do(t, srv.URL, "GET", "/v1/bills", h, nil)
	if r.Status != 503 || r.Code() != "injected" {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	if got := itest.Do(t, srv.URL, "GET", "/v1/shifts", h, nil); got.Status == 503 {
		t.Fatal("only matching requests fail")
	}
	remove()
	if got := itest.Do(t, srv.URL, "GET", "/v1/bills", h, nil); got.Status != 200 {
		t.Fatalf("a removed fault must stop applying: %d", got.Status)
	}
	env.Inject(double.Fault{Match: bills, Delay: 150 * time.Millisecond})
	start := time.Now()
	itest.Do(t, srv.URL, "GET", "/v1/bills", h, nil)
	if time.Since(start) < 150*time.Millisecond {
		t.Fatal("the injected delay did not apply")
	}
	env2, _ := double.NewEnv()
	srv2 := env2.Server()
	defer srv2.Close()
	env2.Inject(double.Fault{Match: func(*http.Request) bool { return true }, Hang: true})
	c := &http.Client{Timeout: 100 * time.Millisecond}
	if _, err := c.Get(srv2.URL + "/v1/bills"); err == nil {
		t.Fatal("a hung request must time out for the caller")
	}
}

// Another service's test feeds the double ops through the same projection and reads the result over HTTP.
func TestDoubleProjectsOps(t *testing.T) {
	env, _ := double.NewEnv()
	if err := env.SeedWorld(context.Background(), "base"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := env.Svc.RecordOutlet(tenantCtx(ctx), service.OutletRec{OutletID: seed.Outlet, RegistrationID: seed.Registration, Regime: "regular"}); err != nil {
		t.Fatal(err)
	}
	srv := env.Server()
	defer srv.Close()
	h := itest.Bearer(env.TenantToken(ids.ID(seed.Tenant)))
	h.Set("X-Outlet-Id", seed.Outlet)
	r := itest.Do(t, srv.URL, "GET", "/v1/exceptions", h, nil)
	if r.Status != 200 {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
}
