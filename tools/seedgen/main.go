// Command seedgen extracts the response and request examples of the bundled billing OpenAPI document into
// internal/seed/examples.json, keyed by operationId. The example worlds of the contract (info.x-example-worlds) are
// seeded from them, so the double and the real service start from exactly the state the examples describe. It is run
// by `make gen` after the bundle step.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"

	"gopkg.in/yaml.v3"
)

type media struct {
	Example any `yaml:"example"`
}

type response struct {
	Content map[string]media `yaml:"content"`
}

type op struct {
	OperationID string              `yaml:"operationId"`
	World       string              `yaml:"x-example-world"`
	RequestBody *response           `yaml:"requestBody"`
	Responses   map[string]response `yaml:"responses"`
}

// Example is what the seed needs of one operation.
type Example struct {
	World    string `json:"world"`
	Request  any    `json:"request,omitempty"`
	Response any    `json:"response,omitempty"`
}

func main() {
	in := flag.String("in", ".tmp/billing.yaml", "bundled OpenAPI document")
	out := flag.String("out", "internal/seed/examples.json", "output")
	flag.Parse()
	b, err := os.ReadFile(*in)
	if err != nil {
		fail(err)
	}
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		fail(err)
	}
	res := map[string]Example{}
	for _, item := range doc.Paths {
		for method, node := range item {
			switch method {
			case "get", "put", "post", "delete", "patch":
			default:
				continue
			}
			var o op
			if err := node.Decode(&o); err != nil {
				fail(err)
			}
			ex := Example{World: o.World}
			if ex.World == "" {
				ex.World = "base"
			}
			if o.RequestBody != nil {
				ex.Request = o.RequestBody.Content["application/json"].Example
			}
			codes := make([]string, 0, len(o.Responses))
			for c := range o.Responses {
				codes = append(codes, c)
			}
			sort.Strings(codes)
			for _, c := range codes {
				if c[0] == '2' {
					ex.Response = o.Responses[c].Content["application/json"].Example
					break
				}
			}
			res[o.OperationID] = ex
		}
	}
	j, err := json.MarshalIndent(res, "", " ")
	if err != nil {
		fail(err)
	}
	if err := os.WriteFile(*out, append(j, '\n'), 0o644); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "seedgen:", err)
	os.Exit(1)
}
