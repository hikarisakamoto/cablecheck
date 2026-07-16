package parser

import (
	"slices"
	"strings"
	"testing"
)

func TestEthtoolLink(t *testing.T) {
	t.Run("e1000e_1g", func(t *testing.T) {
		ls := ParseEthtoolSettings(fixture(t, "ethtool", "settings_e1000e_1g.txt"))
		if ls.SpeedMbps != 1000 {
			t.Errorf("SpeedMbps = %d, want 1000", ls.SpeedMbps)
		}
		if ls.Duplex != "full" {
			t.Errorf("Duplex = %q, want %q", ls.Duplex, "full")
		}
		if ls.AutoNeg != "on" {
			t.Errorf("AutoNeg = %q, want %q", ls.AutoNeg, "on")
		}
		if !ls.LinkDetected {
			t.Error("LinkDetected = false, want true")
		}
		if ls.Port != "Twisted Pair" {
			t.Errorf("Port = %q, want %q", ls.Port, "Twisted Pair")
		}
		if !slices.Equal(ls.SupportedPorts, []string{"TP"}) {
			t.Errorf("SupportedPorts = %v, want [TP]", ls.SupportedPorts)
		}
		wantModes := []string{
			"10baseT/Half", "10baseT/Full",
			"100baseT/Half", "100baseT/Full",
			"1000baseT/Full",
		}
		if !slices.Equal(ls.SupportedModes, wantModes) {
			t.Errorf("SupportedModes = %v, want %v", ls.SupportedModes, wantModes)
		}
		if !slices.Equal(ls.AdvertisedModes, wantModes) {
			t.Errorf("AdvertisedModes = %v, want %v", ls.AdvertisedModes, wantModes)
		}
		if !slices.Equal(ls.PartnerModes, wantModes) {
			t.Errorf("PartnerModes = %v, want %v", ls.PartnerModes, wantModes)
		}
		if ls.MDIX != "on (auto)" {
			t.Errorf("MDIX = %q, want %q", ls.MDIX, "on (auto)")
		}
		if got := ls.Raw["Transceiver"]; got != "internal" {
			t.Errorf(`Raw["Transceiver"] = %q, want "internal"`, got)
		}
		// The colon-free continuation of "Current message level" must be
		// appended to the same Raw entry, not treated as mode tokens.
		if got := ls.Raw["Current message level"]; !strings.Contains(got, "drv probe link") {
			t.Errorf(`Raw["Current message level"] = %q, want continuation "drv probe link" appended`, got)
		}
	})

	t.Run("r8169_2g5", func(t *testing.T) {
		ls := ParseEthtoolSettings(fixture(t, "ethtool", "settings_r8169_2g5.txt"))
		if ls.SpeedMbps != 2500 {
			t.Errorf("SpeedMbps = %d, want 2500", ls.SpeedMbps)
		}
		if ls.Duplex != "full" {
			t.Errorf("Duplex = %q, want %q", ls.Duplex, "full")
		}
		if !slices.Equal(ls.SupportedPorts, []string{"TP", "MII"}) {
			t.Errorf("SupportedPorts = %v, want [TP MII]", ls.SupportedPorts)
		}
		if !slices.Contains(ls.SupportedModes, "2500baseT/Full") {
			t.Errorf("SupportedModes = %v, want to contain 2500baseT/Full", ls.SupportedModes)
		}
		if !slices.Contains(ls.PartnerModes, "2500baseT/Full") {
			t.Errorf("PartnerModes = %v, want to contain 2500baseT/Full", ls.PartnerModes)
		}
		if got := ls.Raw["master-slave status"]; got != "slave" {
			t.Errorf(`Raw["master-slave status"] = %q, want "slave"`, got)
		}
	})

	t.Run("r8152_100m_half", func(t *testing.T) {
		ls := ParseEthtoolSettings(fixture(t, "ethtool", "settings_r8152_100m_half.txt"))
		if ls.SpeedMbps != 100 {
			t.Errorf("SpeedMbps = %d, want 100", ls.SpeedMbps)
		}
		if ls.Duplex != "half" {
			t.Errorf("Duplex = %q, want %q", ls.Duplex, "half")
		}
		if !slices.Equal(ls.PartnerModes, []string{"100baseT/Half"}) {
			t.Errorf("PartnerModes = %v, want [100baseT/Half]", ls.PartnerModes)
		}
		if ls.Port != "MII" {
			t.Errorf("Port = %q, want %q", ls.Port, "MII")
		}
		if !ls.LinkDetected {
			t.Error("LinkDetected = false, want true")
		}
	})

	t.Run("no_link", func(t *testing.T) {
		ls := ParseEthtoolSettings(fixture(t, "ethtool", "settings_no_link.txt"))
		if ls.SpeedMbps != -1 {
			t.Errorf(`SpeedMbps = %d, want -1 for "Speed: Unknown!" (never 0)`, ls.SpeedMbps)
		}
		if ls.Duplex != "unknown" {
			t.Errorf("Duplex = %q, want %q", ls.Duplex, "unknown")
		}
		if ls.LinkDetected {
			t.Error("LinkDetected = true, want false")
		}
		if ls.AutoNeg != "on" {
			t.Errorf("AutoNeg = %q, want %q", ls.AutoNeg, "on")
		}
		if len(ls.PartnerModes) != 0 {
			t.Errorf(`PartnerModes = %v, want empty for "Not reported"`, ls.PartnerModes)
		}
	})
}

