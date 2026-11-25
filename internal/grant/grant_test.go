package grant_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hostryhq/svc-billing/internal/grant"
	"github.com/hostryhq/svc-billing/internal/seed"
	"github.com/hostryhq/svc-billing/service"
)

type vectors struct {
	SubjectHash []struct {
		Kind        string            `json:"kind"`
		Fields      map[string]string `json:"fields"`
		Text        string            `json:"text"`
		SubjectHash string            `json:"subject_hash"`
	} `json:"subject_hash"`
	ExamplePack  map[string]string `json:"example_pack"`
	ExampleGrant struct {
		Token  string         `json:"token"`
		Claims map[string]any `json:"claims"`
	} `json:"example_grant"`
}

func load(t *testing.T) vectors {
	t.Helper()
	var v vectors
	if err := json.Unmarshal(seed.GrantVectors(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// pack is the example vendor-realm key set of the shared vectors.
type pack map[string]*ecdsa.PublicKey

func (p pack) Key(kid string) (*ecdsa.PublicKey, error) {
	if k, ok := p[kid]; ok {
		return k, nil
	}
	return nil, errors.New("unknown key")
}

func examplePack(t *testing.T, v vectors) pack {
	t.Helper()
	p := pack{}
	for kid, b64 := range v.ExamplePack {
		raw, err := base64.RawURLEncoding.DecodeString(b64)
		if err != nil {
			t.Fatal(err)
		}
		k, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), raw)
		if err != nil {
			t.Fatal(err)
		}
		p[kid] = k
	}
	return p
}

// The subject hash the service recomputes for a publication is the shared vector (ADR-0014 A2.6 item 2), byte for byte.
func TestPublishSubjectHashIsTheSharedVector(t *testing.T) {
	var found bool
	for _, c := range load(t).SubjectHash {
		if c.Kind != service.GrantKindPublish {
			continue
		}
		found = true
		if got := service.PublishSubjectHash(c.Fields["rule_set_id"], c.Fields["content_hash"], c.Fields["effective_date"]); got != c.SubjectHash {
			t.Fatalf("subject hash %s, vector %s (text %q)", got, c.SubjectHash, c.Text)
		}
	}
	if !found {
		t.Fatal("no tax_rule_set_publish vector")
	}
}

// The signature check accepts the example grant of the shared vectors, returns its claims as written, and refuses a grant
// whose payload, signature, algorithm or key id is not identity's.
func TestVerifierChecksTheSignatureAndReadsTheClaims(t *testing.T) {
	v := load(t)
	ver := &grant.Verifier{Keys: examplePack(t, v)}
	g, err := ver.Verify(context.Background(), v.ExampleGrant.Token)
	if err != nil {
		t.Fatal(err)
	}
	c := v.ExampleGrant.Claims
	if g.ID != c["jti"] || g.Kind != c["kind"] || g.Audience != c["aud"] || g.Realm != c["realm"] || g.Type != c["typ"] || g.Org != c["org"] ||
		g.Approver != c["approver"] || g.Requester != c["requester"] || g.SubjectHash != c["subject_hash"] || g.ApprovalID != c["approval_id"] || g.ExpiresAt.Unix() != int64(c["exp"].(float64)) {
		t.Fatalf("claims %+v", g)
	}
	parts := strings.Split(v.ExampleGrant.Token, ".")
	b64 := func(m map[string]any) string { b, _ := json.Marshal(m); return base64.RawURLEncoding.EncodeToString(b) }
	forged := map[string]any{}
	for k, x := range c {
		forged[k] = x
	}
	forged["kind"], forged["aud"] = service.GrantKindPublish, "billing"
	badSig := parts[0] + "." + parts[1] + "." + parts[2][:len(parts[2])-2] + "AA"
	for name, raw := range map[string]string{
		"payload replaced":   parts[0] + "." + b64(forged) + "." + parts[2],
		"signature changed":  badSig,
		"alg none":           b64(map[string]any{"alg": "none", "kid": "vendor-realm-1"}) + "." + parts[1] + ".",
		"unknown kid":        b64(map[string]any{"alg": "ES256", "kid": "other"}) + "." + parts[1] + "." + parts[2],
		"not a compact JWS":  "grant_example_0190a1b2",
		"header only is bad": parts[0],
	} {
		if _, err := ver.Verify(context.Background(), raw); !errors.Is(err, service.ErrGrantInvalid) {
			t.Errorf("%s: %v, want ErrGrantInvalid", name, err)
		}
	}
}
