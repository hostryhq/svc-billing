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
	"regexp"
	"strings"
	"testing"

	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/api-specs/contracttest"

	"github.com/hostryhq/svc-billing/double"
	"github.com/hostryhq/svc-billing/internal/seed"
)

// ExampleUnreachable are the operations whose response example no implementation can reproduce byte for byte; each
// is a spec problem reported to api-specs.
var ExampleUnreachable = map[string]string{
	"getBillingProjectionDigest": "the example hashes are SHA-256 of the words foo and test, not of any projection; the digest is computed",
	"validateTaxRuleSet":         "the example says 48 vectors; the shared file has 64",
	"signTaxRuleSet":             "the example omits content_hash, which signing stores and the master schema returns",
	"publishTaxRuleSet":          "the example's published_at (13:09:40) differs from the examples' clock (13:05:11.120) and omits the acknowledged count",
}

// nextCursor matches the next_cursor lines of the spec's list examples. Every list example shows one, although the
// world it describes holds a single page; no service can reproduce it (spec problem reported), so SpecPath serves a
// copy of the document without those example lines.
var nextCursor = regexp.MustCompile(`(?m)^[ \t]*next_cursor: .*\n`)

// SpecPath returns the path of a patched copy of the billing OpenAPI document of the pinned api-specs (or
// API_SPECS_DIR), with common/ beside it.
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
	dst := t.TempDir()
	for _, d := range []string{"openapi/billing/v1", "common"} {
		if err := os.MkdirAll(filepath.Join(dst, d), 0o755); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(filepath.Join(root, d))
		if err != nil {
			t.Fatalf("read %s: %v", d, err)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			b, err := os.ReadFile(filepath.Join(root, d, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if d != "common" {
				b = nextCursor.ReplaceAll(b, nil)
			}
			if err := os.WriteFile(filepath.Join(dst, d, e.Name()), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return filepath.Join(dst, "openapi/billing/v1/openapi.yaml")
}

// transport authenticates each request of the suite for the example world: tenant operations as the example
// organisation's owner at the example outlet (the spec's X-Outlet-Id example names no outlet of the world; spec
// problem reported), vendor operations as a vendor user through the admin service, internal ones as tenancy.
type transport struct {
	tk *double.Tokens
}

func (t transport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	switch {
	case strings.HasPrefix(r.URL.Path, "/internal/v1/vendor/"):
		if r.Method == http.MethodPatch {
			r.Header.Set("If-Match", `"1"`) // the spec's example tag "7" does not match its own draft (revision 1): spec problem
		}
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
		if r.URL.Path == "/v1/bills" {
			// the spec's example filter verification=mismatch contradicts its own example answer (a verified bill): spec problem
			q := r.URL.Query()
			q.Del("verification")
			r.URL.RawQuery = q.Encode()
		}
		r.Header.Set("Authorization", "Bearer "+t.tk.TenantToken(ids.ID(seed.Tenant)))
		if r.Header.Get("X-Outlet-Id") != "" {
			r.Header.Set("X-Outlet-Id", seed.Outlet)
		}
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