func TestEthtoolStatsParse(t *testing.T) {
	t.Run("e1000e_crc", func(t *testing.T) {
		st := ParseEthtoolStats(fixture(t, "ethtool", "stats_e1000e_crc.txt"))
		want := map[string]uint64{
			"rx_crc_errors":         1543,
			"rx_align_errors":       12,
			"rx_missed_errors":      4,
			"rx_long_length_errors": 2,
			"rx_packets":            48291733,
		}
		for k, v := range want {
			if got, ok := st[k]; !ok || got != v {
				t.Errorf("stats[%q] = %d (present=%v), want %d", k, got, ok, v)
			}
		}
		if _, ok := st["NIC statistics"]; ok {
			t.Error("banner line leaked into the counter map")
		}
		if len(st) < 40 {
			t.Errorf("parsed %d counters, want the full e1000e list (>= 40)", len(st))
		}
	})

	t.Run("r8169_naming", func(t *testing.T) {
		st := ParseEthtoolStats(fixture(t, "ethtool", "stats_r8169.txt"))
		want := map[string]uint64{
			"align_errors": 41,
			"rx_missed":    9,
			"tx_aborted":   3,
			"rx_errors":    184,
		}
		for k, v := range want {
			if got, ok := st[k]; !ok || got != v {
				t.Errorf("stats[%q] = %d (present=%v), want %d", k, got, ok, v)
			}
		}
		if _, ok := st["rx_crc_errors"]; ok {
			t.Error("r8169 must not report rx_crc_errors (absent is not zero)")
		}
	})

	t.Run("virtio_per_queue_only", func(t *testing.T) {
		st := ParseEthtoolStats(fixture(t, "ethtool", "stats_virtio.txt"))
		if got := st["rx_queue_0_packets"]; got != 152034 {
			t.Errorf("stats[rx_queue_0_packets] = %d, want 152034", got)
		}
		for _, k := range []string{"rx_crc_errors", "rx_errors", "align_errors"} {
			if _, ok := st[k]; ok {
				t.Errorf("virtio must not expose %q", k)
			}
		}
	})

	t.Run("junk_ignored", func(t *testing.T) {
		in := []byte("NIC statistics:\n" +
			"Some section header\n" +
			"     rx_packets: 10\n" +
			"     hex_thing: 0x1f\n" +
			"     odd line without colon\n" +
			"     negative: -3\n" +
			"     dup: 1\n" +
			"     dup: 2\n")
		st := ParseEthtoolStats(in)
		if got := st["rx_packets"]; got != 10 {
			t.Errorf("stats[rx_packets] = %d, want 10", got)
		}
		if got := st["dup"]; got != 2 {
			t.Errorf("stats[dup] = %d, want 2 (last wins)", got)
		}
		for _, k := range []string{"Some section header", "hex_thing", "negative"} {
			if _, ok := st[k]; ok {
				t.Errorf("junk key %q leaked into the counter map", k)
			}
		}
	})
}

func TestEthtoolDriverInfo(t *testing.T) {
	di := ParseEthtoolDriverInfo(fixture(t, "ethtool", "driverinfo_e1000e.txt"))
	if got := di["driver"]; got != "e1000e" {
		t.Errorf(`driverinfo["driver"] = %q, want "e1000e"`, got)
	}
	if got := di["bus-info"]; got != "0000:00:1f.6" {
		t.Errorf(`driverinfo["bus-info"] = %q, want "0000:00:1f.6"`, got)
	}
	if got, ok := di["expansion-rom-version"]; !ok || got != "" {
		t.Errorf(`driverinfo["expansion-rom-version"] = %q (present=%v), want present and empty`, got, ok)
	}
}
