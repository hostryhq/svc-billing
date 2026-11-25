// Package grant verifies the vendor approval grant of ADR-0014 A1.3: a compact ES256 JWS signed by identity, single
// use, five minutes. The claim names are provisional (api-specs common/vendor.yaml fixes the meaning, not the keys;
// see docs/notes.md): jti, approval_id, kind, requester, approver, subject_hash, exp. Single use is enforced by the
// service in the action's own transaction (UseGrant); this package checks signature, type and expiry.
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
	"github.com/hostryhq/go-common/clock"

	"github.com/hostryhq/svc-billing/service"
)

// Verifier checks grants against identity's vendor-realm key set.
type Verifier struct {
	Keys  auth.KeySet
	Clock clock.Clock
}

var _ service.GrantVerifier = (*Verifier)(nil)

type header struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ"`
}

type claims struct {
	JTI         string `json:"jti"`
	Type        string `json:"typ"`
	ApprovalID  string `json:"approval_id"`
	Kind        string `json:"kind"`
	Requester   string `json:"requester"`
	Approver    string `json:"approver"`
	SubjectHash string `json:"subject_hash"`
	ExpiresAt   int64  `json:"exp"`
}

// Verify implements service.GrantVerifier.
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
	if json.Unmarshal(hb, &h) != nil || h.Alg != "ES256" {
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
	if json.Unmarshal(cb, &c) != nil || c.Type != "vendor_approval" || c.JTI == "" {
		return service.Grant{}, service.ErrGrantInvalid
	}
	g := service.Grant{ID: c.JTI, ApprovalID: c.ApprovalID, Kind: c.Kind, Requester: c.Requester, Approver: c.Approver, SubjectHash: c.SubjectHash, ExpiresAt: time.Unix(c.ExpiresAt, 0)}
	now := time.Now()
	if v.Clock != nil {
		now = v.Clock.Now()
	}
	if now.After(g.ExpiresAt) {
		return g, service.ErrGrantExpired
	}
	return g, nil
}
