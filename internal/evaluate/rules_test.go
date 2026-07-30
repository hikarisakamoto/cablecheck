package evaluate

import (
	"math"
	"slices"
	"strings"
	"testing"

	"cablecheck/internal/model"
)

// ruleByID returns the rule with the given ID from Rules(), failing the test
// when the ID is unknown.
func ruleByID(t *testing.T, id string) Rule {
	t.Helper()
	for _, r := range Rules() {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("Rules() has no rule %q", id)
	return Rule{}
}

func evaluateRule(rule Rule, f *Facts) *model.Finding {
	return rule.Evaluate(f, Default())
}

// sideWithCRC builds SideFacts with reliable counters and the given CRC-class
// error delta.
func sideWithCRC(n uint64) SideFacts {
	return SideFacts{CRCClassErrors: n, DeltaOK: true, CountersAvailable: true, RXErrorEvidence: true}
}

// sideWithCarrierPHY builds reliable SideFacts with the given aggregate
// transmit-carrier/PHY error delta.
func sideWithCarrierPHY(n uint64) SideFacts {
	return SideFacts{CarrierPHYErrors: n, DeltaOK: true, CountersAvailable: true, RXErrorEvidence: true}
}

func TestRulePHY02CRCBands(t *testing.T) {
	rule := ruleByID(t, "PHY-02")
	thresholds := Default()
	cases := []struct {
		name     string
		pc1, pc2 SideFacts
		loss     float64
		want     model.Severity
		none     bool
	}{
		{name: "zero errors pass", pc1: sideWithCRC(0), pc2: sideWithCRC(0), none: true},
		{name: "one error warns", pc1: sideWithCRC(1), want: model.SevWarning},
		{name: "poor boundary warns", pc1: sideWithCRC(thresholds.CRCPoorAbove), want: model.SevWarning},
		{name: "above poor boundary is poor", pc1: sideWithCRC(thresholds.CRCPoorAbove + 1), want: model.SevPoor},
		{name: "sides sum before banding", pc1: sideWithCRC(6), pc2: sideWithCRC(6), want: model.SevPoor},
		{name: "failed boundary stays poor", pc1: sideWithCRC(thresholds.CRCFailedAbove), want: model.SevPoor},
		{name: "above failed boundary fails", pc1: sideWithCRC(thresholds.CRCFailedAbove + 1), want: model.SevFailed},
		{name: "over ten with ping loss failed", pc1: sideWithCRC(50), loss: 1.5, want: model.SevFailed},
		{name: "corroborating loss boundary stays poor", pc1: sideWithCRC(50), loss: thresholds.CRCCorroboratingPingLossAbove, want: model.SevPoor},
		{name: "few errors with loss stay warning", pc1: sideWithCRC(5), loss: 2, want: model.SevWarning},
		{
			name: "unreliable deltas are not counted",
			pc1:  SideFacts{CRCClassErrors: 5000, DeltaOK: false, CountersAvailable: true},
			none: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &Facts{PC1: tc.pc1, PC2: tc.pc2, LinkUpAtEnd: true}
			f.Dir[0].PingLossPct = tc.loss
			fd := evaluateRule(rule, f)
			if tc.none {
				if fd != nil {
					t.Errorf("PHY-02 = %+v, want no finding", fd)
				}
				return
			}
			if fd == nil {
				t.Fatalf("PHY-02 = nil, want severity %v", tc.want)
			}
			if fd.Severity != tc.want {
				t.Errorf("PHY-02 severity = %v, want %v", fd.Severity, tc.want)
			}
			if fd.RuleID != "PHY-02" || fd.Category != model.CategoryPhysical {
				t.Errorf("PHY-02 identity = (%q, %q), want (PHY-02, physical)", fd.RuleID, fd.Category)
			}
		})
	}
}

