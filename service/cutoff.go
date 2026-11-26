package service

import (
	"time"

	"github.com/hostryhq/svc-billing/internal/tstamp"
)

// cutoffAt is the cutoff_at of a day_close row: the instant the device closed the day. A close without it (an older
// client) is taken at the time of its op.
func cutoffAt(f map[string]any, opAt time.Time) tstamp.Time {
	if v := fstr(f, "cutoff_at"); v != "" {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			return tstamp.Of(t)
		}
	}
	return tstamp.Of(opAt)
}
