package model

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// TestNoRawDurationInReport walks the full Report type tree by reflection and
// fails on any field whose type is the raw time.Duration. Durations in the
// report must use model.Duration so they marshal as {"ms":...,"text":"..."}.
// time.Time fields are allowed (they marshal as RFC 3339).
func TestNoRawDurationInReport(t *testing.T) {
	banned := reflect.TypeOf(time.Duration(0))
	timeType := reflect.TypeOf(time.Time{})
	visited := map[reflect.Type]bool{}

	var walk func(typ reflect.Type, path string)
	walk = func(typ reflect.Type, path string) {
		if typ == banned {
			t.Errorf("%s is a raw time.Duration; use model.Duration instead", path)
			return
		}
		if typ == timeType {
			return // time.Time is fine; do not descend into its internals
		}
		if visited[typ] {
			return
		}
		visited[typ] = true
		switch typ.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(typ.Elem(), path+"[]")
		case reflect.Map:
			walk(typ.Key(), path+"{key}")
			walk(typ.Elem(), path+"{value}")
		case reflect.Struct:
			for i := 0; i < typ.NumField(); i++ {
				f := typ.Field(i)
				walk(f.Type, path+"."+f.Name)
			}
		}
	}
	walk(reflect.TypeOf(Report{}), "Report")
}

