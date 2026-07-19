package evaluate

import (
	"math"
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

// sideWithCRC builds SideFacts with reliable counters and the given CRC-class
// error delta.
func sideWithCRC(n uint64) SideFacts {
	return SideFacts{CRCClassErrors: n, DeltaOK: true, CountersAvailable: true}
}

func TestRulePHY02CRCBands(t *testing.T) {
	rule := ruleByID(t, "PHY-02")
	cases := []struct {
		name     string
		pc1, pc2 SideFacts
		loss     float64
		want     model.Severity
		none     bool
	}{
		{name: "zero errors pass", pc1: sideWithCRC(0), pc2: sideWithCRC(0), none: true},
		{name: "one error warns", pc1: sideWithCRC(1), want: model.SevWarning},
		{name: "ten errors warn", pc1: sideWithCRC(10), want: model.SevWarning},
		{name: "eleven errors poor", pc1: sideWithCRC(11), want: model.SevPoor},
		{name: "sides sum before banding", pc1: sideWithCRC(6), pc2: sideWithCRC(6), want: model.SevPoor},
		{name: "thousand errors poor", pc1: sideWithCRC(1000), want: model.SevPoor},
		{name: "over thousand failed", pc1: sideWithCRC(1001), want: model.SevFailed},
		{name: "over ten with ping loss failed", pc1: sideWithCRC(50), loss: 1.5, want: model.SevFailed},
		{name: "loss of exactly one percent stays poor", pc1: sideWithCRC(50), loss: 1.0, want: model.SevPoor},
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
			fd := rule.Evaluate(f)
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
			finding := rule.Evaluate(f)
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
			finding := rule.Evaluate(&Facts{CableTestRan: true, CableTestPairs: []model.CablePairResult{{Pair: "A", Status: tc.status}}})
			if finding == nil || finding.Severity != tc.want {
				t.Errorf("PHY-08(%s) = %+v, want severity %v", tc.status, finding, tc.want)
			}
		}
	})

	t.Run("unavailable is not a fault", func(t *testing.T) {
		f := FactsFromReport(&model.Report{Tests: model.TestsSection{CableTest: &model.CableTestResult{
			Available: false, UnavailableReason: "driver does not support cable test",
		}}})
		if finding := rule.Evaluate(f); finding != nil {
			t.Errorf("PHY-08(unavailable) = %+v, want no fault", finding)
		}
	})
}

func TestRuleTR01PingLoss(t *testing.T) {
	rule := ruleByID(t, "TR-01")
	cases := []struct {
		name         string
		loss0, loss1 float64
		want         model.Severity
		none         bool
	}{
		{name: "zero loss passes", none: true},
		{name: "trace loss warns", loss0: 0.05, want: model.SevWarning},
		{name: "boundary tenth percent warns", loss0: 0.1, want: model.SevWarning},
		{name: "fifth of a percent poor", loss0: 0.2, want: model.SevPoor},
		{name: "one percent poor", loss0: 1.0, want: model.SevPoor},
		{name: "heavy loss stays poor not failed", loss0: 5, want: model.SevPoor},
		{name: "worst direction wins", loss0: 0.05, loss1: 2, want: model.SevPoor},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &Facts{LinkUpAtEnd: true}
			f.Dir[0].PingLossPct = tc.loss0
			f.Dir[1].PingLossPct = tc.loss1
			fd := rule.Evaluate(f)
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
		fd := rule.Evaluate(f)
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
		cases := []struct {
			name string
			rate float64
			want model.Severity
			none bool
		}{
			{name: "under a tenth of a percent passes", rate: 0.0005, none: true},
			{name: "tenth of a percent warns", rate: 0.001, want: model.SevWarning},
			{name: "half percent warns", rate: 0.005, want: model.SevWarning},
			{name: "two percent poor", rate: 0.02, want: model.SevPoor},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				f := &Facts{LinkUpAtEnd: true}
				f.Dir[0] = DirFacts{TCPAvailable: true, TCPRetransRate: tc.rate}
				fd := rule.Evaluate(f)
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

func TestRuleTR07UDPGating(t *testing.T) {
	rule := ruleByID(t, "TR-07")
	base := func(loss float64) *Facts {
		f := &Facts{LinkUpAtEnd: true}
		f.Dir[0] = DirFacts{UDPAvailable: true, UDPTargetReached: true, UDPLossPct: loss}
		return f
	}

	t.Run("poor at five percent loss", func(t *testing.T) {
		fd := rule.Evaluate(base(5))
		if fd == nil || fd.Severity != model.SevPoor {
			t.Errorf("TR-07 = %+v, want poor", fd)
		}
	})
	t.Run("warning at one percent loss", func(t *testing.T) {
		fd := rule.Evaluate(base(1))
		if fd == nil || fd.Severity != model.SevWarning {
			t.Errorf("TR-07 = %+v, want warning", fd)
		}
	})
	t.Run("passes under half a percent", func(t *testing.T) {
		if fd := rule.Evaluate(base(0.3)); fd != nil {
			t.Errorf("TR-07 = %+v, want no finding", fd)
		}
	})
	t.Run("no finding when target rate unreached", func(t *testing.T) {
		f := base(5)
		f.Dir[0].UDPTargetReached = false
		if fd := rule.Evaluate(f); fd != nil {
			t.Errorf("TR-07 = %+v, want no finding when sender missed target", fd)
		}
	})
	t.Run("no finding when cpu saturated", func(t *testing.T) {
		f := base(5)
		f.MaxCPUPct = 95
		if fd := rule.Evaluate(f); fd != nil {
			t.Errorf("TR-07 = %+v, want no finding when MaxCPUPct > 90", fd)
		}
	})
	t.Run("qualified reduced-rate fact survives a separate near-line-rate run", func(t *testing.T) {
		f := base(5)
		f.UDPNearSaturation = true
		if fd := rule.Evaluate(f); fd == nil || fd.Severity != model.SevPoor {
			t.Errorf("TR-07 = %+v, want qualifying loss evaluated despite a separate near-saturation run", fd)
		}
	})
}
