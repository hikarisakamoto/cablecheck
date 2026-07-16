package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Duration is a time.Duration with a JSON surface designed for report
// consumers: it marshals to a two-field object {"ms":30000,"text":"30s"}
// (machine-unambiguous milliseconds plus a human-readable Go duration
// string). Raw time.Duration fields are banned from the report model — a
// reflection test enforces this.
type Duration time.Duration

// Std returns the value as a standard time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// String returns the standard Go duration string, e.g. "30s" or "1m30s".
func (d Duration) String() string { return time.Duration(d).String() }

// MarshalJSON encodes the duration as {"ms":<milliseconds>,"text":"<String()>"}.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Ms   int64  `json:"ms"`
		Text string `json:"text"`
	}{time.Duration(d).Milliseconds(), d.String()})
}

// UnmarshalJSON accepts the {"ms":...} object form (the "text" field is
// ignored on input), a bare JSON number of milliseconds, or a Go duration
// string such as "30s". JSON null leaves the value unchanged.
func (d *Duration) UnmarshalJSON(data []byte) error {
	trim := bytes.TrimSpace(data)
	if len(trim) == 0 {
		return errors.New("duration: empty JSON value")
	}
	if string(trim) == "null" {
		return nil
	}
	switch trim[0] {
	case '{':
		var obj struct {
			Ms *int64 `json:"ms"`
		}
		if err := json.Unmarshal(trim, &obj); err != nil {
			return fmt.Errorf("duration: %w", err)
		}
		if obj.Ms == nil {
			return errors.New(`duration: object form requires an "ms" field`)
		}
		*d = Duration(time.Duration(*obj.Ms) * time.Millisecond)
	case '"':
		var s string
		if err := json.Unmarshal(trim, &s); err != nil {
			return fmt.Errorf("duration: %w", err)
		}
		td, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("duration %q: %w", s, err)
		}
		*d = Duration(td)
	default:
		var n json.Number
		if err := json.Unmarshal(trim, &n); err != nil {
			return fmt.Errorf("duration: %w", err)
		}
		if i, err := n.Int64(); err == nil {
			*d = Duration(time.Duration(i) * time.Millisecond)
			return nil
		}
		f, err := n.Float64()
		if err != nil {
			return fmt.Errorf("duration: invalid number %q", n.String())
		}
		*d = Duration(f * float64(time.Millisecond))
	}
	return nil
}
