package evaluate

import (
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"cablecheck/internal/model"
)

// snap builds a counter snapshot with the given standard counters.
func snap(std map[string]uint64) model.CounterSnapshot {
	return model.CounterSnapshot{
		CapturedAt: time.Date(2026, 7, 15, 21, 30, 5, 0, time.UTC),
		Standard:   std,
	}
}

func TestCounterDelta(t *testing.T) {
	t.Run("normal", func(t *testing.T) {
		d, ok := CounterDelta(100, 150)
		if d != 50 || !ok {
			t.Errorf("CounterDelta(100, 150) = (%d, %v), want (50, true)", d, ok)
		}
	})

	t.Run("equal", func(t *testing.T) {
		d, ok := CounterDelta(7, 7)
		if d != 0 || !ok {
			t.Errorf("CounterDelta(7, 7) = (%d, %v), want (0, true)", d, ok)
		}
	})

	t.Run("wrap or reset yields zero not ok", func(t *testing.T) {
		d, ok := CounterDelta(150, 100)
		if d != 0 || ok {
			t.Errorf("CounterDelta(150, 100) = (%d, %v), want (0, false)", d, ok)
		}
	})

	t.Run("missing key on either side is absent and side delta not ok", func(t *testing.T) {
		before := snap(map[string]uint64{"rx_crc": 10, "rx_align": 5, "link_resets": 1})
		after := snap(map[string]uint64{"rx_crc": 12, "link_resets": 1})

		set, ok := DeltaSet(&before, &after)
		if ok {
			t.Errorf("DeltaSet with a key missing in after: ok = true, want false")
		}
		if got, present := set["rx_crc"]; !present || got.Delta != 2 || !got.OK {
			t.Errorf("set[rx_crc] = %+v (present=%v), want {Delta:2 OK:true}", got, present)
		}
		if _, present := set["rx_align"]; present {
			t.Errorf("set[rx_align] present, want key absent (missing in after snapshot)")
		}

		// The same capture through FactsFromReport must mark the side DeltaOK=false.
		r := &model.Report{
			InitialCounters: model.PeerCounters{PC1: &before},
			FinalCounters:   model.PeerCounters{PC1: &after},
		}
		f := FactsFromReport(r)
		if f.PC1.DeltaOK {
			t.Errorf("FactsFromReport: PC1.DeltaOK = true, want false when a key is missing on one side")
		}
	})

	t.Run("missing key in before only also not ok", func(t *testing.T) {
		before := snap(map[string]uint64{"rx_crc": 10})
		after := snap(map[string]uint64{"rx_crc": 10, "rx_align": 3})
		set, ok := DeltaSet(&before, &after)
		if ok {
			t.Errorf("DeltaSet with a key missing in before: ok = true, want false")
		}
		if _, present := set["rx_align"]; present {
			t.Errorf("set[rx_align] present, want key absent (missing in before snapshot)")
		}
	})

	t.Run("wrap inside set flips side DeltaOK", func(t *testing.T) {
		before := snap(map[string]uint64{"rx_crc": 100, "rx_frame": 1})
		after := snap(map[string]uint64{"rx_crc": 5, "rx_frame": 2})
		set, ok := DeltaSet(&before, &after)
		if ok {
			t.Errorf("DeltaSet with a wrapped counter: ok = true, want false")
		}
		if got := set["rx_crc"]; got.Delta != 0 || got.OK {
			t.Errorf("set[rx_crc] = %+v, want {Delta:0 OK:false}", got)
		}
		if got := set["rx_frame"]; got.Delta != 1 || !got.OK {
			t.Errorf("set[rx_frame] = %+v, want {Delta:1 OK:true}", got)
		}
	})

	t.Run("nil snapshot on either side means no data", func(t *testing.T) {
		before := snap(map[string]uint64{"rx_crc": 1})
		if set, ok := DeltaSet(&before, nil); ok || len(set) != 0 {
			t.Errorf("DeltaSet(before, nil) = (%v, %v), want (empty, false)", set, ok)
		}
		if set, ok := DeltaSet(nil, &before); ok || len(set) != 0 {
			t.Errorf("DeltaSet(nil, after) = (%v, %v), want (empty, false)", set, ok)
		}
	})

	t.Run("aggregation into side facts", func(t *testing.T) {
		before := snap(map[string]uint64{
			"rx_crc": 10, "rx_frame": 0, "rx_align": 1, "rx_symbol": 0,
			"jabber": 0, "oversize": 2, "undersize": 0, "rx_length": 0,
			"rx_fifo": 3, "rx_missed": 5, "tx_carrier": 7, "phy_errors": 11,
			"link_resets": 2,
		})
		after := snap(map[string]uint64{
			"rx_crc": 15, "rx_frame": 3, "rx_align": 1, "rx_symbol": 0,
			"jabber": 1, "oversize": 2, "undersize": 0, "rx_length": 4,
			"rx_fifo": 5, "rx_missed": 8, "tx_carrier": 9, "phy_errors": 16,
			"link_resets": 4,
		})
		r := &model.Report{
			InitialCounters: model.PeerCounters{PC1: &before, PC2: &before},
			FinalCounters:   model.PeerCounters{PC1: &after, PC2: &before},
		}
		f := FactsFromReport(r)
		if !f.PC1.DeltaOK {
			t.Errorf("PC1.DeltaOK = false, want true for a clean capture pair")
		}
		if !f.PC1.CountersAvailable {
			t.Errorf("PC1.CountersAvailable = false, want true")
		}
		if f.PC1.CRCClassErrors != 8 { // (15-10) + (3-0) + 0 + 0
			t.Errorf("PC1.CRCClassErrors = %d, want 8", f.PC1.CRCClassErrors)
		}
		if f.PC1.JabberSizeErrors != 5 { // 1 + 0 + 0 + 4
			t.Errorf("PC1.JabberSizeErrors = %d, want 5", f.PC1.JabberSizeErrors)
		}
		if f.PC1.FifoOverrun != 2 {
			t.Errorf("PC1.FifoOverrun = %d, want 2", f.PC1.FifoOverrun)
		}
		if f.PC1.MissedErrors != 3 {
			t.Errorf("PC1.MissedErrors = %d, want 3", f.PC1.MissedErrors)
		}
		if f.PC1.CarrierPHYErrors != 7 { // (9-7) + (16-11)
			t.Errorf("PC1.CarrierPHYErrors = %d, want 7", f.PC1.CarrierPHYErrors)
		}
		if f.PC1.CarrierEvents != 2 {
			t.Errorf("PC1.CarrierEvents = %d, want 2", f.PC1.CarrierEvents)
		}
	})

	t.Run("empty standard maps mean counters unavailable", func(t *testing.T) {
		before := snap(map[string]uint64{})
		after := snap(map[string]uint64{})
		r := &model.Report{
			InitialCounters: model.PeerCounters{PC1: &before},
			FinalCounters:   model.PeerCounters{PC1: &after},
		}
		f := FactsFromReport(r)
		if f.PC1.CountersAvailable {
			t.Errorf("PC1.CountersAvailable = true, want false for empty counter maps")
		}
		if f.PC2.CountersAvailable || f.PC2.DeltaOK {
			t.Errorf("PC2 = %+v, want zero facts for missing snapshots", f.PC2)
		}
	})
}

