package reporting

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cablecheck/internal/model"
)

// update rewrites the golden files instead of comparing against them:
//
//	go test ./internal/reporting -update
var update = flag.Bool("update", false, "rewrite golden files under testdata/golden")

// goldenPath locates a golden file relative to this package directory.
func goldenPath(name string) string {
	return filepath.Join("..", "..", "testdata", "golden", name)
}

// checkGolden compares got against the named golden file, rewriting the file
// when -update is set.
func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := goldenPath(name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir golden dir: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", name, err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (regenerate with: go test ./internal/reporting -update)", name, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("output does not match golden %s (regenerate with -update if the change is intended)\ngot:\n%s", name, got)
	}
}

// u64 and i64 return pointers for optional report fields.
func u64(v uint64) *uint64 { return &v }
func i64(v int64) *int64   { return &v }

// goldenReport builds the deterministic healthy-run seed used by the golden
// tests: fixed timestamps, fixed IDs, every section populated except the
// cable test (skipped, to exercise the "Not run" path).
func goldenReport() *model.Report {
	started := time.Date(2026, 7, 15, 21, 30, 5, 0, time.UTC)
	finished := started.Add(90 * time.Second)
	score := 100

	std := func(crc, resets uint64) map[string]uint64 {
		return map[string]uint64{
			"rx_crc":      crc,
			"rx_frame":    0,
			"rx_align":    0,
			"rx_missed":   0,
			"rx_fifo":     0,
			"link_resets": resets,
		}
	}
	snapshot := func(at time.Time, crc, resets uint64) *model.CounterSnapshot {
		return &model.CounterSnapshot{
			CapturedAt: at,
			Standard:   std(crc, resets),
			Driver:     map[string]uint64{"rx_crc_errors": crc, "rx_packets": 8_412_345},
			IPStats: model.IPStats64{
				RX: model.IPRxStats{Bytes: 11_000_000_000, Packets: 8_412_345},
				TX: model.IPTxStats{Bytes: 11_200_000_000, Packets: 8_400_120, CarrierChanges: resets},
			},
		}
	}
	deltas := model.CounterDeltaSet{
		"rx_crc":      {Delta: 0, OK: true},
		"rx_frame":    {Delta: 0, OK: true},
		"rx_align":    {Delta: 0, OK: true},
		"rx_missed":   {Delta: 0, OK: true},
		"rx_fifo":     {Delta: 0, OK: true},
		"link_resets": {Delta: 0, OK: true},
	}
	linkState := &model.LinkSettings{
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
	peer := func(host, nic, mac string) model.PeerReport {
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
	cpu := model.CPUUsage{
		HostTotal: 12.5, HostUser: 3.1, HostSystem: 9.4,
		RemoteTotal: 9.8, RemoteUser: 2.2, RemoteSystem: 7.6,
	}
	ping := func(direction, target string) model.PingResult {
		return model.PingResult{
			Direction:   direction,
			Target:      target,
			Transmitted: 500,
			Received:    500,
			RTTMinMs:    0.18, RTTAvgMs: 0.21, RTTMaxMs: 0.35, RTTMdevMs: 0.02,
			Percentiles:     map[int]float64{50: 0.21, 90: 0.24, 95: 0.26, 99: 0.31},
			IntervalUsedSec: 0.02,
		}
	}
	tcp := func(direction string, sender, receiver float64) model.TCPResult {
		return model.TCPResult{
			Direction:             direction,
			Duration:              model.Duration(10 * time.Second),
			ParallelStreams:       1,
			SenderBitsPerSecond:   sender,
			ReceiverBitsPerSecond: receiver,
			Retransmissions:       u64(0),
			ThroughputVariation:   0.012,
			MinimumIntervalBps:    receiver - 3e6,
			MaximumIntervalBps:    receiver + 2e6,
			CPUUtilization:        cpu,
		}
	}
	udp := func(direction string, actual float64) model.UDPResult {
		return model.UDPResult{
			Direction:         direction,
			TargetBps:         800_000_000,
			ActualSenderBps:   actual,
			ActualReceiverBps: actual,
			LostPackets:       0,
			TotalPackets:      67_934,
			JitterMs:          0.11,
			OutOfOrder:        i64(0),
			CPU:               cpu,
		}
	}

	return &model.Report{
		SchemaVersion:   "1.0.0",
		ToolVersion:     "1.0.0",
		ProtocolVersion: "1",
		TestID:          "ct-20260715-213005-a1b2c3d4",
		StartedAt:       started,
		FinishedAt:      finished,
		Duration:        model.Duration(90 * time.Second),
		Configuration: model.ConfigEcho{
			Role:            "pc1",
			LocalIP:         "192.168.100.1",
			PeerIP:          "192.168.100.2",
			Interface:       "enp3s0",
			Mode:            "standard",
			ControlPort:     51999,
			IperfPort:       52001,
			TCPDuration:     model.Duration(10 * time.Second),
			UDPDuration:     model.Duration(10 * time.Second),
			UDPRate:         model.Bitrate(800_000_000),
			ParallelStreams: 1,
			PingCount:       500,
			PingInterval:    model.Duration(20 * time.Millisecond),
			TCPRepeats:      1,
			MonitorInterval: model.Duration(500 * time.Millisecond),
			Output:          ".",
			TokenGenerated:  true,
		},
		PC1: peer("alpha", "enp3s0", "aa:bb:cc:00:11:22"),
		PC2: peer("bravo", "enp4s0", "aa:bb:cc:00:33:44"),
		Tests: model.TestsSection{
			Ping: []model.PingResult{
				ping(model.DirectionPC1ToPC2, "192.168.100.2"),
				ping(model.DirectionPC2ToPC1, "192.168.100.1"),
			},
			FullSizePing: []model.PingResult{
				ping(model.DirectionPC1ToPC2, "192.168.100.2"),
				ping(model.DirectionPC2ToPC1, "192.168.100.1"),
			},
			TCP: []model.TCPResult{
				tcp(model.DirectionPC1ToPC2, 941_200_000, 941_000_000),
				tcp(model.DirectionPC2ToPC1, 939_400_000, 939_100_000),
			},
			UDP: []model.UDPResult{
				udp(model.DirectionPC1ToPC2, 799_800_000),
				udp(model.DirectionPC2ToPC1, 799_500_000),
			},
			Bidirectional: &model.BidirResult{
				Duration:        model.Duration(10 * time.Second),
				ParallelStreams: 1,
				PC1ToPC2:        model.BidirDirection{SenderBitsPerSecond: 884_000_000, ReceiverBitsPerSecond: 883_500_000, Retransmissions: u64(0)},
				PC2ToPC1:        model.BidirDirection{SenderBitsPerSecond: 881_200_000, ReceiverBitsPerSecond: 880_900_000, Retransmissions: u64(0)},
				CPUUtilization:  cpu,
			},
		},
		InitialCounters: model.PeerCounters{
			PC1: snapshot(started, 3, 12),
			PC2: snapshot(started, 0, 9),
		},
		FinalCounters: model.PeerCounters{
			PC1: snapshot(finished, 3, 12),
			PC2: snapshot(finished, 0, 9),
		},
		CounterDeltas: model.PeerCounterDeltas{PC1: deltas, PC2: deltas},
		SkippedTests: []model.SkippedTest{
			{Name: "cable_test", Reason: "requires root (rerun with sudo)"},
		},
		Classification:        model.HealthExcellent,
		Score:                 &score,
		ClassificationReasons: []string{"all tests passed with no anomalies"},
		Link: &model.LinkSection{
			PC1: model.LinkEndpoint{Before: linkState, After: linkState},
			PC2: model.LinkEndpoint{Before: linkState, After: linkState},
		},
		RawFiles: []model.RawFileRef{
			{Name: "raw/01-pc1-ethtool-link-before.txt", SHA256: "0f4ad9e2cf0f4a1a3b2c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f809aa1", Bytes: 1832, Description: "pc1 ethtool link-before"},
			{Name: "raw/02-pc1-ip-stats-before.json", SHA256: "1a2b3c4d5e6f708192a3b4c5d6e7f8090f4ad9e2cf0f4a1a3b2c5d6e7f8091b2", Bytes: 2210, Description: "pc1 ip stats-before"},
			{Name: "raw/03-pc1-iperf3-tcp-pc1-to-pc2.json", SHA256: "2b3c4d5e6f708192a3b4c5d6e7f8090f4ad9e2cf0f4a1a3b2c5d6e7f8091c3d4", Bytes: 9184, Description: "pc1 iperf3 tcp-pc1-to-pc2"},
		},
	}
}

func TestMarkdownGoldenHealthy(t *testing.T) {
	checkGolden(t, "report-healthy.md", RenderMarkdown(goldenReport()))
}

func TestSummaryGolden(t *testing.T) {
	got := RenderSummary(goldenReport())
	checkGolden(t, "summary-healthy.txt", got)

	lines := bytes.Count(got, []byte("\n"))
	if lines > 30 {
		t.Errorf("summary has %d lines, want at most 30", lines)
	}
	for i, b := range got {
		if b > 0x7f {
			t.Errorf("summary contains non-ASCII byte %#x at offset %d", b, i)
			break
		}
	}
}

// TestMarkdownSectionOrder asserts that all 23 section headers are present in
// order — both on a fully populated report and on an empty one (absent data
// must render a note, never drop a section: numbering is stable).
func TestMarkdownSectionOrder(t *testing.T) {
	headers := []string{
		"## 1. Overall Result",
		"## 2. Score & Rule Evidence",
		"## 3. Session Info",
		"## 4. Machines & Environment",
		"## 5. Interface & Link Negotiation",
		"## 6. Link Events Timeline",
		"## 7. Counter Baseline",
		"## 8. Counter Deltas",
		"## 9. Ping Stability",
		"## 10. Full-Size Ping",
		"## 11. TCP Throughput PC1→PC2",
		"## 12. TCP Throughput PC2→PC1",
		"## 13. Bidirectional Stress",
		"## 14. UDP Loss & Jitter",
		"## 15. CPU Utilization",
		"## 16. Cable Diagnostics",
		"## 17. Monitoring Timeline",
		"## 18. Findings Detail",
		"## 19. Recommendations",
		"## 20. Limitations & Unavailable Tests",
		"## 21. Configuration Used",
		"## 22. Tool Versions",
		"## 23. Raw Artifact Index",
	}
	reports := map[string]*model.Report{
		"healthy": goldenReport(),
		"empty":   {},
	}
	for name, r := range reports {
		t.Run(name, func(t *testing.T) {
			out := RenderMarkdown(r)
			pos := -1
			for _, h := range headers {
				idx := bytes.Index(out, []byte(h))
				if idx < 0 {
					t.Errorf("missing section header %q", h)
					continue
				}
				if idx <= pos {
					t.Errorf("section header %q out of order (offset %d after %d)", h, idx, pos)
				}
				pos = idx
			}
		})
	}
}
