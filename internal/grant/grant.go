// Package grant checks the signature of the vendor approval grant of ADR-0014 A2.6: a compact ES256 JWS signed by
// identity's vendor-realm key (kid in the header), single use, five minutes. It is the signature check behind
// service.GrantVerifier and nothing more; go-auth is to own issue and verification (and SubjectHash), and this package
// goes away then (ADR-0014 "Changes needed elsewhere": svc-billing drops internal/grant). Every claim check after the
// signature (typ, realm, aud, exp, kind, org, approver, subject hash, single use) is the service's own, in
// service.PublishTaxRuleSet, so it does not change with the verifier.
package grant

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"time"

	"github.com/hostryhq/go-auth"

	"github.com/hostryhq/svc-billing/service"
)

// Verifier checks grants against identity's vendor-realm key set.
type Verifier struct {
	Keys auth.KeySet
}

var _ service.GrantVerifier = (*Verifier)(nil)

type header struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}

// claims are the A2.6 item 1 names (api-specs common/vendor.yaml VendorApprovalGrantClaims); unknown claims are ignored.
type claims struct {
	JTI         string `json:"jti"`
	Realm       string `json:"realm"`
	Audience    string `json:"aud"`
	Type        string `json:"typ"`
	ApprovalID  string `json:"approval_id"`
	Kind        string `json:"kind"`
	Org         string `json:"org"`
	Requester   string `json:"requester"`
	Approver    string `json:"approver"`
	SubjectHash string `json:"subject_hash"`
	ExpiresAt   int64  `json:"exp"`
}

// Verify implements service.GrantVerifier: the signature and key id, then the claims as written.
func (v *Verifier) Verify(_ context.Context, raw string) (service.Grant, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return service.Grant{}, service.ErrGrantInvalid
	}
	hb, err1 := base64.RawURLEncoding.DecodeString(parts[0])
	cb, err2 := base64.RawURLEncoding.DecodeString(parts[1])
	sig, err3 := base64.RawURLEncoding.DecodeString(parts[2])
	if err1 != nil || err2 != nil || err3 != nil || len(sig) != 64 {
		return service.Grant{}, service.ErrGrantInvalid
	}
	var h header
	if json.Unmarshal(hb, &h) != nil || h.Alg != "ES256" || h.Kid == "" {
		return service.Grant{}, service.ErrGrantInvalid
	}
	key, err := v.Keys.Key(h.Kid)
	if err != nil || key == nil {
		return service.Grant{}, service.ErrGrantInvalid
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(key, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return service.Grant{}, service.ErrGrantInvalid
	}
	var c claims
	if json.Unmarshal(cb, &c) != nil || c.JTI == "" {
		return service.Grant{}, service.ErrGrantInvalid
	}
	return service.Grant{ID: c.JTI, ApprovalID: c.ApprovalID, Kind: c.Kind, Type: c.Type, Realm: c.Realm, Audience: c.Audience, Org: c.Org,
		Requester: c.Requester, Approver: c.Approver, SubjectHash: c.SubjectHash, ExpiresAt: time.Unix(c.ExpiresAt, 0)}, nil
}