// TestPC2SelfInflictedCableFlapDoesNotFirePHY03 verifies the worker's own
// window count is subtracted only from the worker's reset counter.
func TestPC2SelfInflictedCableFlapDoesNotFirePHY03(t *testing.T) {
	before := snap(map[string]uint64{"link_resets": 10})
	after := snap(map[string]uint64{"link_resets": 12})
	report := &model.Report{
		InitialCounters: model.PeerCounters{PC2: &before},
		FinalCounters:   model.PeerCounters{PC2: &after},
		Tests: model.TestsSection{CableTest: &model.CableTestResult{
			Available:                  true,
			SelfInflictedCarrierEvents: model.PeerCarrierEvents{PC2: 2},
		}},
	}
	facts := FactsFromReport(report)
	if facts.PC2.CarrierEvents != 0 {
		t.Errorf("PC2.CarrierEvents = %d, want 0 after subtracting its cable-test flap", facts.PC2.CarrierEvents)
	}
	if finding := evaluateRule(ruleByID(t, "PHY-03"), facts); finding != nil {
		t.Errorf("PHY-03 = %+v, want no self-inflicted finding", finding)
	}
}

func TestPC2GenuineCarrierFaultStillFiresPHY03(t *testing.T) {
	before := snap(map[string]uint64{"link_resets": 10})
	after := snap(map[string]uint64{"link_resets": 13})
	report := &model.Report{
		InitialCounters: model.PeerCounters{PC2: &before},
		FinalCounters:   model.PeerCounters{PC2: &after},
		Tests: model.TestsSection{CableTest: &model.CableTestResult{
			Available:                  true,
			SelfInflictedCarrierEvents: model.PeerCarrierEvents{PC2: 2},
		}},
	}
	facts := FactsFromReport(report)
	if facts.PC2.CarrierEvents != 1 {
		t.Errorf("PC2.CarrierEvents = %d, want one genuine reset retained", facts.PC2.CarrierEvents)
	}
	if finding := evaluateRule(ruleByID(t, "PHY-03"), facts); finding == nil {
		t.Errorf("PHY-03 = nil, want genuine PC2 carrier fault finding")
	}
}

func TestCableWindowCarrierCountsIgnorePeerClockSkew(t *testing.T) {
	before := snap(map[string]uint64{"link_resets": 20})
	after := snap(map[string]uint64{"link_resets": 22})
	before.CapturedAt = time.Date(2026, 7, 19, 8, 0, 0, 0, time.UTC)
	after.CapturedAt = before.CapturedAt.Add(time.Second)
	report := &model.Report{
		InitialCounters: model.PeerCounters{PC2: &before},
		FinalCounters:   model.PeerCounters{PC2: &after},
		MonitoringEvents: []model.MonitoringEvent{{
			At: before.CapturedAt.Add(-12 * time.Hour), Type: "carrier_lost", SelfInflicted: true,
		}},
		Tests: model.TestsSection{CableTest: &model.CableTestResult{
			Available:                  true,
			SelfInflictedCarrierEvents: model.PeerCarrierEvents{PC2: 2},
		}},
	}
	if got := FactsFromReport(report).PC2.CarrierEvents; got != 0 {
		t.Errorf("PC2.CarrierEvents = %d under skewed clocks, want 0 from explicit worker count", got)
	}
}

func TestRequestedTDRUnavailabilityIsReportedAsLimitation(t *testing.T) {
	facts := FactsFromReport(&model.Report{Tests: model.TestsSection{
		CableTest: &model.CableTestResult{
			Available:            true,
			TDRUnavailableReason: "driver does not support cable test",
		},
	}})
	if !slices.Contains(facts.Unavailable, "cable_test_tdr") {
		t.Errorf("Unavailable = %q, want cable_test_tdr limitation", facts.Unavailable)
	}
}

