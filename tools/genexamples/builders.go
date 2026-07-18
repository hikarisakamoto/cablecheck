package main

import (
	"time"

	"cablecheck/internal/model"
)

// tcpResult builds a one-way TCP throughput result. cpu lets host-limited
// scenarios pin a hot CPU; other scenarios pass cleanCPU.
func tcpResult(direction string, sender, receiver float64, cpu model.CPUUsage) model.TCPResult {
	return model.TCPResult{
		Direction:             direction,
		Duration:              model.Duration(30 * time.Second),
		ParallelStreams:       4,
		SenderBitsPerSecond:   sender,
		ReceiverBitsPerSecond: receiver,
		Retransmissions:       u64(0),
		ThroughputVariation:   0.012,
		MinimumIntervalBps:    receiver - 3e6,
		MaximumIntervalBps:    receiver + 2e6,
		CPUUtilization:        cpu,
	}
}

// udpResult builds a one-way UDP loss/jitter result at the given target and
// achieved rates.
func udpResult(direction string, target uint64, actual float64, loss float64, lost, total int64) model.UDPResult {
	return model.UDPResult{
		Direction:         direction,
		TargetBps:         target,
		ActualSenderBps:   actual,
		ActualReceiverBps: actual,
		LostPackets:       lost,
		TotalPackets:      total,
		LossPercent:       loss,
		JitterMs:          0.11,
		OutOfOrder:        i64(0),
		CPU:               cleanCPU,
	}
}

// baseConfig is the effective configuration echoed by every quick-mode seed
// (the token never has a field, so it can never appear).
func baseConfig() model.ConfigEcho {
	return model.ConfigEcho{
		Role:            "pc1",
		LocalIP:         "192.168.100.1",
		PeerIP:          "192.168.100.2",
		Interface:       "enp3s0",
		Mode:            "quick",
		ControlPort:     44300,
		IperfPort:       44301,
		TCPDuration:     model.Duration(30 * time.Second),
		UDPDuration:     model.Duration(20 * time.Second),
		UDPRate:         model.Bitrate(800_000_000),
		ParallelStreams: 4,
		PingCount:       500,
		PingInterval:    model.Duration(20 * time.Millisecond),
		TCPRepeats:      1,
		MonitorInterval: model.Duration(time.Second),
		Output:          ".",
		TokenGenerated:  true,
	}
}

// baseReport assembles the shared report skeleton for one scenario: fixed
// timestamps, fixed testId (example-<name>), quick-mode config, and healthy
// e1000e peers. Callers overwrite the fields specific to their scenario.
func baseReport(name string) *model.Report {
	return &model.Report{
		SchemaVersion:   model.SchemaVersion,
		ToolVersion:     "1.0.0",
		ProtocolVersion: "1",
		TestID:          "example-" + name,
		StartedAt:       fixedTime,
		FinishedAt:      fixedTime.Add(90 * time.Second),
		Duration:        model.Duration(90 * time.Second),
		Configuration:   baseConfig(),
		PC1:             gigPeer("alpha", "enp3s0", "aa:bb:cc:00:11:22"),
		PC2:             gigPeer("bravo", "enp4s0", "aa:bb:cc:00:33:44"),
		Link: &model.LinkSection{
			PC1: model.LinkEndpoint{Before: gigLink(), After: gigLink()},
			PC2: model.LinkEndpoint{Before: gigLink(), After: gigLink()},
		},
	}
}
