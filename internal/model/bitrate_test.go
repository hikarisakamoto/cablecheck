package model

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestParseBitrate(t *testing.T) {
	valid := []struct {
		in   string
		want Bitrate
	}{
		{"80M", 80_000_000},
		{"800M", 800_000_000},
		{"1G", 1_000_000_000},
		{"2.5G", 2_500_000_000},
		{"100K", 100_000},
		{"5000000", 5_000_000}, // bare integer = bits/s
		{"2.5g", 2_500_000_000},
		{"80m", 80_000_000},
		{"100k", 100_000},
		{"1.5K", 1_500},
		{"9.5M", 9_500_000},
		{"0.5G", 500_000_000},
		{"18446744073709551615", Bitrate(math.MaxUint64)},
	}
	for _, tc := range valid {
		got, err := ParseBitrate(tc.in)
		if err != nil {
			t.Errorf("ParseBitrate(%q): unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseBitrate(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}

	invalid := []struct {
		in   string
		hint string // required substring of the error message ("" = any error)
	}{
		{"", ""},
		{"0", ""},
		{"0K", ""},
		{"0.0M", ""},
		{"-1G", ""},
		{"-5000000", ""},
		{"2.5", "suffix"},                    // bare fraction needs a unit
		{"1.2345K", "fraction"},              // fraction finer than the unit
		{"1GB", "bytes"},                     // bytes suffix hint
		{"80MB", "bytes"},                    //
		{"1KB", "bytes"},                     //
		{"2.5Gb", "bytes"},                   //
		{"1GiB", "bytes"},                    //
		{"1T", ""},                           // unknown unit
		{"abc", ""},                          //
		{"M", ""},                            // no digits
		{"1.G", ""},                          // empty fraction
		{".5G", ""},                          // no integer part
		{"1.5.5G", ""},                       // double dot
		{"1 G", ""},                          // whitespace
		{"18446744073709551616", "overflow"}, // MaxUint64+1, bare
		{"18446744074G", "overflow"},         // overflow detected before multiply
		{"999999999999G", "overflow"},        //
	}
	for _, tc := range invalid {
		got, err := ParseBitrate(tc.in)
		if err == nil {
			t.Errorf("ParseBitrate(%q) = %d, want error", tc.in, got)
			continue
		}
		if tc.hint != "" && !strings.Contains(err.Error(), tc.hint) {
			t.Errorf("ParseBitrate(%q) error %q does not contain %q", tc.in, err, tc.hint)
		}
	}
}

func TestBitrateString(t *testing.T) {
	cases := []struct {
		in   Bitrate
		want string
	}{
		{1_000_000_000, "1G"},
		{2_500_000_000, "2.5G"},
		{800_000_000, "800M"},
		{10_000_000, "10M"},
		{9_500_000, "9.5M"},
		{100_000_000, "100M"},
		{100_000, "100K"},
		{5_000_000, "5M"},
		{1_501, "1501"}, // bare digits fallback
		{999, "999"},    // bare digits fallback
		{0, "0"},
	}
	for _, tc := range cases {
		got := tc.in.String()
		if got != tc.want {
			t.Errorf("Bitrate(%d).String() = %q, want %q", uint64(tc.in), got, tc.want)
		}
		if tc.in == 0 {
			continue // "0" is not parseable (zero rejected); nothing produces it
		}
		back, err := ParseBitrate(got)
		if err != nil {
			t.Errorf("round trip ParseBitrate(%q): %v", got, err)
			continue
		}
		if back != tc.in {
			t.Errorf("round trip %d -> %q -> %d", uint64(tc.in), got, uint64(back))
		}
	}
}

func TestBitrateJSON(t *testing.T) {
	data, err := json.Marshal(Bitrate(2_500_000_000))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(data) != `{"bps":2500000000,"text":"2.5G"}` {
		t.Errorf("Marshal = %s, want {\"bps\":2500000000,\"text\":\"2.5G\"}", data)
	}

	unmarshal := []struct {
		in   string
		want Bitrate
	}{
		{`{"bps":2500000000,"text":"2.5G"}`, 2_500_000_000},
		{`{"bps":80000000}`, 80_000_000},
		{`800000000`, 800_000_000},
		{`"1G"`, 1_000_000_000},
		{`"2.5G"`, 2_500_000_000},
	}
	for _, tc := range unmarshal {
		var b Bitrate
		if err := json.Unmarshal([]byte(tc.in), &b); err != nil {
			t.Errorf("Unmarshal(%s): %v", tc.in, err)
			continue
		}
		if b != tc.want {
			t.Errorf("Unmarshal(%s) = %d, want %d", tc.in, b, tc.want)
		}
	}

	for _, bad := range []string{`"2.5"`, `"1GB"`, `true`, `{"text":"1G"}`, `-1`} {
		var b Bitrate
		if err := json.Unmarshal([]byte(bad), &b); err == nil {
			t.Errorf("Unmarshal(%s) = %d, want error", bad, b)
		}
	}
}