func TestFactsFromReportDirections(t *testing.T) {
	r := &model.Report{
		Tests: model.TestsSection{
			Ping: []model.PingResult{
				{Direction: model.DirectionPC1ToPC2, LossPercent: 0.5, Duplicates: 2, LongestGapMs: 1500},
				{Direction: model.DirectionPC2ToPC1, LossPercent: 0},
			},
			UDP: []model.UDPResult{
				{Direction: model.DirectionPC1ToPC2, TargetBps: 800_000_000, ActualSenderBps: 795_000_000, LossPercent: 1.5, JitterMs: 0.2},
				{Direction: model.DirectionPC2ToPC1, TargetBps: 800_000_000, ActualSenderBps: 500_000_000, LossPercent: 8},
			},
		},
	}
	f := FactsFromReport(r)
	if got := f.Dir[0].PingLossPct; got != 0.5 {
		t.Errorf("Dir[0].PingLossPct = %v, want 0.5", got)
	}
	if got := f.Dir[0].PingDuplicates; got != 2 {
		t.Errorf("Dir[0].PingDuplicates = %v, want 2", got)
	}
	if got := f.Dir[0].PingMaxGap; got != 1500*time.Millisecond {
		t.Errorf("Dir[0].PingMaxGap = %v, want 1.5s", got)
	}
	if !f.Dir[0].UDPTargetReached {
		t.Errorf("Dir[0].UDPTargetReached = false, want true (99%% of target)")
	}
	if f.Dir[1].UDPTargetReached {
		t.Errorf("Dir[1].UDPTargetReached = true, want false (62%% of target)")
	}
	if math.Abs(f.Dir[1].UDPLossPct) > 1e-9 {
		t.Errorf("Dir[1].UDPLossPct = %v, want 0 because its only run missed target", f.Dir[1].UDPLossPct)
	}
}

func TestFactsFromReportAttributesCPUPerDirection(t *testing.T) {
	report := &model.Report{
		PC1: model.PeerReport{NIC: model.NICReport{SpeedMbps: 1000}},
		PC2: model.PeerReport{NIC: model.NICReport{SpeedMbps: 1000}},
		Tests: model.TestsSection{
			TCP: []model.TCPResult{
				{
					Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 900_000_000,
					CPUUtilization: model.CPUUsage{HostTotal: 95, RemoteTotal: 20},
				},
				{
					Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 910_000_000,
					CPUUtilization: model.CPUUsage{HostTotal: 50, RemoteTotal: 97},
				},
				{
					Direction: model.DirectionPC2ToPC1, ReceiverBitsPerSecond: 920_000_000,
					CPUUtilization: model.CPUUsage{HostTotal: 30, RemoteTotal: 80},
				},
			},
			UDP: []model.UDPResult{
				{
					Direction: model.DirectionPC1ToPC2, TargetBps: 400_000_000,
					ActualSenderBps: 400_000_000, CPU: model.CPUUsage{HostTotal: 91, RemoteTotal: 10},
				},
				{
					Direction: model.DirectionPC2ToPC1, TargetBps: 400_000_000,
					ActualSenderBps: 400_000_000, CPU: model.CPUUsage{HostTotal: 40, RemoteTotal: 45},
				},
				{
					Direction: model.DirectionPC2ToPC1, TargetBps: 400_000_000,
					ActualSenderBps: 100_000_000, CPU: model.CPUUsage{HostTotal: 99, RemoteTotal: 5},
				},
			},
			Bidirectional: &model.BidirResult{
				CPUUtilization: model.CPUUsage{HostTotal: 98, RemoteTotal: 15},
			},
		},
	}

	facts := FactsFromReport(report)
	if got := facts.Dir[0].TCPMaxCPUPct; got != 97 {
		t.Errorf("pc1->pc2 TCP max CPU = %.1f, want 97", got)
	}
	if got := facts.Dir[0].TCPSenderMaxCPUPct; got != 95 {
		t.Errorf("pc1->pc2 TCP sender max CPU = %.1f, want 95", got)
	}
	if got := facts.Dir[1].TCPMaxCPUPct; got != 80 {
		t.Errorf("pc2->pc1 TCP max CPU = %.1f, want 80", got)
	}
	if got := facts.Dir[1].TCPSenderMaxCPUPct; got != 30 {
		t.Errorf("pc2->pc1 TCP sender max CPU = %.1f, want 30", got)
	}
	if got := facts.Dir[0].UDPMaxCPUPct; got != 91 {
		t.Errorf("pc1->pc2 qualifying UDP max CPU = %.1f, want 91", got)
	}
	if got := facts.Dir[1].UDPMaxCPUPct; got != 45 {
		t.Errorf("pc2->pc1 qualifying UDP max CPU = %.1f, want 45; throttled run must not contribute", got)
	}
	if got := facts.MaxCPUPct; got != 99 {
		t.Errorf("global max CPU = %.1f, want 99 from all throughput diagnostics", got)
	}
}

