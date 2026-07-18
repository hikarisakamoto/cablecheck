package main

import (
	"time"

	"cablecheck/internal/model"
)

// seedHealthy: a flawless gigabit run — link up, clean counters both sides,
// ~941 Mb/s TCP both ways, lossless UDP at the 800 Mb/s target. → EXCELLENT.
func seedHealthy() *model.Report {
	r := baseReport("healthy")
	clean := snapshot(0, 0, 0)
	r.InitialCounters = model.PeerCounters{PC1: snapshot(0, 0, 0), PC2: snapshot(0, 0, 0)}
	r.FinalCounters = model.PeerCounters{PC1: clean, PC2: clean}
	r.CounterDeltas = model.PeerCounterDeltas{PC1: deltaSet(0, 0, 0), PC2: deltaSet(0, 0, 0)}
	r.Tests = model.TestsSection{
		Ping: []model.PingResult{
			cleanPing(model.DirectionPC1ToPC2, "192.168.100.2"),
			cleanPing(model.DirectionPC2ToPC1, "192.168.100.1"),
		},
		TCP: []model.TCPResult{
			tcpResult(model.DirectionPC1ToPC2, 941_200_000, 941_000_000, cleanCPU),
			tcpResult(model.DirectionPC2ToPC1, 939_400_000, 939_100_000, cleanCPU),
		},
		UDP: []model.UDPResult{
			udpResult(model.DirectionPC1ToPC2, 800_000_000, 799_800_000, 0, 0, 67_934),
			udpResult(model.DirectionPC2ToPC1, 800_000_000, 799_500_000, 0, 0, 67_931),
		},
	}
	return r
}

// seedReducedSpeed: both NICs advertise 1000BASE-T but the link came up at
// 100 Mb/s with otherwise clean physical evidence. PHY-06 fires WARNING; TCP
// runs at ~94 Mb/s of the 100 Mb/s link (>=90%, no PERF-01). → WARNING.
func seedReducedSpeed() *model.Report {
	r := baseReport("reduced-speed")
	// Link negotiated 100 Mb/s full duplex, but both sides still support
	// 1000BASE-T, so ExpectedSpeed stays 1 Gb/s and PHY-06 fires.
	// Both NICs still support and advertise 1000BASE-T (the partner is
	// gigabit-capable); the link nonetheless negotiated 100 Mb/s, so
	// ExpectedSpeed stays 1 Gb/s and PHY-06 fires.
	link100 := &model.LinkSettings{
		SpeedMbps:       100,
		Duplex:          "full",
		LinkDetected:    true,
		AutoNeg:         "on",
		Port:            "Twisted Pair",
		SupportedModes:  []string{"10baseT/Half", "100baseT/Full", "1000baseT/Full"},
		AdvertisedModes: []string{"1000baseT/Full"},
		PartnerModes:    []string{"1000baseT/Full"},
		MDIX:            "on (auto)",
	}
	r.Link = &model.LinkSection{
		PC1: model.LinkEndpoint{Before: link100, After: link100},
		PC2: model.LinkEndpoint{Before: link100, After: link100},
	}
	r.PC1.NIC.SpeedMbps = 100
	r.PC2.NIC.SpeedMbps = 100
	clean := snapshot(0, 0, 0)
	r.InitialCounters = model.PeerCounters{PC1: snapshot(0, 0, 0), PC2: snapshot(0, 0, 0)}
	r.FinalCounters = model.PeerCounters{PC1: clean, PC2: clean}
	r.CounterDeltas = model.PeerCounterDeltas{PC1: deltaSet(0, 0, 0), PC2: deltaSet(0, 0, 0)}
	r.Tests = model.TestsSection{
		Ping: []model.PingResult{
			cleanPing(model.DirectionPC1ToPC2, "192.168.100.2"),
			cleanPing(model.DirectionPC2ToPC1, "192.168.100.1"),
		},
		TCP: []model.TCPResult{
			tcpResult(model.DirectionPC1ToPC2, 94_200_000, 94_100_000, cleanCPU),
			tcpResult(model.DirectionPC2ToPC1, 94_000_000, 93_900_000, cleanCPU),
		},
		UDP: []model.UDPResult{
			udpResult(model.DirectionPC1ToPC2, 80_000_000, 79_800_000, 0, 0, 6_793),
			udpResult(model.DirectionPC2ToPC1, 80_000_000, 79_500_000, 0, 0, 6_790),
		},
	}
	return r
}