// TestRulePHY02UnclassifiedEvidenceIsNotAssertedAsCRC pins the ceiling on
// ambiguous evidence. A driver's receive-error aggregate has driver-defined
// semantics, so a remainder no per-cause counter explains is not proof of frame
// corruption: it names itself honestly and cannot reach failed on count alone.
func TestRulePHY02UnclassifiedEvidenceIsNotAssertedAsCRC(t *testing.T) {
	rule := ruleByID(t, "PHY-02")
	unclassified := func(n uint64) SideFacts {
		return SideFacts{UnclassifiedRXErrors: n, DeltaOK: true, CountersAvailable: true, RXErrorEvidence: true}
	}

	t.Run("count alone cannot escalate past warning", func(t *testing.T) {
		fd := evaluateRule(rule, &Facts{PC1: unclassified(2000)})
		if fd == nil {
			t.Fatal("PHY-02 = nil, want a finding")
		}
		if fd.Severity != model.SevWarning {
			t.Errorf("PHY-02 severity = %v, want warning: an unexplained aggregate could be the driver's own ring drops", fd.Severity)
		}
		if strings.Contains(fd.Text, "CRC") {
			t.Errorf("PHY-02 text = %q, want it to not claim CRC errors it never measured", fd.Text)
		}
	})

	t.Run("independent packet loss still fails the run", func(t *testing.T) {
		f := &Facts{PC1: unclassified(2000)}
		f.Dir[0].PingLossPct = 2
		if fd := evaluateRule(rule, f); fd == nil || fd.Severity != model.SevFailed {
			t.Errorf("PHY-02 = %+v, want failed when ping loss corroborates the counter", fd)
		}
	})

	t.Run("per-cause counters still fail the run on count", func(t *testing.T) {
		if fd := evaluateRule(rule, &Facts{PC1: sideWithCRC(2000)}); fd == nil || fd.Severity != model.SevFailed {
			t.Errorf("PHY-02 = %+v, want failed for a measured CRC count", fd)
		}
	})
}

func TestRulePHY11CarrierPHYBands(t *testing.T) {
	rule := ruleByID(t, "PHY-11")
	thresholds := Default()
	cases := []struct {
		name     string
		pc1, pc2 SideFacts
		want     model.Severity
		none     bool
	}{
		{name: "zero errors pass", pc1: sideWithCarrierPHY(0), pc2: sideWithCarrierPHY(0), none: true},
		{name: "one error warns", pc1: sideWithCarrierPHY(1), want: model.SevWarning},
		{name: "poor boundary warns", pc1: sideWithCarrierPHY(thresholds.CarrierPHYPoorAbove), want: model.SevWarning},
		{name: "above poor boundary is poor", pc1: sideWithCarrierPHY(thresholds.CarrierPHYPoorAbove + 1), want: model.SevPoor},
		{name: "sides sum before banding", pc1: sideWithCarrierPHY(6), pc2: sideWithCarrierPHY(6), want: model.SevPoor},
		{name: "failed boundary stays poor", pc1: sideWithCarrierPHY(thresholds.CarrierPHYFailedAbove), want: model.SevPoor},
		{name: "above failed boundary fails", pc1: sideWithCarrierPHY(thresholds.CarrierPHYFailedAbove + 1), want: model.SevFailed},
		{
			name: "unreliable deltas are not counted",
			pc1:  SideFacts{CarrierPHYErrors: 5000, DeltaOK: false, CountersAvailable: true},
			none: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fd := evaluateRule(rule, &Facts{PC1: tc.pc1, PC2: tc.pc2, LinkUpAtEnd: true})
			if tc.none {
				if fd != nil {
					t.Errorf("PHY-11 = %+v, want no finding", fd)
				}
				return
			}
			if fd == nil {
				t.Fatalf("PHY-11 = nil, want severity %v", tc.want)
			}
			if fd.Severity != tc.want {
				t.Errorf("PHY-11 severity = %v, want %v", fd.Severity, tc.want)
			}
			if fd.RuleID != "PHY-11" || fd.Category != model.CategoryPhysical {
				t.Errorf("PHY-11 identity = (%q, %q), want (PHY-11, physical)", fd.RuleID, fd.Category)
			}
			if len(fd.Evidence) == 0 {
				t.Error("PHY-11 evidence is empty")
			}
		})
	}
}

