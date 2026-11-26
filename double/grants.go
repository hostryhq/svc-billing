package double

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/clock"

	"github.com/hostryhq/svc-billing/internal/seed"
	"github.com/hostryhq/svc-billing/service"
)

// Grant constants of the double's vendor realm: the key id and issuer its grants carry.
const (
	GrantKid    = "double-vendor-grant-1"
	GrantIssuer = "https://identity.hostry.invalid/vendor"
)

// Grants is the double's vendor approval grant authority: it signs real grants (ES256, claims of A2.6 item 1) under its own
// throw-away key and verifies them with go-auth's GrantVerifier, so a test exercises the same checks the real service runs.
// The contract sends one fixed example token (api-specs common/vendor.yaml, a token for another kind and audience); the
// double maps it to a fresh publish grant for the example rule set of the rule-set-signed world, as the examples intend.
// The zero value works; Clock defaults to the system clock (double.New sets it to the double's clock).
type Grants struct {
	Clock clock.Clock

	once sync.Once
	key  *ecdsa.PrivateKey
	v    *auth.GrantVerifier
	mu   sync.Mutex
	seq  int
}

// NewGrants returns an authority on clk.
func NewGrants(clk clock.Clock) *Grants { return &Grants{Clock: clk} }

// ExampleGrant is the raw grant the contract examples send.
func ExampleGrant() string { return seed.ExampleGrantToken() }

func (g *Grants) init() {
	g.once.Do(func() {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			panic(err)
		}
		g.key = k
		if g.Clock == nil {
			g.Clock = clock.System
		}
		g.v = &auth.GrantVerifier{Keys: auth.NewStaticKeySet(auth.KeyDoc{Keys: map[string]*ecdsa.PublicKey{GrantKid: &k.PublicKey}}),
			Issuer: GrantIssuer, Audience: service.GrantAudience, Clock: g.Clock}
	})
}

// Claims returns publish-grant claims for subjectHash: five minutes from now, issued by the vendor realm for billing, a
// fresh jti, no organisation.
func (g *Grants) Claims(requester, approver, subjectHash string) auth.GrantClaims {
	g.init()
	g.mu.Lock()
	g.seq++
	n := g.seq
	g.mu.Unlock()
	now := g.Clock.Now()
	return auth.GrantClaims{Issuer: GrantIssuer, Realm: auth.RealmVendor, Audience: service.GrantAudience, Type: auth.GrantType,
		JTI: fmt.Sprintf("double-grant-%d", n), ApprovalID: fmt.Sprintf("double-approval-%d", n), Kind: service.GrantKindPublish,
		Requester: requester, Approver: approver, SubjectHash: subjectHash, IssuedAt: now.Unix(), ExpiresAt: now.Add(auth.MaxGrantLifetime).Unix()}
}

// Mint signs c as given (a test edits a claim to make a refusal) and returns the compact token.
func (g *Grants) Mint(c auth.GrantClaims) string { return g.MintKid(GrantKid, c) }

// MintKid signs c under another key id (a grant whose kid the verifier does not know).
func (g *Grants) MintKid(kid string, c auth.GrantClaims) string {
	g.init()
	hb, _ := json.Marshal(map[string]string{"alg": "ES256", "typ": "JWT", "kid": kid})
	pb, _ := json.Marshal(c)
	in := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(pb)
	sum := sha256.Sum256([]byte(in))
	r, s, err := ecdsa.Sign(rand.Reader, g.key, sum[:])
	if err != nil {
		panic(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return in + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// Verify implements service.GrantVerifier.
func (g *Grants) Verify(ctx context.Context, token string, want auth.GrantWant) (*auth.GrantClaims, error) {
	g.init()
	if token == ExampleGrant() {
		c := seed.ExamplePublishClaims(g.Clock.Now())
		c.Issuer = GrantIssuer
		token = g.Mint(c)
	}
	return g.v.Verify(ctx, token, want)
}

var _ service.GrantVerifier = (*Grants)(nil)
