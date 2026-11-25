// Package contractenv prepares the shared contract suite (api-specs/contracttest) for this service. The double and
// the real service run the same checks through it. The suite is example-driven: it sends each operation once with the
// spec's example inputs, after SeedWorld has put the target into the operation's example world (internal/seed).
//
// Two passes: the double must also serve the examples verbatim; the operations whose examples cannot be reproduced
// by a service (see ExampleUnreachable) are checked for status, media type and schema only, in both passes' targets.
package contractenv

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/api-specs/contracttest"

	"github.com/hostryhq/svc-billing/double"
	"github.com/hostryhq/svc-billing/internal/seed"
)

// ExampleUnreachable are the operations whose response example no implementation can reproduce byte for byte; each is a
// spec problem reported to api-specs. The example worlds, the listBills example and the digest (digest-day world) are
// consistent since api-specs b153c62, so what remains is listed here with its reason and checked for status, media type and schema only.
var ExampleUnreachable = map[string]string{
	"signTaxRuleSet": "the example's content_hash is a made-up value; signing stores the real hash of the content (the shared vectors fix it only for the publish subject hash)",
}

// SpecPath returns the billing OpenAPI document of the pinned api-specs (or API_SPECS_DIR), with common/ beside it.
func SpecPath(t testing.TB) string {
	t.Helper()
	root := os.Getenv("API_SPECS_DIR")
	if root == "" {
		out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/hostryhq/api-specs").Output()
		if err != nil {
			t.Fatalf("locate api-specs: %v", err)
		}
		root = strings.TrimSpace(string(out))
	}
	return filepath.Join(root, "openapi/billing/v1/openapi.yaml")
}

// transport authenticates each request of the suite for the example world: tenant operations as the example
// organisation's owner, vendor operations as the vendor user who approves in the rule-set-signed world through the admin
// service, internal ones as tenancy. It changes nothing else of the request.
type transport struct {
	tk *double.Tokens
}

func (t transport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	switch {
	case strings.HasPrefix(r.URL.Path, "/internal/v1/vendor/"):
		for k, vs := range t.tk.VendorHeaders("admin.taxrules.manage", "admin.taxrules.publish") {
			r.Header.Del(k)
			for _, v := range vs {
				r.Header.Add(k, v)
			}
		}
	case strings.HasPrefix(r.URL.Path, "/internal/v1/"):
		for k, vs := range t.tk.ServiceHeaders("tenancy", ids.ID(seed.Tenant)) {
			r.Header.Set(k, vs[0])
		}
	default:
		r.Header.Set("Authorization", "Bearer "+t.tk.TenantToken(ids.ID(seed.Tenant)))
	}
	return http.DefaultTransport.RoundTrip(r)
}

// Run runs the contract suite against base. seedWorld puts the target into an example world.
func Run(t *testing.T, base string, tk *double.Tokens, seedWorld func(context.Context, string) error, expectExamples bool) {
	t.Helper()
	spec := SpecPath(t)
	client := &http.Client{Transport: transport{tk}}
	skipOthers := map[string]string{}
	skipUnreachable := map[string]string{}
	for op, why := range ExampleUnreachable {
		skipUnreachable[op] = "checked in the schema-only pass: " + why
	}
	t.Run("examples", func(t *testing.T) {
		contracttest.Run(t, contracttest.Options{SpecPath: spec, BaseURL: base, Client: client, Skip: mergeSkip(skipOthers, skipUnreachable), ExpectExamples: expectExamples, SeedWorld: seedWorld})
	})
	t.Run("schema-only", func(t *testing.T) {
		keep := map[string]string{}
		ops := OperationIDs(t, spec)
		for _, op := range ops {
			if _, ok := ExampleUnreachable[op]; !ok {
				keep[op] = "covered by the examples pass"
			}
		}
		contracttest.Run(t, contracttest.Options{SpecPath: spec, BaseURL: base, Client: client, Skip: keep, SeedWorld: seedWorld})
	})
}

func mergeSkip(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}