func TestRuleHOST04ReceiveRingMarker(t *testing.T) {
	rule := ruleByID(t, "HOST-04")
	cases := []struct {
		name     string
		pc1, pc2 SideFacts
		wantText []string
		notText  []string
		none     bool
	}{
		{name: "zero movement passes", pc1: SideFacts{DeltaOK: true}, none: true},
		{name: "fifo movement", pc1: SideFacts{FifoOverrun: 2, DeltaOK: true}, wantText: []string{"pc1", "rx_fifo +2"}},
		{name: "missed movement", pc2: SideFacts{MissedErrors: 3, DeltaOK: true}, wantText: []string{"pc2", "rx_missed +3"}},
		{
			name:     "both counters retain separate evidence",
			pc1:      SideFacts{FifoOverrun: 4, MissedErrors: 5, DeltaOK: true},
			wantText: []string{"rx_fifo +4", "rx_missed +5"},
		},
		{
			name: "unreliable movement is ignored",
			pc1:  SideFacts{FifoOverrun: 9, MissedErrors: 9, DeltaOK: false},
			none: true,
		},
		{
			// The field case: 2 dropped frames out of 18.5 million cannot limit
			// throughput, and must not silence the performance score.
			name: "movement far below the drop rate is not a host limitation",
			pc1:  SideFacts{MissedErrors: 2, FramesReceived: 18_500_000, DeltaOK: true},
			none: true,
		},
		{
			name:     "movement above the drop rate marks the run",
			pc1:      SideFacts{MissedErrors: 2_000, FramesReceived: 18_500_000, DeltaOK: true},
			wantText: []string{"pc1", "rx_missed +2000"},
		},
		{
			// The two registers are distinct on the mapped drivers, so the gate
			// rates their combined volume; both are then reported separately.
			name:     "both drop counters are rated on their combined volume",
			pc1:      SideFacts{FifoOverrun: 100, MissedErrors: 100, FramesReceived: 18_500_000, DeltaOK: true},
			wantText: []string{"rx_fifo +100", "rx_missed +100"},
		},
		{
			name:    "a counter that did not move is not reported",
			pc1:     SideFacts{FifoOverrun: 2_000, FramesReceived: 18_500_000, DeltaOK: true},
			notText: []string{"rx_missed"},
		},
		{
			// No traffic denominator means no rate; any movement still marks it.
			name:     "movement without a frame count still marks the run",
			pc2:      SideFacts{MissedErrors: 1, DeltaOK: true},
			wantText: []string{"pc2", "rx_missed +1"},
		},
		{
			// A long soak brackets its counters across the whole run, so a burst
			// confined to one cycle is diluted below any rate. A large absolute
			// count is host evidence regardless of how much clean traffic followed.
			name:     "a large drop burst survives a diluted whole-run denominator",
			pc1:      SideFacts{MissedErrors: 1_000, FramesReceived: 500_000_000, DeltaOK: true},
			wantText: []string{"pc1", "rx_missed +1000"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fd := evaluateRule(rule, &Facts{PC1: tc.pc1, PC2: tc.pc2})
			if tc.none {
				if fd != nil {
					t.Errorf("HOST-04 = %+v, want no finding", fd)
				}
				return
			}
			if fd == nil {
				t.Fatal("HOST-04 = nil, want marker")
			}
			if fd.RuleID != "HOST-04" || fd.Category != model.CategoryHost || fd.Severity != model.SevMarker {
				t.Errorf("HOST-04 identity = (%q, %q, %v), want (HOST-04, host, marker)", fd.RuleID, fd.Category, fd.Severity)
			}
			joined := strings.Join(fd.Evidence, " ")
			for _, want := range tc.wantText {
				if !strings.Contains(joined, want) {
					t.Errorf("HOST-04 evidence = %q, want substring %q", joined, want)
				}
			}
			for _, unwanted := range tc.notText {
				if strings.Contains(joined, unwanted) {
					t.Errorf("HOST-04 evidence = %q, want no substring %q", joined, unwanted)
				}
			}
		})
	}
}