// fullReport builds a Report with every field populated so the JSON
// round-trip test exercises the whole type tree. All times are UTC and all
// durations are whole milliseconds (the JSON surface is millisecond-grained).
func fullReport() *Report {
	started := time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC)
	finished := time.Date(2026, 7, 15, 9, 3, 10, 0, time.UTC)
	score := 92
	retrans := uint64(12)
	intervalRetrans := uint64(2)
	bidirRetrans := uint64(7)
	outOfOrder := int64(3)

	nic1 := NICReport{Name: "enp3s0", Driver: "e1000e", SpeedMbps: 1000, Duplex: "full", MTU: 1500, MAC: "aa:bb:cc:dd:ee:01", USB: false}
	nic2 := NICReport{Name: "enp4s0", Driver: "r8169", SpeedMbps: 1000, Duplex: "full", MTU: 1500, MAC: "aa:bb:cc:dd:ee:02", USB: true}
	pc1 := PeerReport{
		Hostname: "alpha", Kernel: "6.9.1-arch1", OS: "linux/amd64",
		NIC:          nic1,
		ToolVersions: map[string]string{"iperf3": "3.16", "ethtool": "6.7", "ping": "iputils-20240117"},
	}
	pc2 := PeerReport{
		Hostname: "beta", Kernel: "6.8.0-generic", OS: "linux/amd64",
		NIC:          nic2,
		ToolVersions: map[string]string{"iperf3": "3.12", "ethtool": "6.2", "ping": "iputils-20211215"},
	}

	linkBefore := LinkSettings{
		SpeedMbps: 1000, Duplex: "full", LinkDetected: true, AutoNeg: "on",
		Port:            "Twisted Pair",
		SupportedPorts:  []string{"TP", "MII"},
		SupportedModes:  []string{"100baseT/Full", "1000baseT/Full"},
		AdvertisedModes: []string{"1000baseT/Full"},
		PartnerModes:    []string{"1000baseT/Full"},
		MDIX:            "on (auto)",
		Raw:             map[string]string{"Wake-on": "d"},
	}
	linkAfter := linkBefore

	ipStats := IPStats64{
		RX: IPRxStats{Bytes: 123456789, Packets: 98765, Errors: 2, Dropped: 1, OverErrors: 0, Multicast: 5, LengthErrors: 0, CRCErrors: 2, FrameErrors: 0, FifoErrors: 0, MissedErrors: 0, Nohandler: 0},
		TX: IPTxStats{Bytes: 987654321, Packets: 87654, Errors: 0, Dropped: 0, CarrierErrors: 0, Collisions: 0, AbortedErrors: 0, FifoErrors: 0, WindowErrors: 0, HeartbeatErrors: 0, CarrierChanges: 2},
	}
	snapBefore := &CounterSnapshot{
		CapturedAt: started,
		Standard:   map[string]uint64{"rx_crc": 0, "link_resets": 2},
		Driver:     map[string]uint64{"rx_crc_errors": 0, "rx_missed_errors": 4},
		Raw:        "NIC statistics:\n     rx_crc_errors: 0\n",
		IPStats:    ipStats,
	}
	snapAfter := &CounterSnapshot{
		CapturedAt: finished,
		Standard:   map[string]uint64{"rx_crc": 2, "link_resets": 2},
		Driver:     map[string]uint64{"rx_crc_errors": 2, "rx_missed_errors": 4},
		Raw:        "NIC statistics:\n     rx_crc_errors: 2\n",
		IPStats:    ipStats,
	}
	deltas := CounterDeltaSet{
		"rx_crc":      {Delta: 2, OK: true},
		"link_resets": {Delta: 0, OK: true},
		"rx_missed":   {Delta: 0, OK: false}, // reset/wrap: delta unreliable
	}

	ping := PingResult{
		Direction: DirectionPC1ToPC2, Target: "192.168.50.11",
		Transmitted: 500, Received: 499, Duplicates: 1, SendErrors: 0, IcmpErrors: 0,
		LossPercent: 0.2,
		RTTMinMs:    0.101, RTTAvgMs: 0.204, RTTMaxMs: 2.5, RTTMdevMs: 0.06,
		Percentiles:          map[int]float64{50: 0.2, 90: 0.31, 95: 0.4, 99: 1.1},
		Spikes:               []PingSpike{{Seq: 42, RTTMs: 2.5}},
		MissingSeqRuns:       []SeqRun{{FirstSeq: 100, Len: 1}},
		LongestMissingRunLen: 1,
		LongestGapMs:         44.5,
		IntervalUsedSec:      0.02,
		ExitCode:             1,
		UnparsedLines:        0,
	}

	cpu := CPUUsage{HostTotal: 12.5, HostUser: 4.5, HostSystem: 8.0, RemoteTotal: 33.1, RemoteUser: 10.0, RemoteSystem: 23.1}
	tcp := TCPResult{
		Direction:             DirectionPC1ToPC2,
		Duration:              Duration(30 * time.Second),
		ParallelStreams:       4,
		SenderBitsPerSecond:   941_000_000,
		ReceiverBitsPerSecond: 939_500_000,
		Retransmissions:       &retrans,
		IntervalResults: []TCPInterval{
			{StartSec: 0, EndSec: 1, Bytes: 117_625_000, BitsPerSecond: 941_000_000, Retransmits: &intervalRetrans},
			{StartSec: 1, EndSec: 2, Bytes: 117_400_000, BitsPerSecond: 939_200_000},
		},
		ThroughputVariation: 0.013,
		MinimumIntervalBps:  912_000_000,
		MaximumIntervalBps:  948_000_000,
		CPUUtilization:      cpu,
		Warnings:            []string{"first interval excluded from variation analysis"},
	}

	udp := UDPResult{
		Direction:         DirectionPC2ToPC1,
		TargetBps:         800_000_000,
		ActualSenderBps:   799_800_000,
		ActualReceiverBps: 799_100_000,
		LostPackets:       11,
		TotalPackets:      1_360_544,
		LossPercent:       0.0008,
		JitterMs:          0.021,
		OutOfOrder:        &outOfOrder,
		CPU:               cpu,
	}

	bidir := &BidirResult{
		Duration:        Duration(60 * time.Second),
		ParallelStreams: 4,
		PC1ToPC2:        BidirDirection{SenderBitsPerSecond: 899_000_000, ReceiverBitsPerSecond: 897_000_000, Retransmissions: &bidirRetrans},
		PC2ToPC1:        BidirDirection{SenderBitsPerSecond: 901_000_000, ReceiverBitsPerSecond: 900_500_000},
		CPUUtilization:  cpu,
		Warnings:        []string{"derived from per-stream sums"},
	}

	cable := &CableTestResult{
		Available: true,
		Pairs: []CablePairResult{
			{Pair: "A", Status: PairOK, RawCode: "OK"},
			{Pair: "C", Status: PairOpen, RawCode: "Open Circuit", FaultMeters: 32, HasFault: true},
		},
	}

	return &Report{
		SchemaVersion:   "1.0.0",
		ToolVersion:     "1.2.3",
		ProtocolVersion: "1",
		TestID:          "ct-20260715-090000-a1b2c3d4",
		StartedAt:       started,
		FinishedAt:      finished,
		Duration:        Duration(190 * time.Second),
		Configuration: ConfigEcho{
			Role: "pc1", LocalIP: "192.168.50.10", PeerIP: "192.168.50.11",
			Interface: "enp3s0", Mode: "standard",
			ControlPort: 44300, IperfPort: 44301,
			TCPDuration: Duration(60 * time.Second), UDPDuration: Duration(30 * time.Second),
			UDPRate: 800_000_000, ParallelStreams: 4,
			PingCount: 1500, PingInterval: Duration(20 * time.Millisecond), TCPRepeats: 2,
			MonitorInterval: Duration(time.Second),
			CableTest:       true, CableTestTDR: false,
			Output:  "/home/user/reports",
			Verbose: true, NonInteractive: false, NoSudo: false,
			NoReportTransfer: false, AllowVirtualInterface: false,
			TokenGenerated: true,
		},
		PC1: pc1,
		PC2: pc2,
		Tests: TestsSection{
			Ping:          []PingResult{ping},
			FullSizePing:  []PingResult{ping},
			TCP:           []TCPResult{tcp},
			UDP:           []UDPResult{udp},
			Bidirectional: bidir,
			CableTest:     cable,
		},
		InitialCounters: PeerCounters{PC1: snapBefore, PC2: snapBefore},
		FinalCounters:   PeerCounters{PC1: snapAfter, PC2: snapAfter},
		CounterDeltas:   PeerCounterDeltas{PC1: deltas, PC2: deltas},
		MonitoringEvents: []MonitoringEvent{
			{At: started.Add(45 * time.Second), Type: "carrier_lost", Detail: "carrier 1 -> 0"},
			{At: started.Add(47 * time.Second), Type: "carrier_restored", Detail: "renegotiated at 1000 Mb/s full"},
		},
		Warnings:              []string{"report transfer to pc2 failed; local copy is complete"},
		SkippedTests:          []SkippedTest{{Name: "cable_test_tdr", Reason: "requires root (sudo unavailable)"}},
		Classification:        HealthGood,
		Score:                 &score,
		ClassificationReasons: []string{"CRC-class errors within warning band"},
		Recommendations:       []string{"re-seat the cable at both ends"},
		Partial:               false,
		Failure:               &FailureDetails{Stage: "report_transfer", Error: "peer closed connection"},
		Findings: []Finding{
			{
				RuleID:   "PHY-02",
				Category: CategoryPhysical,
				Severity: SevWarning,
				Text:     "CRC-class errors detected during test",
				Evidence: []string{"pc1 enp3s0: rx_crc_errors +2 during test"},
			},
			{
				RuleID:        "HOST-01",
				Category:      CategoryHost,
				Severity:      SevMarker,
				Text:          "receiver CPU above 90% during TCP test",
				Evidence:      []string{"pc2: remote_total 93.1%"},
				HostSensitive: true,
			},
		},
		RawFiles: []RawFileRef{
			{Name: "01-pc1-ethtool-link-before.txt", SHA256: "0f343b0931126a20f133d67c2b018a3b", Bytes: 1420, Description: "ethtool link settings before test"},
		},
		Link: &LinkSection{
			PC1: LinkEndpoint{Before: &linkBefore, After: &linkAfter},
			PC2: LinkEndpoint{Before: &linkBefore, After: &linkAfter},
		},
		Machines: &MachinePair{PC1: pc1, PC2: pc2},
	}
}