func TestEvaluateDirectionalCPUGatingFromReport(t *testing.T) {
	before := &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 0}}
	after := &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 0}}
	report := &model.Report{
		PC1: model.PeerReport{NIC: model.NICReport{SpeedMbps: 1000}},
		PC2: model.PeerReport{NIC: model.NICReport{SpeedMbps: 1000}},
		Tests: model.TestsSection{
			TCP: []model.TCPResult{
				{
					Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 941_000_000,
					Collapses:      []model.TCPCollapseEvent{},
					CPUUtilization: model.CPUUsage{HostTotal: 95, RemoteTotal: 20},
				},
				{
					Direction: model.DirectionPC2ToPC1, ReceiverBitsPerSecond: 600_000_000,
					ThroughputVariation: 0.20, Collapses: []model.TCPCollapseEvent{{Len: 2}},
					CPUUtilization: model.CPUUsage{HostTotal: 20, RemoteTotal: 20},
				},
			},
			UDP: []model.UDPResult{
				{
					Direction: model.DirectionPC1ToPC2, TargetBps: 400_000_000,
					ActualSenderBps: 400_000_000, LossPercent: 5,
					CPU: model.CPUUsage{HostTotal: 95, RemoteTotal: 20},
				},
				{
					Direction: model.DirectionPC2ToPC1, TargetBps: 400_000_000,
					ActualSenderBps: 400_000_000, LossPercent: 1,
					CPU: model.CPUUsage{HostTotal: 20, RemoteTotal: 20},
				},
			},
		},
		InitialCounters: model.PeerCounters{PC1: before, PC2: before},
		FinalCounters:   model.PeerCounters{PC1: after, PC2: after},
	}

	result := Evaluate(FactsFromReport(report))
	if result.Class != model.HealthWarning {
		t.Fatalf("class = %v, want WARNING (findings %v)", result.Class, findingIDs(result))
	}
	if result.Score == nil || *result.Score != 70 {
		t.Errorf("score = %v, want 70 from only clean-direction UDP loss, CoV, collapses, and throughput", result.Score)
	}
	for _, ruleID := range []string{"TR-07", "PERF-01", "PERF-02", "PERF-03", "PERF-04", "HOST-01"} {
		if !hasFinding(result.Findings, ruleID) {
			t.Errorf("findings = %v, want %s", findingIDs(result), ruleID)
		}
	}
	tr07 := findingByID(result, "TR-07")
	if tr07 == nil {
		t.Fatal("TR-07 = nil, want clean-direction UDP finding")
	}
	evidence := strings.Join(tr07.Evidence, " ")
	if strings.Contains(evidence, "pc1->pc2") || !strings.Contains(evidence, "pc2->pc1") {
		t.Errorf("TR-07 evidence = %q, want only clean pc2->pc1 direction", evidence)
	}
}

func TestFactsFromReportUnknownDirectionsDoNotPopulateDirectionalCPU(t *testing.T) {
	report := &model.Report{Tests: model.TestsSection{
		TCP: []model.TCPResult{{
			Direction: "unknown", ReceiverBitsPerSecond: 900_000_000,
			CPUUtilization: model.CPUUsage{HostTotal: 99, RemoteTotal: 98},
		}},
		UDP: []model.UDPResult{{
			Direction: "unknown", TargetBps: 100, ActualSenderBps: 100,
			CPU: model.CPUUsage{HostTotal: 97, RemoteTotal: 96},
		}},
	}}

	facts := FactsFromReport(report)
	for i, direction := range facts.Dir {
		if direction.TCPMaxCPUPct != 0 || direction.TCPSenderMaxCPUPct != 0 || direction.UDPMaxCPUPct != 0 {
			t.Errorf("direction %d CPU facts = %+v, want zero for unknown result directions", i, direction)
		}
	}
	if facts.MaxCPUPct != 99 {
		t.Errorf("global max CPU = %.1f, want 99 diagnostic maximum", facts.MaxCPUPct)
	}
}

func TestLowerMedian(t *testing.T) {
	values := []float64{9, 1, 7, 5}
	tests := []struct {
		name   string
		values []float64
		want   float64
	}{
		{name: "empty", want: 0},
		{name: "singleton", values: []float64{7}, want: 7},
		{name: "two selects lower", values: []float64{9, 1}, want: 1},
		{name: "odd", values: []float64{9, 1, 5}, want: 5},
		{name: "even selects lower middle", values: values, want: 5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := slices.Clone(tc.values)
			if got := lowerMedian(tc.values); got != tc.want {
				t.Errorf("lowerMedian(%v) = %v, want %v", tc.values, got, tc.want)
			}
			if !slices.Equal(tc.values, before) {
				t.Errorf("lowerMedian mutated input: got %v, want %v", tc.values, before)
			}
		})
	}
}

