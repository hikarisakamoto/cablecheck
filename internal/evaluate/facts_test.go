package evaluate

import (
	"math"
	"slices"
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
			"rx_fifo": 3, "link_resets": 2,
		})
		after := snap(map[string]uint64{
			"rx_crc": 15, "rx_frame": 3, "rx_align": 1, "rx_symbol": 0,
			"jabber": 1, "oversize": 2, "undersize": 0, "rx_length": 4,
			"rx_fifo": 5, "link_resets": 4,
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
	if math.Abs(f.Dir[1].UDPLossPct-8) > 1e-9 {
		t.Errorf("Dir[1].UDPLossPct = %v, want 8", f.Dir[1].UDPLossPct)
	}
}

func TestFactsFromReportUsesWorstRepeatedTCPResult(t *testing.T) {
	before := &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 0}}
	after := &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 0}}
	r := &model.Report{
		PC1: model.PeerReport{NIC: model.NICReport{Name: "eth0", Driver: "e1000e", SpeedMbps: 1000}},
		PC2: model.PeerReport{NIC: model.NICReport{Name: "eth1", Driver: "r8169", SpeedMbps: 1000}},
		Tests: model.TestsSection{
			Ping: []model.PingResult{
				{Direction: model.DirectionPC1ToPC2},
				{Direction: model.DirectionPC1ToPC2, LossPercent: 3, Duplicates: 2, Spikes: []model.PingSpike{{}}, LongestGapMs: 1500},
			},
			TCP: []model.TCPResult{
				{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 941_000_000},
				{
					Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 300_000_000,
					ThroughputVariation: 0.4, Retransmissions: uint64Ptr(100),
					IntervalResults: []model.TCPInterval{
						{BitsPerSecond: 100_000_000, Bytes: 12_500_000},
						{BitsPerSecond: 100_000_000, Bytes: 12_500_000},
						{BitsPerSecond: 10_000_000, Bytes: 1_250_000},
						{BitsPerSecond: 100_000_000, Bytes: 12_500_000},
					},
				},
				{Direction: model.DirectionPC2ToPC1, ReceiverBitsPerSecond: 941_000_000},
			},
			UDP: []model.UDPResult{
				{Direction: model.DirectionPC1ToPC2, TargetBps: 800_000_000, ActualSenderBps: 800_000_000, LossPercent: 4, JitterMs: 7},
				{Direction: model.DirectionPC1ToPC2, TargetBps: 400_000_000, ActualSenderBps: 400_000_000},
			},
		},
		InitialCounters: model.PeerCounters{PC1: before, PC2: before},
		FinalCounters:   model.PeerCounters{PC1: after, PC2: after},
	}

	f := FactsFromReport(r)
	if got := f.Dir[0].TCPBitrate; got != 300_000_000 {
		t.Fatalf("pc1->pc2 TCP bitrate = %s, want worst repeated result 300 Mbit/s", got)
	}
	if got := f.Dir[0].TCPCoV; got != 0.4 {
		t.Errorf("pc1->pc2 TCP variation = %v, want worst repeated result 0.4", got)
	}
	if got := f.Dir[0].TCPRetransRate; got <= 0 {
		t.Errorf("pc1->pc2 retransmit rate = %v, want degraded repeat's non-zero rate", got)
	}
	if got := f.Dir[0].TCPCollapses; got != 1 {
		t.Errorf("pc1->pc2 collapses = %d, want the collapse from the degraded repeat", got)
	}
	if got := f.Dir[0].PingLossPct; got != 3 {
		t.Errorf("pc1->pc2 ping loss = %v, want worst repeated result 3", got)
	}
	if got := f.Dir[0].UDPLossPct; got != 4 {
		t.Errorf("pc1->pc2 UDP loss = %v, want lossy primary-rate result 4", got)
	}
	if got := f.Dir[0].UDPJitterMs; got != 7 {
		t.Errorf("pc1->pc2 UDP jitter = %v, want worst repeated result 7", got)
	}
	res := Evaluate(f)
	if res.Class != model.HealthPoor {
		t.Errorf("class = %s, want POOR from the degraded repeat (findings %v)", res.Class, findingIDs(res))
	}
	if !slices.Contains(findingIDs(res), "PERF-01") {
		t.Errorf("findings = %v, want PERF-01 from the degraded repeat", findingIDs(res))
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
	if f.Dir[0].TCPBitrate != 0 || f.Dir[0].TCPCoV != 0 || f.Dir[0].TCPCollapses != 0 || f.Dir[0].TCPRetransRate != 0 {
		t.Errorf("incomplete TCP metrics leaked into facts: %+v", f.Dir[0])
	}
	res := Evaluate(f)
	if res.Class != model.HealthInconclusive {
		t.Errorf("class = %s, want INCONCLUSIVE for partial run (findings %v)", res.Class, findingIDs(res))
	}
	if slices.Contains(findingIDs(res), "PERF-01") {
		t.Errorf("findings %v contain PERF-01 for an incomplete placeholder", findingIDs(res))
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
