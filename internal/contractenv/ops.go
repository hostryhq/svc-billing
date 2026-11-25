package contractenv

import (
	"os"
	"sort"
	"testing"

	"gopkg.in/yaml.v3"
)

// OperationIDs lists the operations of the spec file.
func OperationIDs(t testing.TB, specPath string) []string {
	t.Helper()
	b, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string `yaml:"operationId"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, item := range doc.Paths {
		for _, op := range item {
			if op.OperationID != "" {
				out = append(out, op.OperationID)
			}
		}
	}
	sort.Strings(out)
	return out
}