func TestFactsFromReportUsesLowerMedianForRepeatedTCPResults(t *testing.T) {
	before := &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 0}}
	after := &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 0}}
	trial := func(bitrate float64, cov float64, retrans uint64, collapse bool) model.TCPResult {
		intervals := []model.TCPInterval{{BitsPerSecond: 100_000_000, Bytes: 1_448_000}}
		collapses := []model.TCPCollapseEvent{}
		if collapse {
			collapses = []model.TCPCollapseEvent{{StartSec: 2, Len: 1, MinBps: 10_000_000}}
		}
		return model.TCPResult{
			Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: bitrate,
			ThroughputVariation: cov, Retransmissions: uint64Ptr(retrans),
			IntervalResults: intervals, Collapses: collapses,
		}
	}
	r := &model.Report{
		PC1: model.PeerReport{NIC: model.NICReport{Name: "eth0", Driver: "e1000e", SpeedMbps: 1000}},
		PC2: model.PeerReport{NIC: model.NICReport{Name: "eth1", Driver: "r8169", SpeedMbps: 1000}},
		Tests: model.TestsSection{
			TCP: []model.TCPResult{
				trial(900_000_000, 0.4, 40, false),
				trial(100_000_000, 0.1, 10, false),
				trial(700_000_000, 0.3, 30, true),
				trial(500_000_000, 0.2, 20, false),
				{
					Direction: model.DirectionPC2ToPC1, SenderBitsPerSecond: 920_000_000,
				},
			},
		},
		InitialCounters: model.PeerCounters{PC1: before, PC2: before},
		FinalCounters:   model.PeerCounters{PC1: after, PC2: after},
	}

	f := FactsFromReport(r)
	if got := f.Dir[0].TCPBitrate; got != 500_000_000 {
		t.Fatalf("pc1->pc2 TCP bitrate = %s, want lower median 500 Mbit/s", got)
	}
	if got := f.Dir[0].TCPCoV; got != 0.2 {
		t.Errorf("pc1->pc2 TCP variation = %v, want lower median 0.2", got)
	}
	if got := f.Dir[0].TCPRetransRate; math.Abs(got-0.02) > 1e-12 {
		t.Errorf("pc1->pc2 retransmit rate = %v, want lower median 0.02", got)
	}
	if got := f.Dir[0].TCPCollapses; got != 1 {
		t.Errorf("pc1->pc2 collapses = %d, want maximum 1", got)
	}
	if got := f.Dir[0].TCPTrialCount; got != 4 {
		t.Errorf("pc1->pc2 trial count = %d, want 4", got)
	}
	if got := f.Dir[0].TCPThroughputDeviations; got != 3 {
		t.Errorf("pc1->pc2 throughput deviations = %d, want 3", got)
	}
	if got := f.Dir[1].TCPBitrate; got != 920_000_000 {
		t.Errorf("pc2->pc1 TCP bitrate = %s, want sender fallback 920 Mbit/s", got)
	}
	if got := f.Dir[1].TCPTrialCount; got != 1 {
		t.Errorf("pc2->pc1 trial count = %d, want 1", got)
	}
}

