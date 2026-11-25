// Package ordersclient asks orders how many checks an outlet has open (orders getOpenCheckCount, internal route,
// service caller billing). billing asks only when the owner changes the numbering mode; never on the trading path.
package ordersclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/tenancy"
)

// Client calls orders with a 60-second service token naming the organisation.
type Client struct {
	BaseURL string
	Signer  *auth.ServiceSigner
	HTTP    *http.Client
}

// OpenCheckCount implements service.OpenChecks.
func (c *Client) OpenCheckCount(ctx context.Context, outletID string) (int, error) {
	tc, err := tenancy.Require(ctx)
	if err != nil {
		return 0, err
	}
	tok, err := c.Signer.Sign("orders", tc.TenantID.String())
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/internal/v1/outlets/"+url.PathEscape(outletID)+"/open-check-count", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set(auth.ServiceTokenHeader, tok)
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("orders answered %d", resp.StatusCode)
	}
	var out struct {
		OpenChecks int `json:"open_checks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	return out.OpenChecks, nil
}
