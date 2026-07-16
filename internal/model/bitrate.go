package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Bitrate is a bit rate in bits per second.
//
// Its textual form follows the grammar INT[.FRAC][K|M|G] with a
// case-insensitive decimal suffix (K = 1e3, M = 1e6, G = 1e9); a bare integer
// means bits per second. Bytes-based suffixes (KB, MiB, ...) are rejected.
type Bitrate uint64

// bitrateUnit pairs a decimal multiplier with its canonical suffix.
type bitrateUnit struct {
	mult   uint64
	suffix string
}

// bitrateUnits is ordered largest-first for String rendering.
var bitrateUnits = []bitrateUnit{
	{1_000_000_000, "G"},
	{1_000_000, "M"},
	{1_000, "K"},
}

// ParseBitrate parses a bitrate like "80M", "2.5G", "100K" or a bare integer
// number of bits per second ("5000000").
//
// Parsing is pure integer math (no float artifacts): the integer and fraction
// digits are handled separately, and the fraction is only accepted when
// 10^len(frac) divides the unit multiplier (so "2.5G" is exact and "1.2345K"
// is rejected as finer than the unit). Zero, negative values, bare fractions
// without a unit, unknown suffixes and values overflowing uint64 (checked
// before any multiplication) are all errors.
func ParseBitrate(s string) (Bitrate, error) {
	if s == "" {
		return 0, errors.New("bitrate: empty string")
	}
	if s[0] == '-' {
		return 0, fmt.Errorf("bitrate %q: negative values are not allowed", s)
	}

	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	intDigits := s[:i]
	if intDigits == "" {
		return 0, fmt.Errorf("bitrate %q: must start with a digit", s)
	}
	rest := s[i:]

	fracDigits := ""
	if strings.HasPrefix(rest, ".") {
		rest = rest[1:]
		j := 0
		for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
			j++
		}
		fracDigits = rest[:j]
		if fracDigits == "" {
			return 0, fmt.Errorf("bitrate %q: missing digits after the decimal point", s)
		}
		rest = rest[j:]
	}

	suffix := rest
	var mult uint64
	switch strings.ToLower(suffix) {
	case "":
		if fracDigits != "" {
			return 0, fmt.Errorf("bitrate %q: a fractional value requires a unit suffix (K, M, or G)", s)
		}
		mult = 1
	case "k":
		mult = 1_000
	case "m":
		mult = 1_000_000
	case "g":
		mult = 1_000_000_000
	case "b", "kb", "mb", "gb", "kib", "mib", "gib":
		return 0, fmt.Errorf("bitrate %q: bytes suffixes are not accepted; use decimal bits: K, M, G", s)
	default:
		return 0, fmt.Errorf("bitrate %q: unknown unit %q (use K, M, or G)", s, suffix)
	}

	intVal, err := strconv.ParseUint(intDigits, 10, 64)
	if err != nil {
		// intDigits is all-ASCII digits, so the only possible failure is range.
		return 0, fmt.Errorf("bitrate %q: value overflows uint64", s)
	}
	if mult > 1 && intVal > math.MaxUint64/mult {
		return 0, fmt.Errorf("bitrate %q: value overflows uint64", s)
	}
	total := intVal * mult

	if fracDigits != "" {
		if len(fracDigits) > 9 {
			return 0, fmt.Errorf("bitrate %q: fraction finer than the %s unit", s, strings.ToUpper(suffix))
		}
		pow := uint64(1)
		for range len(fracDigits) {
			pow *= 10
		}
		if mult%pow != 0 {
			return 0, fmt.Errorf("bitrate %q: fraction finer than the %s unit", s, strings.ToUpper(suffix))
		}
		fracVal, err := strconv.ParseUint(fracDigits, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("bitrate %q: invalid fraction: %v", s, err)
		}
		fracPart := fracVal * (mult / pow) // fracVal < pow, so fracPart < mult <= 1e9: no overflow
		if total > math.MaxUint64-fracPart {
			return 0, fmt.Errorf("bitrate %q: value overflows uint64", s)
		}
		total += fracPart
	}

	if total == 0 {
		return 0, fmt.Errorf("bitrate %q: must be greater than zero", s)
	}
	return Bitrate(total), nil
}

// String renders the bitrate with the largest decimal suffix that needs at
// most one decimal digit ("1G", "2.5G", "800M", "10M"); values that do not
// divide cleanly fall back to bare digits. It round-trips through
// ParseBitrate for every value CableCheck produces.
func (b Bitrate) String() string {
	v := uint64(b)
	if v == 0 {
		return "0"
	}
	for _, u := range bitrateUnits {
		if v%u.mult == 0 {
			return strconv.FormatUint(v/u.mult, 10) + u.suffix
		}
		tenth := u.mult / 10
		if v >= u.mult && v%tenth == 0 {
			return fmt.Sprintf("%d.%d%s", v/u.mult, (v%u.mult)/tenth, u.suffix)
		}
	}
	return strconv.FormatUint(v, 10)
}

// MarshalJSON encodes the bitrate as {"bps":<uint64>,"text":"<String()>"} so
// consumers get both the machine value and the human form.
func (b Bitrate) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Bps  uint64 `json:"bps"`
		Text string `json:"text"`
	}{uint64(b), b.String()})
}

// UnmarshalJSON accepts the {"bps":...} object form, a bare JSON number of
// bits per second, or a string such as "2.5G" (parsed via ParseBitrate).
// JSON null leaves the value unchanged.
func (b *Bitrate) UnmarshalJSON(data []byte) error {
	trim := bytes.TrimSpace(data)
	if len(trim) == 0 {
		return errors.New("bitrate: empty JSON value")
	}
	if string(trim) == "null" {
		return nil
	}
	switch trim[0] {
	case '{':
		var obj struct {
			Bps *uint64 `json:"bps"`
		}
		if err := json.Unmarshal(trim, &obj); err != nil {
			return fmt.Errorf("bitrate: %w", err)
		}
		if obj.Bps == nil {
			return errors.New(`bitrate: object form requires a "bps" field`)
		}
		*b = Bitrate(*obj.Bps)
	case '"':
		var s string
		if err := json.Unmarshal(trim, &s); err != nil {
			return fmt.Errorf("bitrate: %w", err)
		}
		v, err := ParseBitrate(s)
		if err != nil {
			return err
		}
		*b = v
	default:
		var n uint64
		if err := json.Unmarshal(trim, &n); err != nil {
			return fmt.Errorf("bitrate: %w", err)
		}
		*b = Bitrate(n)
	}
	return nil
}