func TestCollapseIntervalCountUsesAuthoritativeEventsAndLegacyFallback(t *testing.T) {
	qualifyingIntervals := []model.TCPInterval{
		{StartSec: 0, BitsPerSecond: 100},
		{StartSec: 1, BitsPerSecond: 100},
		{StartSec: 2, BitsPerSecond: 10},
		{StartSec: 3, BitsPerSecond: 100},
	}
	tests := []struct {
		name string
		tr   model.TCPResult
		want int
	}{
		{
			name: "event lengths not event count",
			tr: model.TCPResult{Collapses: []model.TCPCollapseEvent{
				{Len: 2}, {Len: 3}, {Len: 0}, {Len: -1},
			}},
			want: 5,
		},
		{
			name: "authoritative empty ignores diagnostic intervals",
			tr:   model.TCPResult{IntervalResults: qualifyingIntervals, Collapses: []model.TCPCollapseEvent{}},
			want: 0,
		},
		{
			name: "legacy unavailable evidence uses canonical fallback",
			tr:   model.TCPResult{IntervalResults: qualifyingIntervals, Collapses: nil},
			want: 1,
		},
		{
			name: "carried evidence does not depend on retained intervals",
			tr:   model.TCPResult{Collapses: []model.TCPCollapseEvent{{Len: 2}}},
			want: 2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := collapseIntervalCount(tc.tr); got != tc.want {
				t.Errorf("collapseIntervalCount() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestTCPCollapseTotalSaturatesAcrossDirections(t *testing.T) {
	const maxInt = int(^uint(0) >> 1)
	facts := &Facts{Dir: [2]DirFacts{{TCPCollapses: maxInt}, {TCPCollapses: 1}}}
	if got := tcpCollapseTotal(facts); got != maxInt {
		t.Errorf("tcpCollapseTotal = %d, want saturated %d", got, maxInt)
	}
	facts.Dir[0].TCPCollapses = -1
	if got := tcpCollapseTotal(facts); got != 1 {
		t.Errorf("tcpCollapseTotal with malformed negative = %d, want 1", got)
	}
}

func TestFactsFromReportKeepsWorstRepeatedCollapseIntervalCount(t *testing.T) {
	r := &model.Report{Tests: model.TestsSection{TCP: []model.TCPResult{
		{
			Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 900_000_000,
			Collapses: []model.TCPCollapseEvent{{Len: 2}, {Len: 1}},
		},
		{
			Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 900_000_000,
			Collapses: []model.TCPCollapseEvent{{Len: 2}},
		},
	}}}

	facts := FactsFromReport(r)
	if got := facts.Dir[0].TCPCollapses; got != 3 {
		t.Errorf("TCPCollapses = %d, want maximum per-trial interval count 3", got)
	}
}

func TestFactsFromReportExcludesMissingRetransmissionSamples(t *testing.T) {
	withRate := func(retrans *uint64) model.TCPResult {
		return model.TCPResult{
			Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 900_000_000,
			Retransmissions: retrans,
			IntervalResults: []model.TCPInterval{{Bytes: 1_448_000}},
		}
	}
	high := uint64(20)
	zero := uint64(0)
	tests := []struct {
		name   string
		trials []model.TCPResult
		want   float64
	}{
		{name: "missing plus high keeps high", trials: []model.TCPResult{withRate(nil), withRate(&high)}, want: 0.02},
		{name: "explicit zero participates", trials: []model.TCPResult{withRate(&zero), withRate(&high)}, want: 0},
		{name: "all missing has no rate", trials: []model.TCPResult{withRate(nil), withRate(nil)}, want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			facts := FactsFromReport(&model.Report{Tests: model.TestsSection{TCP: tc.trials}})
			if got := facts.Dir[0].TCPRetransRate; math.Abs(got-tc.want) > 1e-12 {
				t.Errorf("TCPRetransRate = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFactsFromReportKeepsWorstQualifyingRepeatedUDPResult(t *testing.T) {
	outOfOrder := int64(20)
	report := &model.Report{
		PC1: model.PeerReport{NIC: model.NICReport{SpeedMbps: 1000}},
		Tests: model.TestsSection{UDP: []model.UDPResult{
			{Direction: model.DirectionPC1ToPC2, TargetBps: 800_000_000, ActualSenderBps: 800_000_000, LossPercent: 1, JitterMs: 2, TotalPackets: 10_000},
			{Direction: model.DirectionPC1ToPC2, TargetBps: 400_000_000, ActualSenderBps: 400_000_000, LossPercent: 4, JitterMs: 7, TotalPackets: 10_000, OutOfOrder: &outOfOrder},
		}},
	}

	facts := FactsFromReport(report)
	d := facts.Dir[0]
	if !d.UDPTargetReached || d.UDPLossPct != 4 || d.UDPJitterMs != 7 || d.UDPOutOfOrderPct != 0.2 {
		t.Errorf("repeated UDP facts = %+v, want qualifying maxima loss=4 jitter=7 reorder=0.2", d)
	}
}

func TestEvaluateCapsOneOfTwoTCPThroughputOutliers(t *testing.T) {
	before := &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 0}}
	after := &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 0}}
	report := &model.Report{
		PC1: model.PeerReport{NIC: model.NICReport{SpeedMbps: 1000}},
		PC2: model.PeerReport{NIC: model.NICReport{SpeedMbps: 1000}},
		Tests: model.TestsSection{TCP: []model.TCPResult{
			{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 941_000_000},
			{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 300_000_000},
			{Direction: model.DirectionPC2ToPC1, ReceiverBitsPerSecond: 941_000_000},
		}},
		InitialCounters: model.PeerCounters{PC1: before, PC2: before},
		FinalCounters:   model.PeerCounters{PC1: after, PC2: after},
	}

	facts := FactsFromReport(report)
	if facts.Dir[0].TCPBitrate != 300_000_000 || facts.Dir[0].TCPTrialCount != 2 || facts.Dir[0].TCPThroughputDeviations != 1 {
		t.Fatalf("pc1->pc2 facts = %+v, want lower 300 Mbit/s with one of two deviations", facts.Dir[0])
	}
	result := Evaluate(facts)
	finding := findingByID(result, "PERF-01")
	if finding == nil || finding.Severity != model.SevWarning {
		t.Errorf("PERF-01 = %+v, want capped warning", finding)
	}
	if result.Class != model.HealthWarning {
		t.Errorf("class = %v, want WARNING (findings %v)", result.Class, findingIDs(result))
	}
	if result.Score == nil || *result.Score != Default().WarningScoreBand.Max {
		t.Errorf("score = %v, want warning-band maximum %d", result.Score, Default().WarningScoreBand.Max)
	}
}

func TestEvaluateCapsIsolatedOutlierOnLowSpeedLink(t *testing.T) {
	before := &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 0}}
	after := &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 0}}
	// The <=100M tier never passes silently (PassDisabled), so a line-rate
	// trial is annotated INFO. The deviation counter must still treat it as a
	// pass; otherwise the isolated-outlier cap could never fire on this tier.
	report := &model.Report{
		PC1: model.PeerReport{NIC: model.NICReport{SpeedMbps: 100}},
		PC2: model.PeerReport{NIC: model.NICReport{SpeedMbps: 100}},
		Tests: model.TestsSection{TCP: []model.TCPResult{
			{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 95_000_000},
			{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 30_000_000},
			{Direction: model.DirectionPC2ToPC1, ReceiverBitsPerSecond: 95_000_000},
		}},
		InitialCounters: model.PeerCounters{PC1: before, PC2: before},
		FinalCounters:   model.PeerCounters{PC1: after, PC2: after},
	}

	facts := FactsFromReport(report)
	if facts.Dir[0].TCPBitrate != 30_000_000 || facts.Dir[0].TCPTrialCount != 2 || facts.Dir[0].TCPThroughputDeviations != 1 {
		t.Fatalf("pc1->pc2 facts = %+v, want lower 30 Mbit/s with the line-rate trial not counted as a deviation", facts.Dir[0])
	}
	finding := findingByID(Evaluate(facts), "PERF-01")
	if finding == nil || finding.Severity != model.SevWarning {
		t.Errorf("PERF-01 = %+v, want capped warning on a <=100M link", finding)
	}
}

func TestAssessThroughputLeavesNonPoorAggregateUncapped(t *testing.T) {
	facts := cleanFacts()
	// 600 Mbit/s of a 1G link is a WARNING aggregate, already below POOR, so
	// the cap must not engage even with an isolated deviating trial.
	facts.Dir[0] = DirFacts{
		TCPAvailable:            true,
		TCPBitrate:              600_000_000,
		TCPTrialCount:           2,
		TCPThroughputDeviations: 1,
	}
	facts.Dir[1].TCPAvailable = false

	_, severity, deviation, capped := assessThroughput(facts, 0, Default())
	if !deviation || severity != model.SevWarning || capped {
		t.Errorf("assessThroughput = (sev %v, deviation %v, capped %v), want warning/true/false", severity, deviation, capped)
	}
	finding := rulePERF01(facts, Default())
	if finding == nil || finding.Severity != model.SevWarning {
		t.Fatalf("PERF-01 = %+v, want warning", finding)
	}
	if evidence := strings.Join(finding.Evidence, " "); strings.Contains(evidence, "trials missed policy") {
		t.Errorf("PERF-01 evidence = %q, want no cap annotation for a non-poor aggregate", evidence)
	}
}

