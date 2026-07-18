package main

import (
	"time"

	"cablecheck/internal/model"
)

// fixedTime is the frozen timestamp every seed uses, so the generated
// artifacts are byte-stable across runs and machines. It matches the value in
// docs/design/clieval.md §9 (2026-01-02T15:04:05Z).
var fixedTime = time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)

// scenario pairs a scenario name (the examples/<name> directory) with its
// pre-evaluation seed report. The generator runs the real evaluate + reporting
// pipeline over each Report, so the committed artifacts are never hand-edited.
type scenario struct {
	// Name is the examples/<name> directory and the report's testId suffix.
	Name string
	// Report is the pre-evaluation seed (its Classification/Score/Findings are
	// filled by the pipeline, not by the seed).
	Report *model.Report
}

// seeds returns the five canonical scenarios in a fixed order: healthy,
// reduced-speed, crc-errors, host-limited, failed. Values are traceable to the
// testdata fixtures (e.g. crc-errors reuses the e1000e CRC-fixture deltas).
func seeds() []scenario {
	return []scenario{
		{Name: "healthy", Report: seedHealthy()},
		{Name: "reduced-speed", Report: seedReducedSpeed()},
		{Name: "crc-errors", Report: seedCRCErrors()},
		{Name: "host-limited", Report: seedHostLimited()},
		{Name: "failed", Report: seedFailed()},
	}
}

// u64 and i64 return pointers for optional report fields.
func u64(v uint64) *uint64 { return &v }
func i64(v int64) *int64   { return &v }

// stdCounters builds the normalized counter view keyed by the standard names
// FactsFromReport aggregates (rx_crc + rx_frame + rx_align drive CRC-class,
// link_resets drives carrier events).
func stdCounters(crc, align, resets uint64) map[string]uint64 {
	return map[string]uint64{
		"rx_crc":      crc,
		"rx_frame":    0,
		"rx_align":    align,
		"rx_missed":   0,
		"rx_fifo":     0,
		"link_resets": resets,
	}
}

// snapshot builds a counter snapshot at the fixed time with the given CRC,
// alignment and carrier-reset totals. The driver map and ipStats echo the
// e1000e fixtures so the raw numbers are recognizable.
func snapshot(crc, align, resets uint64) *model.CounterSnapshot {
	return &model.CounterSnapshot{
		CapturedAt: fixedTime,
		Standard:   stdCounters(crc, align, resets),
		Driver: map[string]uint64{
			"rx_crc_errors":   crc,
			"rx_align_errors": align,
			"rx_packets":      48_291_733,
			"tx_packets":      21_837_410,
		},
		IPStats: model.IPStats64{
			RX: model.IPRxStats{Bytes: 61_250_873_344, Packets: 48_291_733, CRCErrors: crc, FrameErrors: align},
			TX: model.IPTxStats{Bytes: 3_021_459_287, Packets: 21_837_410, CarrierChanges: resets},
		},
	}
}

// deltaSet mirrors snapshot deltas for the standard keys the evaluator reads.
func deltaSet(crc, align, resets uint64) model.CounterDeltaSet {
	return model.CounterDeltaSet{
		"rx_crc":      {Delta: crc, OK: true},
		"rx_frame":    {Delta: 0, OK: true},
		"rx_align":    {Delta: align, OK: true},
		"rx_missed":   {Delta: 0, OK: true},
		"rx_fifo":     {Delta: 0, OK: true},
		"link_resets": {Delta: resets, OK: true},
	}
}

// gigLink is a clean, fully-negotiated 1000BASE-T link state.
func gigLink() *model.LinkSettings {
	return &model.LinkSettings{
		SpeedMbps:       1000,
		Duplex:          "full",
		LinkDetected:    true,
		AutoNeg:         "on",
		Port:            "Twisted Pair",
		SupportedModes:  []string{"10baseT/Half", "100baseT/Full", "1000baseT/Full"},
		AdvertisedModes: []string{"1000baseT/Full"},
		PartnerModes:    []string{"1000baseT/Full"},
		MDIX:            "on (auto)",
	}
}

// gigPeer describes a healthy e1000e peer at 1 Gb/s.
func gigPeer(host, nic, mac string) model.PeerReport {
	return model.PeerReport{
		Hostname: host,
		Kernel:   "6.9.1-generic",
		OS:       "linux/amd64",
		NIC: model.NICReport{
			Name:      nic,
			Driver:    "e1000e",
			SpeedMbps: 1000,
			Duplex:    "full",
			MTU:       1500,
			MAC:       mac,
		},
		ToolVersions: map[string]string{
			"iperf3":  "3.16",
			"ethtool": "6.7",
			"ping":    "iputils-20240117",
		},
	}
}

// cleanCPU is a modest CPU utilization block for a NIC-bound gigabit run.
var cleanCPU = model.CPUUsage{
	HostTotal: 12.5, HostUser: 3.1, HostSystem: 9.4,
	RemoteTotal: 9.8, RemoteUser: 2.2, RemoteSystem: 7.6,
}

// cleanPing is a lossless 500-packet ping stability result in one direction.
func cleanPing(direction, target string) model.PingResult {
	return model.PingResult{
		Direction:       direction,
		Target:          target,
		Transmitted:     500,
		Received:        500,
		RTTMinMs:        0.18,
		RTTAvgMs:        0.21,
		RTTMaxMs:        0.35,
		RTTMdevMs:       0.02,
		Percentiles:     map[int]float64{50: 0.21, 90: 0.24, 95: 0.26, 99: 0.31},
		IntervalUsedSec: 0.02,
	}
}