// TestRulePHY08PairFaults pins the severity ladder for direct cable-test
// evidence and verifies unavailable diagnostics never become a fault.
func TestRulePHY08PairFaults(t *testing.T) {
	rule := ruleByID(t, "PHY-08")

	t.Run("open and short are failed with distance evidence", func(t *testing.T) {
		for _, status := range []model.PairStatus{model.PairOpen, model.PairShortIntra, model.PairShortInter} {
			f := FactsFromReport(&model.Report{Tests: model.TestsSection{CableTest: &model.CableTestResult{
				Available: true,
				Pairs: []model.CablePairResult{{
					Pair: "C", Status: status, HasFault: true, FaultMeters: 32,
				}},
			}}})
			finding := evaluateRule(rule, f)
			if finding == nil || finding.Severity != model.SevFailed {
				t.Errorf("PHY-08(%s) = %+v, want FAILED-tier finding", status, finding)
				continue
			}
			if !strings.Contains(strings.Join(finding.Evidence, " "), "32.0m") {
				t.Errorf("PHY-08(%s) evidence = %q, want fault distance", status, finding.Evidence)
			}
		}
	})

	t.Run("impedance is poor and unspecified warns", func(t *testing.T) {
		for _, tc := range []struct {
			status model.PairStatus
			want   model.Severity
		}{{model.PairImpedance, model.SevPoor}, {model.PairUnspecified, model.SevWarning}} {
			finding := evaluateRule(rule, &Facts{CableTestRan: true, CableTestPairs: []model.CablePairResult{{Pair: "A", Status: tc.status}}})
			if finding == nil || finding.Severity != tc.want {
				t.Errorf("PHY-08(%s) = %+v, want severity %v", tc.status, finding, tc.want)
			}
		}
	})

	t.Run("unavailable is not a fault", func(t *testing.T) {
		f := FactsFromReport(&model.Report{Tests: model.TestsSection{CableTest: &model.CableTestResult{
			Available: false, UnavailableReason: "driver does not support cable test",
		}}})
		if finding := evaluateRule(rule, f); finding != nil {
			t.Errorf("PHY-08(unavailable) = %+v, want no fault", finding)
		}
	})
}

func TestRuleTR01PingLoss(t *testing.T) {
	rule := ruleByID(t, "TR-01")
	thresholds := Default()
	cases := []struct {
		name         string
		loss0, loss1 float64
		want         model.Severity
		none         bool
	}{
		{name: "zero loss passes", none: true},
		{name: "trace loss warns", loss0: 0.05, want: model.SevWarning},
		{name: "poor boundary warns", loss0: thresholds.PingLossPoorAbove, want: model.SevWarning},
		{name: "above poor boundary is poor", loss0: math.Nextafter(thresholds.PingLossPoorAbove, math.Inf(1)), want: model.SevPoor},
		{name: "one percent poor", loss0: 1.0, want: model.SevPoor},
		{name: "heavy loss stays poor not failed", loss0: 5, want: model.SevPoor},
		{name: "worst direction wins", loss0: 0.05, loss1: 2, want: model.SevPoor},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &Facts{LinkUpAtEnd: true}
			f.Dir[0].PingLossPct = tc.loss0
			f.Dir[1].PingLossPct = tc.loss1
			fd := evaluateRule(rule, f)
			if tc.none {
				if fd != nil {
					t.Errorf("TR-01 = %+v, want no finding", fd)
				}
				return
			}
			if fd == nil {
				t.Fatalf("TR-01 = nil, want severity %v", tc.want)
			}
			if fd.Severity != tc.want {
				t.Errorf("TR-01 severity = %v, want %v", fd.Severity, tc.want)
			}
			if fd.Category != model.CategoryTransport {
				t.Errorf("TR-01 category = %q, want transport", fd.Category)
			}
		})
	}
}