// seedCRCErrors: a gigabit run where PC1's CRC-class counters climbed by the
// e1000e CRC fixture's deltas (rx_crc_errors +1543, rx_align_errors +12) while
// PC2 stayed clean. PHY-02 fires in the >1000 band. → FAILED (POOR/FAILED
// pinned as acceptable). Everything else is clean, so the finding is defensible.
func seedCRCErrors() *model.Report {
	r := baseReport("crc-errors")
	// Deltas taken from testdata/ethtool/stats_e1000e_crc.txt minus the clean
	// baseline: rx_crc_errors 1543, rx_align_errors 12.
	const crc, align = 1543, 12
	r.InitialCounters = model.PeerCounters{PC1: snapshot(0, 0, 0), PC2: snapshot(0, 0, 0)}
	r.FinalCounters = model.PeerCounters{PC1: snapshot(crc, align, 0), PC2: snapshot(0, 0, 0)}
	r.CounterDeltas = model.PeerCounterDeltas{PC1: deltaSet(crc, align, 0), PC2: deltaSet(0, 0, 0)}
	r.Tests = model.TestsSection{
		Ping: []model.PingResult{
			cleanPing(model.DirectionPC1ToPC2, "192.168.100.2"),
			cleanPing(model.DirectionPC2ToPC1, "192.168.100.1"),
		},
		TCP: []model.TCPResult{
			tcpResult(model.DirectionPC1ToPC2, 930_400_000, 930_100_000, cleanCPU),
			tcpResult(model.DirectionPC2ToPC1, 928_900_000, 928_500_000, cleanCPU),
		},
		UDP: []model.UDPResult{
			udpResult(model.DirectionPC1ToPC2, 800_000_000, 799_600_000, 0, 0, 67_930),
			udpResult(model.DirectionPC2ToPC1, 800_000_000, 799_300_000, 0, 0, 67_928),
		},
	}
	return r
}

// seedHostLimited: throughput far below the link (PERF-01 POOR, host-sensitive)
// with the CPU pinned above 90% (HOST-01) and a spotless physical layer. The
// performance POOR is softened to INCONCLUSIVE — the cable was never proven
// bad. → INCONCLUSIVE (nil score).
func seedHostLimited() *model.Report {
	r := baseReport("host-limited")
	hotCPU := model.CPUUsage{
		HostTotal: 98.4, HostUser: 71.2, HostSystem: 27.2,
		RemoteTotal: 42.0, RemoteUser: 18.0, RemoteSystem: 24.0,
	}
	clean := snapshot(0, 0, 0)
	r.InitialCounters = model.PeerCounters{PC1: snapshot(0, 0, 0), PC2: snapshot(0, 0, 0)}
	r.FinalCounters = model.PeerCounters{PC1: clean, PC2: clean}
	r.CounterDeltas = model.PeerCounterDeltas{PC1: deltaSet(0, 0, 0), PC2: deltaSet(0, 0, 0)}
	r.Tests = model.TestsSection{
		Ping: []model.PingResult{
			cleanPing(model.DirectionPC1ToPC2, "192.168.100.2"),
			cleanPing(model.DirectionPC2ToPC1, "192.168.100.1"),
		},
		TCP: []model.TCPResult{
			// ~250 Mb/s on a 1 Gb/s link: ratio 0.25 -> PERF-01 POOR.
			tcpResult(model.DirectionPC1ToPC2, 251_000_000, 250_000_000, hotCPU),
			tcpResult(model.DirectionPC2ToPC1, 249_000_000, 248_000_000, hotCPU),
		},
		UDP: []model.UDPResult{
			udpResult(model.DirectionPC1ToPC2, 800_000_000, 799_400_000, 0, 0, 67_926),
			udpResult(model.DirectionPC2ToPC1, 800_000_000, 799_100_000, 0, 0, 67_924),
		},
	}
	return r
}

// seedFailed: the link was down when testing ended (PHY-01 FAILED) after
// bouncing four times (PHY-03 >=3 -> FAILED). No traffic completed. → FAILED.
func seedFailed() *model.Report {
	r := baseReport("failed")
	r.Partial = true
	// Link dropped: after-state shows no carrier on PC1.
	downLink := &model.LinkSettings{
		SpeedMbps:    -1,
		Duplex:       "unknown",
		LinkDetected: false,
		AutoNeg:      "on",
		Port:         "Twisted Pair",
	}
	r.Link = &model.LinkSection{
		PC1: model.LinkEndpoint{Before: gigLink(), After: downLink},
		PC2: model.LinkEndpoint{Before: gigLink(), After: downLink},
	}
	// PC1 saw four carrier changes during the run.
	r.InitialCounters = model.PeerCounters{PC1: snapshot(0, 0, 0), PC2: snapshot(0, 0, 0)}
	r.FinalCounters = model.PeerCounters{PC1: snapshot(0, 0, 4), PC2: snapshot(0, 0, 4)}
	r.CounterDeltas = model.PeerCounterDeltas{PC1: deltaSet(0, 0, 4), PC2: deltaSet(0, 0, 4)}
	r.MonitoringEvents = []model.MonitoringEvent{
		{At: fixedTime.Add(10 * time.Second), Type: "carrier_lost", Detail: "carrier lost on pc1 enp3s0"},
		{At: fixedTime.Add(12 * time.Second), Type: "carrier_restored", Detail: "carrier restored on pc1 enp3s0"},
		{At: fixedTime.Add(40 * time.Second), Type: "carrier_lost", Detail: "carrier lost on pc1 enp3s0"},
	}
	r.SkippedTests = []model.SkippedTest{
		{Name: "tcp_throughput", Reason: "link went down before the TCP phase completed"},
		{Name: "udp_loss", Reason: "link went down before the UDP phase"},
	}
	return r
}