func TestFactsFromReportSoakExpectsRepeatsTimesCompletedCycles(t *testing.T) {
	report := &model.Report{
		Configuration:       model.ConfigEcho{Mode: "soak", TCPRepeats: 2},
		SoakCyclesCompleted: 2,
		Tests: model.TestsSection{TCP: []model.TCPResult{
			{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 941_000_000},
			{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 940_000_000},
			{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 939_000_000},
			{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 938_000_000},
			{Direction: model.DirectionPC2ToPC1, ReceiverBitsPerSecond: 941_000_000},
			{Direction: model.DirectionPC2ToPC1, ReceiverBitsPerSecond: 940_000_000},
			{Direction: model.DirectionPC2ToPC1, ReceiverBitsPerSecond: 939_000_000},
		}},
	}

	facts := FactsFromReport(report)
	if !facts.Dir[0].TCPAvailable || facts.Dir[0].TCPTrialCount != 4 {
		t.Errorf("pc1->pc2 facts = %+v, want all 4 expected soak trials available", facts.Dir[0])
	}
	if facts.Dir[1].TCPAvailable || facts.Dir[1].TCPTrialCount != 0 {
		t.Errorf("pc2->pc1 facts = %+v, want unavailable after only 3 of 4 expected soak trials", facts.Dir[1])
	}
}

func TestFactsFromReportDoesNotMixLossyThrottledUDPWithCleanTargetRun(t *testing.T) {
	r := &model.Report{
		PC1: model.PeerReport{NIC: model.NICReport{SpeedMbps: 1000}},
		PC2: model.PeerReport{NIC: model.NICReport{SpeedMbps: 1000}},
		Tests: model.TestsSection{UDP: []model.UDPResult{
			{Direction: model.DirectionPC1ToPC2, TargetBps: 800_000_000, ActualSenderBps: 800_000_000},
			{Direction: model.DirectionPC1ToPC2, TargetBps: 400_000_000, ActualSenderBps: 200_000_000, LossPercent: 8},
		}},
	}

	f := FactsFromReport(r)
	if !f.Dir[0].UDPTargetReached {
		t.Fatalf("UDPTargetReached = false, want the clean qualifying run retained")
	}
	if got := f.Dir[0].UDPLossPct; got != 0 {
		t.Errorf("qualifying UDP loss = %v, want 0; throttled run must not contribute", got)
	}
	if ids := findingIDs(Evaluate(f)); slices.Contains(ids, "TR-07") {
		t.Errorf("findings = %v, want no TR-07 from a throttled lossy run", ids)
	}
}

func TestFactsFromReportEvaluatesReducedRateUDPWhenPrimaryNearSaturation(t *testing.T) {
	r := &model.Report{
		PC1: model.PeerReport{NIC: model.NICReport{SpeedMbps: 1000}},
		PC2: model.PeerReport{NIC: model.NICReport{SpeedMbps: 1000}},
		Tests: model.TestsSection{UDP: []model.UDPResult{
			{Direction: model.DirectionPC1ToPC2, TargetBps: 960_000_000, ActualSenderBps: 960_000_000},
			{Direction: model.DirectionPC1ToPC2, TargetBps: 400_000_000, ActualSenderBps: 400_000_000, LossPercent: 3},
		}},
	}

	f := FactsFromReport(r)
	if !f.UDPNearSaturation {
		t.Fatalf("UDPNearSaturation = false, want primary run limitation retained")
	}
	if !f.Dir[0].UDPTargetReached || f.Dir[0].UDPLossPct != 3 {
		t.Fatalf("qualifying reduced-rate facts = %+v, want target reached with 3%% loss", f.Dir[0])
	}
	if ids := findingIDs(Evaluate(f)); !slices.Contains(ids, "TR-07") {
		t.Errorf("findings = %v, want TR-07 from qualifying reduced-rate run", ids)
	}
}

func TestFactsFromReportAnyIncompleteTCPRepeatMakesDirectionUnavailable(t *testing.T) {
	r := &model.Report{Tests: model.TestsSection{TCP: []model.TCPResult{
		{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 941_000_000},
		{Direction: model.DirectionPC1ToPC2, Incomplete: true},
	}}}

	f := FactsFromReport(r)
	if f.Dir[0].TCPAvailable {
		t.Errorf("TCPAvailable = true, want false when any repeat is incomplete: %+v", f.Dir[0])
	}
	if f.Dir[0].TCPTrialCount != 0 || f.Dir[0].TCPThroughputDeviations != 0 {
		t.Errorf("incomplete TCP trial facts leaked into direction: %+v", f.Dir[0])
	}
}

