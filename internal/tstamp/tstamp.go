// Package tstamp is the wire timestamp: an RFC 3339 instant in UTC with exactly three fractional digits, the form
// every Hostry example and event uses (2026-11-24T13:05:11.120Z). time.Time would drop a trailing zero.
// The generated API types use it for every date-time property (oapi-codegen type-mapping, see oapi-codegen.yaml).
package tstamp

import (
	"encoding/json"
	"fmt"
	"time"
)

// Layout is the wire format.
const Layout = "2006-01-02T15:04:05.000Z"

// Time is a UTC instant that marshals with millisecond precision.
type Time struct{ time.Time }

// Of wraps t, truncated to milliseconds in UTC.
func Of(t time.Time) Time { return Time{t.UTC().Truncate(time.Millisecond)} }

// Ptr is a pointer to Of(t).
func Ptr(t time.Time) *Time { v := Of(t); return &v }

// MarshalJSON implements json.Marshaler.
func (t Time) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.UTC().Format(Layout))
}

// UnmarshalJSON accepts any RFC 3339 instant.
func (t *Time) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	p, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return fmt.Errorf("tstamp: %w", err)
	}
	t.Time = p.UTC()
	return nil
}

// String implements fmt.Stringer.
func (t Time) String() string { return t.UTC().Format(Layout) }