func TestRuleTR06Retrans(t *testing.T) {
	rule := ruleByID(t, "TR-06")

	t.Run("rate estimated from bytes over MSS 1448", func(t *testing.T) {
		retr := uint64(5000)
		r := &model.Report{Tests: model.TestsSection{TCP: []model.TCPResult{{
			Direction:             model.DirectionPC1ToPC2,
			SenderBitsPerSecond:   941e6,
			ReceiverBitsPerSecond: 940e6,
			Retransmissions:       &retr,
			IntervalResults: []model.TCPInterval{
				{Bytes: 724_000_000},
				{Bytes: 724_000_000},
			},
		}}}}
		f := FactsFromReport(r)
		want := 5000.0 / (1_448_000_000.0 / 1448.0) // 0.005 = 0.5%
		if math.Abs(f.Dir[0].TCPRetransRate-want) > 1e-12 {
			t.Fatalf("TCPRetransRate = %v, want %v", f.Dir[0].TCPRetransRate, want)
		}
		fd := evaluateRule(rule, f)
		if fd == nil {
			t.Fatalf("TR-06 = nil, want WARNING at 0.5%% estimated rate")
		}
		if fd.Severity != model.SevWarning {
			t.Errorf("TR-06 severity = %v, want warning", fd.Severity)
		}
		if !strings.Contains(strings.Join(fd.Evidence, " "), "estimated") {
			t.Errorf("TR-06 evidence %q does not say the rate is estimated", fd.Evidence)
		}
	})

	t.Run("bands", func(t *testing.T) {
		thresholds := Default()
		cases := []struct {
			name string
			rate float64
			want model.Severity
			none bool
		}{
			{name: "below warning boundary passes", rate: math.Nextafter(thresholds.TCPRetransWarningAt, math.Inf(-1)), none: true},
			{name: "warning boundary warns", rate: thresholds.TCPRetransWarningAt, want: model.SevWarning},
			{name: "half percent warns", rate: 0.005, want: model.SevWarning},
			{name: "poor boundary still warns", rate: thresholds.TCPRetransPoorAbove, want: model.SevWarning},
			{name: "above poor boundary is poor", rate: math.Nextafter(thresholds.TCPRetransPoorAbove, math.Inf(1)), want: model.SevPoor},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				f := &Facts{LinkUpAtEnd: true}
				f.Dir[0] = DirFacts{TCPAvailable: true, TCPRetransRate: tc.rate}
				fd := evaluateRule(rule, f)
				if tc.none {
					if fd != nil {
						t.Errorf("TR-06 = %+v, want no finding", fd)
					}
					return
				}
				if fd == nil {
					t.Fatalf("TR-06 = nil, want severity %v", tc.want)
				}
				if fd.Severity != tc.want {
					t.Errorf("TR-06 severity = %v, want %v", fd.Severity, tc.want)
				}
			})
		}
	})
}

// TestTR06IsHostSensitive pins that a retransmission burst cannot outrank
// measured host evidence. Retransmissions alone cannot separate wire corruption
// from local queue drops, so a demonstrated receive-ring starvation must be able
// to hold the verdict at INCONCLUSIVE instead of blaming the cable.
func TestTR06IsHostSensitive(t *testing.T) {
	if fd := evaluateRule(ruleByID(t, "TR-06"), &Facts{
		Dir: [2]DirFacts{{TCPAvailable: true, TCPRetransRate: 0.02}},
	}); fd == nil || !fd.HostSensitive {
		t.Fatalf("TR-06 = %+v, want a host-sensitive finding", fd)
	}

	f := cleanFacts()
	f.Dir[0].TCPRetransRate = 0.02 // poor tier
	f.PC2.MissedErrors = 50_000
	f.PC2.FramesReceived = 18_173_450
	res := Evaluate(f)
	if !slices.Contains(findingIDs(res), "HOST-04") {
		t.Fatalf("findings = %v, want HOST-04 for a 0.27%% ring-drop rate", findingIDs(res))
	}
	if res.Class != model.HealthInconclusive {
		t.Errorf("class = %v, want INCONCLUSIVE: the drop rate explains the retransmissions", res.Class)
	}
}