func TestFactsFromReportMissingConfiguredTCPRepeatMakesDirectionUnavailable(t *testing.T) {
	report := &model.Report{
		Configuration: model.ConfigEcho{Mode: "standard", TCPRepeats: 2},
		Tests: model.TestsSection{TCP: []model.TCPResult{
			{
				Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 941_000_000,
				CPUUtilization: model.CPUUsage{HostTotal: 95, RemoteTotal: 20},
			},
			{Direction: model.DirectionPC2ToPC1, ReceiverBitsPerSecond: 941_000_000},
			{Direction: model.DirectionPC2ToPC1, ReceiverBitsPerSecond: 940_000_000},
		}},
	}

	facts := FactsFromReport(report)
	if facts.Dir[0].TCPAvailable || facts.Dir[0].TCPTrialCount != 0 ||
		facts.Dir[0].TCPMaxCPUPct != 0 || facts.Dir[0].TCPSenderMaxCPUPct != 0 {
		t.Errorf("pc1->pc2 facts = %+v, want unavailable after one of two configured repeats was absent", facts.Dir[0])
	}
	if !facts.Dir[1].TCPAvailable || facts.Dir[1].TCPTrialCount != 2 {
		t.Errorf("pc2->pc1 facts = %+v, want both configured repeats available", facts.Dir[1])
	}
}

func TestFactsFromReportIgnoresIncompleteTCPDirection(t *testing.T) {
	r := &model.Report{
		PC1:     model.PeerReport{NIC: model.NICReport{Name: "eth0", Driver: "e1000e", SpeedMbps: 1000}},
		PC2:     model.PeerReport{NIC: model.NICReport{Name: "eth0", Driver: "e1000e", SpeedMbps: 1000}},
		Partial: true,
		Tests: model.TestsSection{TCP: []model.TCPResult{
			{
				Direction:           model.DirectionPC1ToPC2,
				Incomplete:          true,
				SenderBitsPerSecond: 0,
				ThroughputVariation: 0.75,
				Retransmissions:     uint64Ptr(99),
				IntervalResults:     []model.TCPInterval{{BitsPerSecond: 0, Retransmits: uint64Ptr(99)}},
				Collapses:           []model.TCPCollapseEvent{{Len: 5}},
				CPUUtilization:      model.CPUUsage{HostTotal: 99, RemoteTotal: 98},
			},
			{
				Direction:             model.DirectionPC2ToPC1,
				ReceiverBitsPerSecond: 941_000_000,
			},
		}},
	}

	f := FactsFromReport(r)
	if f.Dir[0].TCPAvailable {
		t.Errorf("incomplete TCP direction marked available: %+v", f.Dir[0])
	}
	if f.Dir[0].TCPBitrate != 0 || f.Dir[0].TCPCoV != 0 || f.Dir[0].TCPCollapses != 0 || f.Dir[0].TCPRetransRate != 0 ||
		f.Dir[0].TCPMaxCPUPct != 0 || f.Dir[0].TCPSenderMaxCPUPct != 0 {
		t.Errorf("incomplete TCP metrics leaked into facts: %+v", f.Dir[0])
	}
	if f.MaxCPUPct != 0 {
		t.Errorf("MaxCPUPct = %v, want incomplete TCP CPU diagnostics ignored", f.MaxCPUPct)
	}
	res := Evaluate(f)
	if res.Class != model.HealthInconclusive {
		t.Errorf("class = %s, want INCONCLUSIVE for partial run (findings %v)", res.Class, findingIDs(res))
	}
	ids := findingIDs(res)
	if slices.Contains(ids, "PERF-01") {
		t.Errorf("findings %v contain PERF-01 for an incomplete placeholder", ids)
	}
	for _, want := range []string{"LIM-02", "LIM-03"} {
		if !slices.Contains(ids, want) {
			t.Errorf("findings %v lack %s for incomplete partial TCP evidence", ids, want)
		}
	}
}

// TestEvaluateOneIncompleteTCPDirectionCapsExcellent exercises the report
// boundary: one usable TCP measurement plus one incomplete placeholder is
// reduced coverage, not a fully clean two-direction run.
func TestEvaluateOneIncompleteTCPDirectionCapsExcellent(t *testing.T) {
	before := &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 0}}
	after := &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 0}}
	r := &model.Report{
		PC1: model.PeerReport{NIC: model.NICReport{Name: "eth0", Driver: "e1000e", SpeedMbps: 1000}},
		PC2: model.PeerReport{NIC: model.NICReport{Name: "eth1", Driver: "r8169", SpeedMbps: 1000}},
		Tests: model.TestsSection{TCP: []model.TCPResult{
			{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 941_000_000},
			{Direction: model.DirectionPC2ToPC1, Incomplete: true},
		}},
		InitialCounters: model.PeerCounters{PC1: before, PC2: before},
		FinalCounters:   model.PeerCounters{PC1: after, PC2: after},
		SkippedTests: []model.SkippedTest{{
			Name: "tcp", Reason: "TCP throughput PC2 to PC1 was incomplete",
		}},
	}

	f := FactsFromReport(r)
	if !f.Dir[0].TCPAvailable || f.Dir[1].TCPAvailable {
		t.Fatalf("TCP availability = [%v %v], want [true false]", f.Dir[0].TCPAvailable, f.Dir[1].TCPAvailable)
	}
	res := Evaluate(f)
	if res.Class != model.HealthGood {
		t.Errorf("class = %s, want GOOD rather than EXCELLENT for one incomplete TCP direction (findings %v)", res.Class, findingIDs(res))
	}
	if !slices.Contains(findingIDs(res), "LIM-02") {
		t.Errorf("findings = %v, want LIM-02", findingIDs(res))
	}
}

func uint64Ptr(v uint64) *uint64 { return &v }
