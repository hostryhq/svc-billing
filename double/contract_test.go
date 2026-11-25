package double_test

import (
	"testing"

	"github.com/hostryhq/svc-billing/double"
	"github.com/hostryhq/svc-billing/internal/contractenv"
)

// TestContract runs the shared contract suite (api-specs/contracttest) against the double: every operation, each in
// its example world, with the double serving the spec's examples verbatim.
func TestContract(t *testing.T) {
	env, err := double.NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	srv := env.Server()
	defer srv.Close()
	contractenv.Run(t, srv.URL, env.Tokens, env.SeedWorld, true)
}