func TestRuleTR07UDPGating(t *testing.T) {
	rule := ruleByID(t, "TR-07")
	thresholds := Default()
	base := func(loss float64) *Facts {
		f := &Facts{LinkUpAtEnd: true}
		f.Dir[0] = DirFacts{UDPAvailable: true, UDPTargetReached: true, UDPLossPct: loss}
		return f
	}

	t.Run("poor at five percent loss", func(t *testing.T) {
		fd := evaluateRule(rule, base(5))
		if fd == nil || fd.Severity != model.SevPoor {
			t.Errorf("TR-07 = %+v, want poor", fd)
		}
	})
	t.Run("warning at one percent loss", func(t *testing.T) {
		fd := evaluateRule(rule, base(1))
		if fd == nil || fd.Severity != model.SevWarning {
			t.Errorf("TR-07 = %+v, want warning", fd)
		}
	})
	t.Run("passes under half a percent", func(t *testing.T) {
		if fd := evaluateRule(rule, base(math.Nextafter(thresholds.UDPLossWarningAt, math.Inf(-1)))); fd != nil {
			t.Errorf("TR-07 = %+v, want no finding", fd)
		}
	})
	t.Run("warning boundary is inclusive", func(t *testing.T) {
		fd := evaluateRule(rule, base(thresholds.UDPLossWarningAt))
		if fd == nil || fd.Severity != model.SevWarning {
			t.Errorf("TR-07 = %+v, want warning", fd)
		}
	})
	t.Run("poor boundary stays warning", func(t *testing.T) {
		fd := evaluateRule(rule, base(thresholds.UDPLossPoorAbove))
		if fd == nil || fd.Severity != model.SevWarning {
			t.Errorf("TR-07 = %+v, want warning", fd)
		}
	})
	t.Run("above poor boundary is poor", func(t *testing.T) {
		fd := evaluateRule(rule, base(math.Nextafter(thresholds.UDPLossPoorAbove, math.Inf(1))))
		if fd == nil || fd.Severity != model.SevPoor {
			t.Errorf("TR-07 = %+v, want poor", fd)
		}
	})
	t.Run("no finding when target rate unreached", func(t *testing.T) {
		f := base(5)
		f.Dir[0].UDPTargetReached = false
		if fd := evaluateRule(rule, f); fd != nil {
			t.Errorf("TR-07 = %+v, want no finding when sender missed target", fd)
		}
	})
	t.Run("no finding when cpu saturated", func(t *testing.T) {
		f := base(5)
		f.Dir[0].UDPMaxCPUPct = 95
		if fd := evaluateRule(rule, f); fd != nil {
			t.Errorf("TR-07 = %+v, want no finding when direction CPU > 90", fd)
		}
	})
	t.Run("CPU boundary remains eligible", func(t *testing.T) {
		f := base(5)
		f.Dir[0].UDPMaxCPUPct = thresholds.CPUHostLimitedAbove
		if fd := evaluateRule(rule, f); fd == nil || fd.Severity != model.SevPoor {
			t.Errorf("TR-07 = %+v, want poor at exact CPU boundary", fd)
		}
	})
	t.Run("hot direction does not hide clean direction loss", func(t *testing.T) {
		f := base(5)
		f.Dir[0].UDPMaxCPUPct = math.Nextafter(thresholds.CPUHostLimitedAbove, math.Inf(1))
		f.Dir[1] = DirFacts{
			UDPAvailable: true, UDPTargetReached: true, UDPLossPct: 1,
			UDPMaxCPUPct: thresholds.CPUHostLimitedAbove,
		}
		fd := evaluateRule(rule, f)
		if fd == nil || fd.Severity != model.SevWarning {
			t.Fatalf("TR-07 = %+v, want warning from clean pc2->pc1 direction", fd)
		}
		evidence := strings.Join(fd.Evidence, " ")
		if strings.Contains(evidence, "pc1->pc2") || !strings.Contains(evidence, "pc2->pc1") {
			t.Errorf("TR-07 evidence = %q, want only clean pc2->pc1 direction", evidence)
		}
	})
	t.Run("unrelated global CPU does not gate qualifying loss", func(t *testing.T) {
		f := base(5)
		f.MaxCPUPct = 99
		if fd := evaluateRule(rule, f); fd == nil || fd.Severity != model.SevPoor {
			t.Errorf("TR-07 = %+v, want poor despite unrelated global CPU", fd)
		}
	})
	t.Run("qualified reduced-rate fact survives a separate near-line-rate run", func(t *testing.T) {
		f := base(5)
		f.UDPNearSaturation = true
		if fd := evaluateRule(rule, f); fd == nil || fd.Severity != model.SevPoor {
			t.Errorf("TR-07 = %+v, want qualifying loss evaluated despite a separate near-saturation run", fd)
		}
	})
}
