package consumer

import (
	"context"
	"testing"

	"github.com/hostryhq/go-common/events"
)

// Gateway notices that share the sync.op prefix are not ops: they are acknowledged without a projection, never parked.
func TestFlaggedNoticeIsNotAnOp(t *testing.T) {
	h := Handler(nil, nil)
	if err := h(context.Background(), nil, events.Envelope{Type: "sync.op.flagged", V: 1, Source: "sync", TenantID: "t", Data: []byte(`{"origin":"d","stream":"s","seq":1}`)}); err != nil {
		t.Fatal(err)
	}
}