func TestReportJSONRoundTrip(t *testing.T) {
	r := fullReport()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got Report
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(*r, got) {
		t.Errorf("round trip mismatch:\n want %+v\n  got %+v", *r, got)
	}

	// schemaVersion must be present verbatim on the JSON surface.
	if !bytes.Contains(data, []byte(`"schemaVersion":"1.0.0"`)) {
		t.Errorf("JSON missing schemaVersion: %.200s", data)
	}
	// Timestamps must marshal as RFC 3339.
	if !bytes.Contains(data, []byte(`"startedAt":"2026-07-15T09:00:00Z"`)) {
		t.Errorf("startedAt is not RFC 3339: %.200s", data)
	}
	if !bytes.Contains(data, []byte(`"finishedAt":"2026-07-15T09:03:10Z"`)) {
		t.Errorf("finishedAt is not RFC 3339: %.200s", data)
	}
	// Durations must use the two-field object form.
	if !bytes.Contains(data, []byte(`"duration":{"ms":190000,"text":"3m10s"}`)) {
		t.Errorf("duration is not the {ms,text} object: %.200s", data)
	}
	// The token must be unrepresentable: no "token" key anywhere.
	if bytes.Contains(data, []byte(`"token":`)) {
		t.Errorf("report JSON contains a token field")
	}
	// PingResult keeps the schema 1.0.0 wire name even though the Go field
	// was clarified to LongestMissingRunLen.
	if !bytes.Contains(data, []byte(`"longestSeqGap":1`)) {
		t.Errorf("JSON missing stable longestSeqGap wire key: %.500s", data)
	}
	if bytes.Contains(data, []byte(`"longestMissingRunLen":`)) {
		t.Errorf("JSON contains breaking longestMissingRunLen wire key: %.500s", data)
	}
	// Severity must marshal as a string.
	if !bytes.Contains(data, []byte(`"severity":"warning"`)) {
		t.Errorf("severity did not marshal as a string: %.200s", data)
	}

	// Spec-verbatim top-level keys must all be present.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatalf("Unmarshal top-level: %v", err)
	}
	for _, key := range []string{
		"schemaVersion", "toolVersion", "protocolVersion", "testId",
		"startedAt", "finishedAt", "duration", "configuration",
		"pc1", "pc2", "tests", "initialCounters", "finalCounters",
		"counterDeltas", "monitoringEvents", "warnings", "skippedTests",
		"classification", "score", "classificationReasons",
		"recommendations", "partial", "failure",
		"findings", "rawFiles", "link", "machines",
	} {
		if _, ok := top[key]; !ok {
			t.Errorf("top-level JSON key %q missing", key)
		}
	}

	// The configuration object must have no token key by construction.
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(top["configuration"], &cfg); err != nil {
		t.Fatalf("Unmarshal configuration: %v", err)
	}
	if _, ok := cfg["token"]; ok {
		t.Errorf("configuration contains a token key")
	}
}

// TestScoreNullForInconclusive pins that a nil score marshals as JSON null
// (the field stays present) — INCONCLUSIVE reports have no score.
func TestScoreNullForInconclusive(t *testing.T) {
	r := fullReport()
	r.Score = nil
	r.Classification = HealthInconclusive
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !bytes.Contains(data, []byte(`"score":null`)) {
		t.Errorf("nil score must marshal as \"score\":null")
	}
	var got Report
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Score != nil {
		t.Errorf("score = %d, want nil", *got.Score)
	}
}
